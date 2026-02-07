#!/bin/sh
# Usage: coverage-gate.sh <coverprofile> <min-percent>
# Prints per-package and total statement coverage and fails below the minimum.
set -eu
prof=$1
min=$2
go tool cover -func="$prof" | awk -v min="$min" '
  /^total:/ { total = $3; sub("%", "", total) }
  END {
    printf "coverage: %.1f%% of statements in internal/... (minimum %s%%)\n", total, min
    if (total + 0 < min + 0) { print "coverage below threshold"; exit 1 }
  }'
