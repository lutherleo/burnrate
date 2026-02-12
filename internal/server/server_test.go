package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/lutherleo/burnrate/internal/engine"
	"github.com/lutherleo/burnrate/internal/otlp"
	"github.com/lutherleo/burnrate/internal/sim"
	"github.com/lutherleo/burnrate/internal/slo"
)

type recorder struct {
	mu     sync.Mutex
	events []engine.Event
	calls  int
}

func (r *recorder) Notify(_ context.Context, _ time.Time, evs []engine.Event, _ []engine.ActiveAlert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.events = append(r.events, evs...)
	return nil
}

func exampleSLOs(t *testing.T) []*slo.SLO {
	t.Helper()
	slos, err := slo.LoadPaths("../../examples/slos")
	if err != nil {
		t.Fatal(err)
	}
	return slos
}

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func getText(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// TestDataClockReplayOverHTTP pushes the outage scenario through the real
// OTLP/HTTP client into the server and checks the API reflects it.
func TestDataClockReplayOverHTTP(t *testing.T) {
	rec := &recorder{}
	srv, err := New(Config{Clock: DataClock, Notifier: rec}, exampleSLOs(t))
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	sc, err := sim.Lookup("outage")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	gen, err := sim.NewGenerator(sc, start)
	if err != nil {
		t.Fatal(err)
	}
	client := &otlp.Client{Endpoint: hs.URL, Gzip: true}
	for {
		req, _, ok := gen.Next()
		if !ok {
			break
		}
		if _, err := client.Export(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}

	var slos slosResponse
	if code := getJSON(t, hs.URL+"/api/v1/slos", &slos); code != 200 || len(slos.SLOs) != 3 || slos.Clock != DataClock {
		t.Fatalf("slos %d %+v", code, slos)
	}
	if !slos.EvaluatedAt.Equal(start.Add(sc.Duration)) {
		t.Fatalf("evaluated at %v", slos.EvaluatedAt)
	}
	var one engine.Status
	if code := getJSON(t, hs.URL+"/api/v1/slos/checkout-availability", &one); code != 200 || one.Budget.State != "exhausted" {
		t.Fatalf("slo %d %+v", code, one.Budget)
	}
	if code := getJSON(t, hs.URL+"/api/v1/slos/nope", nil); code != 404 {
		t.Fatalf("unknown slo %d", code)
	}

	var evs struct{ Events []engine.Event }
	getJSON(t, hs.URL+"/api/v1/events?slo=checkout-availability&kind=alert_firing", &evs)
	if len(evs.Events) != 2 || evs.Events[0].Severity != "page" || evs.Events[1].Severity != "ticket" {
		t.Fatalf("events %+v", evs.Events)
	}
	getJSON(t, hs.URL+"/api/v1/events?limit=1", &evs)
	if len(evs.Events) != 1 {
		t.Fatalf("limit: %d", len(evs.Events))
	}
	if code := getJSON(t, hs.URL+"/api/v1/events?limit=x", nil); code != 400 {
		t.Fatalf("bad limit %d", code)
	}
	var alerts struct{ Alerts []alertJSON }
	getJSON(t, hs.URL+"/api/v1/alerts", &alerts)
	if len(alerts.Alerts) != 0 {
		t.Fatalf("alerts should have resolved: %+v", alerts)
	}

	rec.mu.Lock()
	fired := 0
	for _, ev := range rec.events {
		if ev.Kind == engine.EventAlertFiring {
			fired++
		}
	}
	rec.mu.Unlock()
	if fired != 2 || rec.calls == 0 {
		t.Fatalf("notifier saw %d firings in %d calls", fired, rec.calls)
	}

	metrics := getText(t, hs.URL+"/metrics")
	for _, want := range []string{
		`burnrate_slo_error_budget_remaining_ratio{slo="checkout-availability"}`,
		`burnrate_slo_budget_policy_state{slo="checkout-availability",state="exhausted"} 1`,
		`burnrate_slo_alert_firing{severity="page",slo="checkout-availability"} 0`,
		`burnrate_slo_burn_rate{slo="search-availability",window="7d"}`,
		`burnrate_otlp_requests_total 5760`,
		`burnrate_otlp_points_total{result="rejected"} 0`,
		`burnrate_engine_evaluations_total 5760`,
		"# TYPE burnrate_engine_samples_total counter",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	index := getText(t, hs.URL+"/")
	if !strings.Contains(index, "checkout-availability") || !strings.Contains(index, "data clock") {
		t.Fatalf("index:\n%s", index)
	}
	if got := getText(t, hs.URL+"/healthz"); got != "ok\n" {
		t.Fatalf("healthz %q", got)
	}
	if srv.Engine() == nil {
		t.Fatal("Engine accessor")
	}
}

const fastSLO = `apiVersion: burnrate/v1
kind: SLO
metadata: {name: fast}
spec:
  service: api
  timeWindow: [{duration: 1h}]
  objectives: [{target: 0.9}]
  indicator:
    ratio:
      total: reqs_total{job="api"}
      bad: reqs_total{job="api", code="500"}
  alerting:
    windows:
      - {severity: page, longWindow: 4s, shortWindow: 2s, burnRate: 2}
`

func counterRequest(start, at time.Time, good, bad int64) *colmetricspb.ExportMetricsServiceRequest {
	pt := func(code string, v int64) *metricspb.NumberDataPoint {
		return &metricspb.NumberDataPoint{
			Attributes:        []*commonpb.KeyValue{{Key: "code", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: code}}}},
			StartTimeUnixNano: uint64(start.UnixNano()), TimeUnixNano: uint64(at.UnixNano()),
			Value: &metricspb.NumberDataPoint_AsInt{AsInt: v},
		}
	}
	return &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api"}}}}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
			Name: "reqs", Unit: "1",
			Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				IsMonotonic: true, AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				DataPoints: []*metricspb.NumberDataPoint{pt("200", good), pt("500", bad)},
			}},
		}}}},
	}}}
}

// TestWallClockServe runs the real listener and ticker with a 1s resolution
// and checks a live 50% error burst fires the page.
func TestWallClockServe(t *testing.T) {
	defs, err := slo.Parse([]byte(fastSLO), "fast.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	srv, err := New(Config{Engine: engine.Config{Resolution: time.Second}, Lateness: time.Millisecond, Notifier: rec}, defs)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	client := &otlp.Client{Endpoint: "http://" + ln.Addr().String()}
	var good, bad int64
	streamStart := time.Now().Add(-time.Hour)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		good, bad = good+50, bad+50
		if _, err := client.Export(ctx, counterRequest(streamStart, time.Now(), good, bad)); err != nil {
			t.Fatal(err)
		}
		if len(srv.Engine().ActiveAlerts()) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(srv.Engine().ActiveAlerts()) == 0 {
		t.Fatalf("page never fired; status %+v", srv.Engine().Status())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{Clock: "lunar"}, exampleSLOs(t)); err == nil {
		t.Fatal("unknown clock")
	}
	if _, err := New(Config{Engine: engine.Config{Resolution: 7 * time.Minute}}, exampleSLOs(t)); err == nil {
		t.Fatal("bad resolution")
	}
	if _, err := New(Config{Translation: otlp.Options{Strategy: "NoTranslation"}}, exampleSLOs(t)); err == nil {
		t.Fatal("bad strategy")
	}
	if formatTime(time.Time{}) != "never" || formatValue(0.5) != "0.5" || escapeLabel("a\"b\\\n") != `a\"b\\\n` {
		t.Fatal("helpers")
	}
}
