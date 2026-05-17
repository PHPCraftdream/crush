package agent

import (
	"context"
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
func startStreamWatchdog(
	ctx context.Context,
	cancel context.CancelFunc,
	idleTimeout, tick time.Duration,
	onFire func(idle time.Duration),
) streamWatchdog {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var stalled atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				idle := time.Since(time.Unix(0, last.Load()))
				if idle >= idleTimeout {
					stalled.Store(true)
					// Cancel FIRST so the unblock isn't gated on whatever
					// onFire does. onFire is typically a slog.Warn, which
					// is fast in default config, but a third-party handler
					// (Datadog/Loki/etc.) could do network I/O — that
					// must not delay surfacing control to the caller.
					cancel()
					if onFire != nil {
						onFire(idle)
					}
					return
				}
			}
		}
	}()
	return streamWatchdog{
		bump:    func() { last.Store(time.Now().UnixNano()) },
		stalled: &stalled,
		done:    done,
	}
}
