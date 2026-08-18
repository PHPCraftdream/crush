//go:build windows

package tools

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// osFailureIsFatal is the Windows half of the errno split of the stat/read
// sites in this package (view.go, edit.go, write.go, multiedit.go),
// applying the retry-invariance criterion of the tool error contract in
// tools.go: fatal if and only if no resent input and no passage of time
// would make the call succeed. The errors arriving here are
// *fs.PathError/*os.LinkError values wrapping a native syscall.Errno
// (measured in the wild: 3, 5, 32, 123 — ERROR_PATH_NOT_FOUND,
// ERROR_ACCESS_DENIED, ERROR_SHARING_VIOLATION, ERROR_INVALID_NAME);
// windows.Errno is a type alias of syscall.Errno, so the constants below
// compare directly against what errors.As extracts.
//
// This file exists because the helper's previous Unix-constant switch was
// dead code on Windows: EIO/EROFS/ENOSPC/ENOMEM are synthetic
// APPLICATION_ERROR+n values there (measured: 536870952, 536871024,
// 536870996, 536870991) that no native code can equal, so the old switch
// always returned false and demoted even a full disk to a tool response.
//
// The fatal set, by the criterion: the two ENOMEM analogs
// (ERROR_NOT_ENOUGH_MEMORY, ERROR_OUTOFMEMORY — the process itself is out
// of memory, every call fails identically), write-protected media
// (ERROR_WRITE_PROTECT), hardware/medium failures (ERROR_CRC,
// ERROR_IO_DEVICE, ERROR_DISK_CORRUPT) and a full volume
// (ERROR_HANDLE_DISK_FULL, ERROR_DISK_FULL — every write fails). Unknown
// codes default to recoverable per the contract. ERROR_COMMITMENT_LIMIT
// (pagefile exhaustion) was considered and deliberately left recoverable:
// it can clear with time alone, failing the retry-invariance test, and
// the radius asymmetry in tools.go says a wrongly-demoted error costs a
// turn while a wrongly-fatal one costs the session.
//
// Go stdlib code on Windows can fabricate errnos from the synthetic Unix
// set in rare paths, and Go 1.26 aliases syscall.ENOTDIR to
// ERROR_PATH_NOT_FOUND(3) — none of those land in the fatal set, which is
// intentional: only measured native media/resource codes may end a run.
// The Unix twin of this split lives in os_failure_unix.go.
func osFailureIsFatal(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		// The ENOMEM analogs: the process itself is out of memory, so
		// every call fails identically.
		case windows.ERROR_NOT_ENOUGH_MEMORY, windows.ERROR_OUTOFMEMORY:
			return true
		// Write-protected media.
		case windows.ERROR_WRITE_PROTECT:
			return true
		// Hardware / medium failures.
		case windows.ERROR_CRC, windows.ERROR_IO_DEVICE, windows.ERROR_DISK_CORRUPT:
			return true
		// A full volume fails every write.
		case windows.ERROR_HANDLE_DISK_FULL, windows.ERROR_DISK_FULL:
			return true
		}
	}
	return false
}
