package slo

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

func TestLoadExamples(t *testing.T) {
	slos, err := LoadPaths("../../examples/slos")
	if err != nil {
		t.Fatal(err)
	}
	if len(slos) != 3 {
		t.Fatalf("got %d SLOs", len(slos))
	}
	byName := map[string]*SLO{}
	for _, s := range slos {
		byName[s.Name] = s
	}

	avail := byName["checkout-availability"]
	if avail.Objective != 0.999 || avail.Period != 30*day || avail.Indicator.Kind != KindRatio {
		t.Fatalf("availability resolved wrong: %+v", avail)
	}
	if avail.Indicator.Bad == nil || avail.Indicator.Good != nil {
		t.Fatal("availability should be a bad-events ratio")
	}
	if len(avail.Alerting.Severities) != 2 || avail.Alerting.Severities[0].Name != "page" {
		t.Fatalf("severities %+v", avail.Alerting.Severities)
	}
	wantBurns := []float64{14.4, 6, 3, 1}
	var got []float64
	for _, sev := range avail.Alerting.Severities {
		for _, w := range sev.Windows {
			got = append(got, w.BurnRate)
		}
	}
	for i := range wantBurns {
		if math.Abs(got[i]-wantBurns[i]) > 1e-9 {
			t.Fatalf("burn rates %v, want %v", got, wantBurns)
		}
	}
	wantWindows := []time.Duration{5 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 6 * time.Hour, day, 3 * day, 30 * day}
	if w := avail.Windows(); len(w) != len(wantWindows) {
		t.Fatalf("windows %v", w)
	} else {
		for i := range w {
			if w[i] != wantWindows[i] {
				t.Fatalf("windows %v, want %v", w, wantWindows)
			}
		}
	}
	if avail.Alerting.Annotations["runbook_url"] == "" || avail.Labels["team"] != "payments" {
		t.Fatal("labels/annotations lost")
	}

	lat := byName["checkout-latency"]
	if lat.Indicator.Kind != KindLatency || lat.Indicator.Threshold != 0.25 {
		t.Fatalf("latency %+v", lat.Indicator)
	}
	if got := lat.Indicator.Good.String(); got != `http_server_request_duration_seconds_bucket{job="checkout", http_response_status_code!~"5..", le="0.25"}` {
		t.Fatalf("good selector %s", got)
	}
	if got := lat.Indicator.Total.String(); got != `http_server_request_duration_seconds_count{job="checkout", http_response_status_code!~"5.."}` {
		t.Fatalf("total selector %s", got)
	}
	if st, _ := lat.Policy.Evaluate(0.2); st != "at-risk" {
		t.Fatalf("custom policy state %s", st)
	}

	search := byName["search-availability"]
	if search.Alerting.Name != "SearchBudgetBurn" || search.Period != 7*day {
		t.Fatal("search alerting")
	}
	page := search.Alerting.Severities[0].Windows[0]
	if page.BurnRate != 10 || math.Abs(page.BudgetConsumed-10.0/168) > 1e-12 {
		t.Fatalf("explicit burn rate resolved to %+v", page)
	}
	if search.ErrorBudget() < 0.004999 || search.ErrorBudget() > 0.005001 {
		t.Fatal("error budget")
	}
}

func TestPolicyEvaluate(t *testing.T) {
	p := Policy{States: []PolicyState{{"healthy", 0.5, "a"}, {"warning", 0.25, "b"}, {"critical", 0, "c"}}, ExhaustedAction: "freeze"}
	cases := map[float64]string{1: "healthy", 0.5: "healthy", 0.49: "warning", 0.25: "warning", 0.1: "critical", 0: StateExhausted, -3: StateExhausted, math.NaN(): StateNoData}
	for in, want := range cases {
		if got, _ := p.Evaluate(in); got != want {
			t.Errorf("Evaluate(%v) = %s, want %s", in, got, want)
		}
	}
	// Remaining budget below every configured threshold is exhausted.
	q := Policy{States: []PolicyState{{"ok", 0.5, ""}}, ExhaustedAction: "x"}
	if st, act := q.Evaluate(0.2); st != StateExhausted || act != "x" {
		t.Fatalf("got %s %s", st, act)
	}
}

