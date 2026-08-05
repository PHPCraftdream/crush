package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	crushlog "github.com/charmbracelet/crush/internal/log"
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
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, false, 0, 0, 0, nil)
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
	var firedCause atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(observedIdle time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedIdle.Store(int64(observedIdle))
		firedCause.Store(int32(cause))
	}, false, 0, 0, 0, nil)

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
	assert.Equal(t, causeIdleStall, watchdogCause(firedCause.Load()),
		"a genuine provider idle-stall must report causeIdleStall")
}

func TestStreamWatchdog_ExitsCleanlyOnCtxCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	const idle = 5 * time.Second // very long — must not fire
	const tick = 10 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, false, 0, 0, 0, nil)

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

	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {}, false, 0, 0, 0, nil)

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
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, false, 0, 0, 0, nil)
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
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, false, 0, 0, 0, nil)
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
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, true, hardCap, 0, 0, nil)
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
	var firedCause atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(_ time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
	}, true, hardCap, 0, 0, nil)

	// Bump once to extend, then stop.
	wd.bump()

	select {
	case <-wd.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog should have fired after progress stopped")
	}

	assert.Equal(t, int32(1), fired.Load(), "watchdog must fire when progress stops")
	assert.True(t, wd.stalled.Load())
	assert.Equal(t, causeIdleStall, watchdogCause(firedCause.Load()),
		"progress stopping (not a hard cap, not a stuck tool) must report causeIdleStall")
}

// TestStreamWatchdog_HardCapRespected verifies that even with continuous
// progress, the watchdog fires at the hard cap.
func TestStreamWatchdog_HardCapRespected(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())

	const idle = 200 * time.Millisecond
	const tick = 10 * time.Millisecond
	const hardCap = 400 * time.Millisecond

	var fired atomic.Int32
	var firedCause atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(_ time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
	}, true, hardCap, 0, 0, nil)

	start := time.Now()

	// Bump rapidly — but hard cap should still kill it.
	stop := time.After(2 * time.Second)
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
	assert.Equal(t, causeHardCap, watchdogCause(firedCause.Load()),
		"firing at the hard cap despite continuous progress must report causeHardCap")
	// The hard cap is 400ms; widened from an earlier 200ms-cap/350ms-ceiling
	// version (task #320) whose 150ms of upper-bound slack was tight enough
	// to flake on a loaded CI runner under -race in a full-package parallel
	// run, where a single delayed tick is enough to blow the margin without
	// indicating any real bug. 800ms of slack on top of the cap only fails
	// if the fire is genuinely late, not merely jittered.
	assert.LessOrEqual(t, elapsed, hardCap+800*time.Millisecond,
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

	const idle = 200 * time.Millisecond
	const tick = 10 * time.Millisecond
	const hardCap = 400 * time.Millisecond

	var fired atomic.Int32
	var firedCause atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(_ time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
	}, false, hardCap, 0, 0, nil)

	start := time.Now()

	// Bump rapidly — more often than idleTimeout — so the idle-only check
	// would NEVER fire on its own. The hard cap must still kill it.
	stop := time.After(2 * time.Second)
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
	assert.Equal(t, causeHardCap, watchdogCause(firedCause.Load()),
		"the hard-cap fire outside tool-in-flight must report causeHardCap, not causeIdleStall or causeToolTimeout")
	// The hard cap is 400ms; widened from an earlier 200ms-cap/350ms-ceiling
	// version (task #320) for the same reason as TestStreamWatchdog_HardCapRespected:
	// 150ms of upper-bound slack was tight enough to flake on a loaded CI
	// runner under -race in a full-package parallel run.
	assert.LessOrEqual(t, elapsed, hardCap+800*time.Millisecond,
		"watchdog must fire near the hard cap")
}

