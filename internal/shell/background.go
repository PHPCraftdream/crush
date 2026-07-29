package shell

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/crush/internal/csync"
)

const (
	// MaxBackgroundJobs is the maximum number of concurrent background jobs allowed
	MaxBackgroundJobs = 50
	// CompletedJobRetentionMinutes is how long to keep completed jobs before auto-cleanup (8 hours)
	CompletedJobRetentionMinutes = 8 * 60

	// BufferRetentionMinutes is how long a completed job keeps its buffered
	// stdout/stderr bytes resident before Cleanup releases them (frees the
	// memory but keeps the job record itself — status/exit code/Command/etc.
	// — around until CompletedJobRetentionMinutes). This is intentionally
	// much shorter than CompletedJobRetentionMinutes: the buffered bytes are
	// the expensive part (up to 2*maxStreamBufferBytes per job) and are only
	// realistically useful for a short window right after completion, while
	// job metadata is cheap to keep for the full retention window so status
	// queries (job_output on a long-finished job) still resolve, just
	// without the original bytes.
	BufferRetentionMinutes = 15

	// maxStreamBufferBytes bounds how much of a single stdout/stderr stream a
	// background job keeps resident in memory. A chatty/never-ending command
	// (`yes`, a noisy `watch`, a verbose build that never stops) would
	// otherwise grow its buffer without limit for as long as the job runs —
	// with up to MaxBackgroundJobs (50) such jobs live at once, that's an
	// unbounded multiplier on process memory. 3 MiB per stream (6 MiB per job
	// including both stdout+stderr, ~300 MiB worst case across 50 jobs) is
	// generous enough to hold many thousands of lines of real command output
	// (job_output/bash still only ever surface the last MaxOutputLength=30000
	// characters to the model via TruncateOutput) while capping the pathological
	// case at a small, predictable ceiling.
	maxStreamBufferBytes = 3 * 1024 * 1024

	// truncationMarkerBudget is reserved out of maxStreamBufferBytes for the
	// "[N bytes truncated]" marker text itself, so the buffer's resident size
	// never exceeds maxStreamBufferBytes even after the marker is spliced in.
	truncationMarkerBudget = 128
)

// No separate count-based cap is enforced on COMPLETED jobs, by design. A job
// that has finished is cheap once its buffered stdout/stderr is released
// (BufferRetentionMinutes, ~15 min after completion): only lightweight
// metadata (ID, Command, timestamps, atomic fields — a few hundred bytes) stays
// resident until CompletedJobRetentionMinutes purges it via Cleanup, which the
// bash tool invokes at the start of every run. The time-based retention
// already bounds how long completed jobs linger, and even thousands of them
// amount to only a few MiB of metadata; a separate count-based eviction would
// add a tuning knob and a "which completed job to drop first" policy for
// negligible gain. The concurrency limit (MaxBackgroundJobs) is enforced on
// ACTIVE jobs only via BackgroundShellManager.activeJobs.

// bufferRetention is the duration after completion before a job's buffered
// stdout/stderr bytes are released via a one-shot time.AfterFunc armed in
// Start's completion goroutine. Defaults to BufferRetentionMinutes; tests
// override it to a short duration to exercise the timer path without waiting
// 15 minutes.
var bufferRetention = time.Duration(BufferRetentionMinutes) * time.Minute

// boundedBuffer is a thread-safe, size-bounded byte sink used for a single
// background job's stdout or stderr stream.
//
// Unlike bytes.Buffer (the previous implementation, via syncBuffer), it never
// grows without bound: once the resident content would exceed maxBytes, the
// MIDDLE of the stream is dropped and replaced with a "[N bytes truncated]"
// marker, keeping the HEAD (first bytes ever written — usually identifies the
// command and any early errors) and the TAIL (most recent bytes — the current
// state) both intact. This is the standard head+tail log-truncation pattern:
// simply dropping the tail (keep-first-N-bytes) would make a long-running job
// look permanently frozen at its earliest output, and simply dropping the
// head (keep-last-N-bytes, like a ring buffer) would lose the context of what
// command was even running or how it failed at the start.
//
// A monotonic writtenBytes counter tracks the *total* number of bytes ever
// written, independent of how much is actually resident after truncation.
// This lets callers (WaitForChange) detect "new output has arrived" purely by
// comparing counters, without copying/allocating a snapshot of the buffered
// content on every poll.
type boundedBuffer struct {
	mu  sync.RWMutex
	buf bytes.Buffer

	maxBytes int

	// head holds the first headCap bytes ever written; once populated it is
	// immutable for the lifetime of the buffer.
	head    bytes.Buffer
	headCap int

	// writtenBytes is the total number of bytes ever passed to Write, whether
	// or not they are still resident in buf/head. It only ever increases.
	writtenBytes atomic.Int64

	// truncated is set once bytes have been dropped from the middle, so
	// String() knows to splice in the marker.
	truncated atomic.Bool
	// droppedBytes is how many bytes have been removed from the middle
	// (excludes head, excludes what's still resident in buf).
	droppedBytes atomic.Int64
}

