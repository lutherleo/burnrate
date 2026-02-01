// Package burnmath holds the closed-form arithmetic behind multiwindow,
// multi-burn-rate alerting as described in chapter 5 ("Alerting on SLOs") of
// the Google SRE workbook.
//
// Notation used throughout (see docs/ALERT_MATH.md for the derivations):
//
//	O   objective, e.g. 0.999          budget ratio  = 1 - O
//	P   SLO period, e.g. 30d           burn rate b   = error ratio / (1 - O)
//	W   long window                    S             = short window
//	B   burn-rate threshold of a window pair
//	e   constant error ratio of an incident that starts from a clean history
//
// A window pair fires when the error ratio over both W and S exceeds
// B * (1 - O). Starting from a clean history the ratio over W after t of
// incident is e * min(t, W) / W, which gives the detection time
//
//	t_detect = W * B * (1 - O) / e     (only when e > B * (1 - O))
//
// and after an incident that lasted at least S ends, the short window falls
// back under the threshold after
//
//	t_reset = S * (1 - B * (1 - O) / e).
package burnmath

import (
	"math"
	"time"
)

// BurnRate returns the burn-rate threshold that, sustained for window,
// consumes budgetFraction of the error budget of an SLO with the given period.
// The workbook's 14.4 is BurnRate(0.02, 30d, 1h).
func BurnRate(budgetFraction float64, period, window time.Duration) float64 {
	return budgetFraction * float64(period) / float64(window)
}

// BudgetFraction is the inverse of BurnRate: the share of the period's error
// budget consumed when burning at burn for window.
func BudgetFraction(burn float64, period, window time.Duration) float64 {
	return burn * float64(window) / float64(period)
}

// ErrorRatioThreshold converts a burn-rate threshold into the error ratio the
// recorded SLI is compared against.
func ErrorRatioThreshold(burn, objective float64) float64 {
	return burn * (1 - objective)
}

// DetectionTime returns how long a constant error ratio, starting from a clean
// history, takes to push the error ratio over window above the threshold of
// burn. ok is false when the ratio never exceeds the threshold.
func DetectionTime(window time.Duration, burn, objective, errorRatio float64) (d time.Duration, ok bool) {
	thr := ErrorRatioThreshold(burn, objective)
	if errorRatio <= thr || errorRatio <= 0 {
		return 0, false
	}
	return scale(window, thr/errorRatio), true
}

// ResetTime returns how long after the end of an incident (with constant error
// ratio, lasting at least window) the error ratio over window takes to fall
// back to or below the threshold of burn. ok is false when the incident never
// pushed the window above the threshold.
func ResetTime(window time.Duration, burn, objective, errorRatio float64) (d time.Duration, ok bool) {
	thr := ErrorRatioThreshold(burn, objective)
	if errorRatio <= thr || errorRatio <= 0 {
		return 0, false
	}
	return scale(window, 1-thr/errorRatio), true
}

// Pair is one long/short window pair with its burn-rate threshold.
type Pair struct {
	Long  time.Duration
	Short time.Duration
	Burn  float64
}

// PairDetectionTime is the detection time of a long/short pair: both windows
// must exceed the threshold, and the long window is always the slower one.
func PairDetectionTime(p Pair, objective, errorRatio float64) (time.Duration, bool) {
	long, ok := DetectionTime(p.Long, p.Burn, objective, errorRatio)
	if !ok {
		return 0, false
	}
	short, _ := DetectionTime(p.Short, p.Burn, objective, errorRatio)
	return max(long, short), true
}

// SeverityDetectionTime is the earliest detection time among the pairs that
// make up one alert (the pairs are OR-ed together). ok is false when no pair
// can fire for this error ratio.
func SeverityDetectionTime(pairs []Pair, objective, errorRatio float64) (time.Duration, bool) {
	best, found := time.Duration(math.MaxInt64), false
	for _, p := range pairs {
		if d, ok := PairDetectionTime(p, objective, errorRatio); ok && d < best {
			best, found = d, true
		}
	}
	return best, found
}

// SeverityResetTime returns how long after the end of an incident of the given
// duration the alert made of pairs stops firing. A pair resets as soon as its
// short window drops under the threshold; the alert resets when every pair
// that was firing at the end of the incident has reset. ok is false when the
// alert never fired during the incident.
func SeverityResetTime(pairs []Pair, objective, errorRatio float64, incident time.Duration) (time.Duration, bool) {
	var reset time.Duration
	fired := false
	for _, p := range pairs {
		d, ok := PairDetectionTime(p, objective, errorRatio)
		if !ok || d >= incident {
			continue
		}
		fired = true
		// The short window's ratio after x is e*min(D, S-x)/S, which drops
		// to the threshold at x = S*(1 - thr/e) whether or not the incident
		// (of duration D) outlasted S, because D > S*thr/e once it fired.
		short, _ := ResetTime(p.Short, p.Burn, objective, errorRatio)
		reset = max(reset, short)
	}
	return reset, fired
}

// BudgetConsumed returns the share of the period's error budget consumed by
// an incident with errorRatio that has lasted d.
func BudgetConsumed(d, period time.Duration, objective, errorRatio float64) float64 {
	return errorRatio * float64(d) / ((1 - objective) * float64(period))
}

// TimeToExhaustion returns how long the remaining budget fraction lasts at a
// sustained burn rate. ok is false when burn is not positive.
func TimeToExhaustion(remaining, burn float64, period time.Duration) (time.Duration, bool) {
	if burn <= 0 || math.IsNaN(burn) {
		return 0, false
	}
	if remaining <= 0 {
		return 0, true
	}
	return scale(period, remaining/burn), true
}

func scale(d time.Duration, f float64) time.Duration {
	return time.Duration(math.Round(float64(d) * f))
}
