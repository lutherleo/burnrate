// Package sim generates synthetic OTLP traffic for SLO testing and demos.
//
// A Scenario describes services (request volume, baseline error ratio and
// latency profile) and incidents that change error ratio or latency for a
// while. The Generator turns it into a stream of OTLP
// ExportMetricsServiceRequests, one per export step, shaped like what an
// OpenTelemetry SDK emits for the http.server.request.duration histogram:
// cumulative temporality, one series per status code, error series created
// lazily on the first error.
//
// Error and bucket counts are allocated with carried remainders, so a 2%
// incident produces exactly 2% errors over any whole number of steps. That
// determinism is what lets tests assert alert times to the minute; optional
// volume noise (seeded) is available for demos.
package sim

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// MetricName and MetricUnit follow the OpenTelemetry HTTP semantic
// conventions; Prometheus names the histogram
// http_server_request_duration_seconds.
const (
	MetricName = "http.server.request.duration"
	MetricUnit = "s"
	ScopeName  = "burnrate/sim"
)

// DefaultBounds are the semantic-convention default buckets for
// http.server.request.duration.
var DefaultBounds = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

// DefaultProfile is a healthy latency distribution over DefaultBounds: the
// share of requests in each bucket (len(DefaultBounds)+1 entries). 99.5% of
// requests finish within 250 ms.
var DefaultProfile = []float64{0, 0.02, 0.10, 0.25, 0.20, 0.15, 0.275, 0.0035, 0.001, 0.0005, 0, 0, 0, 0, 0}

// Service is one simulated HTTP service.
type Service struct {
	Name     string // service.name resource attribute (becomes job)
	Instance string // service.instance.id (becomes instance)
	Method   string
	Route    string
	// RequestsPerStep is the mean request count per export step.
	RequestsPerStep float64
	// ErrorRatio is the baseline share of 500 responses.
	ErrorRatio float64
	// Profile is the latency distribution over Bounds; it must have
	// len(Bounds)+1 entries summing to 1. Defaults to DefaultProfile.
	Profile []float64
	Bounds  []float64
}

// Incident changes a service's behaviour for a while.
type Incident struct {
	Service string
	Name    string
	// Start is the offset from the scenario start.
	Start    time.Duration
	Duration time.Duration
	// ErrorRatio replaces the baseline error ratio when > 0.
	ErrorRatio float64
	// SlowRatio moves this share of successful requests into SlowBucket
	// (an index into the bucket counts) when > 0.
	SlowRatio  float64
	SlowBucket int
}

func (in Incident) active(offset time.Duration) bool {
	return offset > in.Start && offset <= in.Start+in.Duration
}

// Scenario is a complete traffic script.
type Scenario struct {
	Name        string
	Description string
	Duration    time.Duration
	// Step is the export interval. Default 1m.
	Step      time.Duration
	Services  []Service
	Incidents []Incident
	// Noise is the relative standard deviation of per-step request volume;
	// 0 keeps the volume constant.
	Noise float64
	Seed  uint64
	// Delta switches the exporter to delta temporality.
	Delta bool
}

// Validate checks the scenario for mistakes.
func (sc *Scenario) Validate() error {
	if sc.Step <= 0 {
		sc.Step = time.Minute
	}
	if sc.Duration < sc.Step {
		return fmt.Errorf("scenario %s: duration %s shorter than one step", sc.Name, sc.Duration)
	}
	names := map[string]bool{}
	for i := range sc.Services {
		s := &sc.Services[i]
		if s.Name == "" {
			return fmt.Errorf("scenario %s: service %d has no name", sc.Name, i)
		}
		names[s.Name] = true
		if s.Bounds == nil {
			s.Bounds = DefaultBounds
		}
		if s.Profile == nil {
			s.Profile = DefaultProfile
		}
		if s.Method == "" {
			s.Method = "GET"
		}
		if s.Route == "" {
			s.Route = "/"
		}
		if len(s.Profile) != len(s.Bounds)+1 {
			return fmt.Errorf("scenario %s: service %s: profile needs %d entries", sc.Name, s.Name, len(s.Bounds)+1)
		}
		sum := 0.0
		for _, p := range s.Profile {
			sum += p
		}
		if math.Abs(sum-1) > 1e-9 {
			return fmt.Errorf("scenario %s: service %s: profile sums to %v, not 1", sc.Name, s.Name, sum)
		}
		if s.ErrorRatio < 0 || s.ErrorRatio > 1 || s.RequestsPerStep <= 0 {
			return fmt.Errorf("scenario %s: service %s: need 0 <= errorRatio <= 1 and requests > 0", sc.Name, s.Name)
		}
	}
	for _, in := range sc.Incidents {
		if !names[in.Service] {
			return fmt.Errorf("scenario %s: incident %s targets unknown service %q", sc.Name, in.Name, in.Service)
		}
		if in.ErrorRatio < 0 || in.ErrorRatio > 1 || in.SlowRatio < 0 || in.SlowRatio > 1 || in.Duration <= 0 {
			return fmt.Errorf("scenario %s: incident %s has invalid ratios or duration", sc.Name, in.Name)
		}
	}
	return nil
}

