// Package engine is the in-process SLO evaluator.
//
// It turns counter samples into per-SLO event counts (total, good, bad) held
// in time-bucketed rings, then evaluates every SLO at each resolution
// boundary: error ratio and burn rate per window, the multiwindow
// multi-burn-rate alerts, and error-budget accounting with policy states.
//
// Counter handling follows Prometheus semantics so that the engine agrees with
// the recording rules generated from the same SLOs: increases are computed
// per series, a decrease is a counter reset, and the first sample of a series
// only establishes a baseline unless the stream provably started from zero
// (see firstValueCounts).
package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/lutherleo/burnrate/internal/series"
	"github.com/lutherleo/burnrate/internal/slo"
	"github.com/lutherleo/burnrate/internal/window"
)

// Config tunes the engine. The zero value is usable.
type Config struct {
	// Resolution is the bucket width and evaluation step. Every SLO window
	// must be a multiple of it. Default 1m.
	Resolution time.Duration
	// SeriesTTL evicts per-series counter state not updated for this long.
	// Default 1h.
	SeriesTTL time.Duration
	// MaxFutureSkew rejects samples stamped further than this ahead of Now.
	// Only used when Now is set (wall-clock mode). Default 5m.
	MaxFutureSkew time.Duration
	// Now is the wall clock; nil in data-clock (replay) mode.
	Now func() time.Time
	// EventHistory bounds the in-memory event log. Default 1000.
	EventHistory int
	// Logger receives debug and warning logs. Default: discard.
	Logger *slog.Logger
}

func (c *Config) defaults() {
	if c.Resolution == 0 {
		c.Resolution = time.Minute
	}
	if c.SeriesTTL == 0 {
		c.SeriesTTL = time.Hour
	}
	if c.MaxFutureSkew == 0 {
		c.MaxFutureSkew = 5 * time.Minute
	}
	if c.EventHistory == 0 {
		c.EventHistory = 1000
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
}

type role int

const (
	roleTotal role = iota
	roleGood
	roleBad
)

func (r role) counts(v float64) window.Counts {
	switch r {
	case roleGood:
		return window.Counts{Good: v}
	case roleBad:
		return window.Counts{Bad: v}
	}
	return window.Counts{Total: v}
}

// source is one selector of one SLO together with the counter state of every
// series it has matched.
type source struct {
	slo    *sloState
	role   role
	sel    series.Selector
	series map[string]*seriesState
}

type seriesState struct {
	last     float64
	lastTime int64
	start    int64
	pkey     string // producer key of the latest sample; "" when unknown
}

// producerState tracks one producer (resource + start time) of cumulative
// streams. evicted holds a tombstone (metric + "\xff" + label key) for every
// series of this producer dropped by SeriesTTL: if such a series is exported
// again with the same start time, its value is the stream's lifetime count,
// so it must re-establish a baseline instead of being taken for a newly
// created series (or for a stream that started after we began observing).
// A producer silent for SeriesTTL is retired with its tombstones, restored if
// it exports again, and forgotten once silent for the longest SLO window.
type producerState struct {
	first, last int64 // unix ns
	evicted     map[string]struct{}
}

type alertState struct {
	sev    slo.Severity
	firing bool
	since  time.Time
	labels map[string]string
	annos  map[string]string
}

type sloState struct {
	def         *slo.SLO
	ring        *window.Ring
	windows     []time.Duration
	alerts      []*alertState
	budgetState string
	status      Status
	evaluated   bool
	etaWindow   time.Duration
}

// Engine evaluates SLOs. All methods are safe for concurrent use.
type Engine struct {
	mu        sync.Mutex
	cfg       Config
	slos      []*sloState
	index     map[string][]*source
	producers map[string]*producerState // keyed by producer + start time
	retired   map[string]*producerState // producers silent for SeriesTTL, same key
	epoch     int64                     // timestamp of the first sample ever ingested
	watermark time.Time                 // newest sample time
	lastEval  time.Time
	events    []Event
	stats     Stats
}

// New builds an engine for the given SLOs.
func New(cfg Config, slos []*slo.SLO) (*Engine, error) {
	cfg.defaults()
	if cfg.Resolution <= 0 {
		return nil, errors.New("engine: resolution must be positive")
	}
	e := &Engine{
		cfg:       cfg,
		index:     map[string][]*source{},
		producers: map[string]*producerState{},
		retired:   map[string]*producerState{},
	}
	for _, def := range slos {
		st := &sloState{def: def, windows: def.Windows(), budgetState: slo.StateNoData}
		for _, w := range st.windows {
			if w%cfg.Resolution != 0 {
				return nil, fmt.Errorf("engine: SLO %s: window %s is not a multiple of the %s resolution",
					def.Name, slo.FormatDuration(w), slo.FormatDuration(cfg.Resolution))
			}
		}
		ring, err := window.New(cfg.Resolution, st.windows[len(st.windows)-1])
		if err != nil {
			return nil, fmt.Errorf("engine: SLO %s: %w", def.Name, err)
		}
		st.ring = ring
		if def.Alerting.Enabled {
			for _, sev := range def.Alerting.Severities {
				st.alerts = append(st.alerts, &alertState{
					sev:    sev,
					labels: def.AlertLabels(sev.Name),
					annos:  def.AlertAnnotations(sev),
				})
			}
		}
		st.etaWindow = etaWindow(st)
		e.addSource(st, roleTotal, def.Indicator.Total)
		if def.Indicator.Good != nil {
			e.addSource(st, roleGood, *def.Indicator.Good)
		}
		if def.Indicator.Bad != nil {
			e.addSource(st, roleBad, *def.Indicator.Bad)
		}
		st.status = e.emptyStatus(st)
		e.slos = append(e.slos, st)
	}
	return e, nil
}

func (e *Engine) addSource(st *sloState, r role, sel series.Selector) {
	e.index[sel.Metric] = append(e.index[sel.Metric], &source{slo: st, role: r, sel: sel, series: map[string]*seriesState{}})
}

// Wants reports whether any SLO reads the given metric name. Translators use
// it to skip expanding series nobody needs.
func (e *Engine) Wants(metric string) bool {
	_, ok := e.index[metric]
	return ok
}

// Resolution returns the evaluation step.
func (e *Engine) Resolution() time.Duration { return e.cfg.Resolution }

// Ingest feeds samples into the engine. It never evaluates; call Advance.
func (e *Engine) Ingest(samples []Sample) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range samples {
		e.ingestOne(&samples[i])
	}
}

