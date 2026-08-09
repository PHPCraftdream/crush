package agent

import (
	"context"
	"fmt"
	"sync"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
)

// mailbox is the per-session owner/mailbox state machine described in
// docs/plans/2026-08-04-session-owner-mailbox-design.md. Stages 2.1-2.4
// wired tryReserveSession/releaseSessionReservation, InterruptAndReplace,
// the two-tier context split, and the inject path onto it; #268/P0-4 wired
// compaction onto beginCompact. #308 completed the migration: QueueMessage
// and all its drain consumers moved from the legacy messageQueue to the
// mailbox's own submitted queue, and messageQueue was removed entirely.
// activeRequests is retained for OnStepFinish abort-path cancel lookups
// — the sessionID+"-summarize" synthetic key it used to hold has been
// removed by #268. See the design doc for the full rationale; this file
// implements the mailbox methods (submit, drainOrRelease,
// drainOrReleaseFinal, interruptAndReplace, drainAfterCancel, inject,
// drainInjects, beginGeneration, beginCompact, queue, popFirstSubmitted)
// with no behavior deviation.
type mailboxState int

// mbReleasing is drainOrReleaseFinal's terminal state, entered ONLY on the
// "nothing queued anywhere" branch — the exact instant that used to flip
// straight to mbIdle while still holding mb.mu across the OS-lock release()
// call (#296/P1-C). It exists so that call can drop mb.mu BEFORE running
// release()'s disk I/O (Truncate/Seek/Sync/sidecar-unlink/unlock/Close, no
// context, no timeout) without opening the HIGH-1 gap that I/O-under-mu was
// added to close: a slow/hung filesystem, antivirus, or SMB share must not
// block submit/Cancel/InterruptAndReplace/IsSessionBusy/IsBusy/CancelAll for
// this session, but no same-process observer may ever be able to conclude
// "session is free" before the OS lock is genuinely gone.
//
// mbReleasing threads that needle by being neither mbIdle nor mbOwned, so
// every mutator's existing gate already does the right thing without a new
// per-method mbReleasing check:
//
//   - submit(): gated on `state == mbIdle` to become owner. mbReleasing
//     fails that check exactly like mbOwned does, so a submit() landing
//     during the release window queues into mb.submitted instead of
//     acquiring a lock that isn't free yet. drainOrReleaseFinal's finalize
//     step (below) re-checks mb.submitted/mb.replacement AFTER reacquiring
//     mb.mu — NOT to hand that work back to the current turn loop (rejected,
//     #297 review: release() has already run by then, so there is no OS
//     lock left to keep a turn loop going under — see drainOrReleaseFinal's
//     own doc), but to drain it out as `orphaned` for the caller to restart
//     independently, under its own fresh OS-lock acquisition.
//   - Cancel()/interruptAndReplace(): gated on `current.cancel`/
//     `state == mbOwned`. During mbReleasing, current.cancel is still nil
//     (cleared before the state left mbOwned) and state is not mbOwned, so
//     Cancel() falls back to dispatcherCancel (also nil by this point —
//     the call becomes a documented no-op: there is no generation left to
//     interrupt, the turn loop is already past its provider work and only
//     doing lock teardown) and interruptAndReplace() takes its existing
//     "nobody running" branch, exactly as it does for mbIdle. This is
//     intentional: a turn in mbReleasing has no in-flight provider call
//     left to cancel, and the caller's fallback ("start a fresh Run()") is
//     already the correct behavior once the finalize step lands mbIdle a
//     moment later.
//   - IsSessionBusy() (`state != mbIdle`) and IsBusy() (see its own updated
//     comment): both must, and do, report BUSY during mbReleasing — this is
//     the crux of HIGH-1. If either treated mbReleasing as idle, a
//     same-process caller could act on "session is free" before release()
//     has actually let go of the OS lock.
//   - drainAfterCancel()/abandonOwnership(): neither is ever called while
//     state == mbReleasing in production — drainOrReleaseFinal is THE only
//     place that sets or clears this state, and it holds mb.mu for both the
//     transition in and the transition out, so no other mutator's call site
//     can observe mbReleasing mid-call. Documented here rather than special
//     -cased in either function: nothing needs to change in them.
//   - hardStop() (CancelAll/shutdown) landing while state == mbReleasing:
//     it only sets the one-way `stopped` latch and returns whatever
//     current.cancel/dispatcherCancel currently are (both nil during
//     mbReleasing, so the caller's cancel calls are no-ops — correctly:
//     there is nothing left running to interrupt). `stopped` is read again
//     when drainOrReleaseFinal reacquires mb.mu to finalize: if it is now
//     set, the finalize step ends the era at mbIdle and DISCARDS (not
//     orphans — a detached restart would start a fresh provider turn while
//     the process is exiting, exactly the P0-C bug this mailbox exists to
//     prevent) anything that landed in mb.submitted/mb.replacement during
//     the release window (mirroring the pre-existing stopped branch taken
//     when hardStop lands BEFORE drainOrReleaseFinal is even called). This
//     is the one behavioral interaction between mbReleasing and stopped
//     that does not fall out
//     of the other gates automatically, so drainOrReleaseFinal checks it
//     explicitly — see its own doc.
const (
	mbIdle      mailboxState = iota // no owner, nothing queued
	mbOwned                         // a turn loop holds ownership
	mbReleasing                     // release() (OS-lock I/O) is running with mb.mu NOT held; see drainOrReleaseFinal (#296/P1-C)
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
// ultimately replacing activeRequests, messageQueue, and the former
// sessionStartMu reservation gate for a single session id (injectQueue and
// sessionStartMu are already deleted). See design doc §1 for the full
// field-by-field rationale.
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

	// testLoopRearmSeam, when non-nil, is invoked by Run's turn loop
	// (agent.go) immediately BEFORE each iteration's beginGeneration(turnCancel)
	// call — i.e. strictly after any end-of-turn drain (drainOrRelease/
	// drainOrReleaseFinal/drainAfterCancel) has already released mb.mu, and
	// strictly before the loop re-arms the mailbox's current generation for
	// the next turn. It exists so a test can deterministically land a
	// concurrent Cancel()/InterruptAndReplace() call inside that exact
	// window and observe its effect BEFORE the loop's own re-arm can
	// overwrite mb.current.cancel — closing the gap
	// TestRun_CancelDuringLegacyReclaimWindow_ActuallyCancelsTurn2 (#289)
	// used to be unable to close deterministically: without this seam, the
	// loop's re-arm and a concurrently-blocked Cancel() call race for the
	// NEXT mb.mu acquisition after a reclaim releases it, so whether Cancel
	// observes the reclaim's fixed state (dispatcherCancel populated,
	// current.cancel nil) or loses the race to the re-arm (which writes its
	// own fresh, unrelated turnCancel into current.cancel first) depends on
	// goroutine-scheduling luck. Unlike testDrainSeam (which fires WHILE
	// holding mb.mu, so a concurrent mb.mu-needing caller blocks on the seam
	// itself), this seam fires OUTSIDE mb.mu — the loop has not yet called
	// beginGeneration when it fires — specifically so a test can let a
	// blocked Cancel() goroutine actually ACQUIRE mb.mu, run to completion,
	// and release it while the loop is parked here, before waving the loop
	// through to make its own beginGeneration call. nil in all non-test
	// construction paths (the zero value of mailbox), so it changes no
	// production behavior — mirrors testDrainSeam's existing idiom.
	testLoopRearmSeam func()

	// testPreAbandonSeam, when non-nil, is invoked by runSummarize (agent.go)
	// strictly AFTER the manual-compaction OS session lock has been released
	// and strictly BEFORE the mailbox is flipped to mbIdle via
	// abandonOwnership. It exists to let a test deterministically observe,
	// at that exact instant, that the two release the invariant Run() and
	// mbReleasing both uphold: an OS lock being free must never trail
	// mb.state still reporting idle to a same-process caller. Concretely: a
	// test parked here must see mb.IsSessionBusy()-equivalent state == true
	// (this compaction is still the owner) while TryAcquireSessionLock
	// already succeeds from "another process" (the OS lock is already
	// free) — proving the lock was released before, not after, idle became
	// visible. Fires outside mb.mu (abandonOwnership has not been called
	// yet), mirroring testLoopRearmSeam's own idiom. nil (a no-op) in every
	// production path.
	testPreAbandonSeam func()

	// testPreSnapshotConsumeSeam, when non-nil, is invoked by runSummarize
	// (agent.go) strictly AFTER the caller's SummarizeSnapshot has been
	// captured and strictly BEFORE it is consumed by runSummarizeBody. It
	// exists to let a test deterministically land a concurrent SetModels (or
	// any other shared-state mutation) exactly inside the window a pre-#341
	// regression would have re-read shared state in, without relying on a
	// wall-clock time.Sleep race that a fast in-memory path can simply
	// out-run. nil (a no-op) in every production path.
	testPreSnapshotConsumeSeam func()

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
// preamble, i.e. after a turn has already started. mb.submitted and
// mb.replacement live purely in this process (in-memory). So on shutdown a
// prompt that never reached a turn IS lost, with only a slog.Error to show
// for it.
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
//
// hardStop landing while state == mbReleasing (#296/P1-C: drainOrReleaseFinal
// is between dropping mb.mu to run release() and reacquiring it to finalize)
// needs no special case here: it takes mb.mu for the single instant needed
// to set the latch and read the two cancel funcs, so it is never blocked by
// the in-flight, lock-free release() call — that is the entire reason that
// call no longer holds mb.mu. Both funcs read back nil (cleared on entry to
// mbReleasing — see that transition), so the caller's cancel invocations are
// correctly no-ops: there is no live generation left to interrupt, the turn
// loop is already past its provider work and mid lock-teardown. The latch
// itself is still fully effective — drainOrReleaseFinal's finalize step
// re-reads mb.stopped after reacquiring mb.mu specifically to catch a
// hardStop that landed during the release() window (see its own doc) and
// ends the era at mbIdle instead of handing any newly-queued work back to
// the (now shutting-down) turn loop.
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
	// mb.state == mbOwned OR mb.state == mbReleasing both land here (#296/
	// P1-C): mbReleasing means release()'s disk I/O is in flight with mb.mu
	// NOT held, so the OS lock is not actually free yet even though a turn
	// loop isn't either — becoming owner here would race a concurrent
	// TryAcquireSessionLock against this call's own in-flight release. See
	// the mbReleasing const's doc. drainOrReleaseFinal's finalize step hands
	// this queued call back to the still-live turn loop once release()
	// returns, so it is not stranded.
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
// release is called ONLY when BOTH mb.submitted came back empty — i.e.
// exactly the branch that used to flip mb.state to mbIdle directly. As of
// #296/P1-C it is called WITHOUT mb.mu held (see the mbReleasing const's doc
// for the full state-machine rationale): mb.state is set to mbReleasing and
// mb.mu is released before invoking it, then mb.mu is reacquired to finalize
// once it returns. It must release the OS-level session lock (or be a no-op
// when a.dataDir == "" and no lock was ever acquired — see
// drainOrReleaseMerged's call site). Any error it returns is surfaced to the
// caller for logging; it never blocks the state flip to mbIdle, mirroring
// how Run's own `defer lk.Release()` today only logs a Release failure rather
// than treating it as fatal — a failed unlock must not leave the mailbox
// permanently non-mbIdle with nobody left to retry it. A panic escaping
// release() is recovered (in the same way) and turned into an error for the
// same reason: the mailbox must always reach a terminal state, never get
// stuck in mbReleasing.
//
// Priority of branch evaluation within this function is now explicitly
// "replacement -> submitted -> release" (round 14 review, P0-A
// fix), closing a race where a late interrupt whose cancel lost to
// Stream's normal completion could strand the replacement: mb.replacement
// was set by interruptAndReplace under mb.mu, but agent.Stream returned
// nil (not context.Canceled), so runTurn took the normal-completion path
// instead of the isCancelErr branch's drainAfterCancel. The replacement
// check here ensures the replacement is recovered without releasing the
// OS lock, preserving the same atomic state machine semantics
// drainAfterCancel already provides for the cancel path.
//
// One accepted trade-off from round 12 review, recorded here rather than
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
//
// #296/P1-C moves release() (a real syscall: unlock, close, and — via
// session.SessionLock.Release — a metadata truncate/remove) OUTSIDE mb.mu:
// it used to run WHILE mb.mu was held, so disk I/O briefly blocked every
// other in-process reader of this mailbox (IsSessionBusy, IsBusy, CancelAll,
// Cancel, QueuedPrompts) for that session — an unbounded control-plane stall
// if the filesystem/AV/SMB share hung, since release() has no context and no
// timeout. mb.state == mbReleasing stands in for the mutex as far as HIGH-1's
// atomicity goes (see the mbReleasing const's doc for why every other
// mutator already does the right thing around it).
//
// CRITICAL INVARIANT, closing brief #297 (a same-process reviewer's finding
// against an earlier version of this fix): once this call has entered
// mbReleasing, it can NEVER return hasNext=true / state==mbOwned again, no
// matter what lands in mb.submitted/mb.replacement during the release()
// window. release() has, by the time the finalize step below runs, already
// been invoked — successfully, with an error, or via a recovered panic — and
// there is no "un-release" operation: the OS lock this call was holding is
// either genuinely gone or its fate is unknown, either way NOT something
// this call can hand to a turn loop as "still holding it for you". An
// earlier draft of this function re-armed mbOwned and returned hasNext=true
// for exactly this case, on the theory that the turn loop still on the stack
// in runTurn could just keep going — but the turn loop's authority to run
// AT ALL came from that now-released lock; running another turn under it is
// indistinguishable, to a second process racing to acquire the same lock,
// from two owners of one session. A submit()/interruptAndReplace() landing
// in the release() window still correctly queues rather than becoming owner
// (mbReleasing reads as busy — see the mbReleasing const's doc), but the
// finalize step's job is to DRAIN that queued work OUT and hand it to the
// caller as orphaned, not to resurrect ownership for it. See orphaned's own
// doc below and drainOrReleaseMerged's doc in agent.go for how the caller is
// required to run it (a fresh, independent Run()-equivalent acquisition,
// mirroring coordinator.startDetachedRun's existing P0-B contract) instead
// of continuing the current turn loop on a lock that is no longer held.
func (mb *mailbox) drainOrReleaseFinal(
	epoch uint64,
	release func() error,
) (call SessionAgentCall, hasNext bool, orphaned []SessionAgentCall, releaseErr error) {
	mb.mu.Lock()

	if mb.epoch != epoch {
		mb.mu.Unlock()
		return SessionAgentCall{}, false, nil, nil
	}
	// Hard-stopped (P0-C): end the era rather than continuing into another
	// turn. Fall through to the mbReleasing path below (rather than an early
	// return) so the OS lock is still released and the mailbox still lands
	// on mbIdle — a shutdown must not leave the session marked busy with the
	// lock held. Queued work is deliberately NOT consulted at all on this
	// path (unlike the live branches below) — hardStop's own doc explains
	// why a stopped mailbox must refuse to hand a turn loop more work.
	if !mb.stopped {
		// Seam for deterministic test interleaving: fires right after the epoch check,
		// before ANY queue (replacement, submitted) is consulted — this is the
		// earliest deterministic pause point. Existing tests that relied on the seam
		// firing before the legacy-queue check are unaffected because their scenarios
		// now queue into mb.submitted instead, so the flow still reaches the same
		// submitted/release branches after the seam.
		if mb.testDrainSeam != nil {
			mb.testDrainSeam()
		}
		// P0-A fix: a late interrupt whose cancel lost the race to Stream's normal
		// completion lands here. mb.replacement was recorded by interruptAndReplace
		// under mb.mu, but agent.Stream returned nil (not context.Canceled), so runTurn
		// took the normal-completion path instead of the isCancelErr branch's
		// drainAfterCancel. Without this check, the replacement is silently stranded:
		// the drain releases ownership and the OS lock, and Run's defer
		// can only no-op it under a new era. Priority "replacement ->
		// submitted -> release" is now ONE atomic state machine operation,
		// matching drainAfterCancel's existing ordering. mb.current.cancel = nil — same invariant as every other "keep running"
		// branch (see mailbox_invariant_test.go). State stays mbOwned, OS lock untouched,
		// epoch NOT bumped — same semantics as the existing keep-running branches.
		if mb.replacement != nil {
			next := *mb.replacement
			mb.replacement = nil
			mb.current.cancel = nil
			mb.mu.Unlock()
			return next, true, nil, nil
		}
		if len(mb.submitted) > 0 {
			next := mb.submitted[0]
			mb.submitted = mb.submitted[1:]
			// mb.current.cancel must be cleared here too (round 12 review,
			// finding A — the SAME MEDIUM-1 shape as the former checkLegacy
			// branch, just on the more commonly hit mb.submitted path): by
			// the time this branch runs, it still holds the JUST-FINISHED
			// turn's own genCtx cancel — already invoked once via runTurn's
			// unconditional `cancel()` call right before this drain — inert
			// but NOT nil, which defeats Cancel()/InterruptAndReplace()'s
			// current.cancel==nil fallback gate exactly like the original
			// defect, for the whole window until the next turn's own
			// beginGeneration (which can be as long as title generation
			// takes).
			// dispatcherCancel is left untouched here (unlike the release
			// branch below): it's already the live runCancel from submit()/the
			// loop's own re-arm, still valid for the caller's own remaining
			// lifetime — nothing to reset.
			mb.current.cancel = nil
			mb.mu.Unlock()
			return next, true, nil, nil // caller runs another turn; state stays mbOwned, lock stays held
		}
	}

	// RELEASING (#296/P1-C): reached when stopped was already latched at
	// entry, OR when replacement/submitted all came back empty —
	// exactly the condition that used to flip mb.state straight to mbIdle
	// while still holding mb.mu across release()'s disk I/O. mbReleasing is
	// published and mb.mu is dropped BEFORE calling release(): every other
	// mutator's existing gate treats mbReleasing as busy/no-live-generation
	// (see the mbReleasing const's doc for the full per-method rationale),
	// which is what keeps HIGH-1 intact without holding the mutex over I/O
	// with no context and no timeout.
	//
	// current.cancel and dispatcherCancel are cleared HERE, on entry to
	// mbReleasing, not left for the finalize step below: hardStop() and
	// Cancel()/interruptAndReplace() can all run while release() is in
	// flight (they only need mb.mu for an instant, never blocked by the
	// in-flight I/O — that is the entire point of this state), and their
	// documented mbReleasing behavior (a no-op interrupt, since there is no
	// live generation left to cancel) depends on finding both nil. current.id
	// is preserved (monotonic, see the generation field's doc) — only the
	// spent cancel funcs are cleared, mirroring every other "era transition"
	// branch above.
	mb.current.cancel = nil
	mb.dispatcherCancel = nil
	mb.state = mbReleasing
	mb.mu.Unlock()

	// release() runs with NO mailbox lock held. A panic is recovered into an
	// error so the finalize step below always runs — the mailbox must never
	// get stuck in mbReleasing, whether release() errors or panics.
	if release != nil {
		releaseErr = callReleaseRecoveringPanic(release)
	}

	mb.mu.Lock()
	defer mb.mu.Unlock()

	// FINALIZE. release() has returned (or panicked, now folded into
	// releaseErr). This step ALWAYS lands on mbIdle — see this function's own
	// doc for why #296/P1-C's hand-back-to-mbOwned shape (an earlier draft of
	// this fix) was wrong: release() has already run by the time we get here,
	// so there is no OS lock left to keep running a turn loop under.
	mb.state = mbIdle
	mb.current.cancel = nil // preserve id (monotonic, see the field doc) — only the cancel func is spent
	mb.dispatcherCancel = nil

	// Re-checking mb.stopped here (rather than trusting the snapshot that
	// sent us down this path) is required, not optional: CancelAll's
	// hardStop can land WHILE release() is in flight (hardStop only takes
	// mb.mu for the instant it sets the latch and reads the cancel funcs —
	// see its own doc — so it is never blocked by a concurrent release()).
	// A hardStop landing in that window must win over anything that raced in
	// alongside it: `orphaned` calls are restarted via a fresh detached
	// Run() (see drainOrReleaseMerged's doc), and starting a fresh provider
	// turn while the process is trying to exit is precisely the P0-C bug
	// this mailbox already exists to prevent — restarting it as "orphaned"
	// would be just as wrong as the rejected hand-back-to-mbOwned shape,
	// just reached one step later. So a stopped mailbox DISCARDS whatever
	// raced in here (mirroring hardStop's own pre-release contract: queued
	// work is lost, not silently dropped without a trace — logged one level
	// up, in drainOrReleaseMerged's caller, exactly like the pre-existing
	// abandonOwnership discard path).
	if mb.stopped {
		mb.submitted = nil
		mb.replacement = nil
		return SessionAgentCall{}, false, nil, releaseErr
	}

	// Drain everything that raced into the mailbox during the release()
	// window (a submit() or a recovered interruptAndReplace observed
	// mbReleasing as "not idle" and queued rather than became owner — see
	// the mbReleasing const's doc) OUT of the mailbox and hand it to the
	// caller as orphaned, instead of resurrecting mbOwned for it.
	//
	// Order (replacement, then submitted FIFO) mirrors the priority the
	// live branches above already use, purely so the orphaned slice's order
	// matches what a live turn loop would have run first — orphaned calls
	// are each restarted independently (see this function's own doc and
	// drainOrReleaseMerged's), so the order has no atomicity consequence,
	// only a "if you can only run one right now, run this one first" one.
	if mb.replacement != nil {
		orphaned = append(orphaned, *mb.replacement)
		mb.replacement = nil
	}
	if len(mb.submitted) > 0 {
		orphaned = append(orphaned, mb.submitted...)
		mb.submitted = nil
	}

	// This is the ONLY point mbIdle becomes observable, strictly AFTER the
	// OS lock was released (or release() failed/panicked — releaseErr is
	// surfaced for logging, but a failed unlock must not leave the mailbox
	// permanently non-idle with nobody left to retry it, mirroring how a
	// failed Run() defer lk.Release() today only logs). This is HIGH-1: no
	// same-process observer could have acted on "session is free" before
	// this line runs, because every observer's read (IsSessionBusy, IsBusy,
	// submit, interruptAndReplace, Cancel) treated mbReleasing as busy for
	// the whole window between the unlock above and this relock — and
	// mbIdle now means exactly that: no in-process owner, AND the OS lock is
	// free. orphaned calls are not exempt: the caller must acquire a FRESH
	// OS lock (a fresh Run(), or the coordinator.startDetachedRun-equivalent
	// restart) for each of them, exactly as if a brand new caller had
	// submitted them against an idle mailbox — because that is, at this
	// instant, genuinely what they are.
	return SessionAgentCall{}, false, orphaned, releaseErr
}

// callReleaseRecoveringPanic invokes release and recovers any panic it
// raises, folding it into the returned error instead of letting it unwind
// through drainOrReleaseFinal. #296/P1-C moved release() (real disk I/O:
// Truncate/Seek/Sync/sidecar-unlink/unlock/Close) outside mb.mu specifically
// so a slow filesystem cannot stall the control plane; a panic escaping
// uncaught would be worse than the stall it replaces — it would leave
// mb.state stuck at mbReleasing forever (mb.mu is not even held at the panic
// site to recover under), permanently reporting the session busy with no
// path back to mbIdle. Isolated into its own function (rather than an
// inline func(){}() in the caller) so the defer/recover pair has a single,
// obviously-correct home.
func callReleaseRecoveringPanic(release func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("mailbox release callback panicked: %v", r)
		}
	}()
	return release()
}

