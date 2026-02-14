package sim_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/lutherleo/burnrate/internal/engine"
	"github.com/lutherleo/burnrate/internal/otlp"
	"github.com/lutherleo/burnrate/internal/sim"
	"github.com/lutherleo/burnrate/internal/slo"
)

// TestScenariosEndToEnd replays every built-in scenario through the protobuf
// wire format, the OTLP translator and the engine, using the example SLOs,
// and checks which alerts fire. This is the integration test for the whole
// in-process pipeline.
func TestScenariosEndToEnd(t *testing.T) {
	slos, err := slo.LoadPaths("../../examples/slos")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"steady":             nil,
		"outage":             {"checkout-availability/page", "checkout-availability/ticket"},
		"partial-outage":     {"checkout-availability/page", "checkout-availability/ticket"},
		"slow-burn":          {"checkout-availability/ticket"},
		"latency-regression": {"checkout-latency/page", "checkout-latency/ticket"},
		"flapping":           {"checkout-availability/page", "checkout-availability/ticket"},
		"search-brownout":    {"search-availability/page"},
		"incident-day": {"checkout-availability/page", "checkout-availability/ticket",
			"checkout-latency/page", "checkout-latency/ticket", "search-availability/page"},
	}
	for _, name := range sim.Names() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fired, events, final := replay(t, name, slos)
			if fmt.Sprint(fired) != fmt.Sprint(want[name]) {
				t.Fatalf("fired %v, want %v\nevents:\n%s", fired, want[name], strings.Join(events, "\n"))
			}
			for _, st := range final {
				for _, a := range st.Alerts {
					if a.Firing {
						t.Errorf("%s/%s still firing at the end of the scenario", st.SLO, a.Severity)
					}
				}
				if name == "steady" && st.Budget.State != "healthy" {
					t.Errorf("steady: %s budget state %s (remaining %v)", st.SLO, st.Budget.State, st.Budget.Remaining)
				}
			}
		})
	}
}

// TestFlappingFiresRepeatedly checks the short windows reset the page between
// blips that are 20 minutes apart (the 5m window drains in ~5m).
func TestFlappingFiresRepeatedly(t *testing.T) {
	slos, err := slo.LoadPaths("../../examples/slos")
	if err != nil {
		t.Fatal(err)
	}
	_, events, _ := replay(t, "flapping", slos)
	pages := 0
	for _, e := range events {
		if strings.Contains(e, "alert_firing checkout-availability/page") {
			pages++
		}
	}
	if pages < 2 {
		t.Fatalf("expected the page to re-fire across blips, got %d firings:\n%s", pages, strings.Join(events, "\n"))
	}
}

func replay(t *testing.T, scenario string, slos []*slo.SLO) (fired, events []string, final []engine.Status) {
	t.Helper()
	sc, err := sim.Lookup(scenario)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	gen, err := sim.NewGenerator(sc, start)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(engine.Config{}, slos)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := otlp.NewTranslator(otlp.Options{}, eng.Wants)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for {
		req, at, ok := gen.Next()
		if !ok {
			break
		}
		// Round-trip through the wire format, as the receiver would.
		b, err := proto.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		decoded := &colmetricspb.ExportMetricsServiceRequest{}
		if err := proto.Unmarshal(b, decoded); err != nil {
			t.Fatal(err)
		}
		samples, res := tr.Translate(decoded)
		if res.Rejected > 0 {
			t.Fatalf("rejected points: %v", res.Errors)
		}
		eng.Ingest(samples)
		for _, ev := range eng.Advance(at) {
			key := ev.SLO + "/" + ev.Severity
			events = append(events, fmt.Sprintf("%s %s %s", ev.Time.Sub(start), ev.Kind, key))
			if ev.Kind == engine.EventAlertFiring {
				seen[key] = true
			}
		}
	}
	for k := range seen {
		fired = append(fired, k)
	}
	sort.Strings(fired)
	return fired, events, eng.Status()
}
