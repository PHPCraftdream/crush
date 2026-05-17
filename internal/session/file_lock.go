package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileLock is a generic exclusive lock on an arbitrary path on disk.
// Use AcquireFileLock for blocking acquisition (waits for the lock) or
// TryAcquireFileLock for non-blocking (returns immediately with an error
// if contended).
//
// Backed by the same OS primitives as SessionLock (flock on POSIX,
// LockFileEx on Windows). The lock is released automatically when the
// process dies; explicit Release closes the file handle.
//
// Use case (callers added 2026-05): the cliprovider qwen/gemini MCP-id
// files and their respective ~/.{qwen,gemini}/settings.json edits are
// read-modify-write across multiple crush processes that share a working
// directory or a home directory. Without a lock, two processes racing to
// generate the same MCP ID end up with split-brain (loser's in-memory
// ID points to no server) and concurrent settings.json edits clobber
// each other.
type FileLock struct {
	Path string
	f    *os.File
}

// Release unlocks and closes the lock file. Safe to call on nil.
func (l *FileLock) Release() error {
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

// TryAcquireFileLock opens lockPath (creating it if needed) and takes an
// exclusive non-blocking lock. Returns immediately with an error if
// another process holds the lock. The lock file directory is created if
// it does not exist.
func TryAcquireFileLock(lockPath string) (*FileLock, error) {
	f, err := openLockFile(lockPath)
	if err != nil {
		return nil, err
	}
	if err := tryLockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("file lock contended (%s): %w", lockPath, err)
	}
	return &FileLock{Path: lockPath, f: f}, nil
}

// AcquireFileLock opens lockPath (creating it if needed) and blocks
// until it can take an exclusive lock. Use for short critical sections
// that mutate a shared file (settings.json, generated-ID files, etc.).
// Combine with deferred Release.
//
// WARNING: blocks indefinitely. If the holding process is wedged
// (debugger attached, suspended, frozen on a network FS), this call
// never returns. Production parallel-process callers must use
// AcquireFileLockContext with a timeout.
func AcquireFileLock(lockPath string) (*FileLock, error) {
	f, err := openLockFile(lockPath)
	if err != nil {
		return nil, err
	}
	if err := lockFileBlocking(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("file lock blocking-acquire failed (%s): %w", lockPath, err)
	}
	return &FileLock{Path: lockPath, f: f}, nil
}

// AcquireFileLockContext is the bounded-wait variant of AcquireFileLock.
// It polls TryAcquireFileLock with exponential backoff until success or
// ctx is done; returns ctx.Err() on timeout/cancel. Use this in any
// parallel-process path where a hung holder must not freeze the whole
// fleet — see for example the MCP register/deregister flow.
//
// Polling (rather than running blocking flock in a goroutine + select
// on ctx) is intentional: a blocking flock that succeeds after ctx
// cancellation would leak the lock file handle held by an orphan
// goroutine, defeating the whole timeout. Backoff schedule is
// 25ms, 50ms, 100ms, 250ms, then capped at 500ms.
func AcquireFileLockContext(ctx context.Context, lockPath string) (*FileLock, error) {
	backoff := 25 * time.Millisecond
	const maxBackoff = 500 * time.Millisecond
	for {
		l, err := TryAcquireFileLock(lockPath)
		if err == nil {
			return l, nil
		}
		// Surface IO/permission errors immediately; only retry on
		// genuine lock contention. TryAcquireFileLock wraps contention
		// in a "file lock contended" message; everything else is fatal.
		// We rely on this distinction by checking for the open-file
		// failure path: if openLockFile failed, the error chain does
		// not start with our "contended" wrapper.
		if !isContentionError(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("file lock %s: %w", lockPath, ctx.Err())
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// isContentionError returns true when err comes from TryAcquireFileLock
// reporting that another holder has the lock (as opposed to IO or
// permission failures that should not be retried).
func isContentionError(err error) bool {
	if err == nil {
		return false
	}
	// TryAcquireFileLock formats contention as "file lock contended (...)".
	// Any error containing that marker is retryable.
	return strings.Contains(err.Error(), "file lock contended")
}

func openLockFile(lockPath string) (*os.File, error) {
	if lockPath == "" {
		return nil, fmt.Errorf("file lock: empty path")
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("file lock: create parent dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("file lock: open %s: %w", lockPath, err)
	}
	return f, nil
}