// abandonOwnership implements Run's cleanup-defer release path (round 9
// review, BLOCKER-2a). It is NOT the same operation as drainOrRelease:
// drainOrRelease is called by an owner's OWN live turn loop, which can
// choose to keep running ("found something queued, stay owned, run it as
// the next turn"). Run's defer calls this instead specifically because it
// has NO live turn loop left — it fires on every return from Run(),
// including early bail-outs (OS-lock acquisition failure) and any
// early-return inside runTurn that skipped runTurn's own final drain (an
// error path). In both cases there is nobody left to hand a "keep
// running" answer to, so — unlike drainOrRelease — this ALWAYS ends the
// era at mbIdle.
//
// Unlike the pre-#308 version, this method does NOT drain submitted or
// replacement out to the caller. Entries left in submitted survive in
// place for the next owner to drain via drainOrReleaseFinal, and any
// pending replacement is folded INTO submitted (so it becomes a normal
// queued follow-up rather than pre-empting the next owner's first turn).
// The former messageQueue re-queue that the caller used to perform is no
// longer needed: the mailbox's own submitted queue is now the single
// source of truth for queued work.
//
// epoch behaves exactly as in drainOrRelease: a mismatch means this era
// already ended (a concurrent submit became the new owner, or
// drainOrReleaseFinal already released it) — a safe no-op that touches
// nothing, since whatever is in submitted/replacement now belongs to
// that later owner.
func (mb *mailbox) abandonOwnership(epoch uint64) (hadWork bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.epoch != epoch {
		return false
	}
	// Fold any pending replacement into submitted so the next owner
	// treats it as a normal queued follow-up, not as a pre-emption
	// target for reclaimReplacementOrKeep. A replacement left in
	// mb.replacement across an era boundary would silently jump the
	// queue of whatever the next owner's first turn happens to be.
	if mb.replacement != nil {
		mb.submitted = append(mb.submitted, *mb.replacement)
		mb.replacement = nil
	}
	hadWork = len(mb.submitted) > 0
	mb.state = mbIdle
	mb.current.cancel = nil // preserve id (monotonic, see the field doc) — only the cancel func is spent
	mb.dispatcherCancel = nil
	return hadWork
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
		//
		// mb.state == mbReleasing (#296/P1-C) also takes this branch, since
		// it fails `mb.state != mbOwned` too: there is no live generation
		// left to interrupt (the turn loop is already past its provider
		// work, mid lock-teardown), and current.cancel/dispatcherCancel are
		// both nil by the time a turn reaches mbReleasing anyway (cleared on
		// the way in — see drainOrReleaseFinal). The caller's fallback
		// ("start a fresh Run()") is correct here too: drainOrReleaseFinal's
		// finalize step is what actually decides whether that fresh call
		// gets handed back to THIS turn loop or a brand new Run() truly
		// starts, once release() returns.
		return nil, false
	}
	// Durably record the replacement FIRST, under the same lock that is
	// about to cancel the current generation. There is no window between
	// "replacement is recorded" and "current generation is cancelled" for
	// an external observer to land in, because both happen before mu is
	// released.
	mb.replacement = &call
	// #307 (P1-2 follow-up): mb.current.cancel == nil while mb.state ==
	// mbOwned is NOT a special case restricted to "before the very first
	// generation" — it is the inter-turn window too: Run's loop clears
	// current.cancel (via the just-finished turn's own drain — every hit
	// branch of drainOrRelease/drainOrReleaseFinal/drainAfterCancel leaves
	// it nil, see mailbox_invariant_test.go) and does not repopulate it
	// until the NEXT iteration's beginGeneration call. In BOTH windows
	// (pre-first-generation and inter-turn) there is genuinely nothing
	// live to interrupt — the turn loop itself is between provider calls,
	// not inside one.
	//
	// A previous version of this method (round 9 review, MEDIUM-1) fell
	// back to mb.dispatcherCancel here on the theory that SOME cancel must
	// be returned or the replacement would be silently stranded. That was
	// wrong: dispatcherCancel is runCancel, the whole-Run() context every
	// future turn's genCtx descends from (see the field's own doc — "never
	// the target of an interrupt"). Firing it here doesn't just end the
	// current (nonexistent) generation, it poisons runCtx for every turn
	// the loop will ever create again, including the replacement's own —
	// the replacement gets recorded, then the loop's next iteration derives
	// a turnCtx from an already-cancelled runCtx, whose preamble immediately
	// fails with context.Canceled. That failure DOES recover the mailbox's
	// replacement (the #284 preamble-cancel-recovery this bug rides on top
	// of), but there is no live runCtx left to run it on either, so the
	// recovered replacement fails the exact same way a turn later —
	// "accepted, reported queued, never executed", the same P0-A/P0-B shape
	// this whole mailbox exists to prevent. This was #307.
	//
	// The fix: return nil here instead of falling back to dispatcherCancel.
	// mb.replacement is already durably recorded under this same lock, which
	// is sufficient — nothing needs to be cancelled, because nothing is
	// running. The loop's own testLoopRearmSeam window (agent.go) is where
	// the replacement is reclaimed: see reclaimReplacementOrKeep, called
	// there immediately before each iteration's beginGeneration, which
	// atomically swaps the loop's stale `call` for a same-window replacement
	// instead of letting the stale call run first. The caller
	// (sessionAgent.InterruptAndReplace) already treats a nil returned
	// CancelFunc as "nothing to invoke" (`if cancelFn != nil { cancelFn() }`)
	// and still reports hadOwner=true, so the caller-visible contract
	// ("accepted, will run next, do not start a fresh Run()") is unchanged —
	// only WHAT gets cancelled (nothing, correctly, instead of the whole
	// dispatcher) changes.
	return mb.current.cancel, true
}

