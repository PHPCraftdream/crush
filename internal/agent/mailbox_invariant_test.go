package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMailbox_Invariant_NoStaleCancelHandleSurvivesAnyMutatorReturn is the
// mechanical, table-driven check rounds 12 and 13's independent reviews
// both recommended in place of further ad-hoc reading. Four rounds of
// human review (BLOCKER-2, round 9; MEDIUM-1, round 11's legacy-reclaim
// branch; finding A, round 12's mb.submitted branch; the fourth instance,
// round 13's drainAfterCancel branches) each found ONE more copy of the
// exact same postcondition violation, one branch at a time, because each
// round re-audited whichever function had most recently changed rather
// than enumerating every branch that can leave the mailbox owned.
//
// The invariant, stated once and checked everywhere: for every mailbox
// operation that hands "hasNext == true" (or the equivalent "still
// running, ownership continues") back to a turn loop, the postcondition
// MUST be state == mbOwned && current.cancel == nil && dispatcherCancel
// != nil. For every operation that ends an ownership era, the
// postcondition MUST be state == mbIdle && current.cancel == nil &&
// dispatcherCancel == nil. current.cancel must NEVER be left holding a
// handle from a generation that already ended — that's the shape every
// one of the four found bugs shared.
//
// Each table row starts from a mailbox that looks exactly like the state
// right after a turn's own genCtx cancel has already fired once (the real
// production shape at every one of these call sites: runTurn calls its
// own `cancel()` before reaching the final drain, and drainAfterCancel is
// only reached from the isCancelErr branch, i.e. after a cancellation
// already happened) — current.cancel is a closure that records if it's
// ever invoked AGAIN, which would mean a stale handle survived and got
// called by a later Cancel()/InterruptAndReplace() instead of the correct
// dispatcherCancel fallback.
func TestMailbox_Invariant_NoStaleCancelHandleSurvivesAnyMutatorReturn(t *testing.T) {
	type expectation struct {
		stillOwned bool // true: era continues (mbOwned); false: era ends (mbIdle)
	}

	newFixture := func() *mailbox {
		return &mailbox{
			state:            mbOwned,
			epoch:            1,
			dispatcherCancel: func() {},
			current:          generation{id: 1, cancel: func() {}},
		}
	}

	cases := []struct {
		name string
		run  func(t *testing.T, mb *mailbox)
		want expectation
	}{
		{
			name: "drainOrRelease/submitted branch keeps ownership",
			run: func(t *testing.T, mb *mailbox) {
				mb.submitted = []SessionAgentCall{{SessionID: "s1"}}
				_, hasNext := mb.drainOrRelease(1)
				require.True(t, hasNext)
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "drainOrRelease/empty ends the era",
			run: func(t *testing.T, mb *mailbox) {
				_, hasNext := mb.drainOrRelease(1)
				require.False(t, hasNext)
			},
			want: expectation{stillOwned: false},
		},
		{
			name: "drainOrReleaseFinal/submitted branch keeps ownership",
			run: func(t *testing.T, mb *mailbox) {
				mb.submitted = []SessionAgentCall{{SessionID: "s1"}}
				_, hasNext, orphaned, err := mb.drainOrReleaseFinal(1, nil)
				require.NoError(t, err)
				require.True(t, hasNext)
				require.Empty(t, orphaned)
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "drainOrReleaseFinal/replacement branch keeps ownership",
			run: func(t *testing.T, mb *mailbox) {
				repl := SessionAgentCall{SessionID: "s1"}
				mb.replacement = &repl
				_, hasNext, orphaned, err := mb.drainOrReleaseFinal(1, nil)
				require.NoError(t, err)
				require.True(t, hasNext)
				require.Empty(t, orphaned)
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "drainOrReleaseFinal/both empty releases and ends the era",
			run: func(t *testing.T, mb *mailbox) {
				_, hasNext, orphaned, err := mb.drainOrReleaseFinal(1, func() error { return nil })
				require.NoError(t, err)
				require.False(t, hasNext)
				require.Empty(t, orphaned)
			},
			want: expectation{stillOwned: false},
		},
		{
			name: "drainAfterCancel/replacement branch keeps ownership",
			run: func(t *testing.T, mb *mailbox) {
				repl := SessionAgentCall{SessionID: "s1"}
				mb.replacement = &repl
				_, ok := mb.drainAfterCancel()
				require.True(t, ok)
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "drainAfterCancel/submitted branch keeps ownership",
			run: func(t *testing.T, mb *mailbox) {
				mb.submitted = []SessionAgentCall{{SessionID: "s1"}}
				_, ok := mb.drainAfterCancel()
				require.True(t, ok)
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "drainAfterCancel/empty returns no work and clears the handle",
			run: func(t *testing.T, mb *mailbox) {
				_, ok := mb.drainAfterCancel()
				require.False(t, ok, "empty mailbox must return no work")
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "abandonOwnership/empty always ends the era",
			run: func(t *testing.T, mb *mailbox) {
				hadWork := mb.abandonOwnership(1)
				require.False(t, hadWork)
			},
			want: expectation{stillOwned: false},
		},
		{
			name: "abandonOwnership/with queued work still always ends the era",
			run: func(t *testing.T, mb *mailbox) {
				mb.submitted = []SessionAgentCall{{SessionID: "s1"}}
				hadWork := mb.abandonOwnership(1)
				require.True(t, hadWork)
			},
			want: expectation{stillOwned: false},
		},
		// The shutdown branches added by the closing-review round. The
		// previous review pointed out this table had stopped covering the
		// branches it promises to cover — these close that gap. All of them
		// return "no more work" while state stays mbOwned (Run's
		// abandonOwnership defer ends the era shortly after), so they must
		// still satisfy the no-stale-handle half of the invariant.
		{
			name: "drainAfterCancel/hard-stopped refuses and clears the handle",
			run: func(t *testing.T, mb *mailbox) {
				repl := SessionAgentCall{SessionID: "s1"}
				mb.replacement = &repl
				mb.hardStop()
				_, ok := mb.drainAfterCancel()
				require.False(t, ok, "a hard-stopped mailbox must not hand the turn loop more work")
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "drainOrReleaseFinal/hard-stopped ends the era and still releases",
			run: func(t *testing.T, mb *mailbox) {
				mb.submitted = []SessionAgentCall{{SessionID: "s1"}}
				mb.hardStop()
				released := false
				_, hasNext, orphaned, err := mb.drainOrReleaseFinal(1, func() error {
					released = true
					return nil
				})
				require.NoError(t, err)
				require.False(t, hasNext, "a hard-stopped mailbox must not continue into another turn")
				require.True(t, released,
					"shutdown must still release the OS lock — refusing to continue is not a reason to leak it")
				require.Empty(t, orphaned, "queued work must NOT be handed out as orphaned on a stopped mailbox — "+
					"hardStop's own contract is that queued work is discarded (lost), not restarted")
			},
			want: expectation{stillOwned: false},
		},
		// interruptAndReplace is deliberately NOT in this table. The
		// invariant here is about operations that hand work back to a turn
		// loop or end an era; interruptAndReplace's refusal path is a pure
		// early return that mutates nothing, so the blanket "current.cancel
		// must be nil" post-condition does not apply to it (and asserting it
		// would just force a pointless write). Its stopped behaviour is
		// covered directly by TestInterruptAndReplace_RefusesOnStoppedMailbox.
		//
		// #307 (P1-2 follow-up) update: interruptAndReplace's OWNED branch
		// (state == mbOwned && !stopped) is also not added here, even though
		// it is no longer a pure early return — it records
		// mb.replacement and returns mb.current.cancel as-is (nil or
		// non-nil, whichever it already was). It still never WRITES to
		// state/current.cancel/dispatcherCancel, the three fields this
		// table's postcondition governs, so there is nothing for the
		// blanket "current.cancel must be nil" (or the mbOwned/mbIdle
		// dichotomy) to assert about it: it neither hands work back to a
		// turn loop (that's reclaimReplacementOrKeep's job, below) nor ends
		// an era. Covered directly instead by
		// TestMailbox_InterruptAndReplace_OwnedButNoLiveGeneration_RecordsReplacementWithoutDispatcherFallback
		// and TestMailbox_InterruptAndReplace_OwnedRecordsReplacementAndReturnsCurrentCancel
		// (mailbox_test.go), which assert its actual postcondition:
		// mb.replacement is set, and the returned cancel is exactly
		// mb.current.cancel (never a dispatcherCancel fallback — that
		// fallback is precisely what #307 removed, since dispatcherCancel
		// is runCancel and firing it poisons every future turn's context,
		// not just the possibly-nonexistent current generation).
		//
		// reclaimReplacementOrKeep is likewise NOT in this table, for the
		// same reason injectIfBusy already isn't (see that method's own
		// doc): it does not change ownership state at all — state, current,
		// and dispatcherCancel are all left untouched. Its job is choosing
		// WHICH SessionAgentCall the next generation runs (the loop's stale
		// `call` vs. a same-window mb.replacement), not deciding whether
		// the era continues or ends — that decision was already made by
		// whichever drain call produced `call` in the first place. On the
		// replacement-hit branch it also pushes the pre-empted `call` back
		// onto the FRONT of mb.submitted (closing review: an earlier draft
		// discarded it instead, the #283/P0-2 class of bug — "interrupt
		// deletes the very message it's supposed to queue behind"), but
		// mb.submitted's contents are not part of this table's ownership-
		// state postcondition either. Covered directly by
		// TestMailbox_ReclaimReplacementOrKeep_* (mailbox_test.go), including
		// the multi-element-queue case that specifically catches the discard
		// defect.

		// #296/P1-C, corrected per #297 review: drainOrReleaseFinal passes
		// THROUGH mbReleasing on its way to mbIdle — it NEVER re-emerges as
		// mbOwned once release() has been invoked, no matter what lands in
		// mb.submitted/mb.replacement during the window (see this function's
		// own doc: release() has already run, so there is no OS lock left to
		// keep a turn loop going under — an earlier draft of this fix handed
		// such work back as hasNext==true/mbOwned, which meant the turn loop
		// kept running turns with the session's OS lock already released,
		// letting a second process acquire the same lock concurrently).
		//
		// These three rows assert the CORRECTED contract: work that races
		// into the mailbox during the release() window always ends up in the
		// `orphaned` return value with the era ending at mbIdle, EXCEPT when
		// hardStop also landed in that same window, in which case it is
		// discarded (not orphaned) per hardStop's existing contract.
		{
			name: "drainOrReleaseFinal/submitted lands during the release() window: orphaned, era still ends",
			run: func(t *testing.T, mb *mailbox) {
				release := func() error {
					mb.mu.Lock()
					mb.submitted = append(mb.submitted, SessionAgentCall{SessionID: "s1", Prompt: "raced in"})
					mb.mu.Unlock()
					return nil
				}
				_, hasNext, orphaned, err := mb.drainOrReleaseFinal(1, release)
				require.NoError(t, err)
				require.False(t, hasNext, "work that lands in mb.submitted while release() is running must NOT "+
					"be handed back to the turn loop — the OS lock is already gone by the time it is noticed")
				require.Equal(t, []SessionAgentCall{{SessionID: "s1", Prompt: "raced in"}}, orphaned,
					"the racing call must be returned as orphaned so the caller can restart it under a fresh lock")
			},
			want: expectation{stillOwned: false},
		},
		{
			name: "drainOrReleaseFinal/replacement lands during the release() window: orphaned, era still ends",
			run: func(t *testing.T, mb *mailbox) {
				release := func() error {
					mb.mu.Lock()
					repl := SessionAgentCall{SessionID: "s1", Prompt: "raced in as replacement"}
					mb.replacement = &repl
					mb.mu.Unlock()
					return nil
				}
				_, hasNext, orphaned, err := mb.drainOrReleaseFinal(1, release)
				require.NoError(t, err)
				require.False(t, hasNext, "a replacement recorded while release() is running must NOT be handed "+
					"back to the turn loop — the OS lock is already gone by the time it is noticed")
				require.Equal(t, []SessionAgentCall{{SessionID: "s1", Prompt: "raced in as replacement"}}, orphaned,
					"the racing replacement must be returned as orphaned so the caller can restart it under a fresh lock")
			},
			want: expectation{stillOwned: false},
		},
		{
			name: "drainOrReleaseFinal/hard-stopped during the release() window discards the race, not orphaned",
			run: func(t *testing.T, mb *mailbox) {
				release := func() error {
					mb.hardStop()
					mb.mu.Lock()
					mb.submitted = append(mb.submitted, SessionAgentCall{SessionID: "s1"})
					mb.mu.Unlock()
					return nil
				}
				_, hasNext, orphaned, err := mb.drainOrReleaseFinal(1, release)
				require.NoError(t, err)
				require.False(t, hasNext, "hardStop landing during the release() window must override any work "+
					"that also landed there — a shutdown must not hand the turn loop a fresh call")
				require.Empty(t, orphaned, "work that races in alongside a shutdown must be discarded, not "+
					"restarted — restarting it would defeat the shutdown exactly like handing it back would")
			},
			want: expectation{stillOwned: false},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mb := newFixture()
			c.run(t, mb)

			if c.want.stillOwned {
				require.Equal(t, mbOwned, mb.state, "expected ownership to continue (state == mbOwned)")
				require.NotNil(t, mb.dispatcherCancel, "dispatcherCancel must stay live while ownership continues "+
					"— Cancel()'s fallback depends on it")
			} else {
				require.Equal(t, mbIdle, mb.state, "expected the ownership era to end (state == mbIdle)")
				require.Nil(t, mb.dispatcherCancel, "dispatcherCancel must be cleared once the era ends")
			}

			// THE invariant every one of the four found bugs violated, on one
			// branch at a time: current.cancel must never survive this call
			// still pointing at the fixture's original (already-spent, in
			// production terms) cancel func. A future branch added to any of
			// these methods that forgets this clear will fail HERE, without
			// needing a fifth review round to notice it by hand.
			require.Nil(t, mb.current.cancel, "current.cancel must never survive a mailbox mutator call as a stale "+
				"handle — this is the exact postcondition rounds 9-13 kept finding violated one branch at a time; "+
				"a non-nil value here means Cancel()/InterruptAndReplace() would silently invoke a spent generation's "+
				"cancel func instead of ever reaching the dispatcherCancel fallback")
		})
	}
}

// TestMailbox_BeginCompact_Postconditions verifies the postconditions of
// beginCompact — the atomic acquisition method added by #268/P0-4 for
// compaction ownership. Unlike the drain/release/abandon operations in the
// main invariant table (whose postcondition is current.cancel == nil),
// beginCompact STARTS an era: its postcondition is the opposite —
// current.cancel must be freshly set (non-nil), state must be mbOwned, and
// dispatcherCancel must be set. It is deliberately in a separate test
// function rather than the stale-handle table because the assertion is
// different.
func TestMailbox_BeginCompact_Postconditions(t *testing.T) {
	t.Run("acquires idle mailbox", func(t *testing.T) {
		mb := &mailbox{
			state: mbIdle,
		}
		myCancel := func() {}
		epoch, ok := mb.beginCompact(myCancel)
		require.True(t, ok, "beginCompact on an idle mailbox must succeed")
		require.NotZero(t, epoch, "epoch must be non-zero after acquisition")
		require.Equal(t, mbOwned, mb.state, "state must be mbOwned after acquisition")
		require.NotNil(t, mb.current.cancel, "current.cancel must be set after acquisition "+
			"— Cancel(sessionID) targets it instead of the old synthetic key")
		require.NotNil(t, mb.dispatcherCancel, "dispatcherCancel must be set after acquisition "+
			"— Cancel()'s fallback depends on it")
	})

	t.Run("refuses owned mailbox", func(t *testing.T) {
		existingCancel := func() {}
		mb := &mailbox{
			state:            mbOwned,
			epoch:            5,
			dispatcherCancel: existingCancel,
			current:          generation{id: 5, cancel: existingCancel},
		}
		_, ok := mb.beginCompact(func() {})
		require.False(t, ok, "beginCompact on an owned mailbox must fail")
		require.Equal(t, mbOwned, mb.state, "state must be unchanged")
		require.Equal(t, uint64(5), mb.epoch, "epoch must be unchanged")
		require.NotNil(t, mb.current.cancel, "current.cancel must be unchanged (non-nil)")
		require.NotNil(t, mb.dispatcherCancel, "dispatcherCancel must be unchanged (non-nil)")
	})

	t.Run("refuses stopped mailbox", func(t *testing.T) {
		mb := &mailbox{
			state:   mbIdle,
			stopped: true,
		}
		_, ok := mb.beginCompact(func() {})
		require.False(t, ok, "beginCompact on a stopped mailbox must fail")
		require.Equal(t, mbIdle, mb.state, "state must be unchanged")
		require.Nil(t, mb.current.cancel, "current.cancel must be unchanged (nil)")
	})

	t.Run("two beginCompact calls are mutually exclusive", func(t *testing.T) {
		mb := &mailbox{state: mbIdle}
		_, ok1 := mb.beginCompact(func() {})
		require.True(t, ok1, "first beginCompact must succeed")
		_, ok2 := mb.beginCompact(func() {})
		require.False(t, ok2, "second beginCompact on the same mailbox must fail "+
			"— this is the core P0-4 fix: two concurrent compactions are impossible")
	})
}
