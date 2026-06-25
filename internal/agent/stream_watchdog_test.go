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
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration) {
		fired.Add(1)
	}, false, 0)
	defer func() {
		cancel()
		<-wd.done
	}()

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
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(observedIdle time.Duration) {
		fired.Add(1)
		firedIdle.Store(int64(observedIdle))
	}, false, 0)

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
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration) {
		fired.Add(1)
	}, false, 0)

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

	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration) {}, false, 0)

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
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration) {
		fired.Add(1)
	}, false, 0)
	defer func() {
		cancel()
		<-wd.done
	}()

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
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration) {
		fired.Add(1)
	}, false, 0)
	defer func() {
		cancel()
		<-wd.done
	}()

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
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration) {
		fired.Add(1)
	}, true, hardCap)
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
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration) {
		fired.Add(1)
	}, true, hardCap)

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
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration) {
		fired.Add(1)
	}, true, hardCap)

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
