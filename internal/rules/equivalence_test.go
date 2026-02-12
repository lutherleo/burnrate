package rules_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/util/teststorage"
	"google.golang.org/protobuf/proto"

	"github.com/lutherleo/burnrate/internal/engine"
	"github.com/lutherleo/burnrate/internal/otlp"
	brrules "github.com/lutherleo/burnrate/internal/rules"
	"github.com/lutherleo/burnrate/internal/sim"
	"github.com/lutherleo/burnrate/internal/slo"
)

// transition is an alert firing or resolving, relative to the scenario start.
type transition struct {
	key    string // slo/severity
	firing bool
	at     time.Duration
}

func (t transition) String() string {
	verb := "resolved"
	if t.firing {
		verb = "firing"
	}
	return fmt.Sprintf("%s %s@%s", t.key, verb, t.at)
}

// tolerance is how far apart the engine and Prometheus may place the same
// transition. Prometheus' rate() extrapolates within each window and ignores
// the first sample of a lazily created error series, so it can lag or lead
// the engine's exact bucket counts by a step or two.
const tolerance = 2 * time.Minute

// TestGeneratedRulesMatchEngine is the equivalence proof. The same OTLP
// protobuf requests are posted to burnrate's receiver and to Prometheus' own
// OTLP write handler (backed by a real TSDB). The generated rule groups are
// then evaluated by Prometheus' rule manager every minute, and the alert
// transitions and budget policy states are compared with the engine's.
func TestGeneratedRulesMatchEngine(t *testing.T) {
	if testing.Short() {
		t.Skip("equivalence test replays days of traffic")
	}
	slos, err := slo.LoadPaths("../../examples/slos")
	if err != nil {
		t.Fatal(err)
	}
	ruleFile := writeRules(t, slos)
	for _, name := range []string{"outage", "partial-outage", "slow-burn", "latency-regression", "flapping", "search-brownout"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sc, err := sim.Lookup(name)
			if err != nil {
				t.Fatal(err)
			}
			compareScenario(t, sc, slos, ruleFile)
		})
	}
}

func writeRules(t *testing.T, slos []*slo.SLO) string {
	t.Helper()
	f, err := brrules.Generate(slos, brrules.Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Marshal("equivalence test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func compareScenario(t *testing.T, sc sim.Scenario, slos []*slo.SLO, ruleFile string) {
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	gen, err := sim.NewGenerator(sc, start)
	if err != nil {
		t.Fatal(err)
	}

	// burnrate side: the real HTTP receiver feeding the engine.
	eng, err := engine.New(engine.Config{}, slos)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := otlp.NewTranslator(otlp.Options{}, eng.Wants)
	if err != nil {
		t.Fatal(err)
	}
	ours := otlp.NewReceiver(tr, eng.Ingest, nil)

	// Prometheus side: its OTLP receiver, TSDB, PromQL engine and rule manager.
	st := teststorage.New(t)
	theirs := remote.NewOTLPWriteHandler(slog.New(slog.DiscardHandler), prometheus.NewRegistry(), st,
		func() config.Config { return config.DefaultConfig }, remote.OTLPOptions{})
	qe := promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{MaxSamples: 50_000_000, Timeout: time.Minute, LookbackDelta: 5 * time.Minute})
	mgr := rules.NewManager(&rules.ManagerOptions{
		QueryFunc: rules.EngineQueryFunc(qe, st), Appendable: st, Queryable: st,
		Context: context.Background(), Logger: slog.New(slog.DiscardHandler),
		NotifyFunc: func(context.Context, string, ...*rules.Alert) {},
	})
	groups, errs := mgr.LoadGroups(time.Minute, labels.EmptyLabels(), "", nil, false, ruleFile)
	if len(errs) > 0 {
		t.Fatalf("Prometheus rejected the generated rules: %v", errs)
	}
	ordered := make([]*rules.Group, 0, len(groups))
	for _, g := range groups {
		ordered = append(ordered, g)
	}

	// Only evaluate Prometheus where something can happen: from just before
	// the first incident until the scenario ends. The generated rules are
	// stateless (no `for`), so skipping earlier steps loses nothing.
	from := sc.Duration
	for _, in := range sc.Incidents {
		from = min(from, in.Start-10*time.Minute)
	}

	var engineT, promT []transition
	promFiring := map[string]bool{}
	for {
		req, at, ok := gen.Next()
		if !ok {
			break
		}
		body, err := proto.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		post(t, ours, body)
		post(t, theirs, body)

		offset := at.Sub(start)
		for _, ev := range eng.Advance(at) {
			if offset >= from && (ev.Kind == engine.EventAlertFiring || ev.Kind == engine.EventAlertResolved) {
				engineT = append(engineT, transition{ev.SLO + "/" + ev.Severity, ev.Kind == engine.EventAlertFiring, offset})
			}
		}
		if offset < from {
			continue
		}
		now := map[string]bool{}
		for _, g := range ordered {
			g.Eval(context.Background(), at)
			for _, ar := range g.AlertingRules() {
				for _, a := range ar.ActiveAlerts() {
					if a.State == rules.StateFiring {
						now[a.Labels.Get("slo")+"/"+a.Labels.Get("severity")] = true
					}
				}
			}
		}
		promT = append(promT, diff(promFiring, now, offset)...)
		promFiring = now
	}

	if len(engineT) == 0 {
		t.Fatal("scenario produced no alerts; the comparison would be vacuous")
	}
	assertTransitionsMatch(t, engineT, promT)
	assertFinalStateMatches(t, qe, st, start.Add(sc.Duration), eng.Status())
}

func post(t *testing.T, h http.Handler, body []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, otlp.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", otlp.ContentTypeProtobuf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code/100 != 2 {
		b, _ := io.ReadAll(rec.Body)
		t.Fatalf("OTLP handler returned %d: %s", rec.Code, b)
	}
}

