// Package slo loads, validates and resolves SLO definitions.
//
// The YAML format is modelled on OpenSLO v1 (apiVersion/kind/metadata/spec,
// timeWindow and objectives lists, budgetingMethod) but the indicator is
// written as PromQL series selectors instead of free-form queries. A selector
// is something both the in-process engine and Prometheus can evaluate, which
// is what makes the generated rules equivalent to the engine.
package slo

// APIVersion is the only accepted apiVersion.
const APIVersion = "burnrate/v1"

// Kind is the only accepted kind.
const Kind = "SLO"

// Document is one YAML document as written by users.
type Document struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

// Metadata identifies an SLO.
type Metadata struct {
	Name        string            `yaml:"name"`
	DisplayName string            `yaml:"displayName"`
	Labels      map[string]string `yaml:"labels"`
}

// Spec is the body of an SLO document.
type Spec struct {
	Service         string        `yaml:"service"`
	Description     string        `yaml:"description"`
	BudgetingMethod string        `yaml:"budgetingMethod"`
	TimeWindow      []TimeWindow  `yaml:"timeWindow"`
	Objectives      []Objective   `yaml:"objectives"`
	Indicator       IndicatorSpec `yaml:"indicator"`
	Alerting        *AlertingSpec `yaml:"alerting"`
	BudgetPolicy    *BudgetPolicy `yaml:"budgetPolicy"`
}

// TimeWindow is the SLO compliance period.
type TimeWindow struct {
	Duration  Duration `yaml:"duration"`
	IsRolling *bool    `yaml:"isRolling"`
}

// Objective is the reliability target.
type Objective struct {
	DisplayName string  `yaml:"displayName"`
	Target      float64 `yaml:"target"`
}

// IndicatorSpec selects exactly one SLI type.
type IndicatorSpec struct {
	Ratio   *RatioSpec   `yaml:"ratio"`
	Latency *LatencySpec `yaml:"latency"`
}

// RatioSpec is an event-ratio SLI: total events and either good or bad events,
// each a PromQL selector over counter series.
type RatioSpec struct {
	Total string `yaml:"total"`
	Good  string `yaml:"good"`
	Bad   string `yaml:"bad"`
}

// LatencySpec is a latency-threshold SLI over a histogram: an event is good
// when it lands in a bucket whose upper bound is <= Threshold seconds (or
// whatever unit the histogram uses).
type LatencySpec struct {
	// Histogram is a selector on the histogram's base name, e.g.
	// http_server_request_duration_seconds{job="checkout"}.
	Histogram string `yaml:"histogram"`
	// Threshold must equal one of the histogram's explicit bucket bounds.
	Threshold float64 `yaml:"threshold"`
}

// AlertingSpec configures the multiwindow, multi-burn-rate alerts.
type AlertingSpec struct {
	// Name is the Prometheus alert name. Default: ErrorBudgetBurn.
	Name        string            `yaml:"name"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
	// Windows overrides the workbook defaults.
	Windows []WindowSpec `yaml:"windows"`
	// Disabled turns off alert generation and evaluation for this SLO.
	Disabled bool `yaml:"disabled"`
}

// WindowSpec is one long/short window pair. Exactly one of BurnRate and
// BudgetConsumed must be set; BudgetConsumed is converted with
// burn = budgetConsumed * period / longWindow.
type WindowSpec struct {
	Severity       string   `yaml:"severity"`
	LongWindow     Duration `yaml:"longWindow"`
	ShortWindow    Duration `yaml:"shortWindow"`
	BurnRate       float64  `yaml:"burnRate"`
	BudgetConsumed float64  `yaml:"budgetConsumed"`
}

// BudgetPolicy maps remaining error budget to named states with actions.
type BudgetPolicy struct {
	States          []PolicyStateSpec `yaml:"states"`
	ExhaustedAction string            `yaml:"exhaustedAction"`
}

// PolicyStateSpec is one budget policy state.
type PolicyStateSpec struct {
	Name         string  `yaml:"name"`
	MinRemaining float64 `yaml:"minRemaining"`
	Action       string  `yaml:"action"`
}
