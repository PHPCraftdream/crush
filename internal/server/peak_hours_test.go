package server

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// These tests cover the pure validation/conversion helpers used by the
// custom-provider WS handlers. The handlers themselves (handleAdd /
// handleUpdate / handleRemove) are not exercised here because the WS
// layer currently has no per-handler test harness — wiring one up would
// require an *app.App, a config Store, and a fake Client/Hub, which is
// out of scope for this change. The helpers below are the only logic
// added that is genuinely unit-testable without new infrastructure.

func TestPeakHoursFromWire_Absent(t *testing.T) {
	w, err := peakHoursFromWire(nil)
	require.NoError(t, err)
	require.Nil(t, w, "absent payload must yield nil (feature off)")
}

func TestPeakHoursFromWire_EmptyWindow(t *testing.T) {
	// Both fields empty is the documented "feature off" shape and must
	// validate cleanly (mirrors config.PeakHoursWindow.Validate).
	w, err := peakHoursFromWire(&PeakHoursWirePayload{})
	require.NoError(t, err)
	require.NotNil(t, w)
	require.Equal(t, "", w.Start)
	require.Equal(t, "", w.End)
}

func TestPeakHoursFromWire_Valid(t *testing.T) {
	w, err := peakHoursFromWire(&PeakHoursWirePayload{Start: "09:00", End: "18:00"})
	require.NoError(t, err)
	require.Equal(t, &config.PeakHoursWindow{Start: "09:00", End: "18:00"}, w)
}

func TestPeakHoursFromWire_ValidOvernight(t *testing.T) {
	w, err := peakHoursFromWire(&PeakHoursWirePayload{Start: "22:00", End: "06:00"})
	require.NoError(t, err)
	require.Equal(t, "22:00", w.Start)
	require.Equal(t, "06:00", w.End)
}

func TestPeakHoursFromWire_MissingEnd(t *testing.T) {
	_, err := peakHoursFromWire(&PeakHoursWirePayload{Start: "09:00"})
	require.Error(t, err, "half-set window must fail validation")
}

func TestPeakHoursFromWire_MissingStart(t *testing.T) {
	_, err := peakHoursFromWire(&PeakHoursWirePayload{End: "18:00"})
	require.Error(t, err)
}

func TestPeakHoursFromWire_BadFormat(t *testing.T) {
	_, err := peakHoursFromWire(&PeakHoursWirePayload{Start: "9:00", End: "18:00"})
	require.Error(t, err, "leading zero is required per parseHHMM")
}

func TestPeakHoursFromWire_OutOfRange(t *testing.T) {
	_, err := peakHoursFromWire(&PeakHoursWirePayload{Start: "24:00", End: "18:00"})
	require.Error(t, err)
}

func TestPeakHoursFromWire_NonDigitBytes(t *testing.T) {
	// Regression for the parseHHMM digit-check fix: "09:0;" must NOT
	// be accepted as 551 minutes.
	_, err := peakHoursFromWire(&PeakHoursWirePayload{Start: "09:0;", End: "18:00"})
	require.Error(t, err)
}

func TestPeakHoursToWire_Nil(t *testing.T) {
	require.Nil(t, peakHoursToWire(nil), "nil config window must yield nil wire payload")
}

func TestPeakHoursToWire_Set(t *testing.T) {
	w := peakHoursToWire(&config.PeakHoursWindow{Start: "22:00", End: "06:00"})
	require.Equal(t, &PeakHoursWirePayload{Start: "22:00", End: "06:00"}, w)
}

// TestPeakHoursWirePayload_JSONRoundTrip locks the wire JSON shape: the
// client sends/expects lowerCamelCase "start"/"end" keys matching
// config.PeakHoursWindow's own json tags, so the WS payload must
// serialize identically (no separate camelCase mapping needed).
func TestPeakHoursWirePayload_JSONRoundTrip(t *testing.T) {
	in := `{"start":"09:00","end":"18:00"}`
	var p PeakHoursWirePayload
	require.NoError(t, json.Unmarshal([]byte(in), &p))
	require.Equal(t, "09:00", p.Start)
	require.Equal(t, "18:00", p.End)

	out, err := json.Marshal(p)
	require.NoError(t, err)
	require.JSONEq(t, in, string(out))
}

// TestProviderWireIncludesPeakHours confirms the ConfigWire DTO exposes
// PeakHours so the web UI can read it. This is a struct-tag contract
// test — it guards against accidentally dropping the field.
func TestProviderWireIncludesPeakHours(t *testing.T) {
	pw := ProviderWire{
		Name:      "x",
		Enabled:   true,
		APIKeySet: true,
		PeakHours: &PeakHoursWirePayload{Start: "09:00", End: "18:00"},
	}
	out, err := json.Marshal(pw)
	require.NoError(t, err)
	require.Contains(t, string(out), `"peakHours":{"start":"09:00","end":"18:00"}`)

	// Absent PeakHours → field omitted (omitempty).
	pw2 := ProviderWire{Name: "y", Enabled: false}
	out2, err := json.Marshal(pw2)
	require.NoError(t, err)
	require.NotContains(t, string(out2), "peakHours")
}
