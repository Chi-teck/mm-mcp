package mattermost

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func formatInt(n int64) string { return strconv.FormatInt(n, 10) }

func localMillis(t *testing.T, layout, value string) int64 {
	t.Helper()
	parsed, err := time.ParseInLocation(layout, value, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.UnixMilli()
}

func TestParseSince(t *testing.T) {
	now := testNow.UnixMilli()
	tests := []struct {
		input string
		want  int64
	}{
		{"45s", now - 45_000},
		{"30m", now - 30*60_000},
		{"2h", now - 2*3_600_000},
		{"3d", now - 3*86_400_000},
		{"0s", now},
		{"1700000000000", 1_700_000_000_000},
		{"100000000000", 100_000_000_000},
		{"2026-06-01", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).UnixMilli()},
		{"2026-06-01T10:30:00Z", time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC).UnixMilli()},
		{"2026-06-01T10:30:00.250+02:00", time.Date(2026, 6, 1, 8, 30, 0, 250e6, time.UTC).UnixMilli()},
		{"2026-06-01T10:30+02:00", time.Date(2026, 6, 1, 8, 30, 0, 0, time.UTC).UnixMilli()},
		{"2026-06-01T10:30:00", localMillis(t, "2006-01-02T15:04:05", "2026-06-01T10:30:00")},
		{"2026-06-01T10:30", localMillis(t, "2006-01-02T15:04", "2026-06-01T10:30")},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseSince(tt.input, testNow)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseSinceInvalid(t *testing.T) {
	for _, input := range []string{
		"", "2w", "-2h", "2 h", "h", "yesterday", "2026-13-01", "2026-06-01 10:30",
		"99999999999999999999", "99999999999999999d",
	} {
		t.Run(input, func(t *testing.T) {
			_, err := ParseSince(input, testNow)
			if err == nil || !strings.HasPrefix(err.Error(), "invalid since: ") {
				t.Fatalf("got %v, want an invalid since error", err)
			}
		})
	}
}

func TestParseSchedule(t *testing.T) {
	now := testNow.UnixMilli()
	tests := []struct {
		input string
		want  int64
	}{
		{"45s", now + 45_000},
		{"30m", now + 30*60_000},
		{"2h", now + 2*3_600_000},
		{"3d", now + 3*86_400_000},
		{"365d", now + 365*86_400_000},
		{"1s", now + 1000},
		{"2027-06-01", time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC).UnixMilli()},
		{"2026-10-01T09:00:00Z", time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).UnixMilli()},
		{"2026-10-01T09:00", localMillis(t, "2006-01-02T15:04", "2026-10-01T09:00")},
		{formatInt(now + 86_400_000), now + 86_400_000},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseSchedule(tt.input, testNow)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseScheduleRefused(t *testing.T) {
	now := testNow.UnixMilli()
	tests := []struct {
		name, input, wantPrefix string
	}{
		{"zero offset", "0s", "cannot schedule in the past: "},
		{"now as epoch ms", formatInt(now), "cannot schedule in the past: "},
		{"past date", "2026-01-01", "cannot schedule in the past: "},
		{"epoch seconds", formatInt(now / 1000), "invalid schedule_at: "},
		{"just over 365 days", formatInt(now + 365*86_400_000 + 1), "cannot schedule more than 365 days out: "},
		{"366d", "366d", "cannot schedule more than 365 days out: "},
		{"epoch microseconds", formatInt(now * 1000), "cannot schedule more than 365 days out: "},
		{"epoch nanoseconds", formatInt(now * 1_000_000), "cannot schedule more than 365 days out: "},
		{"garbage", "next week", "invalid schedule_at: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSchedule(tt.input, testNow)
			if err == nil || !strings.HasPrefix(err.Error(), tt.wantPrefix) {
				t.Fatalf("got %v, want prefix %q", err, tt.wantPrefix)
			}
		})
	}
}

func TestParseScheduleTooFarMessage(t *testing.T) {
	micros := formatInt(testNow.UnixMilli() * 1000)
	tests := []struct{ input, want string }{
		{"366d", `cannot schedule more than 365 days out: "366d" resolved to 2027-09-26`},
		{"2030-01-01", `cannot schedule more than 365 days out: "2030-01-01" resolved to 2030-01-01`},
		{micros, `cannot schedule more than 365 days out: "` + micros + `" resolved to epoch ms ` + micros +
			` — a value this far ahead is usually microseconds or nanoseconds mistaken for milliseconds`},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			_, err := ParseSchedule(tt.input, testNow)
			if err == nil || err.Error() != tt.want {
				t.Errorf("got %v, want %q", err, tt.want)
			}
		})
	}
}

func TestParseShortEpoch(t *testing.T) {
	const hint = "(too short for epoch ms; use epoch milliseconds or an ISO date like 2024-01-01)"
	for _, input := range []string{"20240101", "0", "42", "99999999999", "000000000001"} {
		t.Run(input, func(t *testing.T) {
			_, err := ParseSince(input, testNow)
			if want := "invalid since: " + strconv.Quote(input) + " " + hint; err == nil || err.Error() != want {
				t.Errorf("since: got %v, want %q", err, want)
			}
			_, err = ParseSchedule(input, testNow)
			if want := "invalid schedule_at: " + strconv.Quote(input) + " " + hint; err == nil || err.Error() != want {
				t.Errorf("schedule_at: got %v, want %q", err, want)
			}
		})
	}
}

func TestParseSchedulePastMessage(t *testing.T) {
	_, err := ParseSchedule("2026-09-24", testNow)
	want := `cannot schedule in the past: "2026-09-24" resolved to 1d ago`
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestRelTime(t *testing.T) {
	now := testNow.UnixMilli()
	tests := []struct {
		name string
		ms   int64
		want string
	}{
		{"future", now + 5000, "just now"},
		{"same instant", now, "just now"},
		{"59s", now - 59_999, "just now"},
		{"1m", now - 60_000, "1m ago"},
		{"59m", now - 3_599_999, "59m ago"},
		{"1h", now - 3_600_000, "1h ago"},
		{"23h", now - 86_399_999, "23h ago"},
		{"1d", now - 86_400_000, "1d ago"},
		{"6d", now - 7*86_400_000 + 1, "6d ago"},
		{"7d", now - 7*86_400_000, "2026-09-18"},
		{"old", time.Date(2025, 1, 2, 23, 59, 0, 0, time.UTC).UnixMilli(), "2025-01-02"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RelTime(tt.ms, testNow); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestParseZones pins time.Local to a non-UTC zone so a naive datetime and a bare date resolve
// differently even when the host itself runs in UTC.
func TestParseZones(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("UTC+3", 3*3600)
	t.Cleanup(func() { time.Local = saved })

	tests := []struct {
		input string
		want  time.Time
	}{
		{"2026-06-01", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-06-01T10:30:00", time.Date(2026, 6, 1, 7, 30, 0, 0, time.UTC)},
		{"2026-06-01T10:30", time.Date(2026, 6, 1, 7, 30, 0, 0, time.UTC)},
		{"2026-06-01T10:30:00Z", time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseSince(tt.input, testNow)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want.UnixMilli() {
				t.Errorf("got %d, want %d", got, tt.want.UnixMilli())
			}
		})
	}
}
