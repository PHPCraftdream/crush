package log

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"
)

// logDir records the directory Setup was pointed at, so DumpGoroutines can
// drop its dumps next to crush.log without every caller having to thread the
// config through. Empty until Setup runs (or when Setup got an empty path),
// in which case DumpGoroutines falls back to the OS temp dir.
var logDir atomic.Value // string

// SetLogDirForTest points WriteGoroutineDump's target directory at dir for
// the duration of the calling test, restoring the previous value via
// t.Cleanup. Exists so tests OUTSIDE this package (e.g.
// internal/agent's stream-watchdog-fire tests) can redirect where a REAL
// crushlog.WriteGoroutineDump call lands — including at a deliberately slow
// (deeply nested, not-yet-created) path — without needing Setup's
// process-wide sync.Once or reaching into this package's unexported logDir
// directly. Safe to call from any test package; not for production use.
func SetLogDirForTest(t testingTB, dir string) {
	t.Helper()
	prev := logDir.Load()
	logDir.Store(dir)
	t.Cleanup(func() {
		if s, ok := prev.(string); ok {
			logDir.Store(s)
		} else {
			logDir.Store("")
		}
	})
}

// testingTB is the minimal subset of testing.TB this package needs, so
// SetLogDirForTest can accept *testing.T (or *testing.B) without importing
// the "testing" package into non-test production code.
type testingTB interface {
	Helper()
	Cleanup(func())
}

// maxGoroutineDumpBytes caps how large a dump may grow. A wedged process
// with a few hundred goroutines lands well under a megabyte; the cap only
// exists so a pathological goroutine leak can't try to allocate unbounded
// memory in a process that is already in trouble.
const maxGoroutineDumpBytes = 32 << 20

// CaptureGoroutineStack takes runtime.Stack's snapshot of every current
// goroutine and returns a ready-to-write dump buffer (header + stacks). It
// does no I/O of any kind — no directory creation, no file write — so it
// cannot block on a stuck disk or network mount, and can safely be called
// synchronously from a context where an unbounded block is unacceptable
// (see the stream watchdog's onFire, in internal/agent/agent.go).
//
// This exists because a hung crush process was, in practice, undiagnosable.
// pprof is compiled in (main.go imports net/http/pprof) but only served when
// CRUSH_PROFILE is set, so an operator who hits a hang cannot retroactively
// enable it on the already-running process. Release binaries are built with
// stripped symbols, so attaching a debugger yields "could not find goroutine
// array" — and the attach attempt itself killed the only live instance of the
// bug we had. The result was a real, reproducible freeze whose cause could
// only be guessed at from source reading.
//
// runtime.Stack needs no symbols, no open port and no operator action, so
// calling this at the moment a hang is DETECTED captures the evidence
// automatically, in release builds, exactly once, at the only moment it is
// still available — but only if the capture itself runs synchronously with
// the detection. Deferring it to an async goroutine risks capturing the
// state AFTER whatever unwound the hang (e.g. an outer cancellation) rather
// than the hang itself, or never running at all if the process exits first.
// WriteGoroutineDump, in contrast, is pure I/O and safe to dispatch async.
func CaptureGoroutineStack(reason string) []byte {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	// runtime.Stack truncates silently when the buffer is too small, and
	// reports exactly len(buf) in that case. Grow until it fits (or we hit
	// the cap) so the dump isn't cut off mid-goroutine.
	for n == len(buf) && len(buf) < maxGoroutineDumpBytes {
		buf = make([]byte, len(buf)*2)
		n = runtime.Stack(buf, true)
	}

	header := fmt.Sprintf(
		"crush goroutine dump\nreason: %s\npid: %d\ntime: %s\ngoroutines: %d\n\n",
		reason, os.Getpid(), time.Now().Format(time.RFC3339), runtime.NumGoroutine(),
	)
	return append([]byte(header), buf[:n]...)
}

// WriteGoroutineDump writes a buffer captured by CaptureGoroutineStack to a
// file next to crush.log and returns its path. This is the (potentially
// slow) I/O half of what DumpGoroutines used to do in one synchronous call;
// splitting it out lets a caller capture the stack synchronously (bounded,
// no I/O) and dispatch only the write asynchronously, so a stuck disk or
// network mount can delay the write but never delay or lose the capture.
//
// Best-effort by construction: every failure returns an error for the caller
// to log, and none of them are worth escalating — a missing dump must never
// turn a recoverable stall into a crash.
func WriteGoroutineDump(buf []byte) (string, error) {
	dir, _ := logDir.Load().(string)
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("goroutine dump: create dir %s: %w", dir, err)
	}

	// PID + timestamp so concurrent crush processes sharing one .crush dir
	// (the normal case for parallel `crush run`) never overwrite each other.
	name := fmt.Sprintf("goroutines-%d-%s.txt", os.Getpid(), time.Now().Format("20060102-150405"))
	path := filepath.Join(dir, name)

	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return "", fmt.Errorf("goroutine dump: write %s: %w", path, err)
	}
	return path, nil
}

// DumpGoroutines writes the stack traces of ALL current goroutines to a file
// next to crush.log and returns its path. It is CaptureGoroutineStack
// immediately followed by WriteGoroutineDump — a synchronous convenience
// wrapper for callers that are fine blocking on the write (e.g. tests, or
// call sites not on a cancellation-critical path). Callers for whom the
// write's I/O must not be on a blocking critical path (the stream watchdog's
// onFire) should call CaptureGoroutineStack synchronously themselves and
// dispatch WriteGoroutineDump on their own goroutine instead of using this
// wrapper — see internal/agent/agent.go.
func DumpGoroutines(reason string) (string, error) {
	return WriteGoroutineDump(CaptureGoroutineStack(reason))
}
