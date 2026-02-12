package report

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/lutherleo/burnrate/internal/engine"
	"github.com/lutherleo/burnrate/internal/slo"
)

func TestFormatters(t *testing.T) {
	offsets := map[time.Duration]string{
		0:                          "+00h00m",
		90 * time.Minute:           "+01h30m",
		26*time.Hour + time.Minute: "+1d02h01m",
		-5 * time.Minute:           "-00h05m",
	}
	for d, want := range offsets {
		if got := Offset(d); got != want {
			t.Errorf("Offset(%s) = %q, want %q", d, got, want)
		}
	}
	if Percent(0.381) != "38.1%" || Percent(-1.446) != "-144.6%" || Percent(math.NaN()) != "n/a" || Percent(math.Inf(1)) != "n/a" {
		t.Error("Percent")
	}
	nums := map[float64]string{0: "0", 0.344: "0.34", 14.44: "14.4", 1000: "1000", -250: "-250", math.NaN(): "n/a"}
	for f, want := range nums {
		if got := Num(f); got != want && !(math.IsNaN(f) && got == "n/a") {
			t.Errorf("Num(%g) = %q, want %q", f, got, want)
		}
	}
	humans := []struct {
		d    time.Duration
		ok   bool
		want string
	}{
		{0, false, "never"},
		{51800 * time.Millisecond, true, "51.8s"},
		{30 * time.Minute, true, "30.0m"},
		{36 * time.Hour, true, "36.0h"},
		{72 * time.Hour, true, "3.0d"},
	}
	for _, c := range humans {
		if got := human(c.d, c.ok); got != c.want {
			t.Errorf("human(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestEventsAndStatus(t *testing.T) {
	origin := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pairs := []engine.PairStatus{
		{Long: "1h", Short: "5m", BurnThreshold: 14.4, LongBurn: 16.6, ShortBurn: 192, Active: true},
		{Long: "6h", Short: "30m", BurnThreshold: 6, LongBurn: 2.8, ShortBurn: 33, Active: false},
	}
	events := []engine.Event{
		{Time: origin.Add(26*time.Hour + time.Minute), Kind: engine.EventAlertFiring, SLO: "api", Severity: "page", Pairs: pairs},
		{Time: origin.Add(27 * time.Hour), Kind: engine.EventBudgetState, SLO: "api", FromState: "healthy", ToState: "warning", BudgetRemaining: 0.381},
		{Time: origin.Add(28 * time.Hour), Kind: engine.EventAlertResolved, SLO: "api", Severity: "page", BudgetRemaining: -0.5},
		{Time: origin.Add(29 * time.Hour), Kind: "custom", SLO: "api"},
	}
	var b bytes.Buffer
	if err := Events(&b, events, origin); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"+1d02h01m  FIRING page",
		"1h/5m burn 16.6/192 > 14.4x",
		"healthy -> warning (left 38.1%)",
		"resolved page",
		"budget left -50.0%",
		"custom",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("events output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "6h/30m") {
		t.Errorf("inactive pair rendered:\n%s", out)
	}

	statuses := []engine.Status{{
		SLO: "api", Objective: 0.999,
		Windows: []engine.WindowStatus{{Window: "5m", BurnRate: 0.34}, {Window: "30d", BurnRate: 1}},
		Alerts:  []engine.AlertStatus{{Severity: "page", Firing: true}, {Severity: "ticket"}},
		Budget:  engine.BudgetStatus{Period: "30d", Remaining: Ratio(0.5), State: "healthy"},
	}, {
		SLO: "idle", Objective: 0.99,
		Budget: engine.BudgetStatus{Period: "7d", Remaining: Ratio(math.NaN()), State: "no-data"},
	}}
	b.Reset()
	if err := Status(&b, statuses); err != nil {
		t.Fatal(err)
	}
	out = b.String()
	if !strings.Contains(out, "api   99.9%   30d     50.0%        healthy  page    5m 0.34") {
		t.Errorf("status row:\n%s", out)
	}
	if strings.Contains(out, "30d 1.00") {
		t.Errorf("period window should be omitted from burn list:\n%s", out)
	}
	if !strings.Contains(out, "n/a") || !strings.Contains(out, "no-data") {
		t.Errorf("idle row:\n%s", out)
	}
}

// Ratio converts a float to the engine's JSON-safe ratio.
func Ratio(f float64) engine.Ratio { return engine.Ratio(f) }

const mathSLO = `apiVersion: burnrate/v1
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

func TestMathReproducesWorkbookTable(t *testing.T) {
	s, err := slo.Parse([]byte(mathSLO), "api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := Math(&b, s[0], DefaultRatios); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// The workbook's table 5-8 numbers for a 99.9% SLO: 14.4x over 1h
	// spends 2% of budget; a 100% outage pages after 0.864 minutes.
	for _, want := range []string{
		"page      1h/5m       14.4       1.4%                   2.0%",
		"ticket    3d/6h       1          0.1%                   10.0%",
		"100.0%       1000x   51.8s",
		"never",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("math output lacks %q:\n%s", want, out)
		}
	}
	if err := Math(failWriter{}, s[0], DefaultRatios); err == nil {
		t.Error("write error not reported")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
