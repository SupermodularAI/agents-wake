package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

var (
	// errNegativeDuration rejects a negative value. None of the seven keys has a
	// meaning for one: a negative window or timeout would produce nonsense
	// numbers downstream instead of an error a user can see.
	errNegativeDuration = errors.New("a duration must not be negative")
	// errDurationOutOfRange rejects a day count that does not fit in an int64 of
	// nanoseconds. Wrapping silently to a negative duration is the worst outcome
	// available, so the multiplication is checked rather than attempted.
	errDurationOutOfRange = errors.New("duration out of range")
)

// hoursPerDay is the day unit's definition here: a plain 24 hours, with no
// calendar and no daylight-saving arithmetic. The keys that take a duration are
// retention and staleness thresholds, where "30 days" means 720 hours; treating
// it as a calendar span would make the same config mean different things in
// different weeks.
const hoursPerDay = 24

// maxDayCount is the largest day count time.Duration can hold.
const maxDayCount = int64(math.MaxInt64) / int64(hoursPerDay*time.Hour)

// parseDuration parses a duration in Go's syntax, extended with a `d` (day)
// suffix: `30d` is 720h.
//
// The extension is load-bearing rather than a convenience. ui.default_window
// defaults to 30d (ADR-0014) and time.ParseDuration rejects `d` outright, so
// without this the documented default would not parse. The rule is deliberately
// narrow — a value of ASCII digits followed by a single lowercase `d`, and
// nothing else — so `1d12h` is an error rather than a silent half-parse.
//
// A sentinel word such as `forever` is not handled here: it is a property of the
// key, and validate checks Key.Sentinels before reaching this function.
func parseDuration(s string) (time.Duration, error) {
	if digits, ok := strings.CutSuffix(s, "d"); ok && isASCIIDigits(digits) {
		days, err := strconv.ParseInt(digits, 10, 64)
		// isASCIIDigits has already ruled out every syntax error ParseInt can
		// report, so a failure here is a range failure, which is also what a
		// day count above maxDayCount is.
		if err != nil || days > maxDayCount {
			return 0, fmt.Errorf("%s days: %w", digits, errDurationOutOfRange)
		}
		return time.Duration(days) * hoursPerDay * time.Hour, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("not a duration: %w", err)
	}
	if d < 0 {
		return 0, errNegativeDuration
	}
	return d, nil
}

// isASCIIDigits reports whether s is one or more ASCII digits and nothing else.
// It is what keeps the day suffix from accepting a sign, a space, or a
// non-ASCII digit that strconv would also reject but time.ParseDuration would
// report differently.
func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
