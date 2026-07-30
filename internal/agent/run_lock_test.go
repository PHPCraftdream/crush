package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newLockTestSessionAgent builds the minimal *sessionAgent needed to drive
// Run() as far as the inter-process session-lock check (agent.go ~470-543)
// without panicking on a nil csync field. Bare &sessionAgent{} (the pattern
// used by convert_test.go / usage_fallback_test.go) is NOT enough here:
// Run() calls a.IsSessionBusy, which calls a.activeRequests.Get — a nil
// *csync.Map receiver panics (nil pointer dereference inside sync.RWMutex),
// so activeRequests must be initialized the same way NewSessionAgent does.
// messageQueue is initialized too since the busy path (not exercised by
// these tests, but reached if IsSessionBusy ever returned true) appends to
// it. tools/largeModel/systemPrompt/systemPromptPrefix are initialized too
// because Run's very next block after the lock section (agent.go:546-549,
// "Copy mutable fields under lock") unconditionally dereferences them —
// needed only by TestRun_SubAgentChildSession_ProceedsPastLockWhenFree,
// which drives Run() past the lock into that code; harmless no-ops for the
// lock-rejection test, which returns before reaching that point.
func newLockTestSessionAgent(dataDir string, isSubAgent bool) *sessionAgent {
	return &sessionAgent{
		dataDir:            dataDir,
		isSubAgent:         isSubAgent,
		activeRequests:     csync.NewMap[string, context.CancelFunc](),
		messageQueue:       csync.NewKeyedQueue[SessionAgentCall](),
		tools:              csync.NewSliceFrom[fantasy.AgentTool](nil),
		largeModel:         csync.NewValue(Model{}),
		systemPrompt:       csync.NewValue(""),
		systemPromptPrefix: csync.NewValue(""),
	}
}

// TestRun_SubAgentChildSession_RejectsWhenLockHeldByAnotherProcess is the
// regression test for P1.1 (docs/reviews/2026-07-30-project-review.md,
// commit 18055ba0, task #163) that actually drives agent.Run itself, rather
// than only the lock primitive it depends on (which
// session.TestCrossProcess_SubAgentChildSessionIsProtected already covers).
//
// Before the fix, sessionAgent.Run's inter-process lock acquisition was
// gated by `if !a.isSubAgent && a.dataDir != ""`, so a sub-agent's Run call
// never took the lock for its own CHILD session id (format
// "parentMessageID$$toolCallID", see session.CreateAgentToolSessionID) at
// all. This test simulates "another crush process already holds the lock
// for this exact child session id" by acquiring the lock directly from the
// test (standing in for the other process) before calling Run, and asserts
// Run refuses to proceed — returning the same "already in use" error a
// top-level session would get — instead of silently streaming into an
// already-claimed session.
func TestRun_SubAgentChildSession_RejectsWhenLockHeldByAnotherProcess(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	childSessionID := "msg-parent-xyz$$toolcall-abc" // session.CreateAgentToolSessionID's "%s$$%s" format

	// Simulate another process already holding the lock for this child
	// session id. Deliberately NOT released before calling Run — that's
	// the whole point of the test.
	externalLock, err := session.TryAcquireSessionLock(dataDir, childSessionID)
	require.NoError(t, err, "test setup: must be able to take the lock before Run ever runs")
	t.Cleanup(func() {
		_ = externalLock.Release()
	})

	a := newLockTestSessionAgent(dataDir, true /* isSubAgent */)

	result, runErr := a.Run(context.Background(), SessionAgentCall{
		SessionID: childSessionID,
		Prompt:    "test prompt",
	})

	require.Error(t, runErr, "Run must refuse to proceed into a sub-agent session another process already locked")
	assert.Nil(t, result)

	var busyErr *session.SessionLockBusyError
	assert.True(t, errors.As(runErr, &busyErr),
		"expected the wrapped error to satisfy errors.As(*session.SessionLockBusyError, ...) per agent.go's `fmt.Errorf(...: %%w, lockErr)`, got %v", runErr)
	assert.True(t, strings.Contains(runErr.Error(), "already in use"),
		"expected agent.go's exact wording (\"already in use\"), got %q", runErr.Error())

	// Reverting the #163 fix (re-adding `!a.isSubAgent &&` to the lock
	// guard) would make Run skip the lock check entirely for isSubAgent
	// == true and fall through toward fantasy.Agent/provider execution
	// instead of returning here — it would panic on the nil a.sessions
	// field long before producing this "already in use" error, which is
	// exactly the failure mode that proves this test catches the
	// regression.
}

