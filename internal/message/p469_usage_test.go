package message

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// Task #469 — per-message usage storage and the analytics built on it.
//
// The properties worth defending here are the honesty ones. Token sums are
// easy; what is easy to get silently wrong is presenting a number that looks
// authoritative but was computed over partial or meaningless data:
//
//   - a cache ratio for a provider that never reports caching (reads as a
//     confident "0% hit" instead of "unknown")
//   - a session total computed over 3 of 400 messages with no indication
//   - an aggregate across two models claiming one model's cache visibility

func TestCacheHitRatio_RefusesToAnswerWithoutProviderSupport(t *testing.T) {
	u := TokenUsage{
		InputTokens:  1000,
		CacheSupport: CacheSupportNone,
	}
	_, ok := u.CacheHitRatio()
	require.False(t, ok,
		"a provider that does not report caching must yield no ratio; 0%% would be indistinguishable from a real miss")

	// Same numbers, but the provider does report caching -> answerable.
	u.CacheSupport = CacheSupportNative
	u.CacheReadTokens = 3000
	ratio, ok := u.CacheHitRatio()
	require.True(t, ok)
	require.InDelta(t, 0.75, ratio, 1e-9, "3000 cached of a 4000-token prompt")
}

func TestCacheHitRatio_RefusesOnEmptyPrompt(t *testing.T) {
	u := TokenUsage{CacheSupport: CacheSupportNative}
	_, ok := u.CacheHitRatio()
	require.False(t, ok, "no prompt means no ratio, not a division by zero")
}

func TestPromptTokens_SumsTheThreeDisjointClasses(t *testing.T) {
	u := TokenUsage{InputTokens: 10, CacheReadTokens: 17298, CacheCreationTokens: 6203}
	require.Equal(t, int64(23511), u.PromptTokens(),
		"prompt size is input + cache-read + cache-create; these are disjoint by convention")
}

func TestAdd_DegradesCacheSupportToTheWeakestContributor(t *testing.T) {
	native := TokenUsage{InputTokens: 100, CacheReadTokens: 900, CacheSupport: CacheSupportNative, Provider: "p", Model: "a"}
	silent := TokenUsage{InputTokens: 500, CacheSupport: CacheSupportNone, Provider: "p", Model: "b"}

	sum := native.Add(silent)
	require.Equal(t, CacheSupportNone, sum.CacheSupport,
		"mixing a cache-reporting model with a silent one must not present the silent half's zeros as measured")

	_, ok := sum.CacheHitRatio()
	require.False(t, ok, "the degraded aggregate must refuse to state a ratio")
}

func TestAdd_ClearsIdentityWhenModelsDisagree(t *testing.T) {
	a := TokenUsage{InputTokens: 1, Provider: "zai", Model: "glm-5.3", CacheSupport: CacheSupportNative}
	b := TokenUsage{InputTokens: 2, Provider: "local-cli", Model: "cli-claude-sonnet", CacheSupport: CacheSupportNative}

	sum := a.Add(b)
	require.Empty(t, sum.Provider, "a sum across two providers belongs to neither")
	require.Empty(t, sum.Model)
	require.Equal(t, int64(3), sum.InputTokens)
}

func TestAdd_KeepsIdentityWhenSameModel(t *testing.T) {
	a := TokenUsage{InputTokens: 1, Provider: "zai", Model: "glm-5.3", CacheSupport: CacheSupportNative}
	b := TokenUsage{InputTokens: 2, Provider: "zai", Model: "glm-5.3", CacheSupport: CacheSupportNative}

	sum := a.Add(b)
	require.Equal(t, "zai", sum.Provider)
	require.Equal(t, "glm-5.3", sum.Model)
	require.Equal(t, CacheSupportNative, sum.CacheSupport)
}

