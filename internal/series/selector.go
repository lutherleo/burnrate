package series

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MatchType is the comparison a Matcher performs.
type MatchType int

// The four PromQL label matcher operators.
const (
	MatchEqual     MatchType = iota // =
	MatchNotEqual                   // !=
	MatchRegexp                     // =~
	MatchNotRegexp                  // !~
)

// String returns the PromQL operator.
func (t MatchType) String() string {
	switch t {
	case MatchEqual:
		return "="
	case MatchNotEqual:
		return "!="
	case MatchRegexp:
		return "=~"
	case MatchNotRegexp:
		return "!~"
	}
	return "?"
}

// Matcher tests a single label value. Regular expressions are fully anchored,
// as in PromQL.
type Matcher struct {
	Name  string
	Type  MatchType
	Value string
	re    *regexp.Regexp
}

// NewMatcher builds a matcher, compiling the regular expression when needed.
func NewMatcher(t MatchType, name, value string) (*Matcher, error) {
	m := &Matcher{Name: name, Type: t, Value: value}
	if t == MatchRegexp || t == MatchNotRegexp {
		re, err := regexp.Compile("^(?s:" + value + ")$")
		if err != nil {
			return nil, fmt.Errorf("label %q: invalid regular expression %q: %w", name, value, err)
		}
		m.re = re
	}
	return m, nil
}

// Matches reports whether v satisfies the matcher. A missing label is
// represented by the empty string, so `foo!="x"` matches series without foo.
func (m *Matcher) Matches(v string) bool {
	switch m.Type {
	case MatchEqual:
		return v == m.Value
	case MatchNotEqual:
		return v != m.Value
	case MatchRegexp:
		return m.re.MatchString(v)
	case MatchNotRegexp:
		return !m.re.MatchString(v)
	}
	return false
}

// String renders the matcher in PromQL syntax.
func (m *Matcher) String() string {
	return m.Name + m.Type.String() + quote(m.Value)
}

// Selector is a PromQL instant-vector selector restricted to what burnrate
// needs: a mandatory metric name plus label matchers.
type Selector struct {
	Metric   string
	Matchers []*Matcher
}

// Matches reports whether a series with the given metric name and labels is
// selected.
func (s Selector) Matches(metric string, ls Labels) bool {
	if metric != s.Metric {
		return false
	}
	for _, m := range s.Matchers {
		if !m.Matches(ls.Get(m.Name)) {
			return false
		}
	}
	return true
}

// WithMetric returns a copy of the selector that targets another metric name
// but keeps the matchers.
func (s Selector) WithMetric(metric string) Selector {
	return Selector{Metric: metric, Matchers: append([]*Matcher(nil), s.Matchers...)}
}

// With returns a copy of the selector with extra matchers appended.
func (s Selector) With(ms ...*Matcher) Selector {
	out := Selector{Metric: s.Metric, Matchers: make([]*Matcher, 0, len(s.Matchers)+len(ms))}
	out.Matchers = append(out.Matchers, s.Matchers...)
	out.Matchers = append(out.Matchers, ms...)
	return out
}

// String renders the selector in PromQL syntax. The output parses back to an
// equal selector.
//
// Metric names that collide with PromQL keywords or number literals (sum,
// offset, NaN, ...) are rendered as a __name__ matcher so the output stays
// unambiguous.
func (s Selector) String() string {
	parts := make([]string, 0, len(s.Matchers)+1)
	prefix := s.Metric
	if promqlKeywords[strings.ToLower(s.Metric)] {
		prefix = ""
		parts = append(parts, "__name__="+quote(s.Metric))
	}
	for _, m := range s.Matchers {
		parts = append(parts, m.String())
	}
	if len(parts) == 0 {
		return prefix
	}
	return prefix + "{" + strings.Join(parts, ", ") + "}"
}

// promqlKeywords are identifiers the PromQL lexer never reads as a bare metric
// name (matched case-insensitively), plus the NaN and Inf literals.
var promqlKeywords = map[string]bool{
	"and": true, "or": true, "unless": true, "atan2": true,
	"sum": true, "avg": true, "count": true, "min": true, "max": true, "group": true,
	"stddev": true, "stdvar": true, "topk": true, "bottomk": true, "count_values": true,
	"quantile": true, "limitk": true, "limit_ratio": true,
	"offset": true, "smoothed": true, "anchored": true, "by": true, "without": true,
	"on": true, "ignoring": true, "group_left": true, "group_right": true,
	"fill": true, "fill_left": true, "fill_right": true, "bool": true,
	"start": true, "end": true, "step": true, "range": true, "max_of": true, "min_of": true,
	"nan": true, "inf": true,
}

// ParseError describes where a selector failed to parse.
type ParseError struct {
	Input string
	Pos   int
	Msg   string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("selector %q: position %d: %s", e.Input, e.Pos, e.Msg)
}

// Parse parses a PromQL series selector such as
//
//	http_server_request_duration_seconds_count{job="checkout", http_response_status_code=~"5.."}
//
// The metric name may be given before the braces or as a __name__ equality
// matcher, but it is mandatory: an SLO must name the series it counts.
func Parse(input string) (Selector, error) {
	p := &parser{in: input}
	if !utf8.ValidString(input) {
		return Selector{}, p.errf("input is not valid UTF-8")
	}
	sel, err := p.selector()
	if err != nil {
		return Selector{}, err
	}
	return sel, nil
}

// MustParse is Parse for compile-time constants; it panics on error.
func MustParse(input string) Selector {
	s, err := Parse(input)
	if err != nil {
		panic(err)
	}
	return s
}

type parser struct {
	in  string
	pos int
}

