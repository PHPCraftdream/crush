//go:build !windows

package shell

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// defaultKillTimeout matches mvdan's DefaultExecHandler default. Extracted
// so the coupling to upstream is explicit rather than a buried literal.
const defaultKillTimeout = 2 * time.Second

// isolateProcess sets SysProcAttr so the child runs in its own session,
// fully detached from Crush's controlling terminal and process group.
//
// Fork context: Crush has no interactive TTY of its own (the upstream TUI
// was replaced by the web UI), so the original "don't let zsh steal the
// terminal" motivation is moot for us. What still matters for agent-tooling
// is the second half: a child must not be able to deliver SIGINT/SIGTERM to
// Crush's own process group, and — paired with the negative-PID kill in
// processGroupExecHandler — a cancelled `crush run` step must take its whole
// subtree (build → compiler → spawned helpers) down with it instead of
// leaking orphaned grandchildren.
func isolateProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// processGroupExecHandler returns an ExecHandlerFunc that replaces
// interp.DefaultExecHandler with one that fully isolates child processes
// from Crush's session and signals the entire child process group on
// cancellation.
//
// The implementation mirrors interp.DefaultExecHandler with two additions:
// isolateProcess(&cmd) after construction, and negative-PID signal targeting
// in the cancellation callback so grandchildren spawned by the command are
// reaped along with it.
//
// Grandchild-holds-stdio wedge (task #313, investigated as the exec_unix
// counterpart to cliprovider's grandchild-holds-stderr fix, commit
// 271d4505): cmd.Stdout/cmd.Stderr here are plain io.Writers, not *os.File,
// so os/exec backs them with an OS pipe plus a copy-goroutine that
// cmd.Wait() joins — that goroutine only sees EOF once EVERY process
// holding the pipe's write end has closed it, not just this direct child.
// A shell command that backgrounds a grandchild without waiting for it
// (`server & echo done`, a common pattern for starting a dev server or
// proxy from an agent) leaves that grandchild holding the SAME inherited
// stdout/stderr fd open long after the direct child (this cmd) exits, so
// an unbounded cmd.Wait() would hang until the grandchild itself closes
// that fd — exactly cliprovider's bug, just one layer removed.
//
// This IS already bounded here, unlike the pre-fix cliprovider code,
// because of how isolateProcess's Setsid interacts with normal Unix
// process-group inheritance: Setsid makes this cmd (say pid X) both a new
// session leader AND its own process group leader (pgid X). Any process
// IT forks — including a `foo &` backgrounded inside a shell script this
// cmd runs — inherits pgid X via fork() unless that process explicitly
// calls setpgid/setsid itself (ordinary shells running non-interactively,
// i.e. without job control, do not). So syscall.Kill(-X, ...) in the
// ctx-cancellation callback below reaches the grandchild too, even after X
// itself has already exited (a pgid remains a valid signal target as long
// as ANY process is still a member, regardless of whether the original
// leader is still alive) — which closes the grandchild's fd, which EOFs
// the pipe, which unblocks cmd.Wait(). See
// TestProcessIsolation_GrandchildHoldingStdoutDoesNotWedgeForever
// (isolation_unix_test.go) for the regression test proving this bound
// holds for exactly the "direct child exits, backgrounded grandchild
// lingers holding stdio" shape.
//
// Which of the two signals below actually does the unwedging, for the
// ordinary `foo &` case: NOT the initial SIGINT. POSIX (XCU 2.11)
// requires a non-job-control shell to set SIGINT/SIGQUIT to SIG_IGN for
// asynchronous (backgrounded) commands, and bash/dash/ksh all do this —
// so the SIGINT below is delivered and ignored by a plain background job.
// It's the SIGKILL sent killTimeout later that actually reaches and kills
// it, unblocking cmd.Wait() at that point (bounded by killTimeout, not
// instant — see TestProcessIsolation_GrandchildHoldingStdoutDoesNotWedgeForever's
// 4s assertion window, not the ctx deadline alone).
//
// Known accepted gaps, NOT covered by the above:
//   - A grandchild that deliberately escapes this process group by
//     calling setsid() itself (the classic double-fork daemonize
//     pattern, or an explicit `setsid cmd &` shell invocation) is no
//     longer a member of pgid X and will not receive the negative-PID
//     kill.
//   - Same escape, different trigger: `bash -c 'set -m; foo &'` enables
//     job control non-interactively, which puts the backgrounded job in
//     its OWN process group without any setsid() call at all.
//
// Either way cmd.Wait() would then block until ctx-independent
// termination. Both are narrower and more deliberate than the ordinary
// backgrounded-job case above (`nohup`/`disown`/plain `&` without `set -m`
// do NOT get their own process group and remain covered); fixing them
// would require walking the full process tree (as Windows' taskkill /T
// does, see exec_windows.go) rather than relying on a single process-group
// signal, and was judged out of scope for this LOW-priority investigation
// since neither has been observed in practice, unlike cliprovider's
// grandchild bug which was a real, reproducible CI failure.
//
// Also worth noting: all of the above protection requires ctx to actually
// be cancelled or deadlined — it is the ctx-cancellation callback that
// does the killing. A caller that starts this exec handler's shell with
// context.Background() (as internal/agent/tools/bash.go's foreground path
// does) has no independent timeout of its own here; in practice it's
// bounded by whatever OUTER mechanism eventually cancels that context
// (turn cancellation, the stream watchdog, `job_kill` for a backgrounded
// tool run), not by anything in this file.
func processGroupExecHandler(killTimeout time.Duration) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
		if err != nil {
			fmt.Fprintln(hc.Stderr, err)
			return interp.ExitStatus(127)
		}

		cmd := exec.Cmd{
			Path:   path,
			Args:   args,
			Env:    execEnvList(hc.Env),
			Dir:    hc.Dir,
			Stdin:  hc.Stdin,
			Stdout: hc.Stdout,
			Stderr: hc.Stderr,
		}
		isolateProcess(&cmd)

		err = cmd.Start()
		if err == nil {
			stopf := context.AfterFunc(ctx, func() {
				if killTimeout <= 0 {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					return
				}
				// Signal the child's process group (negative PID) so
				// grandchildren also receive it.
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
				time.Sleep(killTimeout)
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			})
			defer stopf()

			err = cmd.Wait()
		}

		return exitStatusFromError(ctx, hc.Stderr, err)
	}
}

// exitStatusFromError translates an exec error into an interp exit status,
// matching the conventions of interp.DefaultExecHandler. Extracted so the
// platform-specific exec handler stays focused on isolation mechanics.
func exitStatusFromError(ctx context.Context, stderr io.Writer, err error) error {
	if err == nil {
		return nil
	}
	switch err := err.(type) {
	case *exec.ExitError:
		if status, ok := err.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return interp.ExitStatus(128 + uint8(status.Signal()))
		}
		return interp.ExitStatus(uint8(err.ExitCode()))
	case *exec.Error:
		fmt.Fprintf(stderr, "%v\n", err)
		return interp.ExitStatus(127)
	default:
		return err
	}
}
