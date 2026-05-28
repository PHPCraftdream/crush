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

// Content-of-the-slash-command pin tests intentionally removed (2026-05):
// asserting that specific marketing-style phrases ("opt-in only",
// "zero trust", "never echo … verbatim") appear in the installed
// markdown is testing the documentation, not the code. The slash
// command is prose meant to instruct another LLM — its wording will
// drift as we learn what works, and brittle string asserts only mean
// every refinement also has to update a test file. The behavioural
// contract that DOES matter — file gets written, our sentinel marker
// is present (so claude-del can recognise it), $ARGUMENTS placeholder
// is present (so Claude Code's slash-command machinery can substitute
// the user's prompt) — is already covered by
// TestClaudeInit_CreatesSlashCommand /
// TestClaudeInit_SlashCommandOverwritesWithSentinel /
// TestClaudeInit_SlashCommandSkipsWithoutSentinel above. The
// claude_slash_command.md content review happens at code-review
// time, not in CI.

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

// ---------------------------------------------------------------------------
// claude-init agent tests (batch 29)
// ---------------------------------------------------------------------------

func TestClaudeInit_InstallsAgents(t *testing.T) {
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".claude", "agents")
	err := writeModelAgentsToDir(agentsDir)
	require.NoError(t, err)

	// Check a few representative files exist with correct content.
	for _, name := range []string{"ao47h", "ao47xx", "as46m", "ah45l", "aol", "asl", "ahh"} {
		path := filepath.Join(agentsDir, name+".md")
		data, err := os.ReadFile(path)
		require.NoError(t, err, "agent %s should exist", name)
		content := string(data)
		assert.Contains(t, content, "claude-", "agent %s should contain model name", name)
		assert.Contains(t, content, claudeModelAgentSentinel, "agent %s should contain sentinel", name)
		assert.Contains(t, content, "$ARGUMENTS", "agent %s should contain $ARGUMENTS", name)
		assert.Contains(t, content, "name: "+name, "agent %s should have name frontmatter", name)
	}
}

func TestClaudeInit_AgentFrontmatter(t *testing.T) {
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".claude", "agents")
	require.NoError(t, writeModelAgentsToDir(agentsDir))

	// Check o47h has correct frontmatter fields.
	path := filepath.Join(agentsDir, "ao47h.md")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, "name: ao47h")
	assert.Contains(t, content, "model: claude-opus-4-7")
	assert.Contains(t, content, "effort=high")
	assert.Contains(t, content, "You are a delegated worker invoked with reasoning effort: high")
	// Git-safety clause is part of the agent body for every model — pin
	// just the anchor phrase so future tweaks to the wording don't break
	// the test, but a missing clause does.
	assert.Contains(t, content, "Git safety", "agent should carry the shared-workspace git-safety clause")
}

func TestClaudeInit_AgentSkipsWithoutSentinel(t *testing.T) {
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".claude", "agents")
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))

	// Write a foreign file.
	foreignPath := filepath.Join(agentsDir, "ao47h.md")
	require.NoError(t, os.WriteFile(foreignPath, []byte("someone else's agent"), 0o644))

	stderr := captureStderr(t, func() {
		err := writeModelAgentsToDir(agentsDir)
		require.NoError(t, err)
	})
	assert.Contains(t, stderr, "not ours — skipping")

	// Foreign file untouched.
	data, err := os.ReadFile(foreignPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's agent", string(data))
}

func TestClaudeDel_RemovesAgents(t *testing.T) {
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".claude", "agents")
	require.NoError(t, writeModelAgentsToDir(agentsDir))
	require.NoError(t, removeModelAgentsFromDir(agentsDir))

	// Verify all agent files are gone.
	entries, err := os.ReadDir(agentsDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "agents directory should be empty after removal")
}

func TestClaudeDel_AgentRefusesWithoutSentinel(t *testing.T) {
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".claude", "agents")
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))

	// Write a foreign agent file.
	foreignPath := filepath.Join(agentsDir, "ao47h.md")
	require.NoError(t, os.WriteFile(foreignPath, []byte("not our agent"), 0o644))

	stderr := captureStderr(t, func() {
		err := removeModelAgentsFromDir(agentsDir)
		require.NoError(t, err)
	})
	assert.Contains(t, stderr, "refusing to delete")

	// Foreign file still there.
	data, err := os.ReadFile(foreignPath)
	require.NoError(t, err)
	assert.Equal(t, "not our agent", string(data))
}

func TestClaudeInit_InstallsBothCommandsAndAgents(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)

	cmdDir := filepath.Join(dir, ".claude", "commands")
	agentsDir := filepath.Join(dir, ".claude", "agents")

	// Verify slash-commands exist.
	for _, name := range []string{"o47h", "s46m", "hh"} {
		_, err := os.Stat(filepath.Join(cmdDir, name+".md"))
		require.NoError(t, err, "slash-command %s should exist", name)
	}

	// Verify agents exist.
	for _, name := range []string{"ao47h", "as46m", "ahh"} {
		_, err := os.Stat(filepath.Join(agentsDir, name+".md"))
		require.NoError(t, err, "agent %s should exist", name)
	}
}

func TestClaudeDel_RemovesBothCommandsAndAgents(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)

	// Verify files exist.
	cmdDir := filepath.Join(dir, ".claude", "commands")
	agentsDir := filepath.Join(dir, ".claude", "agents")
	_, err := os.Stat(filepath.Join(cmdDir, "o47h.md"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(agentsDir, "ao47h.md"))
	require.NoError(t, err)

	// Delete.
	runClaudeDelInDir(t, dir)

	// Verify both are gone.
	_, err = os.Stat(filepath.Join(cmdDir, "o47h.md"))
	assert.True(t, os.IsNotExist(err), "slash-command should be removed")
	_, err = os.Stat(filepath.Join(agentsDir, "ao47h.md"))
	assert.True(t, os.IsNotExist(err), "agent should be removed")
}
