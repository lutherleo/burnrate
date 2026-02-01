// Package rules generates Prometheus recording and alerting rules that are
// equivalent to the engine's evaluation of the same SLOs.
//
// For every SLO it emits one rule group (so Prometheus evaluates the
// recordings before the alerts that read them, in the same cycle):
//
//   - slo:sli_error:ratio_rate<W> for every window W the SLO needs (each alert
//     window and the period), computed from raw counters as
//     sum(rate(bad[W])) / sum(rate(total[W])) — never by averaging shorter
//     ratios, which would weight quiet and busy periods equally;
//   - slo:objective:ratio, slo:error_budget:ratio,
//     slo:error_budget_remaining:ratio and slo:error_budget_policy_state;
//   - one alert per severity, OR-ing its long/short window pairs exactly like
//     the workbook's multiwindow, multi-burn-rate example.
//
// Thresholds are written as `burn * (1 - objective)` so PromQL performs the
// same float64 operations as the engine and ties break identically.
package rules

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/lutherleo/burnrate/internal/slo"
)

// Recording rule names.
const (
	RecordErrorRatioPrefix = "slo:sli_error:ratio_rate"
	RecordObjective        = "slo:objective:ratio"
	RecordErrorBudget      = "slo:error_budget:ratio"
	RecordBudgetRemaining  = "slo:error_budget_remaining:ratio"
	RecordPolicyState      = "slo:error_budget_policy_state"
)

// ErrorRatioRecord returns the recording rule name for a window.
func ErrorRatioRecord(w time.Duration) string {
	return RecordErrorRatioPrefix + slo.FormatDuration(w)
}

// File is a Prometheus rule file.
type File struct {
	Groups []Group `yaml:"groups"`
}

// Group is a rule group.
type Group struct {
	Name     string `yaml:"name"`
	Interval string `yaml:"interval,omitempty"`
	Rules    []Rule `yaml:"rules"`
}

