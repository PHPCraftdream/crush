package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestP0_2_RetryExhaustion_QueuesCall is a regression test for Problem 1:
// restartOrphanedWithRetry must queue calls after retry exhaustion to prevent
// data loss when OS lock is held for longer than the retry window (~1.6s).
//
// This test FAILS when mb.queue(call) is removed or commented out from the
// retry exhaustion path, because the orphaned call disappears completely.
func TestP0_2_RetryExhaustion_QueuesCall(t *testing.T) {
	t.Parallel()

	// Set up a mock provider that ALWAYS fails (simulates OS lock contention).
	busyLockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't respond at all — the run will fail.
	}))
	t.Cleanup(busyLockSrv.Close)

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(busyLockSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}

	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a probe",
		DataDirectory:        env.workingDir,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sessionAgent := sa.(*sessionAgent)

	sessionID := "p0-2-retry-exhaustion-test"
	ctx := context.Background()

	// Create the session.
	sess, err := env.sessions.Create(ctx, sessionID)
	require.NoError(t, err)

	// Add a seed message.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test"}},
	})
	require.NoError(t, err)

	// Call that will be orphaned and retried.
	orphanedCall := SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "orphaned call",
	}

	// Start a detached run that will exhaust retries.
	sessionAgent.restartOrphanedWithRetry([]SessionAgentCall{orphanedCall})

	// Wait for all retry attempts to complete (5 attempts + backoff: ~1.6s total).
	time.Sleep(2 * time.Second)

	// With the fix, the call should be queued in mb.submitted.
	// Without the fix (commenting out mb.queue(call)), submitted is empty
	// and the call is lost.
	mb := sessionAgent.getMailbox(sessionID)
	mb.mu.Lock()
	submittedCalls := make([]SessionAgentCall, len(mb.submitted))
	copy(submittedCalls, mb.submitted)
	mb.mu.Unlock()
	require.Len(t, submittedCalls, 1, "orphaned call should be queued after retry exhaustion")
	require.Equal(t, orphanedCall.SessionID, submittedCalls[0].SessionID)
	require.Equal(t, orphanedCall.Prompt, submittedCalls[0].Prompt)
}
