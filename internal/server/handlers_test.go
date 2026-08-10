package server

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// isolateAllGlobalConfigPaths redirects every environment variable that
// config.GlobalConfig()/config.GlobalConfigData() (and the XDG fallbacks
// they defer to) can resolve through, so config.Init below can never read
// or merge the operator's real ~/.config/crush/crush.json (live API keys,
// MCP server definitions) nor the real global data-dir crush.json into the
// App under test — see internal/cmd/providers_test.go's
// runProvidersCmdInIsolatedApp for the concrete failure mode this guards
// against (a leaked real MCP config made app.New's mcp.Initialize hang on a
// real network connection for 9+ minutes inside a test).
//
// This is a package-local twin of internal/config's and internal/agent's
// identically-named helpers: the logic must stay identical, but it can't be
// shared as a single file because these are three separate Go packages and
// all three copies are test-only (no non-test importer to hang a shared
// helper off of).
func isolateAllGlobalConfigPaths(t *testing.T) {
	t.Helper()

	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "global-config")
	dataDir := filepath.Join(tmp, "global-data")
	skillsDir := filepath.Join(tmp, "global-skills")

	t.Setenv("CRUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("CRUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)

	// Without this, provider discovery makes a real network call to Catwalk
	// (and Hyper) the first time it runs against a fresh, cache-empty
	// isolated data dir — see internal/cmd/providers_test.go's identical
	// use of this env var for the same reason.
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")

	// If a local CLI (claude/gemini/...) happens to be on this machine's
	// PATH, config.Load auto-synthesizes a "local-cli" provider, which
	// makes cfg.IsConfigured() true and drives app.New into
	// InitCoderAgent -> NewCoordinator -> discoverSkills. Without this
	// override that reads the OPERATOR's real
	// ~/.config/crush/skills / ~/.claude/skills, making the test
	// non-deterministic across machines and leaking real filesystem state
	// into a test that has nothing to do with skills.
	t.Setenv("CRUSH_SKILLS_DIR", skillsDir)
}

// newAttachmentsTestApp builds a real *appPkg.App over a config.Init'd temp
// workingDir/dataDir pair, isolated from the host's real global config.
// dataDir may be "" to exercise config's own default-resolution path (see
// setDefaults); the resolved directory (store.Config().Options.DataDirectory)
// is what actually gets used for the DB connection, not the raw parameter.
func newAttachmentsTestApp(t *testing.T, workingDir, dataDir string) *appPkg.App {
	t.Helper()
	isolateAllGlobalConfigPaths(t)

	store, err := config.Init(workingDir, dataDir, false)
	require.NoError(t, err)
	resolvedDataDir := store.Config().Options.DataDirectory
	require.NotEmpty(t, resolvedDataDir)
	// Mirrors setupApp's createDotCrushDir (internal/cmd/root.go): db.Connect
	// expects the data directory to already exist.
	require.NoError(t, os.MkdirAll(resolvedDataDir, 0o700))

	conn, err := db.Connect(t.Context(), resolvedDataDir)
	require.NoError(t, err)
	// ReleaseAll, not Release: App.Shutdown's forced-shutdown path (taken
	// whenever an agent doesn't finish within its grace period — several
	// tests using this helper deliberately install a coordinator that never
	// stops being busy, to test fails-closed behavior) intentionally skips
	// calling Release at all, so a single paired Release call here can leave
	// the pool entry's refCount above zero forever once app.New's own
	// internal Connect/ConnectRead calls are counted — silently leaking the
	// DB file handle for the rest of this test binary's life and breaking
	// t.TempDir()'s own cleanup on Windows (found investigating a real,
	// consistently-reproducing pre-push CI failure). ReleaseAll guarantees
	// this dataDir's connection is fully closed regardless of how Shutdown
	// behaved — see its own doc for why this is safe under t.Parallel().
	t.Cleanup(func() { _ = db.ReleaseAll(resolvedDataDir) })

	a, err := appPkg.New(t.Context(), conn, store)
	require.NoError(t, err)
	t.Cleanup(a.Shutdown)

	return a
}

// TestSaveAttachmentToDisk_UsesDataDirNotWorkingDir verifies that
// saveAttachmentToDisk writes under <dataDir>/attachments/ using the
// dataDir argument as-is (no extra ".crush" segment appended), and does NOT
// fall back to any working-directory-derived path. This guards against
// regressing to the old cwd-hardcoded behavior (task #248 / attachments dir
// bug), where attachments always landed under
// "<workingDir>/.crush/attachments" even when a different data_directory
// (or --data-dir) was configured.
func TestSaveAttachmentToDisk_UsesDataDirNotWorkingDir(t *testing.T) {
	dataDir := t.TempDir()
	workingDir := t.TempDir()
	require.NotEqual(t, dataDir, workingDir)

	path, err := saveAttachmentToDisk(dataDir, "notes.txt", []byte("hello"))
	require.NoError(t, err)

	// The file must live under dataDir/attachments/, not dataDir itself and
	// not anywhere derived from workingDir.
	rel, err := filepath.Rel(dataDir, path)
	require.NoError(t, err)
	require.False(t, filepath.IsAbs(rel))
	require.NotContains(t, rel, "..")

	dir := filepath.Dir(path)
	require.Equal(t, filepath.Join(dataDir, "attachments"), dir)

	// Sanity: nothing was written under workingDir at all.
	entries, err := os.ReadDir(workingDir)
	require.NoError(t, err)
	require.Empty(t, entries)

	// The file actually exists with the expected contents.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))
}

