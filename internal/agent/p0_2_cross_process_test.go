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
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestP0_2_CrossProcessInterrupt_RowRecreatedOnFailure is a regression test for Problem 2:
// cross-process interrupt inject must not lose pending_injects rows when detached run
// fails even after retries. The row is deleted immediately by ConsumeInterruptInject,
// then recreated by startDetachedRun if it fails.
//
// This test FAILS when the recreate logic is removed or broken, because the
// pending_injects row is permanently lost after a failed detached run.
func TestP0_2_CrossProcessInterrupt_RowRecreatedOnFailure(t *testing.T) {
	t.Parallel()

	// Set up a mock provider that ALWAYS returns SessionLockBusyError,
	// simulating an OS lock held by another process.
	busyLockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't respond at all — the run will fail to acquire OS lock.
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

	coord := &coordinator{
		cfg:          &config.ConfigStore{},
		sessions:     env.sessions,
		messages:     env.messages,
		currentAgent: sessionAgent,
	}

	ctx := context.Background()

	// Create the session.
	sess, err := env.sessions.Create(ctx, "p0-2-cross-process-interrupt-test")
	require.NoError(t, err)

	// Create a user message (simulating `crush sessions inject`).
	msg, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "interrupted message"}},
	})
	require.NoError(t, err)

	// Create a pending_injects row (simulating the CLI creating it).
	inject := session.PendingInject{
		ID:        "test-inject-id-recreate-" + sess.ID,
		SessionID: sess.ID, // Use actual session ID from DB
		MessageID: msg.ID,
		Content:   msg.FullText(),
		Interrupt: true,
	}
	err = env.sessions.CreatePendingInject(ctx, inject)
	require.NoError(t, err)

	// Verify the row exists before the interrupt.
	pi, err := env.sessions.ConsumeInterruptInject(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, pi, "pending_injects row should exist before interrupt")
	// The row was deleted by ConsumeInterruptInject, so recreate it for the test.
	err = env.sessions.CreatePendingInject(ctx, inject)
	require.NoError(t, err)

	// Build the call manually (simulating requeueInterruptMessage).
	// We can't use coord.buildCall because it requires full initialization.
	call := SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    msg.FullText(),
		// ExistingMessageID: msg.ID,
		// InjectID:          inject.ID,
	}
	call.ExistingMessageID = msg.ID
	call.InjectID = inject.ID

	// Acquire the OS lock for this session to force failure.
	// This simulates another process holding the lock.
	lock, err := session.TryAcquireSessionLock(env.workingDir, sess.ID)
	require.NoError(t, err, "should acquire OS lock to force failure")
	defer lock.Release()

	// Call startDetachedRun directly. It should:
	// 1. Delete the row at start
	// 2. Fail to acquire OS lock (all retries)
	// 3. Recreate the row
	coord.startDetachedRun(ctx, call)

	// Wait for detached goroutine to complete (5 retries + backoff: ~1.6s total).
	time.Sleep(2 * time.Second)

	// With the fix, a row should be recreated (with a new ID to avoid UNIQUE constraint).
	// We don't check the exact ID (it's new), just that a row exists.
	pi, err = env.sessions.ConsumeInterruptInject(ctx, sess.ID)
	if err != nil {
		t.Logf("Error consuming after recreate: %v", err)
	}
	if pi == nil {
		t.Fatalf("Row was not recreated. This means the fix is broken - pending_injects row was permanently lost after detached run failure.")
	}
	require.NotNil(t, pi, "pending_injects row should be recreated after detached run failure")
	require.Equal(t, sess.ID, pi.SessionID, "recreated row should have the same session")
	require.Equal(t, msg.ID, pi.MessageID, "recreated row should reference the same message")
	require.True(t, pi.Interrupt, "recreated row should be an interrupt")
}
