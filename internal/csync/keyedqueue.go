package csync

import "sync"

// KeyedQueue is a thread-safe collection of per-key FIFO queues. It exists
// to replace the historically racy pattern of pairing a [Map] holding
// `[]T` values with hand-rolled "Get -> mutate slice -> Set" or
// "Get -> Del" sequences at call sites: those two steps are not atomic
// with respect to each other, so a concurrent writer can interleave
// between them and either overwrite another goroutine's append (lost
// update on the Set side) or have its own append vanish into a queue that
// is being drained concurrently (lost update on the Get->Del side).
// KeyedQueue closes both windows by making Append, TakeAll, PopFront,
// Clear, Len and Snapshot each a single critical section per key.
type KeyedQueue[T any] struct {
	mu    sync.Mutex
	inner map[string][]T
}

// NewKeyedQueue creates a new empty [KeyedQueue].
func NewKeyedQueue[T any]() *KeyedQueue[T] {
	return &KeyedQueue[T]{
		inner: make(map[string][]T),
	}
}

// Append atomically adds item to the end of key's queue.
func (q *KeyedQueue[T]) Append(key string, item T) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.inner[key] = append(q.inner[key], item)
}

// TakeAll atomically removes and returns every item currently queued for
// key. The queue for key is empty (and the key removed from the backing
// map) once this returns. Any Append that happens-before this call (in
// the usual Go memory-model sense, e.g. because it's protected by the
// same mutex) is guaranteed to either be included in the returned slice
// or to remain visible to a later TakeAll/Snapshot — it can never be
// silently dropped, because there is no gap between reading and clearing.
func (q *KeyedQueue[T]) TakeAll(key string) []T {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.inner[key]
	if len(items) == 0 {
		return nil
	}
	delete(q.inner, key)
	return items
}

// PopFront atomically removes and returns the first item in key's queue,
// leaving the rest of the queue intact. Returns the zero value and false
// if the queue for key is empty.
func (q *KeyedQueue[T]) PopFront(key string) (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.inner[key]
	if len(items) == 0 {
		var zero T
		return zero, false
	}
	first := items[0]
	rest := items[1:]
	if len(rest) == 0 {
		delete(q.inner, key)
	} else {
		// Copy the remainder so the backing array of the original slice
		// (which items[0] and any external snapshot may still reference)
		// isn't mutated in place by a future Append reusing that capacity.
		next := make([]T, len(rest))
		copy(next, rest)
		q.inner[key] = next
	}
	return first, true
}

// Clear atomically removes every item queued for key.
func (q *KeyedQueue[T]) Clear(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.inner, key)
}

// Len returns the number of items currently queued for key.
func (q *KeyedQueue[T]) Len(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.inner[key])
}

// Snapshot returns a copy of the items currently queued for key without
// removing them.
func (q *KeyedQueue[T]) Snapshot(key string) []T {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.inner[key]
	if len(items) == 0 {
		return nil
	}
	out := make([]T, len(items))
	copy(out, items)
	return out
}
