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

	"github.com/charmbracelet/crush/internal/db"
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

// Orphan outbox drain interval: 15 seconds (fallback path for when main
// queue was temporarily unavailable. Longer than main tick since this is
// for rare recovery scenarios, not normal operation).
const OrphanOutboxDrainInterval = 15 * time.Second

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
//
// Production: 5 seconds means we cancel at least 5s before the lease actually expires.
// A separate pump instance therefore cannot legitimately take ownership until at least
// 5s after this executor has stopped, providing a strong bound against double-execution.
// The comment about "bounded to one TTL residual window" (P1-2 original fix) was too
// optimistic: with a fixed 30s DB timeout per renewal, an executor could continue up to
// 40s (TTL + full timeout) after the lease expired. The watchdog closes this gap.
const LeaseWatchdogSafetyMargin = 5 * time.Second

// DrainSessionNowPollInterval is the production poll granularity
// DrainSessionNow uses while waiting for a same-pump in-flight execution it
// lost the lease race against (see DrainSessionNow's own doc). Kept short:
// the wait is bounded by an ordinary turn's remaining duration, not a full
// lease TTL, and this is the loop's only source of added latency in that
// branch.
const DrainSessionNowPollInterval = 25 * time.Millisecond

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

// errLeaseLost is returned by executeEntrySync when the entry's lease was
// reassigned to a different owner during the renewal loop (see leaseLost's
// doc there). It carries no outcome for the row itself — the new owner
// writes the eventual Ack/Nack/TerminalFail — callers just need to know
// nothing further can be learned from this particular execution attempt.
var errLeaseLost = errors.New("run_queue_pump: lost lease ownership mid-execution")

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

	// TestDrainTick is a test seam for overriding the orphan outbox drain interval.
	// nil = use production OrphanOutboxDrainInterval.
	TestDrainTick func() time.Duration

	// TestDrainSessionPollInterval is a test seam for overriding
	// DrainSessionNowPollInterval, the poll granularity DrainSessionNow uses
	// while waiting for a same-pump in-flight execution it lost the lease
	// race against. 0 = use the production constant. Regression tests need
	// this small (sub-millisecond) to observe the race deterministically
	// without a real wall-clock wait.
	TestDrainSessionPollInterval time.Duration
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

// drainSessionPollInterval returns the effective poll interval
// DrainSessionNow uses while waiting on a same-pump in-flight execution —
// cfg.TestDrainSessionPollInterval if set, otherwise the production
// DrainSessionNowPollInterval.
func (p *RunQueuePump) drainSessionPollInterval() time.Duration {
	if p.cfg.TestDrainSessionPollInterval > 0 {
		return p.cfg.TestDrainSessionPollInterval
	}
	return DrainSessionNowPollInterval
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

	// Determine drain interval
	drainInterval := OrphanOutboxDrainInterval
	if p.cfg.TestDrainTick != nil {
		drainInterval = p.cfg.TestDrainTick()
	}
	drainTicker := time.NewTicker(drainInterval)
	defer drainTicker.Stop()

	// Start orphan outbox drain goroutine. Runs an initial drain immediately
	// (P1-4 fix, docs/reviews/2026-08-13-release-readiness-static-audit.md
	// §P1-4), mirroring p.tick()'s own initial call below — otherwise the
	// first drain wouldn't happen until drainInterval (15s in production)
	// has elapsed, and a short-lived process (crush run) could start and
	// exit without ever attempting to recover a pending outbox entry.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if p.ctx.Err() == nil {
			p.drainOrphanOutbox()
		}
		for {
			select {
			case <-p.ctx.Done():
				return
			case <-drainTicker.C:
				p.drainOrphanOutbox()
			}
		}
	}()

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