// TestStreamWatchdog_HardCapRespectedWithToolInFlight is the regression test
// for a real bug: the explicit hardCap check inside the toolsInFlight branch
// (the one immediately below the never-freeze toolMaxDuration backstop) used
// to unconditionally report toolTimeout=true to onFire, even though it is
// the SAME wall-clock --timeout-hard-cap check as the one on the idle path.
// 77c1104a fixed the immediate boolean misclassification (both paths now
// agree on toolTimeout=false), but that only made the hard-cap-with-tool
// case indistinguishable from a genuine provider idle-stall instead of from
// a stuck tool — either way onFire couldn't tell "the turn hit its
// configured ceiling" apart from "the provider went silent". This test
// (task #223) proves the fully-fixed three-way signature: with a small
// hardCap and a generous toolMaxDuration (so the never-freeze backstop does
// not fire first), the watchdog must still fire at the hard cap while a
// tool is in flight, and must report the DISTINCT causeHardCap value — not
// causeToolTimeout (that would blame the tool) and not causeIdleStall (that
// would blame the provider for silence it didn't cause).
func TestStreamWatchdog_HardCapRespectedWithToolInFlight(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 5 * time.Second // large — idle path must not be relevant
	const tick = 10 * time.Millisecond
	const hardCap = 200 * time.Millisecond
	const toolMaxDuration = 5 * time.Second // generous — must not fire before hardCap

	var fired atomic.Int32
	var firedCause atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(_ time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
	}, false, hardCap, toolMaxDuration, 0, nil)

	// A tool starts and stays in flight — never finishes — while the hard
	// cap elapses. The watchdog must fire purely from hardCap, not from the
	// toolMaxDuration backstop.
	wd.toolStarted()

	select {
	case <-wd.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog should have fired at hard cap despite a tool being in flight")
	}

	assert.Equal(t, int32(1), fired.Load(), "watchdog must fire at hard cap")
	assert.True(t, wd.stalled.Load())
	assert.Equal(t, causeHardCap, watchdogCause(firedCause.Load()),
		"the hard-cap fire while a tool is in flight must report causeHardCap — this is a wall-clock turn limit, not a stuck tool and not provider idle")
}

