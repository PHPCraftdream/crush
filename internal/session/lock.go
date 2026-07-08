package session

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	lockHeartbeatInterval = 10 * time.Second
	lockStaleDuration     = 20 * time.Second
)

// LockStaleDuration is the exported view of lockStaleDuration, for callers
// outside this package that need the same "how old is too old" threshold
// this package's own heartbeat logic uses. In particular: `crush sessions
// why`/`sessions list` must fall back to heartbeat freshness when the PID
// can't be read — see the Windows note on readLockFile below. A holder PID
// of 0 does NOT mean "unreadable/dead"; on Windows it very often means
// "actively held" (see readLockFile).
const LockStaleDuration = lockStaleDuration

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

	f       *os.File
	stop    chan struct{} // closed by Release to stop the heartbeat goroutine
	release sync.Once     // Fork patch: review-fix — prevents double-close panic on concurrent Release()
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
// TryAcquireSessionLockWithTimeout is like TryAcquireSessionLock but also
// writes the run's --timeout (in seconds) as a second line in the lock file.
// `sessions locks` reads this to display ELAPSED / BUDGET.
func TryAcquireSessionLockWithTimeout(dataDir, sessionID string, timeoutSec int64) (*SessionLock, error) {
	lk, err := TryAcquireSessionLock(dataDir, sessionID)
	if err != nil {
		return nil, err
	}
	if timeoutSec > 0 && lk.f != nil {
		// Append the timeout on the second line; reader handles missing line gracefully.
		_, _ = fmt.Fprintf(lk.f, "%d\n", timeoutSec)
		_ = lk.f.Sync()
	}
	return lk, nil
}

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

	lk, err := acquireSessionLockFile(path)
	if err == nil {
		return lk, nil
	}

	var busyErr *SessionLockBusyError
	if !errors.As(err, &busyErr) {
		return nil, err
	}

	// The pre-open stale check can race with another process whose heartbeat
	// expires just after we checked but before tryLockFile reports contention.
	// If the heartbeat is stale now, reclaim the file and try once more.
	reclaimed, reclaimErr := reclaimStaleLock(path, "lock_contention")
	if reclaimErr != nil {
		return nil, reclaimErr
	}
	if !reclaimed {
		return nil, busyErr
	}
	lk, err = acquireSessionLockFile(path)
	if err != nil {
		return nil, err
	}
	return lk, nil
}

func acquireSessionLockFile(path string) (*SessionLock, error) {
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
// Safe to call on nil. Idempotent and concurrency-safe.
func (l *SessionLock) Release() error {
	if l == nil {
		return nil
	}
	var releaseErr error
	l.release.Do(func() {
		if l.stop != nil {
			close(l.stop)
		}
		if l.f != nil {
			unlockErr := unlockFile(l.f)
			closeErr := l.f.Close()
			if unlockErr != nil {
				releaseErr = unlockErr
			} else {
				releaseErr = closeErr
			}
		}
	})
	return releaseErr
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

// removeIfStale deletes the lock file if it exists and is unambiguously
// stale. A missing file is not an error.
//
// Two staleness signals (either is sufficient):
//  1. mtime older than lockStaleDuration — heartbeat would have touched
//     the file every 10s if the holder were alive.
//  2. holder PID is no longer a running process — covers the orphan case
//     where the wrapper that started crush was killed but crush itself
//     also died at the same time, so within the 20s mtime window we'd
//     otherwise refuse a clean re-entry. See feedback round 2, #12.
//
// We only check the PID branch when the file is older than one
// heartbeat tick (10s). Inside the first 10s the file may exist but PID
// hasn't been written yet (acquirer is still in the open→lock→write
// dance), so a PID-not-alive check would race the acquirer.
func removeIfStale(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("removeIfStale: stat %s: %w", path, err)
	}
	age := time.Since(info.ModTime())
	if age > lockStaleDuration {
		_, err := reclaimStaleLock(path, "mtime_expired")
		return err
	}
	// Fork patch (orchestrator UX, round 2 #12): PID-based fast-path. If
	// the holder PID is dead, snap the lock immediately instead of making
	// the operator wait 20s. Skip the check for very young locks to avoid
	// racing the acquirer's "open → lock → write PID" sequence.
	if age > lockHeartbeatInterval {
		if pid := readLockHolderPID(path); pid > 0 && !isProcessAlive(pid) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removeIfStale: remove orphan lock %s (PID %d dead): %w", path, pid, err)
			}
			slog.Info("reclaimed orphan session lock",
				"reason", "holder_pid_dead",
				"path", path,
				"holder_pid", pid,
				"age_seconds", int(age.Seconds()))
		}
	}
	return nil
}

