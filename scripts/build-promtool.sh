#!/bin/sh
# Build promtool from the Prometheus module this repo already depends on, at
# the exact pinned version, into bin/promtool. `go install ...@version` is
# refused for Prometheus (its go.mod has replace directives), so the build
# runs inside this module with -mod=mod; go.sum is restored afterwards so the
# working tree stays clean. Needs network on first run (module downloads).
set -eu
backup=$(mktemp)
cp go.sum "$backup"
trap 'cp "$backup" go.sum; rm -f "$backup"' EXIT
mkdir -p bin
go build -mod=mod -o bin/promtool github.com/prometheus/prometheus/cmd/promtool
echo "built bin/promtool (prometheus $(go list -m -f '{{.Version}}' github.com/prometheus/prometheus))"
