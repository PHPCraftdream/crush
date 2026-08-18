//go:build windows

package cliprovider

import (
	"log/slog"
	"os"
	"os/exec"

	"github.com/charmbracelet/crush/internal/session"
)

// trackChildTree registers the freshly started CLI child's whole tree
// for teardown: session.TrackProcessTree assigns it a kill-on-close Job
// Object that session.KillProcess terminates, reaching grandchildren
// the PPID walk behind taskkill /T cannot see (MSYS2/Git-Bash
// intermediary parents). Returns the pid for the matching
// UntrackProcessTree call.
//
// A failure to track (pre-Win8 nested-job limits, exotic job silos) is
// logged and otherwise ignored: the taskkill fallback inside
// KillProcess still applies, which is exactly the pre-Job-Object
// behavior.
func trackChildTree(proc *os.Process) int {
	if proc == nil {
		return 0
	}
	if err := session.TrackProcessTree(proc.Pid); err != nil {
		slog.Warn("cliprovider: could not track child process tree; falling back to taskkill teardown", "pid", proc.Pid, "err", err)
	}
	return proc.Pid
}

// configureChildProcessGroup is a no-op on Windows: there are no POSIX
// process groups, and tree teardown is the Job Object assigned by
// trackChildTree instead.
func configureChildProcessGroup(*exec.Cmd) {}