func reclaimStaleLock(path, reason string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("reclaimStaleLock: stat %s: %w", path, err)
	}
	age := time.Since(info.ModTime())
	if age <= lockStaleDuration {
		return false, nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("reclaimStaleLock: remove stale lock %s: %w", path, err)
	}
	slog.Info("reclaimed stale session lock",
		"reason", reason,
		"path", path,
		"age_seconds", int(age.Seconds()))
	return true, nil
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
	pid, _ := readLockFile(path)
	return pid
}

// ReadLockPID is the exported variant of readLockHolderPID, used by
// `crush sessions kill` / `reset --force` to read the PID off a lock
// file without having to re-implement the multi-line parse (the file
// stores PID on line 1, optional timeout in seconds on line 2).
func ReadLockPID(path string) int {
	return readLockHolderPID(path)
}

// ReadLockTimeoutSec returns the timeout-in-seconds stored on the second line
// of a lock file (written by TryAcquireSessionLockWithTimeout). Returns 0 if
// not present or unreadable — backward compatible.
func ReadLockTimeoutSec(path string) int64 {
	_, t := readLockFile(path)
	return t
}

// LockState describes the inter-process ownership of a session: who holds
// the lock right now, and how fresh the heartbeat is. Used by the web
// server's session list to surface "Owned externally by PID N" so a tab
// opened on a session that's being driven from another process renders
// read-only.
type LockState struct {
	Exists bool          // lock file is present on disk
	PID    int           // PID written into the lock file (0 if unreadable)
	Age    time.Duration // time since the last heartbeat touch (mtime)
	Live   bool          // Age < liveThreshold — a healthy owner is touching it
}

// InspectSessionLock reads the lock file for `sessionID` under `dataDir`
// without acquiring it. Safe to call from any process — no side effects.
// `liveThreshold` defines how fresh the heartbeat must be to count as
// "live" (callers typically pass 20s — the same expiry the heartbeat
// loop uses; see TryAcquireSessionLock comments).
func InspectSessionLock(dataDir, sessionID string, liveThreshold time.Duration) LockState {
	if dataDir == "" || sessionID == "" {
		return LockState{}
	}
	path := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionID(sessionID)+".lock")
	st, err := os.Stat(path)
	if err != nil {
		return LockState{}
	}
	pid := ReadLockPID(path)
	age := time.Since(st.ModTime())
	return LockState{
		Exists: true,
		PID:    pid,
		Age:    age,
		Live:   age < liveThreshold,
	}
}

// readLockFile returns (PID, timeoutSec) from a lock file. Both default to 0
// on any parse error — backward compatible with old one-line files.
//
// Windows note: tryLockFile takes a LockFileEx exclusive lock over the
// WHOLE file for the entire lifetime of the holder. Unlike POSIX advisory
// locks, Windows enforces this as a MANDATORY lock — any plain read from a
// different handle/process into that byte range (which is exactly what
// os.ReadFile does here) fails with a sharing/lock violation for as long as
// the holder is alive, not just during a brief write race. So on Windows,
// (0, 0) from this function for an ACTIVELY RUNNING session is the norm,
// not the exception — callers that gate a "crashed" verdict on `pid > 0`
// alone will misdiagnose every live session on Windows. Always fall back to
// heartbeat freshness (mtime age vs LockStaleDuration) before concluding a
// session is dead; see readLockHolderPID's callers in internal/cmd.
func readLockFile(path string) (int, int64) {
	bts, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	lines := strings.Split(strings.TrimSpace(string(bts)), "\n")
	pid := 0
	var timeoutSec int64
	if len(lines) >= 1 {
		pid, _ = strconv.Atoi(strings.TrimSpace(lines[0]))
	}
	if len(lines) >= 2 {
		timeoutSec, _ = strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	}
	return pid, timeoutSec
}
