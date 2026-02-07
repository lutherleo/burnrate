package slo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/lutherleo/burnrate/internal/burnmath"
	"github.com/lutherleo/burnrate/internal/series"
)

// DefaultAlertName is used when alerting.name is empty.
const DefaultAlertName = "ErrorBudgetBurn"

// Reserved label names that burnrate attaches to every generated series and
// alert; SLO metadata labels may not reuse them.
var reservedLabels = map[string]bool{"slo": true, "service": true, "severity": true, "le": true, "__name__": true}

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// DefaultWindows returns the workbook's recommended window pairs, expressed
// as budget consumption so they scale with the SLO period. For a 30-day period
// they produce the familiar burn rates 14.4, 6, 3 and 1.
func DefaultWindows() []WindowSpec {
	return []WindowSpec{
		{Severity: "page", LongWindow: Duration(time.Hour), ShortWindow: Duration(5 * time.Minute), BudgetConsumed: 0.02},
		{Severity: "page", LongWindow: Duration(6 * time.Hour), ShortWindow: Duration(30 * time.Minute), BudgetConsumed: 0.05},
		{Severity: "ticket", LongWindow: Duration(24 * time.Hour), ShortWindow: Duration(2 * time.Hour), BudgetConsumed: 0.10},
		{Severity: "ticket", LongWindow: Duration(72 * time.Hour), ShortWindow: Duration(6 * time.Hour), BudgetConsumed: 0.10},
	}
}

// DefaultPolicy returns the budget policy used when none is configured.
func DefaultPolicy() BudgetPolicy {
	return BudgetPolicy{
		States: []PolicyStateSpec{
			{Name: "healthy", MinRemaining: 0.5, Action: "Release at normal cadence."},
			{Name: "warning", MinRemaining: 0.25, Action: "Prioritise reliability work in the next iteration; review recent incidents."},
			{Name: "critical", MinRemaining: 0, Action: "Only low-risk changes ship; every deploy needs a rollback plan."},
		},
		ExhaustedAction: "Freeze feature releases until the budget recovers; reliability fixes only.",
	}
}

// ValidationError collects every problem found in one SLO document.
type ValidationError struct {
	Source string
	Name   string
	Errs   []string
}

func (e *ValidationError) Error() string {
	where := e.Source
	if e.Name != "" {
		where += " (slo " + e.Name + ")"
	}
	return where + ": " + strings.Join(e.Errs, "; ")
}

// LoadPaths loads SLOs from files and directories. Directories are scanned
// (non-recursively) for *.yaml and *.yml files. SLO names must be unique
// across all inputs.
func LoadPaths(paths ...string) ([]*SLO, error) {
	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, p)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if !e.IsDir() && (ext == ".yaml" || ext == ".yml") {
				files = append(files, filepath.Join(p, e.Name()))
			}
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no SLO files found in %s", strings.Join(paths, ", "))
	}
	sort.Strings(files)
	var all []*SLO
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		slos, err := Parse(b, f)
		if err != nil {
			return nil, err
		}
		all = append(all, slos...)
	}
	if err := checkUnique(all); err != nil {
		return nil, err
	}
	return all, nil
}

// Parse decodes and resolves every SLO document in a (possibly multi-document)
// YAML stream. Unknown fields are rejected.
func Parse(data []byte, source string) ([]*SLO, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var out []*SLO
	for i := 0; ; i++ {
		var doc Document
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, i+1, err)
		}
		if doc.APIVersion == "" && doc.Kind == "" && doc.Metadata.Name == "" {
			continue // empty document, e.g. a trailing "---"
		}
		s, err := Resolve(doc, source)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := checkUnique(out); err != nil {
		return nil, err
	}
	return out, nil
}

func checkUnique(slos []*SLO) error {
	seen := map[string]string{}
	for _, s := range slos {
		if prev, ok := seen[s.Name]; ok {
			return fmt.Errorf("duplicate SLO name %q in %s and %s", s.Name, prev, s.Source)
		}
		seen[s.Name] = s.Source
	}
	return nil
}