func newBoundedBuffer(maxBytes int) *boundedBuffer {
	if maxBytes <= 0 {
		maxBytes = maxStreamBufferBytes
	}
	return &boundedBuffer{
		maxBytes: maxBytes,
		// Keep a fixed head allotment of a quarter of the budget (bounded to
		// a sane floor/ceiling) — enough to preserve early context without
		// starving the tail, which is what most callers actually care about
		// while a job is still running.
		headCap: max(min(maxBytes/4, 256*1024), 1),
	}
}

// Write appends p to the buffer, enforcing the size bound. It always reports
// having written len(p) bytes (matching bytes.Buffer/io.Writer semantics) —
// bytes beyond the cap are counted in writtenBytes and then dropped from the
// middle of the resident content, never silently discarded from the caller's
// point of view.
func (b *boundedBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n = len(p)
	b.writtenBytes.Add(int64(n))

	// Fill head first, from the very first bytes ever written.
	if b.head.Len() < b.headCap {
		room := b.headCap - b.head.Len()
		take := min(room, len(p))
		b.head.Write(p[:take])
		p = p[take:]
	}

	if len(p) == 0 {
		return n, nil
	}

	b.buf.Write(p)
	b.enforceLimitLocked()
	return n, nil
}

// WriteString is the string equivalent of Write.
func (b *boundedBuffer) WriteString(s string) (n int, err error) {
	return b.Write([]byte(s))
}

// enforceLimitLocked drops bytes from the front of b.buf (the middle of the
// overall stream, since head is stored separately) once the resident head +
// tail content would exceed maxBytes. Caller must hold b.mu.
func (b *boundedBuffer) enforceLimitLocked() {
	tailBudget := b.maxBytes - b.head.Len() - truncationMarkerBudget
	if tailBudget < 0 {
		tailBudget = 0
	}
	if b.buf.Len() <= tailBudget {
		return
	}
	excess := b.buf.Len() - tailBudget
	b.buf.Next(excess) // drop `excess` bytes from the front (middle of stream)
	b.droppedBytes.Add(int64(excess))
	b.truncated.Store(true)
}

// String returns a bounded snapshot of the buffered content: if nothing has
// ever been truncated, it's simply everything written so far (head==empty in
// that case since all bytes fit in buf); otherwise it's head + a
// "[N bytes truncated]" marker + tail, which never exceeds maxBytes.
func (b *boundedBuffer) String() string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if !b.truncated.Load() {
		if b.head.Len() == 0 {
			return b.buf.String()
		}
		return b.head.String() + b.buf.String()
	}

	dropped := b.droppedBytes.Load()
	marker := fmt.Sprintf("\n... [%d bytes truncated] ...\n", dropped)
	return b.head.String() + marker + b.buf.String()
}

// Len reports the total number of bytes ever written to the buffer,
// including ones already dropped by truncation. This is the value
// WaitForChange compares against, so growth is always observable even after
// the buffer itself has started discarding old data.
func (b *boundedBuffer) Len() int {
	return int(b.writtenBytes.Load())
}

// release drops the buffered content (head/tail bytes) while preserving the
// monotonic writtenBytes counter and truncation bookkeeping. Used once a
// completed job's output has been delivered to subscribers, so long-lived
// jobs don't hold their full (bounded, but still up to a few MiB) buffer
// resident for the entire CompletedJobRetentionMinutes retention window.
func (b *boundedBuffer) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Reassign zero-valued buffers instead of calling Reset().
	// bytes.Buffer.Reset() keeps the underlying capacity (documented stdlib
	// behaviour), so the old multi-MiB backing arrays would stay resident in
	// the heap until the boundedBuffer itself is collected. A fresh zero-value
	// Buffer has no backing array, letting GC reclaim the old one immediately.
	b.buf = bytes.Buffer{}
	b.head = bytes.Buffer{}
	// Keep writtenBytes/truncated/droppedBytes as-is: String() below will
	// still report a non-empty, honest placeholder rather than silently
	// looking like the command produced no output.
}

