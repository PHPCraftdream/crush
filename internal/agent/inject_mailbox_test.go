package agent

import (
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// drainAndSpliceInjects drives the REAL production drain — sessionAgent's
// drainDueInjects, the exact function PrepareStep calls — against a fixture
// agent holding mb as sessionID's mailbox.
//
// The first draft of these tests re-implemented drain-and-dedup locally and
// asserted against the copy. That mirror passes whether or not production
// still agrees with it, which is precisely the defect task #243 was filed
// for ("new tests don't exercise agent.go's actual closure"). Production was
// therefore extracted into drainDueInjects so this helper can be a thin
// adapter instead of a second implementation.
func drainAndSpliceInjects(mb *mailbox, genID uint64, historyIDs map[string]struct{}) []message.Message {
	const sessionID = "inject-test-session"
	a := &sessionAgent{mailboxes: csync.NewMap[string, *mailbox]()}
	a.mailboxes.Set(sessionID, mb)
	return a.drainDueInjects(sessionID, genID, historyIDs)
}

// TestInject_NoDuplicate_WhenAlreadyInHistory is the regression test for the
// DUPLICATION symptom of P1-1 (mailbox stage 2.4):
//
// InjectMessage's DB write can land BEFORE the owner's preamble
// getSessionMessages call, so the row ends up in BOTH the loaded history AND
// the mailbox's injects queue (injectIfBusy saw the session as busy and
// queued it). Without the ID-based dedup check in PrepareStep, the row would
// be spliced a second time on top of the history copy — the message appears
// twice in the turn's prepared.Messages.
//
// With the fix: drainInjects returns the entry, but the ID check finds it in
// historyIDs and skips the splice. The message appears exactly once (via the
// DB-loaded history, zero times via the splice).
func TestInject_NoDuplicate_WhenAlreadyInHistory(t *testing.T) {
	mb := &mailbox{
		state:   mbOwned,
		current: generation{id: 1, cancel: func() {}},
	}

	// InjectMessage during a busy session: DB write + injectIfBusy.
	msg := message.Message{ID: "msg-dup", Role: message.User}
	require.True(t, mb.injectIfBusy(msg),
		"session is busy (mbOwned) → inject must succeed")

	// The preamble loaded this same row from the DB (DB write was before
	// getSessionMessages), so it is in the history set.
	historyIDs := map[string]struct{}{"msg-dup": {}}

	// PrepareStep's drain: ID check must skip the entry.
	spliced := drainAndSpliceInjects(mb, 1, historyIDs)
	require.Empty(t, spliced,
		"a message already in history must be spliced zero times — "+
			"without the ID check it would appear twice (once via history, once via splice)")
	require.Empty(t, mb.injects,
		"the entry must be consumed by drainInjects regardless of the ID check")
}

// TestInject_NoLoss_SurvivesToNextTurn is the regression test for the
// LOSS/DELAY symptom of P1-1 (mailbox stage 2.4):
//
// An inject that lands AFTER the owner's last PrepareStep but BEFORE the
// owner releases must survive in mb.injects and be picked up by the VERY
// NEXT turn's drainInjects — not stranded with no owner to drain it. The
// generation stamp ensures the entry (stamped with gen 1) is returned by
// drainInjects(gen2) in the next turn (1 ≤ 2). The ID check then decides
// splice-vs-skip: here the next turn's preamble did NOT load it from DB
// (DB write was after the preamble), so it is spliced — delivered exactly
// once.
//
// With the old non-atomic code, the inject could land in injectQueue AFTER
// the session became idle, with the message also in the DB: the next Run's
// TakeAll AND preamble DB read would both deliver it — duplication. And if
// no next Run was triggered, the message sat indefinitely until one was.
func TestInject_NoLoss_SurvivesToNextTurn(t *testing.T) {
	mb := &mailbox{
		state:            mbOwned,
		epoch:            1,
		dispatcherCancel: func() {},
		current:          generation{id: 1, cancel: func() {}},
	}

	// Turn 1's PrepareStep: nothing queued yet.
	due1 := mb.drainInjects(1)
	require.Empty(t, due1, "no injects before turn 1's PrepareStep")

	// InjectMessage lands AFTER turn 1's PrepareStep (stamped with gen 1).
	msg := message.Message{ID: "msg-late", Role: message.User}
	require.True(t, mb.injectIfBusy(msg),
		"session still owned → inject must succeed")

	// Turn 1 finishes, nothing else queued → owner releases.
	_, hasNext, _ := mb.drainOrReleaseFinal(1, nil, nil, nil)
	require.False(t, hasNext)
	require.Equal(t, mbIdle, mb.state, "owner released")

	// The inject entry MUST survive the release.
	require.Len(t, mb.injects, 1,
		"late inject must survive in mb.injects across the release")

	// A new Run starts: submit → become owner under a new epoch.
	becomeOwner, newEpoch := mb.submit(SessionAgentCall{SessionID: "s1"}, func() {})
	require.True(t, becomeOwner)
	require.NotEqual(t, uint64(1), newEpoch, "new era must have a distinct epoch")

	// Turn 2: beginGeneration bumps to gen 2.
	genID2 := mb.beginGeneration(func() {})

	// Turn 2's PrepareStep: drainInjects must find the late inject
	// (stamp=1 ≤ genID2=2). The preamble did NOT load it from DB, so
	// historyIDs is empty and the ID check does not skip it.
	spliced := drainAndSpliceInjects(mb, genID2, map[string]struct{}{})
	require.Len(t, spliced, 1,
		"the late inject must be delivered to the next turn — not stranded")
	require.Equal(t, "msg-late", spliced[0].ID)
	require.Empty(t, mb.injects, "the entry must be consumed by drainInjects")
}

// TestInjectIfBusy_AtomicWithOwnerRelease is the concurrency regression test
// for the atomicity half of the P1-1 fix: injectIfBusy must hold mb.mu for
// the entire busy-check + inject, so a concurrent drainOrReleaseFinal cannot
// flip the state to mbIdle between the check and the inject.
//
// Without this atomicity (the old separate IsSessionBusy + injectQueue.Append),
// IsSessionBusy could observe mbOwned, then the owner could release to mbIdle,
// and the append would strand the message in the queue of a session that was
// already idle — with the row also in the DB, the next Run would duplicate it.
//
// This test uses mailbox.testDrainSeam to deterministically reproduce the
// exact window: drainOrReleaseFinal is paused mid-critical-section (holding
// mb.mu), and injectIfBusy is fired concurrently. The test verifies:
//  1. injectIfBusy blocks while mb.mu is held (the critical sections are
//     mutually exclusive).
//  2. After drainOrReleaseFinal flips to idle and releases mb.mu, injectIfBusy
//     correctly observes mbIdle and does NOT inject — no stranded entry.
func TestInjectIfBusy_AtomicWithOwnerRelease(t *testing.T) {
	mb := &mailbox{
		state:            mbOwned,
		epoch:            1,
		dispatcherCancel: func() {},
		current:          generation{id: 1, cancel: func() {}},
	}

	seamEntered := make(chan struct{})
	releaseSeam := make(chan struct{})
	mb.testDrainSeam = func() {
		close(seamEntered)
		<-releaseSeam
	}

	// Owner's final drain: pauses inside testDrainSeam (holding mb.mu),
	// right before flipping state to mbIdle.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		_, _, _ = mb.drainOrReleaseFinal(1, nil, nil, nil)
	}()

	select {
	case <-seamEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("drainOrReleaseFinal never reached the test seam")
	}

	// Fire injectIfBusy concurrently. It must block on mb.mu.
	injectDone := make(chan bool, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		injectDone <- mb.injectIfBusy(message.Message{ID: "m-race"})
	})

	select {
	case <-injectDone:
		t.Fatal("injectIfBusy returned before drainOrReleaseFinal released mb.mu — critical sections overlapped")
	case <-time.After(50 * time.Millisecond):
	}

	// Release the seam → drainOrReleaseFinal flips to idle, releases mb.mu.
	close(releaseSeam)
	<-drainDone

	// injectIfBusy must now see mbIdle → return false (no inject).
	injected := <-injectDone
	require.False(t, injected,
		"after the owner released to idle, injectIfBusy must NOT inject — "+
			"the non-atomic old code would have left a stranded entry here")
	require.Empty(t, mb.injects,
		"no stranded entry when the session is idle — the message lives only in the DB")
}
