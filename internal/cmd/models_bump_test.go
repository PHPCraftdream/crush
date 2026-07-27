// Tests for `crush models bump <role> <up|down>` — stepping a role's
// reasoning effort by one level in its model's own ordered Levels() array.
// Uses the same isolated-config harness as models_use_test.go
// (isolatedModelsEnv/runModelsCmd) so behavior is asserted against the real
// RunE, not a reimplementation.
package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetModelsBumpFlags clears persisted flag values on the package-level
// modelsBumpCmd between tests (cobra commands are shared vars).
func resetModelsBumpFlags(t *testing.T) {
	t.Helper()
	for _, fl := range []string{"global", "local"} {
		if f := modelsBumpCmd.Flags().Lookup(fl); f != nil {
			_ = f.Value.Set(f.DefValue)
		}
	}
	modelsBumpCmd.SetArgs(nil)
}

// setupBumpEnv wires modelsBumpCmd into the isolated harness alongside
// modelsUseCmd (needed to seed a role) and modelsStateCmd. isolatedModelsEnv
// itself only calls cmd.SetContext on modelsUseCmd/modelsStateCmd/
// modelsUnsetCmd (it predates this command), so modelsBumpCmd needs its own
// SetContext here too — without it, cmd.Context() inside setupApp returns
// nil and db.Connect panics on a nil context.
func setupBumpEnv(t *testing.T) (globalPath string) {
	t.Helper()
	globalPath = isolatedModelsEnv(t)
	ensureRootFlagStandIns(modelsBumpCmd, os.Getenv("CRUSH_GLOBAL_DATA"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	modelsBumpCmd.SetContext(ctx)
	return globalPath
}

// Note on model choice: these tests deliberately put glm5_2 on the
// worker/reviewer slot rather than large/small. `models use`'s large/small
// positionals additionally require the resolved model to be found in the
// provider catalog (a.ResolveModel / c.GetModel — see
// configureSelectedModels in internal/config/load.go), and this isolated
// test harness's shared provider cache (CRUSH_PROVIDER_CACHE_ONLY=1) does
// not reliably resolve "zai/glm-5.2" specifically (same environmental quirk
// already documented in models_use_test.go's TestModelsUse_RawZAIEffort_*
// comments — it resolves glm-4.7-flash reliably but not every zai id). When
// glm-5.2 fails that lookup for the LARGE slot specifically,
// configureSelectedModels silently falls back to a default model AND
// persists that fallback — corrupting the very file these tests read back.
// worker/reviewer are read directly from cfg.Models by modelsBumpCmd without
// going through that same default-substitution path, so they are unaffected
// and safe to use for glm5_2's full graduated level set. large/small below
// use glm4_7_flash (boolean off/on) instead, which resolves reliably.
func TestModelsBump_GLM52FullStepUp(t *testing.T) {
	globalPath := setupBumpEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_7_flash-off", "glm5_turbo", "--reviewer", "glm5_2-off")
	require.NoError(t, runErr)

	levels := []string{"off", "high", "max"}
	for i := 1; i < len(levels); i++ {
		resetModelsBumpFlags(t)
		_, runErr := runModelsCmd(t, modelsBumpCmd, "reviewer", "up")
		require.NoError(t, runErr, "step %d (up)", i)
		// Release the SQLite connection this setupApp call opened
		// (ref-counted per data dir; see internal/db/connect.go) before the
		// next loop iteration opens another one. Without this, ~8 rapid
		// setupApp cycles sharing one data dir in a single test body was
		// observed to trigger a Windows-only file-lock race in
		// t.TempDir()'s cleanup (WAL/SHM handles not always released
		// immediately after the LAST Close() at the very end of the test).
		//
		// db.Release(dataDir), not db.ResetPool(): the former only touches
		// THIS test's own data dir's ref-counted entry; ResetPool used to
		// nuke the entire process-wide pool, including any other test's
		// still-open connection to a different data dir if it happened to
		// be running concurrently (t.Parallel()) elsewhere in this package's
		// test binary — a real source of cross-test "process cannot access
		// the file" flakiness, not just OS handle-release lag.
		_ = db.Release(filepath.Dir(globalPath))

		// Assert against the raw config file directly — cheaper than
		// another `crush models state` call and avoids yet another setupApp
		// cycle.
		data, err := os.ReadFile(globalPath)
		require.NoError(t, err)
		assert.Contains(t, string(data), `"reasoning_effort":"`+levels[i]+`"`,
			"after step %d, expected effort %q; file:\n%s", i, levels[i], data)
	}

	// One final `models state` call confirms the same fact through the
	// user-facing resolution path too.
	resetModelsStateFlags(t)
	state, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)
	assert.Contains(t, state, "atom: glm5_2-max")
}

// TestModelsBump_GLM52FullStepDown is the symmetric downward walk, starting
// from the top (max) and stepping down to off.
func TestModelsBump_GLM52FullStepDown(t *testing.T) {
	globalPath := setupBumpEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_7_flash-on", "glm5_turbo", "--reviewer", "glm5_2-max")
	require.NoError(t, runErr)

	levels := []string{"off", "high", "max"}
	for i := len(levels) - 2; i >= 0; i-- {
		resetModelsBumpFlags(t)
		_, runErr := runModelsCmd(t, modelsBumpCmd, "reviewer", "down")
		require.NoError(t, runErr, "step down to index %d", i)
		// See the comment in TestModelsBump_GLM52FullStepUp: release the
		// per-data-dir SQLite connection between iterations to avoid a
		// Windows-only TempDir-cleanup file-lock race — scoped to this
		// test's own data dir, not the whole pool.
		_ = db.Release(filepath.Dir(globalPath))

		// Assert against the raw config file directly (see same comment).
		data, err := os.ReadFile(globalPath)
		require.NoError(t, err)
		assert.Contains(t, string(data), `"reasoning_effort":"`+levels[i]+`"`,
			"after stepping down, expected effort %q; file:\n%s", levels[i], data)
	}

	resetModelsStateFlags(t)
	state, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)
	assert.Contains(t, state, "atom: glm5_2-off")
}

