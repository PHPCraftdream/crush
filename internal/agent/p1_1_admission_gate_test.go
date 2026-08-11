package agent

// P1-1 regression test (docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md):
// closes a real "Add concurrently with Wait" race in agent.go's Run() and
// Summarize() entry points. Before the fix, both called runWg.Add(1) and then
// checked shuttingDown as two separate, unsynchronized steps — between the
// Add and the check, a concurrent CancelAll could set shuttingDown and spin
// up its runWg.Wait() goroutine while the counter was still 0, letting Wait()
// return (nothing registered yet) a moment before the Add(1) landed. Per the
// sync.WaitGroup contract ("calls with a positive delta that start when the
// counter is zero must happen before a Wait"), this either panics at runtime
// or lets a new Run/Summarize start after CancelAll has already told its caller
// shutdown was safe — reintroducing the exact class of bug P0-3/P0-4 exist
// to close. The fix gates both the check and the Add under one mutex
// (admitMu/stopping, mirroring P0-3's RunQueuePump pattern).
//
// This test exercises tryAdmitRunWg directly (package agent, not agent_test,
// specifically to reach the unexported method and fields) by flipping the
// gate manually — deterministic, rather than trying to time a real CancelAll()
// call against a real dispatch within a few microseconds.
//
// SCOPE NOTE (added on independent review): the three tests below
// (TestP1_1_AdmissionGateRejectsRun/Summarize/AllowsBeforeShuttingDown)
// only check runWg's state AFTER Run()/Summarize() has already returned.
// That is not a valid discriminator: even the ORIGINAL unsynchronized code
// (`runWg.Add(1)` then `defer runWg.Done()` then, later, the shuttingDown
// check) leaves runWg balanced back to its prior count by the time the
// call returns, on EVERY return path — the bug is a pure timing race
// between a concurrent Add and a concurrent Wait, not an observable
// post-hoc imbalance. Verified directly: temporarily rewrote
// tryAdmitRunWg to the historical Add-then-check shape (Add(1) always,
// compensating Done() on the rejected path, no admitMu at all) and all
// three tests below still PASSED unchanged — the sixth vacuous-test
// occurrence in this babygoal round. Kept below anyway as functional
// smoke tests (they do prove Run/Summarize return ErrAgentShuttingDown
// and register/don't-register correctly in the steady state), but
// TestP1_1_TryAdmitRunWgSerializedAgainstAdmitMu is the actual regression
// test for the race: it proves the mutual-exclusion property the fix
// depends on — that tryAdmitRunWg() cannot proceed while another
// goroutine holds admitMu (i.e. is inside what would be CancelAll's own
// critical section) — which the unsynchronized version does not provide
// at all.
//
// REVERT CHECK PROCEDURE (for TestP1_1_TryAdmitRunWgSerializedAgainstAdmitMu):
//  1. In agent.go's tryAdmitRunWg, remove the `a.admitMu.Lock()`/
//     `defer a.admitMu.Unlock()` pair, leaving just the Add/check logic.
//  2. Run: go test ./internal/agent -run TestP1_1_TryAdmitRunWgSerializedAgainstAdmitMu -v
//  3. FAIL: tryAdmitRunWg() returns immediately even while the test holds
//     admitMu locked in the main goroutine — nothing serializes it anymore.
//  4. Restore the admitMu lock/unlock pair inside tryAdmitRunWg.

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/stretchr/testify/require"
)

// newFastModel creates a simple mock model for tests that don't need real
// provider interactions (P1-1 gate tests only test admission logic, not LLM calls).
func newFastModel(t *testing.T) Model {
	t.Helper()
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL("http://127.0.0.1:1"), // never dialed
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)
	return Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
}

// TestP1_1_AdmissionGateRejectsRun proves that Run() refuses to admit
// a new call after the admission gate has been latched, WITHOUT registering
// in runWg (the Add is skipped entirely under admitMu).
func TestP1_1_AdmissionGateRejectsRun(t *testing.T) {
	env := testEnv(t)
	model := newFastModel(t)

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:    model,
		SmallModel:    model,
		SystemPrompt:  "you are a probe",
		DataDirectory: env.workingDir,
		Sessions:      env.sessions,
		Messages:      env.messages,
		IsYolo:        true,
	})
	t.Cleanup(func() { sa.CancelAll() })

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "p1-1-gate-run-test")
	require.NoError(t, err)

	// Flip the admission gate directly, WITHOUT calling the real CancelAll()
	// (which would also sweep mailboxes and spawn its Wait goroutine) — isolates
	// the admission-gate behavior on its own.
	sa.(*sessionAgent).admitMu.Lock()
	sa.(*sessionAgent).shuttingDown.Store(true)
	sa.(*sessionAgent).admitMu.Unlock()

	// Run() must refuse immediately with ErrAgentShuttingDown, and it must NOT
	// register in runWg at all (the Add is skipped inside tryAdmitRunWg).
	result, err := sa.Run(ctx, SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "test",
	})
	require.ErrorIs(t, err, ErrAgentShuttingDown, "Run() must return ErrAgentShuttingDown after gate latched")
	require.Nil(t, result, "Run() must return nil result after gate latched")

	// Prove runWg was NOT incremented: runWg.Wait() returns immediately.
	// If Run() had called Add(1) before the gate check, Wait would block.
	done := make(chan struct{})
	go func() {
		sa.(*sessionAgent).runWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Correct: Wait returned immediately, so no unit was registered.
	case <-time.After(1 * time.Second):
		t.Fatal("runWg.Wait() blocked, meaning Run() called Add(1) despite the admission gate being latched")
	}
}

