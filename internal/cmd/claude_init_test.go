package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// captureStderr runs f while capturing os.Stderr output. Returns the
// captured output as a string. Safe for concurrent use within a single
// test (restores the original stderr on return).
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w
	defer func() { os.Stderr = old }()

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = buf.ReadFrom(r)
		close(done)
	}()

	f()
	_ = w.Close()
	<-done
	return buf.String()
}

// runClaudeInitInDir invokes the claudeInitCmd against the given temp dir.
func runClaudeInitInDir(t *testing.T, dir string) {
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
	require.NoError(t, cmd.ParseFlags([]string{"--cwd", dir}))
	require.NoError(t, claudeInitCmd.RunE(cmd, nil))
}

func runClaudeDelInDir(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, runClaudeDel(dir))
}

// ---------------------------------------------------------------------------
// claude-init tests
// ---------------------------------------------------------------------------

func TestClaudeInitBlock_ShapeAndContent(t *testing.T) {
	block := claudeInitBlock()
	assert.Contains(t, block, claudeInitMarkerStart)
	assert.Contains(t, block, claudeInitMarkerEnd)
	for _, must := range []string{
		"--role",
		"--session",
		"--json",
		"auto-approved",
		"crush sessions list",
		"crush providers list",
		"crush models show",
		"`agent` tool",
		"fan out internally",
		"strategist, planner, reviewer",
		"What stays in your hand",
		"What goes to `crush` by default",
		"Tactical follow-up after review",
		"Markdown beats JSON",
	} {
		assert.Contains(t, block, must, "block must mention %q", must)
	}
}

func TestClaudeInit_CreatesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)

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

	runClaudeInitInDir(t, dir)

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(bts)
	assert.True(t, strings.HasPrefix(got, pre), "existing content must be preserved verbatim at the top")
	assert.Contains(t, got, claudeInitMarkerStart)
}

func TestClaudeInit_AlwaysReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	// First run creates.
	runClaudeInitInDir(t, dir)
	first, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(first), claudeInitMarkerStart))

	// Second run replaces — still exactly one block, same content.
	runClaudeInitInDir(t, dir)
	second, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second), "second run must produce the same file (replace is idempotent)")
	assert.Equal(t, 1, strings.Count(string(second), claudeInitMarkerStart))
}

func TestClaudeInit_ReplacesOldVersions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	stale := "# Project notes\n\n" +
		"<!-- crush-claude-init:v1 -->\nold v1 body line 1\nold v1 body line 2\n<!-- /crush-claude-init -->\n\n" +
		"## Section the user wrote\n\nsome text\n\n" +
		"<!-- crush-claude-init:v1 -->\nstray second copy\n<!-- /crush-claude-init -->\n"
	require.NoError(t, os.WriteFile(path, []byte(stale), 0o644))

	runClaudeInitInDir(t, dir)
	after, err := os.ReadFile(path)
	require.NoError(t, err)

	got := string(after)
	assert.NotContains(t, got, "<!-- crush-claude-init:v1 -->")
	assert.NotContains(t, got, "old v1 body line 1")
	assert.NotContains(t, got, "stray second copy")
	assert.Equal(t, 1, strings.Count(got, claudeInitMarkerStart))
	assert.Contains(t, got, "## Section the user wrote")
	assert.Contains(t, got, "# Project notes")
}

func TestClaudeInit_CreatesSlashCommand(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)

	slashPath := filepath.Join(dir, ".claude", "commands", "crush.md")
	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err, "slash command file must exist after claude-init")
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS", "must carry the Claude Code arg placeholder")
	assert.Contains(t, got, "crush run", "must instruct to invoke crush run")
	assert.Contains(t, got, "--role smart", "must spell out the smart-default rule")
}

