package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SessionLock is an inter-process exclusive lock for a single session ID.
// Acquired around the entire `sessionAgent.Run()` call so two crush
// processes can never write into the same session simultaneously (the
// accidental-double-spawn scenario from the parallel-process audit:
// orchestrator fires `crush run --session X` twice, both processes see
// the per-process `IsSessionBusy` as false, both start streaming).
//
// Backed by OS-level advisory file locks (flock on POSIX, LockFileEx on
// Windows) so the lock is automatically released when the holder
// process dies — no stale-lock cleanup needed even after kill -9 or
// power loss.
type SessionLock struct {
	// Path is the on-disk lock file. Kept for diagnostics.
	Path string
	// HolderPID is the PID that holds this lock. Set on successful
	// acquire so callers can mention it in error messages.
	HolderPID int

	// f is the open file handle; Release uses it to call OS unlock and
	// close. Package-internal — implementation detail.
	f *os.File
}

// SessionLockBusyError is returned by TryAcquireSessionLock when the
// lock is already held by another process. Callers should surface the
// holder PID and the lock file path to the user — the WUI/CLI uses
// these to explain why their `crush run --session X` was rejected.
type SessionLockBusyError struct {
	Path      string
	HolderPID int
}

func (e *SessionLockBusyError) Error() string {
	if e.HolderPID > 0 {
		return fmt.Sprintf("session is already locked by crush process PID %d (lock file: %s)", e.HolderPID, e.Path)
	}
	return fmt.Sprintf("session is already locked by another crush process (lock file: %s)", e.Path)
}

// TryAcquireSessionLock attempts to acquire an exclusive lock for the
// given sessionID under <dataDir>/locks/. Returns a *SessionLock on
// success that the caller MUST Release() (typically via defer).
// Returns *SessionLockBusyError if another live process already holds
// the lock. Other errors (IO, permission) returned as-is.
//
// dataDir is usually cfg.Options.DataDirectory (already absolute by the
// time it reaches here). sessionID is the raw session id; it gets
// sanitised so things like "audit/A" can't escape the locks dir.
func TryAcquireSessionLock(dataDir, sessionID string) (*SessionLock, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("TryAcquireSessionLock: dataDir is empty")
	}
	if sessionID == "" {
		return nil, fmt.Errorf("TryAcquireSessionLock: sessionID is empty")
	}
	locksDir := filepath.Join(dataDir, "locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		return nil, fmt.Errorf("TryAcquireSessionLock: create locks dir: %w", err)
	}
	path := filepath.Join(locksDir, "session-"+sanitiseSessionID(sessionID)+".lock")
	// Open or create the lock file. We DON'T truncate yet — if the
	// acquire fails because someone else holds it, we read the PID
	// they wrote, so the busy-error can name the offender.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("TryAcquireSessionLock: open lock file: %w", err)
	}
	if err := tryLockFile(f); err != nil {
		// Read the holder PID written by the current owner (best-effort).
		holderPID := readLockHolderPID(path)
		f.Close()
		return nil, &SessionLockBusyError{Path: path, HolderPID: holderPID}
	}
	// We hold it. Stamp our PID for the next caller's busy-error.
	myPID := os.Getpid()
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = fmt.Fprintf(f, "%d\n", myPID)
	_ = f.Sync() // make PID visible to a contending reader without close
	return &SessionLock{Path: path, HolderPID: myPID, f: f}, nil
}

// Release unlocks and closes the lock file. Safe to call on nil.
// Idempotent: subsequent calls return nil. The lock file itself is
// left on disk; OS-level lock release happens on Close even if we
// crash before reaching this line, so a leftover file with stale PID
// content is harmless — the next acquire will overwrite the PID.
func (l *SessionLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlockErr := unlockFile(l.f)
	closeErr := l.f.Close()
	l.f = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// sanitiseSessionID replaces filesystem-unsafe characters in a session
// id so the resulting lock file lives inside the locks dir. Session ids
// in this codebase already use uuids or caller-chosen tags (audit-A,
// pr-42); the sanitiser exists as belt-and-suspenders against future
// id schemes that include slashes.
func sanitiseSessionID(id string) string {
	r := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		`"`, "_",
		"<", "_",
		">", "_",
		"|", "_",
		" ", "_",
	)
	return r.Replace(id)
}

// readLockHolderPID best-effort reads the PID stamped into a lock file
// by the holder. Returns 0 if the file is empty / unreadable / malformed.
func readLockHolderPID(path string) int {
	bts, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(bts)))
	if err != nil {
		return 0
	}
	return pid
}