// TestSaveAttachmentToDisk_EmptyDataDirErrors verifies the nil-config edge
// case: saveAttachmentToDisk itself refuses to silently fall back to cwd
// when handed an empty dataDir. Callers (see attachmentsDataDir) are
// responsible for supplying a defensive fallback before calling in.
func TestSaveAttachmentToDisk_EmptyDataDirErrors(t *testing.T) {
	_, err := saveAttachmentToDisk("", "notes.txt", []byte("hello"))
	require.Error(t, err)
}

// TestSaveAttachmentToDisk_SameNameSameSecondDoesNotCollide is the
// regression test for task #274: the filename used to be built from a
// timestamp with ONE-SECOND precision plus filepath.Base(fileName) alone.
// Two uploads of the same-named file within the same second produced the
// identical path, so the second os.WriteFile call silently overwrote the
// first upload's content with no error and no signal to the caller.
func TestSaveAttachmentToDisk_SameNameSameSecondDoesNotCollide(t *testing.T) {
	dataDir := t.TempDir()

	const n = 20
	paths := make([]string, n)
	for i := range n {
		path, err := saveAttachmentToDisk(dataDir, "report.txt", []byte(fmt.Sprintf("upload-%d", i)))
		require.NoError(t, err)
		paths[i] = path
	}

	seen := make(map[string]bool, n)
	for _, p := range paths {
		require.False(t, seen[p], "duplicate path %q — two uploads collided onto the same file", p)
		seen[p] = true
	}

	// Every upload's own content must still be intact under its own path —
	// not merely distinct paths, but that none of the writes clobbered
	// another.
	for i, p := range paths {
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("upload-%d", i), string(data))
	}
}

// TestAttachmentsDataDir_UsesConfiguredDataDirectory is the regression test
// for task #262 (Round 8 review, LOW-1): the previously-existing tests only
// covered saveAttachmentToDisk, a pure path-join function — none of them
// exercised attachmentsDataDir itself or its three call sites in
// handleSendMessage/handleInterruptAndSend/handleInjectMessage. As a result,
// reverting those call sites back to
// saveAttachmentToDisk(a.Store().WorkingDir(), ...) (the pre-#248 bug) left
// every existing test green.
//
// This test builds a real *appPkg.App over a config-initialized dataDir
// that is deliberately DIFFERENT from <workingDir>/.crush, then asserts
// attachmentsDataDir(a) resolves to the configured dataDir — not a
// workingDir-derived path — and that saveAttachmentToDisk driven with that
// result actually lands the file under the configured dir.
func TestAttachmentsDataDir_UsesConfiguredDataDirectory(t *testing.T) {
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	require.NotEqual(t, filepath.Join(workingDir, ".crush"), dataDir)

	a := newAttachmentsTestApp(t, workingDir, dataDir)

	got := attachmentsDataDir(a)
	require.Equal(t, dataDir, got,
		"attachmentsDataDir must resolve to the configured data_directory, not <workingDir>/.crush")

	path, err := saveAttachmentToDisk(got, "notes.txt", []byte("hello"))
	require.NoError(t, err)
	rel, err := filepath.Rel(dataDir, path)
	require.NoError(t, err)
	require.False(t, filepath.IsAbs(rel))
	require.NotContains(t, rel, "..")

	// The historical bug's location: nothing must land under
	// <workingDir>/.crush/attachments.
	_, statErr := os.Stat(filepath.Join(workingDir, ".crush", "attachments"))
	require.True(t, os.IsNotExist(statErr), "no attachments dir should exist under workingDir/.crush")
}

// TestAttachmentsDataDir_DefaultsToWorkingDirCrushWhenUnconfigured is the
// companion case: with no explicit data_directory passed to config.Init
// (empty string), config's own setDefaults falls back to
// "<workingDir>/.crush" — the same value attachmentsDataDir's own
// nil-config defensive fallback would produce. This pins down that the
// "normal" (no override) path still behaves exactly as before #248.
func TestAttachmentsDataDir_DefaultsToWorkingDirCrushWhenUnconfigured(t *testing.T) {
	workingDir := t.TempDir()

	a := newAttachmentsTestApp(t, workingDir, "")

	want := filepath.Join(workingDir, ".crush")
	require.Equal(t, want, attachmentsDataDir(a))
}
