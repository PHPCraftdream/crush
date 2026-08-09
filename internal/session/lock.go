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
	"sync/atomic"
	"time"
)

const (
	lockHeartbeatInterval = 10 * time.Second
	lockStaleDuration     = 20 * time.Second

	// maxPidFallbackAge bounds how long InspectSessionLock trusts a live
	// PID over a stale heartbeat mtime, before falling back to
	// mtime-only liveness. Without this bound, a reused PID (the OS
	// recycling the exact PID number a genuinely-dead holder's lock
	// file recorded) would make Live report true forever, with no
	// self-heal path short of a manual `sessions kill`/`sessions reap`.
	// Set comfortably above internal/agent's toolExecutionMaxDefault (45
	// minutes as of this writing — the maximum a single tool call,
	// including a sub-agent delegation, can legitimately run before the
	// stream watchdog force-cancels it), so a genuinely long-but-healthy
	// tool call is never mistaken for PID reuse. internal/session does
	// not import internal/agent (that would be a layering violation) —
	// this constant is deliberately independent and must be kept
	// roughly in sync by hand if toolExecutionMaxDefault ever changes
	// meaningfully.
	maxPidFallbackAge = 60 * time.Minute
)

// LockStaleDuration is the exported view of lockStaleDuration, for callers
// outside this package that need the same "how old is too old" threshold
// this package's own heartbeat logic uses. In particular: `crush sessions
// why`/`sessions list` must fall back to heartbeat freshness when the PID
// can't be read — see the Windows note on readLockFile below. A holder PID
// of 0 does NOT mean "unreadable/dead"; on Windows it very often means
// "actively held" (see readLockFile).
const LockStaleDuration = lockStaleDuration

// MaxPidFallbackAge is the exported view of maxPidFallbackAge, for callers
// outside this package that independently re-implement the same "stale
// mtime -> fall back to trusting a live PID" liveness check InspectSessionLock
// performs, instead of calling InspectSessionLock itself (e.g. because they
// need a "running"/"crashed" classification shape InspectSessionLock's
// LockState doesn't return, or only have a locks-directory entry in hand
// rather than a (dataDir, sessionID) pair). Task #241/#250: three such
// independent copies — internal/cmd/sessions_watch.go's isSessionFinished
// (now migrated to call InspectSessionLock directly),
// internal/cmd/sessions.go's computeSessionStatuses (kept independent — see
// that function's doc comment for why), and internal/cmd/sessions_why.go's
// explainSessionStatus (same "trust pid>0 unconditionally" shape as
// computeSessionStatuses — the very command meant to explain a session's
// verdict, found unbounded in a later review pass) — were found to trust a
// live-looking PID forever, with no bound of their own, even though
// InspectSessionLock had already been bounded for task #235. Exporting the
// same constant, rather than letting each site hand-roll its own duration,
// keeps the bound doctrinally in sync across every "PID reuse can't pin
// liveness forever" check in the codebase.
const MaxPidFallbackAge = maxPidFallbackAge

// clearHolderMetadataFn is the function used by Release to clear diagnostic
// metadata from the lock file. Can be replaced in tests to inject blocking
// behavior for proving release order invariants (see TestP1_2_ReleaseUnlocksBeforeMetadataCleanup_Hang).
var clearHolderMetadataFn = clearHolderMetadata

