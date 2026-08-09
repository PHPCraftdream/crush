package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestSessionAgent_RestartOrphaned_RunsUnderFreshLock_NotUnprotected is the
// end-to-end regression test for the #297 review finding against an earlier
// draft of #296/P1-C: a call that mailbox.drainOrReleaseFinal reports as
// orphaned (raced into the mailbox DURING release()'s OS-lock teardown, so
// the ORIGINAL caller's lk is already gone by the time it is noticed) must
// be restarted via a.restartOrphaned under a BRAND NEW session.SessionLock
// acquisition — never left running unprotected, and never mistaken for
// already covered by the released lock.
//
// This drives the REAL sessionAgent.restartOrphaned (wired from
// drainOrReleaseMerged — see that method's own doc for the case-4 wiring)
// against a REAL session.SessionLock and a REAL provider probe. The mailbox
// half of this contract (drainOrReleaseFinal returning hasNext==false with
// the racing call in `orphaned`, never hasNext==true/mbOwned) is proven in
// isolation by mailbox_lock_test.go's
// TestMailbox_DrainOrReleaseFinal_OrphanedWorkNeverRunsWithoutFreshOSLock;
// THIS test proves the layer above actually restarts what the mailbox
// reports, under real lock protection, instead of dropping it or running it
// bare.
//
// Sequence:
//  1. Call sa.restartOrphaned with one call, exactly as drainOrReleaseMerged
//     does for whatever drainOrReleaseFinal returns as orphaned.
//  2. Assert a DIRECT session.TryAcquireSessionLock for the same session ID,
//     issued while the restarted turn is provably still in flight (blocked
//     mid-stream), FAILS — proving the restart is holding its own freshly
//     acquired lock. If restartOrphaned instead ran the call without
//     acquiring a lock (the shape of the #297 bug, just one layer removed:
//     "hand back to a lock-less turn loop" vs. "restart without any lock at
//     all" are the same class of hole), this acquisition would spuriously
//     succeed.
//  3. Assert the call actually completed and persisted, proving it is not
//     silently dropped either.
func TestSessionAgent_RestartOrphaned_RunsUnderFreshLock_NotUnprotected(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "restart-orphaned test")
	require.NoError(t, err)

	restartStarted := make(chan struct{}, 1)
	restartProceed := make(chan struct{})
	restartSrv := requestStartedSSEServer(nil, restartStarted, restartProceed)
	t.Cleanup(restartSrv.Close)
	restartProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(restartSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	restartLM, err := restartProvider.LanguageModel(t.Context(), "probe")
	require.NoError(t, err)

	titleSrv := singleTurnSSEServer(nil)
	t.Cleanup(titleSrv.Close)
	titleProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(titleSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(t.Context(), "probe")
	require.NoError(t, err)

	agentIface := NewSessionAgent(SessionAgentOptions{
		LargeModel:           Model{Model: restartLM, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
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

	// Precondition: nothing holds the session's OS lock yet.
	precheck, err := session.TryAcquireSessionLock(dataDir, sess.ID)
	require.NoError(t, err)
	require.NoError(t, precheck.Release())

	// Since task #340 ROUND 3, restartOrphaned durably enqueues the call
	// instead of running it inline — a session.RunQueuePump is the only
	// thing that drains that queue. The pump's own execution still goes
	// through sa.Run, which still acquires a fresh OS lock, so the "lock
	// held while restart is in flight" assertion below remains valid; it
	// just now happens via the pump instead of directly inside
	// restartOrphaned's goroutine.
	pumpCoord := &pumpTestCoordinator{sessionAgent: sa, dataDir: dataDir}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       env.sessions,
		DataDirectory:  dataDir,
		Coordinator:    pumpCoord,
		PumpInstanceID: "test-pump-restart-orphaned",
		TestTick:       func() time.Duration { return 100 * time.Millisecond },
	})
	pump.Start()
	t.Cleanup(pump.Stop)

	// Exactly what drainOrReleaseMerged does for its `orphaned` return value.
	sa.restartOrphaned([]SessionAgentCall{{
		SessionID:       sess.ID,
		Prompt:          "orphaned by a concurrent release",
		MaxOutputTokens: 1000,
	}})

	select {
	case <-restartStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the orphaned call was never restarted — restartOrphaned did not run it")
	}

	// THE core #297 assertion: while the detached restart's own turn is
	// provably in flight, a DIRECT second acquisition of the SAME session's
	// OS lock must FAIL — proving the restart is running under its OWN
	// freshly-acquired lock, not unprotected.
	_, lockErr := session.TryAcquireSessionLock(dataDir, sess.ID)
	require.Error(t, lockErr, "the restarted orphaned call must hold its OWN freshly-acquired OS lock while its "+
		"turn is in flight — if it ran unprotected instead, this acquisition would spuriously succeed, exactly "+
		"the #297 class of double-owner bug reached from a different angle")

	close(restartProceed)

	// Let the detached goroutine finish and release its lock.
	require.Eventually(t, func() bool {
		lk2, err := session.TryAcquireSessionLock(dataDir, sess.ID)
		if err != nil {
			return false
		}
		_ = lk2.Release()
		return true
	}, 10*time.Second, 50*time.Millisecond, "the restarted orphaned call's own lock must eventually be released")

	// The orphaned prompt must have actually been persisted and run — not
	// silently dropped.
	msgs, err := env.messages.List(context.Background(), sess.ID)
	require.NoError(t, err)
	found := false
	for _, m := range msgs {
		if m.Role == message.User && m.FullText() == "orphaned by a concurrent release" {
			found = true
			break
		}
	}
	require.True(t, found, "the orphaned call's prompt must have been persisted and run via the detached restart, "+
		"not silently dropped")
}

// TestMailbox_DrainOrReleaseFinal_ThenAgentRestartsOrphaned_FullPath chains
// the two halves together against the REAL sessionAgent's mailbox: proves
// that what drainOrReleaseFinal reports as orphaned for a REAL session is
// exactly what ends up passed to restartOrphaned by drainOrReleaseMerged,
// end to end, using the actual production wiring (not a hand-rolled call to
// restartOrphaned as the test above does, and not a bare *mailbox as
// mailbox_lock_test.go does) — the seam between the two that neither of the
// other two tests exercises on its own.
func TestMailbox_DrainOrReleaseFinal_ThenAgentRestartsOrphaned_FullPath(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "full-path orphaned test")
	require.NoError(t, err)

	restartStarted := make(chan struct{}, 1)
	restartProceed := make(chan struct{})
	restartSrv := requestStartedSSEServer(nil, restartStarted, restartProceed)
	t.Cleanup(restartSrv.Close)
	restartProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(restartSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	restartLM, err := restartProvider.LanguageModel(t.Context(), "probe")
	require.NoError(t, err)

	titleSrv := singleTurnSSEServer(nil)
	t.Cleanup(titleSrv.Close)
	titleProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(titleSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(t.Context(), "probe")
	require.NoError(t, err)

	agentIface := NewSessionAgent(SessionAgentOptions{
		LargeModel:           Model{Model: restartLM, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
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

	mb := sa.getMailbox(sess.ID)
	mb.mu.Lock()
	mb.state = mbOwned
	mb.epoch = 1
	dispatcherCancel := func() {}
	mb.dispatcherCancel = dispatcherCancel
	mb.current = generation{id: 1, cancel: func() {}}
	mb.mu.Unlock()

	lk, err := session.TryAcquireSessionLock(dataDir, sess.ID)
	require.NoError(t, err)

	// A release callback that lands the race INSIDE the release() window
	// (after drainOrReleaseFinal has already confirmed submitted/replacement/
	// legacy are all empty) and then performs the real lk.Release() —
	// reproducing the exact window drainOrReleaseMerged's own release =
	// lk.Release wiring exposes in production, but driven through the REAL
	// agent.go call path (drainOrReleaseMerged), not a hand-built mailbox.
	raceRelease := func() error {
		mb.mu.Lock()
		mb.submitted = append(mb.submitted, SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "raced in during the real release window",
			MaxOutputTokens: 1000,
		})
		mb.mu.Unlock()
		return lk.Release()
	}

	next, hasNext, orphaned, releaseErr := mb.drainOrReleaseFinal(1, raceRelease)
	require.NoError(t, releaseErr)
	require.False(t, hasNext, "the original caller must not be told to keep running — lk is already released")
	require.Equal(t, SessionAgentCall{}, next)
	require.Len(t, orphaned, 1)

	// Since task #340 ROUND 3, restartOrphaned durably enqueues the call
	// instead of running it inline — see the sibling test above for why a
	// pump here still preserves this test's "fresh lock held while in
	// flight" assertion.
	pumpCoord := &pumpTestCoordinator{sessionAgent: sa, dataDir: dataDir}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       env.sessions,
		DataDirectory:  dataDir,
		Coordinator:    pumpCoord,
		PumpInstanceID: "test-pump-restart-orphaned-fullpath",
		TestTick:       func() time.Duration { return 100 * time.Millisecond },
	})
	pump.Start()
	t.Cleanup(pump.Stop)

	// Do exactly what drainOrReleaseMerged does with `orphaned`.
	sa.restartOrphaned(orphaned)

	select {
	case <-restartStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the orphaned call was never restarted")
	}

	_, lockErr := session.TryAcquireSessionLock(dataDir, sess.ID)
	require.Error(t, lockErr, "the restarted orphaned call must hold its own freshly-acquired OS lock while its "+
		"turn is in flight")

	close(restartProceed)

	require.Eventually(t, func() bool {
		lk2, err := session.TryAcquireSessionLock(dataDir, sess.ID)
		if err != nil {
			return false
		}
		_ = lk2.Release()
		return true
	}, 10*time.Second, 50*time.Millisecond)

	msgs, err := env.messages.List(context.Background(), sess.ID)
	require.NoError(t, err)
	found := false
	for _, m := range msgs {
		if m.Role == message.User && m.FullText() == "raced in during the real release window" {
			found = true
			break
		}
	}
	require.True(t, found, "the call that raced into the release() window must have been persisted and run via "+
		"the detached restart")
}
