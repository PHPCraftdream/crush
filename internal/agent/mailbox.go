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
	// §3). drainOrReleaseFinal (round 11 review, HIGH-1; round 14 review,
	// P0-A fix) calls it at the analogous point too — right after the epoch
	// check, before ANY queue (replacement, submitted, legacy) is consulted;
	// run_reclaim_cancel_test.go relies on this exact position to land a real
	// sa.Cancel call inside the reclaim window (that test's scenario has
	// mb.submitted empty and mb.replacement nil, so the flow still reaches
	// checkLegacy after the seam). It exists solely so a test can
	// deterministically land a concurrent submit() call (or, via
	// drainOrReleaseFinal, a concurrent Cancel/InterruptAndReplace call)
	// inside that instant: since mu is still held while testDrainSeam runs,
	// a concurrent caller needing mu blocks on mu.Lock() until testDrainSeam
	// returns, making the interleaving reproducible on every run instead of
	// relying on goroutine-scheduling luck. nil in all non-test construction
	// paths (the zero value of mailbox), so it changes no production behavior
	// — mirrors the existing onFire test-seam idiom already used by
	// stream_watchdog.go elsewhere in this package.
	testDrainSeam func()

	// epoch identifies the current OWNERSHIP ERA: bumped every time state
	// transitions mbIdle -> mbOwned (a NEW caller becomes owner), never on
	// a continuing turn within the same era (beginGeneration's turn-level
	// `current.id` is a different counter — see its own doc). Round 9
	// review, BLOCKER-2: without this, drainOrRelease had no way to tell
	// "am I still the current owner calling this for the first time" apart
	// from "a stale/duplicate release call from an era that has already
	// ended" — Run's cleanup defer calls drainOrRelease unconditionally on
	// every return path, including ones where runTurn's own internal drain
	// already released ownership (or where an EARLY error return skipped
	// it entirely, leaving the era still open). A caller now presents the
	// epoch IT was granted ownership under; drainOrRelease is a safe no-op
	// if that epoch no longer matches — either because the era already
	// ended and moved on (a concurrent submit became the new owner) or
	// because it never held ownership in the first place.
	epoch uint64

	// stopped is the one-way hard-stop latch set by hardStop (process
	// shutdown / CancelAll). Once set, every "keep running" branch of every
	// drain refuses to hand work back to a turn loop, so a cancelled turn
	// can no longer pull the NEXT queued call in and start a fresh provider
	// request while the process is trying to exit.
	//
	// Round 14 review, P0-C: CancelAll used to call the ordinary
	// Cancel(sessionID), which cancels only the CURRENT generation. The
	// cancel-error branch in runTurn then immediately drained
	// replacement/submitted and looped into the next turn — on a runCtx
	// that was never cancelled, since Cancel deliberately does not touch
	// dispatcherCancel. Shutdown therefore did the opposite of stopping:
	// it cancelled turn N and started turn N+1, and App.Shutdown's bounded
	// wait then tore the DB down underneath a still-running agent.
	//
	// Deliberately one-way (never cleared): a mailbox that has been
	// hard-stopped belongs to a process that is exiting. Mailboxes are
	// per-process, so there is nothing to reset it for.
	stopped bool
}

