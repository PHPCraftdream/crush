package app

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// Task #480 — the `usage.session` object in the `crush run --json` envelope.
//
// The envelope is a public contract for wrapper scripts, so two things matter
// beyond the arithmetic:
//
//  1. cache_hit_ratio must serialize as null when it cannot be stated. A
//     consumer that reads 0 there cannot tell "this provider does not report
//     caching" from "the cache missed every time".
//  2. usage.session must be ABSENT rather than an all-zero object when nothing
//     was recorded, so a pre-tracking session is not reported as a measured
//     run that consumed nothing.

func TestRunEnvelope_SessionUsageAbsentWhenNothingRecorded(t *testing.T) {
	require.Nil(t, buildSessionUsageInfo(message.UsageReport{}),
		"a report with no recorded messages must produce no usage.session object")

	// And the envelope must omit the key entirely.
	raw, err := json.Marshal(usageInfo{DeltaTokens: 10, DeltaCostUSD: 0.5})
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	_, present := decoded["session"]
	require.False(t, present, "omitempty must drop usage.session when nothing was measured")
}

func TestRunEnvelope_SessionUsageSumsAndReportsCoverage(t *testing.T) {
	report := message.UsageReport{
		ByModel: []message.ModelUsage{
			{
				Messages: 2,
				Usage: message.TokenUsage{
					InputTokens: 20, CacheReadTokens: 170, CacheCreationTokens: 10,
					OutputTokens: 30, TotalTokens: 230, CostUSD: 0.25,
					Provider: "local-cli", Model: "cli-claude-sonnet",
					CacheSupport: message.CacheSupportNative,
				},
			},
		},
		MissingUsage: 7,
	}

	got := buildSessionUsageInfo(report)
	require.NotNil(t, got)
	require.Equal(t, int64(200), got.PromptTokens, "prompt is 20 fresh + 170 read + 10 write")
	require.Equal(t, int64(30), got.OutputTokens)
	require.InDelta(t, 0.25, got.CostUSD, 1e-9)
	require.Equal(t, int64(2), got.MessagesRecorded)
	require.Equal(t, int64(7), got.MessagesMissingUsage,
		"the caller must be able to see the figures cover 2 of 9 messages")

	require.NotNil(t, got.CacheHitRatio)
	require.InDelta(t, 170.0/200.0, *got.CacheHitRatio, 1e-9)
}

// TestRunEnvelope_NullRatioSerializesAsNull is the contract check an
// orchestrator depends on.
func TestRunEnvelope_NullRatioSerializesAsNull(t *testing.T) {
	report := message.UsageReport{
		ByModel: []message.ModelUsage{
			{
				Messages: 1,
				Usage: message.TokenUsage{
					InputTokens: 500, OutputTokens: 5, TotalTokens: 505,
					CacheSupport: message.CacheSupportNone, // provider is silent about caching
				},
			},
		},
	}

	raw, err := json.Marshal(usageInfo{Session: buildSessionUsageInfo(report)})
	require.NoError(t, err)

	var decoded struct {
		Session map[string]any `json:"session"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))

	ratio, present := decoded.Session["cache_hit_ratio"]
	require.True(t, present, "the key must be emitted so a consumer can branch on null")
	require.Nil(t, ratio, "an unsupported provider must be null, never 0")
}

// TestRunEnvelope_MeasuredZeroIsStillAnswer distinguishes a real cache miss
// from an unknown, in the opposite direction.
func TestRunEnvelope_MeasuredZeroIsStillAnswer(t *testing.T) {
	report := message.UsageReport{
		ByModel: []message.ModelUsage{
			{
				Messages: 1,
				Usage: message.TokenUsage{
					InputTokens: 1000, OutputTokens: 10, TotalTokens: 1010,
					CacheSupport: message.CacheSupportNative,
				},
			},
		},
	}
	got := buildSessionUsageInfo(report)
	require.NotNil(t, got.CacheHitRatio, "a measured zero IS answerable")
	require.Zero(t, *got.CacheHitRatio)
}

// TestRunEnvelope_MixedModelsDegradeRatherThanBlend guards the aggregate: a
// session that used both a cache-reporting and a silent model must not present
// a hit rate derived from partial visibility.
func TestRunEnvelope_MixedModelsDegradeRatherThanBlend(t *testing.T) {
	report := message.UsageReport{
		ByModel: []message.ModelUsage{
			{Messages: 1, Usage: message.TokenUsage{
				InputTokens: 10, CacheReadTokens: 990, TotalTokens: 1000,
				Provider: "local-cli", Model: "a", CacheSupport: message.CacheSupportNative,
			}},
			{Messages: 1, Usage: message.TokenUsage{
				InputTokens: 1000, TotalTokens: 1000,
				Provider: "local-cli", Model: "b", CacheSupport: message.CacheSupportNone,
			}},
		},
	}

	got := buildSessionUsageInfo(report)
	require.Nil(t, got.CacheHitRatio,
		"half the sample has no cache visibility; the session-level ratio must be withheld")
	// Withholding the ratio must not withhold the counts: the tokens are
	// still measured, only their cache interpretation is unavailable.
	require.Equal(t, int64(2), got.MessagesRecorded)
	require.Equal(t, int64(1010), got.InputTokens, "10 + 1000 fresh across both models")
	require.Equal(t, int64(990), got.CacheReadTokens)
	require.Equal(t, int64(2000), got.PromptTokens)
}

// TestRunEnvelope_EstimatedIsFlagged keeps synthesized numbers from reading as
// measurements.
func TestRunEnvelope_EstimatedIsFlagged(t *testing.T) {
	report := message.UsageReport{
		ByModel: []message.ModelUsage{
			{Messages: 1, Estimated: 1, Usage: message.TokenUsage{
				InputTokens: 100, TotalTokens: 100, Estimated: true,
				CacheSupport: message.CacheSupportNone,
			}},
		},
	}
	require.True(t, buildSessionUsageInfo(report).Estimated)
}
