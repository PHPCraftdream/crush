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
	"testing"

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
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("CRUSH_GLOBAL_DATA", tmp)
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")

	crushlog.Setup("", false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	globalPath = filepath.Join(tmp, "crush.json")
	require.NoError(t, os.WriteFile(globalPath, []byte(`{}`), 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		_ = os.Chdir(orig)
		cancel()
		db.ResetPool()
	})

	for _, cmd := range []*cobra.Command{modelsUseCmd, modelsStateCmd, modelsUnsetCmd} {
		ensureRootFlagStandIns(cmd, tmp)
		cmd.SetContext(ctx)
	}
	return globalPath
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

func TestModelsUse_WorkerAndReviewerFlags(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_2")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, `"glm-5.1"`)   // large
	assert.Contains(t, content, `"glm-5-turbo"`) // small
	assert.Contains(t, content, `"glm-4.7-flash"`) // worker
	assert.Contains(t, content, `"glm-5.2"`)   // reviewer
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
	for _, fl := range []string{"global", "local", "worker", "reviewer"} {
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