// TestStreamWatchdog_HardCapWhileToolInFlightDistinctFromIdleStall proves
// the two previously-indistinguishable "not a tool timeout" cases — a
// hard-cap fire that happens to catch a tool in flight, and a genuine
// provider idle-stall with no tool in flight — now report DIFFERENT
// watchdogCause values from the same onFire callback shape. Before task
// #223 both cases passed toolTimeout=false and were fully indistinguishable
// to the caller; this is the direct regression test for that gap.
func TestStreamWatchdog_HardCapWhileToolInFlightDistinctFromIdleStall(t *testing.T) {
	t.Parallel()

	const tick = 10 * time.Millisecond

	// Case 1: hard cap fires while a tool is in flight.
	hardCapCtx, hardCapCancel := context.WithCancel(t.Context())
	defer hardCapCancel()
	const hardCap = 150 * time.Millisecond
	const toolMaxDuration = 5 * time.Second // generous — never-freeze backstop must not preempt hardCap
	var hardCapCause atomic.Int32
	hardCapWd := startStreamWatchdog(hardCapCtx, hardCapCancel, 5*time.Second, tick,
		func(_ time.Duration, cause watchdogCause) {
			hardCapCause.Store(int32(cause))
		}, false, hardCap, toolMaxDuration, 0, nil)
	hardCapWd.toolStarted()

	// Case 2: genuine idle stall, no tool ever in flight, no hard cap
	// configured.
	idleCtx, idleCancel := context.WithCancel(t.Context())
	defer idleCancel()
	const idleTimeout = 60 * time.Millisecond
	var idleCause atomic.Int32
	idleWd := startStreamWatchdog(idleCtx, idleCancel, idleTimeout, tick,
		func(_ time.Duration, cause watchdogCause) {
			idleCause.Store(int32(cause))
		}, false, 0, 0, 0, nil)

	select {
	case <-hardCapWd.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("hard-cap watchdog never fired")
	}
	select {
	case <-idleWd.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle watchdog never fired")
	}

	gotHardCap := watchdogCause(hardCapCause.Load())
	gotIdle := watchdogCause(idleCause.Load())

	assert.Equal(t, causeHardCap, gotHardCap, "hard-cap-with-tool-in-flight must report causeHardCap")
	assert.Equal(t, causeIdleStall, gotIdle, "genuine provider idle-stall must report causeIdleStall")
	assert.NotEqual(t, gotHardCap, gotIdle,
		"a hard-cap fire while a tool is in flight and a genuine idle-stall must be distinguishable, not collapse to the same cause")
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
	var firedCause atomic.Int32
	var firedElapsed atomic.Int64
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(elapsed time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
		firedElapsed.Store(int64(elapsed))
	}, false, 0, toolMaxDuration, 0, nil)

	// A tool starts and runs past toolMaxDuration with zero provider
	// activity. The watchdog must fire with cause=causeToolTimeout.
	wd.toolStarted()
	select {
	case <-wd.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog never fired after toolMaxDuration")
	}

	assert.Equal(t, int32(1), fired.Load(), "onFire should fire exactly once")
	assert.Equal(t, causeToolTimeout, watchdogCause(firedCause.Load()), "cause must be causeToolTimeout when the cap is exceeded")
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
	var firedCause atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(_ time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
	}, false, 0, toolMaxDuration, 0, nil)
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
	// must fire on idle afterwards (cause==causeIdleStall).
	wd.toolFinished()
	select {
	case <-wd.done:
	case <-time.After(idle + 300*time.Millisecond):
		t.Fatal("watchdog must fire on idle after the tool finished")
	}
	assert.Equal(t, int32(1), fired.Load())
	assert.Equal(t, causeIdleStall, watchdogCause(firedCause.Load()), "the post-tool fire must be an idle fire, not a tool timeout")
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
	const toolMaxDuration = 600 * time.Millisecond
	// stepGap < toolMaxDuration; 4 steps sum to ~1200ms > toolMaxDuration.
	// Scaled 10x up from an earlier 30ms/60ms version (task #320): the
	// invariant this test needs is "each individual gap stays under the
	// cap", and that margin was only 30ms wide — well inside typical
	// scheduler jitter for a goroutine under -race in a full-package
	// parallel run, where a delayed time.Sleep wakeup could push a single
	// gap over the cap and flake the test on pure scheduling luck, not a
	// real bug. Same 2x ratio, ten times the absolute margin.
	const stepGap = 300 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, false, 0, toolMaxDuration, 0, nil)

	// fantasy fires OnToolCall for every tool in the step before executing
	// any of them — simulate that: all four "start" near-simultaneously.
	wd.toolStarted()
	wd.toolStarted()
	wd.toolStarted()
	wd.toolStarted()

	// They finish one at a time, ~300ms apart — each gap is safely under
	// the 600ms cap, but the cumulative batch span (~1200ms) is not.
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
	case <-time.After(toolMaxDuration + time.Second):
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
	var firedCause atomic.Int32
	var firedElapsed atomic.Int64
	start := time.Now()
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(elapsed time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
		firedElapsed.Store(int64(elapsed))
	}, false, 0, toolMaxDuration, toolCleanupGrace, nil)

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
	assert.Equal(t, causeToolTimeout, watchdogCause(firedCause.Load()), "cause must be causeToolTimeout when the cap is exceeded")
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
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, false, 0, toolMaxDuration, toolCleanupGrace, nil)
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

