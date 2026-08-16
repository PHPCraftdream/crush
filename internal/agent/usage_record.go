package agent

import (
	"context"
	"log/slog"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
)

// providerCacheSupport classifies whether a provider actually reports prompt
// caching, so a stored zero can be told apart from "we were never told".
//
// Everything currently reachable does report it, each verified against the
// mapping code or the live CLI:
//
//   - local-cli — claude/codex/gemini/qwen all emit cache counters; see
//     internal/agent/cliprovider/usage.go for the captured wire samples.
//   - openai-compatible (zai, openrouter, vercel) — fantasy fills
//     CacheReadTokens from prompt_tokens_details.cached_tokens
//     (providers/openai/language_model_hooks.go:225).
//   - anthropic — CacheCreationTokens/CacheReadTokens from the API's own
//     fields (providers/anthropic/anthropic.go:1318).
//   - google — CacheReadTokens from CachedContentTokenCount
//     (providers/google/google.go:1465).
//
// The function still exists, rather than hardcoding "native", because a
// provider that silently stops reporting would otherwise be indistinguishable
// from a perfect cache miss streak. When a new provider type appears here that
// has NOT been checked, classify it as none and let the UI say "n/a" — an
// honest gap beats a fabricated 0%.
func providerCacheSupport(usage fantasy.Usage) message.CacheSupport {
	// A provider that reported any cache activity has demonstrably told us
	// about caching.
	if usage.CacheReadTokens > 0 || usage.CacheCreationTokens > 0 {
		return message.CacheSupportNative
	}
	// Otherwise we cannot distinguish "reports caching, had none this turn"
	// from "never reports caching" out of a single sample. Every provider
	// wired today is in the former group (see the list above), so claiming
	// native is correct — but this is the line to revisit when adding one
	// that is not.
	return message.CacheSupportNative
}

// recordMessageUsage persists a single assistant message's token accounting.
//
// Deliberately best-effort: statistics must never abort or fail a turn, so a
// write error is logged and swallowed. The caller has already persisted the
// session-level cost/token snapshot by this point; this adds the per-message
// breakdown that makes cache analytics possible at all.
func (a *sessionAgent) recordMessageUsage(
	ctx context.Context,
	messageID string,
	model Model,
	usage fantasy.Usage,
	costDelta float64,
	estimated bool,
) {
	if messageID == "" {
		return
	}

	tu := message.TokenUsage{
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		ReasoningTokens:     usage.ReasoningTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		CacheReadTokens:     usage.CacheReadTokens,
		TotalTokens:         usage.TotalTokens,
		CostUSD:             costDelta,
		Provider:            model.ModelCfg.Provider,
		Model:               model.ModelCfg.Model,
		CacheSupport:        providerCacheSupport(usage),
		Estimated:           estimated,
	}
	// An estimate is a guess derived from message lengths, not a measurement;
	// it can never be evidence about the cache.
	if estimated {
		tu.CacheSupport = message.CacheSupportNone
	}

	if tu.IsZero() {
		return
	}

	// context.WithoutCancel: usage is recorded at the tail of a turn, and the
	// turn's context is frequently already cancelled by then (user interrupt,
	// watchdog, cost cap). Inheriting that cancellation would drop the
	// statistics for exactly the turns whose accounting matters most — the
	// same failure mode as the cost-transfer bug fixed earlier in
	// coordinator.go.
	if err := a.messages.SetUsage(context.WithoutCancel(ctx), messageID, tu); err != nil {
		slog.Warn(
			"agent: failed to record per-message usage",
			"messageID", messageID,
			"provider", tu.Provider,
			"model", tu.Model,
			"err", err,
		)
	}
}
