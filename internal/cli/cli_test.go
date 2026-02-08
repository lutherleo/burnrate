package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lutherleo/burnrate/internal/server"
	"github.com/lutherleo/burnrate/internal/slo"
)

const slosDir = "../../examples/slos"

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestTopLevel(t *testing.T) {
	if code, _, errs := run(t); code != 2 || !strings.Contains(errs, "Commands:") {
		t.Fatalf("no args: %d %s", code, errs)
	}
	if code, out, _ := run(t, "help"); code != 0 || !strings.Contains(out, "simulate") {
		t.Fatal("help")
	}
	if code, out, _ := run(t, "version"); code != 0 || !strings.HasPrefix(out, "burnrate ") {
		t.Fatal("version")
	}
	if code, _, errs := run(t, "bogus"); code != 2 || !strings.Contains(errs, "unknown command") {
		t.Fatal("unknown command")
	}
	if code, _, _ := run(t, "validate", "-h"); code != 0 {
		t.Fatal("-h should exit 0")
	}
	if code, _, errs := run(t, "validate"); code != 1 || !strings.Contains(errs, "no SLO files given") {
		t.Fatalf("missing slos: %s", errs)
	}
	if code, _, _ := run(t, "validate", "--nope"); code != 1 {
		t.Fatal("bad flag")
	}
}

