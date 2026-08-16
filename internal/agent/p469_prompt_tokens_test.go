package agent

import (
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// Task #469 follow-up — PromptTokens must count cache-WRITE tokens.
//
// InputTokens, CacheReadTokens and CacheCreationTokens are three disjoint
// classes (internal/agent/cliprovider/usage.go), so the prompt is their sum.
// updateSessionTokenCounters previously summed only the first two, dropping
// cache-creation entirely.
//
// Why this matters beyond bookkeeping: session.PromptTokens feeds the
// remaining-context calculation that decides when to auto-summarize
// (agent.go's `usedTokens := CompletionTokens + PromptTokens` checks against
// CatwalkCfg.ContextWindow). Understating the prompt makes compaction fire
// LATE, which is the failure direction that overruns the context window
// rather than the one that merely wastes a summarization.
//
// REVERT CHECK: drop `+ usage.CacheCreationTokens` from
// updateSessionTokenCounters and TestPromptTokens_IncludesCacheCreation fails.

func TestPromptTokens_IncludesCacheCreation(t *testing.T) {
	// Real numbers from a measured claude run (claude 2.1.197): a cold cache
	// writes most of the prompt, so cache-creation dominates.
	usage := fantasy.Usage{
		InputTokens:         5842,
		CacheCreationTokens: 16984,
		CacheReadTokens:     0,
		OutputTokens:        4,
	}

	var sess session.Session
	updateSessionTokenCounters(&sess, usage)

	require.Equal(t, int64(22826), sess.PromptTokens,
		"prompt is input + cache-read + cache-write; dropping cache-write understated this turn by 74%%")
	require.Equal(t, int64(4), sess.CompletionTokens)
}

func TestPromptTokens_WarmCacheStillSumsAllThree(t *testing.T) {
	// The same conversation once the cache is warm: now cache-READ dominates
	// and a small slice is being written. Both must count.
	usage := fantasy.Usage{
		InputTokens:         10,
		CacheCreationTokens: 6203,
		CacheReadTokens:     17298,
		OutputTokens:        157,
	}

	var sess session.Session
	updateSessionTokenCounters(&sess, usage)

	require.Equal(t, int64(23511), sess.PromptTokens)
}

// TestPromptTokens_ZeroUsageDoesNotClobber preserves the upstream behaviour
// this function exists for: a partial-zero usage chunk must not overwrite
// accumulated counters with zero.
func TestPromptTokens_ZeroUsageDoesNotClobber(t *testing.T) {
	sess := session.Session{PromptTokens: 1000, CompletionTokens: 200}

	updateSessionTokenCounters(&sess, fantasy.Usage{})

	require.Equal(t, int64(1000), sess.PromptTokens, "an empty usage chunk must not reset the counter")
	require.Equal(t, int64(200), sess.CompletionTokens)
}

// TestPromptTokens_ExclusiveProviderShapeIsUnchanged guards the OpenAI-style
// shape (zai and friends): input already excludes cache, cache-creation is
// always zero, so the sum must equal the provider's own prompt_tokens.
func TestPromptTokens_ExclusiveProviderShapeIsUnchanged(t *testing.T) {
	// prompt_tokens = 12601, of which 8148 were cached.
	usage := fantasy.Usage{
		InputTokens:     4453,
		CacheReadTokens: 8148,
		OutputTokens:    1,
	}

	var sess session.Session
	updateSessionTokenCounters(&sess, usage)

	require.Equal(t, int64(12601), sess.PromptTokens,
		"providers with no cache-write concept must be unaffected by the fix")
}
