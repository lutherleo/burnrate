// Package notify delivers engine events to the outside world: a JSON-lines
// event log and the Alertmanager v2 API.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lutherleo/burnrate/internal/engine"
)

// Notifier receives the events of one evaluation round together with the set
// of alerts firing after it.
type Notifier interface {
	Notify(ctx context.Context, now time.Time, events []engine.Event, active []engine.ActiveAlert) error
}

// JSONLines writes every event as one JSON object per line.
type JSONLines struct {
	mu sync.Mutex
	w  io.Writer
}

// NewJSONLines returns a notifier writing to w.
func NewJSONLines(w io.Writer) *JSONLines { return &JSONLines{w: w} }

// Notify implements Notifier.
func (j *JSONLines) Notify(_ context.Context, _ time.Time, events []engine.Event, _ []engine.ActiveAlert) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	enc := json.NewEncoder(j.w)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return err
		}
	}
	return nil
}

// Alertmanager posts alerts to an Alertmanager's /api/v2/alerts endpoint.
//
// Firing alerts are (re)sent at least every ResendInterval with an endsAt
// four intervals in the future, as Prometheus does, so Alertmanager resolves
// them on its own if burnrate goes away. Resolutions are sent immediately
// with endsAt set to the resolution time.
type Alertmanager struct {
	URL            string // base URL, e.g. http://alertmanager:9093
	HTTP           *http.Client
	ResendInterval time.Duration
	// GeneratorURL is attached to every alert (e.g. this server's status page).
	GeneratorURL string

	mu       sync.Mutex
	lastSent time.Time
}

// amAlert is the Alertmanager v2 postable alert.
type amAlert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
}

// Notify implements Notifier.
func (a *Alertmanager) Notify(ctx context.Context, now time.Time, events []engine.Event, active []engine.ActiveAlert) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	resend := a.ResendInterval
	if resend <= 0 {
		resend = time.Minute
	}
	var batch []amAlert
	changed := false
	for _, ev := range events {
		switch ev.Kind {
		case engine.EventAlertResolved:
			changed = true
			batch = append(batch, amAlert{
				Labels: alertLabels(ev.AlertName, ev.Labels), Annotations: ev.Annotations,
				StartsAt: ev.Time, EndsAt: ev.Time, GeneratorURL: a.GeneratorURL,
			})
		case engine.EventAlertFiring:
			changed = true
		}
	}
	if changed || now.Sub(a.lastSent) >= resend {
		for _, al := range active {
			batch = append(batch, amAlert{
				Labels: alertLabels(al.Name, al.Labels), Annotations: al.Annotations,
				StartsAt: al.Since, EndsAt: now.Add(4 * resend), GeneratorURL: a.GeneratorURL,
			})
		}
	}
	if len(batch) == 0 {
		return nil
	}
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].Labels["slo"] < batch[j].Labels["slo"] })
	if err := a.post(ctx, batch); err != nil {
		return err
	}
	a.lastSent = now
	return nil
}

func alertLabels(name string, ls map[string]string) map[string]string {
	out := make(map[string]string, len(ls)+1)
	for k, v := range ls {
		out[k] = v
	}
	out["alertname"] = name
	return out
}

func (a *Alertmanager) post(ctx context.Context, batch []amAlert) error {
	body, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.URL, "/")+"/api/v2/alerts", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := a.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("alertmanager: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("alertmanager: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

// Multi fans events out to several notifiers and joins their errors.
type Multi []Notifier

// Notify implements Notifier.
func (m Multi) Notify(ctx context.Context, now time.Time, events []engine.Event, active []engine.ActiveAlert) error {
	var errs []error
	for _, n := range m {
		if err := n.Notify(ctx, now, events, active); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
