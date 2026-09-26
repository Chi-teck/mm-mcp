package mattermost

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"
)

// MaxScheduleDays is how far ahead ParseSchedule accepts a time. The ceiling catches epoch
// seconds, microseconds and nanoseconds passed where milliseconds are expected.
const MaxScheduleDays = 365

const dayMillis = int64(24 * time.Hour / time.Millisecond)

// minEpochMillis (1973-03-03) is the smallest all-digit input read as epoch milliseconds. Shorter
// numbers are refused: they are compact dates (`20240101`) or epoch seconds, not milliseconds.
const minEpochMillis = 100_000_000_000

var (
	relativePattern = regexp.MustCompile(`^(\d+)([smhd])$`)
	epochPattern    = regexp.MustCompile(`^\d+$`)

	unitMillis = map[string]int64{
		"s": int64(time.Second / time.Millisecond),
		"m": int64(time.Minute / time.Millisecond),
		"h": int64(time.Hour / time.Millisecond),
		"d": dayMillis,
	}

	// Layouts with an explicit offset; parsed as written.
	zonedLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04Z07:00"}
	// Layouts without an offset; parsed in the server process's local zone.
	localLayouts = []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04"}
)

var (
	// errGrammar marks input that matches no form of the time grammar.
	errGrammar = errors.New("unrecognized time")
	// errShortEpoch marks an all-digit input below minEpochMillis.
	errShortEpoch = errors.New("too short for epoch ms")
)

// shortEpochHint is appended to the argument error for errShortEpoch.
const shortEpochHint = "too short for epoch ms; use epoch milliseconds or an ISO date like 2024-01-01"

// ParseSince resolves a past-facing time argument (`45s`, `30m`, `2h`, `3d`, an ISO 8601
// date/datetime, or epoch milliseconds) to epoch milliseconds. Relative forms are subtracted
// from now.
func ParseSince(input string, now time.Time) (int64, error) {
	at, err := resolveTime(input, now, -1)
	if errors.Is(err, errShortEpoch) {
		return 0, fmt.Errorf("invalid since: %q (%s)", input, shortEpochHint)
	}
	if err != nil {
		return 0, fmt.Errorf(`invalid since: %q (use "2h", "30m", "45s", "3d", ISO 8601 date, or epoch ms)`, input)
	}
	return at, nil
}

// ParseSchedule resolves a future-facing time argument to epoch milliseconds. It takes the
// ParseSince grammar with relative forms added to now; the result must be strictly after now
// and at most MaxScheduleDays ahead.
func ParseSchedule(input string, now time.Time) (int64, error) {
	at, err := resolveTime(input, now, 1)
	if errors.Is(err, errShortEpoch) {
		return 0, fmt.Errorf("invalid schedule_at: %q (%s)", input, shortEpochHint)
	}
	if err != nil {
		return 0, fmt.Errorf(`invalid schedule_at: %q (use "30m", "2h", "3d", ISO 8601 date, or epoch ms)`, input)
	}
	nowMillis := now.UnixMilli()
	if at <= nowMillis {
		return 0, fmt.Errorf("cannot schedule in the past: %q resolved to %s", input, RelTime(at, now))
	}
	if at-nowMillis > MaxScheduleDays*dayMillis {
		// Only an epoch input can be microseconds or nanoseconds in disguise.
		if epochPattern.MatchString(input) {
			return 0, fmt.Errorf("cannot schedule more than %d days out: %q resolved to epoch ms %d"+
				" — a value this far ahead is usually microseconds or nanoseconds mistaken for milliseconds",
				MaxScheduleDays, input, at)
		}
		return 0, fmt.Errorf("cannot schedule more than %d days out: %q resolved to %s",
			MaxScheduleDays, input, time.UnixMilli(at).UTC().Format(time.DateOnly))
	}
	return at, nil
}

// resolveTime applies the shared grammar; sign is -1 for past-facing and +1 for future-facing
// relative offsets.
func resolveTime(input string, now time.Time, sign int64) (int64, error) {
	if m := relativePattern.FindStringSubmatch(input); m != nil {
		n, err := strconv.ParseInt(m[1], 10, 64)
		unit := unitMillis[m[2]]
		if err != nil || n > math.MaxInt64/unit {
			return 0, errGrammar
		}
		offset := n * unit
		nowMillis := now.UnixMilli()
		if (sign < 0 && nowMillis < math.MinInt64+offset) || (sign > 0 && nowMillis > math.MaxInt64-offset) {
			return 0, errGrammar
		}
		return nowMillis + sign*offset, nil
	}
	if epochPattern.MatchString(input) {
		ms, err := strconv.ParseInt(input, 10, 64)
		if err != nil {
			return 0, errGrammar
		}
		if ms < minEpochMillis {
			return 0, errShortEpoch
		}
		return ms, nil
	}
	if t, err := time.Parse(time.DateOnly, input); err == nil {
		return t.UnixMilli(), nil // bare date: UTC midnight
	}
	for _, layout := range zonedLayouts {
		if t, err := time.Parse(layout, input); err == nil {
			return t.UnixMilli(), nil
		}
	}
	for _, layout := range localLayouts {
		if t, err := time.ParseInLocation(layout, input, time.Local); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, errGrammar
}

// RelTime renders epoch milliseconds relative to now: `just now`, `Nm ago`, `Nh ago`,
// `Nd ago` under 7 days, then the UTC date `YYYY-MM-DD`. Future times count as `just now`.
func RelTime(ms int64, now time.Time) string {
	diff := max(0, now.UnixMilli()-ms)
	switch {
	case diff < unitMillis["m"]:
		return "just now"
	case diff < unitMillis["h"]:
		return fmt.Sprintf("%dm ago", diff/unitMillis["m"])
	case diff < dayMillis:
		return fmt.Sprintf("%dh ago", diff/unitMillis["h"])
	case diff < 7*dayMillis:
		return fmt.Sprintf("%dd ago", diff/dayMillis)
	}
	return time.UnixMilli(ms).UTC().Format(time.DateOnly)
}
