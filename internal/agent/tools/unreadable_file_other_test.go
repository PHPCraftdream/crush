//go:build !windows

package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// makeUnreadableFileForTest chmods the file to 0o000 so that open(2) for
// reading fails with EACCES while the file still exists — mode 000 blocks
// every reader except a root process.
func makeUnreadableFileForTest(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("secret"), 0o644))
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	return path
}
