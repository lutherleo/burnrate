// Package cli implements the burnrate command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lutherleo/burnrate/internal/engine"
	"github.com/lutherleo/burnrate/internal/notify"
	"github.com/lutherleo/burnrate/internal/otlp"
	"github.com/lutherleo/burnrate/internal/report"
	"github.com/lutherleo/burnrate/internal/rules"
	"github.com/lutherleo/burnrate/internal/server"
	"github.com/lutherleo/burnrate/internal/sim"
	"github.com/lutherleo/burnrate/internal/slo"
)

// Version is set at build time with -ldflags "-X .../cli.Version=...".
var Version = "dev"

const usage = `burnrate: SLO engine with multiwindow, multi-burn-rate alerting.

Usage:
  burnrate <command> [flags]

Commands:
  serve      Run the OTLP/HTTP receiver, engine and HTTP API
  validate   Check SLO files and print what they resolve to
  rules      Generate Prometheus recording and alerting rules
  math       Tabulate detection and reset times of an SLO's alert policy
  simulate   Replay a synthetic scenario through the engine in-process
  loadgen    Push a synthetic scenario to an OTLP/HTTP endpoint
  scenarios  List the built-in traffic scenarios
  version    Print the version

Run "burnrate <command> -h" for the flags of a command.
`

// Run executes the CLI and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmds := map[string]func(context.Context, []string, io.Writer, io.Writer) error{
		"serve": runServe, "validate": runValidate, "rules": runRules, "math": runMath,
		"simulate": runSimulate, "loadgen": runLoadgen, "scenarios": runScenarios,
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, "burnrate", Version)
		return 0
	}
	cmd, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "burnrate: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if err := cmd(ctx, args[1:], stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "burnrate:", err)
		return 1
	}
	return 0
}

// stringList is a repeatable, comma-splitting flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func loadSLOs(paths stringList) ([]*slo.SLO, error) {
	if len(paths) == 0 {
		return nil, errors.New("no SLO files given; use --slos <file-or-dir>")
	}
	return slo.LoadPaths(paths...)
}

func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("invalid --log-format %q (text or json)", format)
}

func runServe(ctx context.Context, args []string, _, stderr io.Writer) error {
	fs := newFlags("serve", stderr)
	var slos, promote stringList
	fs.Var(&slos, "slos", "SLO file or directory (repeatable)")
	fs.Var(&promote, "promote-resource-attributes", "resource attributes to copy onto series (comma-separated)")
	listen := fs.String("listen", "127.0.0.1:4318", "address for OTLP/HTTP and the API")
	clock := fs.String("clock", "wall", "evaluation clock: wall (live) or data (follow sample timestamps, for replays)")
	resolution := fs.Duration("resolution", time.Minute, "bucket width and evaluation step")
	lateness := fs.Duration("lateness", 30*time.Second, "wall clock: how far behind now evaluation runs")
	seriesTTL := fs.Duration("series-ttl", time.Hour, "forget counter state of series not seen for this long")
	amURL := fs.String("alertmanager-url", "", "Alertmanager base URL to send alerts to")
	eventsLog := fs.String("events-log", "", "append events as JSON lines to this file ('-' for stderr)")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log, err := newLogger(stderr, *logLevel, *logFormat)
	if err != nil {
		return err
	}
	defs, err := loadSLOs(slos)
	if err != nil {
		return err
	}
	var notifiers notify.Multi
	switch *eventsLog {
	case "":
	case "-":
		notifiers = append(notifiers, notify.NewJSONLines(stderr))
	default:
		f, err := os.OpenFile(*eventsLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := f.Close(); cerr != nil {
				log.Warn("closing events log", "err", cerr)
			}
		}()
		notifiers = append(notifiers, notify.NewJSONLines(f))
	}
	if *amURL != "" {
		notifiers = append(notifiers, &notify.Alertmanager{URL: *amURL, ResendInterval: *resolution, GeneratorURL: "http://" + *listen + "/"})
	}
	cfg := server.Config{
		Clock:       server.Clock(*clock),
		Lateness:    *lateness,
		Engine:      engine.Config{Resolution: *resolution, SeriesTTL: *seriesTTL},
		Translation: otlp.Options{PromoteResourceAttributes: promote},
		Logger:      log,
	}
	if len(notifiers) > 0 {
		cfg.Notifier = notifiers
	}
	srv, err := server.New(cfg, defs)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("loaded SLOs", "count", len(defs))
	return srv.Serve(ctx, ln)
}