// released reports whether release() has been called (buffer content
// discarded) despite the stream having produced output at some point.
func (b *boundedBuffer) releasedWithContent() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.head.Len() == 0 && b.buf.Len() == 0 && b.writtenBytes.Load() > 0
}

// BackgroundShell represents a shell running in the background.
type BackgroundShell struct {
	ID          string
	Command     string
	Description string
	Shell       *Shell
	WorkingDir  string
	StartTime   time.Time
	ctx         context.Context
	cancel      context.CancelFunc
	stdout      *boundedBuffer
	stderr      *boundedBuffer
	done        chan struct{}
	exitErr     error
	completedAt atomic.Int64 // Unix timestamp when job completed (0 if still running)
	bufReleased atomic.Bool  // true once the stdout/stderr buffers have been released post-completion
}

// BackgroundShellManager manages background shell instances.
type BackgroundShellManager struct {
	shells *csync.Map[string, *BackgroundShell]

	// activeJobs counts the number of currently-running (not yet completed)
	// background jobs. It is the value the MaxBackgroundJobs limit is checked
	// against: unlike shells.Len() — which also counts completed jobs held for
	// up to CompletedJobRetentionMinutes so their results stay queryable — this
	// counter is decremented the moment a job's goroutine finishes, so a job
	// completing immediately frees its concurrency slot. Incremented under
	// startMu in Start (atomically with the limit check and the shells.Set
	// insert); decremented lock-free from the job's completion goroutine.
	activeJobs atomic.Int64

	// startMu serializes the "check MaxBackgroundJobs then insert" sequence
	// in Start so two concurrent Start calls can't both observe room under
	// the limit and both insert, overshooting MaxBackgroundJobs. shells
	// itself is a csync.Map (safe for concurrent Get/Set/Len individually),
	// but the check-then-increment-and-insert across two calls is a classic
	// non-atomic check-then-act race without this extra lock serializing the
	// sequence.
	startMu sync.Mutex
}

var (
	backgroundManager     *BackgroundShellManager
	backgroundManagerOnce sync.Once
	idCounter             atomic.Uint64
)

// newBackgroundShellManager creates a new BackgroundShellManager instance.
func newBackgroundShellManager() *BackgroundShellManager {
	return &BackgroundShellManager{
		shells: csync.NewMap[string, *BackgroundShell](),
	}
}

// GetBackgroundShellManager returns the singleton background shell manager.
func GetBackgroundShellManager() *BackgroundShellManager {
	backgroundManagerOnce.Do(func() {
		backgroundManager = newBackgroundShellManager()
	})
	return backgroundManager
}

// Start creates and starts a new background shell with the given command.
func (m *BackgroundShellManager) Start(ctx context.Context, workingDir string, blockFuncs []BlockFunc, command string, description string) (*BackgroundShell, error) {
	m.startMu.Lock()
	defer m.startMu.Unlock()

	// Check job limit against ACTIVE jobs only (not shells.Len(), which also
	// includes completed jobs retained for up to CompletedJobRetentionMinutes
	// so their results stay queryable — counting those would permanently block
	// new jobs once the retention window fills up even if everything has
	// finished). Holding startMu across the check-and-insert below (and the
	// activeJobs increment) makes this atomic with respect to other concurrent
	// Start calls, so the limit can't be overshot by a check-then-act race.
	if m.activeJobs.Load() >= int64(MaxBackgroundJobs) {
		return nil, fmt.Errorf("maximum number of background jobs (%d) reached. Please terminate or wait for some jobs to complete", MaxBackgroundJobs)
	}

	id := fmt.Sprintf("%03X", idCounter.Add(1))

	shell := NewShell(&Options{
		WorkingDir: workingDir,
		BlockFuncs: blockFuncs,
	})

	shellCtx, cancel := context.WithCancel(ctx)

	bgShell := &BackgroundShell{
		ID:          id,
		Command:     command,
		Description: description,
		WorkingDir:  workingDir,
		StartTime:   time.Now(),
		Shell:       shell,
		ctx:         shellCtx,
		cancel:      cancel,
		stdout:      newBoundedBuffer(maxStreamBufferBytes),
		stderr:      newBoundedBuffer(maxStreamBufferBytes),
		done:        make(chan struct{}),
	}

	m.shells.Set(id, bgShell)
	// Reserve the concurrency slot now (under startMu, atomically with the
	// limit check above). Released when the goroutine finishes, right after
	// completedAt is set — that single point covers normal completion, Kill,
	// and KillAll, since all three reach "done" via this goroutine returning.
	m.activeJobs.Add(1)

	go func() {
		defer close(bgShell.done)

		err := shell.ExecStream(shellCtx, command, bgShell.stdout, bgShell.stderr)

		bgShell.exitErr = err
		bgShell.completedAt.Store(time.Now().Unix())
		m.activeJobs.Add(-1)
		// Schedule buffer release on a timer so the (up to 6 MiB) buffered
		// stdout/stderr is freed after bufferRetention even if no further
		// bash task ever calls Cleanup. releaseBuffers is guarded by
		// bufReleased, so a later Cleanup or duplicate fire is a no-op.
		time.AfterFunc(bufferRetention, bgShell.releaseBuffers)
	}()

	return bgShell, nil
}