// SessionLock is an inter-process exclusive lock for a single session ID.
// Acquired around the entire `sessionAgent.Run()` call so two crush
// processes can never write into the same session simultaneously.
//
// Backed exclusively by OS-level advisory file locks (flock on POSIX,
// LockFileEx on Windows) for mutual exclusion between live processes.
// The holder also runs a heartbeat that touches the lock file's mtime
// every 10 seconds, but that heartbeat (and the 20s "stale" threshold
// derived from it) is diagnostics only — surfaced to operators via
// `sessions locks`/`sessions why` as "does this look wedged". It is
// NEVER used to decide whether a lock file may be reclaimed. Reclaiming
// is decided exclusively by attempting the real OS lock on the existing
// file: if that attempt succeeds, the previous holder is provably gone
// at the kernel level (flock/LockFileEx auto-releases on process death,
// even if its mtime is fresh or its heartbeat is merely lagging); if the
// attempt reports contention, the file is busy, full stop, regardless of
// how stale its mtime looks. This avoids a real bug in an earlier
// version of this file: unlinking the path based on mtime alone raced a
// live holder whose heartbeat had merely lagged (GC pause, transient
// Chtimes failure) — flock is bound to the inode, not the path, so the
// unlink didn't revoke the live holder's lock, it just let a second
// process create a new inode at the same path and believe it owned the
// session, producing two simultaneous owners of one session id.
type SessionLock struct {
	// Path is the on-disk lock file. Kept for diagnostics.
	Path string
	// HolderPID is the PID that holds this lock.
	HolderPID int

	f       *os.File
	stop    chan struct{} // closed by Release to stop the heartbeat goroutine
	release sync.Once     // Fork patch: review-fix — prevents double-close panic on concurrent Release()

	// active is set by RecordActivity and consumed by the heartbeat
	// goroutine on each tick. See RecordActivity's doc comment: the
	// heartbeat's mtime touch is gated on real activity, not a blind
	// timer, so a genuinely wedged process (no forward progress) stops
	// looking alive to diagnostics after one interval.
	active atomic.Bool
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
		writePIDSidecar(lk.Path, lk.HolderPID, timeoutSec)
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

	// logStaleDiagnostics is a best-effort, decision-free hint: it never
	// removes anything and never influences whether we succeed or fail
	// below. The ONLY thing that may ever decide "the previous holder is
	// gone" is actually winning the OS-level lock (flock/LockFileEx) on
	// this exact path. mtime stopped being trustworthy as a removal
	// trigger the day someone's GC pause or a transient Chtimes failure
	// let a live holder's heartbeat go stale while it still held the OS
	// lock — reclaiming on mtime alone in that case gives two "owners"
	// of the same session at once (see package doc).
	logStaleDiagnostics(path)

	lk, err := acquireSessionLockFile(path)
	if err == nil {
		return lk, nil
	}

	var busyErr *SessionLockBusyError
	if !errors.As(err, &busyErr) {
		return nil, err
	}
	return nil, busyErr
}

// acquireSessionLockFile opens (creating if necessary) the lock file at
// path and attempts to take the OS-level advisory lock on THAT SAME
// file/inode/handle. There is no "remove the file, make a new one"
// fallback anywhere in this path: unlinking a path out from under an
// OS lock does not release the lock (flock is bound to the inode, not
// the path) — it just lets a second process create a fresh inode at
// the same path and believe it owns the session while the original
// holder, if still alive, keeps running against the old inode. Two
// "owners" of one session id is exactly the bug this file exists to
// prevent.
//
// If tryLockFile succeeds here, that is authoritative proof the
// previous holder is gone at the OS level — the kernel releases flock/
// LockFileEx automatically when the holding process dies or closes the
// descriptor, regardless of what its lock file's mtime says. Only then
// do we truncate and stamp our own PID into the file and start our own
// heartbeat.
//
// If tryLockFile reports contention, that is likewise authoritative:
// somebody else holds the OS lock right now, full stop. No mtime
// threshold, no PID liveness probe, nothing overrides that — we return
// SessionLockBusyError unconditionally.
func acquireSessionLockFile(path string) (*SessionLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("TryAcquireSessionLock: open lock file: %w", err)
	}
	if err := tryLockFile(f); err != nil {
		holderPID := readLockHolderPID(path)
		f.Close()
		if isLockContentionError(err) {
			return nil, &SessionLockBusyError{Path: path, HolderPID: holderPID}
		}
		// Genuinely unidentified failure (permission denied, IO error,
		// etc — not "someone else holds it"). Surface it as-is so the
		// caller can tell the difference between "busy" and "broken";
		// see agent.Run's handling, which now treats anything that
		// isn't a *SessionLockBusyError as fatal rather than silently
		// running without lock protection.
		return nil, fmt.Errorf("TryAcquireSessionLock: lock file %s: %w", path, err)
	}

	myPID := os.Getpid()
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = fmt.Fprintf(f, "%d\n", myPID)
	_ = f.Sync()
	writePIDSidecar(path, myPID, 0)

	// Touch the file now so mtime is fresh from the start. mtime is a
	// diagnostic aid only (surfaced via InspectSessionLock / `sessions
	// locks`) — see the package doc on SessionLock. It is never used to
	// decide whether a lock may be reclaimed.
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		slog.Warn("session lock: failed to set initial heartbeat mtime",
			"path", path, "err", err)
	}

	stop := make(chan struct{})
	lk := &SessionLock{Path: path, HolderPID: myPID, f: f, stop: stop}
	go heartbeat(path, stop, &lk.active)

	return lk, nil
}