// blockingGetSessionService is a minimal session.Service fake used only to
// pin down the exact instant Run() gets past the lock-acquisition code
// (agent.go ~510-543): its Get closes entered (once, on first call) before
// blocking until the test signals it to proceed via unblock. entered is
// the deterministic synchronization signal proving Run has ALREADY
// acquired the session lock (Get is the very next thing Run calls after
// the lock section, agent.go:546-586) — it is intentionally NOT a race
// against Run's own lock acquisition the way an external polling loop
// would be (an earlier version of this test polled
// session.TryAcquireSessionLock in a loop from a second goroutine, which
// intermittently won the race for the still-free lock before Run's own
// goroutine got scheduled, starving Run's real attempt and producing a
// flaky false "Run was rejected"). Every other session.Service method is
// unreachable from Run before Get, so embedding the nil interface for the
// rest is safe: they're never called.
type blockingGetSessionService struct {
	session.Service
	entered chan struct{}
	unblock chan struct{}
}

func (b *blockingGetSessionService) Get(ctx context.Context, id string) (session.Session, error) {
	close(b.entered)
	<-b.unblock
	return session.Session{}, errors.New("blockingGetSessionService: Get intentionally fails after unblocking")
}

// TestRun_SubAgentChildSession_ProceedsPastLockWhenFree is the inverse of
// the above: with no external holder, a sub-agent's Run call for the same
// child-session-id shape must NOT be rejected by the lock check. Driving a
// full turn (fantasy.Agent + provider) from a bare sessionAgent isn't
// practical here, so this proves the negative directly: Run does not
// return the lock-busy error, and — while Run is blocked deterministically
// inside a.sessions.Get (i.e. demonstrably past the lock section) — a
// fresh attempt to acquire the very same session lock from outside now
// correctly fails as busy, proving Run itself took (and is still holding)
// the lock for the sub-agent session rather than the lock check being
// skipped/no-op'd.
func TestRun_SubAgentChildSession_ProceedsPastLockWhenFree(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	childSessionID := "msg-parent-free$$toolcall-def"

	a := newLockTestSessionAgent(dataDir, true /* isSubAgent */)
	fakeSessions := &blockingGetSessionService{
		entered: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	a.sessions = fakeSessions

	runErrCh := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), SessionAgentCall{
			SessionID: childSessionID,
			Prompt:    "test prompt",
		})
		runErrCh <- err
	}()

	// Wait for Run to reach a.sessions.Get — the very next call after the
	// lock section succeeds (agent.go:546-586) — which deterministically
	// proves Run's own lock acquisition already succeeded (no
	// "already in use" rejection) without racing Run for the lock
	// ourselves.
	select {
	case <-fakeSessions.entered:
		// Good: Run got past the lock-acquisition code and is now
		// blocked inside a.sessions.Get, meaning it currently holds the
		// session lock.
	case runErr := <-runErrCh:
		t.Fatalf("Run returned before reaching a.sessions.Get (err=%v) — was it rejected by the lock check?", runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Run to reach a.sessions.Get")
	}

	// Now that Run demonstrably holds the lock (blocked inside Get), a
	// fresh external acquire attempt for the exact same child session id
	// must fail as busy — proving Run itself (not just the underlying
	// primitive) takes the inter-process lock for sub-agent sessions.
	_, lockErr := session.TryAcquireSessionLock(dataDir, childSessionID)
	var busyErr *session.SessionLockBusyError
	require.Error(t, lockErr, "expected the session lock to be held while Run is blocked inside a.sessions.Get")
	require.True(t, errors.As(lockErr, &busyErr), "expected SessionLockBusyError, got %v", lockErr)

	close(fakeSessions.unblock)

	select {
	case runErr := <-runErrCh:
		require.Error(t, runErr, "expected Run to surface blockingGetSessionService's Get error")
		assert.NotContains(t, runErr.Error(), "already in use",
			"Run must not report the session as locked once no external process holds it")
		assert.False(t, errors.As(runErr, &busyErr),
			"got a SessionLockBusyError with no external holder — the lock check incorrectly rejected a free session")
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Run to return after unblocking Get")
	}

	// Lock must be released now that Run has returned.
	finalLock, err := session.TryAcquireSessionLock(dataDir, childSessionID)
	require.NoError(t, err, "expected the lock to be released after Run returned")
	require.NoError(t, finalLock.Release())
}