func runValidate(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("validate", stderr)
	var slos stringList
	fs.Var(&slos, "slos", "SLO file or directory (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	defs, err := loadSLOs(slos)
	if err != nil {
		return err
	}
	for _, s := range defs {
		fmt.Fprintf(stdout, "%s (%s): %s %s over %s, service %s\n", s.Name, s.Source, s.Indicator.Kind,
			slo.FormatPercent(s.Objective), slo.FormatDuration(s.Period), s.Service)
		fmt.Fprintf(stdout, "  total: %s\n", s.Indicator.Total)
		if s.Indicator.Bad != nil {
			fmt.Fprintf(stdout, "  bad:   %s\n", s.Indicator.Bad)
		} else {
			fmt.Fprintf(stdout, "  good:  %s\n", s.Indicator.Good)
		}
		if !s.Alerting.Enabled {
			fmt.Fprintln(stdout, "  alerting disabled")
		}
		for _, sev := range s.Alerting.Severities {
			for _, w := range sev.Windows {
				fmt.Fprintf(stdout, "  %-7s %s/%s burn %.4gx (error ratio > %.4g, %.1f%% of budget)\n", sev.Name,
					slo.FormatDuration(w.Long), slo.FormatDuration(w.Short), w.BurnRate, w.Threshold(s.Objective), w.BudgetConsumed*100)
			}
		}
	}
	fmt.Fprintf(stdout, "%d SLO(s) valid\n", len(defs))
	return nil
}

func runRules(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("rules", stderr)
	var slos stringList
	fs.Var(&slos, "slos", "SLO file or directory (repeatable)")
	out := fs.String("o", "-", "output file ('-' for stdout)")
	interval := fs.Duration("interval", 0, "rule group evaluation interval (0 = Prometheus global default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	defs, err := loadSLOs(slos)
	if err != nil {
		return err
	}
	f, err := rules.Generate(defs, rules.Options{Interval: *interval})
	if err != nil {
		return err
	}
	sources := make([]string, len(defs))
	for i, s := range defs {
		sources[i] = s.Source
	}
	b, err := f.Marshal(rules.Header(sources))
	if err != nil {
		return err
	}
	if *out == "-" {
		_, err = stdout.Write(b)
		return err
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "wrote %d rule groups to %s\n", len(f.Groups), *out)
	return nil
}

func runMath(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("math", stderr)
	var slos stringList
	fs.Var(&slos, "slos", "SLO file or directory (repeatable)")
	only := fs.String("slo", "", "only this SLO")
	if err := fs.Parse(args); err != nil {
		return err
	}
	defs, err := loadSLOs(slos)
	if err != nil {
		return err
	}
	found := false
	for _, s := range defs {
		if *only != "" && s.Name != *only {
			continue
		}
		if found {
			fmt.Fprintln(stdout)
		}
		found = true
		if err := report.Math(stdout, s, report.DefaultRatios); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("no SLO named %q", *only)
	}
	return nil
}

func runScenarios(_ context.Context, _ []string, stdout, _ io.Writer) error {
	for _, n := range sim.Names() {
		sc, err := sim.Lookup(n)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%-20s %s\n", n, sc.Description)
	}
	return nil
}

func runSimulate(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("simulate", stderr)
	var slos stringList
	fs.Var(&slos, "slos", "SLO file or directory (repeatable)")
	scenario := fs.String("scenario", "incident-day", "built-in scenario (see 'burnrate scenarios')")
	asJSON := fs.Bool("json", false, "print events and final status as JSON")
	resolution := fs.Duration("resolution", time.Minute, "bucket width and evaluation step")
	if err := fs.Parse(args); err != nil {
		return err
	}
	defs, err := loadSLOs(slos)
	if err != nil {
		return err
	}
	sc, err := sim.Lookup(*scenario)
	if err != nil {
		return err
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	gen, err := sim.NewGenerator(sc, start)
	if err != nil {
		return err
	}
	eng, err := engine.New(engine.Config{Resolution: *resolution, EventHistory: 1 << 20}, defs)
	if err != nil {
		return err
	}
	tr, err := otlp.NewTranslator(otlp.Options{}, eng.Wants)
	if err != nil {
		return err
	}
	began := time.Now()
	var events []engine.Event
	for {
		req, at, ok := gen.Next()
		if !ok {
			break
		}
		samples, res := tr.Translate(req)
		if res.Rejected > 0 {
			return fmt.Errorf("generator produced invalid points: %v", res.Errors)
		}
		eng.Ingest(samples)
		events = append(events, eng.Advance(at)...)
	}
	elapsed := time.Since(began)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"scenario": sc.Name, "start": start, "events": events, "status": eng.Status()})
	}
	fmt.Fprintf(stdout, "Scenario %s: %s\n", sc.Name, sc.Description)
	fmt.Fprintf(stdout, "Replayed %s of traffic (%d exports, %d evaluation steps) in %s.\n\n",
		slo.FormatDuration(sc.Duration), gen.Steps(), eng.Stats().Evaluations, elapsed.Round(time.Millisecond))
	if len(sc.Incidents) > 0 {
		fmt.Fprintln(stdout, "Injected incidents:")
		for _, in := range sc.Incidents {
			what := fmt.Sprintf("%.4g%% errors", in.ErrorRatio*100)
			if in.SlowRatio > 0 {
				what = fmt.Sprintf("%.4g%% of successes slow", in.SlowRatio*100)
			}
			fmt.Fprintf(stdout, "  %s  %-14s %-9s %s for %s\n", report.Offset(in.Start), in.Name, in.Service, what, slo.FormatDuration(in.Duration))
		}
		fmt.Fprintln(stdout)
	}
	fmt.Fprintln(stdout, "Events (alerts and budget policy changes):")
	if err := report.Events(stdout, events, start); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nFinal status at %s:\n", report.Offset(sc.Duration))
	return report.Status(stdout, eng.Status())
}

