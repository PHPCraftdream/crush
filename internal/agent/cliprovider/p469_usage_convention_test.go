package cliprovider

import (
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// Task #469 — the token-accounting convention, pinned against REAL output
// lines captured from each CLI on 2026-08-16.
//
// Every literal below was copied from an actual run, not hand-written, so a
// change in a CLI's wire format shows up here as a failing test rather than as
// silently wrong statistics. Versions: claude 2.1.197, codex-cli 0.147.0,
// gemini 0.55.1.
//
// The property under test is the one downstream code depends on
// (internal/agent/agent.go:4517 cost, :4544 PromptTokens):
//
//	InputTokens, CacheReadTokens and CacheCreationTokens are DISJOINT, and
//	their sum is the true prompt size.
//
// Getting this wrong is not cosmetic. Two of the three parsers were wrong
// before this change, in opposite directions:
//   - codex double-counted every cached token (input already included them)
//   - claude folded cache into input, destroying the breakdown entirely

// promptTotalOf is the invariant: prompt size reconstructed from the
// normalized, disjoint fields.
func promptTotalOf(u fantasy.Usage) int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

func TestClaudeUsage_RealLine_InputIsExclusiveOfCache(t *testing.T) {
	// Captured from: claude --model haiku -p "say OK" --output-format json
	line := []byte(`{"type":"result","subtype":"success","is_error":false,` +
		`"usage":{"input_tokens":10,"cache_creation_input_tokens":6203,` +
		`"cache_read_input_tokens":17298,"output_tokens":157}}`)

	u, ok := claudeParseUsageLine(line)
	require.True(t, ok, "the real claude result line must be recognized as usage")

	require.Equal(t, int64(10), u.InputTokens,
		"claude's input_tokens is already exclusive of cache; it must be passed through untouched")
	require.Equal(t, int64(6203), u.CacheCreationTokens)
	require.Equal(t, int64(17298), u.CacheReadTokens)
	require.Equal(t, int64(157), u.OutputTokens)

	// 10 + 6203 + 17298 — the same prompt total the old fold produced, but now
	// recoverable as three separate numbers.
	require.Equal(t, int64(23511), promptTotalOf(u))
	require.Equal(t, int64(23511+157), u.TotalTokens)
}

func TestCodexUsage_RealLine_InputIsInclusiveAndMustNotDoubleCount(t *testing.T) {
	// Captured from: codex exec --json --skip-git-repo-check -m gpt-5.6-terra
	line := []byte(`{"type":"turn.completed","usage":{"input_tokens":16856,` +
		`"cached_input_tokens":6912,"cache_write_input_tokens":0,` +
		`"output_tokens":5,"reasoning_output_tokens":0}}`)

	u, ok := codexParseUsageLine(line)
	require.True(t, ok)

	// THE regression this test exists for: the old code returned
	// InputTokens = 16856 + 6912 = 23768, counting the cached tokens twice.
	require.NotEqual(t, int64(23768), u.InputTokens,
		"cached tokens are already inside input_tokens; adding them double-counts")
	require.Equal(t, int64(16856-6912), u.InputTokens)
	require.Equal(t, int64(6912), u.CacheReadTokens)
	require.Equal(t, int64(5), u.OutputTokens)

	// The prompt total must equal codex's own input_tokens exactly.
	require.Equal(t, int64(16856), promptTotalOf(u),
		"reconstructed prompt size must match the CLI's own input_tokens")
}

func TestCodexUsage_CapturesCacheWriteAndReasoning(t *testing.T) {
	// Same shape, with the two fields that codexEvent used to omit set
	// non-zero, so their plumbing is exercised rather than assumed.
	line := []byte(`{"type":"turn.completed","usage":{"input_tokens":1000,` +
		`"cached_input_tokens":600,"cache_write_input_tokens":150,` +
		`"output_tokens":20,"reasoning_output_tokens":12}}`)

	u, ok := codexParseUsageLine(line)
	require.True(t, ok)

	require.Equal(t, int64(150), u.CacheCreationTokens, "cache_write_input_tokens must not be dropped")
	require.Equal(t, int64(12), u.ReasoningTokens, "reasoning_output_tokens must not be dropped")
	require.Equal(t, int64(1000-600-150), u.InputTokens)
	require.Equal(t, int64(1000), promptTotalOf(u))
}

func TestGeminiUsage_RealLine_CacheIsNoLongerDiscarded(t *testing.T) {
	// Captured from: gemini -m gemini-3.5-flash --skip-trust -p "say OK"
	//                       --output-format stream-json
	line := []byte(`{"type":"result","timestamp":"2026-08-16T09:52:06.141Z",` +
		`"status":"success","stats":{"total_tokens":12898,"input_tokens":12601,` +
		`"output_tokens":1,"cached":8148,"input":4453,"duration_ms":25076,"tool_calls":0}}`)

	u, ok := geminiParseUsageLine(line)
	require.True(t, ok)

	require.Equal(t, int64(8148), u.CacheReadTokens,
		"gemini does report cache reads; they used to be dropped at unmarshal")
	// The CLI itself publishes the exclusive input (4453), so the subtraction
	// is checkable against the provider's own number rather than only against
	// our arithmetic.
	require.Equal(t, int64(4453), u.InputTokens,
		"derived exclusive input must match the `input` field gemini emits alongside")
	require.Equal(t, int64(12601), promptTotalOf(u))

	// gemini's own total (12898) is LARGER than input_tokens+output_tokens
	// (12602): it also counts thinking tokens the stats block never itemizes.
	// Recomputing the total would silently drop those 296 tokens, so the
	// provider's figure is kept verbatim.
	require.Equal(t, int64(12898), u.TotalTokens,
		"the provider's own total_tokens must be preserved, not recomputed")
	require.Greater(t, u.TotalTokens, u.InputTokens+u.CacheReadTokens+u.OutputTokens,
		"sanity: this line is only meaningful because gemini's total exceeds the itemized parts")
}

// TestUsageNormalize_ClampsInconsistentProvider guards the arithmetic against
// a provider that reports more cached tokens than total input. Without the
// clamp this produces a negative InputTokens, which would SHRINK
// PromptTokens (= InputTokens + CacheReadTokens) and push auto-summarization
// later — a silent failure far from its cause.
func TestUsageNormalize_ClampsInconsistentProvider(t *testing.T) {
	raw := rawUsage{
		input:              100,
		cacheRead:          500,
		output:             7,
		inputIncludesCache: true,
		reportsCache:       true,
	}
	u := raw.normalize()
	require.GreaterOrEqual(t, u.InputTokens, int64(0), "InputTokens must never go negative")
	require.Equal(t, int64(0), u.InputTokens)
}

// TestUsageParsers_IgnoreNonUsageLines keeps the parsers from overwriting a
// real reading with zeroes: Stream calls ParseUsageLine on EVERY line and
// assigns whenever it returns true (provider.go's scan loop).
func TestUsageParsers_IgnoreNonUsageLines(t *testing.T) {
	for name, tc := range map[string]struct {
		parse func([]byte) (fantasy.Usage, bool)
		line  string
	}{
		"claude/non-result":  {claudeParseUsageLine, `{"type":"assistant","message":{"content":[]}}`},
		"claude/zero-usage":  {claudeParseUsageLine, `{"type":"result","usage":{"input_tokens":0,"output_tokens":0}}`},
		"codex/non-turn":     {codexParseUsageLine, `{"type":"item.completed","item":{"type":"agent_message","text":"hi"}}`},
		"codex/zero-usage":   {codexParseUsageLine, `{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":0}}`},
		"gemini/non-result":  {geminiParseUsageLine, `{"type":"message","role":"assistant","content":"OK","delta":true}`},
		"gemini/zero-usage":  {geminiParseUsageLine, `{"type":"result","status":"success","stats":{"total_tokens":0,"input_tokens":0,"output_tokens":0}}`},
		"any/not-valid-json": {claudeParseUsageLine, `not json at all`},
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := tc.parse([]byte(tc.line))
			require.False(t, ok, "line must not be reported as usage")
		})
	}
}
