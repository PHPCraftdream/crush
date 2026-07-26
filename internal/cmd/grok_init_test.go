package cmd

import (
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

func runGrokInitInDir(t *testing.T, dir string) {
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
	require.NoError(t, grokInitCmd.RunE(cmd, nil))
}

func runGrokDelInDir(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, runGrokDel(dir))
}

// ---------------------------------------------------------------------------
// grok-init tests
// ---------------------------------------------------------------------------

func TestGrokInit_CreatesSlashCommand(t *testing.T) {
	dir := t.TempDir()
	runGrokInitInDir(t, dir)

	skillPath := filepath.Join(dir, ".grok", "skills", "crush", "SKILL.md")
	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "crush run")
	assert.Contains(t, got, "--role smart")
	assert.Contains(t, got, "name: crush")
}

func TestGrokInit_CreatesFallbackSkill(t *testing.T) {
	dir := t.TempDir()
	runGrokInitInDir(t, dir)

	skillPath := filepath.Join(dir, ".grok", "skills", "crush-fallback", "SKILL.md")
	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "CronCreate")
	assert.Contains(t, got, "TaskCreate")
	assert.Contains(t, got, "name: crush-fallback")
}

func TestGrokInit_SlashCommandOverwritesWithSentinel(t *testing.T) {
	dir := t.TempDir()
	runGrokInitInDir(t, dir)
	skillPath := filepath.Join(dir, ".grok", "skills", "crush", "SKILL.md")
	first, err := os.ReadFile(skillPath)
	require.NoError(t, err)

	runGrokInitInDir(t, dir)
	second, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
}

func TestGrokInit_SlashCommandSkipsWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, ".grok", "skills", "crush", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o755))
	require.NoError(t, os.WriteFile(skillPath, []byte("someone else's file"), 0o644))

	stderr := captureStderr(t, func() {
		runGrokInitInDir(t, dir)
	})

	assert.Contains(t, stderr, "does not contain our sentinel")
	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's file", string(bts))
}

// ---------------------------------------------------------------------------
// grok-del tests
// ---------------------------------------------------------------------------

func TestGrokDel_RemovesSlashCommandWithSentinel(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, ".grok", "skills", "crush", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o755))
	require.NoError(t, os.WriteFile(skillPath, []byte("<!-- crush-slash-command:v1 -->\nsome content\n"), 0o644))

	runGrokDelInDir(t, dir)

	_, err := os.Stat(skillPath)
	assert.True(t, os.IsNotExist(err), "skill file should be removed when it has our sentinel")

	// The now-empty crush/ skill directory should be cleaned up too.
	_, err = os.Stat(filepath.Dir(skillPath))
	assert.True(t, os.IsNotExist(err), "now-empty skill directory should be removed")
}

func TestGrokDel_RefusesSlashCommandWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, ".grok", "skills", "crush", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o755))
	require.NoError(t, os.WriteFile(skillPath, []byte("not ours"), 0o644))

	stderr := captureStderr(t, func() {
		runGrokDelInDir(t, dir)
	})

	assert.Contains(t, stderr, "refusing to delete")
	assert.Contains(t, stderr, "missing sentinel")

	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assert.Equal(t, "not ours", string(bts))
}

func TestGrokDel_IdempotentOnSecondRun(t *testing.T) {
	dir := t.TempDir()
	runGrokInitInDir(t, dir)

	runGrokDelInDir(t, dir)
	stderr := captureStderr(t, func() {
		runGrokDelInDir(t, dir)
	})
	// Second run is a no-op: nothing left to remove, no errors raised.
	assert.NotContains(t, stderr, "refusing to delete")
}