func TestAdd_EstimatedIsSticky(t *testing.T) {
	measured := TokenUsage{InputTokens: 100, CacheSupport: CacheSupportNative}
	guessed := TokenUsage{InputTokens: 100, Estimated: true, CacheSupport: CacheSupportNone}

	require.True(t, measured.Add(guessed).Estimated,
		"one estimated contributor makes the whole total an estimate")
}

func TestAdd_OntoEmptyAccumulatorAdoptsIdentity(t *testing.T) {
	// The common aggregation shape: start from the zero value and fold.
	var acc TokenUsage
	acc = acc.Add(TokenUsage{InputTokens: 5, Provider: "zai", Model: "glm-5.3", CacheSupport: CacheSupportNative})

	require.Equal(t, "zai", acc.Provider,
		"folding onto a zero accumulator must not look like a disagreement and blank the identity")
	require.Equal(t, CacheSupportNative, acc.CacheSupport)
}

func TestUsageReport_CoverageReportsPartialData(t *testing.T) {
	r := UsageReport{
		ByModel: []ModelUsage{
			{Messages: 3, Usage: TokenUsage{InputTokens: 30, CacheSupport: CacheSupportNative}},
		},
		MissingUsage: 397,
	}
	ratio, ok := r.Coverage()
	require.True(t, ok)
	require.InDelta(t, 3.0/400.0, ratio, 1e-9,
		"a statistic over 3 of 400 messages must be reportable as such, not presented as the session's")
	require.Equal(t, int64(3), r.Messages())
}

func TestUsageReport_CoverageUnanswerableWhenNothingRecorded(t *testing.T) {
	_, ok := UsageReport{}.Coverage()
	require.False(t, ok, "no messages at all means no coverage figure, not 0%")
}

func TestUsageReport_TotalFoldsAllGroups(t *testing.T) {
	r := UsageReport{ByModel: []ModelUsage{
		{Messages: 1, Usage: TokenUsage{InputTokens: 10, CacheReadTokens: 90, CostUSD: 0.5, Provider: "zai", Model: "a", CacheSupport: CacheSupportNative}},
		{Messages: 1, Usage: TokenUsage{InputTokens: 20, CacheReadTokens: 80, CostUSD: 0.25, Provider: "zai", Model: "b", CacheSupport: CacheSupportNative}},
	}}
	total := r.Total()
	require.Equal(t, int64(30), total.InputTokens)
	require.Equal(t, int64(170), total.CacheReadTokens)
	require.InDelta(t, 0.75, total.CostUSD, 1e-9)
	require.Empty(t, total.Model, "a cross-model total names no single model")
}

func TestIsZero_DistinguishesNothingRecordedFromMeasuredZero(t *testing.T) {
	require.True(t, TokenUsage{}.IsZero())
	// A turn that genuinely produced no output but did consume a prompt is
	// NOT nothing — it must be storable.
	require.False(t, TokenUsage{InputTokens: 1}.IsZero())
	// Cost alone counts too (flat-rate providers report tokens but zero cost;
	// an override-cost provider can do the reverse).
	require.False(t, TokenUsage{CostUSD: 0.01}.IsZero())
}

// ── Storage round trip ───────────────────────────────────────────────────────

