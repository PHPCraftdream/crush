package cliprovider

import (
	"fmt"
	"log/slog"

	"charm.land/fantasy"
)

// The canonical token-accounting convention, enforced here and nowhere else.
//
// fantasy.Usage.InputTokens counts ONLY tokens billed as fresh input. Tokens
// served from the provider's prompt cache live in CacheReadTokens; tokens
// written INTO the cache live in CacheCreationTokens. The three are disjoint,
// so the full prompt size is their sum. Downstream code relies on this:
//
//	internal/agent/agent.go:4544  PromptTokens = InputTokens + CacheReadTokens
//	internal/agent/agent.go:4517  cost = in*InputTokens + inCached*CacheCreation
//	                                     + outCached*CacheRead + out*OutputTokens
//
// The CLIs do NOT agree on this. Measured 2026-08-16 by running each binary
// repeatedly against a fixed prompt and watching which counter moved:
//
//   - claude 2.1.197 — EXCLUSIVE. input_tokens held at 10 across three runs
//     while cache_creation/cache_read shifted (6203+17298 -> 0+23501). The
//     prompt total is input + cache_creation + cache_read.
//
//   - codex 0.147.0 — INCLUSIVE. input_tokens held at 16856 across three runs
//     while cached_input_tokens moved (6912 -> 5888). Had it been exclusive it
//     would have risen when the cache share fell. input_tokens IS the total,
//     exactly like OpenAI's prompt_tokens.
//
//   - gemini 0.55.1 — INCLUSIVE, and self-evidencing: it emits both
//     input_tokens (12601) and a separate exclusive input (4453) alongside
//     cached (8148), and 4453+8148 == 12601.
//
// Mixing those conventions in one field is what rawUsage exists to prevent.

// rawUsage is a CLI's own numbers, in that CLI's own convention. Parsers fill
// it verbatim from the wire and let normalize() do the arithmetic, so the
// inclusive->exclusive subtraction lives in exactly one place.
type rawUsage struct {
	// input is the CLI's prompt-token counter. Whether it already contains
	// cacheRead/cacheCreation is declared by inputIncludesCache.
	input  int64
	output int64

	// cacheCreation/cacheRead are cache-write and cache-hit tokens. Zero is
	// ambiguous on its own — a CLI that never reports caching and a genuine
	// cache miss both land here — which is why callers set reportsCache.
	cacheCreation int64
	cacheRead     int64

	reasoning int64

	// inputIncludesCache is true when the CLI folds cache tokens into its
	// input counter (codex, gemini) and false when input is already the
	// uncached remainder (claude).
	inputIncludesCache bool

	// reportsCache records whether this CLI emits cache counters at all, so a
	// downstream "0% cache hit" can be distinguished from "we were never
	// told". Kept even though all three current CLIs report caching: an older
	// CLI build, or a new provider, can still be silent.
	reportsCache bool

	// totalOverride is the provider's OWN total-token figure, when it
	// publishes one. Prefer it over recomputing: gemini's total_tokens is
	// larger than input_tokens+output_tokens (measured: 12898 vs 12602)
	// because it also counts thinking tokens that the stream-json stats block
	// never itemizes. Recomputing would silently discard them. Zero means the
	// provider gave no total and one is derived instead.
	totalOverride int64

	// model, when non-empty, is the model id the CLI says actually served the
	// request. CLIs resolve aliases server-side (gemini-3-flash now answers as
	// gemini-3.5-flash), so this can differ from what we asked for.
	model string
}

// promptTotal is the full prompt size regardless of which convention the CLI
// used, for logging and for the invariant check in normalize.
func (r rawUsage) promptTotal() int64 {
	if r.inputIncludesCache {
		return r.input
	}
	return r.input + r.cacheCreation + r.cacheRead
}

// normalize converts a CLI's raw counters into the canonical exclusive
// convention documented at the top of this file.
func (r rawUsage) normalize() fantasy.Usage {
	inputExclusive := r.input
	if r.inputIncludesCache {
		inputExclusive = r.input - r.cacheRead - r.cacheCreation
		// A provider that reports cache tokens exceeding its own prompt total
		// is self-inconsistent; clamping keeps a bad upstream number from
		// turning into a negative that would silently shrink PromptTokens (and
		// with it the auto-summarization trigger) instead of just being wrong.
		if inputExclusive < 0 {
			slog.Warn(
				"cliprovider: provider reported more cached tokens than total input; clamping",
				"input", r.input,
				"cacheRead", r.cacheRead,
				"cacheCreation", r.cacheCreation,
				"model", r.model,
			)
			inputExclusive = 0
		}
	}

	// Prefer the provider's own total; derive only when it gives none. Derived
	// from promptTotal rather than inputExclusive so the result is identical
	// under both conventions.
	total := r.totalOverride
	if total == 0 {
		total = r.promptTotal() + r.output
	}

	return fantasy.Usage{
		InputTokens:         inputExclusive,
		OutputTokens:        r.output,
		ReasoningTokens:     r.reasoning,
		CacheCreationTokens: r.cacheCreation,
		CacheReadTokens:     r.cacheRead,
		TotalTokens:         total,
	}
}

// isEmpty reports whether the CLI gave us nothing worth recording. Parsers use
// it to keep returning false for non-usage lines rather than overwriting a
// real earlier reading with zeroes.
func (r rawUsage) isEmpty() bool {
	return r.promptTotal() == 0 && r.output == 0
}

// logUsage emits the operator-facing token line, including the cache-hit
// share. This used to be computed inline in the Claude parser and thrown away
// after logging; it is now derived from the normalized numbers so every CLI
// reports it the same way.
func logUsage(binary string, r rawUsage, u fantasy.Usage) {
	attrs := []any{
		"binary", binary,
		"input", u.InputTokens,
		"cache_create", u.CacheCreationTokens,
		"cache_read", u.CacheReadTokens,
		"output", u.OutputTokens,
		"prompt_total", r.promptTotal(),
	}
	if r.model != "" {
		attrs = append(attrs, "resolved_model", r.model)
	}
	if !r.reportsCache {
		attrs = append(attrs, "cache_hit_pct", "n/a")
	} else if total := r.promptTotal(); total > 0 {
		attrs = append(attrs, "cache_hit_pct",
			fmt.Sprintf("%.1f%%", float64(u.CacheReadTokens)/float64(total)*100))
	}
	slog.Info("cliprovider: token usage", attrs...)
}
