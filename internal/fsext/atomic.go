package fsext

import (
	"fmt"
	"os"
	"path/filepath"
)

// AtomicWriteFile writes data to path atomically: write to a unique temp file
// in the same directory, fsync, then rename over the destination. A crash
// (kill -9, OOM, power loss) cannot leave the destination half-written —
// either the old content is still there, or the new content is fully there.
//
// Required for parallel-process workflows where the orchestrator may SIGKILL
// children that exceed budget: without this, a write/edit tool call killed
// mid-flight truncates the user's file and silently loses content (the DB
// history snapshot is also taken AFTER the write, so there's no recovery).
//
// On Windows the rename can occasionally fail if another process has the
// destination open with a sharing-violation lock; the temp file is cleaned
// up and the original error returned so the caller surfaces it as a normal
// tool error. We don't retry because retries can mask real bugs.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("atomic write: create temp: %w", err)
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("atomic write: write temp: %w", err)
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("atomic write: chmod temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("atomic write: fsync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("atomic write: close temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("atomic write: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