func diff(before, after map[string]bool, at time.Duration) []transition {
	var out []transition
	for k := range after {
		if !before[k] {
			out = append(out, transition{k, true, at})
		}
	}
	for k := range before {
		if !after[k] {
			out = append(out, transition{k, false, at})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

func byKey(ts []transition) map[string][]transition {
	m := map[string][]transition{}
	for _, t := range ts {
		m[t.key] = append(m[t.key], t)
	}
	return m
}

func assertTransitionsMatch(t *testing.T, engineT, promT []transition) {
	t.Helper()
	e, p := byKey(engineT), byKey(promT)
	report := func() string { return fmt.Sprintf("engine:     %v\nprometheus: %v", engineT, promT) }
	if len(e) != len(p) {
		t.Fatalf("different alerts fired\n%s", report())
	}
	var worst time.Duration
	for k, es := range e {
		ps := p[k]
		if len(es) != len(ps) {
			t.Fatalf("%s: %d transitions vs %d\n%s", k, len(es), len(ps), report())
		}
		for i := range es {
			d := es[i].at - ps[i].at
			if d < 0 {
				d = -d
			}
			worst = max(worst, d)
			if es[i].firing != ps[i].firing || d > tolerance {
				t.Fatalf("%s transition %d: engine %v vs prometheus %v\n%s", k, i, es[i], ps[i], report())
			}
		}
	}
	t.Logf("%d alert transitions matched; largest offset %s", len(engineT), worst)
}

// assertFinalStateMatches compares the budget policy state and the 1h error
// ratio recorded by Prometheus with the engine at the end of the scenario.
func assertFinalStateMatches(t *testing.T, qe *promql.Engine, st *teststorage.TestStorage, at time.Time, status []engine.Status) {
	t.Helper()
	query := func(q string) promql.Vector {
		qry, err := qe.NewInstantQuery(context.Background(), st, nil, q, at)
		if err != nil {
			t.Fatal(err)
		}
		res := qry.Exec(context.Background())
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		v, err := res.Vector()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	states := map[string]string{}
	for _, s := range query(brrules.RecordPolicyState) {
		states[s.Metric.Get("slo")] = s.Metric.Get("state")
	}
	ratios := map[string]float64{}
	for _, s := range query(brrules.ErrorRatioRecord(time.Hour)) {
		ratios[s.Metric.Get("slo")] = s.F
	}
	var lines []string
	for _, s := range status {
		if states[s.SLO] != s.Budget.State {
			t.Errorf("%s: policy state engine=%s prometheus=%s (engine remaining %v)", s.SLO, s.Budget.State, states[s.SLO], s.Budget.Remaining)
		}
		w, _ := s.Window(time.Hour)
		er, pr := float64(w.ErrorRatio), ratios[s.SLO]
		if math.Abs(er-pr) > 0.1*math.Max(er, pr)+1e-5 {
			t.Errorf("%s: 1h error ratio engine=%.6g prometheus=%.6g", s.SLO, er, pr)
		}
		lines = append(lines, fmt.Sprintf("%s state=%s ratio1h engine=%.4g prom=%.4g", s.SLO, s.Budget.State, er, pr))
	}
	t.Log(strings.Join(lines, "; "))
}
