package engine

import (
	"math"
	"time"

	"github.com/lutherleo/burnrate/internal/burnmath"
	"github.com/lutherleo/burnrate/internal/slo"
	"github.com/lutherleo/burnrate/internal/window"
)

// Advance evaluates every SLO at each resolution boundary in (last, to] and
// returns the state transitions in time order. Calling it with a time at or
// before the last evaluation is a no-op.
func (e *Engine) Advance(to time.Time) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	res := e.cfg.Resolution
	end := to.Truncate(res)
	if !e.lastEval.IsZero() && !end.After(e.lastEval) {
		return nil
	}
	start := end
	if !e.lastEval.IsZero() {
		start = e.lastEval.Add(res)
		// After a long gap only the last retention-worth of steps can
		// change state; skip the rest.
		if limit := end.Add(-e.maxWindow()); start.Before(limit) {
			start = limit
		}
	}
	var out []Event
	for t := start; !t.After(end); t = t.Add(res) {
		for _, st := range e.slos {
			out = append(out, e.evaluate(st, t)...)
		}
		e.stats.Evaluations++
	}
	e.lastEval = end
	e.evictStale(end)
	e.record(out)
	return out
}

func (e *Engine) maxWindow() time.Duration {
	var m time.Duration
	for _, st := range e.slos {
		m = max(m, st.windows[len(st.windows)-1])
	}
	return m
}

func (e *Engine) record(evs []Event) {
	e.events = append(e.events, evs...)
	if over := len(e.events) - e.cfg.EventHistory; over > 0 {
		e.events = append(e.events[:0:0], e.events[over:]...)
	}
	for _, ev := range evs {
		e.cfg.Logger.Info("slo event", "kind", ev.Kind, "slo", ev.SLO, "severity", ev.Severity,
			"from", ev.FromState, "to", ev.ToState, "time", ev.Time)
	}
}

// errorRatio returns bad/total for the SLI's counting mode. For good-event
// SLIs bad is total-good, clamped at zero because good and total come from
// different series and can be momentarily out of step.
func errorRatio(def *slo.SLO, c window.Counts) (bad, ratio float64) {
	bad = c.Bad
	if def.Indicator.Good != nil {
		bad = math.Max(0, c.Total-c.Good)
	}
	if c.Total <= 0 {
		return bad, math.NaN()
	}
	return bad, math.Min(1, bad/c.Total)
}

func (e *Engine) evaluate(st *sloState, t time.Time) []Event {
	def := st.def
	budget := def.ErrorBudget()
	status := Status{
		SLO: def.Name, DisplayName: def.DisplayName, Service: def.Service,
		Indicator: indicatorName(def), Objective: def.Objective, EvaluatedAt: t,
	}
	ratios := make(map[time.Duration]float64, len(st.windows))
	for _, w := range st.windows {
		c := st.ring.Sum(t, w)
		bad, r := errorRatio(def, c)
		ratios[w] = r
		status.Windows = append(status.Windows, WindowStatus{
			Window: slo.FormatDuration(w), Total: c.Total, Bad: bad,
			ErrorRatio: Ratio(r), BurnRate: Ratio(r / budget), duration: w,
		})
	}

	// Error budget over the period.
	pw, _ := status.Window(def.Period)
	allowed := budget * pw.Total
	consumed, remaining := math.NaN(), math.NaN()
	if pw.Total > 0 {
		consumed = pw.Bad / allowed
		remaining = 1 - consumed
	}
	state, action := def.Policy.Evaluate(remaining)
	status.Budget = BudgetStatus{
		Period: slo.FormatDuration(def.Period), Total: pw.Total, Bad: pw.Bad, Allowed: allowed,
		Consumed: Ratio(consumed), Remaining: Ratio(remaining), State: state, Action: action,
	}

	var events []Event
	if state != st.budgetState && (st.evaluated || state != slo.StateNoData) {
		events = append(events, Event{
			Time: t, Kind: EventBudgetState, SLO: def.Name, Service: def.Service,
			FromState: st.budgetState, ToState: state, BudgetRemaining: Ratio(remaining),
		})
	}
	st.budgetState = state

	for _, a := range st.alerts {
		as := AlertStatus{Severity: a.sev.Name}
		firing := false
		for _, w := range a.sev.Windows {
			thr := w.Threshold(def.Objective)
			lr, sr := ratios[w.Long], ratios[w.Short]
			active := lr > thr && sr > thr // NaN compares false: no data never fires
			firing = firing || active
			as.Pairs = append(as.Pairs, PairStatus{
				Long: slo.FormatDuration(w.Long), Short: slo.FormatDuration(w.Short), BurnThreshold: w.BurnRate,
				LongBurn: Ratio(lr / budget), ShortBurn: Ratio(sr / budget), Active: active,
			})
		}
		switch {
		case firing && !a.firing:
			a.firing, a.since = true, t
			events = append(events, e.alertEvent(st, a, EventAlertFiring, t, as.Pairs, remaining))
		case !firing && a.firing:
			a.firing = false
			events = append(events, e.alertEvent(st, a, EventAlertResolved, t, as.Pairs, remaining))
		}
		as.Firing = a.firing
		if a.firing {
			since := a.since
			as.Since = &since
		}
		status.Alerts = append(status.Alerts, as)
	}
	if burn := ratios[st.etaWindow] / budget; st.etaWindow > 0 && burn > 0 && !math.IsNaN(remaining) {
		if d, ok := burnmath.TimeToExhaustion(remaining, burn, def.Period); ok {
			status.Budget.ExhaustionETA = slo.FormatDuration(d.Round(time.Minute))
		}
	}
	st.status = status
	st.evaluated = true
	return events
}

