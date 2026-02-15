# Alert math

This is the arithmetic behind burnrate's alerts and error budgets: where the
burn-rate thresholds come from, when an alert fires, when it clears, and how
the error budget is counted. It follows chapter 5, "Alerting on SLOs", of the
Google SRE workbook (*The Site Reliability Workbook*, O'Reilly 2018). The
closed forms live in `internal/burnmath`; `burnrate math` prints them for any
SLO, and the tests check that the engine and Prometheus fire at these times.

## Notation

| Symbol | Meaning | Example |
|---|---|---|
| `O` | objective (target ratio of good events) | 0.999 |
| `1 − O` | error budget, as a ratio of events | 0.001 |
| `P` | SLO period (rolling) | 30d |
| `e` | error ratio of the traffic: bad / total | 0.02 |
| `b = e / (1 − O)` | burn rate: how many times faster than "exactly on budget" | 20 |
| `W`, `S` | long and short window of an alert pair | 1h, 5m |
| `B` | burn-rate threshold of a pair | 14.4 |

A burn rate of 1 spends the whole budget in exactly one period. A burn rate of
`b` spends it in `P / b`.

## 1. Burn-rate thresholds from budget fractions

Burning at rate `b` for a window `W` consumes `b · W / P` of the period's budget.
The workbook picks, for each alert, the share `f` of the budget that may be
spent before someone is told, and solves for the threshold:

```
B = f · P / W
```

For a 30-day period this gives the workbook's recommended table, which is also
burnrate's default policy (`slo.DefaultWindows`):

| Severity | Long `W` | Short `S = W/12` | Budget spent `f` | `B = f·P/W` | Error-ratio threshold at 99.9% |
|---|---|---|---|---|---|
| page | 1h | 5m | 2% | 0.02 · 720h / 1h = **14.4** | 1.44% |
| page | 6h | 30m | 5% | 0.05 · 720h / 6h = **6** | 0.6% |
| ticket | 1d | 2h | 10% | 0.10 · 30d / 1d = **3** | 0.3% |
| ticket | 3d | 6h | 10% | 0.10 · 30d / 3d = **1** | 0.1% |

The defaults are stored as budget fractions (`budgetConsumed`), not burn rates, so the same
policy scales to other periods: with a 7-day period, 2% of the budget in 1h is
a 3.36x threshold. A window pair may give either `budgetConsumed` or an explicit `burnRate`
(see `examples/slos/search.yaml`). Pinned by
`internal/burnmath TestWorkbookBurnRates`.

## 2. Multiwindow, multi-burn-rate

A pair fires when the error ratio over **both** windows exceeds the threshold:

```
ratio(W) > B · (1 − O)   and   ratio(S) > B · (1 − O)
```

and a severity fires when **any** of its pairs fires (the pairs are OR-ed).
The long window gives precision: a short spike cannot spend `f` of the budget,
so it cannot fire the alert. The short window gives a fast reset: once the
incident is over, the 5-minute ratio drops back within minutes, while the
1-hour ratio would keep the page open for most of an hour.

In the generated Prometheus rules this is literally:

```promql
(
  slo:sli_error:ratio_rate1h{slo="checkout-availability"} > (14.4 * (1 - 0.999))
and
  slo:sli_error:ratio_rate5m{slo="checkout-availability"} > (14.4 * (1 - 0.999))
)
or
( ...6h / 30m at 6x... )
```

## 3. Detection time

Start from a clean history and let the error ratio jump to a constant `e`.
After `t ≤ W` of incident, the ratio over the long window is `e · t / W`
(assuming steady traffic volume). It crosses the threshold when

```
t_detect(W) = W · B · (1 − O) / e          (only if e > B · (1 − O))
```

which is the workbook's "detection time = ((1 − SLO) / error ratio) × alerting
window size × burn rate". The short window crosses at `S · B(1−O)/e`, which is
earlier because `S < W`, so a pair is detected at `t_detect(W)` and a severity
at the minimum over its pairs.

If `e ≤ B · (1 − O)` the pair never fires, however long the incident lasts.
That is a design property, not a gap: an incident burning at less than `B`
does not spend `f` of the budget in `W`, and slower pairs exist to catch it.

### Worked table: `burnrate math --slos examples/slos --slo checkout-availability`

Real output for the 99.9%, 30-day SLO with the default policy:

```
ERROR RATIO  BURN    PAGE DETECT  PAGE RESET  TICKET DETECT  TICKET RESET  BUDGET GONE IN
100.0%       1000x   51.8s        29.8m       4.3m           6.0h          43.2m
50.0%        500x    1.7m         29.6m       8.6m           6.0h          1.4h
10.0%        100.0x  8.6m         28.2m       43.2m          5.9h          7.2h
5.0%         50.0x   17.3m        26.4m       1.4h           5.9h          14.4h
2.0%         20.0x   43.2m        21.0m       3.6h           5.7h          36.0h
1.0%         10.00x  3.6h         12.0m       7.2h           5.4h          3.0d
0.5%         5.00x   never        never       14.4h          4.8h          6.0d
0.2%         2.00x   never        never       36.0h          3.0h          15.0d
0.1%         1.50x   never        never       2.0d           2.0h          20.0d
```

Some rows checked by hand:

* **100% outage, page.** `1h · 14.4 · 0.001 / 1 = 0.864 min = 51.8 s`, the
  workbook's headline number. The budget would be gone in `30d / 1000 = 43.2 min`.
* **2% errors, page.** The 1h/5m pair needs `1h · 0.0144 / 0.02 = 43.2 min`;
  the 6h/30m pair needs `6h · 0.006 / 0.02 = 1.8h`; the page fires at 43.2 min.
* **1% errors, page.** Burn 10 is under 14.4, so only 6h/30m can fire:
  `6h · 0.006 / 0.01 = 3.6h`.
* **0.5% errors.** Burn 5 is under both page thresholds: no page, ever. The
  1d/2h ticket fires after `1d · 0.003 / 0.005 = 14.4h`.

(The last row uses `e = 0.15%`, i.e. burn 1.5, so that the 3d/6h ticket can
fire; at exactly 0.1% the burn is 1 and the strict `>` never trips.)

## 4. Reset time

After an incident that lasted at least `S` ends, the short-window ratio is
`e · (S − x) / S` at `x` after the end. It falls back to the threshold at

```
t_reset(S) = S · (1 − B · (1 − O) / e)
```

and the pair stops firing then, because the `and` needs both windows. The
same formula holds for incidents shorter than `S`, because a pair that fired
had `D > S · B(1−O)/e`. A severity clears when every pair that was firing has
reset, so its reset time is the maximum over those pairs. That is why the
page for a 100% outage stays open for ~30 min (the 30m short window of the
6h/30m pair, which also tripped) and the ticket for ~6h (the 6h short window
of the 3d/6h pair). `burnmath.SeverityResetTime` implements this.

## 5. Discretisation

The engine evaluates on a fixed step (the `--resolution`, default 1 minute)
over right-closed buckets: bucket `k` holds events in `(k·res, (k+1)·res]`, so
an evaluation at `T` sees every sample stamped at or before `T`. Therefore:

* an alert **fires** at the first step strictly after onset + `t_detect`
  (the ratio must be strictly above the threshold), and
* it **clears** at the first step at or after end + `t_reset`.

`internal/engine TestWorkbookDetectionAndResetTimes` feeds counter samples
for seven incident shapes (100% for 10m and 2m, 5% for 2h, 2.5% for 4h, 0.7%
for 12h, 0.21% for 36h, 0.09% for 6h) after three days of clean history and
asserts every firing and resolving transition equals the closed form rounded
this way, to the minute. Two headline values are pinned literally in
`TestWorkbookHeadlineNumbers`: a 10-minute total outage pages at +1m,
resolves at +40m (10m + the 30m window), tickets at +5m
(`1d · 3 · 0.001 = 4.32m`) and resolves at +6h10m.

## 6. Error budget accounting

Over the rolling period window:

```
allowed   = (1 − O) · total_P
consumed  = bad_P / allowed
remaining = 1 − consumed            (negative when overspent)
```

The recording rule is the ratio form of the same quantity:
`slo:error_budget_remaining:ratio = 1 − slo:sli_error:ratio_rate30d / (1 − O)`.

Both count the events **observed in the window**. Until a full period of data
exists, the denominator is smaller than a steady-state 30 days, so a single
incident costs a larger share of the budget than it will once the window is
full. The demo shows this: a 12-minute total outage inside four days of data
leaves checkout-availability between −145% and −148% (the demo prints both runs), whereas in a full 30-day window it would cost
`12 / 43.2 ≈ 28%`. Prometheus behaves identically for `rate(x[30d])` over a
young TSDB, which is what the equivalence test checks.

**Policy states.** Each SLO has ordered states with a `minRemaining` floor;
the first state whose floor `remaining` meets is current, `remaining ≤ 0` is
always `exhausted`, and no traffic in the period is `no-data`. The default
policy is `healthy ≥ 50%`, `warning ≥ 25%`, `critical > 0`, `exhausted`.
The generated rules publish the state as
`slo:error_budget_policy_state{state="..."} == 1`.

**Exhaustion ETA.** At the current burn rate of the shortest alerting long
window (1h for the defaults), the remaining budget lasts
`P · remaining / b`, reported as `exhaustionEta` in the API.

## 7. SLI definitions

* **Ratio (availability):** `e = bad / total`, or `e = 1 − good / total` when
  the SLO gives good events. Good-event ratios are clamped at 0 because good
  and total arrive as separate series and can be momentarily out of step.
* **Latency threshold ratio:** good events are the histogram bucket
  `le="<threshold>"`, total is the histogram `_count`; `e = 1 − good/total`.
  The threshold must be an existing bucket bound (the OpenTelemetry default
  bounds include 0.25 s), because neither the engine nor PromQL can count
  events under an arbitrary threshold exactly from bucketed data. A threshold
  that is not a bound selects no good series, so the SLI reads 100% errors.

Counter increases follow Prometheus semantics: per-series differences, a
decrease is a reset, and a series' first sample is only a baseline unless the
stream provably started from zero (OTLP start time after the engine began
observing, or the same producer already exported without that series). This
matters most for error series, which SDKs create lazily on the first error.
A series that returns after being evicted for inactivity, with the same
producer and start time, is never treated as new: its first value back is a
baseline, as `rate()` sees no increase across the gap in short windows.

## 8. Engine vs. generated rules

The engine counts exact events per bucket; Prometheus' `rate()` extrapolates
to the window edges and ignores the first sample of a lazily created series.
`internal/rules TestGeneratedRulesMatchEngine` posts the same OTLP protobuf
requests to burnrate's receiver and to Prometheus' own OTLP write handler
(backed by a real TSDB), evaluates the generated rules with Prometheus' rule
manager every minute, and requires the same alerts to fire and resolve, each
transition within ±2 minutes, the same final budget policy state, and the same
1-hour error ratio, for six scenarios.
