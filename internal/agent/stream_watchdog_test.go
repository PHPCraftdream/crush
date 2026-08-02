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
	}, false, 0, 0, 0)
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
	}, false, 0, 0, 0)

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
	}, false, 0, 0, 0)

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

	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, bool) {}, false, 0, 0, 0)

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
	}, false, 0, 0, 0)
	// A tool starts and runs WAY past idleTimeout with zero provider
	// activity — the watchdog must NOT fire.
	wd.toolStarted()
	time.Sleep(idle * 4)
	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire while a tool is executing, even past idleTimeout")
	assert.False(t, wd.stalled.Load())
	assert.NoError(t, ctx.Err())

	// Tool finishes; with no further activity the watchdog resumes and must
	// fire after the idle window.
	wd.toolFinished()
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
	}, false, 0, 0, 0)
	// Two parallel tool calls in flight; finishing ONE must keep the
	// watchdog paused (counter still > 0).
	wd.toolStarted()
	wd.toolStarted()
	wd.toolFinished()
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
	}, true, hardCap, 0, 0)
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
	}, true, hardCap, 0, 0)

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
	}, true, hardCap, 0, 0)

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

// TestStreamWatchdog_HardCapRespectedWithoutExtendsOnProgress is the
// regression test for a real bug: the hardCap check used to live ONLY
// inside the `if extendsOnProgress` branch of the idle-path check, so when
// extendsOnProgress is false (the default — operator did not pass
// --timeout-extends-on-progress) an explicitly configured --timeout-hard-cap
// was never enforced as long as the provider kept the stream alive with
// regular chunks: idle never reached idleTimeout, and the hardCap check was
// unreachable on that path. This proves the fix: even with extendsOnProgress
// false and bump() called continuously (idle never approaches idleTimeout),
// the watchdog must still fire at the hard cap.
func TestStreamWatchdog_HardCapRespectedWithoutExtendsOnProgress(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())

	const idle = 80 * time.Millisecond
	const tick = 10 * time.Millisecond
	const hardCap = 200 * time.Millisecond

	var fired atomic.Int32
	var firedToolTimeout atomic.Bool
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(_ time.Duration, toolTimeout bool) {
		fired.Add(1)
		firedToolTimeout.Store(toolTimeout)
	}, false, hardCap, 0, 0)

	start := time.Now()

	// Bump rapidly — more often than idleTimeout — so the idle-only check
	// would NEVER fire on its own. The hard cap must still kill it.
	stop := time.After(500 * time.Millisecond)
loop:
	for {
		select {
		case <-wd.done:
			break loop
		case <-stop:
			t.Fatal("watchdog should have fired at hard cap despite continuous activity")
		case <-time.After(10 * time.Millisecond):
			wd.bump()
		}
	}

	elapsed := time.Since(start)
	assert.Equal(t, int32(1), fired.Load(), "watchdog must fire at hard cap")
	assert.True(t, wd.stalled.Load())
	assert.False(t, firedToolTimeout.Load(),
		"the hard-cap fire outside tool-in-flight must report toolTimeout=false")
	// The hard cap is 200ms with a tick of 10ms, so it should fire
	// somewhere between 200-250ms.
	assert.LessOrEqual(t, elapsed, 350*time.Millisecond,
		"watchdog must fire near the hard cap")
}