// Rule is a recording or alerting rule.
type Rule struct {
	Record      string            `yaml:"record,omitempty"`
	Alert       string            `yaml:"alert,omitempty"`
	Expr        Expr              `yaml:"expr"`
	For         string            `yaml:"for,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// Expr is a PromQL expression; multi-line expressions are written as YAML
// literal blocks for readability.
type Expr string

// MarshalYAML implements yaml.Marshaler.
func (e Expr) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: string(e)}
	if strings.Contains(string(e), "\n") {
		n.Style = yaml.LiteralStyle
	}
	return n, nil
}

// Options tunes generation.
type Options struct {
	// Interval sets each group's evaluation interval; zero uses the
	// Prometheus global default.
	Interval time.Duration
}

// Generate builds the rule file for the given SLOs, in input order.
func Generate(slos []*slo.SLO, opts Options) (*File, error) {
	f := &File{}
	for _, s := range slos {
		g, err := group(s, opts)
		if err != nil {
			return nil, fmt.Errorf("slo %s: %w", s.Name, err)
		}
		f.Groups = append(f.Groups, g)
	}
	return f, nil
}

// GroupName is the rule group name of an SLO.
func GroupName(s *slo.SLO) string { return "burnrate-" + s.Name }

func group(s *slo.SLO, opts Options) (Group, error) {
	g := Group{Name: GroupName(s)}
	if opts.Interval > 0 {
		g.Interval = slo.FormatDuration(opts.Interval)
	}
	labels := s.SeriesLabels()
	for _, w := range s.Windows() {
		expr, err := errorRatioExpr(s.Indicator, w)
		if err != nil {
			return Group{}, err
		}
		g.Rules = append(g.Rules, Rule{Record: ErrorRatioRecord(w), Expr: Expr(expr), Labels: labels})
	}

	sel := fmt.Sprintf("{slo=%s}", strconv.Quote(s.Name))
	obj := num(s.Objective)
	budget := fmt.Sprintf("(1 - %s)", obj)
	remaining := RecordBudgetRemaining + sel
	g.Rules = append(g.Rules,
		Rule{Record: RecordObjective, Expr: Expr("vector(" + obj + ")"), Labels: labels},
		Rule{Record: RecordErrorBudget, Expr: Expr("vector" + budget), Labels: labels},
		Rule{Record: RecordBudgetRemaining, Expr: Expr(fmt.Sprintf("1 - %s%s / %s", ErrorRatioRecord(s.Period), sel, budget)), Labels: labels},
	)
	g.Rules = append(g.Rules, policyRules(s, remaining, labels)...)

	if s.Alerting.Enabled {
		for _, sev := range s.Alerting.Severities {
			g.Rules = append(g.Rules, Rule{
				Alert:       s.Alerting.Name,
				Expr:        Expr(alertExpr(s, sev, sel)),
				Labels:      s.AlertLabels(sev.Name),
				Annotations: s.AlertAnnotations(sev),
			})
		}
	}
	return g, nil
}

// errorRatioExpr is the bad-event ratio over w, restricted to windows with
// traffic (the denominator filter makes empty windows produce no sample,
// matching the engine's "no data" NaN).
func errorRatioExpr(ind slo.Indicator, w time.Duration) (string, error) {
	rng := "[" + slo.FormatDuration(w) + "]"
	total := fmt.Sprintf("(sum(rate(%s%s)) > 0)", ind.Total, rng)
	switch {
	case ind.Bad != nil:
		// "or vector(0)": a bad-event series that does not exist yet (no
		// error has ever happened) must read as zero, not as absent.
		return fmt.Sprintf("clamp_max((sum(rate(%s%s)) or vector(0)) / %s, 1)", ind.Bad, rng, total), nil
	case ind.Good != nil:
		return fmt.Sprintf("clamp_min(1 - (sum(rate(%s%s)) or vector(0)) / %s, 0)", ind.Good, rng, total), nil
	}
	return "", fmt.Errorf("indicator has neither good nor bad events")
}

func alertExpr(s *slo.SLO, sev slo.Severity, sel string) string {
	parts := make([]string, len(sev.Windows))
	obj := num(s.Objective)
	for i, w := range sev.Windows {
		thr := fmt.Sprintf("(%s * (1 - %s))", num(w.BurnRate), obj)
		parts[i] = fmt.Sprintf("(\n  %s%s > %s\nand\n  %s%s > %s\n)",
			ErrorRatioRecord(w.Long), sel, thr, ErrorRatioRecord(w.Short), sel, thr)
	}
	return strings.Join(parts, "\nor\n") + "\n"
}

// policyRules record slo:error_budget_policy_state{state=...} = 1 for the
// state the remaining budget falls in, mirroring slo.Policy.Evaluate: the
// first state (highest threshold first) whose minRemaining is met, and
// "exhausted" at or below zero or below every threshold.
func policyRules(s *slo.SLO, remaining string, labels map[string]string) []Rule {
	var out []Rule
	states := s.Policy.States
	for i, st := range states {
		cond := fmt.Sprintf("%s >= %s", remaining, num(st.MinRemaining))
		if st.MinRemaining <= 0 {
			cond = fmt.Sprintf("%s > 0", remaining)
		}
		if i > 0 {
			cond += fmt.Sprintf(" < %s", num(states[i-1].MinRemaining))
		}
		out = append(out, Rule{Record: RecordPolicyState, Expr: Expr("0 * (" + cond + ") + 1"), Labels: with(labels, "state", st.Name)})
	}
	last := states[len(states)-1].MinRemaining
	exhausted := fmt.Sprintf("%s <= 0", remaining)
	if last > 0 {
		exhausted = fmt.Sprintf("%s < %s", remaining, num(last))
	}
	out = append(out, Rule{Record: RecordPolicyState, Expr: Expr("0 * (" + exhausted + ") + 1"), Labels: with(labels, "state", slo.StateExhausted)})
	return out
}

func with(m map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for a, b := range m {
		out[a] = b
	}
	out[k] = v
	return out
}

// num renders a float so PromQL parses back the identical float64.
func num(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Marshal renders the rule file as YAML with a provenance header.
func (f *File) Marshal(header string) ([]byte, error) {
	var buf bytes.Buffer
	for _, line := range strings.Split(strings.TrimSpace(header), "\n") {
		if line != "" {
			buf.WriteString("# " + line + "\n")
		}
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Header is the standard provenance comment for generated files.
func Header(sources []string) string {
	uniq := map[string]bool{}
	for _, s := range sources {
		uniq[s] = true
	}
	list := make([]string, 0, len(uniq))
	for s := range uniq {
		list = append(list, s)
	}
	sort.Strings(list)
	return "Generated by burnrate from " + strings.Join(list, ", ") + ".\nDo not edit by hand: change the SLO definitions and regenerate."
}