// hardStop implements the shutdown half of P0-C: it latches the mailbox
// closed and returns the dispatcher cancel (the DURABLE, whole-Run()
// CancelFunc) plus the current generation's cancel, for the caller to
// invoke outside mb.mu.
//
// Unlike Cancel/interruptAndReplace — which deliberately target only the
// current generation so a turn loop survives to run the replacement —
// this is the operation that must actually END the dispatcher: cancelling
// dispatcherCancel unwinds Run() itself, and `stopped` guarantees that on
// the way out no drain hands it another call to run.
//
// Anything still queued is left in place rather than discarded, but be
// clear about what that does and does not buy (closing review, blocker
// 3 — an earlier version of this comment claimed the queue was already
// persisted, which is false).
//
// A queued SessionAgentCall is NOT a DB row: coordinator.buildCall does
// not create one, and the user message is only written in runTurn's
// preamble, i.e. after a turn has already started. Both mb.submitted and
// mb.replacement live purely in this process, and Run's cleanup defer
// merely moves them to a.messageQueue, which is also in-memory. So on
// shutdown a prompt that never reached a turn IS lost, with only a
// slog.Error to show for it.
//
// That is the accepted trade: losing an unstarted prompt is better than
// starting a fresh provider turn while the process is tearing its
// database down. The one case that does survive is
// requeueInterruptMessage's, whose call carries an ExistingMessageID for
// a row `crush sessions inject` already wrote.
//
// Making this genuinely lossless needs the queue to be durable, which is
// a design change well beyond a shutdown latch — tracked separately, not
// papered over here.
func (mb *mailbox) hardStop() (dispatcherCancel, genCancel context.CancelFunc) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.stopped = true
	return mb.dispatcherCancel, mb.current.cancel
}

// isStopped reports whether hardStop has latched this mailbox closed.
func (mb *mailbox) isStopped() bool {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return mb.stopped
}

// submit implements design §3: replaces both tryReserveSession +
// activeRequests.Set (the "am I the new owner" path) and
// messageQueue.Append (the "queue behind the current owner" path) as one
// atomic operation. Returns true when the caller becomes the new owner and
// must run call itself; false when call was appended to the queue for the
// current owner to drain. When becomeOwner is true, epoch is this NEW
// ownership era's id — the caller must present it to every drainOrRelease
// call it makes for the lifetime of its ownership (BLOCKER-2, see the
// epoch field's doc). epoch is meaningless (0) when becomeOwner is false:
// the caller never held ownership and has nothing to release.
func (mb *mailbox) submit(call SessionAgentCall, dispatcherCancel context.CancelFunc) (becomeOwner bool, epoch uint64) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.state == mbIdle {
		mb.state = mbOwned
		mb.dispatcherCancel = dispatcherCancel
		mb.epoch++
		return true, mb.epoch // caller (Run) becomes the new owner, runs call itself
	}
	mb.submitted = append(mb.submitted, call)
	return false, 0 // caller queues and returns nil, exactly like today
}

// drainOrRelease implements design §3: called by the owner at the exact
// point today's code calls messageQueue.PopFront at the end of a turn. If
// anything is queued, it is returned and ownership stays with the caller
// (state remains mbOwned). Otherwise ownership is atomically released
// (state flips to mbIdle) in the SAME critical section as the emptiness
// check, closing the P0-3 lost-wakeup window.
//
// epoch must be the value submit() returned when granting the caller
// ownership (BLOCKER-2). If the mailbox's current epoch has since moved on
// — a different, later caller became owner, e.g. because THIS call is a
// stale duplicate release from Run's unconditional cleanup defer running
// after runTurn's own drain already ended the era — this is a safe no-op:
// it must not touch submitted/state/current, which belong to that later
// owner now.
func (mb *mailbox) drainOrRelease(epoch uint64) (SessionAgentCall, bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.epoch != epoch {
		return SessionAgentCall{}, false
	}
	if len(mb.submitted) > 0 {
		next := mb.submitted[0]
		mb.submitted = mb.submitted[1:]
		// Same stale-cancel shape as drainOrReleaseFinal's/drainAfterCancel's
		// equivalent branches (rounds 11-13) — this function has no live
		// production caller today (its only wrapper, releaseSessionReservation,
		// has zero call sites), but its own doc explicitly reserves it for a
		// future caller with no OS lock in play, so it gets the same
		// defensive clear rather than being a silent fifth instance waiting
		// to be reintroduced.
		mb.current.cancel = nil
		return next, true // caller runs another turn; state stays mbOwned
	}
	// Nothing queued AT THE INSTANT OF THIS CHECK, and — because mu is
	// held — nothing CAN be queued between this check and the state flip
	// below.
	if mb.testDrainSeam != nil {
		mb.testDrainSeam()
	}
	mb.state = mbIdle
	mb.current.cancel = nil // preserve id (monotonic, see the field doc) — only the cancel func is spent
	mb.dispatcherCancel = nil
	return SessionAgentCall{}, false
}