// TestStreamWatchdog_ToolPauseBoundedByCap verifies the never-freeze
// backstop: when toolMaxDuration > 0 and a tool stays in flight past that
// cap, the watchdog fires with toolTimeout==true instead of pausing
// forever. This is what keeps a stuck tool (hung MCP tool, blocking
// job_output --wait, or a sub-agent delegation via the "agent" tool that
// never returns) from freezing the whole agent turn. One cap applies to
// every tool — there is no separate, larger cap for delegations anymore
// (see toolExecutionMaxDefault's doc in agent.go for why that split was
// removed), so this test doubles as coverage for the delegation case too:
// toolStarted/toolFinished no longer take any argument distinguishing tool
// kinds, so there is no way to even express a different cap at the call
// site.
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
	}, false, 0, toolMaxDuration, 0)

	// A tool starts and runs past toolMaxDuration with zero provider
	// activity. The watchdog must fire with toolTimeout==true.
	wd.toolStarted()
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
	}, false, 0, toolMaxDuration, 0)
	defer func() {
		cancel()
		<-wd.done
	}()

	// Tool runs for a few idle periods — well under the cap. The
	// watchdog must NOT fire.
	wd.toolStarted()
	time.Sleep(idle * 3)
	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire while a tool runs under the cap")
	assert.False(t, wd.stalled.Load())
	assert.NoError(t, ctx.Err())

	// Tool finishes; with no further activity the watchdog resumes and
	// must fire on idle afterwards (toolTimeout==false).
	wd.toolFinished()
	select {
	case <-wd.done:
	case <-time.After(idle + 300*time.Millisecond):
		t.Fatal("watchdog must fire on idle after the tool finished")
	}
	assert.Equal(t, int32(1), fired.Load())
	assert.False(t, firedToolTimeout.Load(), "the post-tool fire must be an idle fire, not a tool timeout")
	assert.True(t, wd.stalled.Load())
}

// TestStreamWatchdog_SequentialBatchProgressResetsCapClock is the regression
// test for a real bug found while live-testing the never-freeze backstop: a
// sub-agent running FOUR individually-fast bash steps (well under the
// configured cap) was force-cancelled anyway, because fantasy fires every
// OnToolCall for a step BEFORE executing any of the tools in it — so a
// "batch" the model issued as several back-to-back tool calls (common for
// faster/smaller models even when explicitly asked to go one at a time) is
// indistinguishable, from the watchdog's counter alone, from true parallel
// execution. Before this fix, toolStartedAt was set once when the FIRST
// tool of the batch started and only reset to 0 once ALL of them had
// finished — so toolMaxDuration bounded the batch's CUMULATIVE wall time,
// not any single tool's runtime, and several fast sequential steps could
// sum past the cap and get killed even though none was ever stuck.
//
// This proves: with toolMaxDuration = 60ms, four tools started together
// (simulating fantasy's upfront OnToolCall batch) finish one at a time
// ~30ms apart (each individual gap safely under the cap, but the batch's
// total span, ~120ms, is well past it) — the watchdog must NOT fire, since
// every gap between consecutive finishes resets the clock.
func TestStreamWatchdog_SequentialBatchProgressResetsCapClock(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 5 * time.Second // large — idle path must not confound this test
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 60 * time.Millisecond
	const stepGap = 30 * time.Millisecond // < toolMaxDuration; 4 steps sum to ~120ms > toolMaxDuration

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, bool) {
		fired.Add(1)
	}, false, 0, toolMaxDuration, 0)

	// fantasy fires OnToolCall for every tool in the step before executing
	// any of them — simulate that: all four "start" near-simultaneously.
	wd.toolStarted()
	wd.toolStarted()
	wd.toolStarted()
	wd.toolStarted()

	// They finish one at a time, ~30ms apart — each gap is safely under the
	// 60ms cap, but the cumulative batch span (~120ms) is not.
	for i := 0; i < 4; i++ {
		time.Sleep(stepGap)
		wd.toolFinished()
	}

	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire for a sequential batch whose individual step gaps stay under the cap, even though the cumulative span exceeds it")
	assert.False(t, wd.stalled.Load())
	assert.NoError(t, ctx.Err())

	// A genuinely stuck tool must still be caught: after the batch above
	// fully finishes (toolsInFlight back to 0), start one more tool and let
	// it run well past the cap with no further progress — this must fire.
	wd.toolStarted()
	select {
	case <-wd.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog must still fire for a genuinely stuck tool after a healthy sequential batch")
	}
	assert.Equal(t, int32(1), fired.Load())
	assert.True(t, wd.stalled.Load())
}

