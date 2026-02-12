package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lutherleo/burnrate/internal/engine"
)

var t0 = time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)

type amServer struct {
	mu      sync.Mutex
	batches [][]amAlert
	status  int
}

func (s *amServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path != "/api/v2/alerts" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var batch []amAlert
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.batches = append(s.batches, batch)
	if s.status != 0 {
		http.Error(w, "boom", s.status)
	}
}

func active() []engine.ActiveAlert {
	return []engine.ActiveAlert{{
		Name: "ErrorBudgetBurn", SLO: "api", Severity: "page", Since: t0,
		Labels: map[string]string{"slo": "api", "severity": "page"}, Annotations: map[string]string{"summary": "s"},
	}}
}

func TestAlertmanagerLifecycle(t *testing.T) {
	am := &amServer{}
	srv := httptest.NewServer(am)
	defer srv.Close()
	n := &Alertmanager{URL: srv.URL + "/", ResendInterval: time.Minute, GeneratorURL: "http://burnrate/"}
	ctx := context.Background()

	firing := []engine.Event{{Kind: engine.EventAlertFiring, Time: t0}}
	if err := n.Notify(ctx, t0, firing, active()); err != nil {
		t.Fatal(err)
	}
	// Nothing changed and the resend interval has not passed: no request.
	if err := n.Notify(ctx, t0.Add(30*time.Second), nil, active()); err != nil {
		t.Fatal(err)
	}
	// Resend after the interval.
	if err := n.Notify(ctx, t0.Add(time.Minute), nil, active()); err != nil {
		t.Fatal(err)
	}
	resolved := []engine.Event{{
		Kind: engine.EventAlertResolved, Time: t0.Add(5 * time.Minute), AlertName: "ErrorBudgetBurn",
		Labels: map[string]string{"slo": "api", "severity": "page"},
	}}
	if err := n.Notify(ctx, t0.Add(5*time.Minute), resolved, nil); err != nil {
		t.Fatal(err)
	}
	if len(am.batches) != 3 {
		t.Fatalf("batches %d", len(am.batches))
	}
	first := am.batches[0][0]
	if first.Labels["alertname"] != "ErrorBudgetBurn" || !first.StartsAt.Equal(t0) || !first.EndsAt.Equal(t0.Add(4*time.Minute)) || first.GeneratorURL != "http://burnrate/" {
		t.Fatalf("firing alert %+v", first)
	}
	last := am.batches[2][0]
	if !last.EndsAt.Equal(t0.Add(5*time.Minute)) || last.Labels["severity"] != "page" {
		t.Fatalf("resolved alert %+v", last)
	}
}

func TestAlertmanagerErrors(t *testing.T) {
	am := &amServer{status: http.StatusInternalServerError}
	srv := httptest.NewServer(am)
	defer srv.Close()
	n := &Alertmanager{URL: srv.URL}
	err := n.Notify(context.Background(), t0, []engine.Event{{Kind: engine.EventAlertFiring}}, active())
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("err = %v", err)
	}
	down := &Alertmanager{URL: "http://127.0.0.1:1"}
	if err := down.Notify(context.Background(), t0, []engine.Event{{Kind: engine.EventAlertFiring}}, active()); err == nil {
		t.Fatal("expected connection error")
	}
	bad := &Alertmanager{URL: "://bad"}
	if err := bad.Notify(context.Background(), t0, []engine.Event{{Kind: engine.EventAlertFiring}}, active()); err == nil {
		t.Fatal("expected URL error")
	}
	// No events and nothing active: no request at all.
	if err := (&Alertmanager{URL: "http://127.0.0.1:1"}).Notify(context.Background(), t0, nil, nil); err != nil {
		t.Fatal(err)
	}
}

type failing struct{}

func (failing) Notify(context.Context, time.Time, []engine.Event, []engine.ActiveAlert) error {
	return errors.New("nope")
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestJSONLinesAndMulti(t *testing.T) {
	var buf bytes.Buffer
	evs := []engine.Event{{Kind: engine.EventBudgetState, SLO: "a", ToState: "warning"}, {Kind: engine.EventAlertFiring, SLO: "b"}}
	m := Multi{NewJSONLines(&buf), failing{}}
	err := m.Notify(context.Background(), t0, evs, nil)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"toState":"warning"`) {
		t.Fatalf("lines %q", lines)
	}
	if err := NewJSONLines(brokenWriter{}).Notify(context.Background(), t0, evs, nil); err == nil {
		t.Fatal("write error must surface")
	}
}
