package prompt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openai"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestDedupeContextFiles_DropsIdenticalContent(t *testing.T) {
	files := []ContextFile{
		{Path: "AGENTS.md", Content: "Follow the style guide.\n"},
		{Path: "CLAUDE.md", Content: "Follow the style guide.\n"},
		{Path: "GEMINI.md", Content: "Follow the style guide.\n"},
	}

	got := dedupeContextFiles(files)

	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1; got = %+v", len(got), got)
	}
	if got[0].Path != "AGENTS.md" {
		t.Errorf("kept path = %q, want %q (first occurrence)", got[0].Path, "AGENTS.md")
	}
}

func TestDedupeContextFiles_KeepsDifferentContent(t *testing.T) {
	files := []ContextFile{
		{Path: "AGENTS.md", Content: "Use tabs.\n"},
		{Path: "CLAUDE.md", Content: "Use spaces.\n"},
	}

	got := dedupeContextFiles(files)

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2; got = %+v", len(got), got)
	}
}

func TestDedupeContextFiles_EmptyInput(t *testing.T) {
	got := dedupeContextFiles(nil)
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
}

func TestDedupeContextFiles_MixOfDuplicateAndUniqueContent(t *testing.T) {
	files := []ContextFile{
		{Path: "AGENTS.md", Content: "shared\n"},
		{Path: "notes.md", Content: "unique\n"},
		{Path: "CLAUDE.md", Content: "shared\n"},
		{Path: "GEMINI.md", Content: "shared\n"},
	}

	got := dedupeContextFiles(files)

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2; got = %+v", len(got), got)
	}
	paths := map[string]bool{got[0].Path: true, got[1].Path: true}
	if !paths["AGENTS.md"] || !paths["notes.md"] {
		t.Errorf("expected AGENTS.md and notes.md to survive, got paths %+v", got)
	}
}

func TestFlattenContextFiles_SortsByPathKeyDeterministically(t *testing.T) {
	byPath := map[string][]ContextFile{
		"z.md": {{Path: "z.md", Content: "z"}},
		"a.md": {{Path: "a.md", Content: "a"}},
		"m.md": {{Path: "m.md", Content: "m"}},
	}

	got := flattenContextFiles(byPath)

	want := []string{"a.md", "m.md", "z.md"}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Path != w {
			t.Errorf("got[%d].Path = %q, want %q", i, got[i].Path, w)
		}
	}
}

func TestFlattenContextFiles_PreservesMultipleFilesPerPathKey(t *testing.T) {
	byPath := map[string][]ContextFile{
		".cursor/rules/": {
			{Path: ".cursor/rules/a.md", Content: "a"},
			{Path: ".cursor/rules/b.md", Content: "b"},
		},
	}

	got := flattenContextFiles(byPath)

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2; got = %+v", len(got), got)
	}
}

// --- Orchestrator-block tests (WorkerAvailable / WorkerContextWindowText) ---
//
// These exercise coder.md.tpl's conditional "Orchestrator mode" block through
// the real Prompt.Build path (not a hand-rolled template string), because
// the backward-compat guarantee that matters is about the actual rendered
// coder prompt, not about promptData in isolation.

// newTestCoderPrompt builds a *Prompt using the real embedded coder.md.tpl
// via a minimal copy of coordinator.coderPrompt's construction (that
// function lives in package agent, which cannot be imported here without an
// import cycle, so the template is embedded directly).
func newTestCoderPrompt(t *testing.T, workingDir string) *Prompt {
	t.Helper()
	tplPath := filepath.Join("..", "templates", "coder.md.tpl")
	tpl, err := os.ReadFile(tplPath)
	require.NoError(t, err)
	p, err := NewPrompt("coder", string(tpl), WithWorkingDir(workingDir))
	require.NoError(t, err)
	return p
}

// registerProvider registers a bare-bones offline-safe provider (building an
// openai.Provider only constructs a client, never a network call) with a
// single model, and returns the SelectedModel pointing at it. Mirrors
// newRoleModelTestCoordinator's helper in internal/agent/coordinator_test.go.
func registerProvider(cfg *config.Config, providerID, modelID string, contextWindow int64) config.SelectedModel {
	cfg.Providers.Set(providerID, config.ProviderConfig{
		ID:   providerID,
		Type: openai.Name,
		Models: []catwalk.Model{
			{ID: modelID, ContextWindow: contextWindow},
		},
	})
	return config.SelectedModel{Provider: providerID, Model: modelID}
}

