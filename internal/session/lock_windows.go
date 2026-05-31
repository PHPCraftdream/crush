//go:build windows

package session

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile takes an exclusive non-blocking lock on the entire file
// using Windows LockFileEx. Returns an error (non-nil) if another
// process already holds the lock.
func tryLockFile(f *os.File) error {
	// LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY: exclusive
	// AND don't block — failure means contention, not IO.
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	// Lock from offset 0, length (max-uint32, max-uint32) = essentially
	// the whole file. Matches the canonical "lock the file" idiom on
	// Windows.
	var overlapped windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		flags,
		0,          // reserved, must be 0
		^uint32(0), // nNumberOfBytesToLockLow
		^uint32(0), // nNumberOfBytesToLockHigh
		&overlapped,
	)
	return err
}

// unlockFile releases the lock taken by tryLockFile.
func unlockFile(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		^uint32(0),
		^uint32(0),
		&overlapped,
	)
}