// drainOrReleaseFinal implements round 11 review, HIGH-1: it replaces the
// former two-step "release to mbIdle under mb.mu, THEN — outside mb.mu, in
// drainOrReleaseMerged — separately check the legacy messageQueue and only
// THEN release the OS session lock much later, once Run's whole call stack
// unwinds" shape with one atomic operation, so that "mb.state == mbIdle"
// (observable in-process via IsSessionBusy/submit/CancelAll/IsBusy, all of
// which just read mb.state under mb.mu) can no longer become true while the
// OS-level session.SessionLock this same call is still holding is not yet
// released.
//
// Without this, a same-process caller that saw IsSessionBusy(sessionID) ==
// false and tried to become the new owner via submit() — legitimately, that
// IS what "not busy" is supposed to mean — could reach
// session.TryAcquireSessionLock and get a spurious SessionLockBusyError from
// its own process's prior turn, which hadn't finished unwinding
// (runTurn's deferred wg.Wait() for title generation, then Run's own
// deferred lk.Release()) yet. Because tryReserveSession's "someone already
// owns it" branch never re-queues (submit()'s owner-branch assumes the
// eventual real owner will drain it), that manifested as a silently
// dropped user message, not a retryable error.
//
// checkLegacy is called (still holding mb.mu) ONLY when mb.submitted is also
// empty — it must perform the legacy-messageQueue.PopFront check (or
// equivalent) and return the same (call, found) shape drainOrRelease itself
// returns for its own submitted-queue branch. It must NOT call back into
// this mailbox (mb.mu is not reentrant) — csync.KeyedQueue.PopFront (the
// only real caller, see drainOrReleaseMerged) is backed by its own
// independent sync.Mutex with no callbacks of its own, so nesting it inside
// mb.mu here is safe (confirmed by reading internal/csync/keyedqueue.go: no
// method calls out to anything).
//
// reclaimDispatcherCancel is stored into mb.dispatcherCancel, and
// mb.current.cancel is explicitly cleared, both still under mb.mu, ONLY on
// the checkLegacy-hit branch — round 11 review, MEDIUM-1. Before this,
// reclaiming from the legacy queue (via the historical reclaimSameEra,
// called with a literal nil dispatcherCancel) left mb.dispatcherCancel nil
// AND left mb.current.cancel pointing at the JUST-FINISHED turn's own
// cancel func — already invoked once by that turn and therefore inert, but
// not nil. Both defects independently made Cancel()/InterruptAndReplace()
// silently ineffective for a call landing before the reclaimed turn's own
// beginGeneration: Cancel()'s `if genCancel == nil { fallback to
// dispatcherCancel }` never even reached the fallback (genCancel was the
// stale, non-nil, spent prior-turn cancel), so fixing only dispatcherCancel
// is not sufficient on its own — both must be set here. Passing the
// caller's runCancel as reclaimDispatcherCancel — the SAME whole-call
// CancelFunc tryReserveSession/submit() already store in this exact field
// for the analogous "no live generation yet" window — closes the
// dispatcherCancel half; explicitly nil-ing current.cancel closes the rest.
//
// release is called (still holding mb.mu) ONLY when BOTH mb.submitted and
// checkLegacy came back empty — i.e. exactly the branch that used to flip
// mb.state to mbIdle. It must release the OS-level session lock (or be a
// no-op when a.dataDir == "" and no lock was ever acquired — see
// drainOrReleaseMerged's call site). Any error it returns is surfaced to the
// caller for logging; it never blocks the state flip to mbIdle, mirroring
// how Run's own `defer lk.Release()` today only logs a Release failure
// rather than treating it as fatal — a failed unlock must not leave the
// mailbox permanently mbOwned with nobody left to retry it.
//
// The two callbacks are invoked in this exact order — checkLegacy before
// release — precisely so a hit in the legacy queue can skip releasing the
// OS lock entirely and keep it held for the reclaimed turn, instead of
// releasing-then-reacquiring (which would open a real cross-process race:
// see the historical reclaimSameEra doc, superseded by this function, for
// why that shape is rejected).
//
// Priority of branch evaluation within this function is now explicitly
// "replacement -> submitted -> legacy -> release" (round 14 review, P0-A
// fix), closing a race where a late interrupt whose cancel lost to
// Stream's normal completion could strand the replacement: mb.replacement
// was set by interruptAndReplace under mb.mu, but agent.Stream returned
// nil (not context.Canceled), so runTurn took the normal-completion path
// instead of the isCancelErr branch's drainAfterCancel. The replacement
// check here ensures the replacement is recovered without releasing the
// OS lock or entering the legacy queue, preserving the same atomic state
// machine semantics drainAfterCancel already provides for the cancel path.
//
// Two accepted trade-offs from round 12 review, recorded here rather than
// left implicit:
//   - The OS lock hand-off point moved earlier than it used to be: before
//     this function existed, the lock stayed held through the REST of
//     runTurn's own deferred cleanup (stream watchdog stop, waiting on the
//     title-generation goroutine) and Run's own trailing defers. Now it is
//     released the instant both queues are confirmed empty, so a title
//     rename (sessions.Rename, including its context.WithoutCancel fallback)
//     or a final cost increment can still be in flight AFTER a different
//     process has already acquired the lock for the same session. Both
//     writes are narrowly-scoped, additive/idempotent SQL updates (not
//     read-modify-write on shared state), so the worst case is a cosmetic
//     title race, not data loss — but it is a real, deliberate narrowing of
//     what the OS lock's hold-time used to cover.
//   - release() (a real syscall: unlock, close, and — via
//     session.SessionLock.Release — a metadata truncate/remove) now runs
//     WHILE mb.mu is held, so disk I/O briefly blocks every other in-process
//     reader of this mailbox (IsSessionBusy, IsBusy, CancelAll, Cancel,
//     QueuedPrompts) for that session. This is required for the atomicity
//     HIGH-1 depends on (see above) and is expected to be very fast in
//     practice, but it is a genuinely new coupling between mailbox-state
//     reads and disk latency that didn't exist before this function.
func (mb *mailbox) drainOrReleaseFinal(
	epoch uint64,
	checkLegacy func() (SessionAgentCall, bool),
	reclaimDispatcherCancel context.CancelFunc,
	release func() error,
) (call SessionAgentCall, hasNext bool, releaseErr error) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.epoch != epoch {
		return SessionAgentCall{}, false, nil
	}
	// Hard-stopped (P0-C): end the era rather than continuing into another
	// turn. Fall through to the release path below so the OS lock is still
	// released and the mailbox still lands on mbIdle — a shutdown must not
	// leave the session marked busy with the lock held.
	if mb.stopped {
		if release != nil {
			releaseErr = release()
		}
		mb.state = mbIdle
		mb.current.cancel = nil
		mb.dispatcherCancel = nil
		return SessionAgentCall{}, false, releaseErr
	}
	// Seam for deterministic test interleaving: fires right after the epoch check,
	// before ANY queue (replacement, submitted, legacy) is consulted — this is the
	// earliest deterministic pause point. Existing tests (e.g. run_reclaim_cancel_test.go)
	// that relied on the seam firing only when mb.submitted is empty are unaffected
	// because their scenarios always have mb.submitted empty and mb.replacement nil,
	// so the flow still reaches the same checkLegacy/release branches after the seam.
	if mb.testDrainSeam != nil {
		mb.testDrainSeam()
	}
	// P0-A fix: a late interrupt whose cancel lost the race to Stream's normal
	// completion lands here. mb.replacement was recorded by interruptAndReplace
	// under mb.mu, but agent.Stream returned nil (not context.Canceled), so runTurn
	// took the normal-completion path instead of the isCancelErr branch's
	// drainAfterCancel. Without this check, the replacement is silently stranded:
	// the drain releases ownership and the OS lock, and Run's defer can only move
	// the payload to the legacy queue (where no runner exists) or no-op it under a
	// new era. Priority "replacement -> submitted -> legacy -> release" is now
	// ONE atomic state machine operation, matching drainAfterCancel's existing
	// ordering. mb.current.cancel = nil — same invariant as every other "keep running"
	// branch (see mailbox_invariant_test.go). State stays mbOwned, OS lock untouched,
	// epoch NOT bumped — same semantics as the existing keep-running branches.
	if mb.replacement != nil {
		next := *mb.replacement
		mb.replacement = nil
		mb.current.cancel = nil
		return next, true, nil
	}
	if len(mb.submitted) > 0 {
		next := mb.submitted[0]
		mb.submitted = mb.submitted[1:]
		// mb.current.cancel must be cleared here too (round 12 review,
		// finding A — the SAME MEDIUM-1 shape as the checkLegacy branch
		// below, just on the more commonly hit mb.submitted path): by the
		// time this branch runs, it still holds the JUST-FINISHED turn's
		// own genCtx cancel — already invoked once via runTurn's
		// unconditional `cancel()` call right before this drain — inert but
		// NOT nil, which defeats Cancel()/InterruptAndReplace()'s
		// current.cancel==nil fallback gate exactly like the original
		// defect, for the whole window until the next turn's own
		// beginGeneration (which can be as long as title generation takes).
		// dispatcherCancel is left untouched here (unlike the release
		// branch below): it's already the live runCancel from submit()/the
		// loop's own re-arm, still valid for the caller's own remaining
		// lifetime — nothing to reset.
		mb.current.cancel = nil
		return next, true, nil // caller runs another turn; state stays mbOwned, lock stays held
	}
	if checkLegacy != nil {
		if next, ok := checkLegacy(); ok {
			// Reclaimed from the legacy queue under the SAME era — mirrors
			// the historical reclaimSameEra's contract (no epoch bump, state
			// stays mbOwned) but done here, still under mb.mu, instead of via
			// a separate release-then-reclaim round trip: the OS lock is
			// simply never released for this handoff.
			// mb.state is set explicitly here (round 12 review, finding C),
			// even though it's already mbOwned on every path that can reach
			// this line today (this function has exactly one call site per
			// era). The old reclaimSameEra both checked AND set state for
			// exactly this reason: relying on an invariant that isn't
			// locally re-asserted is fragile — epoch is not bumped by
			// release, so "epoch matches && state == mbIdle" is a reachable
			// combination in general (it's what the release branch below
			// produces), and a future second call site for this function
			// (e.g. once #281/#284/#285 finish the mailbox migration) could
			// plausibly land here with state already mbIdle, silently
			// granting two owners of the same session with the OS lock
			// already released. Setting it explicitly costs nothing today
			// and removes that whole class of future mistake.
			mb.state = mbOwned
			mb.dispatcherCancel = reclaimDispatcherCancel
			// mb.current.cancel must ALSO be cleared here (round 11 review,
			// MEDIUM-1 — caught by this fix's own regression test failing
			// even with dispatcherCancel populated): it still holds the
			// JUST-FINISHED turn's own cancel func, already invoked once
			// (runTurn's `cancel()` call before this drain runs) and
			// therefore inert, but NOT nil. Cancel()'s fallback to
			// dispatcherCancel is gated on `mb.current.cancel == nil` — if
			// it is left non-nil here, Cancel() calls the spent, harmless
			// prior-turn cancel func instead of ever reaching the fallback,
			// silently failing to interrupt the reclaimed turn exactly like
			// the original defect, just via a different nil-check path.
			// Preserve current.id (monotonic, see the generation field's
			// doc) — only the spent cancel func is cleared, mirroring
			// drainOrRelease's own submitted-empty branch below.
			mb.current.cancel = nil
			return next, true, nil
		}
	}
	// Nothing queued anywhere AT THE INSTANT OF THIS CHECK, and — because mu
	// is held for the whole of this function, including the release call
	// below — nothing CAN be queued (submit() would block on mb.mu) and no
	// same-process observer CAN see mb.state flip to mbIdle before the OS
	// lock this call is about to release is actually gone.
	if release != nil {
		releaseErr = release()
	}
	mb.state = mbIdle
	mb.current.cancel = nil // preserve id (monotonic, see the field doc) — only the cancel func is spent
	mb.dispatcherCancel = nil
	return SessionAgentCall{}, false, releaseErr
}

