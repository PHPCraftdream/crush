package cmd

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRecoverAndLogPanic_LogsBeforeRePanicking is the regression test for
// task #178: Go's default panic handler writes only to os.Stderr, never
// through slog, so an unrecovered panic anywhere in the command tree
// previously left crush.log with zero trace of what happened. This proves
// recoverAndLogPanic (deferred at the top of Execute) logs the panic value
// and a stack trace via slog.Error under crashLogMarker BEFORE re-panicking
// with the exact same value — so the process's normal crash behavior (exit
// code, stderr trace, for anyone watching the terminal directly) is
// unchanged, while crush.log now durably records what happened.
func TestRecoverAndLogPanic_LogsBeforeRePanicking(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	const panicValue = "boom: simulated top-level panic"

	var rePanicked any
	func() {
		defer func() {
			rePanicked = recover()
		}()
		func() {
			defer recoverAndLogPanic()
			panic(panicValue)
		}()
	}()

	require.Equal(t, panicValue, rePanicked,
		"recoverAndLogPanic must re-panic with the exact same value, unchanged")

	logged := logBuf.String()
	require.Contains(t, logged, crashLogMarker,
		"the panic must be logged under the fixed crashLogMarker so sessions_why's hint stays accurate")
	require.Contains(t, logged, panicValue, "the actual panic value must appear in the logged line")
	require.Contains(t, logged, "goroutine", "a real stack trace (runtime/debug.Stack output) must be logged")
}

// TestRecoverAndLogPanic_NoPanicIsANoop confirms recoverAndLogPanic does
// nothing observable when there is no panic to recover — it must not log
// anything or otherwise interfere with a normal, successful return.
func TestRecoverAndLogPanic_NoPanicIsANoop(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	func() {
		defer recoverAndLogPanic()
		// No panic here — normal return.
	}()

	require.False(t, strings.Contains(logBuf.String(), crashLogMarker),
		"recoverAndLogPanic must not log anything when nothing panicked")
}

// withStdin temporarily replaces os.Stdin for the duration of a test.
func withStdin(t *testing.T, f *os.File) {
	t.Helper()
	prev := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = prev })
}

// TestMaybePrependStdin_RegularFileReadsImmediately pins the unchanged
// `< file` behavior: a regular file is fully available immediately and
// must be read and prepended with no bound.
func TestMaybePrependStdin_RegularFileReadsImmediately(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stdin-*.txt")
	require.NoError(t, err)
	_, err = f.WriteString("piped content")
	require.NoError(t, err)
	_, err = f.Seek(0, 0)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	withStdin(t, f)

	got, err := MaybePrependStdin("the prompt")
	require.NoError(t, err)
	require.Equal(t, "piped content\n\nthe prompt", got)
}

// TestMaybePrependStdin_NamedPipeWithDataReadsIt confirms a `|` pipe that
// writes and closes promptly still gets prepended, same as before this
// change.
func TestMaybePrependStdin_NamedPipeWithDataReadsIt(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	withStdin(t, r)

	_, err = w.WriteString("piped content")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	got, err := MaybePrependStdin("the prompt")
	require.NoError(t, err)
	require.Equal(t, "piped content\n\nthe prompt", got)
}

// TestMaybePrependStdin_NamedPipeNeverClosesDoesNotHang is the regression
// test for the incident that motivated this change: an operator (or a
// launcher script) invoked `crush run` with a positional prompt and no
// explicit `< file` redirect. stdin resolved to a dangling pipe — nothing
// written, never closed — and io.ReadAll blocked forever, well before
// --timeout's context deadline is even wired up, leaving the process
// alive with zero visible session for hours. MaybePrependStdin must give
// up after stdinReadGrace and proceed with just the original prompt
// instead of hanging.
func TestMaybePrependStdin_NamedPipeNeverClosesDoesNotHang(t *testing.T) {
	prevGrace := stdinReadGrace
	stdinReadGrace = 50 * time.Millisecond
	t.Cleanup(func() { stdinReadGrace = prevGrace })

	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		r.Close()
		w.Close() // never written to, never closed by the "writer" itself
	})
	withStdin(t, r)

	done := make(chan struct {
		got string
		err error
	}, 1)
	go func() {
		got, err := MaybePrependStdin("the prompt")
		done <- struct {
			got string
			err error
		}{got, err}
	}()

	select {
	case res := <-done:
		require.NoError(t, res.err)
		require.Equal(t, "the prompt", res.got, "must fall back to the bare prompt, not hang or corrupt it")
	case <-time.After(2 * time.Second):
		t.Fatal("MaybePrependStdin hung past stdinReadGrace — it must never block indefinitely on a dangling pipe")
	}
}

// TestMaybePrependStdin_NamedPipeSlowCloseKeepsData is the regression test
// for the data-loss bug in the original stdinReadGrace fix (bea57a9b): that
// version raced io.ReadAll of the WHOLE stream against stdinReadGrace, so a
// producer that wrote real data but took longer than stdinReadGrace to
// close the pipe caused the timeout branch to fire — silently discarding
// every byte the still-running goroutine had already buffered, while
// logging the misleading "produced no data" message even though data
// existed. MaybePrependStdin must now only bound the wait for the FIRST
// byte; once the producer proves it's alive, the data it already sent must
// survive even if the close is slow.
func TestMaybePrependStdin_NamedPipeSlowCloseKeepsData(t *testing.T) {
	prevGrace := stdinReadGrace
	stdinReadGrace = 50 * time.Millisecond
	t.Cleanup(func() { stdinReadGrace = prevGrace })

	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	withStdin(t, r)

	const payload = "some real data written promptly, but the pipe stays open past the grace window"
	_, err = w.WriteString(payload)
	require.NoError(t, err)

	done := make(chan struct {
		got string
		err error
	}, 1)
	go func() {
		got, err := MaybePrependStdin("the prompt")
		done <- struct {
			got string
			err error
		}{got, err}
	}()

	// Give MaybePrependStdin's first-byte read a chance to complete, then
	// wait well past stdinReadGrace before closing — proving the write
	// already happened and was seen before the grace window expired, and
	// that the eventual close (not the grace timer) is what unblocks the
	// full read.
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, w.Close())

	select {
	case res := <-done:
		require.NoError(t, res.err)
		require.Equal(t, payload+"\n\nthe prompt", res.got,
			"data written before the grace window expired must not be silently discarded just because the pipe closed late")
	case <-time.After(2 * time.Second):
		t.Fatal("MaybePrependStdin hung waiting for EOF after already seeing data — it must read through to EOF once the producer proved it's alive")
	}
}
