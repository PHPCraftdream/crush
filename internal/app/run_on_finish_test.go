package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunOnFinishHook_SetsEnvVars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses bash")
	}
	out := filepath.Join(t.TempDir(), "env.txt")
	hook := `env | grep ^CRUSH_ | sort > ` + out

	runOnFinishHook(hook, "test-sess", "stop", 0.042, 12345, 5*time.Second)

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	s := string(data)
	assert.Contains(t, s, "CRUSH_SESSION_ID=test-sess")
	assert.Contains(t, s, "CRUSH_EXIT_REASON=stop")
	assert.Contains(t, s, "CRUSH_COST_USD=0.042000")
	assert.Contains(t, s, "CRUSH_TOKENS=12345")
	assert.Contains(t, s, "CRUSH_DURATION_SEC=5")
}

func TestRunOnFinishHook_TimeoutKillsHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses bash sleep")
	}
	start := time.Now()
	// Hook sleeps 120s — should be killed after onFinishHookTimeout (30s).
	// We lower the bar: just assert it returns well before 120s.
	runOnFinishHook("sleep 120", "s", "stop", 0, 0, 0)
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 60*time.Second, "hook must be killed by timeout, not run for 120s")
}

func TestRunOnFinishHook_ErrorDoesNotPanic(t *testing.T) {
	// Non-existent command should not crash.
	assert.NotPanics(t, func() {
		runOnFinishHook("this-command-does-not-exist-12345", "s", "error", 0, 0, 0)
	})
}
