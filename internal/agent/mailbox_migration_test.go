package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMailbox_Queue_AndDrainOrReleaseFinal_SingleExecution is the core #308
// regression test: a message queued via mailbox.queue() (the new
// QueueMessage path) during a busy session must be returned by
// drainOrReleaseFinal EXACTLY ONCE — never twice. The pre-#308 risk was
// that a message could end up in BOTH mb.submitted (via the new path) and
// the legacy messageQueue (via the old path), causing drainOrReleaseFinal's
// submitted branch AND its checkLegacy branch to each return it, running
// the message as two separate turns. With the legacy queue removed, there
// is only one queue and one branch.
func TestMailbox_Queue_AndDrainOrReleaseFinal_SingleExecution(t *testing.T) {
	mb := &mailbox{
		state:            mbOwned,
		epoch:            1,
		dispatcherCancel: func() {},
		current:          generation{id: 1, cancel: func() {}},
	}

	queued := SessionAgentCall{SessionID: "s1", Prompt: "queued exactly once"}
	mb.queue(queued)

	// First drain: should find the entry and return it.
	next, hasNext, _, _ := mb.drainOrReleaseFinal(1, nil)
	require.True(t, hasNext, "the queued entry must be returned on the first drain")
	require.Equal(t, queued, next)

	// Second drain: submitted is now empty — no legacy queue to check.
	// This MUST return hasNext=false. If the entry had been double-inserted
	// (the exact bug class this migration closes), a second drain would find
	// it again.
	next2, hasNext2, _, _ := mb.drainOrReleaseFinal(1, nil)
	require.False(t, hasNext2, "the queued entry must NOT be returned a second time — single insertion, single execution")
	require.Equal(t, SessionAgentCall{}, next2)
}

// TestMailbox_Queue_AbandonOwnership_SurvivesForNextOwner verifies that
// work queued via mailbox.queue() survives an abandonOwnership call (which
// now leaves entries in submitted instead of draining them to a legacy
// queue). The next owner that calls submit() + drainOrReleaseFinal picks
// them up.
func TestMailbox_Queue_AbandonOwnership_SurvivesForNextOwner(t *testing.T) {
	mb := &mailbox{
		state:            mbOwned,
		epoch:            1,
		dispatcherCancel: func() {},
		current:          generation{id: 1, cancel: func() {}},
	}

	queued := SessionAgentCall{SessionID: "s1", Prompt: "survives abandonOwnership"}
	mb.queue(queued)

	// Simulate Run's cleanup defer: abandonOwnership ends the era at mbIdle
	// but leaves submitted populated.
	hadWork := mb.abandonOwnership(1)
	require.True(t, hadWork)
	require.Equal(t, mbIdle, mb.state)
	require.Len(t, mb.submitted, 1, "the entry must survive in submitted")

	// A new owner arrives.
	becomeOwner, epoch2 := mb.submit(SessionAgentCall{SessionID: "s1", Prompt: "new owner"}, func() {})
	require.True(t, becomeOwner)
	require.NotEqual(t, uint64(1), epoch2)

	// The new owner's end-of-turn drain must find the old entry.
	next, hasNext, _, _ := mb.drainOrReleaseFinal(epoch2, nil)
	require.True(t, hasNext, "the surviving entry must be returned to the new owner")
	require.Equal(t, queued, next)
}

// TestSessionAgent_ClearQueue_ClearsMailboxOnly verifies ClearQueue clears
// all queued work from the mailbox. Since #308 removed the legacy
// messageQueue, ClearQueue's only job is clearing the mailbox's submitted/
// replacement/injects via clearAll().
func TestSessionAgent_ClearQueue_ClearsMailboxOnly(t *testing.T) {
	sa := &sessionAgent{
		mailboxes: newTestMailboxesMap(),
	}
	const sid = "clear-test"

	// Queue three calls via the public QueueMessage API.
	sa.QueueMessage(SessionAgentCall{SessionID: sid, Prompt: "first"})
	sa.QueueMessage(SessionAgentCall{SessionID: sid, Prompt: "second"})
	sa.QueueMessage(SessionAgentCall{SessionID: sid, Prompt: "third"})

	require.Equal(t, 3, sa.QueuedPrompts(sid))
	require.Len(t, sa.QueuedPromptsList(sid), 3)

	sa.ClearQueue(sid)

	require.Equal(t, 0, sa.QueuedPrompts(sid), "ClearQueue must remove every queued entry")
	require.Empty(t, sa.QueuedPromptsList(sid), "ClearQueue must leave no entries in the list either")
}