// Release stops the heartbeat, unlocks and closes the lock file.
// Safe to call on nil. Idempotent and concurrency-safe.
//
// P1-2 fix (2026-08-09): The order of operations is now:
//  1. Stop the heartbeat goroutine (close(l.stop)).
//  2. Unlock the OS-level lock (unlockFile) - this is the critical correctness-critical step.
//  3. Close the file descriptor (f.Close).
//  4. Clear diagnostic metadata (clearHolderMetadata) - best-effort cleanup that can hang.
//
// The old order (clear metadata BEFORE unlock/close) meant that a hung filesystem,
// AV scan, or SMB share during clearHolderMetadata (Truncate/Seek/Sync/Remove) would
// prevent the OS lock from ever being released, wedging the session forever.
//
// The new order prioritizes releasing the OS lock AND closing the file descriptor BEFORE
// any diagnostic cleanup that can hang. If clearHolderMetadata hangs after close, the OS
// lock is already gone and the file descriptor is already closed - no session wedged state,
// no resource leak. The only downside is that clearHolderMetadata must now operate on a
// closed file, so we pass only the path (not the open file handle) and let it reopen
// the file just for the metadata operations.
//
// Before actually unlocking, it wipes the PID it stamped into both the
// primary lock file and the sidecar (see clearHolderMetadata). This
// matters for `crush sessions kill`: without it, a process that exits
// cleanly (Release() runs, e.g. via a normal `defer`) leaves its old PID
// sitting in the lock file/sidecar on disk. If the OS later reuses that
// PID number for a completely unrelated process — routine on a busy
// CI/dev box — a later `sessions kill <id>` invocation would read a
// "plausible" PID from the stale file and forcibly kill that unrelated
// process. The lock FILE itself is deliberately left in place — see
// acquireSessionLockFile and the package doc for why unlinking the path
// is unsafe/unnecessary; only its content is cleared.
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
			// P1-2 fix: unlock and close FIRST, before any diagnostic cleanup that can hang.
			// The OS lock and file descriptor are the correctness-critical resources -
			// releasing/closing them is the priority. Metadata cleanup is best-effort.
			unlockErr := unlockFile(l.f)
			closeErr := l.f.Close()
			if unlockErr != nil {
				releaseErr = unlockErr
			} else if closeErr != nil {
				releaseErr = closeErr
			}
			// Clear diagnostic metadata (best-effort, may hang on slow FS/AV/SMB).
			// This runs AFTER unlockFile and Close, so a hang here does NOT block
			// lock release or leak file descriptors. We pass only the path since
			// the file is already closed; clearHolderMetadataFn will reopen if needed.
			//
			// clearHolderMetadataFn is a test seam (package-level var) so tests can
			// inject blocking behavior to prove unlock/close happen before metadata
			// cleanup (see TestP1_2_ReleaseUnlocksBeforeMetadataCleanup_Hang).
			clearHolderMetadataFn(l.Path)
		}
	})
	return releaseErr
}

// RecordActivity marks that the holder made real progress since the last
// heartbeat tick, so the next tick will actually touch the lock file's
// mtime. Safe to call concurrently from multiple goroutines (backed by
// atomic.Bool) — this is intentional: task #214 wires this in from the
// agent's turn loop and from cross-goroutine watchdog callbacks, both of
// which may fire from goroutines other than the one that holds the lock.
//
// Calling this on a nil *SessionLock or after Release is a harmless
// no-op; the heartbeat goroutine has already exited in the latter case,
// so no tick will ever consume the flag.
//
// This is purely an input to the diagnostics-only heartbeat described in
// the package doc — recording (or not recording) activity never has any
// bearing on lock acquisition/release/reclaim, which remains decided
// solely by the real OS lock.
func (l *SessionLock) RecordActivity() {
	if l == nil {
		return
	}
	l.active.Store(true)
}