// Resolve validates one document and fills in defaults. All problems are
// reported together in a *ValidationError.
func Resolve(doc Document, source string) (*SLO, error) {
	v := &ValidationError{Source: source, Name: doc.Metadata.Name}
	fail := func(format string, args ...any) { v.Errs = append(v.Errs, fmt.Sprintf(format, args...)) }

	if doc.APIVersion != APIVersion {
		fail("apiVersion must be %q, got %q", APIVersion, doc.APIVersion)
	}
	if doc.Kind != Kind {
		fail("kind must be %q, got %q", Kind, doc.Kind)
	}
	s := &SLO{
		Name:        doc.Metadata.Name,
		DisplayName: doc.Metadata.DisplayName,
		Service:     doc.Spec.Service,
		Description: strings.TrimSpace(doc.Spec.Description),
		Labels:      map[string]string{},
		Source:      source,
	}
	if !nameRE.MatchString(s.Name) {
		fail("metadata.name %q must be a DNS label (lowercase letters, digits and '-', at most 63 characters)", s.Name)
	}
	if s.DisplayName == "" {
		s.DisplayName = s.Name
	}
	if s.Service == "" {
		fail("spec.service is required")
	}
	for k, val := range doc.Metadata.Labels {
		switch {
		case !series.ValidLabelName(k) || strings.HasPrefix(k, "__"):
			fail("metadata.labels: %q is not a valid Prometheus label name", k)
		case reservedLabels[k]:
			fail("metadata.labels: %q is reserved", k)
		default:
			s.Labels[k] = val
		}
	}

	switch m := doc.Spec.BudgetingMethod; m {
	case "", "Occurrences":
	default:
		fail("budgetingMethod %q is not supported (only Occurrences)", m)
	}

	switch len(doc.Spec.TimeWindow) {
	case 1:
		tw := doc.Spec.TimeWindow[0]
		s.Period = time.Duration(tw.Duration)
		if tw.IsRolling != nil && !*tw.IsRolling {
			fail("timeWindow: calendar-aligned windows are not supported; set isRolling: true")
		}
		if s.Period < time.Hour || s.Period > 400*24*time.Hour {
			fail("timeWindow.duration must be between 1h and 400d, got %s", tw.Duration)
		}
	case 0:
		fail("spec.timeWindow needs exactly one entry")
	default:
		fail("spec.timeWindow: only one time window per SLO is supported")
	}

	switch len(doc.Spec.Objectives) {
	case 1:
		s.Objective = doc.Spec.Objectives[0].Target
		if !(s.Objective > 0 && s.Objective < 1) {
			fail("objectives[0].target must be in (0, 1), got %v", s.Objective)
		}
	case 0:
		fail("spec.objectives needs exactly one entry")
	default:
		fail("spec.objectives: only one objective per SLO is supported")
	}

	resolveIndicator(doc.Spec.Indicator, s, fail)
	if len(v.Errs) == 0 {
		resolveAlerting(doc.Spec.Alerting, s, fail)
	}
	resolvePolicy(doc.Spec.BudgetPolicy, s, fail)

	if len(v.Errs) > 0 {
		return nil, v
	}
	return s, nil
}

func resolveIndicator(spec IndicatorSpec, s *SLO, fail func(string, ...any)) {
	parse := func(field, text string) *series.Selector {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		sel, err := series.Parse(text)
		if err != nil {
			fail("%s: %v", field, err)
			return nil
		}
		return &sel
	}
	switch {
	case spec.Ratio != nil && spec.Latency != nil:
		fail("indicator: set exactly one of ratio and latency")
	case spec.Ratio != nil:
		r := spec.Ratio
		s.Indicator.Kind = KindRatio
		total := parse("indicator.ratio.total", r.Total)
		if total == nil && strings.TrimSpace(r.Total) == "" {
			fail("indicator.ratio.total is required")
		}
		good := parse("indicator.ratio.good", r.Good)
		bad := parse("indicator.ratio.bad", r.Bad)
		if (strings.TrimSpace(r.Good) == "") == (strings.TrimSpace(r.Bad) == "") {
			fail("indicator.ratio: set exactly one of good and bad")
		}
		if total != nil {
			s.Indicator.Total = *total
		}
		s.Indicator.Good, s.Indicator.Bad = good, bad
	case spec.Latency != nil:
		l := spec.Latency
		s.Indicator.Kind = KindLatency
		h := parse("indicator.latency.histogram", l.Histogram)
		if h == nil {
			if strings.TrimSpace(l.Histogram) == "" {
				fail("indicator.latency.histogram is required")
			}
			return
		}
		for _, suffix := range []string{"_bucket", "_count", "_sum"} {
			if strings.HasSuffix(h.Metric, suffix) {
				fail("indicator.latency.histogram: use the histogram base name without %q", suffix)
				return
			}
		}
		for _, m := range h.Matchers {
			if m.Name == "le" {
				fail("indicator.latency.histogram: do not match on le; set threshold instead")
				return
			}
		}
		if !(l.Threshold > 0) || math.IsInf(l.Threshold, 0) {
			fail("indicator.latency.threshold must be a positive bucket bound, got %v", l.Threshold)
			return
		}
		le, err := series.NewMatcher(series.MatchEqual, "le", FormatBound(l.Threshold))
		if err != nil {
			fail("indicator.latency: %v", err)
			return
		}
		good := h.WithMetric(h.Metric + "_bucket").With(le)
		s.Indicator.Histogram = h
		s.Indicator.Threshold = l.Threshold
		s.Indicator.Total = h.WithMetric(h.Metric + "_count")
		s.Indicator.Good = &good
	default:
		fail("indicator: set one of ratio or latency")
	}
}

