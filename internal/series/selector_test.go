package series

import (
	"strings"
	"testing"

	"github.com/prometheus/common/model"
	promparser "github.com/prometheus/prometheus/promql/parser"
)

func TestParseSelectors(t *testing.T) {
	cases := []struct {
		in       string
		metric   string
		matchers []string
	}{
		{"up", "up", nil},
		{"up{}", "up", nil},
		{`http_requests_total{job="api"}`, "http_requests_total", []string{`job="api"`}},
		{`x{a="1", b!="2", c=~"5..", d!~"4.."}`, "x", []string{`a="1"`, `b!="2"`, `c=~"5.."`, `d!~"4.."`}},
		{`x{a='single', b=` + "`raw\\d`" + `,}`, "x", []string{`a="single"`, `b="raw\\d"`}},
		{`{__name__="via_name", job="j"}`, "via_name", []string{`job="j"`}},
		{`  spaced  { a = "1" , b = "2" }  `, "spaced", []string{`a="1"`, `b="2"`}},
		{`ns:rule:ratio{a="\"quoted\""}`, "ns:rule:ratio", []string{`a="\"quoted\""`}},
		{`x{a="été"}`, "x", []string{`a="été"`}},
		{`x{a='it\'s'}`, "x", []string{`a="it's"`}},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			sel, err := Parse(c.in)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if sel.Metric != c.metric {
				t.Fatalf("metric = %q, want %q", sel.Metric, c.metric)
			}
			if len(sel.Matchers) != len(c.matchers) {
				t.Fatalf("got %d matchers, want %d", len(sel.Matchers), len(c.matchers))
			}
			for i, m := range sel.Matchers {
				if m.String() != c.matchers[i] {
					t.Errorf("matcher %d = %s, want %s", i, m, c.matchers[i])
				}
			}
			again, err := Parse(sel.String())
			if err != nil || again.String() != sel.String() {
				t.Fatalf("round trip %q -> %q (%v)", sel.String(), again.String(), err)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		``:                      "metric name is required",
		`{job="x"}`:             "metric name is required",
		`x{job}`:                "expected one of",
		`x{job="a"`:             "missing '}'",
		`x{job="a`:              "unterminated string",
		`x{job=a}`:              "expected quoted string",
		`x{a="1" b="2"}`:        "expected ',' or '}'",
		`x{a=~"("}`:             "invalid regular expression",
		`x{__name__=~"a"}`:      "__name__ must use '='",
		`x{__name__="y"}`:       "metric name given twice",
		`x{a="1", a="1"}`:       "duplicate matcher",
		`x y`:                   "unexpected",
		`x{a="\q"}`:             "invalid escape",
		"x{a=\"line\nbreak\"}":  "newline in string",
		`x{1a="b"}`:             "expected label name",
		`x{a="1"} or y{b="2"}`:  "unexpected",
		"x{a=`unterminated}":    "unterminated string",
		`{__name__="1bad"}`:     "invalid metric name",
		`x{a="ok", b=~"[z-a]"}`: "invalid regular expression",
	}
	for in, want := range cases {
		_, err := Parse(in)
		if err == nil {
			t.Errorf("Parse(%q): expected error containing %q", in, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v, want error containing %q", in, err, want)
		}
	}
}

func TestMatchSemantics(t *testing.T) {
	sel := MustParse(`m{job="api", code=~"5..", env!="dev", zone!~"eu-.*"}`)
	ls := FromMap(map[string]string{"job": "api", "code": "503", "zone": "us-east"})
	if !sel.Matches("m", ls) {
		t.Fatal("expected match")
	}
	if sel.Matches("other", ls) {
		t.Fatal("metric name must be compared")
	}
	if sel.Matches("m", FromMap(map[string]string{"job": "api", "code": "1503"})) {
		t.Fatal("regex must be anchored")
	}
	if sel.Matches("m", FromMap(map[string]string{"job": "api", "code": "500", "env": "dev"})) {
		t.Fatal("!= must exclude")
	}
	if sel.Matches("m", FromMap(map[string]string{"job": "api", "code": "500", "zone": "eu-west"})) {
		t.Fatal("!~ must exclude")
	}
	// A missing label equals the empty string.
	empty := MustParse(`m{team=""}`)
	if !empty.Matches("m", nil) {
		t.Fatal(`team="" should match a series without team`)
	}
}

func TestSelectorWith(t *testing.T) {
	base := MustParse(`h_bucket{job="a"}`)
	le, err := NewMatcher(MatchEqual, "le", "0.3")
	if err != nil {
		t.Fatal(err)
	}
	got := base.With(le).String()
	if got != `h_bucket{job="a", le="0.3"}` {
		t.Fatalf("With = %s", got)
	}
	if base.String() != `h_bucket{job="a"}` {
		t.Fatal("With must not mutate the receiver")
	}
	if c := base.WithMetric("h_count").String(); c != `h_count{job="a"}` {
		t.Fatalf("WithMetric = %s", c)
	}
}

func TestLabels(t *testing.T) {
	ls := FromMap(map[string]string{"b": "2", "a": "1", "empty": ""})
	if ls.String() != `{a="1", b="2"}` {
		t.Fatalf("String = %s", ls)
	}
	if ls.Get("a") != "1" || ls.Get("zz") != "" {
		t.Fatal("Get")
	}
	if FromMap(map[string]string{"a": "1b"}).Key() == FromMap(map[string]string{"a1": "b"}).Key() {
		t.Fatal("keys must not collide")
	}
	if len(ls.Map()) != 2 {
		t.Fatal("Map")
	}
	b := NewBuilder()
	b.Set("x", "1")
	b.Set("x", "2")
	b.Set("y", "")
	b.Set("y", "3")
	b.Override("z", "9")
	b.Override("z", "10")
	if got := b.Labels().String(); got != `{x="1;2", y="3", z="10"}` {
		t.Fatalf("Builder = %s", got)
	}
}

func TestValidNames(t *testing.T) {
	for _, ok := range []string{"a", "_a", "a:b_c9", "A"} {
		if !ValidMetricName(ok) {
			t.Errorf("metric %q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "9a", "a-b", "a.b"} {
		if ValidMetricName(bad) {
			t.Errorf("metric %q should be invalid", bad)
		}
	}
	if ValidLabelName("a:b") || !ValidLabelName("a_b") || ValidLabelName("") {
		t.Error("label name validation")
	}
	if MatchType(42).String() != "?" {
		t.Error("unknown match type")
	}
}

// TestAgreesWithPrometheusParser cross-checks our parser against the real
// PromQL parser: every selector we accept must parse identically there.
func TestAgreesWithPrometheusParser(t *testing.T) {
	inputs := []string{
		`http_server_request_duration_seconds_count{job="checkout", http_response_status_code=~"5.."}`,
		`x{a!="b", c!~"d|e", f=""}`,
		`x{a='single', b=` + "`raw`" + `}`,
		`{__name__="via_name", job="j"}`,
		`x{a="é", b="tab\tnewline\\n"}`,
	}
	for _, in := range inputs {
		ours, err := Parse(in)
		if err != nil {
			t.Fatalf("ours: %v", err)
		}
		assertSameAsPrometheus(t, ours)
	}
}

func assertSameAsPrometheus(t *testing.T, ours Selector) {
	t.Helper()
	theirs, err := promparser.NewParser(promparser.Options{}).ParseMetricSelector(ours.String())
	if err != nil {
		t.Fatalf("prometheus rejected our rendering %q: %v", ours.String(), err)
	}
	got := map[string]string{}
	for _, m := range theirs {
		if m.Name == model.MetricNameLabel {
			if m.Value != ours.Metric {
				t.Fatalf("metric %q vs %q", m.Value, ours.Metric)
			}
			continue
		}
		got[m.Name+m.Type.String()] = m.Value
	}
	if len(got) != len(ours.Matchers) {
		t.Fatalf("matcher count %d vs %d for %s", len(got), len(ours.Matchers), ours)
	}
	for _, m := range ours.Matchers {
		if v, ok := got[m.Name+m.Type.String()]; !ok || v != m.Value {
			t.Fatalf("matcher %s differs from prometheus (%q)", m, v)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{`x{a="1"}`, `x{a=~"5.."}`, `{__name__="y"}`, "x{a=`r`}", `x{a='q'}`, `x{a="\x41"}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		sel, err := Parse(in)
		if err != nil {
			return
		}
		again, err := Parse(sel.String())
		if err != nil {
			t.Fatalf("rendering %q of %q does not reparse: %v", sel.String(), in, err)
		}
		if again.String() != sel.String() {
			t.Fatalf("unstable rendering %q vs %q", again.String(), sel.String())
		}
		assertSameAsPrometheus(t, sel)
	})
}