// TestStreamWatchdog_ToolCleanupGraceDelaysFire is the regression test for
// the parent/child cancellation race: an `agent`-tool delegation runs a
// nested Run()/runTurn() with its OWN stream watchdog, started strictly
// LATER than the parent's (the parent starts timing from OnToolCall, before
// the child's turn has even begun executing). Both share the same
// toolMaxDuration, so without a grace buffer the parent's head start means
// it always reaches the cap first and force-cancels the whole delegation
// before the child's own watchdog gets a chance to fire on ITS cap and
// unwind cleanly. This test proves the fix at the primitive level: with
// toolCleanupGrace > 0, the watchdog must NOT fire at toolMaxDuration — it
// must only fire once toolMaxDuration+toolCleanupGrace has elapsed.
func TestStreamWatchdog_ToolCleanupGraceDelaysFire(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 5 * time.Second // large — idle path must not fire
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 60 * time.Millisecond
	const toolCleanupGrace = 120 * time.Millisecond

	var fired atomic.Int32
	var firedToolTimeout atomic.Bool
	var firedElapsed atomic.Int64
	start := time.Now()
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(elapsed time.Duration, toolTimeout bool) {
		fired.Add(1)
		firedToolTimeout.Store(toolTimeout)
		firedElapsed.Store(int64(elapsed))
	}, false, 0, toolMaxDuration, toolCleanupGrace)

	wd.toolStarted()

	// Wait until just past toolMaxDuration alone (well before the grace
	// window closes) — the watchdog must NOT have fired yet.
	time.Sleep(toolMaxDuration + 20*time.Millisecond)
	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire at bare toolMaxDuration when toolCleanupGrace > 0 — "+
			"the grace exists precisely so a nested child watchdog gets a chance to fire first")
	assert.False(t, wd.stalled.Load())
	assert.NoError(t, ctx.Err())

	// Now wait for the fire past toolMaxDuration+toolCleanupGrace.
	select {
	case <-wd.done:
	case <-time.After(time.Second):
		t.Fatal("watchdog never fired after toolMaxDuration+toolCleanupGrace")
	}
	elapsedWall := time.Since(start)

	assert.Equal(t, int32(1), fired.Load(), "onFire should fire exactly once")
	assert.True(t, firedToolTimeout.Load(), "toolTimeout must be true when the cap is exceeded")
	assert.True(t, wd.stalled.Load())
	assert.Error(t, ctx.Err())
	assert.GreaterOrEqual(t, time.Duration(firedElapsed.Load()), toolMaxDuration+toolCleanupGrace,
		"elapsed passed to onFire must be >= toolMaxDuration+toolCleanupGrace")
	assert.GreaterOrEqual(t, elapsedWall, toolMaxDuration+toolCleanupGrace,
		"wall-clock time to fire must respect the full grace window")
}

// TestStreamWatchdog_ToolFinishesWithinGraceNeverFires verifies the healthy
// counterpart of the race: if the tool (e.g. a sub-agent delegation)
// finishes on its own — reacting to ITS OWN toolMaxDuration and unwinding
// cleanly — at some point AFTER the parent's bare toolMaxDuration but BEFORE
// toolMaxDuration+toolCleanupGrace elapses, the parent watchdog must never
// fire at all. This is the scenario toolCleanupGrace exists to make
// possible: the child wins the race to finish before the parent's grace
// period runs out.
func TestStreamWatchdog_ToolFinishesWithinGraceNeverFires(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 5 * time.Second // large — idle path must not fire
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 60 * time.Millisecond
	const toolCleanupGrace = 150 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, bool) {
		fired.Add(1)
	}, false, 0, toolMaxDuration, toolCleanupGrace)
	defer func() {
		cancel()
		<-wd.done
	}()

	wd.toolStarted()

	// Simulate the child unwinding cleanly at a point past bare
	// toolMaxDuration but comfortably inside the grace window.
	time.Sleep(toolMaxDuration + 40*time.Millisecond)
	wd.toolFinished()

	// Give the watchdog a couple more ticks to prove it did NOT fire despite
	// having crossed bare toolMaxDuration while the tool was in flight.
	time.Sleep(3 * tick)
	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire when the tool finishes within the grace window, "+
			"even though it ran past bare toolMaxDuration")
	assert.False(t, wd.stalled.Load())
	assert.NoError(t, ctx.Err())
}