// series is one status code's histogram for one service.
type series struct {
	code    int64
	created bool
	count   uint64
	sum     float64
	buckets []uint64
	// previous values, for delta temporality
	prevCount   uint64
	prevSum     float64
	prevBuckets []uint64
}

type serviceState struct {
	svc        Service
	ok, failed *series
	errCarry   float64 // carried fractional errors
	slowCarry  float64
	bucketRem  []float64 // carried fractional bucket shares
}

// StepTruth is the ground truth of one step for one service.
type StepTruth struct {
	Service  string
	Requests uint64
	Errors   uint64
	Slow     uint64 // successful requests moved to the slow bucket
}

// Generator produces the OTLP export stream of a scenario.
type Generator struct {
	sc     Scenario
	start  time.Time
	step   int
	steps  int
	rng    *rand.Rand
	states []*serviceState
	truth  []StepTruth
}

// NewGenerator validates the scenario and returns a generator whose stream
// begins at start (the SDK start time; the first export is at start+Step).
func NewGenerator(sc Scenario, start time.Time) (*Generator, error) {
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	g := &Generator{sc: sc, start: start, steps: int(sc.Duration / sc.Step), rng: rand.New(rand.NewPCG(sc.Seed, sc.Seed^0x9e3779b97f4a7c15))}
	for _, s := range sc.Services {
		nb := len(s.Bounds) + 1
		g.states = append(g.states, &serviceState{
			svc:       s,
			ok:        &series{code: 200, created: true, buckets: make([]uint64, nb), prevBuckets: make([]uint64, nb)},
			failed:    &series{code: 500, buckets: make([]uint64, nb), prevBuckets: make([]uint64, nb)},
			bucketRem: make([]float64, nb),
		})
	}
	return g, nil
}

// Steps returns the total number of exports.
func (g *Generator) Steps() int { return g.steps }

// Start returns the SDK start time.
func (g *Generator) Start() time.Time { return g.start }

// Truth returns the ground truth of the most recent step.
func (g *Generator) Truth() []StepTruth { return g.truth }

// Next returns the next export request and its timestamp; ok is false when
// the scenario is over.
func (g *Generator) Next() (req *colmetricspb.ExportMetricsServiceRequest, at time.Time, ok bool) {
	if g.step >= g.steps {
		return nil, time.Time{}, false
	}
	g.step++
	offset := time.Duration(g.step) * g.sc.Step
	at = g.start.Add(offset)
	req = &colmetricspb.ExportMetricsServiceRequest{}
	g.truth = g.truth[:0]
	for _, st := range g.states {
		g.advance(st, offset)
		req.ResourceMetrics = append(req.ResourceMetrics, g.export(st, at))
	}
	return req, at, true
}

