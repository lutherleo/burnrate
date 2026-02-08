package burnmath

import (
	"math"
	"testing"
	"time"
)

const day = 24 * time.Hour

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The workbook's recommended table for a 30-day, 99.9% SLO: budget consumed,
// long window and resulting burn rate.
func TestWorkbookBurnRates(t *testing.T) {
	cases := []struct {
		budget float64
		window time.Duration
		burn   float64
	}{
		{0.02, time.Hour, 14.4},
		{0.05, 6 * time.Hour, 6},
		{0.10, day, 3},
		{0.10, 3 * day, 1},
	}
	for _, c := range cases {
		if got := BurnRate(c.budget, 30*day, c.window); !near(got, c.burn) {
			t.Errorf("BurnRate(%v, 30d, %v) = %v, want %v", c.budget, c.window, got, c.burn)
		}
		if got := BudgetFraction(c.burn, 30*day, c.window); !near(got, c.budget) {
			t.Errorf("BudgetFraction(%v) = %v, want %v", c.burn, got, c.budget)
		}
	}
	if got := ErrorRatioThreshold(14.4, 0.999); !near(got, 0.0144) {
		t.Errorf("threshold = %v", got)
	}
}

func TestDetectionTime(t *testing.T) {
	// 100% errors against 14.4x over 1h at 99.9%: 1h * 0.0144 = 51.84s.
	d, ok := DetectionTime(time.Hour, 14.4, 0.999, 1.0)
	if !ok || d != 51840*time.Millisecond {
		t.Fatalf("got %v %v", d, ok)
	}
	// 2% errors (burn 20): 1h * 14.4/20 = 43.2 min.
	d, ok = DetectionTime(time.Hour, 14.4, 0.999, 0.02)
	if !ok || d != 43*time.Minute+12*time.Second {
		t.Fatalf("got %v", d)
	}
	// At or under the threshold the alert never fires.
	if _, ok := DetectionTime(time.Hour, 14.4, 0.999, 0.0144); ok {
		t.Fatal("ratio equal to threshold must not fire")
	}
	if _, ok := DetectionTime(time.Hour, 14.4, 0.999, 0); ok {
		t.Fatal("zero errors must not fire")
	}
}

func TestResetTime(t *testing.T) {
	// 100% outage, 5m short window, 14.4x: 5m * (1 - 0.0144) = 4m55.68s.
	d, ok := ResetTime(5*time.Minute, 14.4, 0.999, 1.0)
	if !ok || d != 4*time.Minute+55680*time.Millisecond {
		t.Fatalf("got %v", d)
	}
	if _, ok := ResetTime(5*time.Minute, 14.4, 0.999, 0.01); ok {
		t.Fatal("never fired, so no reset")
	}
}

func workbookPage() []Pair {
	return []Pair{{Long: time.Hour, Short: 5 * time.Minute, Burn: 14.4}, {Long: 6 * time.Hour, Short: 30 * time.Minute, Burn: 6}}
}

func TestSeverityTimes(t *testing.T) {
	// 1% errors (burn 10) only trips the 6h/30m pair: 6h * 6/10 = 3.6h.
	d, ok := SeverityDetectionTime(workbookPage(), 0.999, 0.01)
	if !ok || d != 3*time.Hour+36*time.Minute {
		t.Fatalf("detect = %v %v", d, ok)
	}
	if _, ok := SeverityDetectionTime(workbookPage(), 0.999, 0.005); ok {
		t.Fatal("0.5% must not page")
	}
	// A 10 minute full outage: the 1h pair fires after 51.84s and the 6h pair
	// after 2m9.6s, so both are firing when it ends and the 30m short window
	// sets the reset time: 30m * (1 - 0.006) = 29m49.2s.
	r, ok := SeverityResetTime(workbookPage(), 0.999, 1.0, 10*time.Minute)
	if !ok || r != 29*time.Minute+49200*time.Millisecond {
		t.Fatalf("reset = %v %v", r, ok)
	}
	// A 90 second outage only trips the 1h pair, which resets on its 5m window.
	r, ok = SeverityResetTime(workbookPage(), 0.999, 1.0, 90*time.Second)
	if !ok || r != 4*time.Minute+55680*time.Millisecond {
		t.Fatalf("reset = %v", r)
	}
	if _, ok := SeverityResetTime(workbookPage(), 0.999, 1.0, 30*time.Second); ok {
		t.Fatal("30s outage never pages")
	}
}

func TestPairDetectionUsesSlowerWindow(t *testing.T) {
	// A pathological pair whose short window is the slower one still reports
	// the larger of the two detection times.
	p := Pair{Long: time.Hour, Short: 5 * time.Minute, Burn: 14.4}
	d, _ := PairDetectionTime(p, 0.999, 1)
	long, _ := DetectionTime(time.Hour, 14.4, 0.999, 1)
	if d != long {
		t.Fatalf("pair %v long %v", d, long)
	}
}

func TestBudgetAndExhaustion(t *testing.T) {
	// The 1h/14.4 pair fires once 2% of the 30-day budget has burned,
	// whatever the error ratio: e * (1h*14.4*0.001/e) / (0.001*30d) = 0.02.
	for _, e := range []float64{1, 0.5, 0.05} {
		d, _ := DetectionTime(time.Hour, 14.4, 0.999, e)
		if got := BudgetConsumed(d, 30*day, 0.999, e); math.Abs(got-0.02) > 1e-6 {
			t.Errorf("e=%v consumed %v", e, got)
		}
	}
	d, ok := TimeToExhaustion(0.5, 2, 30*day)
	if !ok || d != 180*time.Hour {
		t.Fatalf("exhaustion %v", d)
	}
	if d, ok := TimeToExhaustion(-0.1, 2, 30*day); !ok || d != 0 {
		t.Fatal("exhausted budget")
	}
	if _, ok := TimeToExhaustion(0.5, 0, 30*day); ok {
		t.Fatal("no burn, no exhaustion")
	}
	if _, ok := TimeToExhaustion(0.5, math.NaN(), 30*day); ok {
		t.Fatal("NaN burn")
	}
}
