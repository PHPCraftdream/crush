package agent

import (
	"context"
	"sync"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
)

// mailbox is the per-session owner/mailbox state machine described in
// docs/plans/2026-08-04-session-owner-mailbox-design.md. It is introduced
// here (stage 1/5 of the migration) purely as an additive, unwired type:
// nothing in sessionAgent calls into it yet, and none of the existing
// activeRequests/messageQueue/injectQueue/sessionStartMu structures are
// touched. See the design doc for the full rationale; this file implements
// exactly the seven methods specified in sections 3, 4, and 5 there
// (submit, drainOrRelease, interruptAndReplace, drainAfterCancel, inject,
// drainInjects, beginGeneration) with no behavior deviation.
type mailboxState int

const (
	mbIdle      mailboxState = iota // no owner, nothing queued
	mbOwned                         // a turn loop holds ownership
	mbReleasing                     // owner is mid drain-or-release transition (see design §3)
)

// generation identifies one in-flight turn (or, once #268 lands, one
// compact) within a session's mailbox. id is monotonic per session,
// bumped every time beginGeneration is called; cancel cancels ONLY this
// generation's context, never the whole dispatcher (see design §4).
type generation struct {
	id     uint64
	cancel context.CancelFunc
}

// pendingInject is one message queued via mailbox.inject, stamped with the
// generation id that was current at submit time so drainInjects (design §5)
// can decide which turns are responsible for splicing it into
// prepared.Messages.
type pendingInject struct {
	msg        message.Message
	afterGenID uint64
}

// mailbox holds all per-session ownership/queueing state behind one mutex,
// replacing (once wired, in later stages) activeRequests, messageQueue,
// injectQueue, and the sessionStartMu reservation gate for a single session
// id. See design doc §1 for the full field-by-field rationale.
type mailbox struct {
	mu sync.Mutex // single critical section for ALL fields below

	state mailboxState

	// dispatcherCancel is the durable, call-scoped cancel func — spans the
	// whole Run() call (every turn + every preamble), analogous to today's
	// runCancel registered by tryReserveSession. Never the target of an
	// interrupt; used only by CancelAll/process shutdown.
	dispatcherCancel context.CancelFunc

	// current is the active generation's id+cancel, or the zero value when
	// state != mbOwned. Interrupt/Cancel target THIS, never dispatcherCancel.
	current generation

	// submitted holds pending SessionAgentCall values submitted while
	// owned — replaces messageQueue for the "queue a normal follow-up"
	// case. Kept as an unbounded slice (matching messageQueue's FIFO
	// contract today) rather than a single slot; see design §7 open
	// question 1.
	submitted []SessionAgentCall

	// replacement, when non-nil, is an interrupt-and-replace payload that
	// must be consumed by the NEXT generation the owner starts, and the
	// CURRENT generation must be cancelled to make room for it.
	replacement *SessionAgentCall

	// injects holds messages already persisted to the DB, waiting to be
	// spliced into prepared.Messages by the owner's PrepareStep.
	injects []pendingInject

	// compact holds at most one pending manual-compact request. Present
	// only so a compact submitted while owned is remembered until
	// drain-or-release; not exercised until #268 (design §6) — unused
	// placeholder field for this stage.
	compact *fantasy.ProviderOptions

	// testDrainSeam, when non-nil, is invoked by drainOrRelease AFTER it has
	// observed mb.submitted empty but BEFORE it flips state to mbIdle — i.e.
	// exactly inside the critical section that used to NOT exist as a single
	// atomic unit before this migration (see drainOrRelease's doc and design
	// §3). It exists solely so a test can deterministically land a
	// concurrent submit() call inside that instant: since mu is still held
	// by drainOrRelease while testDrainSeam runs, a concurrent submit()
	// blocks on mu.Lock() until testDrainSeam returns, making the
	// interleaving reproducible on every run instead of relying on
	// goroutine-scheduling luck. nil in all non-test construction paths
	// (the zero value of mailbox), so it changes no production behavior —
	// mirrors the existing onFire test-seam idiom already used by
	// stream_watchdog.go elsewhere in this package.
	testDrainSeam func()
}

// submit implements design §3: replaces both tryReserveSession +
// activeRequests.Set (the "am I the new owner" path) and
// messageQueue.Append (the "queue behind the current owner" path) as one
// atomic operation. Returns true when the caller becomes the new owner and
// must run call itself; false when call was appended to the queue for the
// current owner to drain.
func (mb *mailbox) submit(call SessionAgentCall, dispatcherCancel context.CancelFunc) bool {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.state == mbIdle {
		mb.state = mbOwned
		mb.dispatcherCancel = dispatcherCancel
		return true // caller (Run) becomes the new owner, runs call itself
	}
	mb.submitted = append(mb.submitted, call)
	return false // caller queues and returns nil, exactly like today
}