// drainOrphanOutbox performs one scan of the orphan outbox and attempts
// to move pending entries to the main run queue.
func (p *RunQueuePump) drainOrphanOutbox() {
	// Use a deadline-bound context for DB reads to prevent indefinite hangs
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	// Scan for pending orphan outbox entries
	pending, err := p.cfg.Sessions.ListPendingOrphanOutboxEntries(ctx)
	if err != nil {
		slog.Warn("run_queue_pump: drain orphan outbox failed to list pending", "err", err, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	if len(pending) == 0 {
		return // No orphan outbox work to do
	}

	slog.Info("run_queue_pump: draining orphan outbox entries", "count", len(pending), "instance_id", p.cfg.PumpInstanceID)

	// Attempt to move each pending entry to the main run queue
	for _, entry := range pending {
		if p.ctx.Err() != nil {
			return // Shutdown requested mid-drain
		}

		p.processOrphanOutboxEntry(ctx, entry)
	}
}

// processOrphanOutboxEntry attempts to move a single orphan
// outbox entry to the main run queue atomically.
func (p *RunQueuePump) processOrphanOutboxEntry(_ context.Context, entry db.OrphanCallOutbox) {
	// Derive from p.ctx, NOT context.Background() -- found by independent
	// review (docs/reviews/2026-08-13-release-readiness-static-audit.md,
	// finding M6): using context.Background() here detached this drain
	// entirely from p.ctx/Stop(), so a slow drain could keep running for
	// up to 10s past Stop() being called, well beyond Stop()'s own 5s
	// shutdown grace period -- which would then report a forced
	// (non-graceful) shutdown even though nothing was actually stuck, just
	// running on a clock nothing else knew about.
	//
	// Also NOT the caller's ctx parameter (drainOrphanOutbox's own 5s-
	// bound scan context, now unused here): chaining context.WithTimeout
	// off an already-5s-bound parent still caps the effective deadline at
	// 5s regardless of the timeout passed here (context.WithTimeout takes
	// the EARLIER of the two deadlines) -- that 5s budget is sized for the
	// list scan, not for this transaction, and cutting the transaction
	// short at that boundary would be worse than giving it a longer, but
	// still pump-shutdown-aware, budget. Deriving straight from p.ctx (not
	// context.Background(), not the 5s-capped scan ctx) gives both:
	// cancelled promptly on Stop(), with a real 10s budget in the
	// meantime.
	drainCtx, drainCancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer drainCancel()

	// Atomically drain the entry (INSERT to main queue + DELETE from orphan outbox in one tx)
	// This eliminates the vulnerable 'processing' intermediate state that could leave
	// entries stranded after crashes (P0-4 fix).
	//
	// No attempts-counter/terminal-failed state on this path (task #440
	// follow-up decision): an entry whose call_data is genuinely malformed
	// -- the ONLY way DrainOrphanOutboxEntry's inner INSERT can keep
	// failing forever, since the FK ON DELETE CASCADE on session_id
	// already removes any entry whose session no longer exists -- retries
	// on every drain tick indefinitely, logged at ERROR each time rather
	// than silently dropped or auto-escalated to a terminal state. This
	// was a deliberate choice to keep DrainOrphanOutboxEntry a single
	// atomic transaction (insert-to-main-queue + delete-from-outbox) with
	// no separate attempts-tracking write to race against; the older
	// claim/mark-failed model this superseded (task #426) is gone. See
	// internal/db/sql/orphan_outbox.sql's header comment for the full
	// rationale.
	drained, err := p.cfg.Sessions.DrainOrphanOutboxEntry(drainCtx, entry.ID)
	if err != nil {
		slog.Error("run_queue_pump: failed to drain orphan outbox entry", "id", entry.ID, "session_id", entry.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		return
	}
	if !drained {
		// Entry was already drained by another pump instance or not in pending state
		slog.Debug("run_queue_pump: orphan outbox entry already drained, skipping", "id", entry.ID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	slog.Info("run_queue_pump: successfully drained orphan outbox entry to main queue",
		"id", entry.ID,
		"session_id", entry.SessionID,
		"instance_id", p.cfg.PumpInstanceID)
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

// executeEntry runs a leased entry and handles success/failure. Called
// detached (go executeEntry(...)) by the background tick's processEntry,
// which has already reserved this call's execSem slot and workerWg
// registration and marked the session inFlight before dispatching — this
// releases all three via defer regardless of outcome, matching the
// admission it assumes was already granted.
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

	// The returned error is deliberately discarded here: the background
	// tick's own outcome handling (Ack/Nack/TerminalFail) already happened
	// inside executeEntrySync, and nothing on this detached path needs to
	// inspect the result further. DrainSessionNow (task #421/P0-1) is the
	// caller that DOES need it, to decide whether to keep draining or
	// surface a failure — see there.
	_ = p.executeEntrySync(ctx, leased)
}

// executeEntrySync is executeEntry's body, extracted (task #421/P0-1) so a
// synchronous caller — DrainSessionNow — can invoke the exact same
// lease/renew/watchdog/ack machinery without going through the async
// worker-pool bookkeeping (workerWg/execSem/inFlight) that wraps every
// executeEntry call site; each caller manages that bookkeeping itself, in
// whatever shape fits its own concurrency model.
//
// Returns nil on ack'd success. On failure, returns the underlying error
// (including the ErrCallQueuedNotExecuted sentinel, or an error matching
// AlreadyAttempted/*SessionLockBusyError) after already performing the
// matching Ack/Nack/TerminalFail write — callers never need to interpret
// the error to decide what to persist for the row, only what to do next on
// their own side (retry, stop, surface as a failure). Returns errLeaseLost
// if the lease was reassigned mid-execution (see leaseLost's own doc) — no
// outcome write happens in that case; the new owner is responsible for it.
func (p *RunQueuePump) executeEntrySync(ctx context.Context, leased *RunQueueEntry) error {
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
		return err
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
	// P0-3 fix: P1-1's watchdog still measured "TTL - margin from the last
	// successful renewal", where "last successful renewal" meant the wall-clock
	// time the RenewRunQueueLease call RETURNED (post-call), while the
	// lease_expires_at value it wrote to the DB was computed from the
	// wall-clock time the call STARTED (pre-call), via
	// newExpiresAt := time.Now().Add(TTL) before the DB round-trip. For a
	// FAST renewal these are indistinguishable, but for a SLOW-but-successful
	// renewal (DB contention, GC pause, disk stall) that takes duration D to
	// complete, the watchdog would believe the lease is D newer than what is
	// actually recorded in the DB. If D exceeds the safety margin, the
	// watchdog fires AFTER the real DB expiry has already passed — during
	// that gap, another pump's CleanupExpiredLeases can reclaim the row and
	// dispatch a duplicate execution while this one is still believed
	// (locally) to be safely within its lease. The fix: track the ABSOLUTE
	// lease_expires_at (leaseExpiresAtAtomic below), seeded from the row's
	// initial lease_expires_at (as returned by LeaseRunQueueEntry) and
	// updated, on each successful renewal, to the exact newExpiresAt value
	// that was just written to the DB — never to a post-call time.Now(). The
	// watchdog then compares wall-clock now() directly against that absolute
	// deadline minus the safety margin, so its decision always matches what
	// is actually persisted, regardless of how long any individual renewal
	// call took.
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
	// P0-3 fix: the watchdog must compare against the ABSOLUTE
	// lease_expires_at value actually committed to the DB, not against
	// "time since the last successful renewal call returned". The two are
	// NOT interchangeable: newExpiresAt is computed as time.Now().Add(TTL)
	// BEFORE the RenewRunQueueLease DB call is issued, and that exact value
	// (not a commit-time value) is what gets written to lease_expires_at.
	// If a successful renewal call takes D to complete (slow DB, GC pause,
	// lock contention), then "time since post-call timestamp" understates
	// the row's actual age by D. A watchdog keyed off the post-call
	// timestamp would then keep the execution alive for up to D longer than
	// the row is actually leased for — if D exceeds the safety margin, the
	// watchdog fires AFTER the real DB expiry, leaving a window where
	// another pump's CleanupExpiredLeases can already have reclaimed the
	// row and dispatched a duplicate execution while this one is still
	// running. Tracking the absolute expiry (seeded from the initially
	// leased row, then updated to the exact value written by each
	// successful renewal) makes the watchdog's deadline match the DB's
	// deadline exactly, independent of how long any individual renewal
	// call took.
	//
	// Design: Use atomic storage for the absolute lease deadline + ticker.
	// This avoids timer.Reset() complexity and channel race conditions.
	// The watchdog wakes every 10ms to check if we're past the safe deadline.
	//
	// The INITIAL seed uses leased.LeaseExpiresAt — the value
	// LeaseRunQueueEntry actually wrote to the DB — exactly like the
	// renewal path below reuses its own newExpiresAt. This is safe/precise
	// because LeaseRunQueueEntry rounds UP (ceiling) to the next whole Unix
	// second rather than flooring (see session.go's ceilUnixSeconds and its
	// use in LeaseRunQueueEntry): the persisted deadline can only be later
	// than the true, sub-second-precision intended deadline, by less than
	// 1s, never earlier. A plain floor here (or in LeaseRunQueueEntry) would
	// be fine for the production TTL (30s) but catastrophically imprecise
	// for the short TTLs (100s of milliseconds) this pump's own test suite
	// relies on to run quickly — e.g. a 300ms TTL would floor to +0 seconds,
	// seeding the watchdog with a deadline that has already passed.
	var leaseExpiresAtAtomic atomic.Int64 // Unix nanoseconds, absolute deadline
	leaseExpiresAtAtomic.Store(time.Unix(leased.LeaseExpiresAt, 0).UnixNano())

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
				// Check if watchdog should fire: compare now against the
				// absolute lease_expires_at actually committed to the DB,
				// minus the safety margin.
				leaseExpiresAt := time.Unix(0, leaseExpiresAtAtomic.Load())
				deadline := leaseExpiresAt.Add(-p.leaseWatchdogSafetyMargin())

				if !time.Now().Before(deadline) {
					// Watchdog deadline passed: cancel execution
					leaseLost.Store(true)
					execCancel()
					slog.Error("run_queue_pump: lease watchdog fired: canceling execution before expiry",
						"id", leased.ID,
						"session_id", leased.SessionID,
						"ttl", p.leaseTTL(),
						"safety_margin", p.leaseWatchdogSafetyMargin(),
						"lease_expires_at", leaseExpiresAt,
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

		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				// P0-3: derive the DB call's timeout budget, and whether a
				// renewal attempt is even worthwhile, from the SAME absolute
				// deadline the watchdog uses — not from a local
				// "time since we last heard back" clock. The two used to
				// diverge whenever a renewal call itself took a while to
				// complete (see the watchdog goroutine's doc above for the
				// exact failure mode this closes).
				currentExpiresAt := time.Unix(0, leaseExpiresAtAtomic.Load())
				timeUntilWatchdog := time.Until(currentExpiresAt.Add(-p.leaseWatchdogSafetyMargin()))
				if timeUntilWatchdog <= 0 {
					// We're already past the safe budget: watchdog is imminent.
					// Don't even attempt renewal — let the watchdog fire.
					continue
				}

				// newExpiresAt is computed from time.Now() here, BEFORE the
				// DB call, and this exact value (not a DB commit-time value)
				// is what RenewRunQueueLease writes to lease_expires_at on
				// success (see sql/run_queue.sql: RenewRunQueueLease sets
				// lease_expires_at = ? directly from the parameter). That
				// makes it safe and correct to seed the watchdog's absolute
				// deadline from this same value once the call succeeds,
				// below — it is exactly what lands in the DB, regardless of
				// how long the call itself takes to complete.
				//
				// ceilUnixSeconds (not a plain .Unix() floor): lease_expires_at
				// is a whole-Unix-seconds column, and a plain floor can lose
				// up to ~1s depending purely on the arbitrary sub-second
				// wall-clock phase a renewal happens to land on — for short
				// TTLs with correspondingly small safety margins (this pump's
				// own test suite uses TTLs as low as 100s of milliseconds),
				// that loss can exceed the ENTIRE margin, making the watchdog
				// fire far too early relative to the row's true intended
				// lifetime. This is a genuinely nondeterministic failure mode
				// (confirmed empirically: flips a real test between reliably
				// passing and reliably failing depending only on wall-clock
				// phase at process start), not a hypothetical edge case.
				// Rounding UP instead is the safe direction: it can only make
				// the DB-recorded (and watchdog-tracked) deadline later than
				// the true deadline, by less than 1s, never earlier — see
				// ceilUnixSeconds's doc and the P0-3 fix note on
				// LeaseRunQueueEntry for the matching initial-lease case.
				newExpiresAt := ceilUnixSeconds(time.Now().Add(p.leaseTTL()))
				renewDBCtx, renewDBCancel := context.WithTimeout(context.Background(), timeUntilWatchdog)
				ok, err := p.cfg.Sessions.RenewRunQueueLease(renewDBCtx, leased.ID, p.cfg.PumpInstanceID, newExpiresAt)
				renewDBCancel()
				if err != nil {
					slog.Warn("run_queue_pump: lease renewal failed, will retry next interval", "id", leased.ID, "session_id", leased.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
					continue
				}
				// P0-3: update the watchdog's absolute deadline to the exact
				// value just committed to the DB (newExpiresAt), NOT to
				// time.Now() + TTL measured after the call returned. Using
				// post-call time here would reintroduce the bug: a renewal
				// that took D to complete would make the watchdog believe
				// the lease is D newer than it actually is in the DB.
				leaseExpiresAtAtomic.Store(time.Unix(newExpiresAt, 0).UnixNano())

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
		return errLeaseLost
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
		return nil
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
		return err
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
		return err
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
		return err
	}

	// Retryable failure: nack and let the pump retry on next tick
	if nackErr := p.cfg.Sessions.NackRunQueueEntry(dbCtx, leased.ID, p.cfg.PumpInstanceID, err.Error()); nackErr != nil {
		slog.Error("run_queue_pump: nack failed", "id", leased.ID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
	}
	slog.Debug("run_queue_pump: entry failed, will retry", "id", leased.ID, "session_id", leased.SessionID, "err", err, "attempts", leased.Attempts+1, "instance_id", p.cfg.PumpInstanceID)
	return err
}

// DrainSessionNow synchronously executes every currently-pending run-queue
// entry for sessionID, blocking the caller until the session's durable
// queue is empty and no execution of it is in flight in this pump instance
// — or until ctx is done.
//
// It exists so a short-lived process (crush run) can finish a durable
// continuation in the SAME process instead of leaving it for some future
// invocation's background tick to eventually pick up (task #421/P0-1): a
// cross-process interrupt landing on a busy session cancels the in-flight
// generation and durably enqueues its replacement (handleInterruptTick,
// mailbox.go's FromDurableQueue guard), deliberately WITHOUT a live
// mb.replacement handoff — the durable row is the only remaining owner.
// Without something calling this, that row sits pending until the
// background pump's next tick (RunQueuePumpInterval, 3s in production)
// happens to fire before the process exits — a race the process routinely
// loses, since RunNonInteractive's own completion path runs in
// milliseconds after the cancellation.
//
// Returns drained=true if at least one entry was observed to execute for
// this session during the call — either leased and run by this call
// directly, or (see below) by this pump's own background tick racing ahead
// of it. drained=false, err=nil means nothing was pending; callers must
// NOT treat that as having recovered anything (a plain user-initiated
// cancel/timeout with no durable continuation looks identical to "nothing
// to drain" and must be left as the caller's original outcome).
//
// Race against the background tick: LeaseRunQueueEntry is atomic at the DB
// level, so two callers racing for the same row can never both execute it
// — but if the background tick wins the race, THIS call's own lease
// attempt simply finds nothing pending, even though the row is genuinely
// being executed right now by a goroutine this call didn't start. Silently
// returning "nothing to drain" in that case would reproduce the exact bug
// this function exists to close, just via a race instead of a certainty.
// The fix: check p.inFlight for this session before concluding there is
// nothing left to wait for. If busy, that busy state was set either by
// this call's own leasing branch below (mirroring processEntry) or by the
// background tick's processEntry/executeEntry — either way, poll (bounded,
// drainSessionPollInterval) until it clears, then loop back and check
// again for anything newly pending, rather than trying to coordinate a
// clean handoff between the two paths.
//
// Deliberately does NOT replicate processEntry's RunQueueMaxAttempts
// pre-check, busyBackoffUntil dedup, or admitMu/stopping shutdown gate —
// those exist for the long-running, many-tick background scenario. A
// synchronous drain bounded by the caller's own ctx (crush run's --timeout)
// does not need them: a genuinely stuck or poison entry hits ctx's
// deadline (or, for attempts, the loop below still honors
// RunQueueMaxAttempts directly so a truly poison entry terminal-fails
// instead of being retried forever inside one call) and this call returns
// with that error rather than looping unboundedly.
func (p *RunQueuePump) DrainSessionNow(ctx context.Context, sessionID string) (drained bool, err error) {
	for {
		if ctx.Err() != nil {
			return drained, ctx.Err()
		}

		leased, leaseErr := p.cfg.Sessions.LeaseRunQueueEntry(ctx, sessionID, p.cfg.PumpInstanceID, p.leaseTTL())
		if leaseErr != nil {
			return drained, leaseErr
		}

		if leased != nil {
			drained = true

			// Mirrors processEntry's own attempts-exhausted check (see
			// there) — this call bypassed that check by leasing directly
			// instead of scanning pending entries first, so it must be
			// re-applied here or a poison entry that always fails would
			// retry inside this loop until ctx's deadline instead of
			// terminal-failing at RunQueueMaxAttempts like every other
			// path does.
			if leased.Attempts >= RunQueueMaxAttempts && !leased.TerminalFailure {
				termCtx, termCancel := context.WithTimeout(context.Background(), p.dbWriteTimeout())
				termErr := p.cfg.Sessions.TerminalFailRunQueueEntry(termCtx, leased.ID, p.cfg.PumpInstanceID)
				termCancel()
				if termErr != nil {
					slog.Error("run_queue_pump: DrainSessionNow terminal fail failed", "id", leased.ID, "session_id", sessionID, "err", termErr, "instance_id", p.cfg.PumpInstanceID)
				}
				err = fmt.Errorf("run queue entry %q exceeded max attempts", leased.ID)
				continue
			}

			p.inFlightMu.Lock()
			p.inFlight[sessionID] = struct{}{}
			p.inFlightMu.Unlock()

			select {
			case p.execSem <- struct{}{}:
			case <-ctx.Done():
				// Release the lease we just took rather than leaving it
				// leased with nobody executing it. A later tick (this
				// process or another) recovers it via the ordinary
				// lease-expiry path regardless, but releasing promptly
				// avoids waiting out a full TTL for no reason.
				p.inFlightMu.Lock()
				delete(p.inFlight, sessionID)
				p.inFlightMu.Unlock()
				nackCtx, nackCancel := context.WithTimeout(context.Background(), p.dbWriteTimeout())
				if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(nackCtx, leased.ID, p.cfg.PumpInstanceID, "run_queue_pump: DrainSessionNow's ctx ended before an execution slot was available"); nackErr != nil {
					slog.Error("run_queue_pump: DrainSessionNow release-on-ctx-done nack failed", "id", leased.ID, "session_id", sessionID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
				}
				nackCancel()
				return drained, ctx.Err()
			}

			execErr := p.executeEntrySync(ctx, leased)

			<-p.execSem
			p.inFlightMu.Lock()
			delete(p.inFlight, sessionID)
			p.inFlightMu.Unlock()

			if errors.Is(execErr, ErrCallQueuedNotExecuted) || isSessionLockBusyErr(execErr) {
				// A genuinely different live owner has this session right
				// now — not this call, not the background tick (see those
				// errors' own docs on executeEntrySync). Waiting for a
				// stranger's turn to finish is not this call's job: stop
				// and let the caller's original outcome stand for
				// whatever wasn't drained.
				return drained, nil
			}
			err = execErr
			continue // more may be pending (e.g. a second stacked interrupt) — loop and check again
		}

		// Nothing pending for THIS call to lease. Someone else in this
		// pump instance — the background tick, racing ahead of us — might
		// already be executing this session's entry; if so, wait for it
		// to finish and re-check, since its outcome matters exactly as
		// much as this call's own would (see the race note in the doc
		// comment above).
		p.inFlightMu.Lock()
		_, busy := p.inFlight[sessionID]
		p.inFlightMu.Unlock()
		if !busy {
			return drained, err
		}
		drained = true
		select {
		case <-ctx.Done():
			return drained, ctx.Err()
		case <-time.After(p.drainSessionPollInterval()):
		}
	}
}

// isSessionLockBusyErr reports whether err is (or wraps) a
// *SessionLockBusyError — the same check executeEntrySync performs inline,
// factored out so DrainSessionNow can use it without duplicating the
// errors.As boilerplate.
func isSessionLockBusyErr(err error) bool {
	var busyErr *SessionLockBusyError
	return errors.As(err, &busyErr)
}
