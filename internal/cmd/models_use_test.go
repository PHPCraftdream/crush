// Tests for the worker/reviewer CLI settability gap: `crush models use` gained
// optional --worker/--reviewer flags, `crush models state` reports all four
// slots, and `crush models unset` can clear worker/reviewer individually or
// via the new "all" positional. These tests run the real RunE functions
// against an isolated data dir (same harness as runProvidersCmdInIsolatedApp
// in providers_test.go) so behavior is asserted against actual code paths,
// not a reimplementation.
package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	crushlog "github.com/charmbracelet/crush/internal/log"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolatedModelsEnv stands up a temp global data dir with a minimal
// crush.json and chdirs into a temp workspace. Unlike
// runProvidersCmdInIsolatedApp in providers_test.go — which invokes RunE with
// a separate "carrier" *cobra.Command standing in for the `cmd` parameter —
// this harness attaches the data-dir/debug flags setupApp needs DIRECTLY onto
// the real subcommand and passes that same command as both receiver and the
// `cmd` argument to RunE. That distinction matters here: models_use.go's
// RunE reads its OWN local flags (--worker/--reviewer) via cmd.Flags(), and
// if `cmd` inside RunE were a stand-in carrier without those flags
// registered, GetString would silently return "" instead of erroring —
// exactly the failure mode this harness exists to avoid. Real cobra
// Execute() always calls RunE(cmd, args) with cmd being the actual
// subcommand instance, so this matches production behavior more closely.
func isolatedModelsEnv(t *testing.T) (globalPath string) {
	t.Helper()
	// config.ResetProviderCacheForTests: internal/config's provider/hyper
	// catalog resolution is memoized process-wide via sync.Once — whichever
	// test in this binary calls it FIRST freezes the result (embedded vs.
	// on-disk cache vs. network) for every other test, regardless of their
	// own CRUSH_PROVIDER_CACHE_ONLY/CRUSH_GLOBAL_DATA. Safe here because
	// these tests are serial (no t.Parallel()) — see the exported func's
	// own doc comment for why this must never be called from a parallel
	// test.
	config.ResetProviderCacheForTests()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("CRUSH_GLOBAL_DATA", dataDir)
	// GlobalConfig() (CRUSH_GLOBAL_CONFIG/XDG_CONFIG_HOME) is a SEPARATE
	// resolution path from GlobalConfigData() (CRUSH_GLOBAL_DATA) above —
	// see CLAUDE.md's "two real config paths" caveat. Without this, app.New()
	// (invoked by e.g. TestModelsBump_AllFourRoles) reads the
	// real host ~/.config/crush/crush.json and, if it configures MCP
	// servers, tries to open real network connections to them from
	// inside the test — observed hanging a stress run for 9+ minutes
	// until the 10-minute go test panic-timeout.
	//
	// configDir MUST be a directory distinct from dataDir, not the same tmp
	// reused for both env vars: lookupConfigs (internal/config/load.go)
	// loads BOTH GlobalConfig() and GlobalConfigData() and merges them via
	// go-jsons, which merges (rather than replaces) array-valued fields on
	// collision. Pointing both env vars at the identical "<dir>/crush.json"
	// path made lookupConfigs load and merge that one file with itself —
	// harmless while the seeded config below has no array fields, but a
	// latent bug (doubled array entries, doubled ConfigStore.LoadedPaths()
	// entries) waiting for a future test to add one. globalPath below stays
	// pointed at dataDir (where GlobalConfigData()/ScopeGlobal actually
	// resolves and where modelsUseCmd/modelsBumpCmd actually write); configDir
	// only needs to exist and be isolated from the real host file —
	// lookupConfigs still merges dataDir's crush.json (with the seeded zai
	// key) in regardless of which of the two paths it physically lives under.
	configDir := filepath.Join(tmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("CRUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")

	crushlog.Setup("", false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	globalPath = filepath.Join(dataDir, "crush.json")
	// Seed a synthetic zai api_key from the start rather than starting
	// from a bare "{}". Live model resolution (a.ResolveModel /
	// configureSelectedModels, exercised by e.g. modelsUseCmd's
	// large/small positionals and modelsBumpCmd/modelsStateCmd's role
	// reads) drops any provider whose api_key doesn't resolve non-empty —
	// see configureProviders' zai case in internal/config/load.go. Before
	// CRUSH_GLOBAL_CONFIG was isolated above, that requirement was met by
	// accident via a real host ~/.config/crush/crush.json's real zai key
	// leaking in; now that the leak is closed, every test using this
	// harness needs its own deterministic key so zai atoms keep resolving
	// instead of silently falling back to a default provider (observed:
	// local-cli/cli-claude-haiku) and corrupting the file these tests
	// read back. seedZAIProvider below performs the identical write for
	// callers that want to do it explicitly/redundantly; both write the
	// same content.
	require.NoError(t, os.WriteFile(globalPath, []byte(`{"providers":{"zai":{"api_key":"test-zai-key"}}}`), 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		_ = os.Chdir(orig)
		cancel()
		// Release only THIS test's own connection (ref-counted, keyed by
		// tmp) rather than db.ResetPool(), which used to nuke the ENTIRE
		// process-wide connection pool — including any other test's still-
		// open connection to a completely different data dir, if that
		// other test happened to be running concurrently (t.Parallel()) in
		// this same package's test binary. That cross-test interference,
		// not just OS-level handle-release lag, was a real contributor to
		// this package's Windows-only "process cannot access the file"
		// flakiness: one test's cleanup could force-close a sibling's live
		// connection out from under it.
		_ = db.Release(tmp)

		// Root-caused Windows-only flake (testing.go's TempDir
		// RemoveAll cleanup): db.Release() above is synchronous —
		// it calls sql.DB.Close() directly under a mutex, which in
		// turn drives modernc.org/sqlite's conn.Close() ->
		// sqlite3_close_v2() -> a synchronous Win32 CloseHandle on
		// the db/-wal/-shm files (modernc's Windows VFS opens files
		// via raw CreateFileW, no goroutines or runtime.SetFinalizer
		// anywhere in modernc.org/sqlite or modernc.org/libc). There
		// is no Go-controlled handle leak here: by the time this
		// Cleanup runs, the DB was already closed once via
		// db.Release in app.Shutdown's cleanup wg (awaited
		// synchronously by "defer a.Shutdown()" in every models_*
		// RunE before runModelsCmd returns), and ResetPool above is
		// just a belt-and-suspenders no-op repeat of that.
		//
		// The actual failure is the OS/kernel not finishing the
		// handle release before t.TempDir()'s own registered
		// cleanup (which runs AFTER this one — t.Cleanup is LIFO and
		// this closure is registered after both t.TempDir() calls
		// above) does its os.RemoveAll. Go's testing package already
		// retries ERROR_SHARING_VIOLATION/ERROR_ACCESS_DENIED for up
		// to a hardcoded 2s (see testing_windows.go's
		// isWindowsRetryable + removeAll's arbitraryTimeout), which
		// is normally plenty but was observed to be exceeded once
		// under full-parallel-suite (-failfast ./...) system load.
		// Actively probe for the sqlite files becoming removable
		// here, bounded well past that 2s window, so this test's own
		// cleanup absorbs the OS lag instead of racing t.TempDir()'s
		// fixed budget.
		waitForSQLiteHandleRelease(t, tmp)

		// Same treatment for workDir (the second t.TempDir() call
		// above, used as this test's cwd): os.Chdir(orig) already ran
		// at the top of this closure, so workDir is no longer the
		// process's current directory by this point, but a command run
		// under test (crushlog.Setup, setupApp, or the command itself)
		// can still leave a file underneath it with pending Windows
		// handle-release lag, same class of race as the SQLite files —
		// observed directly: a TestModelsBump_GLM52FullStepUp failure
		// with the locked path ending in \002 (workDir, not \001/tmp).
		// waitForSQLiteHandleRelease's actual mechanism (retry the real
		// os.RemoveAll) isn't SQLite-specific despite the name, so it
		// applies here unchanged.
		waitForSQLiteHandleRelease(t, workDir)
	})

	for _, cmd := range []*cobra.Command{modelsUseCmd, modelsStateCmd, modelsUnsetCmd} {
		ensureRootFlagStandIns(cmd, tmp)
		cmd.SetContext(ctx)
	}
	return globalPath
}

// waitForSQLiteHandleRelease polls (bounded) for dataDir to become fully
// removable, absorbing Windows' OS-level lag in finishing a CloseHandle
// after sqlite3_close_v2() returns — Go's stdlib testing package only
// tolerates ERROR_SHARING_VIOLATION/ERROR_ACCESS_DENIED for a fixed 2s (see
// testing_windows.go's isWindowsRetryable + removeAll's arbitraryTimeout),
// which was repeatedly observed exceeded under real concurrent-test load on
// this machine (both across packages and across this package's own
// t.Parallel() subtests).
//
// A prior version of this helper only probed specific named files
// (crush.db/-wal/-shm/-journal) via a rename-in-place check. That missed at
// least one real failure (TestModelsBump_RoleNotSet_ReportsCleanly) where
// something else under dataDir was still locked — the named-file allowlist
// can't be kept exhaustive as isolatedModelsEnv/setupBumpEnv's callers grow
// (config files, lock files, whatever a future command under test happens
// to create). This version instead retries the REAL os.RemoveAll(dataDir)
// directly, mirroring the equivalent fix in
// internal/agent/cliprovider/provider_test.go's waitForRemovable: if this
// preemptive removal succeeds, t.TempDir()'s own later os.RemoveAll simply
// finds nothing to do (RemoveAll on an already-missing path is a no-op
// success), so doing the real deletion here rather than merely probing is
// safe and catches any locked file regardless of name.
func waitForSQLiteHandleRelease(t *testing.T, dataDir string) {
	t.Helper()

	// 30s, not 10s: observed TestModelsBump_GLM52FullStepUp (which cycles
	// the DB open/close path multiple times, stepping a role through
	// several effort levels in one test) exceed a 10s budget under a cold
	// `go test` cache (every test binary recompiling at once — far heavier
	// transient CPU/IO load than a typical warm pre-push run).
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := os.RemoveAll(dataDir); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	if lastErr != nil {
		// Give up silently: t.TempDir()'s own cleanup will make one more
		// attempt (with its own 2s retry) and report the real error itself
		// if the handle is truly still held. We don't want to fail the
		// test here for a condition Go's own cleanup already surfaces
		// clearly.
		t.Logf("waitForSQLiteHandleRelease: %s still not removable after budget (%v); proceeding anyway", dataDir, lastErr)
	}
}

// ensureRootFlagStandIns registers debug/data-dir flags directly on cmd if
// not already present (idempotent across the package-level command vars
// reused by multiple tests), and points data-dir at tmp. cwd is intentionally
// left unset here — ResolveCwd falls back to os.Getwd(), which
// isolatedModelsEnv has already chdir'd into workDir, so omitting it is
// equivalent to passing it explicitly and one fewer flag to reset.
func ensureRootFlagStandIns(cmd *cobra.Command, dataDir string) {
	if f := cmd.Flags().Lookup("debug"); f == nil {
		cmd.Flags().Bool("debug", false, "")
	}
	if f := cmd.Flags().Lookup("data-dir"); f == nil {
		cmd.Flags().String("data-dir", "", "")
	}
	_ = cmd.Flags().Set("data-dir", dataDir)
}

// runModelsCmd parses args onto cmd (caller is responsible for resetting any
// flags left over from a prior call via resetModelsUseFlags/
// resetModelsUnsetFlags — cobra commands are package-level vars, shared
// across all tests in this file), captures stdout, and runs the real RunE
// with cmd as its own receiver (see isolatedModelsEnv for why).
func runModelsCmd(t *testing.T, cmd *cobra.Command, args ...string) (stdout string, runErr error) {
	t.Helper()
	require.NoError(t, cmd.ParseFlags(args))

	var buf bytes.Buffer
	oldOut := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(done) }()

	runErr = cmd.RunE(cmd, cmd.Flags().Args())

	_ = w.Close()
	os.Stdout = oldOut
	<-done
	return buf.String(), runErr
}

func TestModelsUse_TwoPositionalRegression(t *testing.T) {
	// The most important test in this file: the existing, established
	// two-positional `crush models use <large> <small>` form must behave
	// identically to before the --worker/--reviewer flags were added.
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm5_1", "glm5_turbo")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, `"large"`)
	assert.Contains(t, content, `"glm-5.1"`)
	assert.Contains(t, content, `"small"`)
	assert.Contains(t, content, `"glm-5-turbo"`)
	// No worker/reviewer keys should appear when the flags are omitted.
	assert.NotContains(t, content, `"worker"`)
	assert.NotContains(t, content, `"reviewer"`)
}

// TestModelsUse_SmallFlagOnly_LeavesLargeUntouched is the regression test for
// task #249: previously the only way to change the small ("fast") slot was
// the two-positional form, which always rewrote large too — there was no way
// to touch just one of large/small. --small (mirroring the existing
// --worker/--reviewer pattern) must set ONLY the small slot, leaving large
// (and worker/reviewer) exactly as they were before the call.
func TestModelsUse_SmallFlagOnly_LeavesLargeUntouched(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm5_1", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsUseFlags(t)
	_, runErr = runModelsCmd(t, modelsUseCmd, "--small", "glm4_7_flash")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	// Parse the active "models" object specifically rather than grepping raw
	// content: "recent_models" is a separate MRU history that legitimately
	// keeps older values around (that's its entire purpose) alongside the
	// current "models" object, so a raw-content NotContains would wrongly
	// flag that unrelated history array.
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))

	assert.Contains(t, string(doc.Models["large"]), `"glm-5.1"`, "large must be untouched by a --small-only call")
	assert.Contains(t, string(doc.Models["small"]), `"glm-4.7-flash"`, "small must reflect the new --small value")
	assert.NotContains(t, string(doc.Models["small"]), `"glm-5-turbo"`, "the active small slot must not still be the OLD value")
}

