package config

import (
	"errors"
	"testing"
	"time"
)

// ui.default_window defaults to 30d (ADR-0014) and time.ParseDuration rejects a
// `d` suffix, so the day unit is load-bearing rather than a convenience.
func TestParseDurationAcceptsDays(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Duration
	}{
		{"30d", 720 * time.Hour},
		{"7d", 168 * time.Hour},
		{"1d", 24 * time.Hour},
		{"0d", 0},
		{"0030d", 720 * time.Hour},
	} {
		got, err := parseDuration(c.in)
		if err != nil {
			t.Errorf("parseDuration(%q) = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseDurationAcceptsGoUnits(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"24h", 24 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"500ms", 500 * time.Millisecond},
		{"0s", 0},
	} {
		got, err := parseDuration(c.in)
		if err != nil {
			t.Errorf("parseDuration(%q) = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseDurationRejectsEverythingElse(t *testing.T) {
	// A sentinel word such as "forever" is a key-level concern, not a duration:
	// validate() checks Key.Sentinels before it gets here.
	for _, in := range []string{
		"", " ", "30", "d", "30 d", "-1d", "forever", "never", "1x", "30dd", "3 0d", " 30d", "30D", "١d",
	} {
		if got, err := parseDuration(in); err == nil {
			t.Errorf("parseDuration(%q) = (%v, nil), want an error", in, got)
		}
	}
}

// A negative window or timeout has no meaning for any of the eight keys, and
// accepting one would produce nonsense numbers downstream rather than an error a
// user can see.
func TestParseDurationRejectsNegatives(t *testing.T) {
	for _, in := range []string{"-1s", "-5m", "-1h30m"} {
		_, err := parseDuration(in)
		if !errors.Is(err, errNegativeDuration) {
			t.Errorf("parseDuration(%q) = %v, want errNegativeDuration", in, err)
		}
	}
}

// Days are multiplied out to nanoseconds, so a large day count overflows int64.
// Silently wrapping to a negative duration would be the worst outcome available.
func TestParseDurationRejectsOutOfRangeDays(t *testing.T) {
	for _, in := range []string{"106752d", "9999999999999999999d", "99999999999999999999999d"} {
		got, err := parseDuration(in)
		if !errors.Is(err, errDurationOutOfRange) {
			t.Errorf("parseDuration(%q) = (%v, %v), want errDurationOutOfRange", in, got, err)
		}
	}
	// The largest day count that still fits must be accepted, so the boundary is
	// pinned from both sides.
	if _, err := parseDuration("106751d"); err != nil {
		t.Errorf("parseDuration(\"106751d\") = %v, want it to fit", err)
	}
}
