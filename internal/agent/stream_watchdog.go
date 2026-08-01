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
	// A single toolMaxDuration bounds the pause regardless of which tool is
	// in flight — including a sub-agent delegation via the `agent` tool.
	// There used to be a separate, larger cap just for delegations, but
	// distinguishing "exempt" from "plain" tools this way produced its own
	// false cutoffs: a sub-agent's OWN plain tool call (a slow build/test
	// inside its turn) hit the shorter plain cap just as often as a parent
	// waiting on a delegation hit the old short cap. One generous cap for
	// every tool avoids both failure modes at the cost of a genuinely wedged
	// tool taking longer to get caught — an acceptable tradeoff since the
	// point is to bound the wait, not to bound it tightly.
	toolStarted  func()
	toolFinished func()
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
// ends instead of hanging forever on a stuck tool. One cap applies to
// every tool in flight, sub-agent delegations included — see toolStarted's
// doc on the streamWatchdog struct for why a single generous cap replaced
// the old plain-vs-exempt split.
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

	toolStarted := func() {
		if toolsInFlight.Add(1) == 1 {
			toolStartedAt.Store(time.Now().UnixNano())
		}
	}
	toolFinished := func() {
		if remaining := toolsInFlight.Add(-1); remaining <= 0 {
			// Defensive: a missing OnToolCall (or a double result) must not
			// leave the counter negative and silently disable the watchdog.
			toolsInFlight.Store(0)
			toolStartedAt.Store(0)
		} else {
			// Other tools from this batch are still in flight. fantasy fires
			// every OnToolCall for a step BEFORE executing any of them, so a
			// "batch" here is often several tool calls the model issued
			// together but that the executor actually runs one at a time —
			// not true parallel execution. Without this reset,
			// toolStartedAt stays pinned to when the FIRST tool in the
			// batch started, so toolMaxDuration bounds the batch's
			// CUMULATIVE wall time instead of any single tool's runtime —
			// several individually-fast sequential tool calls (e.g. four
			// 8s bash commands under a 12s cap) could sum past the cap and
			// get force-cancelled even though none of them was ever
			// actually stuck (observed live: a sub-agent running four
			// short, safely-bounded bash steps got killed by this exact
			// accumulation). This finish is forward progress on the batch
			// — restart the clock from now, so the cap instead bounds the
			// gap since the LAST tool finished (or the batch started,
			// whichever is more recent). A genuinely stuck remaining tool
			// is still caught: if nothing finishes for a full
			// toolMaxDuration after this reset, the next tick fires
			// exactly as before.
			toolStartedAt.Store(time.Now().UnixNano())
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
				// The pause is bounded: past the applicable cap the watchdog
				// fires with toolTimeout=true (never-freeze backstop).
				if toolsInFlight.Load() > 0 {
					// One cap for the whole batch, regardless of which kind of
					// tool is in it — including a sub-agent delegation via the
					// `agent` tool. Two earlier revisions got this wrong in
					// opposite directions: applying the short primitive-tool cap
					// to a delegation force-cancelled healthy sub-agent work
					// that legitimately runs long; skipping the cap entirely
					// for delegations (and letting the `continue` below bypass
					// every other deadline check too) left the watchdog
					// structurally unable to fire AT ALL while a delegation was
					// in flight — an unbounded parent wait, which is how a
					// wedged child silently froze the whole process (observed:
					// four concurrent `agent` calls in flight, then 15+ minutes
					// of total silence with the process still alive, no error,
					// no finish part, no diagnostics). A single generous cap
					// applied uniformly avoids both: nothing gets a shorter
					// leash than anything else, and nothing gets no leash.
					if toolMaxDuration > 0 {
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
					// An EXPLICITLY configured hard cap must actually be hard:
					// it bounds the whole turn, tool execution included.
					// absoluteDeadline is deliberately NOT applied here — that
					// one is the idle deadline, and a long but healthy tool run
					// must not trip it.
					if hardCap > 0 && now.After(hardDeadline) {
						stalled.Store(true)
						cancel()
						if onFire != nil {
							onFire(now.Sub(startTime), true)
						}
						return
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