// TestModelsUse_LargeFlagOnly_LeavesSmallUntouched is the --large mirror of
// the test above.
func TestModelsUse_LargeFlagOnly_LeavesSmallUntouched(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm5_1", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsUseFlags(t)
	_, runErr = runModelsCmd(t, modelsUseCmd, "--large", "glm4_7_flash")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	// See TestModelsUse_SmallFlagOnly_LeavesLargeUntouched for why this
	// parses the active "models" object rather than grepping raw content.
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))

	assert.Contains(t, string(doc.Models["large"]), `"glm-4.7-flash"`, "large must reflect the new --large value")
	assert.Contains(t, string(doc.Models["small"]), `"glm-5-turbo"`, "small must be untouched by a --large-only call")
	assert.NotContains(t, string(doc.Models["large"]), `"glm-5.1"`, "the active large slot must not still be the OLD value")
}

// TestModelsUse_LargeAndSmallFlagsTogether covers setting both via flags in
// one call (still without touching worker/reviewer), as distinct from the
// positional form.
func TestModelsUse_LargeAndSmallFlagsTogether(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "--large", "glm5_1", "--small", "glm5_turbo")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, `"glm-5.1"`)
	assert.Contains(t, content, `"glm-5-turbo"`)
	assert.NotContains(t, content, `"worker"`)
	assert.NotContains(t, content, `"reviewer"`)
}