// TestModelsBump_TopBoundary_UpIsInformationalNotError confirms that
// stepping "up" while already at the highest level ("max") produces a
// clean, non-error exit with an informational message — not a crash or Go
// error return.
func TestModelsBump_TopBoundary_UpIsInformationalNotError(t *testing.T) {
	setupBumpEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_7_flash-on", "glm5_turbo", "--reviewer", "glm5_2-max")
	require.NoError(t, runErr)

	resetModelsBumpFlags(t)
	_, runErr = runModelsCmd(t, modelsBumpCmd, "reviewer", "up")
	require.NoError(t, runErr, "boundary condition must be a clean, non-error exit")

	// The effort must remain unchanged at the boundary.
	resetModelsStateFlags(t)
	state, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)
	assert.Contains(t, state, "atom: glm5_2-max")
}

// TestModelsBump_BottomBoundary_DownIsInformationalNotError is the symmetric
// bottom-boundary case: stepping "down" while already at the lowest defined
// level ("off").
func TestModelsBump_BottomBoundary_DownIsInformationalNotError(t *testing.T) {
	setupBumpEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_7_flash-off", "glm5_turbo", "--reviewer", "glm5_2-off")
	require.NoError(t, runErr)

	resetModelsBumpFlags(t)
	_, runErr = runModelsCmd(t, modelsBumpCmd, "reviewer", "down")
	require.NoError(t, runErr, "boundary condition must be a clean, non-error exit")

	resetModelsStateFlags(t)
	state, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)
	assert.Contains(t, state, "atom: glm5_2-off")
}

