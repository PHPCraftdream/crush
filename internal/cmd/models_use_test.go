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

// TestModelsUse_RawZAIEffort_ValidSucceeds covers the new validated raw
// "provider/model@effort" path for a Z.AI atom: a level that IS one of the
// real declared levels (off/on, for a boolean-thinking-only model like
// glm-4.7-flash) must still be accepted and written.
//
// Uses glm-4.7-flash rather than glm-5.2 here because this isolated test
// harness's cached provider catalog (CRUSH_PROVIDER_CACHE_ONLY=1, shared
// ambient cache under the user's data dir) resolves "zai/glm-4.7-flash"
// reliably via a.ResolveModel but not every zai model id — an environmental
// quirk of the shared cache, unrelated to this task's validation logic
// (which is exercised precisely, atom-by-atom, by the
// TestValidateEffortForModel_* unit tests below instead).
func TestModelsUse_RawZAIEffort_ValidSucceeds(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

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
	isolatedModelsEnv(t)

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
	isolatedModelsEnv(t)

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