// TestModelsUse_PositionalAndLargeFlagConflict_Rejected proves the two forms
// (positional <large> <small> vs. --large/--small flags) cannot be mixed —
// silently preferring one over the other would be worse than refusing.
func TestModelsUse_PositionalAndLargeFlagConflict_Rejected(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm5_1", "glm5_turbo", "--large", "glm4_7_flash")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "cannot combine positional")
}

// TestModelsUse_OnePositionalArg_RejectedAsAmbiguous proves a single
// positional arg (neither the old 2-arg form nor the new 0-arg-plus-flags
// form) is rejected with a clear message rather than silently guessing which
// slot it was meant for.
func TestModelsUse_OnePositionalArg_RejectedAsAmbiguous(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm5_1")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "got 1")
}

// TestModelsUse_NoArgsNoFlags_RejectedAsNoOp proves calling `models use` with
// nothing at all (no positional args, no --large/--small/--worker/--reviewer
// flags) fails clearly instead of silently doing nothing.
func TestModelsUse_NoArgsNoFlags_RejectedAsNoOp(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd)
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "nothing to set")
}

// TestModelsUse_WorkerOnlyViaFlags_NoPositionals proves --worker/--reviewer
// alone (no large/small at all, positional or flag) still works exactly as
// before — the new large/small flags must not have disturbed the existing
// worker/reviewer-only use case.
func TestModelsUse_WorkerOnlyViaFlags_NoPositionals(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm5_1", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsUseFlags(t)
	_, runErr = runModelsCmd(t, modelsUseCmd, "--worker", "glm4_7_flash")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, `"glm-5.1"`, "large must be untouched")
	assert.Contains(t, content, `"glm-5-turbo"`, "small must be untouched")
	assert.Contains(t, content, `"glm-4.7-flash"`, "worker must reflect the new value")
}

