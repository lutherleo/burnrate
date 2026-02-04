package server

import (
	"bytes"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// metricWriter renders the Prometheus text exposition format by hand; the
// handful of gauges and counters burnrate exports do not justify a client
// library dependency.
type metricWriter struct {
	w bytes.Buffer
}

func (m *metricWriter) family(name, typ, help string) {
	m.w.WriteString("# HELP " + name + " " + help + "\n")
	m.w.WriteString("# TYPE " + name + " " + typ + "\n")
}

func (m *metricWriter) sample(name string, labels map[string]string, v float64) {
	m.w.WriteString(name)
	if len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		m.w.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				m.w.WriteByte(',')
			}
			m.w.WriteString(k + `="` + escapeLabel(labels[k]) + `"`)
		}
		m.w.WriteByte('}')
	}
	m.w.WriteByte(' ')
	m.w.WriteString(formatValue(v))
	m.w.WriteByte('\n')
}

func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(v)
}

func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	m := &metricWriter{}
	statuses := s.eng.Status()

	m.family("burnrate_slo_objective_ratio", "gauge", "SLO objective.")
	for _, st := range statuses {
		m.sample("burnrate_slo_objective_ratio", map[string]string{"slo": st.SLO, "service": st.Service}, st.Objective)
	}
	m.family("burnrate_slo_error_ratio", "gauge", "Share of bad events over the window (NaN without traffic).")
	for _, st := range statuses {
		for _, ws := range st.Windows {
			m.sample("burnrate_slo_error_ratio", map[string]string{"slo": st.SLO, "window": ws.Window}, float64(ws.ErrorRatio))
		}
	}
	m.family("burnrate_slo_burn_rate", "gauge", "Error ratio over the window divided by the error budget.")
	for _, st := range statuses {
		for _, ws := range st.Windows {
			m.sample("burnrate_slo_burn_rate", map[string]string{"slo": st.SLO, "window": ws.Window}, float64(ws.BurnRate))
		}
	}
	m.family("burnrate_slo_error_budget_remaining_ratio", "gauge", "Share of the period's error budget left (negative when overspent).")
	for _, st := range statuses {
		m.sample("burnrate_slo_error_budget_remaining_ratio", map[string]string{"slo": st.SLO}, float64(st.Budget.Remaining))
	}
	m.family("burnrate_slo_budget_policy_state", "gauge", "1 for the current error budget policy state.")
	for _, st := range statuses {
		m.sample("burnrate_slo_budget_policy_state", map[string]string{"slo": st.SLO, "state": st.Budget.State}, 1)
	}
	m.family("burnrate_slo_alert_firing", "gauge", "1 while the burn-rate alert of the severity is firing.")
	for _, st := range statuses {
		for _, a := range st.Alerts {
			v := 0.0
			if a.Firing {
				v = 1
			}
			m.sample("burnrate_slo_alert_firing", map[string]string{"slo": st.SLO, "severity": a.Severity}, v)
		}
	}

	es := s.eng.Stats()
	m.family("burnrate_engine_samples_total", "counter", "Counter samples seen by the engine, by outcome.")
	for _, c := range []struct {
		result string
		v      uint64
	}{
		{"received", es.SamplesReceived}, {"matched", es.SamplesMatched}, {"too_old", es.SamplesTooOld},
		{"future", es.SamplesFuture}, {"out_of_order", es.SamplesOutOfOrder}, {"invalid", es.SamplesInvalid},
	} {
		m.sample("burnrate_engine_samples_total", map[string]string{"result": c.result}, float64(c.v))
	}
	m.family("burnrate_engine_counter_resets_total", "counter", "Counter resets detected.")
	m.sample("burnrate_engine_counter_resets_total", nil, float64(es.CounterResets))
	m.family("burnrate_engine_series", "gauge", "Series with tracked counter state.")
	m.sample("burnrate_engine_series", nil, float64(es.SeriesTracked))
	m.family("burnrate_engine_evaluations_total", "counter", "Evaluation steps run.")
	m.sample("burnrate_engine_evaluations_total", nil, float64(es.Evaluations))
	m.family("burnrate_engine_last_evaluation_timestamp_seconds", "gauge", "Time of the last evaluation step.")
	last := 0.0
	if t := s.eng.LastEvaluation(); !t.IsZero() {
		last = float64(t.UnixMilli()) / 1000
	}
	m.sample("burnrate_engine_last_evaluation_timestamp_seconds", nil, last)

	rs := &s.receiver.Stats
	m.family("burnrate_otlp_requests_total", "counter", "OTLP export requests received.")
	m.sample("burnrate_otlp_requests_total", nil, float64(rs.Requests.Load()))
	m.family("burnrate_otlp_request_errors_total", "counter", "OTLP export requests answered with an error.")
	m.sample("burnrate_otlp_request_errors_total", nil, float64(rs.RequestErrors.Load()))
	m.family("burnrate_otlp_points_total", "counter", "OTLP data points, by outcome.")
	m.sample("burnrate_otlp_points_total", map[string]string{"result": "accepted"}, float64(rs.PointsAccepted.Load()))
	m.sample("burnrate_otlp_points_total", map[string]string{"result": "rejected"}, float64(rs.PointsRejected.Load()))
	m.sample("burnrate_otlp_points_total", map[string]string{"result": "ignored"}, float64(rs.PointsIgnored.Load()))
	m.family("burnrate_notify_errors_total", "counter", "Failed notification attempts.")
	m.sample("burnrate_notify_errors_total", nil, float64(s.notifyErrors.Load()))
	if _, err := w.Write(m.w.Bytes()); err != nil {
		s.log.Debug("writing metrics", "error", err)
	}
}
