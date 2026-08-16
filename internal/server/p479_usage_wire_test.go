package server

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// Task #479 — the per-message usage wire format.
//
// The property under test is the one the whole feature rests on: a provider
// that does not report caching must reach the browser as null, NOT 0. Those
// two render identically if the distinction is lost in serialization, and a
// fabricated 0% is indistinguishable from a genuine cache miss.

func TestUsageWire_NullRatioWhenProviderDoesNotReportCaching(t *testing.T) {
	m := message.Message{
		ID:   "m1",
		Role: message.Assistant,
		Usage: &message.TokenUsage{
			InputTokens:  1000,
			OutputTokens: 20,
			TotalTokens:  1020,
			CacheSupport: message.CacheSupportNone,
		},
	}

	raw, err := json.Marshal(toMessageWire(m))
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	usage, ok := decoded["Usage"].(map[string]any)
	require.True(t, ok, "Usage must be present")

	ratio, present := usage["CacheHitRatio"]
	require.True(t, present, "the key must be emitted, not omitted, so the client can branch on null")
	require.Nil(t, ratio, "an unsupported provider must serialize as null, never as 0")
}

func TestUsageWire_RealRatioSurvivesSerialization(t *testing.T) {
	m := message.Message{
		ID:   "m2",
		Role: message.Assistant,
		Usage: &message.TokenUsage{
			InputTokens:         10,
			CacheReadTokens:     17298,
			CacheCreationTokens: 6203,
			OutputTokens:        157,
			TotalTokens:         23668,
			CostUSD:             0.25,
			Provider:            "local-cli",
			Model:               "cli-claude-sonnet",
			CacheSupport:        message.CacheSupportNative,
		},
	}

	wire := toMessageWire(m)
	require.NotNil(t, wire.Usage)
	require.NotNil(t, wire.Usage.CacheHitRatio)
	// 17298 cached of a 23511-token prompt.
	require.InDelta(t, 17298.0/23511.0, *wire.Usage.CacheHitRatio, 1e-9)
	require.Equal(t, int64(23511), wire.Usage.PromptTokens,
		"PromptTokens is computed for the client so it cannot re-derive it wrongly")
}

// TestUsageWire_AbsentWhenNeverRecorded keeps pre-feature messages from
// arriving as a zero-valued usage object, which the UI would render as a
// measured turn with a 0% cache hit.
func TestUsageWire_AbsentWhenNeverRecorded(t *testing.T) {
	m := message.Message{ID: "m3", Role: message.Assistant, Usage: nil}

	wire := toMessageWire(m)
	require.Nil(t, wire.Usage)

	raw, err := json.Marshal(wire)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	_, present := decoded["Usage"]
	require.False(t, present, "omitempty must drop the key entirely when nothing was measured")
}

// TestUsageWire_MeasuredZeroIsNotAbsent is the counterpart: a turn that really
// had no cache hits is data, and must arrive as 0 rather than vanish.
func TestUsageWire_MeasuredZeroIsNotAbsent(t *testing.T) {
	m := message.Message{
		ID:   "m4",
		Role: message.Assistant,
		Usage: &message.TokenUsage{
			InputTokens:  5000,
			OutputTokens: 30,
			TotalTokens:  5030,
			CacheSupport: message.CacheSupportNative,
		},
	}

	wire := toMessageWire(m)
	require.NotNil(t, wire.Usage)
	require.NotNil(t, wire.Usage.CacheHitRatio, "a measured zero IS answerable")
	require.Zero(t, *wire.Usage.CacheHitRatio)
}