func TestModelsUse_WorkerAndReviewerFlags(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_2")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, `"glm-5.1"`)       // large
	assert.Contains(t, content, `"glm-5-turbo"`)   // small
	assert.Contains(t, content, `"glm-4.7-flash"`) // worker
	assert.Contains(t, content, `"glm-5.2"`)       // reviewer
}

func TestModelsUse_WorkerViaShortCode(t *testing.T) {
	// Verify the short-code/atom resolution path (o47x, h45l, ...) also
	// applies to the new --worker/--reviewer flags, not just large/small.
	globalPath := isolatedModelsEnv(t)

	defer setMockEffortLevels([]string{"low", "medium", "high", "xhigh", "max"})()

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo", "--worker", "fl")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)

	// "fl" is the fable-low short code -> local-cli / cli-claude-fable, effort low.
	assert.Contains(t, content, `"local-cli"`)
	assert.Contains(t, content, `"cli-claude-fable"`)
	assert.Contains(t, content, `"low"`)
}

func TestModelsUse_UnknownWorkerAtomFailsCleanly(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo", "--worker", "not-a-real-atom-xyz")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "worker:")
}

// TestModelsUse_InvalidReviewerEffort_ZeroWrites is the core regression test
// for the partial-write bug: a valid large/small combined with an invalid
// --reviewer effort suffix must result in NO fields written at all — not
// large/small silently persisted while only the reviewer write is rejected.
// Before the fix, RunE wrote large and small to disk (and printed "set
// large = ..." / "set small = ...") BEFORE it ever parsed/validated
// --reviewer, so a typo'd effort like "glm5_2-hihg" left large/small durably
// changed even though the overall command reported failure. This asserts the
// raw on-disk bytes are byte-identical to the pre-command state (the seed
// `{}` isolatedModelsEnv wrote), not just crush state's "effective" view,
// which could misleadingly inherit a prior write.
//
// Uses the atom form "glm5_2-hihg" rather than the raw "zai/glm-5.2@hihg"
// string from the live bug report, because raw provider/model resolution
// goes through a.ResolveModel against the (cached) provider catalog, which
// this isolated test harness doesn't reliably resolve for every zai model id
// (see the comment on TestModelsUse_RawZAIEffort_ValidSucceeds above) — an
// environmental quirk, unrelated to the ordering bug under test. The atom
// path exercises the identical validateEffortForModel call without that
// dependency.
func TestModelsUse_InvalidReviewerEffort_ZeroWrites(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	before, err := os.ReadFile(globalPath)
	require.NoError(t, err)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo", "--reviewer", "glm5_2-hihg")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "reviewer:")
	assert.Contains(t, runErr.Error(), "not a valid level")

	after, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after),
		"config file must be byte-identical after a failed `models use` call — zero writes expected, got: %s", after)
}

