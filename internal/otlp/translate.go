// Package otlp receives OTLP/HTTP metrics and translates them into engine
// samples using the same naming rules as the Prometheus OTLP receiver.
//
// The translation mirrors Prometheus' default configuration
// (translation_strategy: UnderscoreEscapingWithSuffixes): metric names get
// unit and type suffixes (http.server.request.duration in seconds becomes
// http_server_request_duration_seconds), attribute keys are sanitized into
// label names with colliding values joined by ';', service.name (prefixed by
// service.namespace) becomes job and service.instance.id becomes instance.
// Because the generated rules query Prometheus with the very same names, the
// engine and Prometheus look at the same series.
package otlp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/otlptranslator"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"github.com/lutherleo/burnrate/internal/engine"
	"github.com/lutherleo/burnrate/internal/series"
)

// Options configures the translation.
type Options struct {
	// Strategy is the Prometheus OTLP translation strategy. Only the
	// underscore-escaping strategies are supported because SLO selectors use
	// legacy metric names. Default UnderscoreEscapingWithSuffixes.
	Strategy otlptranslator.TranslationStrategyOption
	// PromoteResourceAttributes lists resource attributes copied onto every
	// series, like Prometheus' otlp.promote_resource_attributes.
	PromoteResourceAttributes []string
}

// Result summarises one translation.
type Result struct {
	// Points is the number of OTLP data points seen.
	Points int
	// Rejected points were malformed and dropped; Errors explains the first
	// few of them.
	Rejected int
	// Ignored points were well-formed but of a kind no SLO can use (gauges,
	// non-monotonic sums, summaries, exponential histograms, no-value
	// points).
	Ignored int
	Errors  []string
}

const maxErrors = 5

func (r *Result) reject(format string, args ...any) {
	r.Rejected++
	if len(r.Errors) < maxErrors {
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}
}

// Translator converts OTLP requests to engine samples.
type Translator struct {
	namer      otlptranslator.MetricNamer
	labelNamer otlptranslator.LabelNamer
	promote    []string
	wants      func(metric string) bool
}

// NewTranslator returns a Translator. wants reports whether a Prometheus
// metric name is used by any SLO; samples for other metrics are skipped
// before label translation, which keeps unrelated traffic cheap. A nil wants
// keeps everything.
func NewTranslator(opts Options, wants func(string) bool) (*Translator, error) {
	if opts.Strategy == "" {
		opts.Strategy = otlptranslator.UnderscoreEscapingWithSuffixes
	}
	if !opts.Strategy.ShouldEscape() {
		return nil, fmt.Errorf("otlp: translation strategy %q keeps UTF-8 names, which SLO selectors cannot express; use an underscore-escaping strategy", opts.Strategy)
	}
	if wants == nil {
		wants = func(string) bool { return true }
	}
	return &Translator{
		namer:      otlptranslator.NewMetricNamer("", opts.Strategy),
		labelNamer: otlptranslator.LabelNamer{},
		promote:    opts.PromoteResourceAttributes,
		wants:      wants,
	}, nil
}

type resourceContext struct {
	producer string
	job      string
	instance string
	promoted map[string]string
}

// Translate converts every usable data point in the request.
func (t *Translator) Translate(req *colmetricspb.ExportMetricsServiceRequest) ([]engine.Sample, Result) {
	var (
		out []engine.Sample
		res Result
	)
	for _, rm := range req.GetResourceMetrics() {
		rc := t.resource(rm.GetResource().GetAttributes())
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				out = t.metric(out, &res, rc, m)
			}
		}
	}
	return out, res
}

func (t *Translator) resource(attrs []*commonpb.KeyValue) resourceContext {
	rc := resourceContext{promoted: map[string]string{}}
	kv := make(map[string]string, len(attrs))
	keys := make([]string, 0, len(attrs))
	for _, a := range attrs {
		if _, dup := kv[a.GetKey()]; !dup {
			keys = append(keys, a.GetKey())
		}
		kv[a.GetKey()] = valueString(a.GetValue())
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0xfe)
		b.WriteString(kv[k])
		b.WriteByte(0xff)
	}
	rc.producer = b.String()
	if name := kv["service.name"]; name != "" {
		rc.job = name
		if ns := kv["service.namespace"]; ns != "" {
			rc.job = ns + "/" + name
		}
	}
	rc.instance = kv["service.instance.id"]
	for _, k := range t.promote {
		if v, ok := kv[k]; ok && v != "" {
			if ln, err := t.labelNamer.Build(k); err == nil {
				rc.promoted[ln] = v
			}
		}
	}
	return rc
}

