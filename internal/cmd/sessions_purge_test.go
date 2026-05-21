package cmd

import (
	"testing"
	"time"
)

func TestParseAge_Valid(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"30s", 30 * time.Second},
		{"30sec", 30 * time.Second},
		{"15min", 15 * time.Minute},
		{"3h", 3 * time.Hour},
		{"3d", 3 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"1m", 30 * 24 * time.Hour},
		{"1mo", 30 * 24 * time.Hour},
		{"1mon", 30 * 24 * time.Hour},
		{"1y", 365 * 24 * time.Hour},
		{"0.5h", 30 * time.Minute},
		{"  3d  ", 3 * 24 * time.Hour},
		{"1M", 30 * 24 * time.Hour}, // case-insensitive unit
	}
	for _, c := range cases {
		got, err := parseAge(c.in)
		if err != nil {
			t.Errorf("parseAge(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseAge(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseAge_Invalid(t *testing.T) {
	cases := []string{
		"",
		"abc",
		"3",       // missing unit
		"3x",      // unknown unit
		"d3",      // unit before number
		"-1h",     // negative — parser fails on '-' as non-digit prefix
		"0h",      // zero is rejected
		"1minute", // not a recognised suffix
	}
	for _, in := range cases {
		if _, err := parseAge(in); err == nil {
			t.Errorf("parseAge(%q) expected error, got nil", in)
		}
	}
}