func TestValidate(t *testing.T) {
	code, out, errs := run(t, "validate", "--slos", slosDir)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	for _, want := range []string{"3 SLO(s) valid", "checkout-latency", "burn 14.4x", "good:  http_server_request_duration_seconds_bucket"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("apiVersion: burnrate/v1\nkind: SLO\nmetadata: {name: x}\nspec: {service: s}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, "validate", "--slos", bad); code != 1 || !strings.Contains(errs, "spec.timeWindow") {
		t.Fatalf("invalid file: %d %s", code, errs)
	}
	disabled := filepath.Join(dir, "off.yaml")
	doc := `apiVersion: burnrate/v1
kind: SLO
metadata: {name: off}
spec:
  service: s
  timeWindow: [{duration: 1d}]
  objectives: [{target: 0.99}]
  indicator: {ratio: {total: t_total, bad: b_total}}
  alerting: {disabled: true}
`
	if err := os.WriteFile(disabled, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, "validate", "--slos", disabled); code != 0 || !strings.Contains(out, "alerting disabled") {
		t.Fatalf("disabled: %s", out)
	}
}

func TestRules(t *testing.T) {
	code, out, _ := run(t, "rules", "--slos", slosDir)
	golden, err := os.ReadFile("../../examples/prometheus/slo-rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The header names the source paths, which differ by working directory.
	body := func(s string) string { return s[strings.Index(s, "groups:"):] }
	if code != 0 || body(out) != body(string(golden)) {
		t.Fatalf("rules output differs from the golden file")
	}
	path := filepath.Join(t.TempDir(), "rules.yaml")
	code, _, errs := run(t, "rules", "--slos", slosDir, "-o", path, "--interval", "30s")
	if code != 0 || !strings.Contains(errs, "wrote 3 rule groups") {
		t.Fatalf("%d %s", code, errs)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "interval: 30s") {
		t.Fatal("interval missing")
	}
	if code, _, _ := run(t, "rules", "--slos", slosDir, "-o", filepath.Join(t.TempDir(), "missing", "x.yaml")); code != 1 {
		t.Fatal("unwritable output")
	}
}

func TestMath(t *testing.T) {
	code, out, _ := run(t, "math", "--slos", slosDir, "--slo", "checkout-availability")
	if code != 0 {
		t.Fatal(out)
	}
	for _, want := range []string{"objective 99.9% over 30d", "14.4", "51.8s", "PAGE DETECT", "never"} {
		if !strings.Contains(out, want) {
			t.Fatalf("math output lacks %q:\n%s", want, out)
		}
	}
	if code, out, _ := run(t, "math", "--slos", slosDir); code != 0 || strings.Count(out, "SLO ") != 3 {
		t.Fatalf("all SLOs: %s", out)
	}
	if code, _, errs := run(t, "math", "--slos", slosDir, "--slo", "nope"); code != 1 || !strings.Contains(errs, "no SLO named") {
		t.Fatal("unknown slo")
	}
}

func TestScenariosAndSimulate(t *testing.T) {
	code, out, _ := run(t, "scenarios")
	if code != 0 || strings.Count(out, "\n") != 8 || !strings.Contains(out, "slow-burn") {
		t.Fatalf("scenarios:\n%s", out)
	}
	code, out, errs := run(t, "simulate", "--slos", slosDir, "--scenario", "outage")
	if code != 0 {
		t.Fatal(errs)
	}
	for _, want := range []string{"Scenario outage", "+3d02h00m  full-outage", "FIRING page", "resolved ticket", "Final status"} {
		if !strings.Contains(out, want) {
			t.Fatalf("simulate output lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = run(t, "simulate", "--slos", slosDir, "--scenario", "latency-regression", "--json")
	var parsed struct {
		Scenario string
		Events   []map[string]any
		Status   []map[string]any
	}
	if code != 0 || json.Unmarshal([]byte(out), &parsed) != nil || parsed.Scenario != "latency-regression" || len(parsed.Status) != 3 {
		t.Fatalf("json output: %.300s", out)
	}
	if code, _, errs := run(t, "simulate", "--slos", slosDir, "--scenario", "nope"); code != 1 || !strings.Contains(errs, "unknown scenario") {
		t.Fatal("unknown scenario")
	}
	if code, _, _ := run(t, "simulate", "--slos", slosDir, "--resolution", "7m"); code != 1 {
		t.Fatal("bad resolution")
	}
}

func TestLoadgenAgainstServer(t *testing.T) {
	defs, err := slo.LoadPaths(slosDir)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{Clock: server.DataClock}, defs)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	code, out, errs := run(t, "loadgen", "--endpoint", hs.URL, "--scenario", "search-brownout", "--start", "2026-01-05T00:00:00Z")
	if code != 0 || !strings.Contains(out, "sent 5760 OTLP exports") || !strings.Contains(out, "0 points rejected") {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	fired := false
	for _, ev := range srv.Engine().Events() {
		if ev.SLO == "search-availability" && ev.Kind == "alert_firing" {
			fired = true
		}
	}
	if !fired {
		t.Fatal("search page did not fire after loadgen")
	}
	if code, _, _ := run(t, "loadgen", "--endpoint", "http://127.0.0.1:1", "--scenario", "steady"); code != 1 {
		t.Fatal("unreachable endpoint must fail")
	}
	if code, _, _ := run(t, "loadgen", "--start", "yesterday"); code != 1 {
		t.Fatal("bad start")
	}
	if code, _, _ := run(t, "loadgen", "--scenario", "nope"); code != 1 {
		t.Fatal("bad scenario")
	}
}

func TestServeStartsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out, errb bytes.Buffer
	done := make(chan int, 1)
	eventsLog := filepath.Join(t.TempDir(), "events.jsonl")
	go func() {
		done <- Run(ctx, []string{"serve", "--slos", slosDir, "--listen", "127.0.0.1:0", "--log-format", "json",
			"--events-log", eventsLog, "--alertmanager-url", "http://127.0.0.1:1", "--promote-resource-attributes", "deployment.environment"}, &out, &errb)
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exited %d: %s", code, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	if !strings.Contains(errb.String(), `"msg":"burnrate listening"`) {
		t.Fatalf("log: %s", errb.String())
	}
	for _, args := range [][]string{
		{"serve", "--slos", slosDir, "--log-level", "loud"},
		{"serve", "--slos", slosDir, "--log-format", "xml"},
		{"serve", "--slos", slosDir, "--clock", "lunar", "--listen", "127.0.0.1:0"},
		{"serve", "--slos", slosDir, "--listen", "256.0.0.1:99999"},
		{"serve", "--slos", slosDir, "--events-log", filepath.Join(t.TempDir(), "no", "such", "dir")},
		{"serve"},
	} {
		if code, _, _ := run(t, args...); code != 1 {
			t.Errorf("%v: expected failure", args)
		}
	}
}

func TestStringList(t *testing.T) {
	var s stringList
	_ = s.Set("a, b,,c")
	_ = s.Set("d")
	if s.String() != "a,b,c,d" {
		t.Fatalf("%q", s.String())
	}
}