// abandonOwnership implements Run's cleanup-defer release path (round 9
// review, BLOCKER-2a). It is NOT the same operation as drainOrRelease:
// drainOrRelease is called by an owner's OWN live turn loop, which can
// choose to keep running ("found something queued, stay owned, run it as
// the next turn"). Run's defer calls this instead specifically because it
// has NO live turn loop left — it fires on every return from Run(),
// including early bail-outs (OS-lock acquisition failure) and any
// early-return inside runTurn that skipped runTurn's own final drain (an
// error path returning before reaching it). In both cases there is nobody
// left to hand a "keep running" answer to, so — unlike drainOrRelease —
// this ALWAYS ends the era at mbIdle if epoch still matches, logging
// (rather than silently keeping) anything it finds. Before this method
// existed, Run's defer called the ordinary drainOrRelease/
// drainOrReleaseMerged, which — on finding something in submitted — left
// state == mbOwned with nobody running it: the session was silently wedged
// permanently busy (IsSessionBusy true forever, every future submit()
// silently queues behind a owner that will never drain it again).
//
// epoch behaves exactly as in drainOrRelease: a mismatch means this era
// already ended (a concurrent submit became the new owner, or drainOrRelease
// already released it) — a safe no-op that touches nothing, since whatever
// is in submitted/replacement now belongs to that later owner.
//
// The legacy messageQueue is deliberately NOT touched here (unlike
// drainOrReleaseMerged): its entries are not epoch-scoped and survive
// independently of this mailbox era, so leaving them queued is correct —
// the NEXT Run() call for this session will reach a normal end-of-turn
// drainOrReleaseMerged and pick them up then, not lose them.
func (mb *mailbox) abandonOwnership(epoch uint64) (dropped []SessionAgentCall, hadWork bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.epoch != epoch {
		return nil, false
	}
	dropped = mb.submitted
	if mb.replacement != nil {
		dropped = append(dropped, *mb.replacement)
	}
	mb.submitted = nil
	mb.replacement = nil
	mb.state = mbIdle
	mb.current.cancel = nil // preserve id (monotonic, see the field doc) — only the cancel func is spent
	mb.dispatcherCancel = nil
	return dropped, len(dropped) > 0
}