// TestModelsUse_InvalidReviewerEffort_WorkerAlsoNotWritten is the same
// scenario as above but with a valid --worker present too, confirming the
// batch-write covers ALL provided slots, not just large/small: an invalid
// --reviewer must prevent --worker from being written as well.
func TestModelsUse_InvalidReviewerEffort_WorkerAlsoNotWritten(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	before, err := os.ReadFile(globalPath)
	require.NoError(t, err)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo",
		"--worker", "glm4_7_flash",
		"--reviewer", "glm5_2-hihg")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "reviewer:")

	after, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after),
		"worker must not be written either when reviewer validation fails, got: %s", after)
}

// TestModelsUse_InvalidLargePositional_ShortCircuitsBeforeWorkerReviewer
// covers the reverse direction: an invalid large positional arg must fail
// before even attempting to parse/touch worker/reviewer, with the same
// zero-write guarantee. This also guards against a regression where pass 1
// validation order changes and worker/reviewer get parsed (and their errors
// surface) before large/small, masking the actual first failure.
func TestModelsUse_InvalidLargePositional_ShortCircuitsBeforeWorkerReviewer(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	before, err := os.ReadFile(globalPath)
	require.NoError(t, err)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"not-a-real-atom-xyz", "glm5_turbo",
		"--worker", "glm4_7_flash", "--reviewer", "glm5_2")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "large:")

	after, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after),
		"an invalid large positional must result in zero writes, got: %s", after)
}

// TestModelsUse_AllFourValid_StillWritesAll is the regression guard for the
// fully-valid four-argument case: restructuring RunE into a validate-then-
// write two-pass flow must not break the working case where every argument
// is valid — all four slots must still end up written.
func TestModelsUse_AllFourValid_StillWritesAll(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo",
		"--worker", "glm4_7_flash", "--reviewer", "glm5_2")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))

	assert.Contains(t, doc.Models, "large")
	assert.Contains(t, doc.Models, "small")
	assert.Contains(t, doc.Models, "worker")
	assert.Contains(t, doc.Models, "reviewer")

	content := string(data)
	assert.Contains(t, content, `"glm-5.1"`)
	assert.Contains(t, content, `"glm-5-turbo"`)
	assert.Contains(t, content, `"glm-4.7-flash"`)
	assert.Contains(t, content, `"glm-5.2"`)
}

// seedZAIProvider overwrites the isolated crush.json at globalPath with an
// explicit zai provider that carries a literal (self-resolving, non-empty)
// api_key.
//
// Any test that exercises the raw "zai/glm-4.7-flash[@effort]" syntax needs
// this. That path resolves through a.ResolveModel -> findModels, which
// iterates the LIVE, post-configureProviders provider map — and a provider
// only survives there if its API key resolves non-empty (see
// configureProviders' zai case in internal/config/load.go). isolatedModelsEnv
// has no ZAI_API_KEY/ZHIPU_API_KEY, so without this the zai provider is
// dropped and "zai/glm-4.7-flash" resolves to nothing.
//
// These tests historically passed ONLY by accident: isolatedModelsEnv
// redirects CRUSH_GLOBAL_DATA/XDG_DATA_HOME but NOT GlobalConfig()
// (CRUSH_GLOBAL_CONFIG/XDG_CONFIG_HOME), so on a dev machine with a real
// ~/.config/crush/crush.json configuring zai with an api_key, that host
// config leaked in and kept the provider alive. On a from-scratch CI runner
// (no such file, no env keys) the provider was absent and these tests failed
// with `model "zai/glm-4.7-flash" not found`. Seeding the provider explicitly
// makes the raw-resolution path deterministic, independent of host config.
// The model list itself still comes from the embedded catwalk catalog.
func seedZAIProvider(t *testing.T, globalPath string) {
	t.Helper()
	require.NoError(t, os.WriteFile(globalPath,
		[]byte(`{"providers":{"zai":{"api_key":"test-zai-key"}}}`), 0o644))
}

// TestModelsUse_RawZAIEffort_ValidSucceeds covers the new validated raw
// "provider/model@effort" path for a Z.AI atom: a level that IS one of the
// real declared levels (off/on, for a boolean-thinking-only model like
// glm-4.7-flash) must still be accepted and written. seedZAIProvider makes
// "zai/glm-4.7-flash" resolvable deterministically regardless of host config
// (the raw path goes through a.ResolveModel against the live provider map).
// The effort-validation logic itself is exercised precisely, atom-by-atom, by
// the TestValidateEffortForModel_* unit tests below.
func TestModelsUse_RawZAIEffort_ValidSucceeds(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	seedZAIProvider(t, globalPath)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"zai/glm-4.7-flash@on", "glm5_turbo")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `"glm-4.7-flash"`)
	assert.Contains(t, content, `"on"`)
}

