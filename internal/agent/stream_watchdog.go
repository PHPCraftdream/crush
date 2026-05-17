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
// watchdog itself does not have (session ID, model, provider, etc.).
//
// Fork patch: batch 8 — extendsOnProgress + hardCap parameters.
// When extendsOnProgress is true, every bump() also extends the effective
// deadline to max(absoluteDeadline, now+idleTimeout), capped at hardCap
// from the start time. This prevents killing healthy long compositions
// while still bounding the worst case.
func startStreamWatchdog(
	ctx context.Context,
	cancel context.CancelFunc,
	idleTimeout, tick time.Duration,
	onFire func(idle time.Duration),
	extendsOnProgress bool,
	hardCap time.Duration,
) streamWatchdog {
	var last atomic.Int64
	startTime := time.Now()
	last.Store(startTime.UnixNano())
	var stalled atomic.Bool
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
				slog.Info("stream-watchdog deadline extended",
					"new_deadline", newDeadline.Format(time.RFC3339),
					"reason", "progress",
				)
			}
		}
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
				lastActivity := time.Unix(0, last.Load())
				now := time.Now()
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
							onFire(idle)
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
							onFire(idle)
						}
						return
					}
				}
			}
		}
	}()
	return streamWatchdog{
		bump:    bump,
		stalled: &stalled,
		done:    done,
	}
}
