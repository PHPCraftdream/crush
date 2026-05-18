package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// captureStderr runs f while capturing os.Stderr output.
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
// claude-init tests — new behaviour (batch 22):
//   * Never writes a block into CLAUDE.md.
//   * Strips any legacy crush-claude-init block on invocation.
//   * Removes CLAUDE.md if stripping leaves it empty.
//   * Always installs/refreshes the slash-command file.
// ---------------------------------------------------------------------------

func TestClaudeInit_NoCLAUDEMd_StillInstallsSlashCommand(t *testing.T) {
	dir := t.TempDir()

	runClaudeInitInDir(t, dir)

	// CLAUDE.md is NOT created.
	_, err := os.Stat(filepath.Join(dir, claudeMdFile))
	assert.True(t, os.IsNotExist(err), "claude-init must not create CLAUDE.md when it didn't exist")

	// Slash command IS installed.
	slashPath := filepath.Join(dir, ".claude", "commands", "crush.md")
	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "crush run")
	assert.Contains(t, got, "--role smart")
}

func TestClaudeInit_StripsLegacyBlock_KeepsRestOfFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	content := "# My project\n\n" +
		"<!-- crush-claude-init:v8 -->\nold delegation block\n<!-- /crush-claude-init -->\n\n" +
		"## Other notes\n\nlive content\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeInitInDir(t, dir)
	})

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(bts)
	assert.NotContains(t, got, "crush-claude-init", "legacy block must be stripped")
	assert.NotContains(t, got, "old delegation block")
	assert.Contains(t, got, "# My project", "user content above the block must survive")
	assert.Contains(t, got, "## Other notes", "user content below the block must survive")
	assert.Contains(t, got, "live content")
	assert.Contains(t, stderr, "stripped 1 legacy crush-claude-init block")
}

func TestClaudeInit_StripsLegacyBlock_DeletesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	// CLAUDE.md contains ONLY our legacy block.
	content := "<!-- crush-claude-init:v8 -->\nold delegation block\n<!-- /crush-claude-init -->\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeInitInDir(t, dir)
	})

	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file should be deleted when only our legacy block was present")
	assert.Contains(t, stderr, "removed now-empty")
}

func TestClaudeInit_NoLegacyBlock_LeavesFileAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	original := "# My project\n\nNo crush block here.\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	runClaudeInitInDir(t, dir)

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, string(bts), "CLAUDE.md without our block must not be touched")
}

func TestClaudeInit_StripsMultipleLegacyVersions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	content := "# Project\n\n" +
		"<!-- crush-claude-init:v6 -->\nv6 block\n<!-- /crush-claude-init -->\n\n" +
		"middle text\n\n" +
		"<!-- crush-claude-init:v8 -->\nv8 block\n<!-- /crush-claude-init -->\n\n" +
		"tail text\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeInitInDir(t, dir)
	})

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(bts)
	assert.NotContains(t, got, "crush-claude-init")
	assert.NotContains(t, got, "v6 block")
	assert.NotContains(t, got, "v8 block")
	assert.Contains(t, got, "# Project")
	assert.Contains(t, got, "middle text")
	assert.Contains(t, got, "tail text")
	assert.Contains(t, stderr, "stripped 2 legacy")
}

func TestClaudeInit_CreatesSlashCommand(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)

	slashPath := filepath.Join(dir, ".claude", "commands", "crush.md")
	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "crush run")
	assert.Contains(t, got, "--role smart")
}

func TestClaudeInit_SlashCommandOverwritesWithSentinel(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)
	slashPath := filepath.Join(dir, ".claude", "commands", "crush.md")
	first, err := os.ReadFile(slashPath)
	require.NoError(t, err)

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
	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's file", string(bts))
}

// ---------------------------------------------------------------------------
// claude-del tests (unchanged — claude_del.go logic is unchanged in batch 22)
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

	runClaudeDelInDir(t, dir)
	first, err := os.ReadFile(path)
	require.NoError(t, err)

	stderr := captureStderr(t, func() {
		runClaudeDelInDir(t, dir)
	})

	second, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second), "second run must not change the file")
	assert.Contains(t, stderr, "no crush-claude-init block found")
}