// clearHolderMetadata wipes the PID/timeout previously stamped into the
// lock file's own content and removes the never-locked sidecar file,
// so a cleanly-released lock does not leave a stale, plausible-looking
// PID behind for a later `sessions kill`/`sessions why` to misread as a
// live holder (see Release's doc comment).
//
// P1-2 fix (2026-08-09): Now takes only the path (not an open file *os.File)
// because Release() now closes the file before calling this. This function
// reopens the file just for the metadata operations, ensuring that if it
// hangs, the original file descriptor is already closed and the OS lock
// is already released.
//
// WARNING: This function performs file I/O operations (open/Truncate/Seek/Sync
// on the lock file and Remove on the sidecar) that can hang indefinitely
// on slow/unavailable filesystems, AV scans, or SMB shares. For this reason,
// Release() calls this AFTER unlocking the OS lock and closing the file.
// This means clearHolderMetadata runs WITHOUT holding the OS lock, so:
//   - A hang here does NOT prevent other processes from acquiring the lock.
//   - If it hangs, no file descriptor leaks (the original fd is already closed).
//   - Another process may briefly see stale metadata in the tiny window
//     between our unlock and our successful clear, but the OS lock is the
//     source of truth for ownership.
//
// Best-effort: any failure here only degrades diagnosability (same
// posture as writePIDSidecar) and must never block Release from
// actually unlocking/closing the file — a stuck holder that can't be
// released would be strictly worse than a stale PID left on disk.
func clearHolderMetadata(path string) {
	// Reopen the file just for metadata operations.
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		slog.Warn("session lock: failed to reopen lock file for metadata clear", "path", path, "err", err)
		// Still try to remove the sidecar even if we can't open the main file
		if sidecarErr := os.Remove(pidSidecarPath(path)); sidecarErr != nil && !os.IsNotExist(sidecarErr) {
			slog.Warn("session lock: failed to remove PID sidecar on release", "path", path, "err", sidecarErr)
		}
		return
	}
	defer f.Close()

	if err := f.Truncate(0); err != nil {
		slog.Warn("session lock: failed to clear lock file content on release", "path", path, "err", err)
	} else if _, err := f.Seek(0, 0); err != nil {
		slog.Warn("session lock: failed to seek lock file to start on release", "path", path, "err", err)
	} else {
		_ = f.Sync()
	}
	if err := os.Remove(pidSidecarPath(path)); err != nil && !os.IsNotExist(err) {
		slog.Warn("session lock: failed to remove PID sidecar on release", "path", path, "err", err)
	}
}

// heartbeat touches the lock file every lockHeartbeatInterval, but ONLY
// if RecordActivity was called on the owning SessionLock at least once
// since the previous tick. Stops when done is closed.
//
// This is diagnostics only ("something might be wrong if this goes
// stale") — see the package doc on SessionLock and acquireSessionLockFile.
// It must never be treated as the source of truth for whether the lock
// may be reclaimed; only actually winning the OS lock decides that.
//
// Gating on activity is itself a deliberate design requirement, not an
// optimization: a session that is fully wedged (stuck goroutine, no
// forward progress) must NOT keep presenting a live-looking mtime
// forever — that was a diagnostic false positive. See RecordActivity.
// Skipping a tick because there was no activity is a normal, expected
// outcome, not an error condition, so it does not touch failCount or log
// anything.
//
// Chtimes errors (when a touch IS attempted) are logged (not silently
// dropped) but do not stop the heartbeat loop and do not mark us as dead
// — a transient/read-only-FS Chtimes failure while we are alive and
// still hold the OS lock must never cause another process to conclude it
// can steal the session. Logging is throttled so a persistently-failing
// filesystem doesn't spam the log once every tick for the lifetime of a
// long session.
func heartbeat(path string, done <-chan struct{}, active *atomic.Bool) {
	t := time.NewTicker(lockHeartbeatInterval)
	defer t.Stop()
	var failCount atomic.Int64
	for {
		select {
		case <-done:
			return
		case <-t.C:
			// Consume ("swap to false") the activity recorded since the
			// last tick. This is the sole gate: no activity this window
			// means no Chtimes this tick, and the window resets either
			// way for the next tick.
			if !active.Swap(false) {
				continue
			}
			now := time.Now()
			if err := os.Chtimes(path, now, now); err != nil {
				n := failCount.Add(1)
				// Log the 1st, 2nd, 4th, 8th, 16th... failure so a
				// persistent failure doesn't flood the log but is still
				// visible quickly and periodically.
				if n == 1 || n&(n-1) == 0 {
					slog.Warn("session lock: heartbeat failed to touch lock file mtime",
						"path", path, "err", err, "consecutive_failures", n)
				}
			} else {
				failCount.Store(0)
			}
		}
	}
}