func (g *Generator) advance(st *serviceState, offset time.Duration) {
	s := st.svc
	n := s.RequestsPerStep
	if g.sc.Noise > 0 {
		n *= math.Max(0, 1+g.sc.Noise*g.rng.NormFloat64())
	}
	requests := uint64(math.Round(n))
	errRatio, slowRatio, slowBucket := s.ErrorRatio, 0.0, len(s.Bounds)-1
	for _, in := range g.sc.Incidents {
		if in.Service != s.Name || !in.active(offset) {
			continue
		}
		if in.ErrorRatio > 0 {
			errRatio = in.ErrorRatio
		}
		if in.SlowRatio > 0 {
			slowRatio, slowBucket = in.SlowRatio, in.SlowBucket
		}
	}
	st.errCarry += float64(requests) * errRatio
	errors := uint64(math.Floor(st.errCarry + 1e-9))
	errors = min(errors, requests)
	st.errCarry -= float64(errors)
	okCount := requests - errors

	st.slowCarry += float64(okCount) * slowRatio
	slow := min(uint64(math.Floor(st.slowCarry+1e-9)), okCount)
	st.slowCarry -= float64(slow)

	// Healthy requests (successes not slowed, plus all failures) follow
	// the latency profile; slowed successes land in slowBucket.
	okDist := g.allocate(st, okCount-slow)
	for i, c := range okDist {
		st.ok.buckets[i] += c
		st.ok.sum += float64(c) * bucketMid(s.Bounds, i)
	}
	if slow > 0 {
		b := min(max(slowBucket, 0), len(s.Bounds))
		st.ok.buckets[b] += slow
		st.ok.sum += float64(slow) * bucketMid(s.Bounds, b)
	}
	st.ok.count += okCount
	if errors > 0 {
		st.failed.created = true
		// Failures are fast: they all land in the fastest non-empty bucket
		// of the profile.
		fb := 0
		for fb < len(s.Profile)-1 && s.Profile[fb] == 0 {
			fb++
		}
		st.failed.buckets[fb] += errors
		st.failed.count += errors
		st.failed.sum += float64(errors) * bucketMid(s.Bounds, fb)
	}
	g.truth = append(g.truth, StepTruth{Service: s.Name, Requests: requests, Errors: errors, Slow: slow})
}

// allocate splits n requests across the profile with carried remainders so
// the long-run shares match the profile exactly.
func (g *Generator) allocate(st *serviceState, n uint64) []uint64 {
	out := make([]uint64, len(st.svc.Profile))
	var used uint64
	for i, p := range st.svc.Profile {
		st.bucketRem[i] += float64(n) * p
		c := uint64(math.Floor(st.bucketRem[i] + 1e-9))
		if used+c > n {
			c = n - used
		}
		st.bucketRem[i] -= float64(c)
		out[i] = c
		used += c
	}
	// Rounding leftovers go to the most likely bucket.
	if used < n {
		best := 0
		for i, p := range st.svc.Profile {
			if p > st.svc.Profile[best] {
				best = i
			}
		}
		out[best] += n - used
		st.bucketRem[best] -= float64(n - used)
	}
	return out
}

func bucketMid(bounds []float64, i int) float64 {
	switch {
	case i == 0:
		return bounds[0] / 2
	case i >= len(bounds):
		return bounds[len(bounds)-1] * 1.5
	}
	return (bounds[i-1] + bounds[i]) / 2
}

func (g *Generator) export(st *serviceState, at time.Time) *metricspb.ResourceMetrics {
	s := st.svc
	temporality := metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
	if g.sc.Delta {
		temporality = metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
	}
	hist := &metricspb.Histogram{AggregationTemporality: temporality}
	for _, se := range []*series{st.ok, st.failed} {
		if !se.created {
			continue
		}
		hist.DataPoints = append(hist.DataPoints, g.point(s, se, at))
	}
	res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		strAttr("service.name", s.Name),
		strAttr("telemetry.sdk.name", "burnrate-sim"),
	}}
	if s.Instance != "" {
		res.Attributes = append(res.Attributes, strAttr("service.instance.id", s.Instance))
	}
	return &metricspb.ResourceMetrics{
		Resource: res,
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Scope: &commonpb.InstrumentationScope{Name: ScopeName},
			Metrics: []*metricspb.Metric{{
				Name: MetricName, Unit: MetricUnit,
				Description: "Duration of HTTP server requests.",
				Data:        &metricspb.Metric_Histogram{Histogram: hist},
			}},
		}},
	}
}

func (g *Generator) point(s Service, se *series, at time.Time) *metricspb.HistogramDataPoint {
	count, sum := se.count, se.sum
	buckets := append([]uint64(nil), se.buckets...)
	start := g.start
	if g.sc.Delta {
		count -= se.prevCount
		sum -= se.prevSum
		for i := range buckets {
			buckets[i] -= se.prevBuckets[i]
		}
		start = at.Add(-g.sc.Step)
		se.prevCount, se.prevSum = se.count, se.sum
		copy(se.prevBuckets, se.buckets)
	}
	return &metricspb.HistogramDataPoint{
		Attributes: []*commonpb.KeyValue{
			strAttr("http.request.method", s.Method),
			strAttr("http.route", s.Route),
			{Key: "http.response.status_code", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: se.code}}},
		},
		StartTimeUnixNano: uint64(start.UnixNano()),
		TimeUnixNano:      uint64(at.UnixNano()),
		Count:             count,
		Sum:               &sum,
		BucketCounts:      buckets,
		ExplicitBounds:    append([]float64(nil), s.Bounds...),
	}
}

func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}
