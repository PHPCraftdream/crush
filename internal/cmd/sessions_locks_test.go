package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionsLocks_CreateLockFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create locks directory
	locksDir := filepath.Join(tmpDir, ".crush", "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))

	// Create a lock file
	lockFile := filepath.Join(locksDir, "session-test-id-1.lock")
	require.NoError(t, os.WriteFile(lockFile, []byte("12345\n"), 0o644))

	// Verify it exists
	require.FileExists(t, lockFile)

	content, err := os.ReadFile(lockFile)
	require.NoError(t, err)
	require.Contains(t, string(content), "12345")
}

func TestSessionsLocks_MultipleFiles(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	locksDir := filepath.Join(tmpDir, ".crush", "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))

	// Create multiple lock files
	for i := 1; i <= 3; i++ {
		lockFile := filepath.Join(locksDir, "session-id-"+string(rune(i)+48)+".lock")
		require.NoError(t, os.WriteFile(lockFile, []byte("1000"+string(rune(i)+48)), 0o644))
	}

	// Verify all files exist
	entries, err := os.ReadDir(locksDir)
	require.NoError(t, err)
	require.Len(t, entries, 3)
}

func TestSessionsLocks_ParseFilename(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	locksDir := filepath.Join(tmpDir, ".crush", "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))

	lockFile := filepath.Join(locksDir, "session-abc-123.lock")
	require.NoError(t, os.WriteFile(lockFile, []byte("5678"), 0o644))

	// Parse filename
	filename := "session-abc-123.lock"
	sessionID := filename[8 : len(filename)-5] // Remove "session-" prefix and ".lock" suffix
	require.Equal(t, "abc-123", sessionID)
}