// drainOrRelease implements design §3: called by the owner at the exact
// point today's code calls messageQueue.PopFront at the end of a turn. If
// anything is queued, it is returned and ownership stays with the caller
// (state remains mbOwned). Otherwise ownership is atomically released
// (state flips to mbIdle) in the SAME critical section as the emptiness
// check, closing the P0-3 lost-wakeup window.
func (mb *mailbox) drainOrRelease() (SessionAgentCall, bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if len(mb.submitted) > 0 {
		next := mb.submitted[0]
		mb.submitted = mb.submitted[1:]
		return next, true // caller runs another turn; state stays mbOwned
	}
	// Nothing queued AT THE INSTANT OF THIS CHECK, and — because mu is
	// held — nothing CAN be queued between this check and the state flip
	// below.
	if mb.testDrainSeam != nil {
		mb.testDrainSeam()
	}
	mb.state = mbIdle
	mb.current = generation{}
	mb.dispatcherCancel = nil
	return SessionAgentCall{}, false
}

// interruptAndReplace implements design §4: the coordinator's single entry
// point for "interrupt and replace", replacing QueueMessage+Cancel as one
// atomic mailbox operation. When nobody owns the session, it returns
// hadOwner=false and does nothing further (the caller should instead start
// a fresh Run() with call directly). When a turn is in progress, it
// durably records call as the replacement to be consumed by the NEXT
// generation, and returns the CURRENT generation's cancel func so the
// caller can cancel it (outside the mailbox lock).
func (mb *mailbox) interruptAndReplace(call SessionAgentCall) (context.CancelFunc, bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.state != mbOwned {
		// Nobody running: behave like a plain submit that also happens to
		// return "no cancel needed" — the caller (coordinator) then starts
		// a fresh Run() with call directly instead of relying on drain.
		return nil, false
	}
	// Durably record the replacement FIRST, under the same lock that is
	// about to cancel the current generation. There is no window between
	// "replacement is recorded" and "current generation is cancelled" for
	// an external observer to land in, because both happen before mu is
	// released.
	mb.replacement = &call
	cancel := mb.current.cancel
	return cancel, true
}

// drainAfterCancel implements design §4: the generation-aware drain called
// by the owner's cancel-handling branch (replacing today's
// messageQueue.PopFront check at the isCancelErr site). It checks
// replacement before submitted, since a replacement is a durable "cancel
// current, then run me next" instruction distinct from a plain queued
// follow-up.
func (mb *mailbox) drainAfterCancel() (SessionAgentCall, bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.replacement != nil {
		next := *mb.replacement
		mb.replacement = nil
		return next, true
	}
	if len(mb.submitted) > 0 {
		next := mb.submitted[0]
		mb.submitted = mb.submitted[1:]
		return next, true
	}
	return SessionAgentCall{}, false
}

// inject implements design §5: appends msg to the pending-inject list,
// stamping it with the mailbox's CURRENT generation id at submit time so
// drainInjects can later decide which generation(s) are responsible for
// splicing it into prepared.Messages. 0 (no owner) is a valid, meaningful
// stamp.
func (mb *mailbox) inject(msg message.Message) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.injects = append(mb.injects, pendingInject{
		msg:        msg,
		afterGenID: mb.current.id,
	})
}

// drainInjects implements design §5: called by the owner's PrepareStep
// (replacing today's unconditional injectQueue.TakeAll) with the
// generation id of the turn currently preparing. Entries stamped at or
// before genID are due now; entries stamped against a strictly future
// generation id are kept for later (not possible via today's call sites,
// kept for forward-compat per the design doc).
func (mb *mailbox) drainInjects(genID uint64) []pendingInject {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	var due, later []pendingInject
	for _, inj := range mb.injects {
		if inj.afterGenID <= genID {
			due = append(due, inj)
		} else {
			later = append(later, inj)
		}
	}
	mb.injects = later
	return due
}

// beginGeneration implements design §5: called by Run's loop before each
// turn (replacing today's activeRequests.Set(call.SessionID, cancel)
// re-arm). It bumps the per-session generation counter and records cancel
// as the new current generation's cancel func, returning the new
// generation id.
func (mb *mailbox) beginGeneration(cancel context.CancelFunc) (genID uint64) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.current.id++
	mb.current.cancel = cancel
	return mb.current.id
}

// clearAll implements design §4's "ClearQueue is the one intentional
// drop-everything operation": clears submitted, replacement, and injects
// together under mu. It does NOT release ownership (state/current/
// dispatcherCancel are untouched) — the owner is still running and merely
// wants its pending queues discarded, not its reservation yanked. The
// sessionAgent.ClearQueue wrapper also clears the still-live legacy
// messageQueue/injectQueue for completeness during the migration.
func (mb *mailbox) clearAll() {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.submitted = nil
	mb.replacement = nil
	mb.injects = nil
}
