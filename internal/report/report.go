// Package report renders engine state and alert arithmetic as plain-text
// tables for the CLI and the server's index page.
package report

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lutherleo/burnrate/internal/burnmath"
	"github.com/lutherleo/burnrate/internal/engine"
	"github.com/lutherleo/burnrate/internal/slo"
)

// Status writes one row per SLO.
func Status(w io.Writer, statuses []engine.Status) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SLO\tTARGET\tPERIOD\tBUDGET LEFT\tSTATE\tFIRING\tBURN RATE BY WINDOW")
	for _, s := range statuses {
		var firing []string
		for _, a := range s.Alerts {
			if a.Firing {
				firing = append(firing, a.Severity)
			}
		}
		if len(firing) == 0 {
			firing = []string{"-"}
		}
		var burns []string
		for _, ws := range s.Windows {
			if ws.Window == s.Budget.Period {
				continue
			}
			burns = append(burns, ws.Window+" "+Num(float64(ws.BurnRate)))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.SLO, slo.FormatPercent(s.Objective), s.Budget.Period,
			Percent(float64(s.Budget.Remaining)), s.Budget.State, strings.Join(firing, ","), strings.Join(burns, "  "))
	}
	return tw.Flush()
}

// Events writes a timeline of events with times relative to origin.
func Events(w io.Writer, events []engine.Event, origin time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "T+\tEVENT\tSLO\tDETAIL")
	for _, ev := range events {
		var kind, detail string
		switch ev.Kind {
		case engine.EventAlertFiring:
			kind = "FIRING " + ev.Severity
			detail = pairDetail(ev.Pairs, true)
		case engine.EventAlertResolved:
			kind = "resolved " + ev.Severity
			detail = "budget left " + Percent(float64(ev.BudgetRemaining))
		case engine.EventBudgetState:
			kind = "budget"
			detail = fmt.Sprintf("%s -> %s (left %s)", ev.FromState, ev.ToState, Percent(float64(ev.BudgetRemaining)))
		default:
			kind = string(ev.Kind)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", Offset(ev.Time.Sub(origin)), kind, ev.SLO, detail)
	}
	return tw.Flush()
}

func pairDetail(pairs []engine.PairStatus, onlyActive bool) string {
	var parts []string
	for _, p := range pairs {
		if onlyActive && !p.Active {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s/%s burn %s/%s > %sx", p.Long, p.Short,
			Num(float64(p.LongBurn)), Num(float64(p.ShortBurn)), Num(p.BurnThreshold)))
	}
	return strings.Join(parts, "; ")
}

// Offset renders a duration as +1d02h03m.
func Offset(d time.Duration) string {
	sign := "+"
	if d < 0 {
		sign, d = "-", -d
	}
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	if days > 0 {
		return fmt.Sprintf("%s%dd%02dh%02dm", sign, days, h, m)
	}
	return fmt.Sprintf("%s%02dh%02dm", sign, h, m)
}

// Percent renders a ratio as a percentage, "n/a" for NaN.
func Percent(r float64) string {
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return "n/a"
	}
	return strconv.FormatFloat(r*100, 'f', 1, 64) + "%"
}

// Num renders a burn rate or ratio compactly, "n/a" for NaN.
func Num(f float64) string {
	switch {
	case math.IsNaN(f) || math.IsInf(f, 0):
		return "n/a"
	case f == 0:
		return "0"
	case math.Abs(f) >= 100:
		return strconv.FormatFloat(f, 'f', 0, 64)
	case math.Abs(f) >= 10:
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

// DefaultRatios are the incident error ratios tabulated by Math.
var DefaultRatios = []float64{1, 0.5, 0.1, 0.05, 0.02, 0.01, 0.005, 0.002, 0.0015}

// Math tabulates, for an SLO's alerting policy, how quickly each severity
// detects an incident of a given constant error ratio starting from a clean
// history, how much budget is gone by then, and how long the alert keeps
// firing after a long incident ends.
func Math(w io.Writer, s *slo.SLO, ratios []float64) error {
	fmt.Fprintf(w, "SLO %s: objective %s over %s, error budget %s of events\n\n",
		s.Name, slo.FormatPercent(s.Objective), slo.FormatDuration(s.Period), slo.FormatPercent(s.ErrorBudget()))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SEVERITY\tLONG/SHORT\tBURN RATE\tERROR-RATIO THRESHOLD\tBUDGET SPENT AT FIRE")
	for _, sev := range s.Alerting.Severities {
		for _, win := range sev.Windows {
			fmt.Fprintf(tw, "%s\t%s/%s\t%s\t%s\t%s\n", sev.Name, slo.FormatDuration(win.Long), slo.FormatDuration(win.Short),
				strconv.FormatFloat(win.BurnRate, 'g', 4, 64), Percent(win.Threshold(s.Objective)), Percent(win.BudgetConsumed))
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := "ERROR RATIO\tBURN"
	for _, sev := range s.Alerting.Severities {
		header += "\t" + strings.ToUpper(sev.Name) + " DETECT\t" + strings.ToUpper(sev.Name) + " RESET"
	}
	fmt.Fprintln(tw, header+"\tBUDGET GONE IN")
	for _, e := range ratios {
		row := fmt.Sprintf("%s\t%sx", Percent(e), Num(e/s.ErrorBudget()))
		for _, sev := range s.Alerting.Severities {
			d, ok := burnmath.SeverityDetectionTime(sev.Pairs(), s.Objective, e)
			r, rok := burnmath.SeverityResetTime(sev.Pairs(), s.Objective, e, s.Period)
			row += "\t" + human(d, ok) + "\t" + human(r, rok)
		}
		gone, _ := burnmath.TimeToExhaustion(1, e/s.ErrorBudget(), s.Period)
		fmt.Fprintln(tw, row+"\t"+human(gone, true))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "\nDETECT: time from onset to the alert firing; RESET: time from the end of a long incident until it stops.")
	return err
}

func human(d time.Duration, ok bool) string {
	if !ok {
		return "never"
	}
	switch {
	case d < time.Minute:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	case d < time.Hour:
		return strconv.FormatFloat(d.Minutes(), 'f', 1, 64) + "m"
	case d < 48*time.Hour:
		return strconv.FormatFloat(d.Hours(), 'f', 1, 64) + "h"
	}
	return strconv.FormatFloat(d.Hours()/24, 'f', 1, 64) + "d"
}