// reclaimReplacementOrKeep is called by Run's turn loop (agent.go)
// immediately before each iteration's beginGeneration call — the exact
// point testLoopRearmSeam already parks a test at (see that seam's own doc)
// — to atomically check whether an interrupt landed in the inter-turn
// window (#307) and recorded a fresher mb.replacement than the `call` the
// loop is about to run. If so, the replacement is consumed and returned
// instead, so the interrupt's intent ("run this now, not the old one")
// actually takes effect on the very next generation rather than being
// stranded until some later turn's own drain happens to notice it.
//
// Without this, interruptAndReplace's #307 fix (recording mb.replacement
// but returning a nil cancel instead of firing dispatcherCancel) would only
// be half the fix: the loop's local `call` variable was already decided by
// the PREVIOUS turn's own drain before this window began, so simply not
// cancelling anything would let that stale call run to completion first —
// an entire extra provider turn against the ALREADY-SUPERSEDED prompt —
// before the replacement is picked up by that stale turn's own end-of-turn
// drain (drainOrReleaseFinal checks mb.replacement first, ahead of
// submitted/legacy). That is not "replace", it is "queue behind" — the
// caller asked to interrupt-and-replace and would silently get run-then-
// replace instead. Consuming the replacement HERE, before the stale call is
// ever handed to beginGeneration/runTurn, makes replace actually mean
// replace.
//
// `call` is NOT discarded on the replacement-hit branch (closing review
// after the first draft of this fix, same class as #283/P0-2 — "the
// interrupt destroys the very message it's supposed to queue behind"):
// `call` at this point is not an abstraction. On every iteration after the
// first, it is a SessionAgentCall a PRIOR drain (drainOrRelease/
// drainOrReleaseFinal) already popped out of mb.submitted (or the legacy
// queue) specifically because "next" meant "run this next" — it has not run
// yet, and for the ExistingMessageID path (e.g. `crush sessions inject
// --interrupt`) its DB row already exists with nothing that will ever
// answer it if silently dropped here. On the LOOP'S VERY FIRST iteration,
// `call` is instead the original argument to Run() itself — verified, not
// assumed, by TestRun_InterruptBeforeFirstGeneration_ReplacementRunsExactlyOnce:
// an interrupt landing before turn 1's own beginGeneration is just as real a
// window as the inter-turn one (mb.state is already mbOwned by the time
// Run()'s loop reaches its first testLoopRearmSeam call — see submit()'s own
// doc — only mb.current.cancel is still nil), and this method treats that
// `call` exactly the same way: requeued, not special-cased as "never queued
// anywhere so safe to drop". An earlier version of this method returned only
// the replacement and let `call` vanish — with more than one message already
// queued behind the current turn (submitted = [B, C], drain already popped A
// as `call`), that silently destroyed exactly one message (A) while B and C
// survived untouched in mb.submitted, with the choice of victim decided
// purely by scheduling luck (whether the interrupt landed a moment before or
// after the drain popped A). ClearQueue is documented as "the single
// intentional drop-everything-queued operation" — discarding queued work is
// its job alone, not an accidental side effect of this one.
//
// The fix: push `call` back onto the FRONT of mb.submitted before handing
// back the replacement. This mirrors how drainAfterCancel/drainOrReleaseFinal
// already treat replacement-vs-submitted priority elsewhere in this file —
// replacement runs next, but submitted is never destroyed, only deferred.
// So for the A/B/C scenario above: D (the replacement) runs now, A is
// restored to the front of mb.submitted (order: A, B, C) and is picked up
// by D's own end-of-turn drain, so the FIFO order the caller originally
// queued in is preserved end to end, nothing lost, nothing reordered past
// the one deliberate pre-emption (D jumping ahead of A because the caller
// explicitly asked to interrupt).
//
// This does not appear in mailbox_invariant_test.go's stale-cancel-handle
// table: that table's postcondition (current.cancel == nil) is unrelated to
// this method's job, which is choosing WHICH SessionAgentCall the next
// generation runs — it does not touch state/current/dispatcherCancel at
// all. Covered instead by TestMailbox_ReclaimReplacementOrKeep_* in
// mailbox_test.go.
func (mb *mailbox) reclaimReplacementOrKeep(call SessionAgentCall) SessionAgentCall {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.replacement != nil {
		next := *mb.replacement
		mb.replacement = nil
		mb.submitted = append([]SessionAgentCall{call}, mb.submitted...)
		return next
	}
	return call
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
	// Clear the spent handle on the empty branch too (the sixth instance
	// of the same stale-cancel shape rounds 9-14 fixed one branch at a
	// time). This branch is now reachable from runTurn's preamble
	// cancel-recovery (#284): without the clear, current.cancel survives
	// as a spent handle, defeating Cancel()/InterruptAndReplace()'s
	// nil-check fallback to dispatcherCancel for the window between this
	// return and the caller's abandonOwnership defer.
	mb.current.cancel = nil
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

// injectIfBusy is the atomic busy-check-plus-enqueue operation InjectMessage
// needs (design §5, stage 2.4 of the mailbox migration): it checks whether
// the mailbox currently has an owner and, if so, appends msg to the
// pending-inject list stamped with the current generation id — all under a
// single mb.mu hold. This closes the P1-1 race where the old code's separate
// IsSessionBusy check and injectQueue.Append allowed the owner to finish and
// release to mbIdle between the two operations, stranding the message in the
// queue with nobody left to drain it (or, from the other direction,
// duplicating it when the next Run's preamble DB read already included the
// row).
//
// Returns true when the message was queued (session was busy); false when the
// session was idle, in which case the message lives only in the DB and will
// be picked up by the next Run's natural preamble DB read.
//
// This method does NOT change ownership state (state/current/dispatcherCancel
// are untouched) and therefore does NOT appear in mailbox_invariant_test.go's
// postcondition table — that table covers operations that hand work to a turn
// loop or end an era, which injectIfBusy deliberately does neither.
func (mb *mailbox) injectIfBusy(msg message.Message) bool {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.state == mbIdle {
		return false
	}
	mb.injects = append(mb.injects, pendingInject{
		msg:        msg,
		afterGenID: mb.current.id,
	})
	return true
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

// beginCompact atomically claims mailbox ownership for a compaction (manual
// /compact or inline auto-summarize). It is the atomic check-and-reserve
// that replaces the old non-atomic IsSessionBusy + runSummarize pair
// (#268/P0-4, design §6): the idle check and the state flip happen under
// one mb.mu hold, so no concurrent Run or second compaction can slip
// between them.
//
// Returns (epoch, true) when the caller becomes the sole owner. The
// compact's cancel is stored in BOTH current.cancel and dispatcherCancel so
// Cancel(sessionID), CancelAll, and interruptAndReplace reach it through
// the SAME mb.current.cancel field as a turn's generation cancel — no
// synthetic "sessionID-summarize" string key. The caller must present
// epoch to abandonOwnership when the compaction finishes.
//
// Returns (0, false) when the session is already owned (a turn or another
// compaction is in progress) or stopped. The caller queues (manual /compact
// via summarizeQueue) or skips.
//
// beginCompact is NOT in mailbox_invariant_test.go's stale-handle table:
// that table covers operations that END or CONTINUE an era (drain/release/
// abandon), whose postcondition is current.cancel == nil. beginCompact
// STARTS an era with a FRESH cancel — its postcondition is the opposite
// (current.cancel != nil). Its postconditions are covered by a dedicated
// test in the regression test file added alongside this change.
func (mb *mailbox) beginCompact(cancel context.CancelFunc) (epoch uint64, ok bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.state != mbIdle || mb.stopped {
		return 0, false
	}
	mb.state = mbOwned
	mb.epoch++
	mb.dispatcherCancel = cancel
	mb.current.id++
	mb.current.cancel = cancel
	return mb.epoch, true
}

// clearAll implements design §4's "ClearQueue is the one intentional
// drop-everything operation": clears submitted, replacement, and injects
// together under mu. It does NOT release ownership (state/current/
// dispatcherCancel are untouched) — the owner is still running and merely
// wants its pending queues discarded, not its reservation yanked.
func (mb *mailbox) clearAll() {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.submitted = nil
	mb.replacement = nil
	mb.injects = nil
}

// queue appends call to the submitted queue regardless of ownership state.
// Used by QueueMessage (the fire-and-forget queue primitive) and by the
// re-queue paths in Run's defer and runSummarize that used to target the
// legacy messageQueue. When the session is idle, the entry survives in
// submitted until the next submit() becomes owner and drains it via
// drainOrReleaseFinal.
func (mb *mailbox) queue(call SessionAgentCall) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.submitted = append(mb.submitted, call)
}

// popFirstSubmitted removes and returns the first entry from the submitted
// queue, regardless of mailbox state. Used by runSummarize to extract the
// first queued entry after abandonOwnership left work in submitted with
// state mbIdle, so it can start a fresh Run for it.
func (mb *mailbox) popFirstSubmitted() (SessionAgentCall, bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if len(mb.submitted) == 0 {
		return SessionAgentCall{}, false
	}
	first := mb.submitted[0]
	mb.submitted = mb.submitted[1:]
	return first, true
}

// popAllSubmitted removes and returns ALL entries from the submitted queue,
// regardless of mailbox state. Used by abandonOwnershipWithHandoff to start
// detached runs for all work left in the mailbox after a non-cancel error.
// This method is safe to call when state is mbIdle (which abandonOwnership
// guarantees), because no new submit can land on mbIdle — they all queue
// into submitted under the same lock.
func (mb *mailbox) popAllSubmitted() []SessionAgentCall {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if len(mb.submitted) == 0 {
		return nil
	}
	all := make([]SessionAgentCall, len(mb.submitted))
	copy(all, mb.submitted)
	mb.submitted = mb.submitted[:0]
	return all
}

// beginRelease atomically transitions the mailbox to mbReleasing state
// for a manual compaction. This mirrors the first half of drainOrReleaseFinal's
// mbReleasing transition pattern, allowing manual compaction to signal
// "OS lock release is in progress" separately from "still actively streaming".
//
// P1-2 fix (2026-08-09): Manual compaction used to call lk.Release() directly
// without any mbReleasing visibility (mailbox stayed mbOwned during the release).
// This meant that a hung filesystem/AV/SMB during lk.Release() would leave the
// mailbox permanently mbOwned, indistinguishable from an actively streaming turn.
// With mbReleasing visibility, diagnostic tools and future code can distinguish
// "releasing OS lock" from "still working".
//
// Returns true on successful transition, false if epoch mismatch (era already ended)
// or state cannot transition (already mbReleasing, mbIdle, etc.).
//
// The caller must:
//  1. Call beginRelease(epoch) to get mbReleasing visibility.
//  2. Release mb.mu (done by this method).
//  3. Call the actual release callback (e.g., lk.Release()).
//  4. Call finishRelease(epoch) to transition to mbIdle.
//
// This ensures the same "mbReleasing without holding mb.mu over I/O" invariant
// that drainOrReleaseFinal maintains.
func (mb *mailbox) beginRelease(epoch uint64) bool {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.epoch != epoch {
		return false
	}
	// Can only transition from mbOwned to mbReleasing.
	// If already mbReleasing, mbIdle, or some other state, this is a no-op.
	if mb.state != mbOwned {
		return false
	}

	// Clear the cancel handles before entering mbReleasing, matching drainOrReleaseFinal's
	// pattern. This ensures Cancel()/interruptAndReplace() correctly treat mbReleasing
	// as "no live generation left to cancel".
	mb.current.cancel = nil
	mb.dispatcherCancel = nil
	mb.state = mbReleasing
	return true
}

// finishRelease completes the release transition, moving from mbReleasing to mbIdle.
// This must be called after the actual release callback (e.g., lk.Release()) completes.
// Returns true on successful transition to mbIdle, false if epoch mismatch or state
// was not mbReleasing (stale/racing call).
func (mb *mailbox) finishRelease(epoch uint64) bool {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.epoch != epoch {
		return false
	}
	// Must be in mbReleasing state to complete the transition.
	if mb.state != mbReleasing {
		return false
	}
	mb.state = mbIdle
	return true
}
