package sim

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
)

var start = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

func small(incidents ...Incident) Scenario {
	return Scenario{
		Name: "t", Duration: 30 * time.Minute,
		Services:  []Service{{Name: "svc", RequestsPerStep: 1000, ErrorRatio: 0.001}},
		Incidents: incidents,
	}
}

func TestExactErrorAllocation(t *testing.T) {
	sc := small(Incident{Service: "svc", Name: "x", Start: 10 * time.Minute, Duration: 10 * time.Minute, ErrorRatio: 0.025})
	g, err := NewGenerator(sc, start)
	if err != nil {
		t.Fatal(err)
	}
	var errs, reqs uint64
	var incidentErrs uint64
	for i := 1; ; i++ {
		_, at, ok := g.Next()
		if !ok {
			break
		}
		tr := g.Truth()[0]
		errs += tr.Errors
		reqs += tr.Requests
		if off := at.Sub(start); off > 10*time.Minute && off <= 20*time.Minute {
			incidentErrs += tr.Errors
			if tr.Errors != 25 {
				t.Fatalf("minute %d: %d errors, want 25", i, tr.Errors)
			}
		}
	}
	if g.Steps() != 30 || reqs != 30000 || incidentErrs != 250 || errs != 250+20 {
		t.Fatalf("steps %d reqs %d errs %d incident %d", g.Steps(), reqs, errs, incidentErrs)
	}
	if !g.Start().Equal(start) {
		t.Fatal("Start")
	}
}

func TestLazyErrorSeriesAndCumulativeShape(t *testing.T) {
	sc := small(Incident{Service: "svc", Name: "x", Start: 5 * time.Minute, Duration: time.Minute, ErrorRatio: 1})
	sc.Services[0].ErrorRatio = 0
	g, _ := NewGenerator(sc, start)
	var lastCount uint64
	for i := 1; i <= 8; i++ {
		req, at, _ := g.Next()
		pts := req.GetResourceMetrics()[0].GetScopeMetrics()[0].GetMetrics()[0].GetHistogram().GetDataPoints()
		wantPts := 1
		if i >= 6 {
			wantPts = 2 // the 500 series appears with the first error and stays
		}
		if len(pts) != wantPts {
			t.Fatalf("step %d: %d points, want %d", i, len(pts), wantPts)
		}
		p := pts[0]
		if p.GetCount() < lastCount {
			t.Fatal("cumulative count decreased")
		}
		lastCount = p.GetCount()
		if p.GetStartTimeUnixNano() != uint64(start.UnixNano()) || p.GetTimeUnixNano() != uint64(at.UnixNano()) {
			t.Fatal("timestamps")
		}
		var sum uint64
		for _, c := range p.GetBucketCounts() {
			sum += c
		}
		if sum != p.GetCount() || len(p.GetBucketCounts()) != len(DefaultBounds)+1 {
			t.Fatalf("buckets %v count %d", p.GetBucketCounts(), p.GetCount())
		}
	}
}

func TestDeltaMatchesCumulative(t *testing.T) {
	sc := small(Incident{Service: "svc", Name: "x", Start: 3 * time.Minute, Duration: 5 * time.Minute, ErrorRatio: 0.5, SlowRatio: 0.2, SlowBucket: 9})
	cum, _ := NewGenerator(sc, start)
	sc.Delta = true
	del, _ := NewGenerator(sc, start)
	var deltaTotal uint64
	var last uint64
	for {
		creq, _, ok := cum.Next()
		dreq, at, _ := del.Next()
		if !ok {
			break
		}
		last = creq.GetResourceMetrics()[0].GetScopeMetrics()[0].GetMetrics()[0].GetHistogram().GetDataPoints()[0].GetCount()
		dp := dreq.GetResourceMetrics()[0].GetScopeMetrics()[0].GetMetrics()[0].GetHistogram().GetDataPoints()[0]
		deltaTotal += dp.GetCount()
		if dp.GetStartTimeUnixNano() != uint64(at.Add(-time.Minute).UnixNano()) {
			t.Fatal("delta start time")
		}
	}
	if deltaTotal != last {
		t.Fatalf("delta sum %d != cumulative %d", deltaTotal, last)
	}
}

func TestSlowRequestsLandInSlowBucket(t *testing.T) {
	sc := small(Incident{Service: "svc", Name: "slow", Start: 0, Duration: time.Hour, SlowRatio: 0.5, SlowBucket: 9})
	sc.Services[0].ErrorRatio = 0
	g, _ := NewGenerator(sc, start)
	req, _, _ := g.Next()
	p := req.GetResourceMetrics()[0].GetScopeMetrics()[0].GetMetrics()[0].GetHistogram().GetDataPoints()[0]
	var fast uint64
	for i := 0; i <= 6; i++ { // buckets up to 0.25s
		fast += p.GetBucketCounts()[i]
	}
	if g.Truth()[0].Slow != 500 || p.GetBucketCounts()[9] < 500 || fast > 500 {
		t.Fatalf("buckets %v truth %+v", p.GetBucketCounts(), g.Truth())
	}
}

func TestNoiseIsSeeded(t *testing.T) {
	sc := small()
	sc.Noise, sc.Seed = 0.1, 7
	a, _ := NewGenerator(sc, start)
	b, _ := NewGenerator(sc, start)
	sc.Seed = 8
	c, _ := NewGenerator(sc, start)
	differs := false
	for {
		ra, _, ok := a.Next()
		rb, _, _ := b.Next()
		rc, _, _ := c.Next()
		if !ok {
			break
		}
		if !proto.Equal(ra, rb) {
			t.Fatal("same seed must give identical output")
		}
		if !proto.Equal(ra, rc) {
			differs = true
		}
	}
	if !differs {
		t.Fatal("different seeds should differ")
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(*Scenario){
		"shorter than one step": func(s *Scenario) { s.Duration = time.Second },
		"has no name":           func(s *Scenario) { s.Services[0].Name = "" },
		"profile needs":         func(s *Scenario) { s.Services[0].Profile = []float64{1} },
		"sums to":               func(s *Scenario) { s.Services[0].Profile = make([]float64, len(DefaultBounds)+1) },
		"errorRatio":            func(s *Scenario) { s.Services[0].ErrorRatio = 2 },
		"unknown service":       func(s *Scenario) { s.Incidents = []Incident{{Service: "nope", Duration: time.Minute}} },
		"invalid ratios":        func(s *Scenario) { s.Incidents = []Incident{{Service: "svc", Duration: 0}} },
	}
	for want, mutate := range cases {
		sc := small()
		mutate(&sc)
		if _, err := NewGenerator(sc, start); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}

func TestBuiltins(t *testing.T) {
	names := Names()
	if len(names) != 8 {
		t.Fatalf("names %v", names)
	}
	for _, n := range names {
		sc, err := Lookup(n)
		if err != nil {
			t.Fatal(err)
		}
		if err := sc.Validate(); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if sc.Description == "" || sc.Duration < 4*day {
			t.Fatalf("%s: missing description or too short", n)
		}
		for _, in := range sc.Incidents {
			if in.Start < 2*day {
				t.Fatalf("%s: incident %s starts before the windows are warm", n, in.Name)
			}
		}
	}
	if _, err := Lookup("nope"); err == nil {
		t.Fatal("unknown scenario")
	}
	if bucketMid(DefaultBounds, 0) != 0.0025 || bucketMid(DefaultBounds, len(DefaultBounds)) != 15 {
		t.Fatal("bucketMid")
	}
}