func (e *Engine) ingestOne(s *Sample) {
	e.stats.SamplesReceived++
	srcs := e.index[s.Metric]
	if len(srcs) == 0 {
		return
	}
	if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) || s.Value < 0 || s.Time.IsZero() {
		e.stats.SamplesInvalid++
		return
	}
	if e.cfg.Now != nil && s.Time.After(e.cfg.Now().Add(e.cfg.MaxFutureSkew)) {
		e.stats.SamplesFuture++
		return
	}
	t := s.Time.UnixNano()
	if e.epoch == 0 {
		e.epoch = t
	}
	if s.Time.After(e.watermark) {
		e.watermark = s.Time
	}
	key, pkey := "", ""
	var prod *producerState
	if s.Temporality == Cumulative {
		key = s.Labels.Key()
		if !s.Start.IsZero() {
			pkey = s.Producer + "\xff" + s.Start.String()
			prod = e.producers[pkey]
			if prod == nil {
				if prod = e.retired[pkey]; prod != nil {
					delete(e.retired, pkey)
				} else {
					prod = &producerState{first: t, last: t}
				}
				e.producers[pkey] = prod
			}
			prod.last = max(prod.last, t)
		}
	}
	// A series this producer exported before it was evicted is returning,
	// not new: see producerState.
	returning, tomb := false, ""
	if prod != nil && len(prod.evicted) > 0 {
		tomb = s.Metric + "\xff" + key
		_, returning = prod.evicted[tomb]
	}
	matched := false
	for _, src := range srcs {
		if !src.sel.Matches(s.Metric, s.Labels) {
			continue
		}
		matched = true
		delta, ok := e.delta(src, s, t, key, pkey, returning)
		if !ok || delta == 0 {
			continue
		}
		if err := src.slo.ring.Add(s.Time, src.role.counts(delta)); err != nil {
			e.stats.SamplesTooOld++
		}
	}
	if returning {
		delete(prod.evicted, tomb)
	}
	if matched {
		e.stats.SamplesMatched++
	}
}

