package config

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

// captureDefaultSlog redirects the process-global slog default logger
// to a buffer for the duration of fn, then restores the prior logger.
// logStartupNotice logs through slog.Default(), so this is how its
// decision is observed. Not safe alongside t.Parallel(): the default
// logger is global. No other test in this package swaps it, so the
// capture window is private to fn.
func captureDefaultSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

// TestLogStartupNotice_SuppressedUnlessDebug is the regression guard for
// startup stderr noise: every invocation, including `crush run --json`
// and the logs/sessions/mcp scripting commands, used to emit WARN lines
// like "No git repository detected..." and "Detected Apple Terminal...".
// These notices describe non-actionable environment defaults; the config
// adjustment (limited walk depth, transparent TUI) still applies. They
// must stay silent on the default/scripted path (debug=false) and only
// surface under the verbose path (`crush --debug` / `crush run --debug`).
func TestLogStartupNotice_SuppressedUnlessDebug(t *testing.T) {
	const gitMsg = "No git repository detected in working directory, will limit file walk operations"
	const termMsg = "Detected Apple Terminal, enabling transparent mode"

	t.Run("debug=false stays silent for both notices", func(t *testing.T) {
		out := captureDefaultSlog(t, func() {
			logStartupNotice(false, gitMsg, "depth", 2, "items", 100)
			logStartupNotice(false, termMsg)
		})
		assert.NotContains(t, out, gitMsg, "git notice must not fire on the scripted path")
		assert.NotContains(t, out, termMsg, "Apple Terminal notice must not fire on the scripted path")
	})

	t.Run("debug=true surfaces both notices and structured args", func(t *testing.T) {
		out := captureDefaultSlog(t, func() {
			logStartupNotice(true, gitMsg, "depth", 2, "items", 100)
			logStartupNotice(true, termMsg)
		})
		assert.Contains(t, out, gitMsg)
		assert.Contains(t, out, termMsg)
		// Text handler renders structured args as key=value.
		assert.Contains(t, out, "depth=2")
		assert.Contains(t, out, "items=100")
	})

	t.Run("variadic args are forwarded, not dropped", func(t *testing.T) {
		// Guards against an accidental future refactor to slog.Warn(msg)
		// that would silently lose the structured context.
		out := captureDefaultSlog(t, func() {
			logStartupNotice(true, "notice body", "k1", "v1", "k2", "v2")
		})
		assert.Contains(t, out, "notice body")
		assert.Contains(t, out, "k1=v1")
		assert.Contains(t, out, "k2=v2")
	})
}

// TestLogStartupNotice_EmitsAtDefaultInfoLevel documents why the gate is
// the `debug` flag rather than slog level: the notices fire inside Load,
// before crushlog.Setup installs the file logger, so they hit Go's
// default handler (level=Info). Emitting at Warn means `--debug` users
// still see them on stderr; a slog.Debug call here would be filtered by
// the Info handler and the diagnostic would be lost even under --debug.
func TestLogStartupNotice_EmitsAtDefaultInfoLevel(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil))) // opts == nil -> Level: Info
	defer slog.SetDefault(prev)

	logStartupNotice(true, "visible under --debug")
	assert.Contains(t, buf.String(), "visible under --debug")
}
