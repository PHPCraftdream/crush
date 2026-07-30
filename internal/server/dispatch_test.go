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
	t.Parallel()

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
	t.Parallel()

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
