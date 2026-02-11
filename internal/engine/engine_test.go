package engine

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/lutherleo/burnrate/internal/series"
	"github.com/lutherleo/burnrate/internal/slo"
)

func lbls(kv ...string) series.Labels {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return series.FromMap(m)
}

func at(m int) time.Time { return tBegin.Add(time.Duration(m) * time.Minute) }

func newEngine(t *testing.T, cfg Config, docs ...string) *Engine {
	t.Helper()
	var slos []*slo.SLO
	for _, d := range docs {
		slos = append(slos, mustSLO(t, d))
	}
	e, err := New(cfg, slos)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func window5m(t *testing.T, e *Engine) WindowStatus {
	t.Helper()
	w, ok := e.Status()[0].Window(5 * time.Minute)
	if !ok {
		t.Fatal("no 5m window")
	}
	return w
}

func TestCounterResetsAndOrdering(t *testing.T) {
	e := newEngine(t, Config{}, workbookSLO)
	s := func(m int, v float64, start time.Time) Sample {
		return Sample{Metric: "reqs_total", Labels: lbls("job", "api", "code", "200"), Value: v, Time: at(m), Start: start}
	}
	e.Ingest([]Sample{s(1, 100, time.Time{})}) // baseline: unknown start
	e.Ingest([]Sample{s(2, 150, time.Time{})}) // +50
	e.Ingest([]Sample{s(3, 20, time.Time{})})  // reset: +20
	e.Ingest([]Sample{s(3, 30, time.Time{})})  // duplicate timestamp: ignored
	e.Ingest([]Sample{s(2, 10, time.Time{})})  // out of order: ignored
	e.Ingest([]Sample{s(4, 25, time.Time{})})  // +5
	e.Advance(at(5))
	if got := window5m(t, e).Total; got != 75 {
		t.Fatalf("total = %v, want 75", got)
	}
	st := e.Stats()
	if st.CounterResets != 1 || st.SamplesOutOfOrder != 2 || st.SamplesMatched != 6 {
		t.Fatalf("stats %+v", st)
	}
}

func TestStartTimeChangeIsAReset(t *testing.T) {
	e := newEngine(t, Config{}, workbookSLO)
	first, restart := at(0), at(3)
	mk := func(m int, v float64, start time.Time) Sample {
		return Sample{Metric: "reqs_total", Labels: lbls("job", "api", "code", "200"), Value: v, Time: at(m), Start: start, Producer: "p"}
	}
	e.Ingest([]Sample{mk(1, 100, first), mk(2, 200, first)}) // first value baselined, +100
	// The process restarted at minute 3 and has already counted 250 by
	// minute 4: higher than before, but the new start time proves a reset.
	e.Ingest([]Sample{mk(4, 250, restart)})
	e.Advance(at(5))
	if got := window5m(t, e).Total; got != 350 {
		t.Fatalf("total = %v, want 350", got)
	}
}

func TestFirstValueRules(t *testing.T) {
	cases := []struct {
		name  string
		start time.Time
		pkey  string
		want  float64
	}{
		// The engine first saw data at minute 1 from producer "p" (start 0).
		{"unknown start is a baseline", time.Time{}, "p", 0},
		{"stream started before we watched, new producer", at(0), "other", 0},
		{"series appeared later on a known producer", at(0), "p", 40},
		{"process started after we began watching", at(2), "fresh", 40},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEngine(t, Config{}, workbookSLO)
			e.Ingest([]Sample{{Metric: "reqs_total", Labels: lbls("job", "api", "code", "200"), Value: 5, Time: at(1), Start: at(0), Producer: "p"}})
			e.Ingest([]Sample{{Metric: "reqs_total", Labels: lbls("job", "api", "code", "500"), Value: 40, Time: at(3), Start: c.start, Producer: c.pkey}})
			e.Advance(at(4))
			if got := window5m(t, e).Bad; got != c.want {
				t.Fatalf("bad = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDeltaTemporality(t *testing.T) {
	e := newEngine(t, Config{}, workbookSLO)
	for m := 1; m <= 5; m++ {
		e.Ingest([]Sample{
			{Metric: "reqs_total", Labels: lbls("job", "api", "code", "200"), Value: 90, Time: at(m), Temporality: Delta},
			{Metric: "reqs_total", Labels: lbls("job", "api", "code", "503"), Value: 10, Time: at(m), Temporality: Delta},
		})
	}
	e.Advance(at(5))
	w := window5m(t, e)
	if w.Total != 500 || w.Bad != 50 || float64(w.ErrorRatio) != 0.1 {
		t.Fatalf("window %+v", w)
	}
	if math.Abs(float64(w.BurnRate)-100) > 1e-6 {
		t.Fatalf("burn = %v", w.BurnRate)
	}
}

func TestRejectsInvalidOldAndFutureSamples(t *testing.T) {
	now := at(60 * 24 * 40) // 40 days in
	e := newEngine(t, Config{Now: func() time.Time { return now }}, workbookSLO)
	base := Sample{Metric: "reqs_total", Labels: lbls("job", "api", "code", "200"), Temporality: Delta}
	good := base
	good.Value, good.Time = 1, now
	nan := base
	nan.Value, nan.Time = math.NaN(), now
	neg := base
	neg.Value, neg.Time = -1, now
	zero := base
	zero.Value = 1
	future := base
	future.Value, future.Time = 1, now.Add(time.Hour)
	old := base
	old.Value, old.Time = 1, now.Add(-35*day)
	other := base
	other.Metric, other.Value, other.Time = "unrelated_total", 1, now
	e.Ingest([]Sample{good, nan, neg, zero, future, old, other})
	st := e.Stats()
	if st.SamplesInvalid != 3 || st.SamplesFuture != 1 || st.SamplesTooOld != 1 || st.SamplesReceived != 7 || st.SamplesMatched != 2 {
		t.Fatalf("stats %+v", st)
	}
	if !e.Wants("reqs_total") || e.Wants("unrelated_total") {
		t.Fatal("Wants")
	}
	if e.Watermark() != now {
		t.Fatalf("watermark %v", e.Watermark())
	}
}

func TestSeriesEviction(t *testing.T) {
	e := newEngine(t, Config{SeriesTTL: 10 * time.Minute}, workbookSLO)
	e.Ingest([]Sample{{Metric: "reqs_total", Labels: lbls("job", "api", "code", "200"), Value: 1, Time: at(1), Start: at(0), Producer: "p"}})
	e.Advance(at(1))
	if e.Stats().SeriesTracked != 1 {
		t.Fatal("series not tracked")
	}
	e.Advance(at(30))
	st := e.Stats()
	if st.SeriesTracked != 0 || st.SeriesEvicted != 1 {
		t.Fatalf("stats %+v", st)
	}
	if len(e.producers) != 0 {
		t.Fatal("producer state not evicted")
	}
}

const goodRatioSLO = `apiVersion: burnrate/v1
kind: SLO
metadata: {name: good-ratio}
spec:
  service: api
  timeWindow: [{duration: 1d}]
  objectives: [{target: 0.99}]
  indicator:
    ratio:
      total: reqs_total{job="api"}
      good: reqs_total{job="api", code="200"}
  alerting: {disabled: true}
  budgetPolicy:
    states:
      - {name: healthy, minRemaining: 0.5, action: ship}
      - {name: low, minRemaining: 0, action: slow down}
    exhaustedAction: freeze
`

func TestGoodRatioBudgetAndPolicyStates(t *testing.T) {
	e := newEngine(t, Config{}, goodRatioSLO)
	// 1000 events per minute; minutes 1-10 clean, 11-12 with 2% errors.
	var events []Event
	cum := map[string]float64{}
	for m := 1; m <= 14; m++ {
		bad := 0.0
		if m > 10 {
			bad = 20
		}
		cum["200"] += 1000 - bad
		cum["500"] += bad
		e.Ingest([]Sample{
			{Metric: "reqs_total", Labels: lbls("job", "api", "code", "200"), Value: cum["200"], Time: at(m), Start: tBegin, Producer: "p"},
			{Metric: "reqs_total", Labels: lbls("job", "api", "code", "500"), Value: cum["500"], Time: at(m), Start: tBegin, Producer: "p"},
		})
		events = append(events, e.Advance(at(m))...)
	}
	st := e.Status()[0]
	// Both series start at tBegin, before the first sample the engine saw,
	// so their first values are baselines: minutes 2-14 are counted.
	if st.Budget.Total != 13000 || st.Budget.Bad != 80 {
		t.Fatalf("budget %+v", st.Budget)
	}
	// Allowed = 1% of 13000 = 130; consumed 80/130.
	if math.Abs(float64(st.Budget.Remaining)-(1-80.0/130)) > 1e-9 || st.Budget.State != "low" || st.Budget.Action != "slow down" {
		t.Fatalf("budget %+v", st.Budget)
	}
	if len(st.Alerts) != 0 {
		t.Fatal("alerting disabled")
	}
	var states []string
	for _, ev := range events {
		if ev.Kind == EventBudgetState {
			states = append(states, ev.FromState+"->"+ev.ToState)
		}
	}
	if strings.Join(states, ",") != "no-data->healthy,healthy->low" {
		t.Fatalf("budget transitions %v", states)
	}
	// Push it over the edge.
	for m := 15; m <= 20; m++ {
		cum["500"] += 1000
		e.Ingest([]Sample{{Metric: "reqs_total", Labels: lbls("job", "api", "code", "500"), Value: cum["500"], Time: at(m), Start: tBegin, Producer: "p"}})
		e.Advance(at(m))
	}
	if got := e.Status()[0].Budget; got.State != slo.StateExhausted || got.Action != "freeze" {
		t.Fatalf("budget %+v", got)
	}
}

const latencySLO = `apiVersion: burnrate/v1
kind: SLO
metadata: {name: lat}
spec:
  service: api
  timeWindow: [{duration: 30d}]
  objectives: [{target: 0.99}]
  indicator:
    latency:
      histogram: dur_seconds{job="api"}
      threshold: 0.25
`

func TestLatencyIndicatorReadsBucketAndCount(t *testing.T) {
	e := newEngine(t, Config{}, latencySLO)
	for m := 1; m <= 5; m++ {
		e.Ingest([]Sample{
			{Metric: "dur_seconds_count", Labels: lbls("job", "api"), Value: 100, Time: at(m), Temporality: Delta},
			{Metric: "dur_seconds_bucket", Labels: lbls("job", "api", "le", "0.1"), Value: 50, Time: at(m), Temporality: Delta},
			{Metric: "dur_seconds_bucket", Labels: lbls("job", "api", "le", "0.25"), Value: 80, Time: at(m), Temporality: Delta},
			{Metric: "dur_seconds_bucket", Labels: lbls("job", "api", "le", "+Inf"), Value: 100, Time: at(m), Temporality: Delta},
		})
	}
	e.Advance(at(5))
	st := e.Status()[0]
	w, _ := st.Window(5 * time.Minute)
	if w.Total != 500 || w.Bad != 100 || math.Abs(float64(w.ErrorRatio)-0.2) > 1e-12 {
		t.Fatalf("window %+v", w)
	}
	if st.Indicator != "latency" {
		t.Fatal("indicator name")
	}
	// 20% errors against a 1% budget: every severity fires.
	if a, _ := st.Alert("page"); !a.Firing || a.Since == nil {
		t.Fatalf("page %+v", a)
	}
	if len(e.ActiveAlerts()) != 2 {
		t.Fatalf("active %+v", e.ActiveAlerts())
	}
	al := e.ActiveAlerts()[0]
	if al.Labels["severity"] != "page" || al.Labels["slo"] != "lat" || al.Name != slo.DefaultAlertName || al.Annotations["summary"] == "" {
		t.Fatalf("alert %+v", al)
	}
	if st.Budget.ExhaustionETA == "" {
		t.Fatal("expected an exhaustion ETA while burning")
	}
}

func TestNoDataAndJSON(t *testing.T) {
	e := newEngine(t, Config{}, workbookSLO)
	st := e.Status()[0]
	if st.Budget.State != slo.StateNoData || len(st.Windows) != 8 || len(st.Alerts) != 2 {
		t.Fatalf("empty status %+v", st)
	}
	if evs := e.Advance(at(10)); len(evs) != 0 {
		t.Fatalf("no data must not emit events: %+v", evs)
	}
	b, err := json.Marshal(e.Status())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"errorRatio":null`) || !strings.Contains(string(b), `"state":"no-data"`) {
		t.Fatalf("json %s", b)
	}
	var r Ratio
	if err := json.Unmarshal([]byte("null"), &r); err != nil || !math.IsNaN(float64(r)) {
		t.Fatal("null -> NaN")
	}
	if err := json.Unmarshal([]byte("0.5"), &r); err != nil || r != 0.5 {
		t.Fatal("number")
	}
	if err := json.Unmarshal([]byte(`"x"`), &r); err == nil {
		t.Fatal("string must fail")
	}
	if _, ok := st.Window(time.Second); ok {
		t.Fatal("unknown window")
	}
	if _, ok := st.Alert("nope"); ok {
		t.Fatal("unknown severity")
	}
}

func TestAdvanceSemantics(t *testing.T) {
	e := newEngine(t, Config{EventHistory: 3}, workbookSLO)
	if e.Resolution() != time.Minute {
		t.Fatal("default resolution")
	}
	e.Advance(at(10).Add(30 * time.Second)) // truncated to minute 10
	if e.LastEvaluation() != at(10) || e.Stats().Evaluations != 1 {
		t.Fatalf("last %v evals %d", e.LastEvaluation(), e.Stats().Evaluations)
	}
	if evs := e.Advance(at(9)); evs != nil || e.Stats().Evaluations != 1 {
		t.Fatal("going backwards must be a no-op")
	}
	e.Advance(at(13))
	if e.Stats().Evaluations != 4 {
		t.Fatalf("evaluations %d", e.Stats().Evaluations)
	}
	// A gap longer than the longest window (30d) only evaluates that much.
	e.Advance(at(13).Add(100 * day))
	if got := e.Stats().Evaluations; got != 4+30*24*60+1 {
		t.Fatalf("evaluations after gap %d", got)
	}
	// Event history is bounded.
	e.record(make([]Event, 5))
	if len(e.Events()) != 3 {
		t.Fatalf("history %d", len(e.Events()))
	}
}

func TestConfigErrors(t *testing.T) {
	def := mustSLO(t, workbookSLO)
	if _, err := New(Config{Resolution: 7 * time.Minute}, []*slo.SLO{def}); err == nil || !strings.Contains(err.Error(), "not a multiple") {
		t.Fatalf("err = %v", err)
	}
	if _, err := New(Config{Resolution: -time.Second}, []*slo.SLO{def}); err == nil {
		t.Fatal("negative resolution")
	}
}

func TestMultipleSLOsShareSeries(t *testing.T) {
	e := newEngine(t, Config{}, workbookSLO, goodRatioSLO)
	e.Ingest([]Sample{
		{Metric: "reqs_total", Labels: lbls("job", "api", "code", "200"), Value: 90, Time: at(1), Temporality: Delta},
		{Metric: "reqs_total", Labels: lbls("job", "api", "code", "500"), Value: 10, Time: at(1), Temporality: Delta},
	})
	e.Advance(at(1))
	for _, st := range e.Status() {
		if st.Budget.Total != 100 || st.Budget.Bad != 10 {
			t.Fatalf("%s budget %+v", st.SLO, st.Budget)
		}
	}
}

// TestEvictedSeriesReturnsAsBaseline is the regression test for a series that
// is evicted by SeriesTTL while its producer stays alive (an OpenTelemetry
// asynchronous instrument whose callback stops observing an attribute set),
// then reappears with the same start time. Its value is the stream's whole
// lifetime count, not new events: it must re-establish a baseline, as rate()
// would, instead of paging. Both first-value rules are covered: a stream that
// started before the engine began observing (its first 5000 is a baseline),
// and one that started after (its first 5000 counts once, legitimately).
func TestEvictedSeriesReturnsAsBaseline(t *testing.T) {
	cases := []struct {
		name  string
		start time.Time
	}{
		{"stream started before observation", tBegin.Add(-day)},
		{"stream started after observation began", tBegin.Add(30 * time.Second)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEngine(t, Config{}, workbookSLO)
			// Another process is the first thing the engine sees, so the
			// second case's stream genuinely starts after the epoch.
			e.Ingest([]Sample{{Metric: "reqs_total", Labels: lbls("job", "api", "code", "200", "instance", "b"), Value: 1, Time: tBegin, Start: tBegin.Add(-day), Producer: "b"}})
			var events []Event
			good := 0.0
			step := func(m int, withErrors bool) {
				good += 1000
				s := []Sample{{Metric: "reqs_total", Labels: lbls("job", "api", "code", "200"), Value: good, Time: at(m), Start: c.start, Producer: "a"}}
				if withErrors {
					s = append(s, Sample{Metric: "reqs_total", Labels: lbls("job", "api", "code", "500"), Value: 5000, Time: at(m), Start: c.start, Producer: "a"})
				}
				e.Ingest(s)
				events = append(events, e.Advance(at(m))...)
			}
			m := 1
			for ; m <= 60; m++ { // both series exported; the 500 series is flat
				step(m, true)
			}
			for ; m <= 180; m++ { // 500 not exported for 2h: evicted after 1h
				step(m, false)
			}
			if e.Stats().SeriesEvicted == 0 {
				t.Fatal("the 500 series was not evicted; the test does not exercise the bug")
			}
			before := e.Status()[0].Budget
			events = nil
			for ; m <= 240; m++ { // 500 comes back, still flat at 5000
				step(m, true)
			}
			for _, ev := range events {
				if ev.Kind == EventAlertFiring {
					t.Fatalf("alert fired with no new errors: %+v", ev)
				}
			}
			if after := e.Status()[0].Budget; after.Bad != before.Bad {
				t.Fatalf("bad events in budget went from %v to %v with no new errors", before.Bad, after.Bad)
			}
			// After the baseline, new errors on the returning series count.
			e.Ingest([]Sample{{Metric: "reqs_total", Labels: lbls("job", "api", "code", "500"), Value: 5007, Time: at(m), Start: c.start, Producer: "a"}})
			e.Advance(at(m))
			if got := window5m(t, e).Bad; got != 7 {
				t.Fatalf("bad after return = %v, want 7", got)
			}
		})
	}
}

// TestSilentProducerReturnsAsBaseline covers the producer-level variant: a
// process whose stream started after the engine began observing goes silent
// for longer than SeriesTTL (a network partition), so its producer state is
// retired, then exports again with the same start time. Its series carry
// lifetime counts and must re-establish baselines; a series it creates later
// still counts in full; and a restart (new start time) is new data.
func TestSilentProducerReturnsAsBaseline(t *testing.T) {
	e := newEngine(t, Config{SeriesTTL: 10 * time.Minute}, workbookSLO)
	e.Ingest([]Sample{{Metric: "reqs_total", Labels: lbls("job", "other"), Value: 1, Time: at(0), Start: at(-60), Producer: "other"}})
	start := at(1) // after the engine's epoch: first values count in full
	mk := func(m int, code string, v float64, start time.Time) Sample {
		return Sample{Metric: "reqs_total", Labels: lbls("job", "api", "code", code), Value: v, Time: at(m), Start: start, Producer: "p"}
	}
	e.Ingest([]Sample{mk(2, "200", 100, start), mk(2, "500", 3, start)})
	e.Advance(at(2))
	if got := window5m(t, e).Bad; got != 3 {
		t.Fatalf("bad = %v, want 3 (stream started after observation)", got)
	}
	// Silent for 40 minutes: series evicted, producer retired.
	e.Advance(at(42))
	pkey := "p\xff" + start.String()
	if p := e.retired[pkey]; len(e.producers) != 0 || p == nil || len(p.evicted) != 2 {
		t.Fatalf("producers %v, retired %+v: want the producer retired with 2 tombstones", e.producers, p)
	}
	// Back with the same start time and unchanged counts: nothing new. A
	// 503 series created during the partition is new and counts in full.
	e.Ingest([]Sample{mk(43, "200", 100, start), mk(43, "500", 3, start), mk(43, "503", 2, start)})
	e.Advance(at(43))
	if w := window5m(t, e); w.Bad != 2 || w.Total != 2 {
		t.Fatalf("window %+v, want only the 2 new 503s", w)
	}
	if p := e.producers[pkey]; p == nil || len(p.evicted) != 0 || e.retired[pkey] != nil {
		t.Fatal("producer not restored, or tombstones of returned series kept")
	}
	// Silent beyond the longest SLO window: the retired state is forgotten.
	e.Advance(at(43).Add(31 * day))
	if len(e.producers) != 0 || len(e.retired) != 0 {
		t.Fatalf("producers %d, retired %d: want both empty", len(e.producers), len(e.retired))
	}
	// A restart (new start time) after the first export is new data.
	restart := at(43).Add(31 * day)
	e.Ingest([]Sample{{Metric: "reqs_total", Labels: lbls("job", "api", "code", "200"), Value: 10, Time: restart.Add(time.Minute), Start: restart, Producer: "p"}})
	e.Ingest([]Sample{{Metric: "reqs_total", Labels: lbls("job", "api", "code", "500"), Value: 4, Time: restart.Add(2 * time.Minute), Start: restart, Producer: "p"}})
	e.Advance(restart.Add(2 * time.Minute))
	if got := window5m(t, e).Bad; got != 4 {
		t.Fatalf("bad = %v, want 4", got)
	}
}
