package server

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// makeReplayMsg builds a size-byte payload whose first 4 bytes encode idx, so a
// sequence of buffered messages can be identified and ordered after eviction.
func makeReplayMsg(idx, size int) []byte {
	b := make([]byte, size)
	b[0] = byte(idx >> 24)
	b[1] = byte(idx >> 16)
	b[2] = byte(idx >> 8)
	b[3] = byte(idx)
	return b
}

func replayMsgIdx(b []byte) int {
	return int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
}

// TestReplayBufferByteBudget verifies the total-byte bound: pushing more bytes
// than replayByteBudget evicts from the front until it fits, and the surviving
// entries are the newest ones in oldest-to-newest order.
func TestReplayBufferByteBudget(t *testing.T) {
	t.Parallel()

	const (
		msgSize = 256 * 1024 // 256 KiB
		n       = 200        // 200 * 256 KiB = 50 MiB > 16 MiB budget
	)
	// Steady state: floor(replayByteBudget / msgSize) entries retained.
	wantCount := replayByteBudget / msgSize

	r := newReplayBuffer()
	for i := 0; i < n; i++ {
		ok := r.push(makeReplayMsg(i, msgSize))
		require.True(t, ok, "msg %d within per-event limit should be stored", i)
		// Invariant after every push: byte budget never exceeded.
		require.LessOrEqual(t, r.bytes, replayByteBudget,
			"byte budget exceeded after push %d", i)
	}

	// Collect surviving entries via forEach.
	var got []int
	var sum int
	r.forEach(func(b []byte) {
		got = append(got, replayMsgIdx(b))
		sum += len(b)
	})

	require.Equal(t, wantCount, r.count, "steady-state entry count")
	require.Equal(t, wantCount, len(got), "forEach must visit every stored entry")
	require.LessOrEqual(t, sum, replayByteBudget, "iterated total within budget")

	// Survivors must be the newest `wantCount` pushes, in order.
	wantFirst := n - wantCount // = 136 for the chosen constants
	for i, idx := range got {
		require.Equal(t, wantFirst+i, idx, "entry %d is not the expected newest-in-order", i)
	}
}

// TestReplayBufferOversizedEvent verifies the per-event ceiling: a single event
// larger than replayMaxEventSize is rejected for storage without disturbing the
// rest of the buffer.
func TestReplayBufferOversizedEvent(t *testing.T) {
	t.Parallel()

	r := newReplayBuffer()
	for i := 0; i < 3; i++ {
		require.True(t, r.push(makeReplayMsg(i, 100)), "small msg %d should store", i)
	}
	countBefore := r.count
	bytesBefore := r.bytes

	big := makeReplayMsg(42, replayMaxEventSize+1)
	ok := r.push(big)
	require.False(t, ok, "oversized event must be rejected for storage")

	// State unchanged by the rejected push.
	require.Equal(t, countBefore, r.count, "rejected push must not change count")
	require.Equal(t, bytesBefore, r.bytes, "rejected push must not change bytes")

	// The three small messages survive in order; the big one is absent.
	var got []int
	r.forEach(func(b []byte) { got = append(got, replayMsgIdx(b)) })
	require.Equal(t, []int{0, 1, 2}, got, "small messages must survive in order")
	for _, idx := range got {
		require.NotEqual(t, 42, idx, "oversized event must not appear in buffer")
	}
}

// TestReplayBufferNoReallocationOnEviction proves eviction is O(1): the backing
// slice is never grown (cap unchanged) nor resliced (len unchanged), and a
// steady-state push on a full buffer allocates nothing.
func TestReplayBufferNoReallocationOnEviction(t *testing.T) {
	// Not parallel: testing.AllocsPerRun forces GOMAXPROCS=1, which is
	// incompatible with t.Parallel().
	r := newReplayBuffer()
	capBefore := cap(r.buf)
	lenBefore := len(r.buf)
	require.Equal(t, maxBufferSize, capBefore, "backing slice pre-allocated to capacity")

	small := make([]byte, 64)
	// Fill to capacity.
	for i := 0; i < maxBufferSize; i++ {
		r.push(small)
	}
	require.Equal(t, maxBufferSize, r.count, "buffer should be full")

	// Push well past capacity — every push now evicts.
	for i := 0; i < 500; i++ {
		r.push(small)
	}

	// No reallocation, no reslicing: eviction never copies the underlying array.
	require.Equal(t, capBefore, cap(r.buf), "cap must not grow under eviction")
	require.Equal(t, lenBefore, len(r.buf), "len must not change under eviction")

	// Steady-state push on a full buffer must allocate zero bytes: the reused
	// `small` slice is created once outside the measured closure, and push only
	// does index arithmetic plus an in-place slot write.
	allocs := testing.AllocsPerRun(100, func() {
		r.push(small)
	})
	require.Equal(t, 0.0, allocs, "steady-state eviction must not allocate")
}

// TestHubReplayOrder is a lightweight integration check that a Hub's buffer
// replays stored events to a (simulated) new client in oldest-to-newest order.
func TestHubReplayOrder(t *testing.T) {
	t.Parallel()

	h := newHub()
	// Drive the buffer directly — deterministic, no Run()/channel timing.
	msgs := [][]byte{
		[]byte("first"),
		[]byte("second"),
		[]byte("third"),
	}
	for _, m := range msgs {
		require.True(t, h.buffer.push(m))
	}

	var got [][]byte
	h.buffer.forEach(func(b []byte) { got = append(got, b) })

	require.Len(t, got, len(msgs))
	for i, m := range msgs {
		require.True(t, bytes.Equal(got[i], m), "entry %d out of order", i)
	}
}
