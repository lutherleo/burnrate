package rules

import (
	"bytes"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/rulefmt"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/lutherleo/burnrate/internal/slo"
)

var update = flag.Bool("update", false, "rewrite golden files")

func exampleSLOs(t *testing.T) []*slo.SLO {
	t.Helper()
	slos, err := slo.LoadPaths("../../examples/slos")
	if err != nil {
		t.Fatal(err)
	}
	return slos
}

func render(t *testing.T, slos []*slo.SLO, opts Options) []byte {
	t.Helper()
	f, err := Generate(slos, opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Marshal(Header([]string{"examples/slos/checkout.yaml", "examples/slos/search.yaml", "examples/slos/search.yaml"}))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestGolden pins the generated rules for the example SLOs. Run
// `go test ./internal/rules -update` after an intended change and review the
// diff of examples/prometheus/slo-rules.yaml.
func TestGolden(t *testing.T) {
	got := render(t, exampleSLOs(t), Options{})
	golden := filepath.Join("..", "..", "examples", "prometheus", "slo-rules.yaml")
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("generated rules differ from %s; rerun with -update and review the diff", golden)
	}
}

// TestPrometheusAcceptsRules runs the generated file through Prometheus' own
// rule-file parser and validator (what `promtool check rules` uses) and
// parses every expression with the PromQL parser.
func TestPrometheusAcceptsRules(t *testing.T) {
	b := render(t, exampleSLOs(t), Options{Interval: 30 * time.Second})
	groups, errs := rulefmt.Parse(b, false, model.LegacyValidation, parser.NewParser(parser.Options{}), slog.New(slog.DiscardHandler))
	if len(errs) > 0 {
		t.Fatalf("rulefmt: %v", errs)
	}
	if len(groups.Groups) != 3 {
		t.Fatalf("groups %d", len(groups.Groups))
	}
	p := parser.NewParser(parser.Options{})
	records, alerts := 0, 0
	for _, g := range groups.Groups {
		if g.Interval.String() != "30s" {
			t.Fatalf("interval %s", g.Interval)
		}
		for _, r := range g.Rules {
			if _, err := p.ParseExpr(r.Expr); err != nil {
				t.Fatalf("%s%s: %v\n%s", r.Record, r.Alert, err, r.Expr)
			}
			if r.Record != "" {
				records++
			} else {
				alerts++
			}
		}
	}
	// checkout-availability: 8 windows, 3 meta, 4 policy; checkout-latency:
	// 8 windows, 3 meta, 3 policy; search: 5 windows, 3 meta, 4 policy.
	if records != 15+14+12 || alerts != 2+2+2 {
		t.Fatalf("records %d alerts %d", records, alerts)
	}
}

func TestExpressionsShape(t *testing.T) {
	f, err := Generate(exampleSLOs(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	find := func(group, name string) Rule {
		for _, g := range f.Groups {
			if g.Name != group {
				continue
			}
			for _, r := range g.Rules {
				if r.Record == name || r.Alert == name {
					return r
				}
			}
		}
		t.Fatalf("rule %s/%s not found", group, name)
		return Rule{}
	}
	r := find("burnrate-checkout-availability", "slo:sli_error:ratio_rate5m")
	want := `clamp_max((sum(rate(http_server_request_duration_seconds_count{job="checkout", http_response_status_code=~"5.."}[5m])) or vector(0)) / (sum(rate(http_server_request_duration_seconds_count{job="checkout"}[5m])) > 0), 1)`
	if string(r.Expr) != want {
		t.Fatalf("expr\n got %s\nwant %s", r.Expr, want)
	}
	if r.Labels["slo"] != "checkout-availability" || r.Labels["service"] != "checkout" || r.Labels["team"] != "payments" {
		t.Fatalf("labels %v", r.Labels)
	}
	lat := find("burnrate-checkout-latency", "slo:sli_error:ratio_rate1h")
	if !strings.Contains(string(lat.Expr), `_bucket{job="checkout", http_response_status_code!~"5..", le="0.25"}[1h]`) || !strings.HasPrefix(string(lat.Expr), "clamp_min(1 - ") {
		t.Fatalf("latency expr %s", lat.Expr)
	}
	page := find("burnrate-checkout-availability", "ErrorBudgetBurn")
	for _, frag := range []string{
		`slo:sli_error:ratio_rate1h{slo="checkout-availability"} > (14.4 * (1 - 0.999))`,
		`slo:sli_error:ratio_rate5m{slo="checkout-availability"} > (14.4 * (1 - 0.999))`,
		`slo:sli_error:ratio_rate6h{slo="checkout-availability"} > (6 * (1 - 0.999))`,
		"\nor\n",
	} {
		if !strings.Contains(string(page.Expr), frag) {
			t.Fatalf("page expr lacks %q:\n%s", frag, page.Expr)
		}
	}
	if page.Labels["severity"] != "page" || page.Annotations["runbook_url"] == "" || page.Annotations["summary"] == "" {
		t.Fatalf("page labels %v annotations %v", page.Labels, page.Annotations)
	}
	search := find("burnrate-search-availability", "SearchBudgetBurn")
	if search.Labels["team"] != "discovery" || !strings.Contains(string(search.Expr), "(10 * (1 - 0.995))") {
		t.Fatalf("search alert %+v", search)
	}
}

func TestPolicyStateRules(t *testing.T) {
	slos := exampleSLOs(t)
	f, _ := Generate(slos, Options{})
	var exprs []string
	for _, r := range f.Groups[1].Rules { // checkout-latency: healthy>=0.4, at-risk>0
		if r.Record == RecordPolicyState {
			exprs = append(exprs, r.Labels["state"]+": "+string(r.Expr))
		}
	}
	want := []string{
		`healthy: 0 * (slo:error_budget_remaining:ratio{slo="checkout-latency"} >= 0.4) + 1`,
		`at-risk: 0 * (slo:error_budget_remaining:ratio{slo="checkout-latency"} > 0 < 0.4) + 1`,
		`exhausted: 0 * (slo:error_budget_remaining:ratio{slo="checkout-latency"} <= 0) + 1`,
	}
	if strings.Join(exprs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("policy rules\n%s", strings.Join(exprs, "\n"))
	}
	var avail []string
	for _, r := range f.Groups[0].Rules { // default policy ends at minRemaining 0
		if r.Record == RecordPolicyState {
			avail = append(avail, string(r.Expr))
		}
	}
	if !strings.Contains(avail[2], `> 0 < 0.25`) || !strings.Contains(avail[3], `<= 0)`) {
		t.Fatalf("default policy rules %v", avail)
	}
	// A policy whose lowest state still needs budget left: anything below
	// it is exhausted.
	cp := *slos[0]
	cp.Policy = slo.Policy{States: []slo.PolicyState{{Name: "ok", MinRemaining: 0.5}, {Name: "low", MinRemaining: 0.1}}}
	g, _ := Generate([]*slo.SLO{&cp}, Options{})
	rs := g.Groups[0].Rules
	if last := rs[len(rs)-1-len(cp.Alerting.Severities)]; !strings.Contains(string(last.Expr), "< 0.1)") || last.Labels["state"] != "exhausted" {
		t.Fatalf("exhausted rule %+v", last)
	}
}

func TestAlertingDisabledAndErrors(t *testing.T) {
	s := exampleSLOs(t)[0]
	cp := *s
	cp.Alerting.Enabled = false
	f, err := Generate([]*slo.SLO{&cp}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range f.Groups[0].Rules {
		if r.Alert != "" {
			t.Fatal("alerts generated for a disabled SLO")
		}
	}
	broken := *s
	broken.Indicator.Bad, broken.Indicator.Good = nil, nil
	if _, err := Generate([]*slo.SLO{&broken}, Options{}); err == nil {
		t.Fatal("expected error for an indicator without good/bad")
	}
	if ErrorRatioRecord(30*24*time.Hour) != "slo:sli_error:ratio_rate30d" {
		t.Fatal("record name")
	}
}
