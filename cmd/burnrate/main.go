// Command burnrate is an SLO engine: it ingests OTLP/HTTP metrics, evaluates
// multiwindow multi-burn-rate alerts and error budgets, and generates the
// equivalent Prometheus rules.
package main

import (
	"context"
	"os"

	"github.com/lutherleo/burnrate/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
