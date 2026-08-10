package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// RunQueuePump is a background goroutine that scans the durable run queue
// for pending or stale-leased entries and attempts to execute them.
//
// It lives independently of any specific request/turn, surviving process
// restarts and ensuring that once a call is enqueued (durable), it will
// eventually be executed by some process.
//
// Design principles (from task #340, P0-2 review):
//   - Independent of request/turn lifecycle (not a child goroutine of Run())
//   - One pump per process (or per dataDir - resolved to "per process" here)
//   - Never acquires the session OS lock itself — it drives execution
//     through Coordinator.Run, which goes through the normal
//     TryAcquireSessionLock path exactly like any other caller (a
//     SessionLockBusyError from ordinary lock contention is handled by
//     processEntry's NackRunQueueEntryNoAttemptPenalty path, see below —
//     the pump does not pre-check or avoid contention, it just doesn't
//     let contention drain the durable queue).
//   - Respects ErrCallAlreadyAttempted as terminal (no retry)
//   - Graceful shutdown via context cancellation
//
// Pump interval: 3 seconds (fast enough to pick up orphaned work quickly,
// but not so fast as to cause excessive lock contention or DB polling).
const RunQueuePumpInterval = 3 * time.Second

// Lease TTL: 30 seconds. A pump that crashes while holding a lease will
// release it within this window, allowing another pump instance to pick it up.
const RunQueueLeaseTTL = 30 * time.Second

// MaxAttempts: 10 retries before giving up (transient errors only).
// ErrCallAlreadyAttempted-type errors respect terminal_failure flag and
// are removed immediately regardless of attempts count.
const RunQueueMaxAttempts = 10

// Coordinator interface is a minimal subset for executing queued calls.
// We use this instead of importing the full agent.Coordinator to avoid
// import cycles (session → agent → session). The real app's AgentCoordinator
// implements this interface.
type Coordinator interface {
	// Run executes a call for the given session using the full call data.
	// Returns the result or an error. Implementations MUST return
	// ErrCallQueuedNotExecuted (not a nil error) if the call was merely
	// appended to an already-owned session's in-process queue rather than
	// actually executed by this call — see that error's doc.
	Run(ctx context.Context, call SessionAgentCallData) (*any, error)
}

// ErrCallQueuedNotExecuted is returned by Coordinator.Run when the target
// session was already owned by a live, in-process turn at the moment of the
// call: the call was appended to that owner's own mailbox queue to be run
// as a later turn, not executed by this call. This is NOT a failure of the
// call and NOT a signal that the current owner is a stale/foreign process —
// it can legitimately be this same pump instance's OWN prior execution of a
// different (or the same, self-raced) entry for that session; see the
// RunQueuePump.inFlight field's doc, which prevents the pump from ever
// causing this itself. What remains, once inFlight closes the self-inflicted
// paths, is a genuinely external live owner (e.g. a web/CLI request running
// concurrently in this same process outside pump control).
//
// executeEntry treats this specially (see there): it must NOT be handled
// like an ordinary success (would Ack/delete a durable row for work that
// has not actually run yet — the queued-mailbox copy is only as durable as
// the owning process staying alive long enough to drain it) and must NOT be
// retried like an ordinary failure either (mailbox.submit unconditionally
// appends on every call, so nacking-and-retrying every tick would append a
// new duplicate of the same call on every attempt, all of which the owner
// eventually runs when it drains its queue). The entry is left exactly as
// leased and untouched; RunQueueLeaseTTL's natural expiry (via
// CleanupExpiredLeases) is the only recovery path, giving the external
// owner a full lease window to drain the queued call before another
// attempt is made.
var ErrCallQueuedNotExecuted = errors.New("run_queue_pump: call was queued into an already-owned session, not executed")