// TestSetUsage_RoundTripsThroughDBAndAggregates is the end-to-end proof: usage
// written per message comes back grouped by the model that produced it, with
// the cache split intact. Without this, everything above is arithmetic on
// values that might never survive the DB.
func TestSetUsage_RoundTripsThroughDBAndAggregates(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()
	const sessionID = "p469-session"

	mk := func(model string) Message {
		m, err := svc.Create(ctx, sessionID, CreateMessageParams{
			Role:     Assistant,
			Parts:    []ContentPart{TextContent{Text: "hi"}},
			Provider: "local-cli",
			Model:    model,
		})
		require.NoError(t, err)
		return m
	}

	a1, a2, b1 := mk("cli-claude-sonnet"), mk("cli-claude-sonnet"), mk("cli-codex-sol")

	require.NoError(t, svc.SetUsage(ctx, a1.ID, TokenUsage{
		InputTokens: 10, CacheReadTokens: 90, CacheCreationTokens: 5, OutputTokens: 3,
		TotalTokens: 108, CostUSD: 0.10,
		Provider: "local-cli", Model: "cli-claude-sonnet", CacheSupport: CacheSupportNative,
	}))
	require.NoError(t, svc.SetUsage(ctx, a2.ID, TokenUsage{
		InputTokens: 20, CacheReadTokens: 80, OutputTokens: 7,
		TotalTokens: 107, CostUSD: 0.20,
		Provider: "local-cli", Model: "cli-claude-sonnet", CacheSupport: CacheSupportNative,
	}))
	require.NoError(t, svc.SetUsage(ctx, b1.ID, TokenUsage{
		InputTokens: 1000, CacheReadTokens: 0, OutputTokens: 5,
		TotalTokens: 1005, CostUSD: 0,
		Provider: "local-cli", Model: "cli-codex-sol", CacheSupport: CacheSupportNative,
	}))

	report, err := svc.UsageBySession(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, report.ByModel, 2, "one group per model that produced messages")

	byModel := map[string]ModelUsage{}
	for _, m := range report.ByModel {
		byModel[m.Usage.Model] = m
	}

	sonnet := byModel["cli-claude-sonnet"]
	require.Equal(t, int64(2), sonnet.Messages)
	require.Equal(t, int64(30), sonnet.Usage.InputTokens)
	require.Equal(t, int64(170), sonnet.Usage.CacheReadTokens)
	require.Equal(t, int64(5), sonnet.Usage.CacheCreationTokens)
	require.InDelta(t, 0.30, sonnet.Usage.CostUSD, 1e-9)

	// 170 cached out of a 205-token prompt (30 + 170 + 5).
	ratio, ok := sonnet.Usage.CacheHitRatio()
	require.True(t, ok)
	require.InDelta(t, 170.0/205.0, ratio, 1e-9)

	// The uncached model must stay separate rather than dragging the cached
	// model's ratio down inside one blended number.
	codex := byModel["cli-codex-sol"]
	require.Equal(t, int64(1), codex.Messages)
	require.Equal(t, int64(0), codex.Usage.CacheReadTokens)
}

// TestSetUsage_MissingUsageIsCountedNotAssumedZero pins the coverage signal:
// an assistant message with no usage recorded must be reported as missing, not
// silently folded in as a zero-token message.
func TestSetUsage_MissingUsageIsCountedNotAssumedZero(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()
	const sessionID = "p469-coverage"

	recorded, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role: Assistant, Parts: []ContentPart{TextContent{Text: "a"}},
	})
	require.NoError(t, err)
	// Two more assistant messages that never get usage.
	for range 2 {
		_, err := svc.Create(ctx, sessionID, CreateMessageParams{
			Role: Assistant, Parts: []ContentPart{TextContent{Text: "b"}},
		})
		require.NoError(t, err)
	}

	require.NoError(t, svc.SetUsage(ctx, recorded.ID, TokenUsage{
		InputTokens: 5, TotalTokens: 5, Provider: "zai", Model: "glm-5.3", CacheSupport: CacheSupportNative,
	}))

	report, err := svc.UsageBySession(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, int64(2), report.MissingUsage)
	require.Equal(t, int64(1), report.Messages())

	ratio, ok := report.Coverage()
	require.True(t, ok)
	require.InDelta(t, 1.0/3.0, ratio, 1e-9)
}