const validDoc = `apiVersion: burnrate/v1
kind: SLO
metadata:
  name: api
spec:
  service: api
  timeWindow: [{duration: 30d, isRolling: true}]
  objectives: [{target: 0.999}]
  indicator:
    ratio:
      total: reqs_total{job="api"}
      bad: reqs_total{job="api", code=~"5.."}
`

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name, old, new, want string
	}{
		{"apiVersion", "apiVersion: burnrate/v1", "apiVersion: openslo/v1", `apiVersion must be "burnrate/v1"`},
		{"kind", "kind: SLO", "kind: Service", `kind must be "SLO"`},
		{"name", "name: api", "name: API_Bad", "must be a DNS label"},
		{"service", "service: api", `service: ""`, "spec.service is required"},
		{"target", "0.999", "1.5", "must be in (0, 1)"},
		{"period", "30d", "30m", "between 1h and 400d"},
		{"bad period", "30d", "30x", "unknown unit"},
		{"calendar", "isRolling: true", "isRolling: false", "calendar-aligned"},
		{"no window", "timeWindow: [{duration: 30d, isRolling: true}]", "timeWindow: []", "exactly one entry"},
		{"two windows", "timeWindow: [{duration: 30d, isRolling: true}]", "timeWindow: [{duration: 30d}, {duration: 7d}]", "only one time window"},
		{"no objective", "objectives: [{target: 0.999}]", "objectives: []", "exactly one entry"},
		{"two objectives", "objectives: [{target: 0.999}]", "objectives: [{target: 0.99}, {target: 0.9}]", "only one objective"},
		{"selector", `total: reqs_total{job="api"}`, `total: reqs_total{job=}`, "indicator.ratio.total"},
		{"both", "      bad:", "      good: x\n      bad:", "exactly one of good and bad"},
		{"method", "service: api", "service: api\n  budgetingMethod: Timeslices", "only Occurrences"},
		{"unknown field", "service: api", "service: api\n  bogus: 1", "field bogus not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(validDoc, c.old) {
				t.Fatalf("pattern %q not in doc", c.old)
			}
			doc := strings.Replace(validDoc, c.old, strings.ReplaceAll(c.new, `\n`, "\n"), 1)
			_, err := Parse([]byte(doc), "test.yaml")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q\n%s", err, c.want, doc)
			}
		})
	}
	if _, err := Parse([]byte(validDoc), "ok.yaml"); err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}
}

