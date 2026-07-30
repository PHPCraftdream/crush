package agent

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// streamWatchdog implements the "Codec must surface control" invariant for
// LLM streaming: if a provider stops sending data mid-stream (network
// glitch, rate-limit, HTTP/2 stall, backend hiccup), we MUST detect it and
// force-unblock the agent rather than freeze with it. The 162-promise-all
// stuck session (D:\dev\garnet-team\.crush, see post-mortem in
// CHANGELOG.fork.md section 4.D) is the failure mode this protects
// against — 4 streams froze for 1.5h until the user killed the process.
type streamWatchdog struct {
	// bump records progress: called from every fantasy stream callback
	// (OnTextDelta, OnReasoningDelta, OnToolInputStart, OnToolCall,
	// OnToolResult, OnStepFinish, OnRetry, ...).
	bump func()
	// toolStarted / toolFinished bracket synchronous tool execution. While
	// any tool is in flight the idle timer is PAUSED — a long `cargo build`,
	// `cargo clippy`, test run or compile is not a provider stall, and the
	// provider legitimately sends nothing until the tool returns. Without
	// this, any single bash command longer than idleTimeout was force-
	// cancelled as a false stall (observed: shamir-db f1-rename-index killed
	// at exactly 180s during a workspace clippy).
	//
	// Each takes an `exempt` bool: true for a sub-agent delegation (the
	// `agent` tool) — see toolStarted's own doc below for why toolMaxDuration
	// must not apply to it.
	toolStarted  func(exempt bool)
	toolFinished func(exempt bool)
	// stalled reports whether THIS watchdog (not user/web cancel) is what
	// fired the cancel. Callers use it to distinguish "user pressed
	// Ctrl-C" from "watchdog timed out" when crafting the finish-part
	// message the user sees.
	stalled *atomic.Bool
	// done is closed when the watchdog goroutine has exited. Callers
	// MUST receive from it before returning to avoid a goroutine leak.
	done <-chan struct{}
}