// isolateAllGlobalConfigPaths redirects every environment variable that
// config.GlobalConfig()/config.GlobalConfigData() (and the XDG fallbacks
// they defer to) can resolve through, plus skill discovery's own
// CRUSH_SKILLS_DIR, so config.Init below can never read the operator's real
// ~/.config/crush/crush.json nor ~/.claude/skills into a test. Without the
// skills isolation specifically, a test asserting the rendered coder prompt
// does NOT mention "delegat" (TestBuild_WorkerNotConfigured_PromptByteIdentical)
// fails or flakes depending on what skills happen to be installed on the
// machine running the test — several real skill descriptions contain that
// substring (task #324, found by @oh's review of the #319/#321 round).
//
// This is a package-local twin of internal/config's, internal/agent's, and
// internal/server's identically-named helpers: the logic must stay
// identical, but it can't be shared as a single file because these are
// separate Go packages and all copies are test-only.
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
	t.Setenv("CRUSH_SKILLS_DIR", skillsDir)

	// Without this, provider discovery makes a real network call to Catwalk
	// (and Hyper) the first time it runs against a fresh, cache-empty
	// isolated data dir — see internal/cmd/providers_test.go's and
	// internal/server/handlers_test.go's identical use of this env var for
	// the same reason.
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")
}

func testConfigStore(t *testing.T) *config.ConfigStore {
	t.Helper()
	isolateAllGlobalConfigPaths(t)
	workingDir := t.TempDir()
	store, err := config.Init(workingDir, "", false)
	require.NoError(t, err)
	return store
}

// TestBuild_WorkerNotConfigured_PromptByteIdentical is the strongest form of
// the backward-compat guarantee: when workerActive is false (worker not
// configured, or run isn't smart), the rendered coder prompt is byte
// identical to a render with WorkerAvailable never having existed as a
// concept — i.e. the orchestrator block is entirely absent, not just empty.
func TestBuild_WorkerNotConfigured_PromptByteIdentical(t *testing.T) {
	store := testConfigStore(t)
	p := newTestCoderPrompt(t, store.WorkingDir())

	got, err := p.Build(context.Background(), "large-provider", "large-model", store, false)
	require.NoError(t, err)

	if strings.Contains(got, "Orchestrator mode") {
		t.Fatalf("worker not configured: rendered prompt must not contain the orchestrator block, got:\n%s", got)
	}
	if strings.Contains(got, "delegat") {
		t.Fatalf("worker not configured: rendered prompt must not mention delegation, got:\n%s", got)
	}

	// Re-render with the exact same inputs and confirm determinism: two
	// workerActive=false builds must match exactly, byte for byte.
	got2, err := p.Build(context.Background(), "large-provider", "large-model", store, false)
	require.NoError(t, err)
	require.Equal(t, got, got2, "rendering twice with workerActive=false must be byte-identical")
}

// TestBuild_WorkerConfiguredAndSmart_BlockPresent checks the block appears
// and carries the two required instructions: delegate hands-on work to the
// agent tool, and chunk the work to fit the worker's context window.
func TestBuild_WorkerConfiguredAndSmart_BlockPresent(t *testing.T) {
	store := testConfigStore(t)
	cfg := store.Config()
	cfg.Models[config.SelectedModelTypeWorker] = registerProvider(cfg, "worker-provider", "worker-model", 200_000)

	p := newTestCoderPrompt(t, store.WorkingDir())

	got, err := p.Build(context.Background(), "large-provider", "large-model", store, true)
	require.NoError(t, err)

	require.Contains(t, got, "Orchestrator mode", "block must be present when worker is configured and run is smart")
	require.Contains(t, got, "`agent` tool", "must instruct delegating hands-on work to the agent tool")
	require.Contains(t, got, "Chunk the work", "must instruct chunking work to fit the worker's context window")
	require.Contains(t, got, "resume_session_id", "should briefly mention resuming a paused worker")
	require.Contains(t, got, "CLAIM, not a receipt", "must tell the orchestrator to treat worker reports as unverified claims")
	require.Contains(t, got, "re-read the file", "must instruct re-checking the actual change rather than trusting the worker's summary")
	require.Contains(t, got, "re-run the test/command", "must instruct re-running tests/commands personally rather than trusting a 'tests pass' claim")
}

// TestBuild_WorkerConfiguredWithKnownContextWindow_NumberMatches checks the
// rendered context-window figure matches the configured worker model's
// actual catwalk ContextWindow, formatted for readability (e.g. "200k
// tokens" rather than "200000").
func TestBuild_WorkerConfiguredWithKnownContextWindow_NumberMatches(t *testing.T) {
	store := testConfigStore(t)
	cfg := store.Config()
	cfg.Models[config.SelectedModelTypeWorker] = registerProvider(cfg, "worker-provider", "worker-model", 200_000)

	p := newTestCoderPrompt(t, store.WorkingDir())

	got, err := p.Build(context.Background(), "large-provider", "large-model", store, true)
	require.NoError(t, err)

	require.Contains(t, got, "200k tokens", "rendered context window must reflect the configured worker model's actual ContextWindow (200_000)")
}