func (t *Translator) metric(out []engine.Sample, res *Result, rc resourceContext, m *metricspb.Metric) []engine.Sample {
	switch d := m.GetData().(type) {
	case *metricspb.Metric_Sum:
		pts := d.Sum.GetDataPoints()
		res.Points += len(pts)
		if !d.Sum.GetIsMonotonic() {
			res.Ignored += len(pts)
			return out
		}
		temp, ok := temporality(d.Sum.GetAggregationTemporality())
		if !ok {
			for range pts {
				res.reject("metric %s: unspecified aggregation temporality", m.GetName())
			}
			return out
		}
		name, err := t.namer.Build(otlptranslator.Metric{Name: m.GetName(), Unit: m.GetUnit(), Type: otlptranslator.MetricTypeMonotonicCounter})
		if err != nil {
			for range pts {
				res.reject("metric %q: %v", m.GetName(), err)
			}
			return out
		}
		if !t.wants(name) {
			return out
		}
		for _, p := range pts {
			out = t.sumPoint(out, res, rc, name, temp, p)
		}
	case *metricspb.Metric_Histogram:
		pts := d.Histogram.GetDataPoints()
		res.Points += len(pts)
		temp, ok := temporality(d.Histogram.GetAggregationTemporality())
		if !ok {
			for range pts {
				res.reject("metric %s: unspecified aggregation temporality", m.GetName())
			}
			return out
		}
		base, err := t.namer.Build(otlptranslator.Metric{Name: m.GetName(), Unit: m.GetUnit(), Type: otlptranslator.MetricTypeHistogram})
		if err != nil {
			for range pts {
				res.reject("metric %q: %v", m.GetName(), err)
			}
			return out
		}
		wantCount, wantBucket := t.wants(base+"_count"), t.wants(base+"_bucket")
		if !wantCount && !wantBucket {
			return out
		}
		for _, p := range pts {
			out = t.histogramPoint(out, res, rc, base, temp, p, wantCount, wantBucket)
		}
	case *metricspb.Metric_Gauge:
		res.Points += len(d.Gauge.GetDataPoints())
		res.Ignored += len(d.Gauge.GetDataPoints())
	case *metricspb.Metric_ExponentialHistogram:
		res.Points += len(d.ExponentialHistogram.GetDataPoints())
		res.Ignored += len(d.ExponentialHistogram.GetDataPoints())
	case *metricspb.Metric_Summary:
		res.Points += len(d.Summary.GetDataPoints())
		res.Ignored += len(d.Summary.GetDataPoints())
	}
	return out
}

func temporality(t metricspb.AggregationTemporality) (engine.Temporality, bool) {
	switch t {
	case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE:
		return engine.Cumulative, true
	case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA:
		return engine.Delta, true
	}
	return 0, false
}

const flagNoRecordedValue = uint32(metricspb.DataPointFlags_DATA_POINT_FLAGS_NO_RECORDED_VALUE_MASK)

func (t *Translator) sumPoint(out []engine.Sample, res *Result, rc resourceContext, name string, temp engine.Temporality, p *metricspb.NumberDataPoint) []engine.Sample {
	if p.GetFlags()&flagNoRecordedValue != 0 {
		res.Ignored++
		return out
	}
	var v float64
	switch x := p.GetValue().(type) {
	case *metricspb.NumberDataPoint_AsDouble:
		v = x.AsDouble
	case *metricspb.NumberDataPoint_AsInt:
		v = float64(x.AsInt)
	default:
		res.reject("metric %s: data point without a value", name)
		return out
	}
	if p.GetTimeUnixNano() == 0 {
		res.reject("metric %s: data point without a timestamp", name)
		return out
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		res.reject("metric %s: monotonic sum with invalid value %v", name, v)
		return out
	}
	return append(out, engine.Sample{
		Metric: name, Labels: t.labels(rc, p.GetAttributes(), "", ""), Value: v,
		Time: nanos(p.GetTimeUnixNano()), Start: nanos(p.GetStartTimeUnixNano()),
		Temporality: temp, Producer: rc.producer,
	})
}

