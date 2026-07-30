package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// All tests use short timeouts (tens of ms) to keep the suite fast. The
// real production constants (streamIdleTimeout = 3min, tick = 30s) are not
// exercised here — that's a property of the integration, not of this unit.

func TestStreamWatchdog_BumpKeepsItAlive(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 80 * time.Millisecond
	const tick = 10 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, bool) {
		fired.Add(1)
	}, false, 0, 0)
	// Bump every 20ms for ~300ms — well past idle*3 worth of ticks.
	// Watchdog must NOT fire.
	stop := time.After(300 * time.Millisecond)
loop:
	for {
		select {
		case <-stop:
			break loop
		case <-time.After(20 * time.Millisecond):
			wd.bump()
		}
	}

	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire while bump() is called more often than idleTimeout")
	assert.False(t, wd.stalled.Load(), "stalled flag must stay false")
	assert.NoError(t, ctx.Err(), "ctx must not be cancelled by the watchdog")
}

func TestStreamWatchdog_FiresOnNoActivity(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 60 * time.Millisecond
	const tick = 10 * time.Millisecond

	var fired atomic.Int32
	var firedIdle atomic.Int64
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(observedIdle time.Duration, _ bool) {
		fired.Add(1)
		firedIdle.Store(int64(observedIdle))
	}, false, 0, 0)

	// Wait long enough for the watchdog to fire on its own.
	select {
	case <-wd.done:
		// Good — watchdog exited (it fires THEN exits).
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog never fired after idle period")
	}

	assert.Equal(t, int32(1), fired.Load(), "onFire should be called exactly once")
	assert.True(t, wd.stalled.Load(), "stalled flag must be true after fire")
	assert.Error(t, ctx.Err(), "ctx must be cancelled by the watchdog")
	assert.GreaterOrEqual(t, time.Duration(firedIdle.Load()), idle,
		"observed idle passed to onFire must be >= idleTimeout")
}

func TestStreamWatchdog_ExitsCleanlyOnCtxCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	const idle = 5 * time.Second // very long — must not fire
	const tick = 10 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, bool) {
		fired.Add(1)
	}, false, 0, 0)

	// Cancel ctx externally — watchdog must exit promptly without firing.
	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case <-wd.done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("watchdog did not exit after external ctx cancel")
	}

	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must NOT fire when ctx is cancelled externally — that's the user/cooperative path")
	assert.False(t, wd.stalled.Load(),
		"stalled flag stays false because the cancel was NOT the watchdog's doing")
}

func TestStreamWatchdog_BumpAfterFireIsHarmless(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 30 * time.Millisecond
	const tick = 5 * time.Millisecond

	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, bool) {}, false, 0, 0)

	// Let it fire.
	<-wd.done
	require.True(t, wd.stalled.Load())

	// Calling bump() after the goroutine has exited is a no-op — it
	// just stores into an atomic that nobody reads anymore. Must not
	// panic or deadlock.
	require.NotPanics(t, func() {
		wd.bump()
		wd.bump()
	})
}

// TestStreamWatchdog_PausedDuringToolExecution verifies the idle timer is
// frozen while a tool is executing — a long `cargo`/compile run is not a
// provider stall and must not be force-cancelled.
func TestStreamWatchdog_PausedDuringToolExecution(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 60 * time.Millisecond
	const tick = 10 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, bool) {
		fired.Add(1)
	}, false, 0, 0)
	// A tool starts and runs WAY past idleTimeout with zero provider
	// activity — the watchdog must NOT fire.
	wd.toolStarted(false)
	time.Sleep(idle * 4)
	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire while a tool is executing, even past idleTimeout")
	assert.False(t, wd.stalled.Load())
	assert.NoError(t, ctx.Err())

	// Tool finishes; with no further activity the watchdog resumes and must
	// fire after the idle window.
	wd.toolFinished(false)
	select {
	case <-wd.done:
	case <-time.After(idle + 300*time.Millisecond):
		t.Fatal("watchdog must fire after the tool finished and the stream went idle")
	}
	assert.Equal(t, int32(1), fired.Load())
	assert.True(t, wd.stalled.Load())
}

// TestStreamWatchdog_PauseCountsParallelTools verifies the pause is
// reference-counted: finishing one of several in-flight tools must keep the
// watchdog paused until ALL of them complete.
func TestStreamWatchdog_PauseCountsParallelTools(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 50 * time.Millisecond
	const tick = 10 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, bool) {
		fired.Add(1)
	}, false, 0, 0)
	// Two parallel tool calls in flight; finishing ONE must keep the
	// watchdog paused (counter still > 0).
	wd.toolStarted(false)
	wd.toolStarted(false)
	wd.toolFinished(false)
	time.Sleep(idle * 3)
	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must stay paused while any tool is still in flight")
	assert.False(t, wd.stalled.Load())
}

// Fork patch: batch 8 — tests for progress-based deadline extension.