// delta converts a sample into the number of new events it represents for one
// source. key is the sample's label key and pkey its producer key; returning
// marks a series its producer exported before it was evicted. ok is false for
// duplicates and out-of-order samples.
func (e *Engine) delta(src *source, s *Sample, t int64, key, pkey string, returning bool) (float64, bool) {
	if s.Temporality == Delta {
		return s.Value, true
	}
	start := int64(0)
	if !s.Start.IsZero() {
		start = s.Start.UnixNano()
	}
	st, ok := src.series[key]
	if !ok {
		src.series[key] = &seriesState{last: s.Value, lastTime: t, start: start, pkey: pkey}
		if !returning && e.firstValueCounts(start, t, pkey) {
			return s.Value, true
		}
		return 0, true
	}
	if t <= st.lastTime {
		e.stats.SamplesOutOfOrder++
		return 0, false
	}
	var d float64
	switch {
	case start != 0 && st.start != 0 && start > st.start:
		// A new start time is an explicit reset: the stream restarted at
		// zero, so its whole value is new.
		d = s.Value
		e.stats.CounterResets++
	case s.Value < st.last:
		d = s.Value
		e.stats.CounterResets++
	default:
		d = s.Value - st.last
	}
	st.last, st.lastTime = s.Value, t
	if start != 0 {
		st.start, st.pkey = start, pkey
	}
	return d, true
}

// firstValueCounts decides whether the first value of a new cumulative series
// is made of new events. It is when the stream's start time is known and
//
//   - the stream started after the engine began observing (the process
//     (re)started while we were watching, so the counter began at zero), or
//   - the same producer, with the same start time, already exported at an
//     earlier timestamp without this series. Cumulative exporters report every
//     series on every export, so the series must have been created since.
//
// Otherwise the value may include events from before we started observing, so
// it only establishes a baseline, as Prometheus' rate() would treat it. Neither
// rule applies to a series returning after eviction (see producerState): the
// caller checks that first.
func (e *Engine) firstValueCounts(start, t int64, pkey string) bool {
	if start == 0 {
		return false
	}
	if start >= e.epoch {
		return true
	}
	p, ok := e.producers[pkey]
	return ok && p.first < t
}

// Watermark returns the timestamp of the newest sample ingested.
func (e *Engine) Watermark() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.watermark
}

// Stats returns a snapshot of the ingestion counters.
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.stats
	for _, srcs := range e.index {
		for _, src := range srcs {
			s.SeriesTracked += len(src.series)
		}
	}
	return s
}

// evictStale drops series state not updated within SeriesTTL, leaving a
// tombstone on the series' producer so that its return is not mistaken for a
// new series. Producers not updated within SeriesTTL are retired, and retired
// producers are forgotten once silent for the longest SLO window.
func (e *Engine) evictStale(now time.Time) {
	horizon := now.Add(-e.cfg.SeriesTTL).UnixNano()
	for _, srcs := range e.index {
		for _, src := range srcs {
			for k, st := range src.series {
				if st.lastTime >= horizon {
					continue
				}
				delete(src.series, k)
				e.stats.SeriesEvicted++
				if p := e.producers[st.pkey]; p != nil {
					if p.evicted == nil {
						p.evicted = map[string]struct{}{}
					}
					p.evicted[src.sel.Metric+"\xff"+k] = struct{}{}
				}
			}
		}
	}
	for k, p := range e.producers {
		if p.last < horizon {
			delete(e.producers, k)
			e.retired[k] = p
		}
	}
	forget := now.Add(-e.maxWindow()).UnixNano()
	for k, p := range e.retired {
		if p.last < forget {
			delete(e.retired, k)
		}
	}
}