// TestP1_1_AdmissionGateRejectsSummarize proves that Summarize() refuses to
// admit a new call after the admission gate has been latched, WITHOUT
// registering in runWg (the Add is skipped entirely under admitMu).
func TestP1_1_AdmissionGateRejectsSummarize(t *testing.T) {
	env := testEnv(t)
	model := newFastModel(t)

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:    model,
		SmallModel:    model,
		SystemPrompt:  "you are a probe",
		DataDirectory: env.workingDir,
		Sessions:      env.sessions,
		Messages:      env.messages,
		IsYolo:        true,
	})
	t.Cleanup(func() { sa.CancelAll() })

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "p1-1-gate-summarize-test")
	require.NoError(t, err)

	// Flip the admission gate directly, WITHOUT calling the real CancelAll().
	sa.(*sessionAgent).admitMu.Lock()
	sa.(*sessionAgent).shuttingDown.Store(true)
	sa.(*sessionAgent).admitMu.Unlock()

	// Summarize() must refuse immediately with ErrAgentShuttingDown, and it must
	// NOT register in runWg at all.
	snapshot := &SummarizeSnapshot{model: model, promptPrefix: "summarize this"}
	err = sa.Summarize(ctx, sess.ID, snapshot)
	require.ErrorIs(t, err, ErrAgentShuttingDown, "Summarize() must return ErrAgentShuttingDown after gate latched")

	// Prove runWg was NOT incremented: runWg.Wait() returns immediately.
	done := make(chan struct{})
	go func() {
		sa.(*sessionAgent).runWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Correct: Wait returned immediately.
	case <-time.After(1 * time.Second):
		t.Fatal("runWg.Wait() blocked, meaning Summarize() called Add(1) despite the admission gate being latched")
	}
}

// TestP1_1_AdmissionGateAllowsBeforeShuttingDown proves that tryAdmitRunWg()
// successfully registers in runWg when the gate is NOT latched.
func TestP1_1_AdmissionGateAllowsBeforeShuttingDown(t *testing.T) {
	env := testEnv(t)
	model := newFastModel(t)

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:    model,
		SmallModel:    model,
		SystemPrompt:  "you are a probe",
		DataDirectory: env.workingDir,
		Sessions:      env.sessions,
		Messages:      env.messages,
		IsYolo:        true,
	})
	t.Cleanup(func() { sa.CancelAll() })

	// tryAdmitRunWg() returns true and increments runWg when gate is not latched.
	require.True(t, sa.(*sessionAgent).tryAdmitRunWg(), "tryAdmitRunWg() must return true when gate is not latched")

	// runWg should now have one pending unit. Prove it by calling Wait in a
	// goroutine and verifying it blocks.
	done := make(chan struct{})
	go func() {
		sa.(*sessionAgent).runWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("runWg.Wait() returned immediately, meaning tryAdmitRunWg() did NOT increment runWg")
	case <-time.After(100 * time.Millisecond):
		// Correct: Wait is blocked, so a unit was registered.
	}

	// Clean up: call Done() to unblock the test.
	sa.(*sessionAgent).runWg.Done()
	select {
	case <-done:
		// Correct: Wait now completes.
	case <-time.After(1 * time.Second):
		t.Fatal("runWg.Wait() did not return after Done()")
	}
}

// TestP1_1_TryAdmitRunWgSerializedAgainstAdmitMu is the actual regression
// test for the P1-1 race: it proves tryAdmitRunWg() cannot proceed while
// another goroutine holds admitMu — the same mutual-exclusion CancelAll's
// own critical section (shuttingDown.Store + the mailbox sweep + spinning
// up its runWg.Wait() goroutine) relies on. Without this property, a
// concurrent Run()/Summarize() could still call runWg.Add(1) while
// CancelAll is in the middle of its own admitMu-guarded transition,
// reopening exactly the "Add concurrently with Wait" window the fix
// exists to close — even though the three tests above (which only check
// runWg's state AFTER the call already returned) cannot detect that,
// since the historical unsynchronized code left runWg balanced on every
// return path regardless of timing.
func TestP1_1_TryAdmitRunWgSerializedAgainstAdmitMu(t *testing.T) {
	env := testEnv(t)
	model := newFastModel(t)

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:    model,
		SmallModel:    model,
		SystemPrompt:  "you are a probe",
		DataDirectory: env.workingDir,
		Sessions:      env.sessions,
		Messages:      env.messages,
		IsYolo:        true,
	})
	t.Cleanup(func() { sa.CancelAll() })
	saConcrete := sa.(*sessionAgent)

	// Simulate being "inside" CancelAll's own admitMu-guarded critical
	// section (the real CancelAll locks admitMu around shuttingDown.Store,
	// see its own code) by holding admitMu directly from the test.
	saConcrete.admitMu.Lock()

	admitReturned := make(chan bool, 1)
	go func() {
		admitReturned <- saConcrete.tryAdmitRunWg()
	}()

	select {
	case <-admitReturned:
		t.Fatal("tryAdmitRunWg() returned while admitMu was held by another goroutine — the check-and-register is not serialized against CancelAll's critical section, reopening the P1-1 race")
	case <-time.After(200 * time.Millisecond):
		// Correct: tryAdmitRunWg() is blocked waiting for admitMu.
	}

	saConcrete.admitMu.Unlock()

	select {
	case ok := <-admitReturned:
		require.True(t, ok, "tryAdmitRunWg() must succeed once admitMu is released and shuttingDown is still false")
	case <-time.After(1 * time.Second):
		t.Fatal("tryAdmitRunWg() never returned after admitMu was released")
	}

	// Balance the runWg increment tryAdmitRunWg() just performed.
	saConcrete.runWg.Done()
}