func runLoadgen(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("loadgen", stderr)
	endpoint := fs.String("endpoint", "http://127.0.0.1:4318", "OTLP/HTTP base URL")
	scenario := fs.String("scenario", "incident-day", "built-in scenario (see 'burnrate scenarios')")
	gzipOn := fs.Bool("gzip", true, "gzip request bodies")
	realtime := fs.Bool("realtime", false, "wait one step between exports and stamp them with the wall clock")
	startFlag := fs.String("start", "", "RFC3339 start time of the stream (default: so that it ends now)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sc, err := sim.Lookup(*scenario)
	if err != nil {
		return err
	}
	start := time.Now().Add(-sc.Duration).Truncate(sc.Step)
	if *realtime {
		start = time.Now().Truncate(sc.Step)
	}
	if *startFlag != "" {
		if start, err = time.Parse(time.RFC3339, *startFlag); err != nil {
			return fmt.Errorf("--start: %w", err)
		}
	}
	gen, err := sim.NewGenerator(sc, start)
	if err != nil {
		return err
	}
	client := &otlp.Client{Endpoint: *endpoint, Gzip: *gzipOn, MaxRetries: 3}
	began := time.Now()
	sent, rejected := 0, int64(0)
	for {
		req, at, ok := gen.Next()
		if !ok {
			break
		}
		if *realtime {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Until(at)):
			}
		}
		resp, err := client.Export(ctx, req)
		if err != nil {
			return fmt.Errorf("export %d: %w", sent+1, err)
		}
		rejected += resp.GetPartialSuccess().GetRejectedDataPoints()
		sent++
	}
	fmt.Fprintf(stdout, "sent %d OTLP exports (%s of %s traffic) to %s in %s; %d points rejected\n",
		sent, slo.FormatDuration(sc.Duration), sc.Name, *endpoint, time.Since(began).Round(time.Millisecond), rejected)
	return nil
}
