package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeInitBlock_ShapeAndContent(t *testing.T) {
	block := claudeInitBlock()
	assert.Contains(t, block, claudeInitMarkerStart)
	assert.Contains(t, block, claudeInitMarkerEnd)
	// Spot-check we cover the things that matter for an LLM following
	// CLAUDE.md — the rules around --role, --session, permissions, json.
	for _, must := range []string{
		"--role",
		"--session",
		"--json",
		"auto-approved",
		"crush sessions list",
		"crush providers list",
		"crush models show",
		"`agent` tool",                    // crush's own delegation primitive
		"fan out internally",              // v4 phrasing of sub-agent orchestration
		"strategist, planner, reviewer",   // v3 posture statement
		"What stays in your hand",         // v3 stays-in-hand list
		"What goes to `crush` by default", // v3 goes-to-crush list
		"Tactical follow-up after review", // v3 carve-out for small post-review fixes
	} {
		assert.Contains(t, block, must, "block must mention %q", must)
	}
}

// runClaudeInitInDir invokes the claudeInitCmd against the given temp dir
// without going through cobra's full root-level parser. We construct a
// minimal command with the same flag set so ResolveCwd works.
//
// Side-effect to be aware of: ResolveCwd calls os.Chdir(dir). On Windows
// that locks the directory so the test's TempDir cleanup can't RemoveAll
// it. We restore cwd as a t.Cleanup so the test framework can delete the
// temp dir afterwards.
func runClaudeInitInDir(t *testing.T, dir string, mode string) {
	t.Helper()
	orig, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := os.Chdir(orig); err != nil {
			t.Logf("restore cwd: %v", err)
		}
	})
	cmd := &cobra.Command{}
	cmd.Flags().StringP("cwd", "c", "", "")
	cmd.Flags().Bool("force", false, "")
	cmd.Flags().Bool("replace", false, "")
	args := []string{"--cwd", dir}
	if mode != "" {
		args = append(args, "--"+mode)
	}
	require.NoError(t, cmd.ParseFlags(args))
	require.NoError(t, claudeInitCmd.RunE(cmd, nil))
}

func TestClaudeInit_CreatesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir, "")

	bts, err := os.ReadFile(filepath.Join(dir, claudeMdFile))
	require.NoError(t, err)
	assert.Contains(t, string(bts), claudeInitMarkerStart)
	assert.Contains(t, string(bts), claudeInitMarkerEnd)
}

func TestClaudeInit_AppendsToExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)
	const pre = "# Existing project notes\n\nSome content.\n"
	require.NoError(t, os.WriteFile(path, []byte(pre), 0o644))

	runClaudeInitInDir(t, dir, "")

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(bts)
	assert.True(t, strings.HasPrefix(got, pre), "existing content must be preserved verbatim at the top")
	assert.Contains(t, got, claudeInitMarkerStart)
}

func TestClaudeInit_IdempotentWithoutForce(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir, "")
	first, err := os.ReadFile(filepath.Join(dir, claudeMdFile))
	require.NoError(t, err)

	runClaudeInitInDir(t, dir, "")
	second, err := os.ReadFile(filepath.Join(dir, claudeMdFile))
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second), "second run without --force must be a no-op")
	assert.Equal(t, 1, strings.Count(string(second), claudeInitMarkerStart))
}

func TestClaudeInit_ForceReappends(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir, "")
	runClaudeInitInDir(t, dir, "force")

	bts, err := os.ReadFile(filepath.Join(dir, claudeMdFile))
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(bts), claudeInitMarkerStart),
		"--force must duplicate the marker (callers know what they're doing)")
}

func TestClaudeInit_ReplaceSwapsCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	runClaudeInitInDir(t, dir, "")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(before), claudeInitMarkerStart))

	runClaudeInitInDir(t, dir, "replace")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	// Still exactly one current block (no duplicate, no leftover).
	assert.Equal(t, 1, strings.Count(string(after), claudeInitMarkerStart),
		"--replace must leave exactly one block of the current version")
	assert.Equal(t, 1, strings.Count(string(after), claudeInitMarkerEnd))
}