// TestSetUsage_IgnoresEmptyUsage keeps a no-op write from creating a row that
// would then count as "recorded" and inflate coverage while carrying nothing.
func TestSetUsage_IgnoresEmptyUsage(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()
	const sessionID = "p469-empty"

	m, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role: Assistant, Parts: []ContentPart{TextContent{Text: "a"}},
	})
	require.NoError(t, err)

	require.NoError(t, svc.SetUsage(ctx, m.ID, TokenUsage{}))

	report, err := svc.UsageBySession(ctx, sessionID)
	require.NoError(t, err)
	require.Empty(t, report.ByModel, "an all-zero usage write must not create a recorded row")
	require.Equal(t, int64(1), report.MissingUsage, "it stays counted as missing instead")
}

// TestSetUsage_StoresMeasuredZeroCacheAsRecorded is the counterpart: a turn
// that really had no cache hits must be stored and reported, since "0 hits" is
// a finding, not an absence of data.
func TestSetUsage_StoresMeasuredZeroCacheAsRecorded(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()
	const sessionID = "p469-zero-cache"

	m, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role: Assistant, Parts: []ContentPart{TextContent{Text: "a"}},
	})
	require.NoError(t, err)

	require.NoError(t, svc.SetUsage(ctx, m.ID, TokenUsage{
		InputTokens: 1234, CacheReadTokens: 0, OutputTokens: 7, TotalTokens: 1241,
		Provider: "zai", Model: "glm-5.3", CacheSupport: CacheSupportNative,
	}))

	report, err := svc.UsageBySession(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, report.ByModel, 1)
	require.Equal(t, int64(0), report.MissingUsage)

	ratio, ok := report.ByModel[0].Usage.CacheHitRatio()
	require.True(t, ok, "a measured zero IS answerable — unlike an unsupported provider")
	require.Zero(t, ratio)
}

// ── Period / cross-session aggregation (task #474) ───────────────────────────

// TestUsageByModelInRange_GroupsByProducingModelAcrossSessions is the property
// `sessions cost` cannot provide: it groups by sessions.large_model_id, so a
// session that switched models attributes every token to whichever model it
// ended on. Per-message provenance fixes that.
func TestUsageByModelInRange_GroupsByProducingModelAcrossSessions(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()

	write := func(sessionID, model string, in, cacheRead int64, cost float64) {
		m, err := svc.Create(ctx, sessionID, CreateMessageParams{
			Role: Assistant, Parts: []ContentPart{TextContent{Text: "x"}},
		})
		require.NoError(t, err)
		require.NoError(t, svc.SetUsage(ctx, m.ID, TokenUsage{
			InputTokens: in, CacheReadTokens: cacheRead, TotalTokens: in + cacheRead,
			CostUSD: cost, Provider: "local-cli", Model: model, CacheSupport: CacheSupportNative,
		}))
	}

	// One session that switched models mid-conversation, plus a second
	// session on one of the same models.
	write("sess-a", "model-x", 10, 90, 0.1)
	write("sess-a", "model-y", 20, 80, 0.2)
	write("sess-b", "model-x", 30, 70, 0.3)

	report, err := svc.UsageByModelInRange(ctx, 0, math.MaxInt64)
	require.NoError(t, err)
	require.Len(t, report.ByModel, 2)

	got := map[string]ModelUsage{}
	for _, m := range report.ByModel {
		got[m.Usage.Model] = m
	}

	// model-x spans TWO sessions and must be summed across them.
	require.Equal(t, int64(2), got["model-x"].Messages)
	require.Equal(t, int64(40), got["model-x"].Usage.InputTokens)
	require.Equal(t, int64(160), got["model-x"].Usage.CacheReadTokens)
	require.InDelta(t, 0.4, got["model-x"].Usage.CostUSD, 1e-9)

	// model-y stays its own row even though it shares a session with model-x.
	require.Equal(t, int64(1), got["model-y"].Messages)
	require.Equal(t, int64(20), got["model-y"].Usage.InputTokens)
}