// RunQueuePumpConfig configures a RunQueuePump instance.
type RunQueuePumpConfig struct {
	// Sessions is the session service for enqueue/lease/ack operations.
	Sessions Service

	// DataDirectory is currently unused by RunQueuePump itself (the pump
	// never touches the OS lock directly — see the design-principles doc
	// comment on RunQueuePump). Kept for now since callers already pass
	// it and a future pump-level use (e.g. direct lock probing) may want
	// it; if it stays unused, consider removing it in a follow-up pass.
	DataDirectory string

	// Coordinator is the agent coordinator that will execute leased calls.
	// Set to nil for pump instances that only scan/cleanup (no execution).
	Coordinator Coordinator

	// PumpInstanceID is a unique identifier for this pump instance (used for leased_by).
	// Defaults to a generated UUID if empty.
	PumpInstanceID string

	// TestTick is a test seam for overriding the pump interval.
	// nil = use production RunQueuePumpInterval.
	TestTick func() time.Duration

	// TestLeaseTTL is a test seam for overriding RunQueueLeaseTTL. 0 = use
	// the production constant. RunQueueLeaseTTL (30s) is not itself
	// test-overridable, which made the lease-renewal-during-a-long-turn
	// scenario (see executeEntry's renewal loop) impossible to exercise
	// deterministically and quickly — a real test would need to block a
	// fake Coordinator.Run call for 30+ real seconds to observe the race
	// this seam exists to close. Found needed by the fourth @oh review
	// pass over #337-349, which noted the existing tests could not
	// exercise post-expiry behavior at all.
	TestLeaseTTL time.Duration
}

// leaseTTL returns the effective lease TTL for this pump instance —
// cfg.TestLeaseTTL if set, otherwise the production RunQueueLeaseTTL.
func (p *RunQueuePump) leaseTTL() time.Duration {
	if p.cfg.TestLeaseTTL > 0 {
		return p.cfg.TestLeaseTTL
	}
	return RunQueueLeaseTTL
}

// RunQueuePump is a background pump for the durable run queue.
type RunQueuePump struct {
	cfg     RunQueuePumpConfig
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
	startMu sync.Mutex

	// inFlight tracks session IDs with an executeEntry goroutine currently
	// running, guarded by inFlightMu. Found by the third @oh review pass
	// over #337-349 (in-range for #340's original design): Coordinator.Run
	// returns (nil, nil) when the target session is already owned
	// in-process — the call was merely appended to the current owner's
	// mailbox queue (mailbox.submit), not actually executed. executeEntry
	// treated err == nil as unconditional success and Acked (deleted) the
	// durable row regardless, so a second concurrent dispatch for the same
	// session could delete a row whose only remaining copy of the work now
	// lives purely in an in-memory mailbox queue — lost for good if that
	// process crashes before draining it, or silently re-run as a
	// duplicate turn once it does drain.
	//
	// This is reachable two ways:
	//   1. Same-tick, no lease-expiry involved: tick() leases and dispatches
	//      entries one at a time in a single pass, so two distinct
	//      durably-queued entries for the same session — e.g. two calls
	//      queued while a process was down — could be leased and
	//      `go executeEntry`-dispatched back to back within the same tick,
	//      before either had run long enough to matter.
	//   2. Sequential, via lease expiry: RunQueueLeaseTTL (30s) is far
	//      shorter than a real LLM turn can take. Without the lease-renewal
	//      loop in executeEntry (added in the fourth @oh review pass, see
	//      that function's own doc), CleanupExpiredLeases could return an
	//      entry to pending while the goroutine executing it was still
	//      genuinely running, and the next tick would then lease and
	//      dispatch a SECOND, sequential (not concurrent) goroutine for the
	//      same session.
	//
	// inFlight closes path 1 at the source: processEntry refuses to lease
	// a pending entry whose session already has an executeEntry goroutine
	// running FROM THIS PUMP INSTANCE, so this pump can never concurrently
	// dispatch two entries for the same session. It does NOT, by itself,
	// close path 2 — an inFlight session that loses its lease to
	// CleanupExpiredLeases still shows as busy in this map (the goroutine
	// is still running), so a same-tick duplicate is still prevented, but
	// nothing stopped the durable row from flipping to pending underneath
	// it and being picked up by a LATER tick once execution (wrongly)
	// looked done to a stale reader. Path 2 is closed by executeEntry's
	// lease-renewal loop keeping the row genuinely 'leased' for the whole
	// duration of a long turn, not by inFlight. See executeEntry's own
	// Run()-result handling for the narrower residual case neither
	// mechanism covers: a genuinely external, non-pump owner (e.g. a live
	// user-facing process) holding the session when the pump attempts it.
	inFlight   map[string]struct{}
	inFlightMu sync.Mutex

	// busyBackoffUntil tracks, per session ID, a local deadline before
	// which this pump instance will not attempt to lease a pending entry
	// for that session again — guarded by busyBackoffMu. Used exclusively
	// for the ErrCallQueuedNotExecuted outcome (see executeEntry): an
	// EARLIER fix tried achieving backoff purely via RenewRunQueueLease,
	// but that call happens almost instantly after the original lease was
	// taken (Coordinator.Run returns near-immediately for this outcome),
	// so it barely extends lease_expires_at beyond what leasing already
	// set — CleanupExpiredLeases still reaped the row (and incremented
	// attempts) after essentially one ordinary TTL window, no different
	// from doing nothing. Confirmed by a failing test before this design
	// was adopted: attempts still reached RunQueueMaxAttempts in the
	// expected ~10 TTL windows, not the "far more than 10" the fix was
	// supposed to guarantee.
	//
	// This map is the actual fix: on ErrCallQueuedNotExecuted, the entry
	// is immediately released via NackRunQueueEntryNoAttemptPenalty
	// (status flips straight to 'pending', attempts untouched — cleanup
	// never even sees a 'leased' row to charge an attempt against), and
	// this pump additionally records a LOCAL backoff deadline so its own
	// processEntry does not immediately re-lease and re-dispatch the same
	// entry into the same busy owner's mailbox on the very next tick
	// (mailbox.submit has no dedup — a tight retry loop would append a
	// new duplicate call on every attempt). A DIFFERENT pump instance (a
	// separate process) is free to attempt the row immediately — its
	// in-process mailbox ownership is independent of this one's, so there
	// is no reason to block it just because this process happens to be
	// busy.
	busyBackoffUntil map[string]time.Time
	busyBackoffMu    sync.Mutex

	// Test seam for waiting for a tick in tests
	tickCh chan struct{}
}

// NewRunQueuePump creates a new RunQueuePump instance.
func NewRunQueuePump(cfg RunQueuePumpConfig) *RunQueuePump {
	if cfg.PumpInstanceID == "" {
		cfg.PumpInstanceID = fmt.Sprintf("pump-%d", time.Now().UnixNano())
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &RunQueuePump{
		cfg:              cfg,
		ctx:              ctx,
		cancel:           cancel,
		inFlight:         make(map[string]struct{}),
		busyBackoffUntil: make(map[string]time.Time),
		tickCh:           make(chan struct{}, 1),
	}
}

// Start begins the pump goroutine. Safe to call multiple times (idempotent).
func (p *RunQueuePump) Start() {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if p.started {
		return
	}
	p.started = true

	p.wg.Add(1)
	go p.run()
	slog.Info("run_queue_pump: started", "instance_id", p.cfg.PumpInstanceID)
}

// Stop gracefully shuts down the pump goroutine.
// Waits for the current tick to complete before returning.
func (p *RunQueuePump) Stop() {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if !p.started {
		return
	}
	p.cancel()
	p.wg.Wait()
	p.started = false
	slog.Info("run_queue_pump: stopped", "instance_id", p.cfg.PumpInstanceID)
}

// run is the main pump loop.
func (p *RunQueuePump) run() {
	defer p.wg.Done()

	// Determine tick interval
	interval := RunQueuePumpInterval
	if p.cfg.TestTick != nil {
		interval = p.cfg.TestTick()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial tick on startup to recover any orphaned work
	p.tick()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.tick()
		}
	}
}

// tick performs one scan of the queue and attempts to execute pending work.
func (p *RunQueuePump) tick() {
	ctx := p.ctx

	// Step 1: Cleanup expired leases (recovery from crashed pumps)
	expiredBefore := time.Now().Unix()
	if err := p.cfg.Sessions.CleanupExpiredLeases(ctx, expiredBefore); err != nil {
		slog.Warn("run_queue_pump: cleanup expired leases failed", "err", err, "instance_id", p.cfg.PumpInstanceID)
	}

	// Step 2: Scan for pending entries (and now-recovered stale leases)
	pending, err := p.cfg.Sessions.ListPendingRunQueueEntries(ctx)
	if err != nil {
		slog.Warn("run_queue_pump: list pending failed", "err", err, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	if len(pending) == 0 {
		return // No work to do
	}

	slog.Debug("run_queue_pump: found pending entries", "count", len(pending), "instance_id", p.cfg.PumpInstanceID)

	// Step 3: Attempt to lease and execute each pending entry
	for _, entry := range pending {
		if p.ctx.Err() != nil {
			return // Shutdown requested mid-tick
		}

		p.processEntry(ctx, &entry)
	}

	// Test seam notification
	select {
	case p.tickCh <- struct{}{}:
	default:
	}
}

// AlreadyAttempted is a marker interface for terminal failures.
// Errors implementing this interface (e.g., agent.ErrCallAlreadyAttempted)
// indicate that the call already left a persistent trace and retry would
// cause duplicates (task #339 regression protection).
//
// This interface is defined in the session package to avoid import cycles:
// the agent package implements it without importing session.
type AlreadyAttempted interface {
	AlreadyAttempted() bool
}

// processEntry attempts to lease and execute a single run queue entry.
func (p *RunQueuePump) processEntry(ctx context.Context, entry *RunQueueEntry) {
	// Refuse to lease ANY entry for a session this pump instance already
	// has an executeEntry goroutine running for — see the inFlight field's
	// doc for why (self-inflicted lease-expiry race, and same-tick
	// same-session double dispatch). Left pending; a later tick retries
	// once that goroutine finishes and releases the session.
	p.inFlightMu.Lock()
	_, busy := p.inFlight[entry.SessionID]
	p.inFlightMu.Unlock()
	if busy {
		slog.Debug("run_queue_pump: session already has an execution in flight from this pump, deferring", "id", entry.ID, "session_id", entry.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// Refuse to lease a pending entry for a session this pump instance
	// backed off from after an ErrCallQueuedNotExecuted outcome — see the
	// busyBackoffUntil field's doc. Without this, the entry (immediately
	// visible again as 'pending', by design — no attempt penalty) would be
	// re-leased and re-dispatched on the very next tick, appending another
	// duplicate call to the same busy owner's mailbox.
	p.busyBackoffMu.Lock()
	until, backingOff := p.busyBackoffUntil[entry.SessionID]
	if backingOff && !time.Now().Before(until) {
		delete(p.busyBackoffUntil, entry.SessionID)
		backingOff = false
	}
	p.busyBackoffMu.Unlock()
	if backingOff {
		slog.Debug("run_queue_pump: session is in local busy-backoff, deferring", "id", entry.ID, "session_id", entry.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// Skip if attempts exceeded (unless terminal failure flag is set).
	// TerminalFailRunQueueEntry only deletes rows in 'leased' state, but an
	// attempts-exhausted entry sits in 'pending' (that's how it was scanned
	// here) — it must be leased first, or the DELETE never matches and this
	// same entry gets re-scanned and re-fails to terminal-fail on every
	// subsequent tick forever.
	if entry.Attempts >= RunQueueMaxAttempts && !entry.TerminalFailure {
		slog.Warn("run_queue_pump: entry exceeded max attempts, terminal failing",
			"id", entry.ID, "session_id", entry.SessionID, "attempts", entry.Attempts, "instance_id", p.cfg.PumpInstanceID)
		leased, err := p.cfg.Sessions.LeaseRunQueueEntry(ctx, entry.SessionID, p.cfg.PumpInstanceID, p.leaseTTL())
		if err != nil {
			slog.Error("run_queue_pump: lease for terminal-fail failed", "id", entry.ID, "session_id", entry.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
			return
		}
		if leased == nil {
			// Raced with another pump instance leasing/consuming it first.
			return
		}
		if leased.ID != entry.ID {
			// LeaseRunQueueEntry claims the OLDEST PENDING entry for the
			// session, not a specific entry by ID — if another pump instance
			// raced us and already consumed the attempts-exhausted entry we
			// scanned, this lease can land on a DIFFERENT, healthy,
			// never-executed entry for the same session (e.g. a fresh call
			// queued after the scan). Terminal-failing THAT entry would
			// silently delete legitimate, unattempted work. Release it
			// unharmed (no attempt penalty — this pump did nothing wrong to
			// it) and let a future tick handle it normally.
			if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(ctx, leased.ID, "released: leased entry did not match the attempts-exhausted entry scanned for terminal-fail"); nackErr != nil {
				slog.Error("run_queue_pump: release of mismatched lease failed", "id", leased.ID, "session_id", leased.SessionID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
			}
			return
		}
		if err := p.cfg.Sessions.TerminalFailRunQueueEntry(ctx, leased.ID); err != nil {
			slog.Error("run_queue_pump: terminal fail failed", "id", leased.ID, "session_id", leased.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		}
		return
	}

	// Skip if no coordinator (pump in scan-only mode)
	if p.cfg.Coordinator == nil {
		slog.Debug("run_queue_pump: no coordinator, skipping execution", "id", entry.ID, "session_id", entry.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// Attempt to lease the entry
	leased, err := p.cfg.Sessions.LeaseRunQueueEntry(ctx, entry.SessionID, p.cfg.PumpInstanceID, p.leaseTTL())
	if err != nil {
		slog.Error("run_queue_pump: lease failed", "id", entry.ID, "session_id", entry.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		return
	}
	if leased == nil {
		// Another pump leased it between the scan and our attempt
		return
	}

	// Mark this session in flight BEFORE dispatching, under the same lock
	// processEntry's own busy-check above uses — closes the check-then-act
	// window between that check and this dispatch (both run synchronously
	// within tick()'s single-threaded per-entry loop, so there is no
	// concurrent processEntry call to race against, but the mark must still
	// land before executeEntry's goroutine can possibly finish and unmark).
	p.inFlightMu.Lock()
	p.inFlight[leased.SessionID] = struct{}{}
	p.inFlightMu.Unlock()

	// Execute the call (detached, not blocking this pump tick)
	go p.executeEntry(ctx, leased)
}

// executeEntry runs a leased entry and handles success/failure.
func (p *RunQueuePump) executeEntry(ctx context.Context, leased *RunQueueEntry) {
	defer func() {
		p.inFlightMu.Lock()
		delete(p.inFlight, leased.SessionID)
		p.inFlightMu.Unlock()
	}()

	// Create a fresh context for this execution (not tied to pump lifecycle)
	execCtx := context.Background()

	// Parse the call data from JSON
	var callData SessionAgentCallData
	if err := json.Unmarshal([]byte(leased.CallData), &callData); err != nil {
		slog.Error("run_queue_pump: failed to parse call data", "id", leased.ID, "session_id", leased.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		// Terminal failure: malformed data can never succeed
		if termErr := p.cfg.Sessions.TerminalFailRunQueueEntry(ctx, leased.ID); termErr != nil {
			slog.Error("run_queue_pump: terminal fail failed", "id", leased.ID, "err", termErr, "instance_id", p.cfg.PumpInstanceID)
		}
		return
	}

	// Renew this lease periodically while Coordinator.Run is in flight.
	// Found by the fourth @oh review pass over #337-349: RunQueueLeaseTTL
	// (30s) is far shorter than a real LLM turn, and without renewal
	// CleanupExpiredLeases would return this entry to pending — status
	// 'pending', not 'leased' — while this goroutine is still genuinely
	// executing it. The eventual AckRunQueueEntry (`WHERE status =
	// leased`) would then silently fail to match, leaving the row pending
	// with an incremented attempts count, and the NEXT tick would lease
	// and dispatch a second, genuinely duplicate execution of the exact
	// same turn — the same data-loss/duplicate-execution outcome the
	// inFlight guard above was meant to close, just via a sequential path
	// instead of a concurrent one. Renewing well inside the TTL (every
	// TTL/3) keeps this pump's own lease alive for the entire duration of
	// a long turn under all but pathological scheduling delays (a >20s
	// stall between renewal ticks) — see the doc below on what happens if
	// renewal itself ever loses the race.
	renewCtx, stopRenewing := context.WithCancel(ctx)
	renewalsDone := make(chan struct{})
	go func() {
		defer close(renewalsDone)
		ticker := time.NewTicker(p.leaseTTL() / 3)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				newExpiresAt := time.Now().Add(p.leaseTTL()).Unix()
				ok, err := p.cfg.Sessions.RenewRunQueueLease(ctx, leased.ID, p.cfg.PumpInstanceID, newExpiresAt)
				if err != nil {
					slog.Warn("run_queue_pump: lease renewal failed, will retry next interval", "id", leased.ID, "session_id", leased.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
					continue
				}
				if !ok {
					// The lease was already reassigned to a different
					// owner — this execution has lost the race and can no
					// longer safely keep the lease alive. There is no
					// cancellation wired to execCtx (renewing is
					// best-effort, not ownership-enforcing), so the
					// in-flight Coordinator.Run call is left to finish on
					// its own; the eventual Ack/Nack below will then
					// correctly fail to match (logged, not silently
					// treated as success) since the row belongs to
					// whatever re-leased it. Surfaced loudly since it
					// means real duplicate work may follow.
					slog.Error("run_queue_pump: lost lease ownership during renewal, another owner has taken this entry", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
					return
				}
			}
		}
	}()

	// Attempt to execute via coordinator.Run
	_, err := p.cfg.Coordinator.Run(execCtx, callData)

	// Stop renewing IMMEDIATELY once Run returns, synchronously, before any
	// of the outcome-handling below touches the row's lease (Ack/Nack/
	// RenewRunQueueLease-for-backoff). Deliberately not a `defer` — a
	// deferred stop would only run at function exit, leaving a window
	// where the renewal goroutine is still alive (and can still fire one
	// more tick) WHILE this function is already deciding the row's fate.
	// Observed directly: with a defer, the ErrCallQueuedNotExecuted branch
	// below could Nack the row back to pending, and the still-live renewal
	// goroutine could then fire and find `status != leased`, logging a
	// spurious "lost lease ownership" — misleading, since nothing else
	// actually took it; this execution's own Nack did.
	stopRenewing()
	<-renewalsDone

	// Handle outcome
	if err == nil {
		// Success: ack the entry (delete it)
		if _, ackErr := p.cfg.Sessions.AckRunQueueEntry(ctx, leased.ID); ackErr != nil {
			slog.Error("run_queue_pump: ack failed after success", "id", leased.ID, "session_id", leased.SessionID, "err", ackErr, "instance_id", p.cfg.PumpInstanceID)
		}
		slog.Info("run_queue_pump: executed entry successfully", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// ErrCallQueuedNotExecuted means the call was appended to a genuinely
	// external live owner's mailbox (inFlight already rules out this being
	// the pump's own doing) rather than executed by this attempt — see that
	// error's doc for why it must be treated as neither success nor an
	// ordinary retryable failure.
	//
	// Found by the fourth @oh review pass over #337-349: the original fix
	// here left the entry exactly as leased and did nothing further,
	// relying on CleanupExpiredLeases's natural expiry as the sole
	// recovery path — but that same cleanup unconditionally increments
	// attempts on every recovery (needed elsewhere, see its own SQL
	// comment, to eventually dead-letter a poison entry that always
	// crashes before a normal Ack/Nack). Treated as an ordinary lease
	// expiry, a session that simply stays externally busy for
	// RunQueueMaxAttempts * RunQueueLeaseTTL (a few minutes) would have
	// its accepted, entirely healthy, never-actually-failed work silently
	// deleted — the exact class of bug SessionLockBusyError's
	// no-attempt-penalty handling exists to prevent for the equivalent
	// OS-lock case, applied inconsistently here for the in-process case.
	//
	// Fixed via NackRunQueueEntryNoAttemptPenalty (immediate release, no
	// attempts increment — mirroring SessionLockBusyError's own handling
	// below) plus a LOCAL busyBackoffUntil deadline recorded for this
	// session so THIS pump instance does not immediately re-lease and
	// re-dispatch the same entry into the same busy owner's mailbox on
	// the very next tick (mailbox.submit has no dedup — a tight retry
	// loop would append a new duplicate call on every attempt). See the
	// busyBackoffUntil field's own doc for why a single RenewRunQueueLease
	// call (tried first) did not actually work: it happens almost
	// instantly after the original lease, so it barely extends
	// lease_expires_at beyond what leasing already set, and
	// CleanupExpiredLeases still reaped the row — and charged an attempt
	// — after essentially one ordinary TTL window, same as doing nothing.
	if errors.Is(err, ErrCallQueuedNotExecuted) {
		if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(ctx, leased.ID, "queued into an externally-owned in-process session, not executed"); nackErr != nil {
			slog.Error("run_queue_pump: no-penalty release after queued-not-executed failed", "id", leased.ID, "session_id", leased.SessionID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
		}
		p.busyBackoffMu.Lock()
		p.busyBackoffUntil[leased.SessionID] = time.Now().Add(p.leaseTTL())
		p.busyBackoffMu.Unlock()
		slog.Debug("run_queue_pump: call was queued into an externally-owned session, backed off locally without an attempt penalty", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// Failure: determine if it's retryable or terminal
	// Use the marker interface to detect terminal failures (task #339 protection)
	// without creating an import cycle between session and agent packages.
	var alreadyAttempted AlreadyAttempted
	if errors.As(err, &alreadyAttempted) && alreadyAttempted.AlreadyAttempted() {
		// Terminal failure (no retry) - protects against duplicates
		if termErr := p.cfg.Sessions.TerminalFailRunQueueEntry(ctx, leased.ID); termErr != nil {
			slog.Error("run_queue_pump: terminal fail failed", "id", leased.ID, "err", termErr, "instance_id", p.cfg.PumpInstanceID)
		}
		slog.Warn("run_queue_pump: entry terminal failed (already attempted)", "id", leased.ID, "session_id", leased.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// SessionLockBusyError means another live process legitimately holds the
	// OS session lock right now — ordinary, expected contention (a normal
	// turn can hold it for as long as a full LLM turn takes), not a failure
	// of the call itself. It must never count toward RunQueueMaxAttempts:
	// counting it would let a few turns' worth of routine contention
	// silently delete accepted user work once attempts exhausts (found in
	// the final @oh review of tasks #337-349 — P0-2).
	var busyErr *SessionLockBusyError
	if errors.As(err, &busyErr) {
		if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(ctx, leased.ID, err.Error()); nackErr != nil {
			slog.Error("run_queue_pump: no-penalty nack failed", "id", leased.ID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
		}
		slog.Debug("run_queue_pump: entry blocked by session lock contention, will retry without attempt penalty", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// Retryable failure: nack and let the pump retry on next tick
	if nackErr := p.cfg.Sessions.NackRunQueueEntry(ctx, leased.ID, err.Error()); nackErr != nil {
		slog.Error("run_queue_pump: nack failed", "id", leased.ID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
	}
	slog.Debug("run_queue_pump: entry failed, will retry", "id", leased.ID, "session_id", leased.SessionID, "err", err, "attempts", leased.Attempts+1, "instance_id", p.cfg.PumpInstanceID)
}