// TestStreamWatchdog_ExtendsOnProgress verifies that with extendsOnProgress
// enabled, continuous progress keeps the watchdog alive beyond the original
// idle timeout.
func TestStreamWatchdog_ExtendsOnProgress(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 80 * time.Millisecond
	const tick = 10 * time.Millisecond
	const hardCap = 500 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, bool) {
		fired.Add(1)
	}, true, hardCap, 0)
	defer func() {
		cancel()
		<-wd.done
	}()

	// Bump every 30ms for 300ms — extends the deadline each time.
	stop := time.After(300 * time.Millisecond)
loop:
	for {
		select {
		case <-stop:
			break loop
		case <-time.After(30 * time.Millisecond):
			wd.bump()
		}
	}

	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire while progress keeps arriving")
	assert.False(t, wd.stalled.Load())
}

// TestStreamWatchdog_ExtendsOnProgress_FiresWhenIdle verifies that with
// extendsOnProgress, the watchdog still fires when progress stops.
func TestStreamWatchdog_ExtendsOnProgress_FiresWhenIdle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())

	const idle = 60 * time.Millisecond
	const tick = 10 * time.Millisecond
	const hardCap = 500 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, bool) {
		fired.Add(1)
	}, true, hardCap, 0)

	// Bump once to extend, then stop.
	wd.bump()

	select {
	case <-wd.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog should have fired after progress stopped")
	}

	assert.Equal(t, int32(1), fired.Load(), "watchdog must fire when progress stops")
	assert.True(t, wd.stalled.Load())
}

// TestStreamWatchdog_HardCapRespected verifies that even with continuous
// progress, the watchdog fires at the hard cap.
func TestStreamWatchdog_HardCapRespected(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())

	const idle = 80 * time.Millisecond
	const tick = 10 * time.Millisecond
	const hardCap = 200 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, bool) {
		fired.Add(1)
	}, true, hardCap, 0)

	start := time.Now()

	// Bump rapidly — but hard cap should still kill it.
	stop := time.After(500 * time.Millisecond)
loop:
	for {
		select {
		case <-wd.done:
			break loop
		case <-stop:
			t.Fatal("watchdog should have fired at hard cap")
		case <-time.After(10 * time.Millisecond):
			wd.bump()
		}
	}

	elapsed := time.Since(start)
	assert.Equal(t, int32(1), fired.Load(), "watchdog must fire at hard cap")
	assert.True(t, wd.stalled.Load())
	// The hard cap is 200ms with a tick of 10ms, so it should fire
	// somewhere between 200-250ms.
	assert.LessOrEqual(t, elapsed, 350*time.Millisecond,
		"watchdog must fire near the hard cap")
}

// TestStreamWatchdog_ToolPauseBoundedByCap verifies the never-freeze
// backstop: when toolMaxDuration > 0 and a tool stays in flight past that
// cap, the watchdog fires with toolTimeout==true instead of pausing
// forever. This is what keeps a stuck tool (hung MCP tool, blocking
// job_output --wait) from freezing the whole agent turn.
func TestStreamWatchdog_ToolPauseBoundedByCap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())

	const idle = 5 * time.Second // large — idle path must NOT fire
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 60 * time.Millisecond

	var fired atomic.Int32
	var firedToolTimeout atomic.Bool
	var firedElapsed atomic.Int64
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(elapsed time.Duration, toolTimeout bool) {
		fired.Add(1)
		firedToolTimeout.Store(toolTimeout)
		firedElapsed.Store(int64(elapsed))
	}, false, 0, toolMaxDuration)

	// A tool starts and runs past toolMaxDuration with zero provider
	// activity. The watchdog must fire with toolTimeout==true.
	wd.toolStarted(false)
	select {
	case <-wd.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog never fired after toolMaxDuration")
	}

	assert.Equal(t, int32(1), fired.Load(), "onFire should fire exactly once")
	assert.True(t, firedToolTimeout.Load(), "toolTimeout must be true when the cap is exceeded")
	assert.True(t, wd.stalled.Load(), "stalled flag must be true after fire")
	assert.Error(t, ctx.Err(), "ctx must be cancelled by the watchdog")
	assert.GreaterOrEqual(t, time.Duration(firedElapsed.Load()), toolMaxDuration,
		"elapsed passed to onFire must be >= toolMaxDuration")
}

