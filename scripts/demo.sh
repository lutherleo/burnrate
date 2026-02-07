#!/bin/sh
# Offline end-to-end demo. Everything runs on 127.0.0.1 against the bundled
# example SLOs and the built-in synthetic scenarios; no network, no Docker.
set -eu
bin=${1:-bin/burnrate}
slos="--slos examples/slos"
work=$(mktemp -d)
srv_pid=""
cleanup() {
  if [ -n "$srv_pid" ]; then kill "$srv_pid" 2>/dev/null || true; wait "$srv_pid" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT INT TERM

step() { printf '\n==> %s\n' "$*"; }

step "1. Validate the SLO definitions"
$bin validate $slos

step "2. Alert math for checkout-availability (closed form, docs/ALERT_MATH.md)"
$bin math $slos --slo checkout-availability

step "3. Replay the 4-day incident-day scenario in process (OTLP protobuf -> translator -> engine)"
$bin simulate $slos --scenario incident-day

step "4. Over the wire: serve OTLP/HTTP on a loopback port, push the 'outage' scenario with loadgen"
$bin serve $slos --listen 127.0.0.1:0 --clock data --events-log "$work/events.jsonl" 2>"$work/serve.log" &
srv_pid=$!
# Port 0 lets the OS pick a free port; the startup log line names it.
addr=""
i=0
while [ -z "$addr" ] && [ $i -lt 100 ]; do
  addr=$(sed -n 's/.*msg="burnrate listening" addr=\([^ ]*\).*/\1/p' "$work/serve.log")
  [ -z "$addr" ] && sleep 0.1
  i=$((i + 1))
done
if [ -z "$addr" ]; then echo "server did not start:"; cat "$work/serve.log"; exit 1; fi
url="http://$addr"
echo "burnrate serve listening on $url (data clock: evaluation follows sample timestamps)"
$bin loadgen --endpoint "$url" --scenario outage --start 2026-01-01T00:00:00Z

step "5. GET / (status page after the replay)"
curl -fsS "$url/"

step "6. Alert transitions written to the JSON-lines event log"
grep -E '"kind":"alert_(firing|resolved)"' "$work/events.jsonl" \
  | sed -E 's/.*"time":"([^"]*)".*"kind":"([^"]*)".*"slo":"([^"]*)".*"severity":"([^"]*)".*/\1  \2  \3\/\4/'

step "7. GET /metrics (burnrate's own SLO gauges, Prometheus text format)"
curl -fsS "$url/metrics" | grep -E '^burnrate_(slo_error_budget_remaining|slo_budget_policy_state|engine_samples_total|engine_evaluations_total)'

step "8. Generate the equivalent Prometheus rules"
$bin rules $slos > "$work/slo-rules.yaml"
grep -cE '^ +- (record|alert):' "$work/slo-rules.yaml" | sed 's/^/rules generated: /'
grep -A12 'alert: ErrorBudgetBurn' "$work/slo-rules.yaml" | head -13

echo
echo "demo finished"
