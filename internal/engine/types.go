package engine

import (
	"encoding/json"
	"math"
	"strconv"
	"time"

	"github.com/lutherleo/burnrate/internal/series"
	"github.com/lutherleo/burnrate/internal/slo"
)

// Temporality is the OTLP aggregation temporality of a counter sample.
type Temporality int

// Supported temporalities.
const (
	Cumulative Temporality = iota
	Delta
)

// Sample is one counter observation, already translated to Prometheus metric
// and label names.
type Sample struct {
	Metric string
	Labels series.Labels
	Value  float64
	// Time is the observation time; Start is the start of the cumulative
	// stream (or of the delta interval). Start may be zero when unknown.
	Time        time.Time
	Start       time.Time
	Temporality Temporality
	// Producer identifies the emitting process (resource attributes), used
	// to decide whether a newly seen cumulative series started from zero.
	Producer string
}

// Ratio is a float64 that encodes NaN and infinities as JSON null.
type Ratio float64

// MarshalJSON implements json.Marshaler.
func (r Ratio) MarshalJSON() ([]byte, error) {
	f := float64(r)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	return strconv.AppendFloat(nil, f, 'g', -1, 64), nil
}

// UnmarshalJSON implements json.Unmarshaler; null decodes to NaN.
func (r *Ratio) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*r = Ratio(math.NaN())
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*r = Ratio(f)
	return nil
}

// WindowStatus is the SLI over one window at an evaluation time.
type WindowStatus struct {
	Window     string  `json:"window"`
	Total      float64 `json:"total"`
	Bad        float64 `json:"bad"`
	ErrorRatio Ratio   `json:"errorRatio"`
	BurnRate   Ratio   `json:"burnRate"`
	duration   time.Duration
}

// PairStatus is one long/short window pair of an alert.
type PairStatus struct {
	Long          string  `json:"long"`
	Short         string  `json:"short"`
	BurnThreshold float64 `json:"burnThreshold"`
	LongBurn      Ratio   `json:"longBurn"`
	ShortBurn     Ratio   `json:"shortBurn"`
	Active        bool    `json:"active"`
}

// AlertStatus is the state of one severity's alert.
type AlertStatus struct {
	Severity string       `json:"severity"`
	Firing   bool         `json:"firing"`
	Since    *time.Time   `json:"since,omitempty"`
	Pairs    []PairStatus `json:"pairs"`
}

// BudgetStatus is the error budget accounting over the SLO period.
type BudgetStatus struct {
	Period    string  `json:"period"`
	Total     float64 `json:"totalEvents"`
	Bad       float64 `json:"badEvents"`
	Allowed   float64 `json:"allowedBadEvents"`
	Consumed  Ratio   `json:"consumed"`
	Remaining Ratio   `json:"remaining"`
	State     string  `json:"state"`
	Action    string  `json:"action,omitempty"`
	// ExhaustionETA is how long the remaining budget lasts at the burn rate
	// of the shortest alert long window; omitted when not burning.
	ExhaustionETA string `json:"exhaustionEta,omitempty"`
}

// Status is the full evaluation result of one SLO.
type Status struct {
	SLO         string         `json:"slo"`
	DisplayName string         `json:"displayName"`
	Service     string         `json:"service"`
	Indicator   string         `json:"indicator"`
	Objective   float64        `json:"objective"`
	EvaluatedAt time.Time      `json:"evaluatedAt"`
	Windows     []WindowStatus `json:"windows"`
	Alerts      []AlertStatus  `json:"alerts"`
	Budget      BudgetStatus   `json:"budget"`
}

// Window returns the status for the window of duration d.
func (s Status) Window(d time.Duration) (WindowStatus, bool) {
	for _, w := range s.Windows {
		if w.duration == d {
			return w, true
		}
	}
	return WindowStatus{}, false
}

// Alert returns the alert status for a severity.
func (s Status) Alert(severity string) (AlertStatus, bool) {
	for _, a := range s.Alerts {
		if a.Severity == severity {
			return a, true
		}
	}
	return AlertStatus{}, false
}

// EventKind classifies events.
type EventKind string

// Event kinds.
const (
	EventAlertFiring   EventKind = "alert_firing"
	EventAlertResolved EventKind = "alert_resolved"
	EventBudgetState   EventKind = "budget_state_changed"
)

// Event is a state transition produced by an evaluation.
type Event struct {
	Time            time.Time         `json:"time"`
	Kind            EventKind         `json:"kind"`
	SLO             string            `json:"slo"`
	Service         string            `json:"service"`
	AlertName       string            `json:"alertName,omitempty"`
	Severity        string            `json:"severity,omitempty"`
	FromState       string            `json:"fromState,omitempty"`
	ToState         string            `json:"toState,omitempty"`
	Pairs           []PairStatus      `json:"pairs,omitempty"`
	BudgetRemaining Ratio             `json:"budgetRemaining"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

// ActiveAlert is a currently firing alert, as notifiers need it.
type ActiveAlert struct {
	Name        string
	SLO         string
	Severity    string
	Since       time.Time
	Labels      map[string]string
	Annotations map[string]string
}

// Stats are cumulative ingestion counters.
type Stats struct {
	SamplesReceived   uint64 `json:"samplesReceived"`
	SamplesMatched    uint64 `json:"samplesMatched"`
	SamplesTooOld     uint64 `json:"samplesTooOld"`
	SamplesFuture     uint64 `json:"samplesFuture"`
	SamplesOutOfOrder uint64 `json:"samplesOutOfOrder"`
	SamplesInvalid    uint64 `json:"samplesInvalid"`
	CounterResets     uint64 `json:"counterResets"`
	SeriesTracked     int    `json:"seriesTracked"`
	SeriesEvicted     uint64 `json:"seriesEvicted"`
	Evaluations       uint64 `json:"evaluations"`
}

func indicatorName(s *slo.SLO) string {
	return string(s.Indicator.Kind)
}
