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
				_, hasNext, err := mb.drainOrReleaseFinal(1, nil, nil, nil)
				require.NoError(t, err)
				require.True(t, hasNext)
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "drainOrReleaseFinal/replacement branch keeps ownership",
			run: func(t *testing.T, mb *mailbox) {
				repl := SessionAgentCall{SessionID: "s1"}
				mb.replacement = &repl
				_, hasNext, err := mb.drainOrReleaseFinal(1, nil, nil, nil)
				require.NoError(t, err)
				require.True(t, hasNext)
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "drainOrReleaseFinal/checkLegacy reclaim keeps ownership",
			run: func(t *testing.T, mb *mailbox) {
				checkLegacy := func() (SessionAgentCall, bool) {
					return SessionAgentCall{SessionID: "s1"}, true
				}
				_, hasNext, err := mb.drainOrReleaseFinal(1, checkLegacy, func() {}, nil)
				require.NoError(t, err)
				require.True(t, hasNext)
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "drainOrReleaseFinal/both empty releases and ends the era",
			run: func(t *testing.T, mb *mailbox) {
				_, hasNext, err := mb.drainOrReleaseFinal(1, func() (SessionAgentCall, bool) {
					return SessionAgentCall{}, false
				}, nil, func() error { return nil })
				require.NoError(t, err)
				require.False(t, hasNext)
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
			name: "abandonOwnership/empty always ends the era",
			run: func(t *testing.T, mb *mailbox) {
				_, hadWork := mb.abandonOwnership(1)
				require.False(t, hadWork)
			},
			want: expectation{stillOwned: false},
		},
		{
			name: "abandonOwnership/with queued work still always ends the era",
			run: func(t *testing.T, mb *mailbox) {
				mb.submitted = []SessionAgentCall{{SessionID: "s1"}}
				_, hadWork := mb.abandonOwnership(1)
				require.True(t, hadWork)
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
