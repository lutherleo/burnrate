package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/otlptranslator"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/lutherleo/burnrate/internal/engine"
)

var ts = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func anyKV(k string, v *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: v}
}

func request(metrics ...*metricspb.Metric) *colmetricspb.ExportMetricsServiceRequest {
	return &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			str("service.name", "checkout"), str("service.namespace", "shop"),
			str("service.instance.id", "pod-1"), str("deployment.environment", "prod"),
		}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: metrics}},
	}}}
}

func counter(name, unit string, temp metricspb.AggregationTemporality, monotonic bool, pts ...*metricspb.NumberDataPoint) *metricspb.Metric {
	return &metricspb.Metric{Name: name, Unit: unit, Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
		AggregationTemporality: temp, IsMonotonic: monotonic, DataPoints: pts,
	}}}
}

func intPoint(v int64, attrs ...*commonpb.KeyValue) *metricspb.NumberDataPoint {
	return &metricspb.NumberDataPoint{
		Attributes: attrs, StartTimeUnixNano: uint64(ts.Add(-time.Hour).UnixNano()), TimeUnixNano: uint64(ts.UnixNano()),
		Value: &metricspb.NumberDataPoint_AsInt{AsInt: v},
	}
}

const cumulative = metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE

func mustTranslator(t *testing.T, opts Options, wants func(string) bool) *Translator {
	t.Helper()
	tr, err := NewTranslator(opts, wants)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestTranslateCounterNamingAndLabels(t *testing.T) {
	tr := mustTranslator(t, Options{PromoteResourceAttributes: []string{"deployment.environment", "missing.attr"}}, nil)
	req := request(counter("http.server.requests", "{request}", cumulative, true,
		intPoint(42,
			str("http.route", "/pay"),
			anyKV("http.response.status_code", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 503}}),
			str("a.b", "x"), str("a_b", "y"), // collide after sanitization
			str("deployment_environment", "staging"), // wins over the promoted resource attribute
		)))
	samples, res := tr.Translate(req)
	if res.Points != 1 || res.Rejected != 0 || len(samples) != 1 {
		t.Fatalf("result %+v samples %d", res, len(samples))
	}
	s := samples[0]
	// Annotation units in braces are dropped and monotonic sums get _total.
	if s.Metric != "http_server_requests_total" {
		t.Fatalf("metric %q", s.Metric)
	}
	want := `{a_b="x;y", deployment_environment="staging", http_response_status_code="503", http_route="/pay", instance="pod-1", job="shop/checkout"}`
	if s.Labels.String() != want {
		t.Fatalf("labels\n got %s\nwant %s", s.Labels, want)
	}
	if s.Value != 42 || !s.Time.Equal(ts) || !s.Start.Equal(ts.Add(-time.Hour)) || s.Temporality != engine.Cumulative || s.Producer == "" {
		t.Fatalf("sample %+v", s)
	}
}

func TestTranslatePromotesResourceAttributes(t *testing.T) {
	tr := mustTranslator(t, Options{PromoteResourceAttributes: []string{"deployment.environment"}}, nil)
	samples, _ := tr.Translate(request(counter("reqs", "1", cumulative, true, intPoint(1))))
	if got := samples[0].Labels.Get("deployment_environment"); got != "prod" {
		t.Fatalf("promoted label = %q", got)
	}
	if samples[0].Metric != "reqs_total" {
		t.Fatalf("unit 1 should add no suffix: %q", samples[0].Metric)
	}
}

func histogram(temp metricspb.AggregationTemporality, pts ...*metricspb.HistogramDataPoint) *metricspb.Metric {
	return &metricspb.Metric{Name: "http.server.request.duration", Unit: "s", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
		AggregationTemporality: temp, DataPoints: pts,
	}}}
}