// TestStreamWatchdog_NoTimerDrivenActivityWhileToolInFlight is the
// regression test for task #300, and deliberately asserts the OPPOSITE of
// what task #222's test used to.
//
// #222 made the watchdog call recordActivity once per tick for as long as
// any tool was in flight, so a long synchronous tool (which produces no
// stream callbacks) would not starve SessionLock's activity-gated
// heartbeat. The cost was that the heartbeat then reported "alive" purely
// because a tool call was OPEN: a wedged tool looked exactly like a
// working one for its entire cap. That was observed in the wild as a
// session showing a fresh heartbeat for 38 minutes while its sub-agent was
// stuck on a trivial command, with sessions locks/why/list all calling it
// healthy.
//
// The heartbeat must now reflect only REAL progress. #222's original
// concern is covered without a timer: withActivityNotify (agent.go)
// composes each session's activity callback with its ancestors', so a
// delegated sub-agent's genuine stream callbacks walk back up and touch
// every ancestor session's lock. Nothing else may synthesise activity.
func TestStreamWatchdog_NoTimerDrivenActivityWhileToolInFlight(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 5 * time.Second // large — the idle path must not confound this
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 5 * time.Second // generous — the tool must not be cut off

	var recordCalls atomic.Int32
	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, false, 0, toolMaxDuration, 0, func() {
		recordCalls.Add(1)
	})
	defer func() {
		cancel()
		<-wd.done
	}()

	// A tool stays in flight for many ticks while producing NOTHING — the
	// exact shape of a hung tool call.
	wd.toolStarted()
	time.Sleep(15 * tick)

	assert.Equal(t, int32(0), fired.Load(), "watchdog must not fire while the tool is under its cap")
	assert.Equal(t, int32(0), recordCalls.Load(),
		"a tool merely being OPEN must never synthesise activity: the timer must not touch the heartbeat, "+
			"otherwise a wedged tool is indistinguishable from a working one for its whole cap (task #300)")

	// A real stream callback — genuine progress — still records activity.
	wd.bump()
	assert.Greater(t, recordCalls.Load(), int32(0),
		"real progress (a stream callback) must still record activity — this is the ONLY thing that may")

	wd.toolFinished()
}

// TestStreamWatchdog_OnFireCompletesBeforeCancel is the regression test for
// task #227: startStreamWatchdog's doc comment says onFire is invoked AFTER
// stalled is set to true and BEFORE cancel(), but until this fix all 5 fire
// sites actually did `stalled.Store(true); cancel(); onFire(...)` — the
// OPPOSITE order. That let a second goroutine blocked on <-ctx.Done() race
// ahead and observe cancellation before onFire (which, in agent.go, stores
// the real watchdogCause AFTER a slow disk-writing goroutine dump) had
// finished recording anything — silently defeating task #223's cause
// attribution on a genuine hard-cap/tool-timeout fire.
//
// This proves the fix deterministically (no time.Sleep guessing): onFire
// blocks on a channel until the test goroutine has confirmed ctx is NOT YET
// Done, then signals onFire to proceed and records the moment it returns.
// The test asserts, via a channel handshake instead of timing, that
// ctx.Done() cannot be observed as closed until AFTER onFire has fully
// returned.
func TestStreamWatchdog_OnFireCompletesBeforeCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 20 * time.Millisecond
	const tick = 5 * time.Millisecond

	// onFireEntered is closed the instant onFire starts running.
	onFireEntered := make(chan struct{})
	// releaseOnFire is closed by the test goroutine once it has confirmed
	// ctx is still NOT Done — only then is onFire allowed to return.
	releaseOnFire := make(chan struct{})
	// onFireReturned is closed right after onFire returns, i.e. right
	// before the watchdog goroutine calls cancel().
	onFireReturned := make(chan struct{})

	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		close(onFireEntered)
		<-releaseOnFire
		close(onFireReturned)
	}, false, 0, 0, 0, nil)

	// Wait for onFire to start.
	select {
	case <-onFireEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("onFire was never invoked")
	}

	// While onFire is blocked (has not returned), ctx must NOT be Done yet
	// — cancel() must not have run. This is the deterministic core of the
	// assertion: sampled precisely while onFire is known to still be
	// in-flight, not via a timing guess.
	select {
	case <-ctx.Done():
		t.Fatal("ctx was cancelled before onFire returned — cancel() must run strictly after onFire completes")
	default:
		// Good: ctx is still live while onFire is blocked.
	}
	assert.NoError(t, ctx.Err(), "ctx must not be cancelled while onFire is still running")

	// Now let onFire finish, and confirm ctx becomes Done only afterward.
	close(releaseOnFire)

	select {
	case <-onFireReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("onFire never returned after being released")
	}

	// ctx.Done() must become observable (cancel() called) after onFire
	// returned. Poll done deterministically via the watchdog's own done
	// channel, which only closes after the fire-site's cancel()+return.
	select {
	case <-wd.done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog goroutine never exited after onFire returned")
	}
	assert.Error(t, ctx.Err(), "ctx must be cancelled after onFire returned")
	assert.True(t, wd.stalled.Load())
}