func (p *parser) errf(format string, args ...any) error {
	return &ParseError{Input: p.in, Pos: p.pos, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) skipSpace() {
	for p.pos < len(p.in) {
		switch p.in[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) peek() byte {
	if p.pos < len(p.in) {
		return p.in[p.pos]
	}
	return 0
}

func (p *parser) selector() (Selector, error) {
	var sel Selector
	p.skipSpace()
	if isNameStart(p.peek(), true) {
		sel.Metric = p.ident(true)
	}
	p.skipSpace()
	if p.peek() == '{' {
		p.pos++
		ms, err := p.matchers()
		if err != nil {
			return Selector{}, err
		}
		for _, m := range ms {
			if m.Name != "__name__" {
				sel.Matchers = append(sel.Matchers, m)
				continue
			}
			if m.Type != MatchEqual {
				return Selector{}, p.errf("__name__ must use '=' in an SLO selector")
			}
			if sel.Metric != "" && sel.Metric != m.Value {
				return Selector{}, p.errf("metric name given twice (%q and %q)", sel.Metric, m.Value)
			}
			sel.Metric = m.Value
		}
	}
	p.skipSpace()
	if p.pos != len(p.in) {
		return Selector{}, p.errf("unexpected %q", p.in[p.pos:])
	}
	if sel.Metric == "" {
		return Selector{}, p.errf("a metric name is required")
	}
	if !ValidMetricName(sel.Metric) {
		return Selector{}, p.errf("invalid metric name %q", sel.Metric)
	}
	return sel, nil
}

func (p *parser) matchers() ([]*Matcher, error) {
	var out []*Matcher
	seen := make(map[string]bool)
	for {
		p.skipSpace()
		if p.peek() == '}' {
			p.pos++
			return out, nil
		}
		if !isNameStart(p.peek(), false) {
			if p.pos >= len(p.in) {
				return nil, p.errf("unterminated selector: missing '}'")
			}
			return nil, p.errf("expected label name")
		}
		name := p.ident(false)
		p.skipSpace()
		op, err := p.operator()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		val, err := p.str()
		if err != nil {
			return nil, err
		}
		key := name + op.String()
		if seen[key+val] {
			return nil, p.errf("duplicate matcher %s%s%q", name, op, val)
		}
		seen[key+val] = true
		m, err := NewMatcher(op, name, val)
		if err != nil {
			return nil, p.errf("%v", err)
		}
		out = append(out, m)
		p.skipSpace()
		switch p.peek() {
		case ',':
			p.pos++
		case '}':
		case 0:
			return nil, p.errf("unterminated selector: missing '}'")
		default:
			return nil, p.errf("expected ',' or '}'")
		}
	}
}

func (p *parser) operator() (MatchType, error) {
	rest := p.in[p.pos:]
	switch {
	case strings.HasPrefix(rest, "=~"):
		p.pos += 2
		return MatchRegexp, nil
	case strings.HasPrefix(rest, "!~"):
		p.pos += 2
		return MatchNotRegexp, nil
	case strings.HasPrefix(rest, "!="):
		p.pos += 2
		return MatchNotEqual, nil
	case strings.HasPrefix(rest, "="):
		p.pos++
		return MatchEqual, nil
	}
	return 0, p.errf("expected one of =, !=, =~, !~")
}

func (p *parser) ident(metric bool) string {
	start := p.pos
	for p.pos < len(p.in) && isNameChar(p.in[p.pos], metric) {
		p.pos++
	}
	return p.in[start:p.pos]
}

// str parses a PromQL string literal: double- or single-quoted with Go escape
// sequences, or a raw backtick string.
func (p *parser) str() (string, error) {
	q := p.peek()
	if q != '"' && q != '\'' && q != '`' {
		return "", p.errf("expected quoted string")
	}
	start := p.pos
	p.pos++
	if q == '`' {
		end := strings.IndexByte(p.in[p.pos:], '`')
		if end < 0 {
			p.pos = start
			return "", p.errf("unterminated string")
		}
		v := p.in[p.pos : p.pos+end]
		p.pos += end + 1
		return v, nil
	}
	var b strings.Builder
	for {
		if p.pos >= len(p.in) {
			p.pos = start
			return "", p.errf("unterminated string")
		}
		if p.in[p.pos] == q {
			p.pos++
			break
		}
		if p.in[p.pos] == '\n' {
			return "", p.errf("newline in string")
		}
		r, multibyte, tail, err := strconv.UnquoteChar(p.in[p.pos:], q)
		if err != nil {
			return "", p.errf("invalid escape sequence in string")
		}
		if r < utf8.RuneSelf || !multibyte {
			b.WriteByte(byte(r))
		} else {
			b.WriteRune(r)
		}
		p.pos = len(p.in) - len(tail)
	}
	v := b.String()
	if !utf8.ValidString(v) {
		return "", p.errf("string is not valid UTF-8")
	}
	return v, nil
}

func isNameStart(c byte, metric bool) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (metric && c == ':')
}

func isNameChar(c byte, metric bool) bool {
	return isNameStart(c, metric) || (c >= '0' && c <= '9')
}

// ValidMetricName reports whether s is a legacy (non-UTF-8) Prometheus metric
// name.
func ValidMetricName(s string) bool {
	if s == "" || !isNameStart(s[0], true) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isNameChar(s[i], true) {
			return false
		}
	}
	return true
}

// ValidLabelName reports whether s is a legacy Prometheus label name.
func ValidLabelName(s string) bool {
	if s == "" || !isNameStart(s[0], false) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isNameChar(s[i], false) {
			return false
		}
	}
	return true
}

func quote(s string) string {
	return strconv.Quote(s)
}
