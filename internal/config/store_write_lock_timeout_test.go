package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestRemoveConfigFieldBestEffort_BoundedByInternalTimeout is the regression
// test for H-2 (task #153, commit 4c16a65f): removeConfigFieldBestEffort is
// the ONLY call path used by configureProviders' legacy
// "providers.anthropic" OAuth cleanup, which runs synchronously inside
// Load/reloadFromDiskLocked WHILE publishMu is held for the entire call.
// Before this fix, that cleanup went through the public, 30s-budget
// withConfigWriteLock; since publishMu gates every reader of the config
// store (including app startup via Load itself), a contended or wedged
// sibling crush process holding the on-disk crush.json.lock sidecar could
// stall the ENTIRE config subsystem for up to 30s. The fix added
// internalConfigWriteLockTimeout (2s) specifically for this call path.
//
// This test contends the exact lock file removeConfigFieldAt/
// withConfigWriteLockCtx acquires (path+".lock", via
// session.AcquireFileLockContext/session.TryAcquireFileLock — the same
// sidecar a second real crush process would take) for well longer than
// internalConfigWriteLockTimeout, then calls removeConfigFieldBestEffort
// and asserts it returns within a bound that proves the SHORT (2s) timeout
// was used, not the full 30s configWriteLockTimeout and not an unbounded
// wait.
func TestRemoveConfigFieldBestEffort_BoundedByInternalTimeout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")
	const key = "providers.anthropic.oauth"
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"anthropic":{"oauth":{"access_token":"secret"}}}}`), 0o600))

	// Hold the exact sidecar lock file removeConfigFieldAt contends on,
	// standing in for a sibling crush process that has it wedged/busy.
	// Held for well longer than internalConfigWriteLockTimeout (2s) and
	// released only after the assertions below run, via t.Cleanup.
	externalLock, err := session.TryAcquireFileLock(configPath + ".lock")
	require.NoError(t, err, "test setup: must be able to take the sidecar lock before the call under test runs")
	t.Cleanup(func() {
		_ = externalLock.Release()
	})

	store := newTestConfigStore(testStoreOpts{globalDataPath: configPath})

	start := time.Now()
	// removeConfigFieldBestEffort returns nothing and must never panic —
	// it logs and swallows failure by contract (see its doc comment), so
	// simply completing normally (this call returning at all) already
	// proves no panic escaped.
	store.removeConfigFieldBestEffort(ScopeGlobal, key)
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, internalConfigWriteLockTimeout,
		"expected the call to wait out roughly the full internal timeout budget before giving up (contended the whole time)")
	assert.Less(t, elapsed, 5*time.Second,
		"expected removeConfigFieldBestEffort to give up around internalConfigWriteLockTimeout (2s); "+
			"took %s — this must stay well under configWriteLockTimeout (30s), proving the SHORT bound was actually used", elapsed)

	// The on-disk key must be untouched: the write never happened because
	// the lock could not be acquired within budget. This confirms the
	// failure was swallowed rather than partially applied or corrupting
	// the file.
	data, rerr := os.ReadFile(configPath)
	require.NoError(t, rerr)
	assert.True(t, gjson.Get(string(data), key).Exists(),
		"key must still be present on disk — the best-effort removal must not have gone through while the sidecar lock was externally held")
}

// TestRemoveConfigFieldBestEffort_SucceedsQuicklyWhenLockFree is the
// control case: with no external contention, removeConfigFieldBestEffort
// must complete quickly (well under internalConfigWriteLockTimeout) and
// actually remove the key from disk. Without this, a bug that made the
// function ALWAYS wait out the full 2s (e.g. an inverted contention check)
// would slip through the timeout-bound test above, which only asserts an
// upper bound.
func TestRemoveConfigFieldBestEffort_SucceedsQuicklyWhenLockFree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")
	const key = "providers.anthropic.oauth"
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"anthropic":{"oauth":{"access_token":"secret"}}}}`), 0o600))

	store := newTestConfigStore(testStoreOpts{globalDataPath: configPath})

	start := time.Now()
	store.removeConfigFieldBestEffort(ScopeGlobal, key)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 500*time.Millisecond,
		"expected removeConfigFieldBestEffort to complete quickly with no lock contention, took %s", elapsed)

	data, rerr := os.ReadFile(configPath)
	require.NoError(t, rerr)
	assert.False(t, gjson.Get(string(data), key).Exists(),
		"key must have been removed from disk when the lock was immediately available")
}
