# burnrate — every gate runs offline; no Docker or network needed.
GO        ?= go
GOFLAGS   ?= -p=3
export GOFLAGS
BIN       := bin/burnrate
COVER_MIN ?= 85
COVERPROF := coverage.out

.PHONY: check fmt fmt-check lint vet build test cover bench demo rules rules-check promtool promtool-check clean help

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*## "}{printf "  %-15s %s\n",$$1,$$2}'

check: fmt-check vet lint cover rules-check ## The one gate: format, vet, lint, tests + coverage, generated-rules drift
	@echo "make check: all gates passed"

fmt: ## Rewrite files with gofmt + goimports
	golangci-lint fmt ./...

fmt-check: ## Fail if any file needs formatting
	@out="$$(golangci-lint fmt --diff ./...)"; if [ -n "$$out" ]; then echo "$$out"; echo "run 'make fmt'"; exit 1; fi
	@echo "fmt-check: ok"

vet: ## go vet (type-check + static checks)
	$(GO) vet ./...

lint: ## golangci-lint, zero issues allowed
	golangci-lint run ./...

build: ## Build bin/burnrate
	$(GO) build -trimpath -ldflags "-X github.com/lutherleo/burnrate/internal/cli.Version=$$(git describe --always --dirty 2>/dev/null || echo dev)" -o $(BIN) ./cmd/burnrate

test: ## Run all tests
	$(GO) test ./...

cover: ## Run all tests; enforce COVER_MIN% (each package measured by its own tests only)
	$(GO) test -coverprofile=$(COVERPROF) ./internal/...
	@scripts/coverage-gate.sh $(COVERPROF) $(COVER_MIN)

rules-check: build ## Regenerate example rules and fail on drift from the committed file
	@$(BIN) rules --slos examples/slos/checkout.yaml --slos examples/slos/search.yaml | diff -u examples/prometheus/slo-rules.yaml - \
		&& echo "rules-check: examples/prometheus/slo-rules.yaml is up to date"

rules: build ## Regenerate examples/prometheus/slo-rules.yaml
	$(BIN) rules --slos examples/slos/checkout.yaml --slos examples/slos/search.yaml -o examples/prometheus/slo-rules.yaml

promtool: ## Build bin/promtool from the pinned Prometheus module (needs network once)
	@scripts/build-promtool.sh

promtool-check: ## promtool check rules + test rules on the example output (skips if no promtool)
	@scripts/promtool-check.sh examples/prometheus

bench: ## Benchmarks: end-to-end replay and window sums
	$(GO) test -run '^$$' -bench . -benchtime 5x ./internal/sim/
	$(GO) test -run '^$$' -bench . ./internal/window/

demo: build ## Offline end-to-end demo against the bundled SLOs and scenarios
	@scripts/demo.sh $(BIN)

clean: ## Remove build and coverage output
	rm -rf bin $(COVERPROF)