func histPoint(count uint64, bounds []float64, buckets []uint64) *metricspb.HistogramDataPoint {
	return &metricspb.HistogramDataPoint{
		Attributes: []*commonpb.KeyValue{str("http.route", "/pay")}, TimeUnixNano: uint64(ts.UnixNano()),
		StartTimeUnixNano: uint64(ts.Add(-time.Hour).UnixNano()), Count: count, ExplicitBounds: bounds, BucketCounts: buckets,
	}
}

func TestTranslateHistogram(t *testing.T) {
	tr := mustTranslator(t, Options{}, nil)
	samples, res := tr.Translate(request(histogram(cumulative, histPoint(10, []float64{0.1, 0.25, 1}, []uint64{2, 5, 2, 1}))))
	if res.Rejected != 0 {
		t.Fatalf("%+v", res)
	}
	got := map[string]float64{}
	for _, s := range samples {
		got[s.Metric+s.Labels.Get("le")] = s.Value
	}
	want := map[string]float64{
		"http_server_request_duration_seconds_count":      10,
		"http_server_request_duration_seconds_bucket0.1":  2,
		"http_server_request_duration_seconds_bucket0.25": 7,
		"http_server_request_duration_seconds_bucket1":    9,
		"http_server_request_duration_seconds_bucket+Inf": 10,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %v, want %v (all %v)", k, got[k], v, got)
		}
	}
}

func TestTranslateSkipsUnwantedAndIgnoresUnsupported(t *testing.T) {
	wanted := map[string]bool{"http_server_request_duration_seconds_count": true}
	tr := mustTranslator(t, Options{}, func(m string) bool { return wanted[m] })
	gauge := &metricspb.Metric{Name: "temp", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{intPoint(1)}}}}
	exp := &metricspb.Metric{Name: "e", Data: &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{DataPoints: []*metricspb.ExponentialHistogramDataPoint{{}}}}}
	sum := &metricspb.Metric{Name: "s", Data: &metricspb.Metric_Summary{Summary: &metricspb.Summary{DataPoints: []*metricspb.SummaryDataPoint{{}}}}}
	updown := counter("queue.depth", "1", cumulative, false, intPoint(3))
	unwanted := counter("other", "1", cumulative, true, intPoint(3))
	noValue := intPoint(0)
	noValue.Flags = uint32(metricspb.DataPointFlags_DATA_POINT_FLAGS_NO_RECORDED_VALUE_MASK)
	flagged := histPoint(1, nil, nil)
	flagged.Flags = noValue.Flags
	samples, res := tr.Translate(request(gauge, exp, sum, updown, unwanted,
		histogram(cumulative, histPoint(4, []float64{1}, []uint64{3, 1}), flagged),
		counter("flagged", "1", cumulative, true, noValue)))
	if len(samples) != 1 || samples[0].Metric != "http_server_request_duration_seconds_count" {
		t.Fatalf("samples %+v", samples)
	}
	// gauge, exp, summary, updown and the flagged histogram point are
	// ignored; "flagged" and "other" are not wanted so never inspected.
	if res.Points != 8 || res.Ignored != 5 || res.Rejected != 0 {
		t.Fatalf("result %+v", res)
	}
}

func TestTranslateRejectsMalformedPoints(t *testing.T) {
	tr := mustTranslator(t, Options{}, nil)
	noTime := intPoint(1)
	noTime.TimeUnixNano = 0
	negative := intPoint(-1)
	nan := &metricspb.NumberDataPoint{TimeUnixNano: 1, Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: math.NaN()}}
	empty := &metricspb.NumberDataPoint{TimeUnixNano: 1}
	badBuckets := histPoint(3, []float64{1, 2}, []uint64{1, 2})
	unsorted := histPoint(3, []float64{2, 1}, []uint64{1, 1, 1})
	wrongCount := histPoint(5, []float64{1}, []uint64{1, 1})
	histNoTime := histPoint(1, nil, nil)
	histNoTime.TimeUnixNano = 0
	unspecified := metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_UNSPECIFIED
	_, res := tr.Translate(request(
		counter("c", "1", cumulative, true, noTime, negative, nan, empty),
		histogram(cumulative, badBuckets, unsorted, wrongCount, histNoTime),
		counter("u", "1", unspecified, true, intPoint(1)),
		histogram(unspecified, histPoint(1, nil, nil)),
	))
	if res.Rejected != 10 || len(res.Errors) != maxErrors {
		t.Fatalf("result %+v", res)
	}
	joined := strings.Join(res.Errors, "|")
	for _, want := range []string{"without a timestamp", "invalid value", "without a value", "bucket counts for"} {
		if !strings.Contains(joined, want) {
			t.Errorf("errors %q lack %q", joined, want)
		}
	}
}