// Get retrieves a background shell by ID.
func (m *BackgroundShellManager) Get(id string) (*BackgroundShell, bool) {
	return m.shells.Get(id)
}

// ActiveJobs returns the number of currently-running background jobs (started
// but not yet completed). This is the value the MaxBackgroundJobs concurrency
// limit is enforced against; completed jobs retained for result querying are
// not counted.
func (m *BackgroundShellManager) ActiveJobs() int {
	return int(m.activeJobs.Load())
}

// Remove removes a background shell from the manager without terminating it.
// This is useful when a shell has already completed and you just want to clean up tracking.
func (m *BackgroundShellManager) Remove(id string) error {
	_, ok := m.shells.Take(id)
	if !ok {
		return fmt.Errorf("background shell not found: %s", id)
	}
	return nil
}

// Kill terminates a background shell by ID. The provided context bounds how
// long Kill waits for the shell to actually exit after cancelling it; this
// mirrors KillAll's bounding and prevents an unresponsive child process tree
// (e.g. a stuck node.exe on Windows that ignores context cancellation) from
// blocking the caller indefinitely. If ctx expires before the shell exits,
// Kill returns ctx.Err() — the shell has still been removed from the manager
// and its cancel func called, but the underlying OS process may live on as an
// orphan (see CLAUDE.md residual-risk note).
func (m *BackgroundShellManager) Kill(ctx context.Context, id string) error {
	shell, ok := m.shells.Take(id)
	if !ok {
		return fmt.Errorf("background shell not found: %s", id)
	}

	shell.cancel()
	select {
	case <-shell.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// BackgroundShellInfo contains information about a background shell.
type BackgroundShellInfo struct {
	ID          string
	Command     string
	Description string
}

// List returns all background shell IDs.
func (m *BackgroundShellManager) List() []string {
	ids := make([]string, 0, m.shells.Len())
	for id := range m.shells.Seq2() {
		ids = append(ids, id)
	}
	return ids
}

// Cleanup removes completed jobs that have been finished for more than the
// retention period, and — for completed jobs past the shorter
// BufferRetentionMinutes but not yet past full retention — releases their
// buffered stdout/stderr bytes to shrink memory use while keeping the job
// record (status, exit code, Command, etc.) queryable for the full window.
func (m *BackgroundShellManager) Cleanup() int {
	now := time.Now().Unix()
	retentionSeconds := int64(CompletedJobRetentionMinutes * 60)
	bufferRetentionSeconds := int64(BufferRetentionMinutes * 60)

	var toRemove []string
	for shell := range m.shells.Seq() {
		completedAt := shell.completedAt.Load()
		if completedAt <= 0 {
			continue
		}
		age := now - completedAt
		if age > retentionSeconds {
			toRemove = append(toRemove, shell.ID)
			continue
		}
		if age > bufferRetentionSeconds && !shell.bufReleased.Load() {
			shell.releaseBuffers()
		}
	}

	for _, id := range toRemove {
		m.Remove(id)
	}

	return len(toRemove)
}

// KillAll terminates all background shells. The provided context bounds how
// long the function waits for each shell to exit.
func (m *BackgroundShellManager) KillAll(ctx context.Context) {
	shells := slices.Collect(m.shells.Seq())
	m.shells.Reset(map[string]*BackgroundShell{})

	var wg sync.WaitGroup
	for _, shell := range shells {
		wg.Go(func() {
			shell.cancel()
			select {
			case <-shell.done:
			case <-ctx.Done():
			}
		})
	}
	wg.Wait()
}

// GetOutput returns the current output of a background shell. The returned
// strings are bounded snapshots (see boundedBuffer) — for a job that has
// produced more than the per-stream cap of output, the middle has been
// replaced with a "[N bytes truncated]" marker. If the buffers were already
// released after completion (scheduled via time.AfterFunc from the job's
// completion goroutine, see Start), and the stream had produced output before
// release, a placeholder note is returned instead of an empty string so this
// doesn't look like the command produced no output.
func (bs *BackgroundShell) GetOutput() (stdout string, stderr string, done bool, err error) {
	stdout, stderr = bs.snapshotOutput()
	select {
	case <-bs.done:
		return stdout, stderr, true, bs.exitErr
	default:
		return stdout, stderr, false, nil
	}
}

func (bs *BackgroundShell) snapshotOutput() (stdout string, stderr string) {
	stdout = bs.stdout.String()
	stderr = bs.stderr.String()
	if bs.bufReleased.Load() {
		if stdout == "" && bs.stdout.releasedWithContent() {
			stdout = "(output was truncated after completion)"
		}
		if stderr == "" && bs.stderr.releasedWithContent() {
			stderr = "(output was truncated after completion)"
		}
	}
	return stdout, stderr
}

// releaseBuffers drops the buffered stdout/stderr content (freeing the
// memory) while leaving the job's status/metadata intact. Idempotent: the
// first caller wins (via bufReleased CAS), subsequent callers return
// immediately. Automatically scheduled via time.AfterFunc from the job's
// completion goroutine (Start) after bufferRetention, and also reachable
// from Cleanup when a subsequent bash task triggers it — whichever fires
// first performs the release, the second is a no-op.
func (bs *BackgroundShell) releaseBuffers() {
	// Swap-based idempotency: the first caller (the completion timer in
	// Start, or Cleanup) flips bufReleased false→true and performs the
	// actual release; any later caller sees the already-true flag and
	// returns immediately. This makes double-release safe regardless of
	// call ordering between the timer and a subsequent Cleanup.
	if bs.bufReleased.Swap(true) {
		return
	}
	bs.stdout.release()
	bs.stderr.release()
}

// OnDone registers fn to be called EXACTLY ONCE when this background shell
// reaches a terminal state (the command exits, or it is killed). If the shell
// is already done when OnDone is called, fn runs promptly. fn runs on its own
// goroutine — it must not block. Used by higher layers (the agent) to react to
// a backgrounded command finishing without internal/shell importing them.
func (bs *BackgroundShell) OnDone(fn func()) {
	if fn == nil {
		return
	}
	go func() {
		<-bs.done
		fn()
	}()
}

// IsDone checks if the background shell has finished execution.
func (bs *BackgroundShell) IsDone() bool {
	select {
	case <-bs.done:
		return true
	default:
		return false
	}
}

// Wait blocks until the background shell completes.
func (bs *BackgroundShell) Wait() {
	<-bs.done
}

func (bs *BackgroundShell) WaitContext(ctx context.Context) bool {
	select {
	case <-bs.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// Elapsed returns the wall-clock runtime of the background shell. For a job
// that has completed this is start→now (not start→finish); callers polling a
// finished job see a stable, monotonically increasing value which is fine for
// a "how long has this been going" status hint.
func (bs *BackgroundShell) Elapsed() time.Duration {
	if bs.StartTime.IsZero() {
		return 0
	}
	return time.Since(bs.StartTime)
}

// TotalWrittenBytes returns the total number of stdout+stderr bytes ever
// written by the job so far, independent of how much is still resident after
// bounded-buffer truncation. Callers that want to poll for "has more output
// arrived" (job_output's wait:true path) MUST use this — not
// len(stdout)+len(stderr) from GetOutput's bounded snapshot — as the baseline
// passed to WaitForChange. Once a stream has overflowed its cap, the
// snapshot's length no longer grows 1:1 with real output, so a baseline
// derived from it would already be smaller than the live counters
// WaitForChange compares against, making WaitForChange return immediately
// instead of actually waiting for new output.
func (bs *BackgroundShell) TotalWrittenBytes() int {
	return bs.stdout.Len() + bs.stderr.Len()
}

// WaitForChange blocks until the job finishes, total written output (stdout+
// stderr combined, counted via each stream's monotonic writtenBytes counter —
// NOT the bounded resident snapshot, so this keeps working correctly even
// after a stream has been truncated) grows beyond sinceLen bytes, or ctx
// ends. sinceLen should come from TotalWrittenBytes, not from measuring a
// GetOutput snapshot. It polls the counters on a short ticker — there is no
// event bus for incremental writes, so the granularity is the ticker
// interval (250ms). Returns without error from any branch.
func (bs *BackgroundShell) WaitForChange(ctx context.Context, sinceLen int) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-bs.done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if bs.stdout.Len()+bs.stderr.Len() > sinceLen {
				return
			}
		}
	}
}