func TestAlertingValidation(t *testing.T) {
	cases := []struct {
		name, alerting, want string
	}{
		{"neither burn nor budget", `{windows: [{severity: page, longWindow: 1h, shortWindow: 5m}]}`, "exactly one of burnRate and budgetConsumed"},
		{"both", `{windows: [{severity: page, longWindow: 1h, shortWindow: 5m, burnRate: 2, budgetConsumed: 0.1}]}`, "exactly one of burnRate"},
		{"short>=long", `{windows: [{severity: page, longWindow: 1h, shortWindow: 1h, burnRate: 2}]}`, "0 < shortWindow < longWindow"},
		{"long>period", `{windows: [{severity: page, longWindow: 31d, shortWindow: 1h, burnRate: 2}]}`, "exceeds the SLO period"},
		{"impossible", `{windows: [{severity: page, longWindow: 1h, shortWindow: 5m, burnRate: 1500}]}`, "can never fire"},
		{"negative", `{windows: [{severity: page, longWindow: 1h, shortWindow: 5m, burnRate: -2}]}`, "must be positive"},
		{"severity", `{windows: [{severity: "", longWindow: 1h, shortWindow: 5m, burnRate: 2}]}`, "severity"},
		{"reserved label", `{labels: {slo: x}}`, "reserved"},
		{"bad annotation", `{annotations: {"bad-name": x}}`, "not a valid annotation name"},
		{"alert name", `{name: "bad name"}`, "not a valid alert name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := validDoc + "  alerting: " + c.alerting + "\n"
			_, err := Parse([]byte(doc), "test.yaml")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestLatencyValidation(t *testing.T) {
	base := strings.Replace(validDoc, `    ratio:
      total: reqs_total{job="api"}
      bad: reqs_total{job="api", code=~"5.."}
`, "    latency: LAT\n", 1)
	cases := map[string]string{
		`{histogram: 'h_bucket{job="a"}', threshold: 0.3}`:  "without \"_bucket\"",
		`{histogram: 'h{job="a", le="1"}', threshold: 0.3}`: "do not match on le",
		`{histogram: 'h{job="a"}', threshold: 0}`:           "positive bucket bound",
		`{histogram: 'h{job="a"}', threshold: .inf}`:        "positive bucket bound",
		`{threshold: 1}`:                  "histogram is required",
		`{histogram: 'h{', threshold: 1}`: "indicator.latency.histogram",
	}
	for lat, want := range cases {
		doc := strings.Replace(base, "LAT", lat, 1)
		_, err := Parse([]byte(doc), "lat.yaml")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", lat, err, want)
		}
	}
	ok := strings.Replace(base, "LAT", `{histogram: 'h{job="a"}', threshold: 1}`, 1)
	s, err := Parse([]byte(ok), "lat.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if s[0].Indicator.Good.String() != `h_bucket{job="a", le="1"}` {
		t.Fatalf("integer bound rendered as %s", s[0].Indicator.Good)
	}
}

func TestIndicatorShape(t *testing.T) {
	noInd := strings.Replace(validDoc, `    ratio:
      total: reqs_total{job="api"}
      bad: reqs_total{job="api", code=~"5.."}
`, "    {}\n", 1)
	if _, err := Parse([]byte(noInd), "x.yaml"); err == nil || !strings.Contains(err.Error(), "set one of ratio or latency") {
		t.Fatalf("err = %v", err)
	}
	both := validDoc + "    latency: {histogram: h, threshold: 1}\n"
	if _, err := Parse([]byte(both), "x.yaml"); err == nil || !strings.Contains(err.Error(), "exactly one of ratio and latency") {
		t.Fatalf("err = %v", err)
	}
	noTotal := strings.Replace(validDoc, `      total: reqs_total{job="api"}
`, "", 1)
	if _, err := Parse([]byte(noTotal), "x.yaml"); err == nil || !strings.Contains(err.Error(), "total is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestPolicyValidation(t *testing.T) {
	cases := map[string]string{
		`{states: []}`: "at least one state",
		`{states: [{name: a, minRemaining: 0.5}, {name: b, minRemaining: 0.6}]}`: "lower than the previous",
		`{states: [{name: exhausted, minRemaining: 0.5}]}`:                       "reserved",
		`{states: [{name: a, minRemaining: 0.5}, {name: a, minRemaining: 0.2}]}`: "duplicated",
		`{states: [{minRemaining: 0.5}]}`:                                        "name is required",
		`{states: [{name: a, minRemaining: 1}]}`:                                 "[0, 1)",
	}
	for pol, want := range cases {
		doc := validDoc + "  budgetPolicy: " + pol + "\n"
		if _, err := Parse([]byte(doc), "p.yaml"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", pol, err, want)
		}
	}
}

func TestMetadataLabels(t *testing.T) {
	doc := strings.Replace(validDoc, "  name: api\n", "  name: api\n  labels: {slo: x, \"bad-label\": y, team: z}\n", 1)
	_, err := Parse([]byte(doc), "l.yaml")
	if err == nil || !strings.Contains(err.Error(), `"slo" is reserved`) || !strings.Contains(err.Error(), `"bad-label" is not a valid`) {
		t.Fatalf("err = %v", err)
	}
}

func TestShortPeriodDropsDefaultPairs(t *testing.T) {
	doc := strings.Replace(validDoc, "30d", "1d", 1)
	s, err := Parse([]byte(doc), "short.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, sev := range s[0].Alerting.Severities {
		for _, w := range sev.Windows {
			if w.Long > day {
				t.Fatalf("pair %v kept for a 1d period", w)
			}
		}
	}
}

func TestMultiDocAndDuplicates(t *testing.T) {
	two := validDoc + "---\n" + strings.Replace(validDoc, "name: api", "name: api2", 1) + "---\n"
	s, err := Parse([]byte(two), "multi.yaml")
	if err != nil || len(s) != 2 {
		t.Fatalf("got %d, %v", len(s), err)
	}
	dup := validDoc + "---\n" + validDoc
	if _, err := Parse([]byte(dup), "dup.yaml"); err == nil || !strings.Contains(err.Error(), "duplicate SLO name") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Parse([]byte("apiVersion: [unclosed"), "bad.yaml"); err == nil {
		t.Fatal("expected YAML error")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(validDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.yml"), []byte(validDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPaths(dir); err == nil || !strings.Contains(err.Error(), "duplicate SLO name") {
		t.Fatalf("cross-file duplicate: %v", err)
	}
	if _, err := LoadPaths(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no SLO files") {
		t.Fatalf("empty dir: %v", err)
	}
	if _, err := LoadPaths(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing file")
	}
	if s, err := LoadPaths(filepath.Join(dir, "a.yaml")); err != nil || len(s) != 1 {
		t.Fatalf("single file: %v", err)
	}
}

func TestDurations(t *testing.T) {
	ok := map[string]time.Duration{
		"5m": 5 * time.Minute, "1h30m": 90 * time.Minute, "30d": 30 * day, "4w": 28 * day,
		"1d12h": 36 * time.Hour, "250ms": 250 * time.Millisecond, "1m1s1ms": time.Minute + time.Second + time.Millisecond,
	}
	for in, want := range ok {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "5", "m", "5x", "1m1h", "1h1h", "1y", "99999999999999999999s", "9223372036w", "-5m"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) should fail", bad)
		}
	}
	fmts := map[time.Duration]string{0: "0s", 30 * day: "30d", 90 * time.Minute: "1h30m", 28 * day: "28d", -time.Minute: "-1m", 1500 * time.Millisecond: "1s500ms"}
	for in, want := range fmts {
		if got := FormatDuration(in); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", in, got, want)
		}
	}
	var d Duration
	if err := d.UnmarshalText([]byte("6h")); err != nil || time.Duration(d) != 6*time.Hour {
		t.Fatal("UnmarshalText")
	}
	if err := d.UnmarshalText([]byte("bogus")); err == nil {
		t.Fatal("UnmarshalText should fail")
	}
	if b, _ := d.MarshalText(); string(b) != "6h" {
		t.Fatalf("MarshalText = %s", b)
	}
}