func TestValueStrings(t *testing.T) {
	cases := map[string]*commonpb.AnyValue{
		"true":  {Value: &commonpb.AnyValue_BoolValue{BoolValue: true}},
		"0.5":   {Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.5}},
		"1e+21": {Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1e21}},
		"+Inf":  {Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.Inf(1)}},
		"-Inf":  {Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.Inf(-1)}},
		"NaN":   {Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.NaN()}},
		"aGk=":  {Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("hi")}},
		`["a",1,true,2.5,"NaN"]`: {Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{
			{Value: &commonpb.AnyValue_StringValue{StringValue: "a"}}, {Value: &commonpb.AnyValue_IntValue{IntValue: 1}},
			{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}, {Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 2.5}},
			{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.NaN()}},
		}}}},
		`{"k":{"n":"x"}}`: {Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{
			anyKV("k", &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{str("n", "x")}}}}),
		}}}},
		"": {},
	}
	for want, v := range cases {
		if got := valueString(v); got != want {
			t.Errorf("valueString = %q, want %q", got, want)
		}
	}
}

func TestUTF8StrategyRejected(t *testing.T) {
	if _, err := NewTranslator(Options{Strategy: otlptranslator.NoUTF8EscapingWithSuffixes}, nil); err == nil {
		t.Fatal("UTF-8 strategy must be rejected")
	}
	tr := mustTranslator(t, Options{Strategy: otlptranslator.UnderscoreEscapingWithoutSuffixes}, nil)
	samples, _ := tr.Translate(request(counter("http.server.requests", "1", cumulative, true, intPoint(1))))
	if samples[0].Metric != "http_server_requests" {
		t.Fatalf("without suffixes: %q", samples[0].Metric)
	}
}

type collector struct {
	mu      sync.Mutex
	samples []engine.Sample
}

func (c *collector) sink(s []engine.Sample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, s...)
}

func newServer(t *testing.T) (*httptest.Server, *Receiver, *collector) {
	t.Helper()
	col := &collector{}
	rc := NewReceiver(mustTranslator(t, Options{}, nil), col.sink, nil)
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	return srv, rc, col
}

