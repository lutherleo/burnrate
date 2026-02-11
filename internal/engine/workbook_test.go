package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/lutherleo/burnrate/internal/burnmath"
	"github.com/lutherleo/burnrate/internal/series"
	"github.com/lutherleo/burnrate/internal/slo"
)

const day = 24 * time.Hour

var tBegin = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const workbookSLO = `apiVersion: burnrate/v1
kind: SLO
metadata:
  name: api-availability
spec:
  service: api
  timeWindow: [{duration: 30d, isRolling: true}]
  objectives: [{target: 0.999}]
  indicator:
    ratio:
      total: reqs_total{job="api"}
      bad: reqs_total{job="api", code=~"5.."}
`

func mustSLO(t testing.TB, doc string) *slo.SLO {
	t.Helper()
	s, err := slo.Parse([]byte(doc), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s[0]
}

// feeder emulates a cumulative OTLP exporter for one service: a 200 series
// that always exists and a 500 series created lazily on the first error, as
// an OpenTelemetry SDK does. All series share the SDK start time.
type feeder struct {
	t           testing.TB
	e           *Engine
	now         time.Time
	rate        float64
	good, bad   float64
	badExists   bool
	events      []Event
	stepsSoFar  int
	producerKey string
}

func newFeeder(t testing.TB, e *Engine, rate float64) *feeder {
	return &feeder{t: t, e: e, now: tBegin, rate: rate, producerKey: "job=api"}
}

func (f *feeder) step(errRatio float64) {
	f.now = f.now.Add(time.Minute)
	bad := errRatio * f.rate
	f.good += f.rate - bad
	f.bad += bad
	samples := []Sample{f.sample("200", f.good)}
	if bad > 0 {
		f.badExists = true
	}
	if f.badExists {
		samples = append(samples, f.sample("500", f.bad))
	}
	f.e.Ingest(samples)
	f.events = append(f.events, f.e.Advance(f.now)...)
	f.stepsSoFar++
}

func (f *feeder) run(d time.Duration, errRatio float64) {
	for i := 0; i < int(d/time.Minute); i++ {
		f.step(errRatio)
	}
}

func (f *feeder) sample(code string, v float64) Sample {
	return Sample{
		Metric: "reqs_total", Labels: series.FromMap(map[string]string{"job": "api", "code": code}),
		Value: v, Time: f.now, Start: tBegin, Producer: f.producerKey,
	}
}

// transitions returns, per severity, the times (relative to origin) at which
// the alert fired and resolved.
func (f *feeder) transitions(origin time.Time) map[string][]string {
	out := map[string][]string{}
	for _, ev := range f.events {
		switch ev.Kind {
		case EventAlertFiring, EventAlertResolved:
			out[ev.Severity] = append(out[ev.Severity], fmt.Sprintf("%s@%s", ev.Kind, ev.Time.Sub(origin)))
		}
	}
	return out
}

// TestWorkbookDetectionAndResetTimes checks that the engine fires and clears
// the page and ticket alerts exactly when the SRE workbook arithmetic says it
// should, discretised to the 1-minute evaluation step:
//
//	fire  = first step strictly after  W * B * (1-O) / e      (after onset)
//	clear = first step at or after     S * (1 - B*(1-O)/e)    (after the end)
//
// The expectations come from burnmath's closed forms; the engine computes the
// same thing by counting events in ring buffers, so the two are independent.
func TestWorkbookDetectionAndResetTimes(t *testing.T) {
	cases := []struct {
		name     string
		ratio    float64
		incident time.Duration
		alerting []string // severities expected to fire
	}{
		{"full outage 10m", 1.0, 10 * time.Minute, []string{"page", "ticket"}},
		{"full outage 2m", 1.0, 2 * time.Minute, []string{"page"}},
		{"5% errors for 2h", 0.05, 2 * time.Hour, []string{"page", "ticket"}},
		{"2.5% errors for 4h", 0.025, 4 * time.Hour, []string{"page", "ticket"}},
		{"0.7% errors for 12h", 0.007, 12 * time.Hour, []string{"page", "ticket"}},
		{"0.21% slow burn for 36h", 0.0021, 36 * time.Hour, []string{"ticket"}},
		{"0.09% under budget for 6h", 0.0009, 6 * time.Hour, nil},
	}
	def := mustSLO(t, workbookSLO)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, err := New(Config{}, []*slo.SLO{def})
			if err != nil {
				t.Fatal(err)
			}
			f := newFeeder(t, e, 1000)
			f.run(3*day+time.Hour, 0) // clean history longer than the 3d window
			onset := f.now
			f.run(c.incident, c.ratio)
			end := f.now
			f.run(7*time.Hour, 0) // longer than the slowest reset (6h window)

			got := f.transitions(onset)
			if len(got) != len(c.alerting) {
				t.Fatalf("severities that fired: %v, want %v", got, c.alerting)
			}
			for _, sev := range c.alerting {
				if len(got[sev]) != 2 {
					t.Fatalf("%s should fire and resolve once, got %v", sev, got[sev])
				}
			}
			for _, sev := range def.Alerting.Severities {
				want := expectedTransitions(sev, def.Objective, c.ratio, c.incident, end.Sub(onset))
				if fmt.Sprint(got[sev.Name]) != fmt.Sprint(want) {
					t.Errorf("%s: got %v, want %v", sev.Name, got[sev.Name], want)
				}
			}
		})
	}
}

func expectedTransitions(sev slo.Severity, objective, ratio float64, incident, endOffset time.Duration) []string {
	detect, ok := burnmath.SeverityDetectionTime(sev.Pairs(), objective, ratio)
	if !ok || detect >= incident {
		return nil
	}
	fire := detect.Truncate(time.Minute) + time.Minute
	reset, _ := burnmath.SeverityResetTime(sev.Pairs(), objective, ratio, incident)
	clear := endOffset + ceilMinute(reset)
	return []string{
		fmt.Sprintf("%s@%s", EventAlertFiring, fire),
		fmt.Sprintf("%s@%s", EventAlertResolved, clear),
	}
}

func ceilMinute(d time.Duration) time.Duration {
	if r := d.Truncate(time.Minute); r < d {
		return r + time.Minute
	}
	return d
}

// TestWorkbookHeadlineNumbers pins a few values readers can check by hand.
func TestWorkbookHeadlineNumbers(t *testing.T) {
	def := mustSLO(t, workbookSLO)
	e, _ := New(Config{}, []*slo.SLO{def})
	f := newFeeder(t, e, 1000)
	f.run(3*day+time.Hour, 0)
	onset := f.now
	f.run(10*time.Minute, 1.0)
	f.run(7*time.Hour, 0)
	got := f.transitions(onset)
	// Total outage: 1h*14.4*0.001 = 51.8s -> pages at the first minute; the
	// 6h/30m pair also trips (after 2.16m), so the page holds until the
	// 30m short window drains: 10m + 30m.
	if want := "[alert_firing@1m0s alert_resolved@40m0s]"; fmt.Sprint(got["page"]) != want {
		t.Errorf("page %v, want %s", got["page"], want)
	}
	// Ticket: 1d*3*0.001 = 4.32m -> fires at 5m, holds until the 6h short
	// window of the 3d pair drains: 10m + 6h.
	if want := "[alert_firing@5m0s alert_resolved@6h10m0s]"; fmt.Sprint(got["ticket"]) != want {
		t.Errorf("ticket %v, want %s", got["ticket"], want)
	}
}
