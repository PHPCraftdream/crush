//go:build windows

package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// makeUnreadableFileForTest locks the file against any other open so that
// os.Open fails with ERROR_SHARING_VIOLATION(32) while os.Stat still
// succeeds — the deterministic Windows way to reach view's residual read
// branch.
func makeUnreadableFileForTest(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("secret"), 0o644))

	h, err := windows.CreateFile(
		windows.StringToUTF16Ptr(path),
		windows.GENERIC_READ,
		0, // shareMode 0: no other open, not even another read, is allowed.
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	require.NoError(t, err, "CreateFile with shareMode 0 must succeed on the file we just wrote")
	t.Cleanup(func() { _ = windows.CloseHandle(h) })

	return path
}
