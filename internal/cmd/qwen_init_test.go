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

func runQwenInitInDir(t *testing.T, dir string) {
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
	require.NoError(t, qwenInitCmd.RunE(cmd, nil))
}

func runQwenDelInDir(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, runQwenDel(dir))
}

// ---------------------------------------------------------------------------
// qwen-init tests
// ---------------------------------------------------------------------------

func TestQwenInit_CreatesSlashCommand(t *testing.T) {
	dir := t.TempDir()
	runQwenInitInDir(t, dir)

	slashPath := filepath.Join(dir, ".qwen", "commands", "crush.md")
	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "{{args}}")
	assert.NotContains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "description:")
	assert.Contains(t, got, "crush run")
	assert.Contains(t, got, "--role smart")
}

func TestQwenInit_CreatesFallbackCommand(t *testing.T) {
	dir := t.TempDir()
	runQwenInitInDir(t, dir)

	fallbackPath := filepath.Join(dir, ".qwen", "commands", "crush-fallback.md")
	bts, err := os.ReadFile(fallbackPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "{{args}}")
	assert.NotContains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "description:")
	assert.Contains(t, got, "CronCreate")
	assert.Contains(t, got, "TaskCreate")
}

func TestQwenInit_SlashCommandOverwritesWithSentinel(t *testing.T) {
	dir := t.TempDir()
	runQwenInitInDir(t, dir)
	slashPath := filepath.Join(dir, ".qwen", "commands", "crush.md")
	first, err := os.ReadFile(slashPath)
	require.NoError(t, err)

	runQwenInitInDir(t, dir)
	second, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
}

func TestQwenInit_SlashCommandSkipsWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	slashPath := filepath.Join(dir, ".qwen", "commands", "crush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(slashPath), 0o755))
	require.NoError(t, os.WriteFile(slashPath, []byte("someone else's file"), 0o644))

	stderr := captureStderr(t, func() {
		runQwenInitInDir(t, dir)
	})

	assert.Contains(t, stderr, "does not contain our sentinel")
	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's file", string(bts))
}

// ---------------------------------------------------------------------------
// qwen-del tests
// ---------------------------------------------------------------------------

func TestQwenDel_RemovesSlashCommandWithSentinel(t *testing.T) {
	dir := t.TempDir()
	slashPath := filepath.Join(dir, ".qwen", "commands", "crush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(slashPath), 0o755))
	require.NoError(t, os.WriteFile(slashPath, []byte("<!-- crush-slash-command:v1 -->\nsome content\n"), 0o644))

	runQwenDelInDir(t, dir)

	_, err := os.Stat(slashPath)
	assert.True(t, os.IsNotExist(err), "slash command file should be removed when it has our sentinel")
}

func TestQwenDel_RefusesSlashCommandWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	slashPath := filepath.Join(dir, ".qwen", "commands", "crush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(slashPath), 0o755))
	require.NoError(t, os.WriteFile(slashPath, []byte("not ours"), 0o644))

	stderr := captureStderr(t, func() {
		runQwenDelInDir(t, dir)
	})

	assert.Contains(t, stderr, "refusing to delete")
	assert.Contains(t, stderr, "missing sentinel")

	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	assert.Equal(t, "not ours", string(bts))
}

func TestQwenDel_IdempotentOnSecondRun(t *testing.T) {
	dir := t.TempDir()
	runQwenInitInDir(t, dir)

	runQwenDelInDir(t, dir)
	stderr := captureStderr(t, func() {
		runQwenDelInDir(t, dir)
	})
	// Second run is a no-op: nothing left to remove, no errors raised.
	assert.NotContains(t, stderr, "refusing to delete")
}