// TestStreamWatchdog_CauseStoredBeforeSlowDiagnosticWork is the regression
// test for task #232 issue 1. It mirrors agent.go's actual onFire closure
// shape: store the fire cause FIRST, synchronously, then do the (potentially
// slow) diagnostic work. Because task #227 made onFire run strictly before
// cancel(), an external cancellation of ctx (independent of this watchdog —
// e.g. user Ctrl-C, a --timeout racing the same instant) could previously be
// observed by another goroutine while the cause store hadn't happened yet
// if the cause store came after the slow work, misattributing the fire.
//
// This test proves the ordering deterministically (no time.Sleep guessing):
// the simulated diagnostic work blocks on a channel, and the test asserts
// the cause is already readable as the real fired cause WHILE that
// diagnostic work is still blocked — not just after onFire eventually
// returns.
func TestStreamWatchdog_CauseStoredBeforeSlowDiagnosticWork(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 5 * time.Second
	const tick = 5 * time.Millisecond

	var causeVal atomic.Int32
	causeVal.Store(int32(causeIdleStall)) // zero value; must be overwritten by the real cause

	diagnosticWorkEntered := make(chan struct{})
	releaseDiagnosticWork := make(chan struct{})

	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(_ time.Duration, cause watchdogCause) {
		// Mirrors agent.go's onFire: store the cause FIRST, synchronously,
		// before any slow diagnostic work.
		causeVal.Store(int32(cause))
		close(diagnosticWorkEntered)
		<-releaseDiagnosticWork
	},
		false, 0,
		1*time.Millisecond, // toolMaxDuration: tiny, so a tool-in-flight fires with causeToolTimeout
		0, nil)

	// Put a tool in flight so the watchdog fires with causeToolTimeout, not
	// causeIdleStall — this makes the assertion meaningful: if the store
	// were still reachable only after the slow work, we'd see the zero
	// value (causeIdleStall) instead.
	wd.toolStarted()

	select {
	case <-diagnosticWorkEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("onFire's diagnostic work was never entered")
	}

	// While the diagnostic work is still blocked, the cause must already be
	// the real fired cause (causeToolTimeout), not the zero value.
	assert.Equal(t, int32(causeToolTimeout), causeVal.Load(),
		"cause must be stored before the slow diagnostic work runs, so a concurrent reader "+
			"never observes the zero value for a real tool-timeout/hard-cap fire")

	close(releaseDiagnosticWork)
	select {
	case <-wd.done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog goroutine never exited")
	}
}