// TestUsageByModelInRange_ChildSessionCostIsNotDoubleCounted pins the reason
// child sessions are deliberately INCLUDED here while `sessions cost` must
// exclude them: TransferChildCostToParent moves a child's cost into the
// PARENT'S sessions.cost column and never rewrites message rows, so each
// message's cost_usd appears exactly once in this aggregate.
func TestUsageByModelInRange_ChildSessionCostIsNotDoubleCounted(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()

	for _, sessionID := range []string{"parent", "child"} {
		m, err := svc.Create(ctx, sessionID, CreateMessageParams{
			Role: Assistant, Parts: []ContentPart{TextContent{Text: "x"}},
		})
		require.NoError(t, err)
		require.NoError(t, svc.SetUsage(ctx, m.ID, TokenUsage{
			InputTokens: 100, TotalTokens: 100, CostUSD: 1.0,
			Provider: "zai", Model: "glm-5.3", CacheSupport: CacheSupportNative,
		}))
	}

	report, err := svc.UsageByModelInRange(ctx, 0, math.MaxInt64)
	require.NoError(t, err)
	require.InDelta(t, 2.0, report.Total().CostUSD, 1e-9,
		"parent + child each contribute their own message cost exactly once")
}

// TestUsageByModelInRange_WindowExcludesOutsideMessages proves the period
// filter actually filters — without this the "over a period" feature could
// silently report all-time numbers.
func TestUsageByModelInRange_WindowExcludesOutsideMessages(t *testing.T) {
	sqlDB, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()

	m, err := svc.Create(ctx, "sess", CreateMessageParams{
		Role: Assistant, Parts: []ContentPart{TextContent{Text: "x"}},
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetUsage(ctx, m.ID, TokenUsage{
		InputTokens: 500, TotalTokens: 500, Provider: "zai", Model: "glm-5.3",
		CacheSupport: CacheSupportNative,
	}))

	// Backdate the row well outside any window we then query.
	_, err = sqlDB.ExecContext(ctx, `UPDATE messages SET created_at = 1000 WHERE id = ?`, m.ID)
	require.NoError(t, err)

	inside, err := svc.UsageByModelInRange(ctx, 0, 2000)
	require.NoError(t, err)
	require.Len(t, inside.ByModel, 1, "the row is inside [0,2000]")

	outside, err := svc.UsageByModelInRange(ctx, 5000, math.MaxInt64)
	require.NoError(t, err)
	require.Empty(t, outside.ByModel, "the row is outside [5000,inf) and must not be counted")
}

// TestUsageByDayInRange_BucketsAndDeclinesToBlendCacheRatios covers the day
// view. A day can span several providers with different cache visibility, so
// the bucket carries no provider/model identity and must NOT offer a ratio.
func TestUsageByDayInRange_BucketsAndDeclinesToBlendCacheRatios(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()

	for range 3 {
		m, err := svc.Create(ctx, "sess", CreateMessageParams{
			Role: Assistant, Parts: []ContentPart{TextContent{Text: "x"}},
		})
		require.NoError(t, err)
		require.NoError(t, svc.SetUsage(ctx, m.ID, TokenUsage{
			InputTokens: 10, CacheReadTokens: 90, TotalTokens: 100, CostUSD: 0.5,
			Provider: "zai", Model: "glm-5.3", CacheSupport: CacheSupportNative,
		}))
	}

	days, err := svc.UsageByDayInRange(ctx, 0, math.MaxInt64)
	require.NoError(t, err)
	require.Len(t, days, 1, "all three messages land on the same day")
	require.Equal(t, int64(3), days[0].Messages)
	require.Equal(t, int64(30), days[0].Usage.InputTokens)
	require.InDelta(t, 1.5, days[0].Usage.CostUSD, 1e-9)
	require.Regexp(t, `^\d{4}-\d{2}-\d{2}$`, days[0].Day)

	_, ok := days[0].Usage.CacheHitRatio()
	require.False(t, ok,
		"a day bucket spans models with differing cache visibility; it must not state a blended ratio")
}

// TestNullUsageRowsNeverDiluteTheCacheRatio is the explicit statement of a
// property the WHERE clauses give us: rows with no usage recorded (every
// message written before this feature existed, plus any turn whose usage
// write failed) carry NULL in the token columns and must be excluded from the
// aggregate entirely.
//
// The danger is not just a wrong total: if a NULL row were folded in as a
// zero, it would enlarge the ratio's DENOMINATOR without contributing cache
// reads, silently dragging the reported hit rate toward zero. On a database
// with thousands of pre-feature messages that would make a perfectly healthy
// cache look broken.
func TestNullUsageRowsNeverDiluteTheCacheRatio(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()
	const sessionID = "p469-null-dilution"

	// One measured message: 900 of a 1000-token prompt served from cache.
	measured, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role: Assistant, Parts: []ContentPart{TextContent{Text: "m"}},
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetUsage(ctx, measured.ID, TokenUsage{
		InputTokens: 100, CacheReadTokens: 900, OutputTokens: 10, TotalTokens: 1010,
		Provider: "zai", Model: "glm-5.3", CacheSupport: CacheSupportNative,
	}))

	// Fifty assistant messages with NO usage at all, as a pre-feature history
	// would look.
	for range 50 {
		_, err := svc.Create(ctx, sessionID, CreateMessageParams{
			Role: Assistant, Parts: []ContentPart{TextContent{Text: "old"}},
		})
		require.NoError(t, err)
	}

	report, err := svc.UsageBySession(ctx, sessionID)
	require.NoError(t, err)

	require.Len(t, report.ByModel, 1, "NULL rows must not form groups of their own")
	require.Equal(t, int64(1), report.Messages(), "only the measured message counts")
	require.Equal(t, int64(50), report.MissingUsage, "the rest are reported as missing, not as zeros")

	ratio, ok := report.ByModel[0].Usage.CacheHitRatio()
	require.True(t, ok)
	require.InDelta(t, 0.9, ratio, 1e-9,
		"90%% must stay 90%%; folding 50 empty rows in as zeros would crush it toward 0")

	// And the coverage figure must expose how thin the sample is.
	cov, ok := report.Coverage()
	require.True(t, ok)
	require.InDelta(t, 1.0/51.0, cov, 1e-9,
		"the caller must be able to say this ratio came from 1 of 51 messages")
}

