package pubsub

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShutdown_ConcurrentCallsDoNotPanic guards against a
// check-then-act race in Shutdown: if two goroutines both observe
// b.done as not-yet-closed before either closes it, a naive
// "select on b.done, default: close(b.done)" implementation lets both
// reach close(b.done), and the second call panics ("close of closed
// channel"). Shutdown must be safe to call concurrently, any number of
// times.
func TestShutdown_ConcurrentCallsDoNotPanic(t *testing.T) {
	const goroutines = 64

	b := NewBroker[int]()

	// Give it a few live subscribers so Shutdown also has real
	// subscriber-draining work to do concurrently with itself.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range 4 {
		_ = b.Subscribe(ctx)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			assert.NotPanics(t, func() {
				b.Shutdown()
			})
		}()
	}
	close(start)
	wg.Wait()

	// done must end up closed exactly once and stay closed.
	select {
	case <-b.done:
	default:
		t.Fatal("expected b.done to be closed after Shutdown")
	}
}

// TestShutdown_Idempotent calls Shutdown sequentially multiple times
// and expects no panic and a stable empty-subscriber state.
func TestShutdown_Idempotent(t *testing.T) {
	b := NewBroker[string]()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = b.Subscribe(ctx)

	for range 5 {
		assert.NotPanics(t, func() {
			b.Shutdown()
		})
	}
	assert.Equal(t, 0, b.GetSubscriberCount())
}

// TestPublish_DropsWhenSubscriberBufferFull verifies the documented
// best-effort, lossy behavior of Publish: a full subscriber channel
// causes the event to be dropped (not block the publisher), and
// increments DropCount.
func TestPublish_DropsWhenSubscriberBufferFull(t *testing.T) {
	b := NewBrokerWithOptions[int](1) // buffer size 1, nobody reads
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub := b.Subscribe(ctx)

	// Fill the single buffer slot.
	b.Publish(CreatedEvent, 1)
	// Second publish must not block, and must be dropped.
	done := make(chan struct{})
	go func() {
		b.Publish(CreatedEvent, 2)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber channel; expected non-blocking drop")
	}

	assert.Equal(t, uint64(1), b.DropCount())

	// Only the first event should have been delivered.
	select {
	case ev := <-sub:
		assert.Equal(t, 1, ev.Payload)
	default:
		t.Fatal("expected the first event to have been delivered")
	}
	select {
	case ev := <-sub:
		t.Fatalf("did not expect a second event to be delivered, got %+v", ev)
	default:
	}
}

// TestPublishMustDeliver_WaitsThenDeliversIfReaderShowsUp verifies that
// PublishMustDeliver, unlike Publish, will wait (bounded by the
// must-deliver timeout) for buffer space rather than dropping
// immediately, and successfully delivers if a reader drains the
// channel before the timeout expires.
func TestPublishMustDeliver_WaitsThenDeliversIfReaderShowsUp(t *testing.T) {
	b := NewBrokerWithOptions[int](1)
	b.SetMustDeliverTimeout(500 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := b.Subscribe(ctx)

	// Fill the buffer so the fast path in PublishMustDeliver must fall
	// back to the bounded-blocking slow path.
	b.Publish(CreatedEvent, 1)

	// Drain the channel shortly after PublishMustDeliver starts
	// blocking, well within the timeout.
	go func() {
		time.Sleep(100 * time.Millisecond)
		<-sub // drain the first (fast-path) event, freeing buffer space
	}()

	deliverDone := make(chan struct{})
	go func() {
		b.PublishMustDeliver(context.Background(), UpdatedEvent, 2)
		close(deliverDone)
	}()

	select {
	case <-deliverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("PublishMustDeliver did not return")
	}

	// The second event should have been delivered (not dropped).
	select {
	case ev := <-sub:
		assert.Equal(t, UpdatedEvent, ev.Type)
		assert.Equal(t, 2, ev.Payload)
	case <-time.After(time.Second):
		t.Fatal("expected the must-deliver event to have reached the subscriber")
	}
	assert.Equal(t, uint64(0), b.MustDeliverDropCount())
}

// TestPublishMustDeliver_DropsAfterTimeoutInsteadOfBlockingForever
// verifies that when no reader ever appears, PublishMustDeliver still
// returns (bounded by the per-subscriber timeout) rather than blocking
// indefinitely, and counts the drop via MustDeliverDropCount.
func TestPublishMustDeliver_DropsAfterTimeoutInsteadOfBlockingForever(t *testing.T) {
	b := NewBrokerWithOptions[int](1)
	b.SetMustDeliverTimeout(100 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = b.Subscribe(ctx) // never read from

	// Fill the buffer so PublishMustDeliver must take the slow,
	// bounded-blocking path.
	b.Publish(CreatedEvent, 1)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		b.PublishMustDeliver(context.Background(), UpdatedEvent, 2)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PublishMustDeliver blocked far longer than its configured timeout")
	}
	elapsed := time.Since(start)
	// Should return close to the configured timeout, and well under
	// the outer safety bound.
	assert.Less(t, elapsed, time.Second)

	assert.Equal(t, uint64(1), b.MustDeliverDropCount())
}

// TestPublishMustDeliver_RespectsCallerContextCancellation verifies the
// publisher never blocks past the caller's own context cancellation
// even if that happens before the must-deliver timeout.
func TestPublishMustDeliver_RespectsCallerContextCancellation(t *testing.T) {
	b := NewBrokerWithOptions[int](1)
	b.SetMustDeliverTimeout(5 * time.Second) // much longer than the ctx below

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	_ = b.Subscribe(subCtx) // never read from

	b.Publish(CreatedEvent, 1) // fill the buffer

	callCtx, callCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer callCancel()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		b.PublishMustDeliver(callCtx, UpdatedEvent, 2)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PublishMustDeliver ignored caller context cancellation")
	}
	assert.Less(t, time.Since(start), time.Second)
}

func TestSetMustDeliverTimeout_ResetsOnNonPositive(t *testing.T) {
	b := NewBroker[int]()
	b.SetMustDeliverTimeout(10 * time.Millisecond)
	require.Equal(t, 10*time.Millisecond, b.mustDeliverTimeout)

	b.SetMustDeliverTimeout(0)
	assert.Equal(t, defaultMustDeliverTimeout, b.mustDeliverTimeout)

	b.SetMustDeliverTimeout(-1)
	assert.Equal(t, defaultMustDeliverTimeout, b.mustDeliverTimeout)
}
