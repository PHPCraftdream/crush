package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Task #475 — --since must honour Go duration units.
//
// parseSinceDuration tries "plain integer as days" BEFORE time.ParseDuration,
// and parsePlainInt used fmt.Sscanf(s, "%d", &n), which parses the leading
// digits and reports no error for the trailing remainder. Every duration that
// starts with a digit — i.e. all of them — was therefore swallowed by the
// day-count branch:
//
//	"2h"    -> 2 days   (48h)
//	"30m"   -> 30 days  (720h)
//	"45s"   -> 45 days  (1080h)
//	"1h30m" -> 1 day    (24h)
//
// This is a PRE-EXISTING defect: `sessions cost --since` shipped with it, and
// its own help text advertises "Go durations (30m, 24h)". It surfaced only
// when `sessions cache --since 2h` visibly failed to exclude a 26-hour-old
// row during manual verification.
//
// REVERT CHECK: restore fmt.Sscanf in parsePlainInt and every unit case below
// fails.

func TestParseSinceDuration_HonoursGoDurationUnits(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"2h", 2 * time.Hour},
		{"30m", 30 * time.Minute},
		{"45s", 45 * time.Second},
		{"1h30m", 90 * time.Minute},
		{"24h", 24 * time.Hour},
	} {
		got, err := parseSinceDuration(tc.in)
		require.NoError(t, err, "input %q", tc.in)
		require.Equal(t, tc.want, got,
			"%q must be %v, not a day count — the day branch used to swallow every unit suffix", tc.in, tc.want)
	}
}

// TestParseSinceDuration_DayFormsStillWork guards the two forms that were
// correct before, so the fix does not trade one bug for another.
func TestParseSinceDuration_DayFormsStillWork(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"7d", 7 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"3", 3 * 24 * time.Hour},   // bare integer means days
		{"14", 14 * 24 * time.Hour}, // multi-digit bare integer
	} {
		got, err := parseSinceDuration(tc.in)
		require.NoError(t, err, "input %q", tc.in)
		require.Equal(t, tc.want, got, "input %q", tc.in)
	}
}

func TestParseSinceDuration_RejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "abc", "7dd", "h", "1x"} {
		_, err := parseSinceDuration(in)
		require.Error(t, err, "%q must not be silently accepted", in)
	}
}

// TestParsePlainInt_RequiresTheWholeString is the unit-level statement of the
// root cause: a partial parse must be an error, or the caller cannot tell
// "7" from "7d" from "7 bananas".
func TestParsePlainInt_RequiresTheWholeString(t *testing.T) {
	n, err := parsePlainInt("7")
	require.NoError(t, err)
	require.Equal(t, 7, n)

	for _, in := range []string{"2h", "30m", "45s", "7d", "1h30m", "12abc"} {
		_, err := parsePlainInt(in)
		require.Error(t, err,
			"%q is not a bare integer; accepting it makes --since off by orders of magnitude", in)
	}
}
