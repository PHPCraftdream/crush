package csync

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKeyedQueue_AppendConcurrent(t *testing.T) {
	t.Parallel()

	q := NewKeyedQueue[int]()
	const n = 100

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(v int) {
			defer wg.Done()
			q.Append("session", v)
		}(i)
	}
	wg.Wait()

	got := q.TakeAll("session")
	require.Len(t, got, n, "every concurrent Append must be present, none lost")

	seen := make(map[int]bool, n)
	for _, v := range got {
		require.False(t, seen[v], "duplicate value %d, Append corrupted the queue", v)
		seen[v] = true
	}
	for i := range n {
		require.True(t, seen[i], "value %d missing from queue after concurrent Append", i)
	}
}

// TestKeyedQueue_AppendVsTakeAll_NoLostUpdate proves the core fix: while one
// goroutine Appends N items, another concurrently drains via TakeAll several
// times. Every appended item must end up either in one of the TakeAll
// results or in the queue after the last TakeAll — never vanish. This is
// exactly the window that the old csync.Map[string, []T] Get->Del pattern
// could lose an item in.
func TestKeyedQueue_AppendVsTakeAll_NoLostUpdate(t *testing.T) {
	t.Parallel()

	q := NewKeyedQueue[int]()
	const n = 5000
	const key = "session"

	var drained []int
	var drainedMu sync.Mutex

	var appenderWG sync.WaitGroup
	appenderWG.Add(1)
	go func() {
		defer appenderWG.Done()
		for i := range n {
			q.Append(key, i)
		}
	}()

	stop := make(chan struct{})
	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		for {
			select {
			case <-stop:
				return
			default:
				items := q.TakeAll(key)
				if len(items) > 0 {
					drainedMu.Lock()
					drained = append(drained, items...)
					drainedMu.Unlock()
				}
			}
		}
	}()

	// Wait for the appender to finish, then stop the drainer and take
	// whatever is left over.
	appenderWG.Wait()
	close(stop)
	<-drainerDone

	remaining := q.TakeAll(key)

	drainedMu.Lock()
	total := len(drained) + len(remaining)
	allItems := append(append([]int{}, drained...), remaining...)
	drainedMu.Unlock()

	require.Equal(t, n, total, "sum of all TakeAll results plus what remains must equal the number of Appends; a mismatch means an item was lost or duplicated")

	seen := make(map[int]bool, n)
	for _, v := range allItems {
		require.False(t, seen[v], "value %d appeared twice across TakeAll calls", v)
		seen[v] = true
	}
	for i := range n {
		require.True(t, seen[i], "value %d was lost", i)
	}
}

func TestKeyedQueue_FIFOOrder(t *testing.T) {
	t.Parallel()

	q := NewKeyedQueue[string]()
	q.Append("s1", "a")
	q.Append("s1", "b")
	q.Append("s1", "c")

	require.Equal(t, []string{"a", "b", "c"}, q.Snapshot("s1"))

	first, ok := q.PopFront("s1")
	require.True(t, ok)
	require.Equal(t, "a", first)
	require.Equal(t, []string{"b", "c"}, q.Snapshot("s1"))

	q.Append("s1", "d")
	require.Equal(t, []string{"b", "c", "d"}, q.Snapshot("s1"))

	all := q.TakeAll("s1")
	require.Equal(t, []string{"b", "c", "d"}, all)
	require.Equal(t, 0, q.Len("s1"))
}

func TestKeyedQueue_ClearUnderLoad(t *testing.T) {
	t.Parallel()

	q := NewKeyedQueue[int]()
	const key = "session"

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for i := range 50 {
				q.Append(key, i)
			}
		}()
		go func() {
			defer wg.Done()
			q.Clear(key)
		}()
		go func() {
			defer wg.Done()
			_ = q.TakeAll(key)
		}()
	}
	wg.Wait()

	// No panic, and the queue is left in a well-defined (usable) state:
	// draining now must terminate and leave Len at 0, and a fresh
	// Append/TakeAll cycle after that must round-trip exactly.
	_ = q.TakeAll(key)
	require.Equal(t, 0, q.Len(key))

	q.Append(key, 999)
	got := q.TakeAll(key)
	require.Equal(t, []int{999}, got)
	require.Equal(t, 0, q.Len(key))
}

func TestKeyedQueue_EmptyKeyOperations(t *testing.T) {
	t.Parallel()

	q := NewKeyedQueue[int]()
	require.Equal(t, 0, q.Len("missing"))
	require.Nil(t, q.Snapshot("missing"))
	require.Nil(t, q.TakeAll("missing"))
	_, ok := q.PopFront("missing")
	require.False(t, ok)
	// Clear on a never-touched key must not panic.
	q.Clear("missing")
}

func TestKeyedQueue_PerKeyIsolation(t *testing.T) {
	t.Parallel()

	q := NewKeyedQueue[int]()
	q.Append("a", 1)
	q.Append("b", 2)
	require.Equal(t, 1, q.Len("a"))
	require.Equal(t, 1, q.Len("b"))

	q.Clear("a")
	require.Equal(t, 0, q.Len("a"))
	require.Equal(t, 1, q.Len("b"))
}