func post(t *testing.T, url, ct, enc string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+Path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", ct)
	if enc != "" {
		req.Header.Set("Content-Encoding", enc)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func gz(t *testing.T, b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReceiverProtobufGzipAndJSON(t *testing.T) {
	srv, rc, col := newServer(t)
	req := request(counter("reqs", "1", cumulative, true, intPoint(7)))
	pb, _ := proto.Marshal(req)

	resp := post(t, srv.URL, "application/x-protobuf", "", pb)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != ContentTypeProtobuf {
		t.Fatalf("protobuf: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp = post(t, srv.URL, "application/x-protobuf", "gzip", gz(t, pb))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gzip: %d", resp.StatusCode)
	}
	js, _ := protojson.Marshal(req)
	resp = post(t, srv.URL, "application/json; charset=utf-8", "", js)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != ContentTypeJSON {
		t.Fatalf("json: %d", resp.StatusCode)
	}
	if len(col.samples) != 3 || rc.Stats.PointsAccepted.Load() != 3 || rc.Stats.Requests.Load() != 3 {
		t.Fatalf("samples %d accepted %d", len(col.samples), rc.Stats.PointsAccepted.Load())
	}
}

func TestReceiverPartialSuccess(t *testing.T) {
	srv, rc, _ := newServer(t)
	bad := intPoint(1)
	bad.TimeUnixNano = 0
	pb, _ := proto.Marshal(request(counter("reqs", "1", cumulative, true, intPoint(1), bad)))
	resp := post(t, srv.URL, ContentTypeProtobuf, "", pb)
	defer func() { _ = resp.Body.Close() }()
	var body bytes.Buffer
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	out := &colmetricspb.ExportMetricsServiceResponse{}
	if err := proto.Unmarshal(body.Bytes(), out); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || out.GetPartialSuccess().GetRejectedDataPoints() != 1 || !strings.Contains(out.GetPartialSuccess().GetErrorMessage(), "timestamp") {
		t.Fatalf("partial success %v", out)
	}
	if rc.Stats.PointsRejected.Load() != 1 {
		t.Fatal("rejected counter")
	}
}

func TestReceiverErrors(t *testing.T) {
	srv, rc, _ := newServer(t)
	rc.SetMaxBodyBytes(64)
	big := make([]byte, 1000)
	cases := []struct {
		name, ct, enc string
		body          []byte
		status        int
	}{
		{"content type", "text/plain", "", []byte("x"), http.StatusUnsupportedMediaType},
		{"bad proto", ContentTypeProtobuf, "", []byte{0xff, 0xff, 0xff}, http.StatusBadRequest},
		{"bad json", ContentTypeJSON, "", []byte("{"), http.StatusBadRequest},
		{"encoding", ContentTypeProtobuf, "br", []byte("x"), http.StatusUnsupportedMediaType},
		{"bad gzip", ContentTypeProtobuf, "gzip", []byte("not gzip"), http.StatusBadRequest},
		{"too large", ContentTypeProtobuf, "", big, http.StatusRequestEntityTooLarge},
		{"gzip bomb", ContentTypeProtobuf, "gzip", gz(t, big), http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := post(t, srv.URL, c.ct, c.enc, c.body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != c.status {
				t.Fatalf("status %d, want %d", resp.StatusCode, c.status)
			}
		})
	}
	resp, err := http.Get(srv.URL + Path)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	if rc.Stats.RequestErrors.Load() != uint64(len(cases)+1) {
		t.Fatalf("errors %d", rc.Stats.RequestErrors.Load())
	}
}

func TestClientExportAndRetry(t *testing.T) {
	var calls atomic.Int32
	col := &collector{}
	rc := NewReceiver(mustTranslator(t, Options{}, nil), col.sink, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			b, _ := proto.Marshal(&statuspb.Status{Code: 14, Message: "busy"})
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(b)
			return
		}
		rc.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL + "/", Gzip: true, MaxRetries: 2, Backoff: time.Millisecond}
	resp, err := c.Export(context.Background(), request(counter("reqs", "1", cumulative, true, intPoint(3))))
	if err != nil || resp == nil || calls.Load() != 2 || len(col.samples) != 1 {
		t.Fatalf("resp %v err %v calls %d", resp, err, calls.Load())
	}

	// Non-retryable failures surface the server's status message.
	bad := &Client{Endpoint: srv.URL}
	calls.Store(1)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		b, _ := proto.Marshal(&statuspb.Status{Code: 3, Message: "nope"})
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(b)
	})
	if _, err := bad.Export(context.Background(), request()); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
	// Retries stop when the context is cancelled.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	retrying := &Client{Endpoint: srv.URL, MaxRetries: 5, Backoff: time.Hour}
	if _, err := retrying.Export(ctx, request()); err == nil {
		t.Fatal("expected error")
	}
	// Garbage in a 200 response.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte{0xff, 0xff}) })
	if _, err := bad.Export(context.Background(), request()); err == nil || !strings.Contains(err.Error(), "decoding response") {
		t.Fatalf("err = %v", err)
	}
	// Unreachable endpoint.
	down := &Client{Endpoint: "http://127.0.0.1:1"}
	if _, err := down.Export(context.Background(), request()); err == nil {
		t.Fatal("expected connection error")
	}
}
