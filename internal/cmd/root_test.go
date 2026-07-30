package cmd

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

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