// TestStreamWatchdog_AsyncDiagnosticWorkDoesNotDelayCancel is the regression
// test for task #232 issue 2. Since task #227, onFire runs strictly before
// cancel() on every fire path — so a slow or hung disk write inside onFire
// (DumpGoroutines in production, see internal/log/goroutine_dump.go, has no
// timeout of its own) would previously delay cancel() by however long the
// write takes, or block it forever if the write hangs. agent.go's fix
// dispatches the diagnostic work in its own goroutine and returns from
// onFire immediately, without awaiting it.
//
// This test proves that pattern deterministically: onFire launches a
// goroutine that blocks on a channel the test never closes (simulating a
// permanently hung write) and returns immediately. The watchdog's own
// cancel()+done must complete promptly regardless — proving the hang can
// never delay cancellation, not just that it usually doesn't.
func TestStreamWatchdog_AsyncDiagnosticWorkDoesNotDelayCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 20 * time.Millisecond
	const tick = 5 * time.Millisecond

	// neverReleased is intentionally never closed, simulating a diagnostic
	// write that hangs forever (e.g. a stuck network/SMB mount).
	neverReleased := make(chan struct{})
	diagnosticStarted := make(chan struct{})
	t.Cleanup(func() {
		// Let the leaked goroutine exit at test end rather than leaking past
		// this test (it would otherwise block until process exit).
		close(neverReleased)
	})

	onFireReturned := make(chan struct{})
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		go func() {
			close(diagnosticStarted)
			<-neverReleased // never closed by the test — simulates a hung write
		}()
		close(onFireReturned)
	}, false, 0, 0, 0, nil)

	// onFire must return promptly even though the diagnostic goroutine it
	// launched will never finish.
	select {
	case <-onFireReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("onFire did not return — async diagnostic work must not be awaited")
	}
	select {
	case <-diagnosticStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the diagnostic goroutine was never scheduled")
	}

	// cancel()/done must complete promptly — NOT blocked on the permanently
	// hung diagnostic goroutine.
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("ctx was never cancelled — a hung async diagnostic write must not block cancel()")
	}
	select {
	case <-wd.done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog goroutine never exited — a hung async diagnostic write must not block it")
	}
	assert.True(t, wd.stalled.Load())
}