func TestClaudeInit_SlashCommandOverwritesWithSentinel(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)
	slashPath := filepath.Join(dir, ".claude", "commands", "crush.md")
	first, err := os.ReadFile(slashPath)
	require.NoError(t, err)

	// Second run still overwrites (always-replace).
	runClaudeInitInDir(t, dir)
	second, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
}

func TestClaudeInit_SlashCommandSkipsWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	slashPath := filepath.Join(dir, ".claude", "commands", "crush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(slashPath), 0o755))
	require.NoError(t, os.WriteFile(slashPath, []byte("someone else's file"), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeInitInDir(t, dir)
	})

	assert.Contains(t, stderr, "does not contain our sentinel")

	// File should be untouched.
	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's file", string(bts))
}

// ---------------------------------------------------------------------------
// claude-del tests
// ---------------------------------------------------------------------------

func TestClaudeDel_RemovesBlockAndKeepsRestOfFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	content := "# Project notes\n\n" +
		"<!-- crush-claude-init:v8 -->\nsome block content\n<!-- /crush-claude-init -->\n\n" +
		"## User section\n\nsome text\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	runClaudeDelInDir(t, dir)

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(bts)
	assert.NotContains(t, got, "crush-claude-init")
	assert.NotContains(t, got, "some block content")
	assert.Contains(t, got, "# Project notes")
	assert.Contains(t, got, "## User section")
	assert.Contains(t, got, "some text")
}

func TestClaudeDel_RemovesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	content := "<!-- crush-claude-init:v8 -->\nsome block content\n<!-- /crush-claude-init -->\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeDelInDir(t, dir)
	})

	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file should be deleted when only our block was present")
	assert.Contains(t, stderr, "removed empty CLAUDE.md")
}

func TestClaudeDel_RemovesSlashCommandWithSentinel(t *testing.T) {
	dir := t.TempDir()
	slashPath := filepath.Join(dir, ".claude", "commands", "crush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(slashPath), 0o755))
	require.NoError(t, os.WriteFile(slashPath, []byte("<!-- crush-slash-command:v1 -->\nsome content\n"), 0o644))

	// Create a minimal CLAUDE.md so the del doesn't complain.
	claudeMdPath := filepath.Join(dir, claudeMdFile)
	require.NoError(t, os.WriteFile(claudeMdPath, []byte("# Notes\n"), 0o644))

	runClaudeDelInDir(t, dir)

	_, err := os.Stat(slashPath)
	assert.True(t, os.IsNotExist(err), "slash command file should be removed when it has our sentinel")
}

func TestClaudeDel_RefusesSlashCommandWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	slashPath := filepath.Join(dir, ".claude", "commands", "crush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(slashPath), 0o755))
	require.NoError(t, os.WriteFile(slashPath, []byte("not ours"), 0o644))

	claudeMdPath := filepath.Join(dir, claudeMdFile)
	require.NoError(t, os.WriteFile(claudeMdPath, []byte("# Notes\n"), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeDelInDir(t, dir)
	})

	assert.Contains(t, stderr, "refusing to delete")
	assert.Contains(t, stderr, "missing sentinel")

	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	assert.Equal(t, "not ours", string(bts))
}

func TestClaudeDel_NoOpWhenNothingPresent(t *testing.T) {
	dir := t.TempDir()

	stderr := captureStderr(t, func() {
		runClaudeDelInDir(t, dir)
	})

	assert.Contains(t, stderr, "no CLAUDE.md found")
}

func TestClaudeDel_IdempotentOnSecondRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	content := "# Project notes\n\n" +
		"<!-- crush-claude-init:v8 -->\nsome block content\n<!-- /crush-claude-init -->\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	// First run removes the block.
	runClaudeDelInDir(t, dir)
	first, err := os.ReadFile(path)
	require.NoError(t, err)

	// Second run is a no-op — same content, no error.
	stderr := captureStderr(t, func() {
		runClaudeDelInDir(t, dir)
	})

	second, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second), "second run must not change the file")
	assert.Contains(t, stderr, "no crush-claude-init block found")
}
