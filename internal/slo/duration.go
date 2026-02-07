package slo

import (
	"fmt"
	"strconv"
	"time"
)

// Duration is a time.Duration that reads and writes Prometheus duration
// syntax ("5m", "1h30m", "30d", "4w").
type Duration time.Duration

var durationUnits = []struct {
	suffix string
	d      time.Duration
}{
	{"w", 7 * 24 * time.Hour},
	{"d", 24 * time.Hour},
	{"h", time.Hour},
	{"m", time.Minute},
	{"s", time.Second},
	{"ms", time.Millisecond},
}

// ParseDuration parses Prometheus duration syntax: one or more <int><unit>
// terms with units in descending order, units being ms, s, m, h, d, w.
// Years are rejected because their length is ambiguous.
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var total time.Duration
	lastUnit := -1
	i := 0
	for i < len(s) {
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i {
			return 0, fmt.Errorf("duration %q: expected a number at position %d", s, i)
		}
		n, err := strconv.ParseInt(s[i:j], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("duration %q: %w", s, err)
		}
		k := j
		for k < len(s) && (s[k] < '0' || s[k] > '9') {
			k++
		}
		unit := s[j:k]
		idx := -1
		for u, du := range durationUnits {
			if du.suffix == unit {
				idx = u
			}
		}
		if idx < 0 {
			return 0, fmt.Errorf("duration %q: unknown unit %q (use ms, s, m, h, d or w)", s, unit)
		}
		if idx <= lastUnit {
			return 0, fmt.Errorf("duration %q: units must appear once, largest first", s)
		}
		lastUnit = idx
		step := durationUnits[idx].d
		if n > int64((1<<63-1)/step) || total > time.Duration(1<<63-1)-time.Duration(n)*step {
			return 0, fmt.Errorf("duration %q overflows", s)
		}
		total += time.Duration(n) * step
		i = k
	}
	return total, nil
}

// FormatDuration renders d in compact Prometheus syntax, using days as the
// largest unit so SLO windows read naturally ("30d", not "4w2d").
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	out := ""
	if d < 0 {
		out = "-"
		d = -d
	}
	for _, u := range durationUnits[1:] {
		if d >= u.d {
			n := d / u.d
			out += strconv.FormatInt(int64(n), 10) + u.suffix
			d -= n * u.d
		}
	}
	return out
}

// String implements fmt.Stringer.
func (d Duration) String() string { return FormatDuration(time.Duration(d)) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }
