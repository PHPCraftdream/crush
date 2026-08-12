package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
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
//     executeEntry's NackRunQueueEntryNoAttemptPenalty path, see below —
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

// MaxConcurrentExecutions: maximum number of executeEntry goroutines that
// can run simultaneously across all sessions. Without this limit, a process
// crash followed by a large backlog would spawn one goroutine per pending
// entry in a single tick, potentially creating hundreds of concurrent
// provider streams, DB connections, and subprocesses — indistinguishable
// from a hang and prone to rate-limit/retry storms.
// 10 is a reasonable default for typical resource constraints: one turn
// can hold ~1-2 HTTP connections (LLM request + tools) plus a few SQLite
// writers, so 10 concurrent turns stay within practical limits for a
// single-process Crush instance. Each session still has its own inFlight
// guard, so multiple entries for the same session never run concurrently
// even if the pool is not full.
const RunQueueMaxConcurrentExecutions = 10

// LeaseWatchdogSafetyMargin: the safety margin before lease expiry at which
// the watchdog cancels execution. This margin accounts for:
//   - Scheduling delays between watchdog timer firing and execCtx cancellation
//   - Time for cancellation to propagate through Coordinator.Run to actual LLM/tool calls
//   - Time for provider/tool code to respect cancellation (in-flight requests may complete)
// Production: 5 seconds means we cancel at least 5s before the lease actually expires.
// A separate pump instance therefore cannot legitimately take ownership until at least
// 5s after this executor has stopped, providing a strong bound against double-execution.
// The comment about "bounded to one TTL residual window" (P1-2 original fix) was too
// optimistic: with a fixed 30s DB timeout per renewal, an executor could continue up to
// 40s (TTL + full timeout) after the lease expired. The watchdog closes this gap.
const LeaseWatchdogSafetyMargin = 5 * time.Second

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
// retried on every tick like an ordinary failure either (mailbox.submit
// unconditionally appends on every call, so nacking-and-retrying every tick
// would append a new duplicate of the same call on every attempt, all of
// which the owner eventually runs when it drains its queue).
//
// Fixed by the fourth @oh review pass over #337-349: an earlier version of
// this doc (and the code) left the entry exactly as leased and untouched,
// relying on RunQueueLeaseTTL's natural expiry (via CleanupExpiredLeases) as
// the only recovery path — but that cleanup unconditionally counts an
// attempt on every recovery, so a session that stayed externally busy for a
// few lease windows would have its accepted, never-actually-failed work
// silently dead-lettered. The entry is instead released immediately via
// NackRunQueueEntryNoAttemptPenalty (never counts an attempt) paired with a
// local RunQueuePump.busyBackoffUntil deadline that stops THIS pump instance
// specifically from re-attempting the same session before a full lease
// window has passed — see that field's doc for the full rationale,
// including why a naive lease-renewal-based backoff was tried first and
// found not to work.
var ErrCallQueuedNotExecuted = errors.New("run_queue_pump: call was queued into an already-owned session, not executed")

// RunQueuePumpConfig configures a RunQueuePump instance.
type RunQueuePumpConfig struct {
	// Sessions is the session service for enqueue/lease/ack operations.
	Sessions Service

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

	// TestMaxConcurrentExecutions is a test seam for overriding
	// RunQueueMaxConcurrentExecutions. 0 = use the production constant.
	// Allows regression tests to force an artificially small pool size
	// to verify the bounded-concurrency guarantee holds even under
	// extreme backlog pressure.
	TestMaxConcurrentExecutions int

	// TestDBWriteTimeout is a test seam for overriding the 30s budget
	// newDBCtx() gives each individual outcome/renewal DB write in
	// executeEntry. 0 = use the production 30s. Added to deterministically
	// and quickly reproduce a real regression found in the closing review
	// of the release-readiness round: a single dbCtx created once at
	// executeEntry's entry, before Coordinator.Run, was reused for the
	// post-Run outcome write too — so any turn longer than 30s hit an
	// already-expired context on its Ack/Nack/TerminalFail call. Without
	// this seam, exercising that bug needs a real Coordinator.Run call that
	// blocks for 30+ real seconds.
	TestDBWriteTimeout time.Duration

	// TestLeaseWatchdogSafetyMargin is a test seam for overriding
	// LeaseWatchdogSafetyMargin. 0 = use the production 5s. Allows tests
	// to verify the watchdog fires at the right time even with very short
	// TTLs (where a fixed 5s margin would be longer than the TTL itself).
	TestLeaseWatchdogSafetyMargin time.Duration
}

// leaseTTL returns the effective lease TTL for this pump instance —
// cfg.TestLeaseTTL if set, otherwise the production RunQueueLeaseTTL.
func (p *RunQueuePump) leaseTTL() time.Duration {
	if p.cfg.TestLeaseTTL > 0 {
		return p.cfg.TestLeaseTTL
	}
	return RunQueueLeaseTTL
}

// maxConcurrentExecutions returns the effective maximum concurrent
// executions for this pump instance — cfg.TestMaxConcurrentExecutions
// if non-zero, otherwise the production RunQueueMaxConcurrentExecutions.
func (p *RunQueuePump) maxConcurrentExecutions() int {
	if p.cfg.TestMaxConcurrentExecutions > 0 {
		return p.cfg.TestMaxConcurrentExecutions
	}
	return RunQueueMaxConcurrentExecutions
}

// dbWriteTimeout returns the effective per-write DB context budget —
// cfg.TestDBWriteTimeout if set, otherwise the production 30 seconds.
func (p *RunQueuePump) dbWriteTimeout() time.Duration {
	if p.cfg.TestDBWriteTimeout > 0 {
		return p.cfg.TestDBWriteTimeout
	}
	return 30 * time.Second
}

// leaseWatchdogSafetyMargin returns the effective safety margin for the
// lease watchdog — cfg.TestLeaseWatchdogSafetyMargin if set, otherwise
// the production LeaseWatchdogSafetyMargin (5s).
//
// The effective margin is clamped to at most TTL/2 to prevent the watchdog
// from firing immediately in test configs where the margin would be larger
// than the TTL itself.
func (p *RunQueuePump) leaseWatchdogSafetyMargin() time.Duration {
	margin := LeaseWatchdogSafetyMargin
	if p.cfg.TestLeaseWatchdogSafetyMargin > 0 {
		margin = p.cfg.TestLeaseWatchdogSafetyMargin
	}
	// Clamp margin to at most TTL/2 to prevent immediate firing
	ttl := p.leaseTTL()
	if margin > ttl/2 {
		margin = ttl / 2
	}
	return margin
}

// RunQueuePump is a background pump for the durable run queue.
type RunQueuePump struct {
	cfg     RunQueuePumpConfig
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
	startMu sync.Mutex

	// workerWg tracks all executeEntry goroutines, ensuring Stop() waits
	// for them to finish before returning. Critical for P0-3: without this,
	// Stop() can return while workers are still writing Ack/Nack/TerminalFail
	// to the database, causing rows to remain leased/pending after restart.
	// Pattern documented for reuse in P0-4 (agent.Summarize) and P1-1.
	workerWg sync.WaitGroup

	// admitMu/stopping form the admission gate that closes a real "Add
	// concurrently with Wait" race an earlier version of this fix
	// introduced: processEntry checked p.ctx.Err() and then called
	// workerWg.Add(1) as two separate, unsynchronized steps. Between the
	// check and the Add, Stop() could cancel p.ctx and start its
	// workerWg.Wait() goroutine while the counter was still 0 — Wait()
	// could then return (nothing to wait for) and Stop() could report
	// "stopped gracefully" a moment before the Add(1) actually landed,
	// which either panics ("Add called concurrently with Wait", per the
	// sync.WaitGroup contract: "calls with a positive delta that start
	// when the counter is zero must happen before a Wait") or, if it
	// doesn't panic, lets a brand-new worker start after Stop() has
	// already told the caller (App.Shutdown) it is safe to close the DB —
	// reintroducing the exact class of bug P0-3 exists to close, just one
	// level down.
	//
	// The fix: stopping is only ever flipped false->true, and every read
	// (processEntry) and the one write (Stop) happen under admitMu. So
	// either processEntry's critical section runs entirely before Stop's
	// (stopping was still false: Add(1) is guaranteed to complete, and
	// complete-before, Stop's subsequent cancel()+Wait()-spawn), or it
	// runs entirely after (stopping is already true: processEntry bails
	// out and never calls Add at all). There is no interleaving left where
	// Add and Wait can race.
	admitMu  sync.Mutex
	stopping bool

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

	// execSem is a bounded semaphore that limits total concurrent
	// executeEntry goroutines across all sessions. Unlike inFlight
	// (which is per-session), this prevents unbounded fan-out after
	// a process crash with a large backlog — without it, a single
	// tick would spawn one goroutine per pending entry, potentially
	// creating hundreds of concurrent provider streams, DB connections,
	// and subprocesses. P1-4 fix (docs/reviews/2026-08-11).
	execSem chan struct{}

	// Test seam for waiting for a tick in tests
	tickCh chan struct{}
}

// NewRunQueuePump creates a new RunQueuePump instance.
func NewRunQueuePump(cfg RunQueuePumpConfig) *RunQueuePump {
	if cfg.PumpInstanceID == "" {
		cfg.PumpInstanceID = fmt.Sprintf("pump-%d", time.Now().UnixNano())
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &RunQueuePump{
		cfg:              cfg,
		ctx:              ctx,
		cancel:           cancel,
		inFlight:         make(map[string]struct{}),
		busyBackoffUntil: make(map[string]time.Time),
		tickCh:           make(chan struct{}, 1),
	}
	// execSem must be initialized after p is constructed, so maxConcurrentExecutions() works
	p.execSem = make(chan struct{}, p.maxConcurrentExecutions())
	return p
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
// Waits for all in-flight executeEntry workers to finish with a 5-second grace period.
// Returns true if shutdown was forced (workers still running after grace period),
// false if all workers finished gracefully.
// Pattern: matches internal/agent/agent.go CancelAll's 5-second grace + stillBusy return.
func (p *RunQueuePump) Stop() bool {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if !p.started {
		return false
	}

	// Step 0: flip the admission gate BEFORE cancelling or waiting — see
	// the stopping field's doc. This must happen first so that any
	// processEntry call already past its own admitMu section (Add done)
	// is guaranteed visible to workerWg before we ever call Wait().
	p.admitMu.Lock()
	p.stopping = true
	p.admitMu.Unlock()

	// Step 1: Cancel the main pump context first to stop new lease/dispatch.
	// This ensures tick() stops accepting new work BEFORE we wait for workers.
	p.cancel()

	// Step 2: Wait for all in-flight workers AND the main run() loop with a
	// unified 5-second grace period. We use a single deadline for both to
	// keep the total shutdown time bounded to 5s, not 10s (P1-1). Without this,
	// a hung tick() DB call (stuck disk/filesystem) would make Stop() hang
	// forever, breaking the "bounded shutdown" guarantee in App.Shutdown.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	workerDone := make(chan struct{})
	go func() {
		p.workerWg.Wait()
		close(workerDone)
	}()

	mainLoopDone := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(mainLoopDone)
	}()

	select {
	case <-workerDone:
		// Workers finished. Now wait for the main run() loop to exit.
		select {
		case <-mainLoopDone:
			p.started = false
			slog.Info("run_queue_pump: stopped gracefully", "instance_id", p.cfg.PumpInstanceID)
			return false
		case <-shutdownCtx.Done():
			// Main loop didn't finish in time - forced shutdown
			slog.Warn("run_queue_pump: forced shutdown (main loop still running after 5s grace)", "instance_id", p.cfg.PumpInstanceID)
			return true
		}
	case <-mainLoopDone:
		// Main loop finished. Now wait for workers.
		select {
		case <-workerDone:
			p.started = false
			slog.Info("run_queue_pump: stopped gracefully", "instance_id", p.cfg.PumpInstanceID)
			return false
		case <-shutdownCtx.Done():
			// Workers didn't finish in time - forced shutdown
			slog.Warn("run_queue_pump: forced shutdown (workers still running after 5s grace)", "instance_id", p.cfg.PumpInstanceID)
			return true
		}
	case <-shutdownCtx.Done():
		// Neither workers nor main loop finished in time - forced shutdown
		slog.Warn("run_queue_pump: forced shutdown (workers and/or main loop still running after 5s grace)", "instance_id", p.cfg.PumpInstanceID)
		return true
	}
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
	// Use a deadline-bound context for DB reads to prevent indefinite hangs
	// on stuck disk/filesystem (P1-1). The 5s budget matches Stop()'s total
	// grace period - if tick() itself hangs, Stop() will force-shutdown after
	// the same deadline anyway, so this protects both the shutdown path and
	// prevents a single stuck tick from blocking the pump indefinitely.
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	// Step 1: Cleanup expired leases (recovery from crashed pumps)
	expiredBefore := time.Now().Unix()
	if err := p.cfg.Sessions.CleanupExpiredLeases(ctx, expiredBefore); err != nil {
		slog.Warn("run_queue_pump: cleanup expired leases failed", "err", err, "instance_id", p.cfg.PumpInstanceID)
	}

	// Step 1.5: sweep expired busyBackoffUntil entries. processEntry's own
	// lazy delete (see there) only fires when a PENDING entry for that
	// session is actually rescanned — a session whose entry was acked,
	// terminal-failed, or picked up by a different pump instance in the
	// meantime never gets rescanned, so its key would otherwise linger in
	// this map for the rest of the process lifetime. Found by the fifth
	// @oh review pass over #337-349 (unbounded growth, not a correctness
	// bug — a stale key can only ever make this pump wait slightly longer
	// than necessary before trying a session again, never cause incorrect
	// behavior).
	now := time.Now()
	p.busyBackoffMu.Lock()
	for sessionID, until := range p.busyBackoffUntil {
		if !now.Before(until) {
			delete(p.busyBackoffUntil, sessionID)
		}
	}
	p.busyBackoffMu.Unlock()

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

	// Attempt to acquire a slot from the bounded worker pool (P1-4).
	// Non-blocking: if the pool is full, leave this entry entirely
	// untouched for the next tick rather than leasing it. This prevents
	// unbounded fan-out after a process crash with a large backlog.
	select {
	case p.execSem <- struct{}{}:
		// Slot acquired, proceed below to lease and dispatch
	default:
		slog.Debug("run_queue_pump: worker pool full, deferring", "id", entry.ID, "session_id", entry.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// releaseSlot is a helper for returning a slot to the pool on error paths.
	// Must only be called if a slot was acquired above.
	releaseSlot := func() {
		<-p.execSem
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
			releaseSlot()
			return
		}
		if leased == nil {
			// Raced with another pump instance leasing/consuming it first.
			releaseSlot()
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
			if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(ctx, leased.ID, p.cfg.PumpInstanceID, "released: leased entry did not match the attempts-exhausted entry scanned for terminal-fail"); nackErr != nil {
				slog.Error("run_queue_pump: release of mismatched lease failed", "id", leased.ID, "session_id", leased.SessionID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
			}
			releaseSlot()
			return
		}
		if err := p.cfg.Sessions.TerminalFailRunQueueEntry(ctx, leased.ID, p.cfg.PumpInstanceID); err != nil {
			slog.Error("run_queue_pump: terminal fail failed", "id", leased.ID, "session_id", leased.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		}
		releaseSlot()
		return
	}

	// Skip if no coordinator (pump in scan-only mode)
	if p.cfg.Coordinator == nil {
		slog.Debug("run_queue_pump: no coordinator, skipping execution", "id", entry.ID, "session_id", entry.SessionID, "instance_id", p.cfg.PumpInstanceID)
		releaseSlot()
		return
	}

	// Attempt to lease the entry
	leased, err := p.cfg.Sessions.LeaseRunQueueEntry(ctx, entry.SessionID, p.cfg.PumpInstanceID, p.leaseTTL())
	if err != nil {
		slog.Error("run_queue_pump: lease failed", "id", entry.ID, "session_id", entry.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		releaseSlot()
		return
	}
	if leased == nil {
		// Another pump leased it between the scan and our attempt
		releaseSlot()
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

	// Refuse to start a new worker once shutdown has begun, and register
	// the one that IS started with workerWg — both under admitMu, so the
	// check and the Add are one atomic operation relative to Stop()'s own
	// critical section (see admitMu/stopping's doc: an unsynchronized
	// check-then-Add here raced Stop()'s cancel+Wait sequence and could
	// either panic ("Add called concurrently with Wait") or let a new
	// worker start after Stop() had already told its caller shutdown was
	// safe — undoing the fix this task exists to make).
	p.admitMu.Lock()
	if p.stopping {
		p.admitMu.Unlock()
		// inFlight was marked above (before this gate); it is only ever
		// cleared by executeEntry's defer, which will now never run for
		// this entry, so clear it here or the session would appear
		// permanently busy to this pump instance.
		p.inFlightMu.Lock()
		delete(p.inFlight, leased.SessionID)
		p.inFlightMu.Unlock()
		// Release the lease immediately instead of leaving the row
		// "leased" for up to a full leaseTTL: any pump instance (this
		// process on restart, or a different live process) can then pick
		// it up as soon as it next ticks. No attempt penalty — this pump
		// never actually attempted it. Use an independent, short-lived
		// context rather than p.ctx: p.cancel() may already have fired or
		// be about to, and this write must not be lost to that same race.
		nackCtx, nackCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(nackCtx, leased.ID, p.cfg.PumpInstanceID, "run_queue_pump: shutting down, releasing lease without executing"); nackErr != nil {
			slog.Warn("run_queue_pump: release-on-shutdown nack failed, entry will recover via lease expiry", "id", leased.ID, "session_id", leased.SessionID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
		}
		nackCancel()
		releaseSlot()
		return
	}
	p.workerWg.Add(1)
	p.admitMu.Unlock()

	// Execute the call (detached, not blocking this pump tick)
	go p.executeEntry(ctx, leased)
}

// executeEntry runs a leased entry and handles success/failure.
func (p *RunQueuePump) executeEntry(ctx context.Context, leased *RunQueueEntry) {
	defer p.workerWg.Done()

	// Release the semaphore slot when execution completes (P1-4).
	// This guarantees the slot is returned even on panic or any error path.
	defer func() { <-p.execSem }()

	defer func() {
		p.inFlightMu.Lock()
		delete(p.inFlight, leased.SessionID)
		p.inFlightMu.Unlock()
	}()

	// newDBCtx creates a fresh, short-lived (30s) context for a single DB
	// write, deliberately rooted in context.Background() rather than p.ctx or
	// execCtx: it must outlive both the pump's own lifecycle (Ack/Nack/
	// TerminalFail must still be able to write after Stop() has canceled
	// p.ctx — the original P0-3 rationale) and, separately, the in-flight
	// Coordinator.Run call.
	//
	// A single dbCtx created once at function entry (the original P0-3
	// shape) does NOT work: its 30s deadline is measured from before
	// Coordinator.Run is ever called, but Run can legitimately take minutes
	// for a real turn. Reusing that same, already-expired context for the
	// post-Run outcome write (Ack/Nack/TerminalFail) made every such write
	// fail with "context deadline exceeded" for any turn over 30s, leaving
	// the row leased/pending and causing the exact duplicate-execution bug
	// P0-3 exists to prevent — found during the closing review of this
	// round. Each DB write below now gets its own fresh 30s budget instead.
	newDBCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), p.dbWriteTimeout())
	}

	// Create a cancellable context for execution. This allows us to cancel
	// Coordinator.Run if we lose lease ownership during the renewal loop (P1-2).
	// Without this, the executor would continue doing real work (LLM calls,
	// tool execution, writing messages) even after a different pump instance
	// has taken ownership of the lease, potentially causing duplicate execution.
	execCtx, execCancel := context.WithCancel(context.Background())
	defer execCancel()

	// leaseLost tracks whether this execution lost its lease ownership during
	// the renewal loop. When true, we skip all outcome writes (Ack/Nack/TerminalFail)
	// since the row no longer belongs to this executor — the new owner will
	// handle the outcome. This is a read-after-write flag: the renewal goroutine
	// sets it atomically, and the main execution reads it after Coordinator.Run.
	var leaseLost atomic.Bool

	// Parse the call data from JSON
	var callData SessionAgentCallData
	if err := json.Unmarshal([]byte(leased.CallData), &callData); err != nil {
		slog.Error("run_queue_pump: failed to parse call data", "id", leased.ID, "session_id", leased.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		// Terminal failure: malformed data can never succeed
		parseDBCtx, parseDBCancel := newDBCtx()
		defer parseDBCancel()
		if termErr := p.cfg.Sessions.TerminalFailRunQueueEntry(parseDBCtx, leased.ID, p.cfg.PumpInstanceID); termErr != nil {
			slog.Error("run_queue_pump: terminal fail failed", "id", leased.ID, "err", termErr, "instance_id", p.cfg.PumpInstanceID)
		}
		return
	}

	// Renew this lease periodically while Coordinator.Run is in flight.
	// renewCtx is rooted in context.Background(), NOT newDBCtx()'s 30s
	// budget: it must stay alive for the entire duration of Coordinator.Run,
	// which can legitimately run far longer than 30s, and is only ever
	// cancelled by stopRenewing() once Run returns (or by execCtx-driven
	// lease loss below). Each individual RenewRunQueueLease call still gets
	// its own fresh, short-lived DB context via newDBCtx() — a single
	// long-lived context would eventually starve a single slow DB call from
	// ever timing out, and a single short-lived one reused across ticks
	// would (as found in the closing review of this round) expire well
	// before the loop's own natural lifetime ends.
	//
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
	//
	// P1-1 watchdog fix: The original fail-closed timeout (tracking
	// lastSuccessfulRenewal and checking time.Since(lastSuccessfulRenewal) >= TTL)
	// had a critical flaw: if a RenewRunQueueLease call started at ~10s and
	// hung until its own 30s timeout, the executor would continue running until
	// ~40s even though the lease expired at ~30s. During those extra ~10s, another
	// pump instance could legitimately take ownership and start a duplicate execution.
	//
	// The fix: introduce an INDEPENDENT watchdog timer (separate from the renewal
	// loop) that cancels execCtx BEFORE lease expiry with a safety margin (production:
	// 5s). The watchdog fires at TTL - safety_margin from the last successful renewal,
	// regardless of whether the next renewal call is stuck or not. This provides a
	// strong bound against double-execution even when the DB layer hangs.
	//
	// The watchdog is the sole lease-expiry enforcement mechanism. The old
	// fail-closed timeout (timeSinceLastSuccess >= TTL) has been removed because
	// the watchdog ALWAYS fires first at TTL - safety_margin (where safety_margin > 0
	// in production and test configs). The watchdog provides a stricter guarantee
	// (TTL - margin vs TTL), eliminating the double-execution window the old code
	// could not catch when DB writes stalled.
	//
	// Additionally, each RenewRunQueueLease call now gets a timeout equal to the
	// remaining safe lease budget (time until watchdog would fire), not a fixed 30s.
	// This ensures a stalled renewal cannot outlive the safe window.
	//
	// SEMANTICS: The durable queue provides AT-LEAST-ONCE guarantees for
	// persistent side effects (LLM calls, tool execution, message writes),
	// not exactly-once. The watchdog MINIMIZES but does NOT GUARANTEE
	// elimination of all duplicate-execution windows. The residual overlap window is:
	//   - From "watchdog fires and execCancel() propagates to Coordinator.Run"
	//     until "provider/tool actually stops respecting cancellation"
	//     (in-flight HTTP requests may complete).
	//   - A full fencing token (checked before each persistent write) is
	//     required for strict exactly-once semantics, which is an
	//     architectural change beyond this watchdog fix.
	//
	// Residual windows are bounded to the safety margin (production: 5s) plus
	// provider/tool cancellation latency, not the full TTL.
	renewCtx, stopRenewing := context.WithCancel(context.Background())
	renewalsDone := make(chan struct{})

	// P1-1 watchdog: an independent goroutine that cancels execCtx BEFORE
	// lease expiry if renewal stalls.
	//
	// Design: Use atomic storage for last successful renewal time + ticker.
	// This avoids timer.Reset() complexity and channel race conditions.
	// The watchdog wakes every 10ms to check if we're past the safe deadline.
	var lastSuccessfulRenewalAtomic atomic.Int64 // Unix nanoseconds
	lastSuccessfulRenewalAtomic.Store(time.Now().UnixNano())

	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-renewCtx.Done():
				// Renewal loop stopped
				return
			case <-ticker.C:
				// Check if watchdog should fire
				lastRenewalTime := time.Unix(0, lastSuccessfulRenewalAtomic.Load())
				timeSinceRenewal := time.Since(lastRenewalTime)
				safeBudget := p.leaseTTL() - p.leaseWatchdogSafetyMargin()
				if safeBudget <= 0 {
					safeBudget = time.Millisecond
				}

				if timeSinceRenewal >= safeBudget {
					// Watchdog deadline passed: cancel execution
					leaseLost.Store(true)
					execCancel()
					slog.Error("run_queue_pump: lease watchdog fired: canceling execution before expiry",
						"id", leased.ID,
						"session_id", leased.SessionID,
						"ttl", p.leaseTTL(),
						"safety_margin", p.leaseWatchdogSafetyMargin(),
						"time_since_renewal", timeSinceRenewal,
						"instance_id", p.cfg.PumpInstanceID)
					return
				}
			}
		}
	}()

	go func() {
		defer close(renewalsDone)
		// time.NewTicker panics on a non-positive interval — clamp so a
		// pathologically tiny TestLeaseTTL (a test foot-gun, never a
		// production value) can't crash the pump instead of just renewing
		// unnecessarily often.
		renewInterval := p.leaseTTL() / 3
		if renewInterval <= 0 {
			renewInterval = time.Millisecond
		}
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()

		// Track the last successful renewal time for fail-closed timeout.
		// Initialize to the time execution starts — the lease was just taken
		// successfully by processEntry before launching this goroutine.
		// This gives us a full TTL window from the start before we consider
		// ourselves in "renewal failure" territory.
		lastSuccessfulRenewal := time.Now()
		lastSuccessfulRenewalAtomic.Store(lastSuccessfulRenewal.UnixNano())

		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				timeSinceLastSuccess := time.Since(lastSuccessfulRenewal)

				// P1-1: set DB timeout based on remaining safe lease budget.
				// The watchdog will fire at TTL - safety_margin from the last
				// successful renewal. If this renewal takes longer than that
				// remaining budget, the watchdog will cancel execCtx while we're
				// still stuck in RenewRunQueueLease. We ensure the DB call itself
				// times out BEFORE the watchdog would fire, so we get a clean error
				// path rather than a watchdog-triggered cancellation.
				timeUntilWatchdog := p.leaseTTL() - p.leaseWatchdogSafetyMargin() - timeSinceLastSuccess
				if timeUntilWatchdog <= 0 {
					// We're already past the safe budget: watchdog is imminent.
					// Don't even attempt renewal — let the watchdog fire.
					continue
				}

				newExpiresAt := time.Now().Add(p.leaseTTL()).Unix()
				renewDBCtx, renewDBCancel := context.WithTimeout(context.Background(), timeUntilWatchdog)
				ok, err := p.cfg.Sessions.RenewRunQueueLease(renewDBCtx, leased.ID, p.cfg.PumpInstanceID, newExpiresAt)
				renewDBCancel()
				if err != nil {
					slog.Warn("run_queue_pump: lease renewal failed, will retry next interval", "id", leased.ID, "session_id", leased.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
					continue
				}
				// Update lastSuccessfulRenewal on every successful renewal
				// (including ok=false — the renewal call itself succeeded, we just
				// lost ownership).
				lastSuccessfulRenewal = time.Now()

				// P1-1: update watchdog's last successful renewal time
				lastSuccessfulRenewalAtomic.Store(lastSuccessfulRenewal.UnixNano())

				if !ok {
					// The lease was already reassigned to a different
					// owner — this execution has lost the race and can no
					// longer safely keep the lease alive. Cancel execCtx
					// immediately to stop the in-flight Coordinator.Run
					// (P1-2). The eventual Ack/Nack/TerminalFail below
					// will then correctly fail to match (logged, not
					// silently treated as success) since those queries
					// are scoped to `leased_by = PumpInstanceID` (found by
					// the fifth @oh review pass — this WAS a real gap
					// before that scoping existed) and the row now
					// belongs to whatever re-leased it. We set leaseLost
					// so the main execution path knows to skip the outcome
					// write entirely and log a clear "aborted due to lease
					// loss" message instead of a confusing "0 rows matched"
					// error.
					leaseLost.Store(true)
					execCancel()
					slog.Error("run_queue_pump: lost lease ownership during renewal, canceling in-flight execution", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
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
	//
	// P1-1: also stop the watchdog goroutine.
	stopRenewing()
	<-renewalsDone
	<-watchdogDone

	// P1-2: If lease ownership was lost during the renewal loop, skip all
	// outcome writes. The row no longer belongs to this executor (the new
	// owner will handle Ack/Nack/TerminalFail), and attempting to write
	// would fail with "0 rows matched" and produce confusing log messages.
	// The leaseLost flag is set atomically by the renewal goroutine when
	// RenewRunQueueLease returns !ok, and execCancel() is called there
	// to stop the in-flight Coordinator.Run as early as possible.
	if leaseLost.Load() {
		slog.Debug("run_queue_pump: execution aborted due to lease loss, skipping outcome write (reconciliation deferred to new owner)", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// Fresh 30s budget for the outcome write below, created only now —
	// AFTER Coordinator.Run has already returned — not at function entry.
	// See newDBCtx's own doc for why reusing one context across the
	// entire Run() call was the bug.
	dbCtx, dbCancel := newDBCtx()
	defer dbCancel()

	// Handle outcome
	if err == nil {
		// Success: ack the entry (delete it). The "executed successfully" log
		// must be conditioned on the ack itself succeeding (P2-6) — logging it
		// unconditionally previously claimed success even when the row was
		// never actually deleted, which is misleading for anyone debugging a
		// row that keeps reappearing after an Ack failure.
		if _, ackErr := p.cfg.Sessions.AckRunQueueEntry(dbCtx, leased.ID, p.cfg.PumpInstanceID); ackErr != nil {
			slog.Error("run_queue_pump: ack failed after success", "id", leased.ID, "session_id", leased.SessionID, "err", ackErr, "instance_id", p.cfg.PumpInstanceID)
		} else {
			slog.Info("run_queue_pump: executed entry successfully", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
		}
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
		if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(dbCtx, leased.ID, p.cfg.PumpInstanceID, "queued into an externally-owned in-process session, not executed"); nackErr != nil {
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
		if termErr := p.cfg.Sessions.TerminalFailRunQueueEntry(dbCtx, leased.ID, p.cfg.PumpInstanceID); termErr != nil {
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
		if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(dbCtx, leased.ID, p.cfg.PumpInstanceID, err.Error()); nackErr != nil {
			slog.Error("run_queue_pump: no-penalty nack failed", "id", leased.ID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
		}
		slog.Debug("run_queue_pump: entry blocked by session lock contention, will retry without attempt penalty", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// Retryable failure: nack and let the pump retry on next tick
	if nackErr := p.cfg.Sessions.NackRunQueueEntry(dbCtx, leased.ID, p.cfg.PumpInstanceID, err.Error()); nackErr != nil {
		slog.Error("run_queue_pump: nack failed", "id", leased.ID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
	}
	slog.Debug("run_queue_pump: entry failed, will retry", "id", leased.ID, "session_id", leased.SessionID, "err", err, "attempts", leased.Attempts+1, "instance_id", p.cfg.PumpInstanceID)
}
