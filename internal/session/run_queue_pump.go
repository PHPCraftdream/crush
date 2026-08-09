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
//   - Uses TryAcquireSessionLock to avoid contention with active sessions
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
	// Run executes a call for the given session. Returns the result or an error.
	Run(ctx context.Context, sessionID, prompt string, providerOptions map[string]any, attachments ...any) (*any, error)
}

// RunQueuePumpConfig configures a RunQueuePump instance.
type RunQueuePumpConfig struct {
	// Sessions is the session service for enqueue/lease/ack operations.
	Sessions Service

	// DataDirectory is the workspace root for OS lock acquisition.
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
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
		tickCh: make(chan struct{}, 1),
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
	// Skip if attempts exceeded (unless terminal failure flag is set)
	if entry.Attempts >= RunQueueMaxAttempts && !entry.TerminalFailure {
		slog.Warn("run_queue_pump: entry exceeded max attempts, terminal failing",
			"id", entry.ID, "session_id", entry.SessionID, "attempts", entry.Attempts, "instance_id", p.cfg.PumpInstanceID)
		if err := p.cfg.Sessions.TerminalFailRunQueueEntry(ctx, entry.ID); err != nil {
			slog.Error("run_queue_pump: terminal fail failed", "id", entry.ID, "err", err, "instance_id", p.cfg.PumpInstanceID)
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

	// Parse the serialized SessionAgentCall (we use a simple map for now)
	var call struct {
		SessionID       string
		Prompt          string
		ProviderOptions map[string]any
	}
	if err := json.Unmarshal([]byte(leased.CallData), &call); err != nil {
		slog.Error("run_queue_pump: failed to unmarshal call data", "id", leased.ID, "session_id", leased.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		// Don't retry this - malformed data won't fix itself
		if err := p.cfg.Sessions.TerminalFailRunQueueEntry(ctx, leased.ID); err != nil {
			slog.Error("run_queue_pump: terminal fail failed after unmarshal error", "id", leased.ID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		}
		return
	}

	// Execute the call (detached, not blocking this pump tick)
	go p.executeEntry(ctx, leased, call.SessionID, call.Prompt, call.ProviderOptions)
}

// executeEntry runs a leased entry and handles success/failure.
func (p *RunQueuePump) executeEntry(ctx context.Context, leased *RunQueueEntry, sessionID, prompt string, providerOptions map[string]any) {
	// Create a fresh context for this execution (not tied to pump lifecycle)
	execCtx := context.Background()

	// Attempt to execute via coordinator.Run
	_, err := p.cfg.Coordinator.Run(execCtx, sessionID, prompt, providerOptions)

	// Handle outcome
	if err == nil {
		// Success: ack the entry (delete it)
		if _, ackErr := p.cfg.Sessions.AckRunQueueEntry(ctx, leased.ID); ackErr != nil {
			slog.Error("run_queue_pump: ack failed after success", "id", leased.ID, "session_id", leased.SessionID, "err", ackErr, "instance_id", p.cfg.PumpInstanceID)
		}
		slog.Info("run_queue_pump: executed entry successfully", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
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

	// Retryable failure: nack and let the pump retry on next tick
	if nackErr := p.cfg.Sessions.NackRunQueueEntry(ctx, leased.ID, err.Error()); nackErr != nil {
		slog.Error("run_queue_pump: nack failed", "id", leased.ID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
	}
	slog.Debug("run_queue_pump: entry failed, will retry", "id", leased.ID, "session_id", leased.SessionID, "err", err, "attempts", leased.Attempts+1, "instance_id", p.cfg.PumpInstanceID)
}