// logStaleDiagnostics logs (without touching the file) whether the lock
// at path looks stale by heartbeat mtime or by holder-PID liveness. This
// is purely informational — it feeds `sessions locks`/`sessions why`
// style visibility into "why did my acquire attempt fail" and helps an
// operator notice a wedged holder. It must NEVER remove the file or
// otherwise influence acquireSessionLockFile's decision: that decision
// is made exclusively by actually attempting the OS-level lock. See the
// package doc and acquireSessionLockFile for the full rationale (mtime
// can go stale for a live holder — GC pause, throttled heartbeat log,
// transient Chtimes failure — while the OS lock is still held).
func logStaleDiagnostics(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	age := time.Since(info.ModTime())
	if age <= lockHeartbeatInterval {
		return
	}
	pid := readLockHolderPID(path)
	pidAlive := pid > 0 && isProcessAlive(pid)
	if age > lockStaleDuration || (pid > 0 && !pidAlive) {
		slog.Debug("session lock: existing lock file looks stale by heartbeat/PID, will attempt real OS lock to confirm",
			"path", path,
			"age_seconds", int(age.Seconds()),
			"holder_pid", pid,
			"holder_pid_alive", pidAlive)
	}
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
	// Live reports whether the lock holder is still (probably) alive.
	// Primarily Age < liveThreshold — a healthy owner is touching the
	// heartbeat. But per task #228, mtime alone is not sufficient: the
	// heartbeat's mtime touch is gated on real RecordActivity() calls
	// (task #213/#214), and the stream watchdog that drives those calls
	// during a tool-in-flight only fires roughly every 30s (task #222,
	// streamWatchdogTick) — larger than the 20s liveThreshold callers
	// typically pass. That leaves a real window where a perfectly healthy
	// session blocked on one long tool call looks stale by mtime alone.
	// When mtime looks stale, InspectSessionLock falls back to a real PID
	// liveness probe (session.IsProcessAlive) before concluding the
	// holder is dead — same principle sessions_watch.go's
	// combinedLockLiveness already established for the `sessions watch`
	// consumer. That PID-liveness fallback is itself bounded by
	// maxPidFallbackAge (60m): once a stale lock is older than that, a
	// live-looking PID is no longer trusted and Live falls back to
	// mtime-only, so an OS PID reused by an unrelated process long after
	// the real holder died can't pin Live: true forever.
	Live bool
}

