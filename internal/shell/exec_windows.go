//go:build windows

package shell

import (
	"os/exec"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// defaultKillTimeout matches mvdan's DefaultExecHandler default.
const defaultKillTimeout = 2 * time.Second

// isolateProcess is a no-op on Windows. Session isolation via Setsid is a
// Unix-only concept; Windows process-group handling is left to mvdan's
// default handler, which already creates child processes adequately
// detached for our purposes.
func isolateProcess(_ *exec.Cmd) {}

// processGroupExecHandler returns interp.DefaultExecHandler on Windows.
func processGroupExecHandler(killTimeout time.Duration) interp.ExecHandlerFunc {
	return interp.DefaultExecHandler(killTimeout)
}
