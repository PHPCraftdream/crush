//go:build windows

package shell

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/charmbracelet/crush/internal/platform"
	"github.com/charmbracelet/crush/internal/session"
	"mvdan.cc/sh/v3/interp"
)

// defaultKillTimeout matches mvdan's DefaultExecHandler default.
const defaultKillTimeout = 2 * time.Second

// isolateProcess on Windows just hides the console window (session
// isolation via Setsid is a Unix-only concept; Windows process-group
// handling is left to our own exec handler below, which already creates
// child processes adequately detached for our purposes). Used by the
// shebang-script fallback path in dispatch.go.
func isolateProcess(cmd *exec.Cmd) { platform.HideConsoleWindow(cmd) }

// processGroupExecHandler returns a Windows exec handler that behaves like
// interp.DefaultExecHandler but additionally sets SysProcAttr.HideWindow —
// upstream's DefaultExecHandler builds a bare exec.Cmd with no
// SysProcAttr, so every single bash-tool command spawns a NEW, briefly
// visible console window when the crush process itself has no console to
// share (see cmd.maybeDetachConsole's doc comment for why crush ends up
// console-less on a detached/orchestrator launch). HideWindow: true sets
// the Windows CREATE_NO_WINDOW creation flag, which suppresses that window
// unconditionally — independent of whatever console state crush itself is
// in, so this is the correct fix at the source rather than trying to give
// crush a console for children to inherit.
func processGroupExecHandler(killTimeout time.Duration) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
		if err != nil {
			fmt.Fprintln(hc.Stderr, err)
			return interp.ExitStatus(127)
		}
		cmd := exec.Cmd{
			Path:        path,
			Args:        args,
			Env:         execEnvList(hc.Env),
			Dir:         hc.Dir,
			Stdin:       hc.Stdin,
			Stdout:      hc.Stdout,
			Stderr:      hc.Stderr,
			SysProcAttr: &syscall.SysProcAttr{HideWindow: true},
		}

		err = cmd.Start()
		if err == nil {
			stopf := context.AfterFunc(ctx, func() {
				// cmd.Stdout/Stderr here are plain io.Writers (our
				// syncBuffer, not *os.File), so os/exec backs them with an
				// OS pipe and a copy-goroutine that cmd.Wait() joins. That
				// goroutine only sees EOF once EVERY process holding the
				// pipe's write end closes it. A plain Process.Signal —
				// Go doesn't support sending Interrupt on Windows anyway,
				// so this was always a hard kill — only terminates this
				// DIRECT child; any grandchild it spawned (a dev-server or
				// proxy forking a worker process is a common real case)
				// keeps its inherited copy of the handle open, so
				// cmd.Wait() then hangs indefinitely instead of returning
				// once the direct child is gone. Tree-kill via taskkill /T
				// instead, matching session.KillProcess's approach to the
				// exact same class of problem in `crush sessions kill` —
				// it kills the direct process AND everything Windows
				// recorded as its descendant.
				if cmd.Process != nil {
					_ = session.KillProcess(cmd.Process.Pid)
				}
			})
			defer stopf()
			err = cmd.Wait()
		}

		switch err := err.(type) {
		case *exec.ExitError:
			return interp.ExitStatus(err.ExitCode())
		case *exec.Error:
			fmt.Fprintf(hc.Stderr, "%v\n", err)
			return interp.ExitStatus(127)
		default:
			return err
		}
	}
}