// TestSessionAgent_QueuedPrompts_NoDoubleCount verifies that QueuedPrompts
// and QueuedPromptsList count each queued message exactly once. Before #308,
// these methods summed counts across both the legacy messageQueue and the
// mailbox's submitted queue — a message that appeared in both would be
// counted twice. With the legacy queue gone, there is only one source.
func TestSessionAgent_QueuedPrompts_NoDoubleCount(t *testing.T) {
	sa := &sessionAgent{
		mailboxes: newTestMailboxesMap(),
	}
	const sid = "count-test"

	require.Equal(t, 0, sa.QueuedPrompts(sid))

	sa.QueueMessage(SessionAgentCall{SessionID: sid, Prompt: "a"})
	require.Equal(t, 1, sa.QueuedPrompts(sid), "one message queued = count of 1")

	sa.QueueMessage(SessionAgentCall{SessionID: sid, Prompt: "b"})
	require.Equal(t, 2, sa.QueuedPrompts(sid), "two messages queued = count of 2")

	// QueuedPromptsList must match.
	list := sa.QueuedPromptsList(sid)
	require.Len(t, list, 2)
	assert.Equal(t, []string{"a", "b"}, list)

	// Different session must have its own count.
	require.Equal(t, 0, sa.QueuedPrompts("other-session"))
}

// TestMailbox_ReclaimReplacementOrKeep_StillPushesToFrontAfterMigration
// verifies that reclaimReplacementOrKeep (#307) continues to return the
// displaced call to the FRONT of mb.submitted after the #308 migration
// removed the legacy messageQueue. The submitted queue is now the ONLY
// queue; if reclaimReplacementOrKeep failed to re-queue the displaced call,
// it would be silently destroyed — the same #283/P0-2 class of bug.
func TestMailbox_ReclaimReplacementOrKeep_StillPushesToFrontAfterMigration(t *testing.T) {
	callA := SessionAgentCall{SessionID: "s1", Prompt: "A - about to be pre-empted"}
	callB := SessionAgentCall{SessionID: "s1", Prompt: "B - already queued"}
	callD := SessionAgentCall{SessionID: "s1", Prompt: "D - the interrupt's replacement"}
	mb := &mailbox{
		replacement: &callD,
		submitted:   []SessionAgentCall{callB},
	}

	got := mb.reclaimReplacementOrKeep(callA)

	require.Equal(t, callD, got, "D (the replacement) must be returned to run next")
	require.Nil(t, mb.replacement, "replacement must be consumed")
	require.Equal(t, []SessionAgentCall{callA, callB}, mb.submitted,
		"A must be pushed to the FRONT of submitted (ahead of B), preserving FIFO order — "+
			"with the legacy queue gone, submitted is the ONLY queue; dropping A here would lose it permanently")
}

// TestRun_QueuedMessageDuringBusySession_RunsExactlyOnce is the end-to-end
// integration test for the #308 migration's core promise: a message queued
// via QueueMessage during a busy session executes EXACTLY ONCE (as exactly
// one additional turn), not zero times (stranded) and not twice
// (double-inserted). It replaces the former
// TestRun_LegacyQueueMessageDuringFinalDrain_DoesNotWedgeAcrossTurns's
// direct messageQueue.Append with the public QueueMessage API, proving the
// full public path works after the migration.
func TestRun_QueuedMessageDuringBusySession_RunsExactlyOnce(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "single execution migration test")
	require.NoError(t, err)

	var calls atomic.Int64
	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	srv := requestStartedSSEServer(&calls, requestStarted, proceed)
	t.Cleanup(srv.Close)
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	titleSrv := singleTurnSSEServer(nil)
	t.Cleanup(titleSrv.Close)
	titleProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(titleSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	agentIface := NewSessionAgent(SessionAgentOptions{
		LargeModel:           Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		SmallModel:           Model{Model: titleLM, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
		DataDirectory:        dataDir,
	})
	sa := agentIface.(*sessionAgent)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := agentIface.Run(t.Context(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "first message",
			MaxOutputTokens: 1000,
		})
		runDone <- runErr
	}()

	// Wait for turn 1 to reach the provider.
	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("turn 1 never reached the provider")
	}

	// Queue via the PUBLIC QueueMessage API during turn 1's stream —
	// this used to go to the legacy messageQueue, now goes to mailbox.submitted.
	sa.QueueMessage(SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "queued during turn 1",
		MaxOutputTokens: 1000,
	})

	close(proceed)

	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}

	// THE core assertion: exactly 2 provider calls — turn 1 and turn 2
	// (the queued message). If the message were double-inserted, this
	// would be 3. If it were stranded, this would be 1.
	assert.Equal(t, int64(2), calls.Load(),
		"expected exactly two provider calls: turn 1 + the queued message as turn 2 — "+
			"not 1 (stranded) and not 3 (double-inserted)")

	// The session must be idle after both turns complete.
	assert.False(t, sa.IsSessionBusy(sess.ID),
		"the session must be idle once both turns complete")
}

// newTestMailboxesMap creates a csync.Map for a bare sessionAgent that only
// needs the mailboxes field (used by ClearQueue/QueuedPrompts tests that
// don't need a provider or DB).
func newTestMailboxesMap() *csync.Map[string, *mailbox] {
	return csync.NewMap[string, *mailbox]()
}
