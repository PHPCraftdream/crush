package log

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDumpGoroutines_WritesUsableDump is the regression guard for the reason
// this function exists at all: when a crush process hung in production there
// was no way to find out where. pprof is only served when CRUSH_PROFILE is
// set (so it cannot be enabled after the fact on a live hang) and release
// builds strip symbols (so attaching a debugger returns nothing and kills the
// process). A dump written by the process itself, at the moment the hang is
// detected, is the only mechanism that survives all of that — so it must
// actually contain real stacks, not just an empty file.
func TestDumpGoroutines_WritesUsableDump(t *testing.T) {
	dir := t.TempDir()
	prev := logDir.Load()
	logDir.Store(dir)
	t.Cleanup(func() {
		if s, ok := prev.(string); ok {
			logDir.Store(s)
		} else {
			logDir.Store("")
		}
	})

	path, err := DumpGoroutines("unit test")
	require.NoError(t, err)
	require.NotEmpty(t, path)
	assert.Equal(t, dir, filepath.Dir(path), "dump must land next to crush.log, not somewhere unrelated")

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(bts)

	assert.Contains(t, content, "reason: unit test", "the dump must record WHY it was taken")
	assert.Contains(t, content, "pid: "+strconv.Itoa(os.Getpid()),
		"the dump must name the process it came from — several crush processes share one .crush dir")
	assert.Contains(t, content, "goroutine ",
		"the dump must contain actual goroutine stacks, which is the entire point")
	assert.Contains(t, content, "DumpGoroutines",
		"the calling goroutine's own frame must be present, proving stacks are real and not truncated away")
}

// TestDumpGoroutines_FallsBackWhenLogDirUnset proves a dump is still produced
// before/without log.Setup — a hang during early startup is exactly when the
// operator has the least information, so this path must not silently no-op.
func TestDumpGoroutines_FallsBackWhenLogDirUnset(t *testing.T) {
	prev := logDir.Load()
	logDir.Store("")
	t.Cleanup(func() {
		if s, ok := prev.(string); ok {
			logDir.Store(s)
		} else {
			logDir.Store("")
		}
	})

	path, err := DumpGoroutines("no log dir configured")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(path) })

	assert.True(t, strings.HasPrefix(filepath.Clean(path), filepath.Clean(os.TempDir())),
		"with no log dir configured the dump must fall back to the temp dir, got %s", path)
	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(bts), "goroutine ")
}