// TestModelsUse_RawZAIEffort_TypoNowFailsCleanly is the load-bearing
// before/after test for this task: before adding a real, validated
// ReasoningLevels array for Z.AI atoms, ANY string after "@" in the raw
// "provider/model@effort" syntax (splitModelEffort) was accepted silently —
// a typo like "hgih" would have been written to config as-is and either
// ignored or mismapped by the wire-level provider code, with no error at
// all. Confirm that gap is now closed: an unsupported/typo'd level for a
// model that resolves to a KNOWN atom (glm-4.7-flash -> glm4_7_flash, which
// declares {"off","on"}) must now be rejected with a clear error.
func TestModelsUse_RawZAIEffort_TypoNowFailsCleanly(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	// The model must resolve first for effort validation to be reached; seed
	// zai so this asserts the effort-typo path, not a spurious "not found".
	seedZAIProvider(t, globalPath)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"zai/glm-4.7-flash@hgih", "glm5_turbo")
	require.Error(t, runErr, "a typo'd effort level for a known atom must now be rejected, not silently accepted")
	assert.Contains(t, runErr.Error(), "hgih")
	assert.Contains(t, runErr.Error(), "not a valid effort level")
	assert.Contains(t, runErr.Error(), "off|on")
}

// TestModelsUse_RawZAIEffort_TypoRejectedForWorkerSlot proves the same
// validation applies uniformly to the --worker/--reviewer role slots added
// in task #68, not just the two positional large/small args.
func TestModelsUse_RawZAIEffort_TypoRejectedForWorkerSlot(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	// The model must resolve first for effort validation to be reached; seed
	// zai so this asserts the effort-typo path, not a spurious "not found".
	seedZAIProvider(t, globalPath)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo", "--worker", "zai/glm-4.7-flash@ultramega")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "worker:")
	assert.Contains(t, runErr.Error(), "not a valid effort level")
}

// TestValidateEffortForModel_NonAtomStillUnvalidated is a regression guard,
// exercised directly against validateEffortForModel (models_atoms.go) rather
// than the full `models use` CLI path (which additionally requires the
// model to resolve against a real provider catalog — orthogonal to effort
// validation and awkward to fake in this isolated-config test harness): a
// (provider, model) pair that ISN'T in the atom registry at all has no
// levels array to validate against, so any effort string must still be
// accepted. This task closes the validation gap only for known atoms, not
// for arbitrary models outside the registry.
func TestValidateEffortForModel_NonAtomStillUnvalidated(t *testing.T) {
	err := validateEffortForModel("openai", "gpt-5", "totally-not-a-real-level")
	assert.NoError(t, err, "models outside the atom registry remain unvalidated by design")
}

// TestValidateEffortForModel_KnownAtomRejectsTypo is the direct-unit-test
// counterpart to TestModelsUse_RawZAIEffort_TypoNowFailsCleanly above: same
// fact, asserted without going through the full CLI/config-write path.
// Uses glm-4.7-flash (boolean off/on levels — see zaiBooleanThinkingLevels)
// rather than glm-5.2, whose graduated 8-value set is covered separately by
// TestValidateEffortForModel_GLM52AcceptsGraduatedLevel below.
func TestValidateEffortForModel_KnownAtomRejectsTypo(t *testing.T) {
	err := validateEffortForModel("zai", "glm-4.7-flash", "hgih")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hgih")
	assert.Contains(t, err.Error(), "not a valid effort level")
	assert.Contains(t, err.Error(), "off|on")
}

// TestValidateEffortForModel_KnownAtomAcceptsRealLevel confirms every level
// in a Z.AI atom's real declared array validates successfully, for a
// boolean-thinking-only model (glm-4.7-flash: only off/on, per Z.AI's docs —
// no documented graduated reasoning_effort outside GLM-5.2).
func TestValidateEffortForModel_KnownAtomAcceptsRealLevel(t *testing.T) {
	for _, level := range []string{"off", "on"} {
		assert.NoError(t, validateEffortForModel("zai", "glm-4.7-flash", level), "level %q", level)
	}
}

// TestValidateEffortForModel_GLM52AcceptsGraduatedLevel confirms GLM-5.2
// specifically validates against its OWN, larger vocabulary (3 real wire
// states: off/high/max — one more than every other Z.AI atom's off/on).
// A wider Z.AI-documented value like "xhigh" is deliberately NOT accepted:
// this fork's coordinator.go collapses it to the same "max" wire value as
// an explicit "max", so it isn't a meaningful, distinct level to expose.
// See the comment on zaiReasoningLevels in models_atoms.go.
func TestValidateEffortForModel_GLM52AcceptsGraduatedLevel(t *testing.T) {
	for _, level := range []string{"off", "high", "max"} {
		assert.NoError(t, validateEffortForModel("zai", "glm-5.2", level), "level %q", level)
	}
	// "on" is a glm4_7_flash-style boolean value, not part of glm-5.2's
	// vocabulary — must still be rejected for glm-5.2.
	err := validateEffortForModel("zai", "glm-5.2", "on")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid effort level")

	// "xhigh" is part of Z.AI's own documented reasoning_effort enum but
	// collapses to "max" on the wire — no longer accepted as a distinct
	// glm5_2 level (deliberately stricter than before this task).
	err = validateEffortForModel("zai", "glm-5.2", "xhigh")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid effort level")
}