// InspectSessionLock reads the lock file for `sessionID` under `dataDir`
// without acquiring it. Safe to call from any process — no side effects.
// `liveThreshold` defines how fresh the heartbeat must be to count as
// "live" by mtime alone (callers typically pass 20s — the same expiry the
// heartbeat loop uses; see TryAcquireSessionLock comments).
//
// mtime freshness is only the fast path, not the whole story — see the
// Live field's doc comment on LockState for why: task #228 found that a
// healthy, tool-busy session can transiently look stale by mtime alone
// (heartbeat is activity-gated, the stream watchdog's tick that supplies
// that activity can be slower than liveThreshold). When mtime already
// looks stale, this falls back to probing whether the recorded PID is
// still a live OS process (session.IsProcessAlive) before reporting
// Live: false — mirroring the same "don't trust mtime alone" fallback
// sessions_watch.go's combinedLockLiveness already uses. The PID probe is
// only attempted once mtime is already stale, so the common case (mtime
// fresh) pays no extra cost.
//
// The PID-fallback itself is bounded by maxPidFallbackAge (60 minutes):
// once the lock's mtime is older than that, a live-looking PID is no
// longer trusted and Live falls back to mtime-only, even if the recorded
// PID happens to currently belong to some live OS process. Without this
// bound, a stale lock left behind by a killed/crashed holder would
// report Live: true forever the moment the OS happened to recycle that
// exact PID number for an unrelated process — turning a false-negative
// fix (task #228) into an unbounded false-positive. See maxPidFallbackAge's
// doc comment for how the bound was chosen.
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
	live := age < liveThreshold
	if !live && pid > 0 && age < maxPidFallbackAge {
		// mtime looks stale — task #228: fall back to a real PID liveness
		// check before concluding the holder is dead, since the
		// heartbeat's mtime touch is now gated on real activity and a
		// healthy session blocked on one long tool call can look stale
		// here even though it is still genuinely running. Bounded by
		// maxPidFallbackAge (task #235) so a PID the OS has since recycled
		// for an unrelated process can't keep reporting Live: true forever
		// once the lock is old enough that no genuinely healthy holder
		// could still be running.
		live = IsProcessAlive(pid)
	}
	return LockState{
		Exists: true,
		PID:    pid,
		Age:    age,
		Live:   live,
	}
}

// pidSidecarPath returns the companion, never-locked file that carries a
// live copy of the PID/timeout normally stamped into the lock file itself.
// See readLockFile's doc comment for why this sidecar exists.
func pidSidecarPath(lockPath string) string {
	return lockPath + ".pid"
}

// writePIDSidecar (re)writes the sidecar file next to a session lock with
// the holder's PID and (optionally) its --timeout budget in seconds,
// mirroring what's stamped into the lock file itself. Unlike the lock
// file, this file is opened, written, and closed immediately — no lock is
// ever held on it — so any other process can read it with a plain
// os.ReadFile at any time, including while the lock file's own mandatory
// Windows range-lock makes IT unreadable (see readLockFile).
//
// Best-effort: a write failure only degrades diagnosability (PID shown as
// 0 to a concurrent reader) and is logged, never returned as an error —
// the OS lock on the primary file remains the sole source of truth for
// correctness, exactly like the mtime heartbeat.
func writePIDSidecar(lockPath string, pid int, timeoutSec int64) {
	content := strconv.Itoa(pid) + "\n"
	if timeoutSec > 0 {
		content += strconv.FormatInt(timeoutSec, 10) + "\n"
	}
	if err := os.WriteFile(pidSidecarPath(lockPath), []byte(content), 0o644); err != nil {
		slog.Warn("session lock: failed to write PID sidecar", "path", lockPath, "err", err)
	}
}

// readLockFile returns (PID, timeoutSec) for a session lock. Both default
// to 0 on any parse error — backward compatible with old one-line files.
//
// Windows note: tryLockFile takes a LockFileEx exclusive lock over the
// WHOLE file for the entire lifetime of the holder. Unlike POSIX advisory
// locks, Windows enforces this as a MANDATORY lock — any plain read from a
// different handle/process into that byte range (which is exactly what
// os.ReadFile does) fails with a sharing/lock violation for as long as the
// holder is alive, not just during a brief write race. So reading the lock
// file itself would return (0, 0) for an ACTIVELY RUNNING session on
// Windows — not because the PID is unknown, but because the mandatory lock
// blocks the read outright. `sessions kill` hitting exactly this case (a
// genuinely live holder) used to be unable to read a PID to kill at all.
//
// To fix that, TryAcquireSessionLock also stamps the same PID/timeout into
// a sidecar file (pidSidecarPath) that is never locked. Prefer that
// sidecar; fall back to the primary lock file only when no sidecar exists
// (an old lock file predating this change, or — in tests — a lock file
// written directly via os.WriteFile rather than through
// TryAcquireSessionLock).
func readLockFile(path string) (int, int64) {
	bts, err := os.ReadFile(pidSidecarPath(path))
	if err != nil {
		bts, err = os.ReadFile(path)
		if err != nil {
			return 0, 0
		}
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
