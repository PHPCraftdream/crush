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

func runGeminiInitInDir(t *testing.T, dir string) {
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
	require.NoError(t, geminiInitCmd.RunE(cmd, nil))
}

func runGeminiDelInDir(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, runGeminiDel(dir))
}

// ---------------------------------------------------------------------------
// gemini-init tests
// ---------------------------------------------------------------------------

func TestGeminiInit_CreatesSlashCommand(t *testing.T) {
	dir := t.TempDir()
	runGeminiInitInDir(t, dir)

	commandPath := filepath.Join(dir, ".gemini", "commands", "crush.toml")
	bts, err := os.ReadFile(commandPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, "crush-slash-command:v1")
	assert.Contains(t, got, `prompt = """`)
	assert.Contains(t, got, `description = "`)
	assert.NotContains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "{{args}}")
	assert.Contains(t, got, "crush run")
	assert.Contains(t, got, "--role smart")
}

func TestGeminiInit_CreatesFallbackCommand(t *testing.T) {
	dir := t.TempDir()
	runGeminiInitInDir(t, dir)

	commandPath := filepath.Join(dir, ".gemini", "commands", "crush-fallback.toml")
	bts, err := os.ReadFile(commandPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, "crush-slash-command:v1")
	assert.Contains(t, got, `prompt = """`)
	assert.Contains(t, got, `description = "`)
	assert.NotContains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "{{args}}")
	assert.Contains(t, got, "CronCreate")
	assert.Contains(t, got, "TaskCreate")
}

func TestGeminiInit_SlashCommandOverwritesWithSentinel(t *testing.T) {
	dir := t.TempDir()
	runGeminiInitInDir(t, dir)
	commandPath := filepath.Join(dir, ".gemini", "commands", "crush.toml")
	first, err := os.ReadFile(commandPath)
	require.NoError(t, err)

	runGeminiInitInDir(t, dir)
	second, err := os.ReadFile(commandPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
}

func TestGeminiInit_SlashCommandSkipsWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, ".gemini", "commands", "crush.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(commandPath), 0o755))
	require.NoError(t, os.WriteFile(commandPath, []byte("someone else's file"), 0o644))

	stderr := captureStderr(t, func() {
		runGeminiInitInDir(t, dir)
	})

	assert.Contains(t, stderr, "does not contain our sentinel")
	bts, err := os.ReadFile(commandPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's file", string(bts))
}

// ---------------------------------------------------------------------------
// gemini-del tests
// ---------------------------------------------------------------------------

func TestGeminiDel_RemovesSlashCommandWithSentinel(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, ".gemini", "commands", "crush.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(commandPath), 0o755))
	require.NoError(t, os.WriteFile(commandPath, []byte("# crush-slash-command:v1\nsome content\n"), 0o644))

	runGeminiDelInDir(t, dir)

	_, err := os.Stat(commandPath)
	assert.True(t, os.IsNotExist(err), "command file should be removed when it has our sentinel")
}

func TestGeminiDel_RefusesSlashCommandWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, ".gemini", "commands", "crush.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(commandPath), 0o755))
	require.NoError(t, os.WriteFile(commandPath, []byte("not ours"), 0o644))

	stderr := captureStderr(t, func() {
		runGeminiDelInDir(t, dir)
	})

	assert.Contains(t, stderr, "refusing to delete")
	assert.Contains(t, stderr, "missing sentinel")

	bts, err := os.ReadFile(commandPath)
	require.NoError(t, err)
	assert.Equal(t, "not ours", string(bts))
}

func TestGeminiDel_IdempotentOnSecondRun(t *testing.T) {
	dir := t.TempDir()
	runGeminiInitInDir(t, dir)

	runGeminiDelInDir(t, dir)
	stderr := captureStderr(t, func() {
		runGeminiDelInDir(t, dir)
	})
	// Second run is a no-op: nothing left to remove, no errors raised.
	assert.NotContains(t, stderr, "refusing to delete")
}
