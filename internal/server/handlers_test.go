package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSaveAttachmentToDisk_UsesDataDirNotWorkingDir verifies that
// saveAttachmentToDisk writes under <dataDir>/attachments/ using the
// dataDir argument as-is (no extra ".crush" segment appended), and does NOT
// fall back to any working-directory-derived path. This guards against
// regressing to the old cwd-hardcoded behavior (task #248 / attachments dir
// bug), where attachments always landed under
// "<workingDir>/.crush/attachments" even when a different data_directory
// (or --data-dir) was configured.
func TestSaveAttachmentToDisk_UsesDataDirNotWorkingDir(t *testing.T) {
	dataDir := t.TempDir()
	workingDir := t.TempDir()
	require.NotEqual(t, dataDir, workingDir)

	path, err := saveAttachmentToDisk(dataDir, "notes.txt", []byte("hello"))
	require.NoError(t, err)

	// The file must live under dataDir/attachments/, not dataDir itself and
	// not anywhere derived from workingDir.
	rel, err := filepath.Rel(dataDir, path)
	require.NoError(t, err)
	require.False(t, filepath.IsAbs(rel))
	require.NotContains(t, rel, "..")

	dir := filepath.Dir(path)
	require.Equal(t, filepath.Join(dataDir, "attachments"), dir)

	// Sanity: nothing was written under workingDir at all.
	entries, err := os.ReadDir(workingDir)
	require.NoError(t, err)
	require.Empty(t, entries)

	// The file actually exists with the expected contents.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))
}

// TestSaveAttachmentToDisk_EmptyDataDirErrors verifies the nil-config edge
// case: saveAttachmentToDisk itself refuses to silently fall back to cwd
// when handed an empty dataDir. Callers (see attachmentsDataDir) are
// responsible for supplying a defensive fallback before calling in.
func TestSaveAttachmentToDisk_EmptyDataDirErrors(t *testing.T) {
	_, err := saveAttachmentToDisk("", "notes.txt", []byte("hello"))
	require.Error(t, err)
}