// startStreamWatchdog launches a goroutine that calls cancel() if bump() is
// not invoked for idleTimeout. The goroutine exits when ctx is cancelled
// (either by the caller or by the watchdog itself) and closes done.
//
// onFire, if non-nil, is invoked AFTER stalled is set to true and BEFORE
// cancel() — typically used to emit a slog.Warn with diagnostic context the
// watchdog itself does not have (session ID, model, provider, etc.). The
// toolTimeout bool is true when the fire was triggered by a tool exceeding
// toolMaxDuration (rather than by provider idle).
//
// Fork patch: batch 8 — extendsOnProgress + hardCap parameters.
// When extendsOnProgress is true, every bump() also extends the effective
// deadline to max(absoluteDeadline, now+idleTimeout), capped at hardCap
// from the start time. This prevents killing healthy long compositions
// while still bounding the worst case.
//
// Fork patch: never-freeze backstop — toolMaxDuration bounds the tool
// pause. Past it the watchdog fires with toolTimeout=true so the turn
// ends instead of hanging forever on a stuck tool.
func startStreamWatchdog(
	ctx context.Context,
	cancel context.CancelFunc,
	idleTimeout, tick time.Duration,
	onFire func(elapsed time.Duration, toolTimeout bool),
	extendsOnProgress bool,
	hardCap time.Duration,
	toolMaxDuration time.Duration,
) streamWatchdog {
	var last atomic.Int64
	startTime := time.Now()
	last.Store(startTime.UnixNano())
	var stalled atomic.Bool
	// toolsInFlight counts tool executions currently running. While > 0 the
	// idle timer is paused (see toolStarted/toolFinished in the struct doc).
	var toolsInFlight atomic.Int64
	// toolStartedAt records the wall-clock time (UnixNano) at which the
	// first tool in the current in-flight batch started. Used with
	// toolMaxDuration to bound the tool pause. Reset to 0 when all tools
	// finish.
	var toolStartedAt atomic.Int64
	// exemptToolsInFlight counts currently-running tool calls that must NOT
	// be bounded by toolMaxDuration — specifically sub-agent delegations via
	// the `agent` tool (see AgentToolName). A delegated sub-agent's own
	// Run() call already has its OWN stream watchdog providing never-freeze
	// protection for whatever IT does; bounding the PARENT's wait on that
	// same call a second time, with the same cap meant for a single
	// primitive tool like bash, kills perfectly healthy delegations that
	// simply take longer than toolMaxDuration to finish non-trivial work
	// (routinely 15-30+ minutes for real tasks) — this was a real,
	// reproducible bug: a parent's `agent` tool call was force-cancelled by
	// this watchdog with "context canceled" while its sub-agent was still
	// productively working, well before the sub-agent's own internal
	// watchdog would have had reason to fire. The idle timer still pauses
	// normally while an exempt tool is in flight (a blocked, silent
	// delegation call is exactly the "provider legitimately sends nothing"
	// case toolsInFlight already exists to handle) — only the toolMaxDuration
	// hard-cap check is skipped for as long as at least one exempt tool is
	// part of the current in-flight batch.
	var exemptToolsInFlight atomic.Int64
	// absoluteDeadline is the original deadline from process start.
	absoluteDeadline := startTime.Add(idleTimeout)
	// hardDeadline is the hard cap (e.g. 4x idleTimeout).
	hardDeadline := absoluteDeadline
	if hardCap > 0 {
		hardDeadline = startTime.Add(hardCap)
	}
	done := make(chan struct{})

	// Rate-limited logging for deadline extensions.
	var lastLogNanos atomic.Int64
	const logInterval = 30 * time.Second

	bump := func() {
		now := time.Now()
		last.Store(now.UnixNano())
		if extendsOnProgress {
			// Extend the absolute deadline, capped at hardDeadline.
			newDeadline := now.Add(idleTimeout)
			if newDeadline.After(hardDeadline) {
				newDeadline = hardDeadline
			}
			// Rate-limited INFO log for extensions.
			if now.UnixNano()-lastLogNanos.Load() > int64(logInterval) {
				lastLogNanos.Store(now.UnixNano())
				slog.Info(
					"stream-watchdog deadline extended",
					"new_deadline", newDeadline.Format(time.RFC3339),
					"reason", "progress",
				)
			}
		}
	}

	toolStarted := func(exempt bool) {
		if toolsInFlight.Add(1) == 1 {
			toolStartedAt.Store(time.Now().UnixNano())
		}
		if exempt {
			exemptToolsInFlight.Add(1)
		}
	}
	toolFinished := func(exempt bool) {
		if toolsInFlight.Add(-1) <= 0 {
			// Defensive: a missing OnToolCall (or a double result) must not
			// leave the counter negative and silently disable the watchdog.
			toolsInFlight.Store(0)
			toolStartedAt.Store(0)
		}
		if exempt {
			if exemptToolsInFlight.Add(-1) < 0 {
				// Same defensive floor as toolsInFlight above.
				exemptToolsInFlight.Store(0)
			}
		}
		// Restart the idle clock fresh: the provider is about to resume, so
		// don't count the tool's runtime against the next stall window.
		last.Store(time.Now().UnixNano())
	}

	go func() {
		defer close(done)
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				// Pause while a tool is executing — provider silence during a
				// long compile/test is expected, not a stall. Keep `last`
				// fresh so the idle window starts clean once the tool returns.
				// The pause is bounded by toolMaxDuration: past it the
				// watchdog fires with toolTimeout=true (never-freeze
				// backstop) — UNLESS an exempt tool (a sub-agent delegation)
				// is part of the current batch, in which case that cap is
				// skipped entirely for as long as it's in flight (see
				// exemptToolsInFlight's doc above).
				if toolsInFlight.Load() > 0 {
					if toolMaxDuration > 0 && exemptToolsInFlight.Load() == 0 {
						if startedAt := toolStartedAt.Load(); startedAt > 0 {
							if elapsed := now.Sub(time.Unix(0, startedAt)); elapsed >= toolMaxDuration {
								stalled.Store(true)
								cancel()
								if onFire != nil {
									onFire(elapsed, true)
								}
								return
							}
						}
					}
					last.Store(now.UnixNano())
					continue
				}
				lastActivity := time.Unix(0, last.Load())
				idle := now.Sub(lastActivity)

				if extendsOnProgress {
					// Effective deadline: max(absoluteDeadline,
					// lastActivity+idleTimeout), capped at hardDeadline.
					effectiveDeadline := absoluteDeadline
					extended := lastActivity.Add(idleTimeout)
					if extended.After(effectiveDeadline) {
						effectiveDeadline = extended
					}
					if effectiveDeadline.After(hardDeadline) {
						effectiveDeadline = hardDeadline
					}
					if now.After(effectiveDeadline) {
						stalled.Store(true)
						cancel()
						if onFire != nil {
							onFire(idle, false)
						}
						return
					}
				} else {
					// Original behavior: fire if idle since last
					// activity exceeds idleTimeout.
					if idle >= idleTimeout {
						stalled.Store(true)
						cancel()
						if onFire != nil {
							onFire(idle, false)
						}
						return
					}
				}
			}
		}
	}()
	return streamWatchdog{
		bump:         bump,
		toolStarted:  toolStarted,
		toolFinished: toolFinished,
		stalled:      &stalled,
		done:         done,
	}
}
