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
		"orchestrate sub-agents",       // crush's own agent tool
		"`agent` tool that spawns",     // explanation of how delegation works
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
func runClaudeInitInDir(t *testing.T, dir string, force bool) {
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
	args := []string{"--cwd", dir}
	if force {
		args = append(args, "--force")
	}
	require.NoError(t, cmd.ParseFlags(args))
	require.NoError(t, claudeInitCmd.RunE(cmd, nil))
}

func TestClaudeInit_CreatesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir, false)

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

	runClaudeInitInDir(t, dir, false)

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(bts)
	assert.True(t, strings.HasPrefix(got, pre), "existing content must be preserved verbatim at the top")
	assert.Contains(t, got, claudeInitMarkerStart)
}

func TestClaudeInit_IdempotentWithoutForce(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir, false)
	first, err := os.ReadFile(filepath.Join(dir, claudeMdFile))
	require.NoError(t, err)

	runClaudeInitInDir(t, dir, false)
	second, err := os.ReadFile(filepath.Join(dir, claudeMdFile))
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second), "second run without --force must be a no-op")
	assert.Equal(t, 1, strings.Count(string(second), claudeInitMarkerStart))
}

func TestClaudeInit_ForceReappends(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir, false)
	runClaudeInitInDir(t, dir, true)

	bts, err := os.ReadFile(filepath.Join(dir, claudeMdFile))
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(bts), claudeInitMarkerStart),
		"--force must duplicate the marker (callers know what they're doing)")
}
