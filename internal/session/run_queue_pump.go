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
	// This is reachable without any external contention, self-inflicted by
	// the pump alone: RunQueueLeaseTTL (30s) is far shorter than a real
	// LLM turn can take, and executeEntry never renews its lease while
	// Coordinator.Run is in flight, so CleanupExpiredLeases can return an
	// entry to pending while the goroutine executing it is still genuinely
	// running — the next tick then leases and dispatches a SECOND goroutine
	// for the same session. It is also reachable with no lease-expiry
	// involved at all: tick() leases and dispatches entries one at a time
	// in a single pass (LeaseRunQueueEntry claims the oldest pending entry
	// PER SESSION), so two distinct durably-queued entries for the same
	// session — e.g. two calls queued while a process was down — are
	// leased and `go executeEntry`-dispatched back to back within the same
	// tick, before either has run long enough to matter.
	//
	// inFlight closes both paths at the source: processEntry refuses to
	// lease a pending entry whose session already has an executeEntry
	// goroutine running FROM THIS PUMP INSTANCE, so this pump can never
	// concurrently (or via a self-caused lease-expiry) dispatch two
	// entries for the same session. See executeEntry's own Run()-result
	// handling for the narrower residual case this does NOT cover: a
	// genuinely external, non-pump owner (e.g. a live user-facing
	// process) holding the session when the pump attempts it.
	inFlight   map[string]struct{}
	inFlightMu sync.Mutex

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
		cfg:      cfg,
		ctx:      ctx,
		cancel:   cancel,
		inFlight: make(map[string]struct{}),
		tickCh:   make(chan struct{}, 1),
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

	// Skip if attempts exceeded (unless terminal failure flag is set).
	// TerminalFailRunQueueEntry only deletes rows in 'leased' state, but an
	// attempts-exhausted entry sits in 'pending' (that's how it was scanned
	// here) — it must be leased first, or the DELETE never matches and this
	// same entry gets re-scanned and re-fails to terminal-fail on every
	// subsequent tick forever.
	if entry.Attempts >= RunQueueMaxAttempts && !entry.TerminalFailure {
		slog.Warn("run_queue_pump: entry exceeded max attempts, terminal failing",
			"id", entry.ID, "session_id", entry.SessionID, "attempts", entry.Attempts, "instance_id", p.cfg.PumpInstanceID)
		leased, err := p.cfg.Sessions.LeaseRunQueueEntry(ctx, entry.SessionID, p.cfg.PumpInstanceID, RunQueueLeaseTTL)
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
	leased, err := p.cfg.Sessions.LeaseRunQueueEntry(ctx, entry.SessionID, p.cfg.PumpInstanceID, RunQueueLeaseTTL)
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

	// Attempt to execute via coordinator.Run
	_, err := p.cfg.Coordinator.Run(execCtx, callData)

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
	// ordinary retryable failure. Leave the entry exactly as leased and do
	// nothing further; RunQueueLeaseTTL's natural expiry is the recovery
	// path once the external owner has had a full lease window to drain it.
	if errors.Is(err, ErrCallQueuedNotExecuted) {
		slog.Debug("run_queue_pump: call was queued into an externally-owned session, leaving leased for natural expiry", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
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
