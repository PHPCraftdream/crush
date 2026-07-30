package server

import (
	"bytes"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDispatchRecoversPanic verifies that a handler panic inside dispatch is
// recovered and logged rather than propagating and crashing the process (the
// underlying goroutine belongs to the Go runtime's scheduler, so an
// unrecovered panic there brings down the whole binary — every other
// connection and in-flight agent session along with it).
func TestDispatchRecoversPanic(t *testing.T) {
	// Deliberately NOT t.Parallel(): this test captures process-global
	// slog.Default() output. TestDispatchConnectionSurvivesHandlerPanic
	// below also triggers a "ws: handler panic" log via dispatch's
	// recover(), which always logs through whatever slog.Default()
	// currently is - running them concurrently lets one test's panic log
	// leak into (or crowd out) the other's captured buffer, since neither
	// test's t.Cleanup ordering is synchronized with the other's capture
	// window.
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := newClient(newHub(), nil)

	var done sync.WaitGroup
	done.Add(1)
	c.dispatch("handlePanicky", func() {
		defer done.Done()
		var m map[string]int
		m["boom"] = 1 // nil map write -> panic
	})

	// If the panic escaped dispatch's recover, the test binary itself would
	// crash here rather than reaching this point — so simply completing the
	// wait is part of the assertion.
	waitWithTimeout(t, &done, 2*time.Second)

	require.Eventually(t, func() bool {
		return bytes.Contains(buf.Bytes(), []byte("ws: handler panic"))
	}, time.Second, 10*time.Millisecond, "panic must be logged")
	require.Contains(t, buf.String(), "handlePanicky", "log must identify the handler by name")
}

// TestDispatchConnectionSurvivesHandlerPanic proves the SAME connection stays
// usable after one of its handlers panics: further dispatch calls on the same
// *Client still run to completion. This is the "server stays alive" half of
// the H-3 fix — panic isolation is only useful if the connection (and thus
// the rest of the session) keeps working afterwards.
func TestDispatchConnectionSurvivesHandlerPanic(t *testing.T) {
	// Deliberately NOT t.Parallel(): see the comment on
	// TestDispatchRecoversPanic above - this test's panic also logs
	// through the process-global slog.Default(), which that test captures.
	c := newClient(newHub(), nil)

	var panicked sync.WaitGroup
	panicked.Add(1)
	c.dispatch("boom", func() {
		defer panicked.Done()
		panic("simulated handler panic")
	})
	waitWithTimeout(t, &panicked, 2*time.Second)

	// The connection's semaphore and dispatch machinery must still work:
	// run N more handlers after the panic and confirm every one executes.
	const n = 5
	var ran int32
	var rest sync.WaitGroup
	rest.Add(n)
	for i := 0; i < n; i++ {
		c.dispatch("afterPanic", func() {
			defer rest.Done()
			atomic.AddInt32(&ran, 1)
		})
	}
	waitWithTimeout(t, &rest, 2*time.Second)

	require.EqualValues(t, n, atomic.LoadInt32(&ran), "handlers after a panic must still run")
}

// TestDispatchBoundsConcurrencyPerConnection verifies the per-connection
// semaphore actually caps in-flight handlers at maxConcurrentHandlersPerConn:
// a burst of far more than that many blocking handlers never has more than
// the cap running at once.
func TestDispatchBoundsConcurrencyPerConnection(t *testing.T) {
	t.Parallel()

	c := newClient(newHub(), nil)

	const burst = maxConcurrentHandlersPerConn * 4
	release := make(chan struct{})
	var inFlight int32
	var maxObserved int32
	var started sync.WaitGroup
	started.Add(burst)

	for i := 0; i < burst; i++ {
		go func() {
			c.dispatch("slow", func() {
				n := atomic.AddInt32(&inFlight, 1)
				for {
					old := atomic.LoadInt32(&maxObserved)
					if n <= old || atomic.CompareAndSwapInt32(&maxObserved, old, n) {
						break
					}
				}
				started.Done()
				<-release
				atomic.AddInt32(&inFlight, -1)
			})
		}()
	}

	// Give the burst time to saturate the semaphore, then confirm the
	// observed concurrency never exceeded the configured cap.
	time.Sleep(300 * time.Millisecond)
	require.LessOrEqual(t, atomic.LoadInt32(&maxObserved), int32(maxConcurrentHandlersPerConn),
		"in-flight handlers for one connection must never exceed the per-connection cap")

	close(release)
	waitWithTimeout(t, &started, 5*time.Second)
}

// TestDispatchControlBypassesWorkSemaphore is the regression test for the
// bug fixed here: with maxConcurrentHandlersPerConn work handlers already
// blocked/held open on one Client (simulating 12 long-running agent turns
// in flight, exactly like TestDispatchBoundsConcurrencyPerConnection does),
// a control-plane-shaped dispatch (dispatchControl, which is what
// CmdCancelAgent/CmdInterruptAndSend now go through instead of dispatch)
// must still complete promptly instead of queuing behind those 12 slots.
//
// Before the fix, dispatchControl didn't exist and control-plane commands
// were routed through dispatch/c.sem — this test would time out waiting for
// "control" to run, exactly reproducing the readPump starvation scenario
// described in the bug report (a stuck cancel/interrupt eventually leads to
// a 60s i/o-timeout connection teardown because readPump never calls
// ReadMessage() again while blocked acquiring a semaphore slot).
func TestDispatchControlBypassesWorkSemaphore(t *testing.T) {
	t.Parallel()

	c := newClient(newHub(), nil)

	// Saturate the WORK semaphore with maxConcurrentHandlersPerConn handlers
	// that block until the test releases them.
	release := make(chan struct{})
	var blocked sync.WaitGroup
	blocked.Add(maxConcurrentHandlersPerConn)
	for i := 0; i < maxConcurrentHandlersPerConn; i++ {
		c.dispatch("slowWork", func() {
			blocked.Done()
			<-release
		})
	}
	// Wait for all of them to actually be running (i.e. the work semaphore
	// is fully saturated) before exercising the control-plane path.
	waitWithTimeout(t, &blocked, 2*time.Second)
	defer close(release)

	// Now dispatch a control-plane-shaped command. If it shared c.sem with
	// the 12 blocked handlers above, this would hang until release fires.
	var controlRan sync.WaitGroup
	controlRan.Add(1)
	start := time.Now()
	c.dispatchControl("handleCancelAgent", func() {
		defer controlRan.Done()
	})
	waitWithTimeout(t, &controlRan, time.Second)
	elapsed := time.Since(start)

	require.Less(t, elapsed, time.Second,
		"control-plane dispatch must not queue behind saturated work handlers")
}

// TestDispatchControlDoesNotStarveWorkSemaphore proves the fix doesn't cut
// both ways: dispatching control-plane commands (even many of them) must
// never let them interfere with, or starve, the legitimate backpressure
// applied to long-running work handlers. Concretely: saturating the work
// semaphore still blocks a 13th work-shaped dispatch's CALLER exactly as
// before, regardless of how many control-plane dispatches ran concurrently.
func TestDispatchControlDoesNotStarveWorkSemaphore(t *testing.T) {
	t.Parallel()

	c := newClient(newHub(), nil)

	release := make(chan struct{})

	var blocked sync.WaitGroup
	blocked.Add(maxConcurrentHandlersPerConn)
	for i := 0; i < maxConcurrentHandlersPerConn; i++ {
		c.dispatch("slowWork", func() {
			blocked.Done()
			<-release
		})
	}
	waitWithTimeout(t, &blocked, 2*time.Second)

	// Fire a burst of control-plane dispatches concurrently with the
	// saturated work semaphore — none of this should affect sem's state.
	const controlBurst = 50
	var controlDone sync.WaitGroup
	controlDone.Add(controlBurst)
	for i := 0; i < controlBurst; i++ {
		c.dispatchControl("handleCancelAgent", func() {
			controlDone.Done()
		})
	}
	waitWithTimeout(t, &controlDone, 2*time.Second)

	// A 13th WORK-shaped dispatch must still block its caller (the sem is
	// still fully saturated by the 12 handlers held open above) — this is
	// the backpressure semantics #154 introduced and must be preserved.
	acquired := make(chan struct{})
	go func() {
		c.dispatch("oneMoreWork", func() {})
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("13th work dispatch acquired a semaphore slot while all 12 were still held — backpressure regressed")
	case <-time.After(200 * time.Millisecond):
		// Expected: still blocked waiting for a slot.
	}

	// Release the held handlers; the queued 13th dispatch must now proceed.
	close(release)

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("13th work dispatch never acquired a slot after handlers were released")
	}
}

// waitWithTimeout fails the test instead of hanging forever if wg never
// completes (e.g. a regression reintroduces a deadlock in dispatch).
func waitWithTimeout(t *testing.T, wg *sync.WaitGroup, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for dispatched handlers to finish")
	}
}

// syncBuffer is a concurrency-safe bytes.Buffer, needed because dispatch logs
// from a background goroutine while the test reads the buffer concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