// TestBuild_WorkerConfiguredWithUnknownContextWindow_NoBogusNumber is the
// CLI-provider case: a Worker model is configured, but config.GetModel
// returns nil or a zero ContextWindow for it (e.g. the cliprovider binary
// wasn't found on PATH at config-load time, so no catwalk entry was ever
// synthesized for it). The block must still appear with the chunking
// guidance, but must never render a fabricated token figure.
func TestBuild_WorkerConfiguredWithUnknownContextWindow_NoBogusNumber(t *testing.T) {
	store := testConfigStore(t)
	cfg := store.Config()
	// ContextWindow left at zero value deliberately: simulates a worker
	// model with no catwalk entry / unknown size (e.g. CLI provider whose
	// binary wasn't on PATH at load time).
	cfg.Models[config.SelectedModelTypeWorker] = registerProvider(cfg, "worker-provider", "worker-model", 0)

	p := newTestCoderPrompt(t, store.WorkingDir())

	got, err := p.Build(context.Background(), "large-provider", "large-model", store, true)
	require.NoError(t, err)

	require.Contains(t, got, "Orchestrator mode", "block must still be present even when the context window is unknown")
	require.Contains(t, got, "Chunk the work", "chunking guidance must still be present without a number")
	require.NotContains(t, got, "tokens)", "must not render any '(~N tokens)' figure when the context window is unknown/zero")
	require.NotRegexp(t, `\(~[^)]*\d[^)]*\)`, got, "must not render any parenthesized number for the context window when unknown")
}

// TestBuild_IOContract_ExemptsDiagnosisFromLineLimit is a prompt-regression
// guard for review finding P3.9: the "under 4 lines of prose" rule must
// carry an explicit, visible exemption for diagnosis/security-review/
// handoff turns, not just an implicit one buried in a different rule — a
// model under pressure to stay terse will otherwise compress away the
// evidence rule 6 requires. This does not depend on WorkerAvailable.
func TestBuild_IOContract_ExemptsDiagnosisFromLineLimit(t *testing.T) {
	store := testConfigStore(t)
	p := newTestCoderPrompt(t, store.WorkingDir())

	got, err := p.Build(context.Background(), "large-provider", "large-model", store, false)
	require.NoError(t, err)

	require.Contains(t, got, "4 lines of prose per turn", "the line-limit rule must still be present")
	require.Contains(t, got, "routine work", "the limit must be scoped to routine work, not every turn")
	require.Contains(t, got, "diagnosis, security findings, and complex handoffs are exempt",
		"the line-limit rule must explicitly exempt diagnosis/security/handoff turns, not rely on an implicit carve-out elsewhere")
}

// TestBuild_WorkerConfiguredAndSmart_VerifiesPerChunkNotJustAtEnd is a
// prompt-regression guard for review finding P3.9's second concern: an
// earlier draft of the orchestrator-mode rule deferred zero-trust
// verification to a single pass "not after each individual chunk", which
// lets errors from consecutive delegated chunks compound before anything
// catches them. The rule must instruct verifying at each chunk's boundary.
func TestBuild_WorkerConfiguredAndSmart_VerifiesPerChunkNotJustAtEnd(t *testing.T) {
	store := testConfigStore(t)
	cfg := store.Config()
	cfg.Models[config.SelectedModelTypeWorker] = registerProvider(cfg, "worker-provider", "worker-model", 200_000)

	p := newTestCoderPrompt(t, store.WorkingDir())

	got, err := p.Build(context.Background(), "large-provider", "large-model", store, true)
	require.NoError(t, err)

	require.Contains(t, got, "before counting any chunk done",
		"must instruct verifying each chunk before counting it done, not deferring to a final pass")
	require.Contains(t, got, "each chunk's boundary", "must instruct verifying at chunk boundaries")
	require.NotContains(t, got, "not after each individual chunk",
		"regression: must not instruct deferring verification away from individual chunks")
}

// TestBuild_WorkerConfiguredButModelUnregistered_NoBogusNumber covers the
// stricter case where GetModel returns nil outright (no catwalk entry at
// all for the configured worker provider/model), not just a zero
// ContextWindow on an existing entry.
func TestBuild_WorkerConfiguredButModelUnregistered_NoBogusNumber(t *testing.T) {
	store := testConfigStore(t)
	cfg := store.Config()
	// Point the Worker slot at a provider/model that was never registered
	// via cfg.Providers.Set — GetModel must return nil for it.
	cfg.Models[config.SelectedModelTypeWorker] = config.SelectedModel{
		Provider: "cli-provider",
		Model:    "cli-claude-sonnet",
	}
	require.Nil(t, cfg.GetModel("cli-provider", "cli-claude-sonnet"), "sanity check: model must be genuinely unregistered")

	p := newTestCoderPrompt(t, store.WorkingDir())

	got, err := p.Build(context.Background(), "large-provider", "large-model", store, true)
	require.NoError(t, err)

	require.Contains(t, got, "Orchestrator mode")
	require.Contains(t, got, "Chunk the work")
	require.NotRegexp(t, `\(~[^)]*\d[^)]*\)`, got, "must not render any parenthesized number for the context window when the model is unregistered")
}
