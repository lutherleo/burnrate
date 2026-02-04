// Package server wires the OTLP receiver, the engine, notifiers and the HTTP
// API into one process.
//
// Two clocks are supported. In wall-clock mode (the default for live use) a
// ticker evaluates the engine every resolution step up to now minus a small
// lateness allowance. In data-clock mode evaluation follows the newest sample
// timestamp instead, which lets a recorded or simulated stream be replayed as
// fast as it can be sent.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lutherleo/burnrate/internal/engine"
	"github.com/lutherleo/burnrate/internal/notify"
	"github.com/lutherleo/burnrate/internal/otlp"
	"github.com/lutherleo/burnrate/internal/report"
	"github.com/lutherleo/burnrate/internal/slo"
)

// Clock selects what drives evaluation.
type Clock string

// Supported clocks.
const (
	WallClock Clock = "wall"
	DataClock Clock = "data"
)

// Config configures a Server.
type Config struct {
	Clock Clock
	// Lateness is how far behind the wall clock evaluation runs, giving
	// exporters time to deliver. Default 30s. Wall clock only.
	Lateness    time.Duration
	Engine      engine.Config
	Translation otlp.Options
	Notifier    notify.Notifier
	Logger      *slog.Logger
	// Now overrides the wall clock (tests).
	Now func() time.Time
}

// Server is a running burnrate instance.
type Server struct {
	cfg      Config
	eng      *engine.Engine
	receiver *otlp.Receiver
	mux      *http.ServeMux
	log      *slog.Logger

	// advanceMu serialises Advance+notify so events reach notifiers in order.
	advanceMu    sync.Mutex
	notifyErrors atomic.Uint64
}

// New builds a server for the given SLOs.
func New(cfg Config, slos []*slo.SLO) (*Server, error) {
	if cfg.Clock == "" {
		cfg.Clock = WallClock
	}
	if cfg.Clock != WallClock && cfg.Clock != DataClock {
		return nil, fmt.Errorf("server: unknown clock %q (wall or data)", cfg.Clock)
	}
	if cfg.Lateness == 0 {
		cfg.Lateness = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Engine.Logger == nil {
		cfg.Engine.Logger = cfg.Logger
	}
	if cfg.Clock == WallClock && cfg.Engine.Now == nil {
		cfg.Engine.Now = cfg.Now
	}
	eng, err := engine.New(cfg.Engine, slos)
	if err != nil {
		return nil, err
	}
	tr, err := otlp.NewTranslator(cfg.Translation, eng.Wants)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, eng: eng, log: cfg.Logger}
	s.receiver = otlp.NewReceiver(tr, s.ingest, cfg.Logger)
	s.mux = http.NewServeMux()
	s.routes()
	return s, nil
}

// Engine exposes the underlying engine.
func (s *Server) Engine() *engine.Engine { return s.eng }

// Handler returns the HTTP handler serving OTLP and the API.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) ingest(samples []engine.Sample) {
	s.eng.Ingest(samples)
	if s.cfg.Clock == DataClock {
		s.advance(s.eng.Watermark())
	}
}

// advance evaluates up to t and hands the results to the notifier.
func (s *Server) advance(t time.Time) {
	s.advanceMu.Lock()
	defer s.advanceMu.Unlock()
	events := s.eng.Advance(t)
	if s.cfg.Notifier == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.cfg.Notifier.Notify(ctx, s.eng.LastEvaluation(), events, s.eng.ActiveAlerts()); err != nil {
		s.notifyErrors.Add(1)
		s.log.Warn("notification failed", "error", err)
	}
}

// Serve accepts connections on ln until ctx is cancelled, running the
// evaluation ticker in wall-clock mode. It returns after a graceful shutdown.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	var tick <-chan time.Time
	if s.cfg.Clock == WallClock {
		t := time.NewTicker(s.eng.Resolution())
		defer t.Stop()
		tick = t.C
	}
	s.log.Info("burnrate listening", "addr", ln.Addr().String(), "clock", s.cfg.Clock, "resolution", s.eng.Resolution())
	for {
		select {
		case <-tick:
			s.advance(s.cfg.Now().Add(-s.cfg.Lateness))
		case err := <-errc:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutdown)
		}
	}
}

func (s *Server) routes() {
	s.mux.Handle("POST "+otlp.Path, s.receiver)
	s.mux.Handle(otlp.Path, s.receiver) // the receiver answers 405 itself
	s.mux.HandleFunc("GET /api/v1/slos", s.handleSLOs)
	s.mux.HandleFunc("GET /api/v1/slos/{name}", s.handleSLO)
	s.mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	s.mux.HandleFunc("GET /api/v1/alerts", s.handleAlerts)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeText(w, "ok\n") })
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
}

type slosResponse struct {
	EvaluatedAt *time.Time      `json:"evaluatedAt"`
	Clock       Clock           `json:"clock"`
	SLOs        []engine.Status `json:"slos"`
}

func (s *Server) handleSLOs(w http.ResponseWriter, _ *http.Request) {
	resp := slosResponse{Clock: s.cfg.Clock, SLOs: s.eng.Status()}
	if t := s.eng.LastEvaluation(); !t.IsZero() {
		resp.EvaluatedAt = &t
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSLO(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, st := range s.eng.Status() {
		if st.SLO == name {
			writeJSON(w, http.StatusOK, st)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "no SLO named " + strconv.Quote(name)})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be a positive integer"})
			return
		}
		limit = n
	}
	out := []engine.Event{}
	all := s.eng.Events()
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		ev := all[i]
		if (q.Get("slo") == "" || ev.SLO == q.Get("slo")) && (q.Get("kind") == "" || string(ev.Kind) == q.Get("kind")) {
			out = append(out, ev)
		}
	}
	// Oldest first, like the engine's history.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

type alertJSON struct {
	Name        string            `json:"name"`
	SLO         string            `json:"slo"`
	Severity    string            `json:"severity"`
	Since       time.Time         `json:"since"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

func (s *Server) handleAlerts(w http.ResponseWriter, _ *http.Request) {
	out := []alertJSON{}
	for _, a := range s.eng.ActiveAlerts() {
		out = append(out, alertJSON(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": out})
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "burnrate: %s clock, last evaluation %s\n\n", s.cfg.Clock, formatTime(s.eng.LastEvaluation()))
	if err := report.Status(w, s.eng.Status()); err != nil {
		s.log.Debug("writing index", "error", err)
	}
	fmt.Fprintln(w, "\nAPI: /api/v1/slos  /api/v1/slos/{name}  /api/v1/events  /api/v1/alerts  /metrics  POST /v1/metrics (OTLP)")
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v) // the client went away; nothing useful to do
}

func writeText(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s))
}