// TestNullUsageRowsExcludedFromPeriodAggregates is the same property for the
// cross-session views that back `sessions cache --by model|day`.
func TestNullUsageRowsExcludedFromPeriodAggregates(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()

	m, err := svc.Create(ctx, "s1", CreateMessageParams{
		Role: Assistant, Parts: []ContentPart{TextContent{Text: "m"}},
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetUsage(ctx, m.ID, TokenUsage{
		InputTokens: 100, CacheReadTokens: 900, TotalTokens: 1000,
		Provider: "zai", Model: "glm-5.3", CacheSupport: CacheSupportNative,
	}))
	for range 20 {
		_, err := svc.Create(ctx, "s2", CreateMessageParams{
			Role: Assistant, Parts: []ContentPart{TextContent{Text: "old"}},
		})
		require.NoError(t, err)
	}

	byModel, err := svc.UsageByModelInRange(ctx, 0, math.MaxInt64)
	require.NoError(t, err)
	require.Len(t, byModel.ByModel, 1)
	require.Equal(t, int64(1), byModel.Messages())
	require.Equal(t, int64(20), byModel.MissingUsage)
	ratio, ok := byModel.ByModel[0].Usage.CacheHitRatio()
	require.True(t, ok)
	require.InDelta(t, 0.9, ratio, 1e-9)

	days, err := svc.UsageByDayInRange(ctx, 0, math.MaxInt64)
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, int64(1), days[0].Messages,
		"the day bucket must count only the message that actually has usage")
	require.Equal(t, int64(1000), days[0].Usage.InputTokens+days[0].Usage.CacheReadTokens)
}
