package sim

import (
	"fmt"
	"sort"
	"time"
)

const (
	day  = 24 * time.Hour
	hour = time.Hour
)

// slowBucket is the (0.75s, 1s] bucket of DefaultBounds: well above the
// 250 ms latency objective of the example SLOs.
const slowBucket = 9

// baseServices are the two services of the example SLOs. checkout runs at
// 0.04% errors (40% of its 99.9% budget); search at 0.1% (a fifth of its
// 99.5% budget).
func baseServices() []Service {
	return []Service{
		{Name: "checkout", Instance: "checkout-1", Method: "POST", Route: "/api/checkout", RequestsPerStep: 1200, ErrorRatio: 0.0004},
		{Name: "search", Instance: "search-1", Method: "GET", Route: "/api/search", RequestsPerStep: 3000, ErrorRatio: 0.001},
	}
}

// Every scenario runs three days of healthy traffic first so the longest
// alert window (3d) is full before anything happens.
var builtins = map[string]func() Scenario{
	"steady": func() Scenario {
		return Scenario{
			Name: "steady", Description: "Four days of healthy traffic; no alert should fire.",
			Duration: 4 * day,
		}
	},
	"outage": func() Scenario {
		return Scenario{
			Name: "outage", Description: "checkout returns 100% errors for 12 minutes on day 4.",
			Duration: 4 * day,
			Incidents: []Incident{
				{Service: "checkout", Name: "full-outage", Start: 3*day + 2*hour, Duration: 12 * time.Minute, ErrorRatio: 1},
			},
		}
	},
	"partial-outage": func() Scenario {
		return Scenario{
			Name: "partial-outage", Description: "checkout fails 6% of requests for 90 minutes.",
			Duration: 4 * day,
			Incidents: []Incident{
				{Service: "checkout", Name: "bad-shard", Start: 3*day + 8*hour, Duration: 90 * time.Minute, ErrorRatio: 0.06},
			},
		}
	},
	"slow-burn": func() Scenario {
		return Scenario{
			Name: "slow-burn", Description: "checkout fails 0.26% of requests for 34 hours: a ticket, never a page.",
			Duration: 5 * day,
			Incidents: []Incident{
				{Service: "checkout", Name: "leaky-retry", Start: 3 * day, Duration: 34 * hour, ErrorRatio: 0.0026},
			},
		}
	},
	"latency-regression": func() Scenario {
		return Scenario{
			Name: "latency-regression", Description: "15% of successful checkout requests take ~1s for 3 hours; availability is unaffected.",
			Duration: 4 * day,
			Incidents: []Incident{
				{Service: "checkout", Name: "slow-db", Start: 3*day + 6*hour, Duration: 3 * hour, SlowRatio: 0.15, SlowBucket: slowBucket},
			},
		}
	},
	"flapping": func() Scenario {
		var in []Incident
		for i := 0; i < 6; i++ {
			in = append(in, Incident{
				Service: "checkout", Name: fmt.Sprintf("blip-%d", i+1),
				Start: 3*day + 10*hour + time.Duration(i)*20*time.Minute, Duration: 2 * time.Minute, ErrorRatio: 1,
			})
		}
		return Scenario{
			Name: "flapping", Description: "Six 2-minute checkout outages, 20 minutes apart.",
			Duration: 4 * day, Incidents: in,
		}
	},
	"search-brownout": func() Scenario {
		return Scenario{
			Name: "search-brownout", Description: "search fails 8% of requests for an hour (7-day SLO, custom burn rates).",
			Duration: 4 * day,
			Incidents: []Incident{
				{Service: "search", Name: "index-rebuild", Start: 3*day + 16*hour, Duration: hour, ErrorRatio: 0.08},
			},
		}
	},
	"incident-day": func() Scenario {
		return Scenario{
			Name: "incident-day", Description: "A bad day: a 12m checkout outage, a 3h latency regression and a 1h search brownout, with 3% volume noise.",
			Duration: 4 * day, Noise: 0.03, Seed: 42,
			Incidents: []Incident{
				{Service: "checkout", Name: "full-outage", Start: 3*day + 2*hour, Duration: 12 * time.Minute, ErrorRatio: 1},
				{Service: "checkout", Name: "slow-db", Start: 3*day + 6*hour, Duration: 3 * hour, SlowRatio: 0.15, SlowBucket: slowBucket},
				{Service: "search", Name: "index-rebuild", Start: 3*day + 16*hour, Duration: hour, ErrorRatio: 0.08},
			},
		}
	},
}

// Names lists the built-in scenarios, sorted.
func Names() []string {
	out := make([]string, 0, len(builtins))
	for k := range builtins {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Lookup returns a fresh copy of a built-in scenario with the base services
// filled in.
func Lookup(name string) (Scenario, error) {
	f, ok := builtins[name]
	if !ok {
		return Scenario{}, fmt.Errorf("unknown scenario %q (available: %v)", name, Names())
	}
	sc := f()
	sc.Services = baseServices()
	sc.Step = time.Minute
	return sc, nil
}