func resolveAlerting(spec *AlertingSpec, s *SLO, fail func(string, ...any)) {
	a := Alerting{Enabled: true, Name: DefaultAlertName, Labels: map[string]string{}, Annotations: map[string]string{}}
	windows, defaults := DefaultWindows(), true
	if spec != nil {
		a.Enabled = !spec.Disabled
		if spec.Name != "" {
			a.Name = spec.Name
		}
		for k, v := range spec.Labels {
			if !series.ValidLabelName(k) || reservedLabels[k] || strings.HasPrefix(k, "__") {
				fail("alerting.labels: %q is reserved or not a valid label name", k)
				continue
			}
			a.Labels[k] = v
		}
		for k, v := range spec.Annotations {
			if !series.ValidLabelName(k) {
				fail("alerting.annotations: %q is not a valid annotation name", k)
				continue
			}
			a.Annotations[k] = v
		}
		if len(spec.Windows) > 0 {
			windows, defaults = spec.Windows, false
		}
	}
	if !series.ValidMetricName(a.Name) || strings.Contains(a.Name, ":") {
		fail("alerting.name %q is not a valid alert name", a.Name)
	}
	for i, w := range windows {
		where := fmt.Sprintf("alerting.windows[%d]", i)
		long, short := time.Duration(w.LongWindow), time.Duration(w.ShortWindow)
		if w.Severity == "" || !series.ValidLabelName(w.Severity) {
			fail("%s.severity %q must be a non-empty identifier", where, w.Severity)
			continue
		}
		if short <= 0 || long <= short {
			fail("%s: need 0 < shortWindow < longWindow, got %s/%s", where, w.ShortWindow, w.LongWindow)
			continue
		}
		if long > s.Period {
			if defaults {
				continue // default pairs that do not fit a short period are dropped
			}
			fail("%s: longWindow %s exceeds the SLO period %s", where, w.LongWindow, FormatDuration(s.Period))
			continue
		}
		win := Window{Long: long, Short: short}
		switch {
		case (w.BurnRate != 0) == (w.BudgetConsumed != 0):
			fail("%s: set exactly one of burnRate and budgetConsumed", where)
			continue
		case w.BurnRate != 0:
			win.BurnRate = w.BurnRate
			win.BudgetConsumed = burnmath.BudgetFraction(w.BurnRate, s.Period, long)
		default:
			win.BudgetConsumed = w.BudgetConsumed
			win.BurnRate = burnmath.BurnRate(w.BudgetConsumed, s.Period, long)
		}
		if !(win.BurnRate > 0) || math.IsInf(win.BurnRate, 0) {
			fail("%s: burn rate must be positive", where)
			continue
		}
		if thr := win.Threshold(s.Objective); thr >= 1 {
			fail("%s: burn rate %.4g needs an error ratio of %.4g > 100%% for a %.4g objective, so it can never fire; lower the burn rate or budgetConsumed",
				where, win.BurnRate, thr, s.Objective)
			continue
		}
		idx := slices.IndexFunc(a.Severities, func(sv Severity) bool { return sv.Name == w.Severity })
		if idx < 0 {
			a.Severities = append(a.Severities, Severity{Name: w.Severity})
			idx = len(a.Severities) - 1
		}
		a.Severities[idx].Windows = append(a.Severities[idx].Windows, win)
	}
	s.Alerting = a
}

func resolvePolicy(spec *BudgetPolicy, s *SLO, fail func(string, ...any)) {
	p := DefaultPolicy()
	if spec != nil {
		p = *spec
	}
	if len(p.States) == 0 {
		fail("budgetPolicy.states needs at least one state")
		return
	}
	names := map[string]bool{}
	for i, st := range p.States {
		where := fmt.Sprintf("budgetPolicy.states[%d]", i)
		switch {
		case st.Name == "":
			fail("%s.name is required", where)
		case st.Name == StateExhausted || st.Name == StateNoData:
			fail("%s.name %q is reserved", where, st.Name)
		case names[st.Name]:
			fail("%s.name %q is duplicated", where, st.Name)
		}
		names[st.Name] = true
		if st.MinRemaining < 0 || st.MinRemaining >= 1 {
			fail("%s.minRemaining must be in [0, 1), got %v", where, st.MinRemaining)
		}
		if i > 0 && st.MinRemaining >= p.States[i-1].MinRemaining {
			fail("%s.minRemaining must be lower than the previous state's", where)
		}
		s.Policy.States = append(s.Policy.States, PolicyState(st))
	}
	s.Policy.ExhaustedAction = p.ExhaustedAction
}