// TestValidateEffortForModel_EmptyEffortAlwaysOK confirms omitting an
// effort suffix entirely (the common case) is always valid, atom or not.
func TestValidateEffortForModel_EmptyEffortAlwaysOK(t *testing.T) {
	assert.NoError(t, validateEffortForModel("zai", "glm-5.2", ""))
	assert.NoError(t, validateEffortForModel("openai", "gpt-5", ""))
}

func TestModelsState_ReportsWorkerAndReviewer(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_2")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)

	assert.Contains(t, out, "worker:")
	assert.Contains(t, out, "glm-4.7-flash")
	assert.Contains(t, out, "reviewer:")
	assert.Contains(t, out, "glm-5.2")
}

func TestModelsState_JSONReportsWorkerAndReviewer(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_2")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd, "--json")
	require.NoError(t, runErr)

	assert.Contains(t, out, `"worker"`)
	assert.Contains(t, out, `"reviewer"`)
	assert.Contains(t, out, "glm-4.7-flash")
	assert.Contains(t, out, "glm-5.2")
}

// TestModelsState_UnsetEffort_ShowsKnownZAIDefault covers the core case for
// this task: a slot with no explicit effort on a provider with a KNOWN
// documented unset-default (Z.AI, "unset -> thinking on, high" per
// coordinator.go's getProviderOptions and providerEffortDocs in
// models_efforts.go) must show that default as a terse parenthetical.
// "glm5_1" (large, in this test) is set with no @effort suffix, so
// m.ReasoningEffort == "" and effortEffectiveNote must fire.
func TestModelsState_UnsetEffort_ShowsKnownZAIDefault(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm5_1", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)

	assert.Contains(t, out, "unset -> thinking on, high")
}

// TestModelsState_ExplicitEffort_TakesPrecedenceOverDefault covers the
// no-confusion requirement: a slot WITH an explicit effort must show that
// explicit value (via the existing effortSuffix " effort=<level>" rendering),
// not the provider's unset-default note — the two must never appear together
// on the same slot's line, or a reader can't tell "you set this" from "this
// merely happens by default". Uses glm4_7_flash (boolean off/on Z.AI atom) rather than
// glm5_2: the embedded catwalk provider catalog this test env falls back to
// in CRUSH_PROVIDER_CACHE_ONLY mode doesn't list glm-5.2 at all, which makes
// config's large/small validation silently substitute a different zai model
// on load — an unrelated, pre-existing environmental quirk of the vendored
// catwalk embedded data, not something this task touches. glm4_7_flash IS in
// that embedded list, so it round-trips through config load untouched.
func TestModelsState_ExplicitEffort_TakesPrecedenceOverDefault(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_7_flash-on", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)

	largeLine, ok := lineContaining(out, "large:")
	require.True(t, ok, "expected a 'large:' line in output:\n%s", out)
	assert.Contains(t, largeLine, "effort=on")
	assert.NotContains(t, largeLine, "unset ->")
}

// lineContaining returns the first line of s containing substr, and whether
// one was found.
func lineContaining(s, substr string) (string, bool) {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, substr) {
			return line, true
		}
	}
	return "", false
}