// TestClaudeInit_ReplaceStripsOldVersions verifies the safety hatch in
// action: a CLAUDE.md that has older-version blocks scattered in it (from
// when v1 was current) gets cleanly collapsed to one v2 block on
// `claude-init --replace`. The regex matches *any* "crush-claude-init:v\d+"
// sentinel, not just the current one, so versions can be bumped without
// abandoning copies in users' files.
func TestClaudeInit_ReplaceStripsOldVersions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	// Hand-craft a file with two stale blocks (v1) sandwiched around
	// some user content.
	stale := "# Project notes\n\n" +
		"<!-- crush-claude-init:v1 -->\nold v1 body line 1\nold v1 body line 2\n<!-- /crush-claude-init -->\n\n" +
		"## Section the user wrote\n\nsome text\n\n" +
		"<!-- crush-claude-init:v1 -->\nstray second copy\n<!-- /crush-claude-init -->\n"
	require.NoError(t, os.WriteFile(path, []byte(stale), 0o644))

	runClaudeInitInDir(t, dir, "replace")
	after, err := os.ReadFile(path)
	require.NoError(t, err)

	got := string(after)
	// No v1 should survive.
	assert.NotContains(t, got, "<!-- crush-claude-init:v1 -->",
		"--replace must strip every prior version, not just the current one")
	assert.NotContains(t, got, "old v1 body line 1")
	assert.NotContains(t, got, "stray second copy")
	// Exactly one fresh block of the current version.
	assert.Equal(t, 1, strings.Count(got, claudeInitMarkerStart))
	// User content is preserved.
	assert.Contains(t, got, "## Section the user wrote")
	assert.Contains(t, got, "# Project notes")
}

// TestClaudeInit_ForceAndReplaceMutuallyExclusive is a guard against a
// future bug where someone forgets the cmd.MarkFlagsMutuallyExclusive
// equivalent — they currently can't both be true because the RunE
// returns early on the combination.
// TestClaudeInit_CreatesSlashCommand verifies the .claude/commands/crush.md
// drop-in is written next to the CLAUDE.md update. The slash command file
// gives Claude Code users a "/crush <task>" trigger that points at the
// claude-init block.
func TestClaudeInit_CreatesSlashCommand(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir, "")

	slashPath := filepath.Join(dir, ".claude", "commands", "crush.md")
	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err, "slash command file must exist after claude-init")
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS", "must carry the Claude Code arg placeholder")
	assert.Contains(t, got, "crush run", "must instruct to invoke crush run")
	assert.Contains(t, got, "--role smart", "must spell out the smart-default rule")
}

func TestClaudeInit_SlashCommandIdempotent(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir, "")
	slashPath := filepath.Join(dir, ".claude", "commands", "crush.md")
	first, err := os.ReadFile(slashPath)
	require.NoError(t, err)

	runClaudeInitInDir(t, dir, "")
	second, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second), "second run must be a no-op for the slash command too")
}

func TestClaudeInit_SlashCommandReplaceRewrites(t *testing.T) {
	dir := t.TempDir()
	slashPath := filepath.Join(dir, ".claude", "commands", "crush.md")
	// Pre-seed with a stale fake. --replace must overwrite it.
	require.NoError(t, os.MkdirAll(filepath.Dir(slashPath), 0o755))
	require.NoError(t, os.WriteFile(slashPath, []byte("stale content, no sentinel"), 0o644))

	runClaudeInitInDir(t, dir, "replace")
	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	got := string(bts)
	assert.NotContains(t, got, "stale content", "--replace must overwrite, not preserve")
	assert.Contains(t, got, claudeSlashCommandSentinel)
}

func TestClaudeInit_ForceAndReplaceMutuallyExclusive(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(orig) })

	cmd := &cobra.Command{}
	cmd.Flags().StringP("cwd", "c", "", "")
	cmd.Flags().Bool("force", false, "")
	cmd.Flags().Bool("replace", false, "")
	require.NoError(t, cmd.ParseFlags([]string{"--cwd", dir, "--force", "--replace"}))
	err = claudeInitCmd.RunE(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}