// etaWindow is the window whose burn rate projects budget exhaustion: the
// shortest long window among the alert pairs, or zero without alerts.
func etaWindow(st *sloState) time.Duration {
	var best time.Duration
	for _, a := range st.alerts {
		for _, w := range a.sev.Windows {
			if best == 0 || w.Long < best {
				best = w.Long
			}
		}
	}
	return best
}

func (e *Engine) alertEvent(st *sloState, a *alertState, kind EventKind, t time.Time, pairs []PairStatus, remaining float64) Event {
	return Event{
		Time: t, Kind: kind, SLO: st.def.Name, Service: st.def.Service,
		AlertName: st.def.Alerting.Name, Severity: a.sev.Name, Pairs: pairs,
		BudgetRemaining: Ratio(remaining), Labels: copyMap(a.labels), Annotations: copyMap(a.annos),
	}
}

func (e *Engine) emptyStatus(st *sloState) Status {
	def := st.def
	s := Status{
		SLO: def.Name, DisplayName: def.DisplayName, Service: def.Service,
		Indicator: indicatorName(def), Objective: def.Objective,
		Budget: BudgetStatus{
			Period: slo.FormatDuration(def.Period), State: slo.StateNoData,
			Consumed: Ratio(math.NaN()), Remaining: Ratio(math.NaN()),
		},
	}
	for _, w := range st.windows {
		s.Windows = append(s.Windows, WindowStatus{Window: slo.FormatDuration(w), ErrorRatio: Ratio(math.NaN()), BurnRate: Ratio(math.NaN()), duration: w})
	}
	for _, a := range st.alerts {
		s.Alerts = append(s.Alerts, AlertStatus{Severity: a.sev.Name})
	}
	return s
}

// Status returns the latest evaluation of every SLO, in configuration order.
func (e *Engine) Status() []Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Status, len(e.slos))
	for i, st := range e.slos {
		out[i] = st.status
	}
	return out
}

// Events returns the retained event history, oldest first.
func (e *Engine) Events() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Event(nil), e.events...)
}

// ActiveAlerts returns the alerts currently firing.
func (e *Engine) ActiveAlerts() []ActiveAlert {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []ActiveAlert
	for _, st := range e.slos {
		for _, a := range st.alerts {
			if a.firing {
				out = append(out, ActiveAlert{
					Name: st.def.Alerting.Name, SLO: st.def.Name, Severity: a.sev.Name, Since: a.since,
					Labels: copyMap(a.labels), Annotations: copyMap(a.annos),
				})
			}
		}
	}
	return out
}

// LastEvaluation returns the time of the most recent evaluation step.
func (e *Engine) LastEvaluation() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastEval
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