func (t *Translator) histogramPoint(out []engine.Sample, res *Result, rc resourceContext, base string, temp engine.Temporality,
	p *metricspb.HistogramDataPoint, wantCount, wantBucket bool) []engine.Sample {
	if p.GetFlags()&flagNoRecordedValue != 0 {
		res.Ignored++
		return out
	}
	bounds, counts := p.GetExplicitBounds(), p.GetBucketCounts()
	switch {
	case p.GetTimeUnixNano() == 0:
		res.reject("metric %s: data point without a timestamp", base)
		return out
	case len(counts) != 0 && len(counts) != len(bounds)+1:
		res.reject("metric %s: %d bucket counts for %d bounds", base, len(counts), len(bounds))
		return out
	case !sort.Float64sAreSorted(bounds):
		res.reject("metric %s: explicit bounds are not sorted", base)
		return out
	}
	var sum uint64
	for _, c := range counts {
		sum += c
	}
	if len(counts) > 0 && sum != p.GetCount() {
		res.reject("metric %s: bucket counts add up to %d, count is %d", base, sum, p.GetCount())
		return out
	}
	ts, start := nanos(p.GetTimeUnixNano()), nanos(p.GetStartTimeUnixNano())
	mk := func(name string, v float64, le string) engine.Sample {
		ls := t.labels(rc, p.GetAttributes(), "le", le)
		return engine.Sample{Metric: name, Labels: ls, Value: v, Time: ts, Start: start, Temporality: temp, Producer: rc.producer}
	}
	if wantCount {
		out = append(out, mk(base+"_count", float64(p.GetCount()), ""))
	}
	if wantBucket && len(counts) > 0 {
		var cum uint64
		for i, b := range bounds {
			cum += counts[i]
			out = append(out, mk(base+"_bucket", float64(cum), strconv.FormatFloat(b, 'f', -1, 64)))
		}
		out = append(out, mk(base+"_bucket", float64(p.GetCount()), "+Inf"))
	}
	return out
}

// labels builds the Prometheus label set of a point: sanitized point
// attributes (sorted by original key, colliding values joined with ';'),
// promoted resource attributes that do not clash, then job and instance, and
// finally the optional extra label (le).
func (t *Translator) labels(rc resourceContext, attrs []*commonpb.KeyValue, extraName, extraValue string) series.Labels {
	sorted := make([]*commonpb.KeyValue, len(attrs))
	copy(sorted, attrs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].GetKey() < sorted[j].GetKey() })
	b := series.NewBuilder()
	for _, a := range sorted {
		name, err := t.labelNamer.Build(a.GetKey())
		if err != nil {
			continue
		}
		b.Set(name, valueString(a.GetValue()))
	}
	have := b.Labels()
	for k, v := range rc.promoted {
		if have.Get(k) == "" {
			b.Override(k, v)
		}
	}
	if rc.job != "" {
		b.Override("job", rc.job)
	}
	if rc.instance != "" {
		b.Override("instance", rc.instance)
	}
	if extraName != "" {
		b.Override(extraName, extraValue)
	}
	return b.Labels()
}

// valueString renders an attribute value as the OpenTelemetry Collector's
// pcommon.Value.AsString does, which is what Prometheus uses for label values.
func valueString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_DoubleValue:
		return formatDouble(x.DoubleValue)
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue, *commonpb.AnyValue_KvlistValue:
		b, _ := json.Marshal(rawValue(v))
		return string(b)
	}
	return ""
}

func rawValue(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_DoubleValue:
		if math.IsNaN(x.DoubleValue) || math.IsInf(x.DoubleValue, 0) {
			return formatDouble(x.DoubleValue)
		}
		return x.DoubleValue
	case *commonpb.AnyValue_ArrayValue:
		vals := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			vals = append(vals, rawValue(e))
		}
		return vals
	case *commonpb.AnyValue_KvlistValue:
		m := make(map[string]any, len(x.KvlistValue.GetValues()))
		for _, kv := range x.KvlistValue.GetValues() {
			m[kv.GetKey()] = rawValue(kv.GetValue())
		}
		return m
	}
	return valueString(v)
}

func formatDouble(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func nanos(ns uint64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(ns)).UTC()
}