// TestModelsState_JSON_UnsetEffort_ShowsKnownDefault verifies the --json
// mode also surfaces the same fact machine-readably (nilOrEffortDefault),
// not just the human text rendering.
func TestModelsState_JSON_UnsetEffort_ShowsKnownDefault(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm5_1", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd, "--json")
	require.NoError(t, runErr)

	var doc struct {
		Effective struct {
			LargeEffortDefault *string `json:"large_effort_default"`
		} `json:"effective"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	require.NotNil(t, doc.Effective.LargeEffortDefault)
	assert.Equal(t, "unset -> thinking on, high", *doc.Effective.LargeEffortDefault)
}

// TestModelsState_JSON_ExplicitEffort_NullDefault verifies the JSON default
// field is null (not the fact string) when the slot has an explicit effort —
// the machine-readable mirror of the text-mode precedence test above. See
// the comment on TestModelsState_ExplicitEffort_TakesPrecedenceOverDefault
// for why glm4_7_flash is used instead of glm5_2 here.
func TestModelsState_JSON_ExplicitEffort_NullDefault(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_7_flash-on", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd, "--json")
	require.NoError(t, runErr)

	var doc struct {
		Effective struct {
			LargeEffortDefault *string `json:"large_effort_default"`
		} `json:"effective"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	assert.Nil(t, doc.Effective.LargeEffortDefault)
}

// TestUnsetEffortNote_KnownAndUnknownProviders is the direct-unit-test
// counterpart of the CLI-level tests above (same rationale as
// TestValidateEffortForModel_NonAtomStillUnvalidated: exercising provider
// resolution through the full `models use`/`models state` CLI path for a
// raw, non-atom provider/model requires the model to resolve against a real
// provider catalog, which this isolated-config test harness can't reliably
// fake). Covers all three requirements from this task in one place: a
// KNOWN default for Z.AI, the OPPOSITE known default for DeepSeek (proving
// unsetEffortNote actually branches per provider instead of hardcoding one
// fact), and silence ("") for io.net/hyper/Alibaba/generic-openai, none of
// which has a single unambiguous unset-effort fact documented in
// providerEffortDocs — effortEffectiveNote and nilOrEffortDefault must never
// guess for those.
func TestUnsetEffortNote_KnownAndUnknownProviders(t *testing.T) {
	assert.Equal(t, "unset -> thinking on, high", unsetEffortNote("zai"))
	assert.Equal(t, "unset -> thinking off", unsetEffortNote("deepseek"))

	for _, provider := range []string{"ionet", "hyper", "alibaba-singapore", "openai", "anthropic", "local-cli"} {
		assert.Empty(t, unsetEffortNote(provider), "provider %q has no documented unset-default fact and must not guess one", provider)
	}
}

// TestEffortEffectiveNote_ExplicitVsUnset is a direct unit test of the
// precedence rule effortEffectiveNote implements: an explicit effort always
// wins (nothing extra to print — effortSuffix already shows the explicit
// value), and an unset effort shows the provider's known default, or nothing
// for an undocumented provider.
func TestEffortEffectiveNote_ExplicitVsUnset(t *testing.T) {
	// Explicit effort set: no default note, regardless of provider.
	explicit := config.SelectedModel{Provider: "zai", Model: "glm-5.2", ReasoningEffort: "max"}
	assert.Empty(t, effortEffectiveNote(explicit))

	// Unset effort, known Z.AI default: note must appear.
	unsetZAI := config.SelectedModel{Provider: "zai", Model: "glm-5.2"}
	assert.Contains(t, effortEffectiveNote(unsetZAI), "unset -> thinking on, high")

	// Unset effort, undocumented provider: must stay silent, not guess.
	unsetUnknown := config.SelectedModel{Provider: "openai", Model: "gpt-5"}
	assert.Empty(t, effortEffectiveNote(unsetUnknown))
}

// TestNilOrEffortDefault_JSONCounterpart mirrors the text-mode test above
// for the --json path's nilOrEffortDefault helper.
func TestNilOrEffortDefault_JSONCounterpart(t *testing.T) {
	explicit := config.SelectedModel{Provider: "zai", Model: "glm-5.2", ReasoningEffort: "max"}
	assert.Nil(t, nilOrEffortDefault(true, explicit))

	unsetZAI := config.SelectedModel{Provider: "zai", Model: "glm-5.2"}
	assert.Equal(t, "unset -> thinking on, high", nilOrEffortDefault(true, unsetZAI))

	unsetUnknown := config.SelectedModel{Provider: "openai", Model: "gpt-5"}
	assert.Nil(t, nilOrEffortDefault(true, unsetUnknown))

	// Slot not set at all: nil regardless of provider/effort.
	assert.Nil(t, nilOrEffortDefault(false, unsetZAI))
}

func TestModelsUnset_ClearsWorkerOnly(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_2")
	require.NoError(t, runErr)

	resetModelsUnsetFlags(t)
	_, runErr = runModelsCmd(t, modelsUnsetCmd, "worker")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))

	// "models.worker" (the active override) must be gone; "recent_models"
	// (separate MRU history, untouched by unset) may still list it — assert
	// only against the "models" object so that distinction isn't conflated.
	_, workerStillSet := doc.Models["worker"]
	assert.False(t, workerStillSet, "models.worker should have been removed, got: %s", data)
	assert.Contains(t, doc.Models, "reviewer") // reviewer survives
	assert.Contains(t, doc.Models, "large")    // large survives
	assert.Contains(t, doc.Models, "small")    // small survives
}

func TestModelsUnset_AllClearsAllFourSlots(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_2")
	require.NoError(t, runErr)

	resetModelsUnsetFlags(t)
	_, runErr = runModelsCmd(t, modelsUnsetCmd, "all")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)

	assert.NotContains(t, content, `"models"`)
}

func TestModelsUnset_UnknownSlotFailsCleanly(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUnsetFlags(t)
	_, runErr := runModelsCmd(t, modelsUnsetCmd, "bogus-slot")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "expected large|small|worker|reviewer|both|all")
}

// resetModelsUseFlags clears the persisted flag values on modelsUseCmd so
// state from a previous test in this same process doesn't leak in (cobra
// commands are package-level vars, shared across all tests in this file).
func resetModelsUseFlags(t *testing.T) {
	t.Helper()
	for _, fl := range []string{"global", "local", "large", "small", "worker", "reviewer"} {
		if f := modelsUseCmd.Flags().Lookup(fl); f != nil {
			_ = f.Value.Set(f.DefValue)
		}
	}
	modelsUseCmd.SetArgs(nil)
}

func resetModelsUnsetFlags(t *testing.T) {
	t.Helper()
	for _, fl := range []string{"global", "local"} {
		if f := modelsUnsetCmd.Flags().Lookup(fl); f != nil {
			_ = f.Value.Set(f.DefValue)
		}
	}
	modelsUnsetCmd.SetArgs(nil)
}

func resetModelsStateFlags(t *testing.T) {
	t.Helper()
	if f := modelsStateCmd.Flags().Lookup("json"); f != nil {
		_ = f.Value.Set(f.DefValue)
	}
	modelsStateCmd.SetArgs(nil)
}
