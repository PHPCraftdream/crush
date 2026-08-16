package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// Task #481 — a sub-agent's tokens belong to the CHILD session.
//
// Two mechanisms move numbers between a child and its parent, and they are
// deliberately different:
//
//   - COST is transferred upward. session.TransferChildCostToParent charges the
//     delta to the parent's sessions.cost column so an operator sees the true
//     price of a delegation from the parent alone.
//   - TOKENS are NOT. recordMessageUsage writes per-message rows into whichever
//     session owns the message, so a child's tokens stay on the child's rows.
//
// Conflating them would double-count. The cross-session aggregate
// (message.UsageByModelInRange, backing `crush sessions cache`) deliberately
// INCLUDES child sessions precisely because cost transfer never rewrites
// message rows — each message's cost_usd is counted exactly once there. If a
// future change started copying child usage rows onto the parent, that
// aggregate would silently double every delegated turn.
//
// These tests pin the boundary at the storage layer, where it is actually
// enforced, rather than through a full sub-agent dispatch (which would need a
// recorded provider fixture and would test the harness more than the rule).

func TestSubAgentUsage_StaysInChildSession(t *testing.T) {
	env := testEnv(t)
	ctx := t.Context()

	parent, err := env.sessions.Create(ctx, "parent")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(ctx, "tool-call-1", parent.ID, "child")
	require.NoError(t, err)

	write := func(sessionID string, in, cacheRead int64, cost float64) {
		m, err := env.messages.Create(ctx, sessionID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: "x"}},
		})
		require.NoError(t, err)
		require.NoError(t, env.messages.SetUsage(ctx, m.ID, message.TokenUsage{
			InputTokens: in, CacheReadTokens: cacheRead, TotalTokens: in + cacheRead,
			CostUSD: cost, Provider: "zai", Model: "glm-5.3",
			CacheSupport: message.CacheSupportNative,
		}))
	}

	write(parent.ID, 100, 0, 0.10)
	write(child.ID, 5000, 1000, 2.50)

	parentReport, err := env.messages.UsageBySession(ctx, parent.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), parentReport.Messages(),
		"the parent must account for its OWN message only")
	require.Equal(t, int64(100), parentReport.Total().InputTokens,
		"the child's 5000 input tokens must not appear on the parent")
	require.InDelta(t, 0.10, parentReport.Total().CostUSD, 1e-9)

	childReport, err := env.messages.UsageBySession(ctx, child.ID)
	require.NoError(t, err)
	require.Equal(t, int64(5000), childReport.Total().InputTokens)
	require.Equal(t, int64(1000), childReport.Total().CacheReadTokens)
	require.InDelta(t, 2.50, childReport.Total().CostUSD, 1e-9)
}

// TestSubAgentUsage_CostTransferDoesNotDuplicateMessageRows is the important
// half: transferring cost upward must leave the message rows alone, or the
// cross-session aggregate would count a delegated turn twice.
func TestSubAgentUsage_CostTransferDoesNotDuplicateMessageRows(t *testing.T) {
	env := testEnv(t)
	ctx := t.Context()

	parent, err := env.sessions.Create(ctx, "parent")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(ctx, "tool-call-2", parent.ID, "child")
	require.NoError(t, err)

	m, err := env.messages.Create(ctx, child.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "work"}},
	})
	require.NoError(t, err)
	require.NoError(t, env.messages.SetUsage(ctx, m.ID, message.TokenUsage{
		InputTokens: 1000, OutputTokens: 50, TotalTokens: 1050, CostUSD: 1.25,
		Provider: "zai", Model: "glm-5.3", CacheSupport: message.CacheSupportNative,
	}))

	// Charge the child's cost to the parent, the way a finished sub-agent does.
	_, err = env.sessions.IncrementCost(ctx, child.ID, 1.25)
	require.NoError(t, err)
	require.NoError(t, env.sessions.TransferChildCostToParent(ctx, child.ID, parent.ID))

	// The parent's SESSION cost moved...
	freshParent, err := env.sessions.Get(ctx, parent.ID)
	require.NoError(t, err)
	require.InDelta(t, 1.25, freshParent.Cost, 1e-9,
		"cost transfer is the intended mechanism and must still work")

	// ...but no message row followed it.
	parentReport, err := env.messages.UsageBySession(ctx, parent.ID)
	require.NoError(t, err)
	require.Empty(t, parentReport.ByModel,
		"cost transfer must not create usage rows on the parent; the cross-session "+
			"aggregate includes child sessions and would double-count the turn")

	// And the whole-fleet aggregate still sees the cost exactly once.
	all, err := env.messages.UsageByModelInRange(ctx, 0, 1<<62)
	require.NoError(t, err)
	require.InDelta(t, 1.25, all.Total().CostUSD, 1e-9,
		"one delegated turn, one charge in the cross-session total")
	require.Equal(t, int64(1), all.Messages())
}
