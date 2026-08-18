//go:build windows

package tools

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// TestOSFailureIsFatal_Windows pins the Windows half of the errno split.
// The errno values are constructed directly rather than provoked through
// real syscalls because a genuinely full or broken disk cannot be
// provisioned in a test — and this is exactly how the split itself was
// reviewed: this test pins the classification table, while
// error_contract_test.go pins the call sites that consult it.
func TestOSFailureIsFatal_Windows(t *testing.T) {
	t.Parallel()

	fatal := []struct {
		desc string
		err  error
	}{
		{
			"ERROR_NOT_ENOUGH_MEMORY wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_NOT_ENOUGH_MEMORY},
		},
		{
			"ERROR_OUTOFMEMORY wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_OUTOFMEMORY},
		},
		{
			"ERROR_WRITE_PROTECT wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_WRITE_PROTECT},
		},
		{
			"ERROR_CRC wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_CRC},
		},
		{
			"ERROR_IO_DEVICE wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_IO_DEVICE},
		},
		{
			"ERROR_DISK_CORRUPT wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_DISK_CORRUPT},
		},
		{
			"ERROR_HANDLE_DISK_FULL wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_HANDLE_DISK_FULL},
		},
		{
			"ERROR_DISK_FULL wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_DISK_FULL},
		},
		{
			// Proves errors.As unwraps *os.LinkError, the shape a
			// failed rename arrives in.
			"ERROR_DISK_FULL wrapped in *os.LinkError",
			&os.LinkError{Op: "rename", Old: "a", New: "b", Err: windows.ERROR_DISK_FULL},
		},
		{
			// Proves errors.As accepts a bare errno with no wrapper.
			"bare ERROR_HANDLE_DISK_FULL",
			windows.ERROR_HANDLE_DISK_FULL,
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
			"ERROR_ACCESS_DENIED wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_ACCESS_DENIED},
		},
		{
			"ERROR_PATH_NOT_FOUND wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_PATH_NOT_FOUND},
		},
		{
			"ERROR_FILE_NOT_FOUND wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_FILE_NOT_FOUND},
		},
		{
			"ERROR_ALREADY_EXISTS wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_ALREADY_EXISTS},
		},
		{
			"ERROR_SHARING_VIOLATION wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_SHARING_VIOLATION},
		},
		{
			"ERROR_INVALID_NAME wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_INVALID_NAME},
		},
		{
			"ERROR_FILENAME_EXCED_RANGE wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: windows.ERROR_FILENAME_EXCED_RANGE},
		},
		{
			// syscall.ENOTDIR equals 3, i.e. ERROR_PATH_NOT_FOUND, on
			// Go 1.26 Windows — the alias is documented in
			// os_failure_windows.go and stays recoverable.
			"syscall.ENOTDIR (aliased to ERROR_PATH_NOT_FOUND, value 3) wrapped in *fs.PathError",
			&fs.PathError{Op: "write", Path: "x", Err: syscall.ENOTDIR},
		},
		{
			// No syscall.Errno inside: errors.As finds nothing, so the
			// classifier must say recoverable.
			"wrapped fs.ErrPermission with no errno inside",
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