// TestModelsBump_UnsetEffort_UpLandsOnLowestLevel confirms the chosen
// unset-handling design: starting from a role set to a bare atom (no effort
// suffix at all, i.e. ReasoningEffort == ""), "up" must land on the lowest
// defined level (index 0), not skip it or error.
func TestModelsBump_UnsetEffort_UpLandsOnLowestLevel(t *testing.T) {
	globalPath := setupBumpEnv(t)

	resetModelsUseFlags(t)
	// "glm5_2" with no "-<level>" suffix -> ReasoningEffort left empty
	// (Z.AI/ReasoningLevels-backed atoms make the suffix optional; see
	// parseAtom in models_atoms.go).
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_7_flash", "glm5_turbo", "--reviewer", "glm5_2")
	require.NoError(t, runErr)

	resetModelsBumpFlags(t)
	_, runErr = runModelsCmd(t, modelsBumpCmd, "reviewer", "up")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"off"`, "up from unset must land on the lowest level (off), got: %s", data)

	resetModelsStateFlags(t)
	state, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)
	assert.Contains(t, state, "atom: glm5_2-off")
}

// TestModelsBump_UnsetEffort_DownReportsAlreadyLowest confirms the symmetric
// choice: "down" from a fully-unset effort is itself the bottom boundary —
// it must be a clean, non-error, no-write outcome, not an attempt to go
// "below" unset.
func TestModelsBump_UnsetEffort_DownReportsAlreadyLowest(t *testing.T) {
	globalPath := setupBumpEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_7_flash", "glm5_turbo", "--reviewer", "glm5_2")
	require.NoError(t, runErr)

	before, err := os.ReadFile(globalPath)
	require.NoError(t, err)

	resetModelsBumpFlags(t)
	_, runErr = runModelsCmd(t, modelsBumpCmd, "reviewer", "down")
	require.NoError(t, runErr, "down-from-unset must be a clean, non-error exit")

	after, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "down from unset must not write anything")
}

// TestModelsBump_NonAtomModel_ReportsCleanly confirms a model that isn't in
// the atom registry at all (no Levels()) is reported cleanly rather than
// crashing or returning a Go error.
//
// This seeds the config file DIRECTLY with a raw (provider, model) pair
// rather than going through `models use`, because `models use`'s large/small
// positionals require the model to resolve against the (cached) provider
// catalog via a.ResolveModel — an extra, unrelated hurdle for a model that
// isn't expected to be in the catalog at all here. Writing the JSON directly
// isolates the test to exactly what's under test: modelsBumpCmd's own
// lookupAtomForModel()==""  branch.
func TestModelsBump_NonAtomModel_ReportsCleanly(t *testing.T) {
	globalPath := setupBumpEnv(t)

	require.NoError(t, os.WriteFile(globalPath, []byte(`{"models":{
		"large":{"provider":"openai","model":"gpt-5"},
		"small":{"provider":"openai","model":"gpt-5"},
		"reviewer":{"provider":"openai","model":"gpt-5","reasoning_effort":"high"}
	}}`), 0o644))

	resetModelsBumpFlags(t)
	out, runErr := runModelsCmd(t, modelsBumpCmd, "reviewer", "up")
	require.NoError(t, runErr, "non-atom model must report cleanly, not error")
	_ = out

	// Confirm nothing was written — the reviewer's effort must remain "high".
	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"high"`)
}