// TestSessionAgent_HandleWatchdogFire_StoresCauseAndDispatchesDumpAsync is
// the regression test for task #243. TestStreamWatchdog_CauseStoredBeforeSlowDiagnosticWork
// and TestStreamWatchdog_AsyncDiagnosticWorkDoesNotDelayCancel (task #232,
// above) only prove that a TEST-LOCAL COPY of onFire's shape behaves as
// documented — neither one builds a *sessionAgent or calls the real
// production method, so reverting agent.go's actual ordering left both of
// those tests green (they test the copy, not the original). This test closes
// that gap by calling sessionAgent.handleWatchdogFire directly — the exact
// method runTurn's onFire closure dispatches to (see agent.go) — with no
// watchdog, no turn, no VCR involved.
//
// The write is the REAL crushlog.WriteGoroutineDump (not a fake), so to make
// "the write is dispatched async and does not delay the return" a
// deterministic, non-flaky assertion rather than a timing guess against an
// ordinarily near-instant local disk write, this test redirects
// crushlog's dump directory (via the exported crushlog.SetLogDirForTest
// seam) at a deeply nested, not-yet-existing directory. os.MkdirAll walking
// ~300 missing path segments measurably slows the real write (hundreds of ms,
// verified empirically) while leaving the code path completely unmodified —
// giving a wide, reliable margin between "the call returned" and "the slow
// real write finished" without faking any production logic.
//
// It proves, against the REAL method:
//  1. watchdogCauseVal is stored with the real fired cause by the time
//     handleWatchdogFire returns (this is the field runTurn's post-stream
//     error path reads via watchdogCause(watchdogCauseVal.Load()) to decide
//     the user-facing finish message and tool-result text).
//  2. The method returns BEFORE the slow real write has completed — proving
//     the write is genuinely dispatched on its own goroutine, not awaited.
//  3. The write nonetheless completes shortly after and lands real,
//     readable goroutine-stack content on disk.
func TestSessionAgent_HandleWatchdogFire_StoresCauseAndDispatchesDumpAsync(t *testing.T) {
	deepDir := t.TempDir()
	for i := range 300 {
		deepDir = filepath.Join(deepDir, "nested"+strconv.Itoa(i))
	}
	crushlog.SetLogDirForTest(t, deepDir)

	largeModel := Model{
		ModelCfg: config.SelectedModel{
			Provider: "test-provider",
			Model:    "test-model",
		},
	}
	a := &sessionAgent{
		largeModel:     csync.NewValue(largeModel),
		timeoutHardCap: 5 * time.Second,
	}

	var watchdogCauseVal atomic.Int32
	watchdogCauseVal.Store(int32(causeIdleStall)) // zero value; must be overwritten

	const sessionID = "sess-243-test"
	const toolMaxDuration = 5 * time.Second
	const idleTimeout = 3 * time.Minute

	callReturned := make(chan struct{})
	go func() {
		a.handleWatchdogFire(causeToolTimeout, 7*time.Second, sessionID, &watchdogCauseVal, toolMaxDuration, idleTimeout, largeModel)
		close(callReturned)
	}()

	select {
	case <-callReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("handleWatchdogFire did not return — it must not block on the async goroutine-dump write")
	}

	// (1) The real cause must be stored by the time the call returns.
	assert.Equal(t, int32(causeToolTimeout), watchdogCauseVal.Load(),
		"handleWatchdogFire must store the real fired cause, not leave the zero value, "+
			"by the time it returns — this is what runTurn's error path reads to build the finish message")

	pattern := filepath.Join(deepDir, "goroutines-"+strconv.Itoa(os.Getpid())+"-*.txt")

	// (2) The method must have returned BEFORE the deliberately-slowed write
	// could have finished. WriteGoroutineDump's 300-segment os.MkdirAll takes
	// hundreds of ms, so if handleWatchdogFire had awaited the write inline the
	// dump file would already exist here. Checking its absence immediately after
	// return proves the write was dispatched on its own goroutine — directly,
	// without a timing budget that the now-synchronous CaptureGoroutineStack
	// (runtime.Stack over every goroutine plus a grow-and-retry loop; see task
	// #242 / internal/log/goroutine_dump.go) would make both large and variable,
	// scaling with live goroutine count (worse in a full-package run than under
	// an isolated -run filter).
	immediate, _ := filepath.Glob(pattern)
	assert.Empty(t, immediate,
		"handleWatchdogFire must return before the slow dump write completes; "+
			"a file present this early means the write was awaited inline, not dispatched async")

	// (3) The real write must still complete shortly after and land actual
	// goroutine-stack content on disk — proving the async dispatch genuinely
	// happened rather than being silently skipped. The content check lives
	// INSIDE the predicate (not after it) so the poll waits for the file to
	// contain real data, not merely to exist: os.WriteFile creates the file
	// before writing its ~1 MiB body, so a naive existence-only poll can
	// observe a zero-byte file mid-write and pass on empty content.
	//
	// Budget is 30s, not a small fixed guess (task #320): this assertion
	// isn't timing a race, it's waiting out real, variable-cost work — the
	// 300-segment os.MkdirAll this test deliberately constructs, plus
	// runtime.Stack over every live goroutine, plus a ~1 MiB disk write —
	// all under -race, in a full-package parallel run where every other
	// test's goroutines compete for scheduler time (observed failing on a
	// loaded macOS CI runner at the previous 5s budget: CI log showed
	// "Condition never satisfied" with zero indication the write ever
	// failed, only that it hadn't finished in time). 30s only fails if the
	// dispatch is genuinely broken (skipped, deadlocked, or the content
	// assertions are wrong), not merely slow.
	var dumpContent []byte
	var matches []string
	require.Eventually(t, func() bool {
		var err error
		matches, err = filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			return false
		}
		dumpContent, err = os.ReadFile(matches[0])
		if err != nil {
			return false
		}
		return bytes.Contains(dumpContent, []byte("stream watchdog fired")) &&
			bytes.Contains(dumpContent, []byte("goroutine "))
	}, 30*time.Second, 50*time.Millisecond, "expected a goroutine dump with real content to appear matching %s", pattern)
	require.Len(t, matches, 1, "expected exactly one dump file from this call")
	assert.Contains(t, string(dumpContent), "stream watchdog fired",
		"the dump must carry the reason handleWatchdogFire passes to crushlog.CaptureGoroutineStack")
	assert.Contains(t, string(dumpContent), "goroutine ", "the dump must contain real goroutine stacks")
}