// interruptAndReplace implements design §4: the coordinator's single entry
// point for "interrupt and replace", replacing QueueMessage+Cancel as one
// atomic mailbox operation. When nobody owns the session, it returns
// hadOwner=false and does nothing further (the caller should instead start
// a fresh Run() with call directly). When a turn is in progress, it
// durably records call as the replacement to be consumed by the NEXT
// generation, and returns a cancel func the caller can invoke to interrupt
// the in-flight generation (outside the mailbox lock).
func (mb *mailbox) interruptAndReplace(call SessionAgentCall) (context.CancelFunc, bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.state != mbOwned || mb.stopped {
		// Nobody running: behave like a plain submit that also happens to
		// return "no cancel needed" — the caller (coordinator) then starts
		// a fresh Run() with call directly instead of relying on drain.
		//
		// `stopped` counts as "nobody running" too (closing review, blocker
		// 2), even while state is still mbOwned. Recording a replacement on
		// a hard-stopped mailbox would report success to the caller — and
		// through it "queued" to the web client — for a payload both drains
		// are now guaranteed to refuse, leaving it to be swept into an
		// in-memory queue by a process that is exiting. Returning false
		// instead routes the caller to its own no-owner path, where Run
		// refuses with ErrAgentShuttingDown so the failure is visible
		// rather than silent.
		return nil, false
	}
	// Durably record the replacement FIRST, under the same lock that is
	// about to cancel the current generation. There is no window between
	// "replacement is recorded" and "current generation is cancelled" for
	// an external observer to land in, because both happen before mu is
	// released.
	mb.replacement = &call
	// Fall back to dispatcherCancel when no generation is live yet (round 9
	// review, MEDIUM-1 — mirrors Cancel's own fallback, agent.go's Cancel
	// doc explains the exact window: between submit() granting ownership
	// and the loop's first beginGeneration, current.cancel is nil for the
	// whole OS-lock preamble). Without this, an interrupt landing in that
	// window returned (nil, true) — success, with NOTHING actually
	// cancelled — so the turn ran to completion on the ORIGINAL prompt and
	// the replacement was silently stranded until some later, unrelated
	// cancel eventually surfaced it out of order (HIGH-2).
	cancel := mb.current.cancel
	if cancel == nil {
		cancel = mb.dispatcherCancel
	}
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

	// Hard-stopped (P0-C): the process is shutting down, so refuse to hand
	// the turn loop another call. This is THE branch that made CancelAll
	// start turn N+1 instead of stopping — it runs immediately after the
	// cancel CancelAll itself caused. Queued work is left in place rather
	// than discarded — but see hardStop's doc for what that does and does
	// not buy: a queued call is NOT a DB row, so an unstarted prompt is
	// genuinely lost when the process exits. Leaving it is the lesser evil,
	// not a save.
	if mb.stopped {
		// Clear the spent generation handle on the way out, like the two hit
		// branches below (closing-review follow-up). This branch returns "no
		// more work" while state is still mbOwned, which is exactly the
		// stale-cancel shape rounds 9-13 found five times: left set, a
		// Cancel landing before Run's abandonOwnership defer catches up
		// invokes an already-spent handle instead of falling through to
		// dispatcherCancel. The window is short — so was every one of those.
		mb.current.cancel = nil
		return SessionAgentCall{}, false
	}

	// Both hit branches below clear mb.current.cancel (round 13 review,
	// the fourth instance of the same shape rounds 11/12 already fixed on
	// drainOrReleaseFinal's two branches): by the time this is called
	// (agent.go's isCancelErr block, right before its own `cancel()` call),
	// mb.current.cancel is the just-cancelled generation's own genCtx
	// cancel func — spent (whether invoked by the external Cancel() that
	// caused isCancelErr, or by this function's own caller a line later)
	// but NOT nil. Left untouched, it defeats Cancel()/InterruptAndReplace()'s
	// current.cancel==nil fallback gate exactly like every prior instance,
	// for the window until the next turn's own beginGeneration —
	// arguably the worst of the four, since this is precisely the "user
	// already cancelled once and wants the replacement instead" path: a
	// second Cancel/interrupt landing in this window would silently no-op
	// while the replacement turn streams anyway. dispatcherCancel is left
	// untouched, same reasoning as the other two fixed branches — it's
	// already the live runCancel, nothing to reset.
	if mb.replacement != nil {
		next := *mb.replacement
		mb.replacement = nil
		mb.current.cancel = nil
		return next, true
	}
	if len(mb.submitted) > 0 {
		next := mb.submitted[0]
		mb.submitted = mb.submitted[1:]
		mb.current.cancel = nil
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
