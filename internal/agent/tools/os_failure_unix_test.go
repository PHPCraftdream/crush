//go:build !windows

package tools

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOSFailureIsFatal_Unix pins the Unix half of the errno split. The
// errno values are constructed directly rather than provoked through real
// syscalls because a genuinely full disk or out-of-memory process cannot
// be provisioned in a test — and this is exactly how the split itself was
// reviewed: this test pins the classification table, while
// error_contract_test.go pins the call sites that consult it.
func TestOSFailureIsFatal_Unix(t *testing.T) {
	t.Parallel()

	fatal := []struct {
		desc string
		err  error
	}{
		{
			"EIO wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: syscall.EIO},
		},
		{
			"EROFS wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: syscall.EROFS},
		},
		{
			// Proves errors.As unwraps *os.LinkError, the shape a
			// failed rename arrives in.
			"ENOSPC wrapped in *os.LinkError",
			&os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.ENOSPC},
		},
		{
			// Proves errors.As accepts a bare errno with no wrapper.
			"bare ENOMEM",
			syscall.ENOMEM,
		},
	}
	for _, tc := range fatal {
		require.True(t, osFailureIsFatal(tc.err),
			"expected fatal: %s", tc.desc)
	}

	recoverable := []struct {
		desc string
		err  error
	}{
		{
			"EACCES wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: syscall.EACCES},
		},
		{
			"EPERM wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: syscall.EPERM},
		},
		{
			"ENOENT wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: syscall.ENOENT},
		},
		{
			"ENOTDIR wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: syscall.ENOTDIR},
		},
		{
			"EEXIST wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: syscall.EEXIST},
		},
		{
			"EINVAL wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: syscall.EINVAL},
		},
		{
			// No syscall.Errno inside: errors.As finds nothing, so the
			// classifier must say recoverable.
			"wrapped error with no errno inside",
			fmt.Errorf("plain: %w", fs.ErrPermission),
		},
		{
			"nil error",
			nil,
		},
	}
	for _, tc := range recoverable {
		require.False(t, osFailureIsFatal(tc.err),
			"expected recoverable: %s", tc.desc)
	}
}
