package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	lockHeartbeatInterval = 10 * time.Second
	lockStaleDuration     = 20 * time.Second
)

// SessionLock is an inter-process exclusive lock for a single session ID.
// Acquired around the entire `sessionAgent.Run()` call so two crush
// processes can never write into the same session simultaneously.
//
// Backed by OS-level advisory file locks (flock on POSIX, LockFileEx on
// Windows) for mutual exclusion between live processes, PLUS a heartbeat
// that touches the lock file every 10 seconds. If the file has not been
// touched for 20 seconds the lock is considered stale (holder crashed or
// was killed without releasing) and the next caller deletes it and
// proceeds.
type SessionLock struct {
	// Path is the on-disk lock file. Kept for diagnostics.
	Path string
	// HolderPID is the PID that holds this lock.
	HolderPID int

	f    *os.File
	stop chan struct{} // closed by Release to stop the heartbeat goroutine
}

// SessionLockBusyError is returned by TryAcquireSessionLock when the
// lock is already held by another process.
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
// success (caller MUST Release()). Returns *SessionLockBusyError if
// another live process holds the lock. Other errors returned as-is.
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

	// Remove stale lock file before attempting OS lock.
	if err := removeIfStale(path); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("TryAcquireSessionLock: open lock file: %w", err)
	}
	if err := tryLockFile(f); err != nil {
		holderPID := readLockHolderPID(path)
		f.Close()
		return nil, &SessionLockBusyError{Path: path, HolderPID: holderPID}
	}

	myPID := os.Getpid()
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = fmt.Fprintf(f, "%d\n", myPID)
	_ = f.Sync()

	// Touch the file now so mtime is fresh from the start.
	now := time.Now()
	_ = os.Chtimes(path, now, now)

	stop := make(chan struct{})
	go heartbeat(path, stop)

	return &SessionLock{Path: path, HolderPID: myPID, f: f, stop: stop}, nil
}

// Release stops the heartbeat, unlocks and closes the lock file.
// Safe to call on nil. Idempotent.
func (l *SessionLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	// Stop heartbeat first so it doesn't touch the file after we release.
	if l.stop != nil {
		close(l.stop)
		l.stop = nil
	}
	unlockErr := unlockFile(l.f)
	closeErr := l.f.Close()
	l.f = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// heartbeat touches the lock file every lockHeartbeatInterval to signal
// the holder is still alive. Stops when done is closed.
func heartbeat(path string, done <-chan struct{}) {
	t := time.NewTicker(lockHeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			now := time.Now()
			_ = os.Chtimes(path, now, now)
		}
	}
}

// removeIfStale deletes the lock file if it exists and has not been
// touched for lockStaleDuration. A missing file is not an error.
func removeIfStale(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("removeIfStale: stat %s: %w", path, err)
	}
	if time.Since(info.ModTime()) > lockStaleDuration {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removeIfStale: remove stale lock %s: %w", path, err)
		}
	}
	return nil
}

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