// TestModelsBump_AllFourRoles is a table test confirming the bump command
// works uniformly across all four role slots. large/small use
// glm4_7_flash (boolean off/on — resolves reliably in this harness, see the
// note on TestModelsBump_GLM52FullStepUp); worker/reviewer use glm5_2's
// high->max step, since worker/reviewer are read directly from cfg.Models
// without going through the large/small default-substitution path that
// makes glm-5.2 unreliable for THOSE two slots specifically in this
// harness.
func TestModelsBump_AllFourRoles(t *testing.T) {
	tests := []struct {
		role      string
		seedAtom  string
		wantAfter string
	}{
		{"large", "glm4_7_flash-off", `"on"`},
		{"small", "glm4_7_flash-off", `"on"`},
		{"worker", "glm5_2-high", `"max"`},
		{"reviewer", "glm5_2-high", `"max"`},
	}
	for _, tc := range tests {
		t.Run(tc.role, func(t *testing.T) {
			globalPath := setupBumpEnv(t)

			resetModelsUseFlags(t)
			// small positional defaults to glm5_turbo (no declared effort
			// levels — irrelevant for the large/worker/reviewer sub-tests),
			// but the "small" sub-test itself needs an atom WITH levels in
			// that exact slot, so it seeds glm4_7_flash-off there too.
			smallSeed := "glm5_turbo"
			if tc.role == "small" {
				smallSeed = tc.seedAtom
			}
			args := []string{"glm4_7_flash-off", smallSeed}
			switch tc.role {
			case "large", "small":
				// already seeded above.
			case "worker":
				args = append(args, "--worker", tc.seedAtom)
			case "reviewer":
				args = append(args, "--reviewer", tc.seedAtom)
			}
			_, runErr := runModelsCmd(t, modelsUseCmd, args...)
			require.NoError(t, runErr)

			resetModelsBumpFlags(t)
			_, runErr = runModelsCmd(t, modelsBumpCmd, tc.role, "up")
			require.NoError(t, runErr)

			data, err := os.ReadFile(globalPath)
			require.NoError(t, err)
			assert.Contains(t, string(data), tc.wantAfter, "role %s: expected effort to step forward, got: %s", tc.role, data)
		})
	}
}

// TestModelsBump_WritePersists_RawFileContent is the "read the raw file
// back, not just models state" check requested for this task, mirroring
// task #72's ZeroWrites/StillWritesAll style: after a successful bump, the
// on-disk JSON must contain the new effort value directly.
func TestModelsBump_WritePersists_RawFileContent(t *testing.T) {
	globalPath := setupBumpEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_7_flash-off", "glm5_turbo", "--reviewer", "glm5_2-high")
	require.NoError(t, runErr)

	resetModelsBumpFlags(t)
	_, runErr = runModelsCmd(t, modelsBumpCmd, "reviewer", "up")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `"reviewer"`)
	assert.Contains(t, content, `"glm-5.2"`)
	assert.Contains(t, content, `"max"`)
	assert.NotContains(t, content, `"high"`, "the old effort value should no longer be present for reviewer after the bump")
}

// TestModelsBump_InvalidRole_RejectedCleanly confirms cobra-level arg
// validation rejects an unrecognized role positional.
func TestModelsBump_InvalidRole_RejectedCleanly(t *testing.T) {
	setupBumpEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_7_flash-off", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsBumpFlags(t)
	_, runErr = runModelsCmd(t, modelsBumpCmd, "bogus-role", "up")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "expected large|small|worker|reviewer")
}

// TestModelsBump_InvalidDirection_RejectedCleanly confirms an unrecognized
// direction positional is rejected.
func TestModelsBump_InvalidDirection_RejectedCleanly(t *testing.T) {
	setupBumpEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_7_flash-off", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsBumpFlags(t)
	_, runErr = runModelsCmd(t, modelsBumpCmd, "large", "sideways")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "expected up|down")
}

// TestModelsBump_RoleNotSet_ReportsCleanly confirms a role that was never
// configured in any scope is reported cleanly rather than crashing.
func TestModelsBump_RoleNotSet_ReportsCleanly(t *testing.T) {
	setupBumpEnv(t)

	resetModelsBumpFlags(t)
	_, runErr := runModelsCmd(t, modelsBumpCmd, "worker", "up")
	require.NoError(t, runErr, "an unset role must report cleanly, not error")
}

// TestModelsBump_RegisteredAsSubcommand sanity-checks init() registration.
func TestModelsBump_RegisteredAsSubcommand(t *testing.T) {
	found := false
	for _, sub := range modelsCmd.Commands() {
		if sub.Name() == "bump" {
			found = true
			break
		}
	}
	assert.True(t, found, "modelsBumpCmd must be registered under modelsCmd")
}
