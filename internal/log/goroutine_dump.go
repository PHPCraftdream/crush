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

// maxGoroutineDumpBytes caps how large a dump may grow. A wedged process
// with a few hundred goroutines lands well under a megabyte; the cap only
// exists so a pathological goroutine leak can't try to allocate unbounded
// memory in a process that is already in trouble.
const maxGoroutineDumpBytes = 32 << 20

// DumpGoroutines writes the stack traces of ALL current goroutines to a file
// next to crush.log and returns its path.
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
// calling this at the moment a hang is DETECTED (see the stream watchdog's
// onFire) captures the evidence automatically, in release builds, exactly
// once, at the only moment it is still available.
//
// Best-effort by construction: every failure returns an error for the caller
// to log, and none of them are worth escalating — a missing dump must never
// turn a recoverable stall into a crash.
func DumpGoroutines(reason string) (string, error) {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	// runtime.Stack truncates silently when the buffer is too small, and
	// reports exactly len(buf) in that case. Grow until it fits (or we hit
	// the cap) so the dump isn't cut off mid-goroutine.
	for n == len(buf) && len(buf) < maxGoroutineDumpBytes {
		buf = make([]byte, len(buf)*2)
		n = runtime.Stack(buf, true)
	}

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

	header := fmt.Sprintf(
		"crush goroutine dump\nreason: %s\npid: %d\ntime: %s\ngoroutines: %d\n\n",
		reason, os.Getpid(), time.Now().Format(time.RFC3339), runtime.NumGoroutine(),
	)
	if err := os.WriteFile(path, append([]byte(header), buf[:n]...), 0o644); err != nil {
		return "", fmt.Errorf("goroutine dump: write %s: %w", path, err)
	}
	return path, nil
}