// TestStreamWatchdog_ToolPauseUnderCapDoesNotFire verifies that a tool
// running UNDER the cap does not trip the backstop, and that after the
// tool finishes the watchdog still fires normally on idle.
func TestStreamWatchdog_ToolPauseUnderCapDoesNotFire(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 60 * time.Millisecond
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 5 * time.Second // generous — well above the tool runtime

	var fired atomic.Int32
	var firedToolTimeout atomic.Bool
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(_ time.Duration, toolTimeout bool) {
		fired.Add(1)
		firedToolTimeout.Store(toolTimeout)
	}, false, 0, toolMaxDuration)
	defer func() {
		cancel()
		<-wd.done
	}()

	// Tool runs for a few idle periods — well under the cap. The
	// watchdog must NOT fire.
	wd.toolStarted(false)
	time.Sleep(idle * 3)
	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire while a tool runs under the cap")
	assert.False(t, wd.stalled.Load())
	assert.NoError(t, ctx.Err())

	// Tool finishes; with no further activity the watchdog resumes and
	// must fire on idle afterwards (toolTimeout==false).
	wd.toolFinished(false)
	select {
	case <-wd.done:
	case <-time.After(idle + 300*time.Millisecond):
		t.Fatal("watchdog must fire on idle after the tool finished")
	}
	assert.Equal(t, int32(1), fired.Load())
	assert.False(t, firedToolTimeout.Load(), "the post-tool fire must be an idle fire, not a tool timeout")
	assert.True(t, wd.stalled.Load())
}

// TestStreamWatchdog_ExemptToolNotBoundedByCap is the regression test for a
// real, reproducible bug: a sub-agent delegation via the `agent` tool was
// force-cancelled by the PARENT's own stream watchdog with a generic
// "context canceled" error while the sub-agent was still productively
// working — the same toolMaxDuration cap meant for a single primitive tool
// (bash, edit, ...) was also being applied to the parent's wait on an
// entire nested sub-agent conversation, which routinely takes far longer
// than that cap for non-trivial work. The sub-agent's OWN Run() call
// already runs under its own stream watchdog, so a SECOND, tighter bound on
// the parent's side is both redundant and actively harmful. This proves an
// exempt tool (toolStarted(true)) is never killed by toolMaxDuration, no
// matter how long it stays in flight.
func TestStreamWatchdog_ExemptToolNotBoundedByCap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 60 * time.Millisecond // idle path must NOT fire either
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 60 * time.Millisecond // tiny — would fire almost immediately for a non-exempt tool

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, bool) {
		fired.Add(1)
	}, false, 0, toolMaxDuration)

	// An exempt (sub-agent delegation) tool starts and runs WAY past
	// toolMaxDuration with zero provider activity — the watchdog must NOT
	// fire, unlike TestStreamWatchdog_ToolPauseBoundedByCap's non-exempt case.
	wd.toolStarted(true)
	time.Sleep(toolMaxDuration * 5)
	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire for an exempt tool even well past toolMaxDuration")
	assert.False(t, wd.stalled.Load())
	assert.NoError(t, ctx.Err())

	// Once the exempt tool finishes, the watchdog must resume normal idle
	// behavior (proving exemption is scoped to while the tool is in flight,
	// not a permanent disable).
	wd.toolFinished(true)
	select {
	case <-wd.done:
	case <-time.After(idle + 300*time.Millisecond):
		t.Fatal("watchdog must resume firing on idle once the exempt tool finishes")
	}
	assert.Equal(t, int32(1), fired.Load())
	assert.True(t, wd.stalled.Load())
}

// TestStreamWatchdog_ExemptToolInSameBatchExemptsNonExemptToo verifies the
// batch-level semantics documented on exemptToolsInFlight: when an exempt
// tool (sub-agent delegation) is part of the SAME in-flight batch as a
// regular tool (parallel tool calls), the whole batch is exempted from
// toolMaxDuration — not just the exempt one. This is a deliberate,
// documented simplification (the watchdog tracks one shared toolStartedAt
// for the whole batch, not a per-tool timer), and errs toward not killing a
// batch that includes a legitimate long-running delegation.
func TestStreamWatchdog_ExemptToolInSameBatchExemptsNonExemptToo(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 60 * time.Millisecond
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 60 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, bool) {
		fired.Add(1)
	}, false, 0, toolMaxDuration)

	// Two parallel tool calls: one regular (bash-shaped), one exempt
	// (agent-shaped). Both must be protected for as long as the exempt one
	// is still in flight.
	wd.toolStarted(false)
	wd.toolStarted(true)
	time.Sleep(toolMaxDuration * 5)
	assert.Equal(t, int32(0), fired.Load(),
		"the whole batch must be exempt while any exempt tool is still in flight")

	// The regular tool finishes first; the exempt one is still running —
	// the batch must remain exempt.
	wd.toolFinished(false)
	time.Sleep(toolMaxDuration * 5)
	assert.Equal(t, int32(0), fired.Load(),
		"batch must stay exempt as long as the exempt tool is still in flight")
	assert.False(t, wd.stalled.Load())

	// The exempt tool finishes too; the watchdog must resume normal idle
	// behavior.
	wd.toolFinished(true)
	select {
	case <-wd.done:
	case <-time.After(idle + 300*time.Millisecond):
		t.Fatal("watchdog must resume firing on idle once all tools finish")
	}
	assert.Equal(t, int32(1), fired.Load())
}
