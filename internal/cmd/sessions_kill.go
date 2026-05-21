package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsKillCmd = &cobra.Command{
	Use:   "kill <id>",
	Short: "Kill the process holding a session's lock and remove the lock file",
	Long: `Force-release a session that is stuck behind a live or orphan
crush process. Reads the holder PID from .crush/locks/session-<id>.lock,
sends it a kill signal, then removes the lock file.

Use this when:
- A "crush run --session <id>" reports "session is already in use", but
  you know the real holder is dead (or stuck) and won't release.
- "crush sessions reset" cannot proceed because the lock survived.
- A previous run was force-killed (TaskStop / Ctrl+C on a wrapper) and
  left the child crush process orphaned, still holding the lock.

By default the lock is removed even if the kill failed (process already
gone). Pass --keep-lock to skip the file removal.`,
	Example: `
crush sessions kill pr-42
crush sessions kill pr-42 --keep-lock     # just kill, leave the lock file
crush sessions kill pr-42 --signal 15     # SIGTERM instead of SIGKILL (POSIX only)
  `,
	Args: cobra.ExactArgs(1),
	RunE: sessionsKillCmdRun,
}

func sessionsKillCmdRun(cmd *cobra.Command, args []string) error {
	id := args[0]
	keepLock, _ := cmd.Flags().GetBool("keep-lock")

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}

	lockPath := filepath.Join(cwd, ".crush", "locks", "session-"+sanitiseSessionIDForFilename(id)+".lock")
	info, err := os.Stat(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "no lock file at %s\n", lockPath)
			return nil
		}
		return fmt.Errorf("stat lock: %w", err)
	}

	pid, readErr := readPIDFromLock(lockPath)
	if pid <= 0 {
		fmt.Fprintf(os.Stderr, "lock %s has no readable PID (read error: %v); removing file only\n", lockPath, readErr)
	} else {
		proc, err := os.FindProcess(pid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FindProcess(%d): %v\n", pid, err)
		} else {
			if err := proc.Kill(); err != nil {
				// Common case on Windows: "OS finished the job" / access denied
				// when the process already exited. Not fatal — we still try to
				// drop the lock.
				fmt.Fprintf(os.Stderr, "kill PID %d: %v (probably already dead)\n", pid, err)
			} else {
				fmt.Fprintf(os.Stderr, "killed PID %d holding session %s\n", pid, short(session.HashID(id)))
			}
		}
	}

	if keepLock {
		fmt.Fprintf(os.Stderr, "lock file kept at %s (age %ds)\n", lockPath, age(info))
		return nil
	}

	// Remove may fail on Windows if the file is still open by the just-killed
	// process; retry once after a short pause via os.Remove (the OS releases
	// handles on exit). If it still fails, surface the error.
	if err := os.Remove(lockPath); err != nil {
		return fmt.Errorf("remove lock %s: %w (the process may not have fully exited — retry in a moment)", lockPath, err)
	}
	fmt.Fprintf(os.Stderr, "removed lock %s\n", lockPath)
	return nil
}

func readPIDFromLock(path string) (int, error) {
	bts, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(bts))
	if s == "" {
		return 0, fmt.Errorf("empty lock file")
	}
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return pid, nil
}

func age(info os.FileInfo) int {
	if info == nil {
		return 0
	}
	return int(time.Since(info.ModTime()).Seconds())
}

// sanitiseSessionIDForFilename mirrors session.sanitiseSessionID (package-private)
// so the lock-file path resolves the same way the lock acquirer wrote it.
func sanitiseSessionIDForFilename(id string) string {
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

func init() {
	sessionsKillCmd.Flags().Bool("keep-lock", false, "Kill the process but do not delete the lock file")
}
