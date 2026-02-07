#!/bin/sh
# Validate the generated rules with promtool: `check rules` on the rule file
# and `test rules` on its unit tests. Uses $PROMTOOL, then bin/promtool
# (built by `make promtool`), then promtool on PATH.
#
# Without promtool this exits 0 with a notice: the same validation runs in
# `make check` through Prometheus' own rulefmt and PromQL parser
# (internal/rules TestPrometheusAcceptsRules) and its rule manager
# (TestGeneratedRulesMatchEngine).
set -eu
dir=${1:-examples/prometheus}
promtool=${PROMTOOL:-}
if [ -z "$promtool" ] && [ -x bin/promtool ]; then promtool=bin/promtool; fi
if [ -z "$promtool" ]; then promtool=$(command -v promtool || true); fi
if [ -z "$promtool" ]; then
  echo "promtool-check: promtool not found; skipped (run 'make promtool' to build it; rulefmt validation already runs in 'make check')"
  exit 0
fi
"$promtool" check rules "$dir/slo-rules.yaml"
"$promtool" test rules "$dir/slo-rules.test.yaml"