// lockedBuffer is a concurrency-safe bytes.Buffer sink for slog.TextHandler,
// needed because handleWatchdogFire dispatches an async goroutine that also
// calls slog.Warn while the test reads the buffer.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSessionAgent_HandleWatchdogFire_UsesPassedLargeModelSnapshot is the
// regression test for task #252. Task #243 (b55d701a) extracted runTurn's
// onFire closure into the named handleWatchdogFire method and claimed the
// extraction was "byte-for-byte the same logic". It was not: the original
// closure CAPTURED largeModel — the runTurn local taken ONCE at turn start
// (agent.go: `largeModel := a.largeModel.Get()`, before the turn loop) — so
// the diagnostic slog.Warn always named the model the turn STARTED with.
// The extraction instead re-read a.largeModel.Get() inside the method, at
// fire time. a.largeModel is MUTABLE during a turn: sessionAgent.SetModels
// is called from coordinator.UpdateModels and the web-UI override path, so a
// model switch mid-turn (provider A hangs, user switches to provider B in
// the web UI, watchdog fires after the switch) would log provider B as the
// one that hung — misattributing the diagnostic. This affects ONLY the
// diagnostic log, not the user-facing finish message, hence LOW severity.
//
// This test proves the fix: it calls handleWatchdogFire with an explicit
// largeModel parameter whose Provider/Model differ from a.largeModel's
// CURRENT value, and asserts the captured diagnostic log names the PASSED
// snapshot, not the live field. Because the diagnostic goes through slog
// (not a return value), the default slog.Logger is temporarily swapped for a
// TextHandler writing to a mutex-guarded buffer.
func TestSessionAgent_HandleWatchdogFire_UsesPassedLargeModelSnapshot(t *testing.T) {
	// Redirect the goroutine-dump dir so the async WriteGoroutineDump inside
	// handleWatchdogFire does not pollute the real log dir; a shallow tempdir
	// also keeps that write near-instant.
	crushlog.SetLogDirForTest(t, t.TempDir())

	// a.largeModel's CURRENT value — what the BUGGY code would read at fire
	// time via a.largeModel.Get().
	a := &sessionAgent{
		largeModel: csync.NewValue(Model{
			ModelCfg: config.SelectedModel{
				Provider: "current-provider",
				Model:    "current-model",
			},
		}),
		timeoutHardCap: 5 * time.Second,
	}

	// The SNAPSHOT runTurn would pass in — taken once at turn start. Simulate
	// a mid-turn model switch by making it differ from a.largeModel's value.
	snapshotModel := Model{
		ModelCfg: config.SelectedModel{
			Provider: "snapshot-provider",
			Model:    "snapshot-model",
		},
	}

	// Capture slog output: swap the default logger for a TextHandler over a
	// mutex-guarded buffer. handleWatchdogFire's own async dump-write
	// goroutine calls slog.Warn too, so the sink must be concurrency-safe.
	var logBuf lockedBuffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prevDefault)

	var watchdogCauseVal atomic.Int32
	a.handleWatchdogFire(
		causeIdleStall, 42*time.Second, "sess-252-test",
		&watchdogCauseVal, 5*time.Second, 3*time.Minute,
		snapshotModel,
	)

	require.Equal(t, int32(causeIdleStall), watchdogCauseVal.Load(),
		"cause must still be stored regardless of the model-source fix")

	logged := logBuf.String()
	assert.Contains(t, logged, "provider=snapshot-provider",
		"the diagnostic log must name the largeModel SNAPSHOT passed to handleWatchdogFire "+
			"(the runTurn value captured once at turn start), not the live a.largeModel field — "+
			"a mid-turn model switch (SetModels via web-UI) must not rewrite which provider/model the watchdog blames")
	assert.Contains(t, logged, "model=snapshot-model",
		"the diagnostic log must name the snapshot model, not the live field")
	assert.NotContains(t, logged, "current-provider",
		"the live a.largeModel.Provider (current-provider) must NOT appear — it reflects a model switch that happened AFTER the turn started, not the model that actually hung")
	assert.NotContains(t, logged, "current-model",
		"the live a.largeModel.Model (current-model) must NOT appear in the diagnostic")
}
