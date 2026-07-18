//go:build windows

package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// helperEnvVar switches this test binary into one of two "leaky process
// tree" helper modes (see TestMain) instead of running the normal test
// suite. This is the standard Go idiom for reproducing real OS-level
// process/handle-inheritance behavior deterministically (the same pattern
// os/exec's own tests use), rather than depending on external tools like
// cmd.exe's `start` or PowerShell's `Start-Process`, whose handle
// inheritance semantics are not guaranteed to be stable across Windows
// versions or CI images.
const helperEnvVar = "CRUSH_SHELL_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnvVar) {
	case "level1":
		runLevel1Helper()
		return
	case "level2":
		runLevel2Helper()
		return
	}
	os.Exit(m.Run())
}

// runLevel1Helper models the "direct child" a background shell command
// tracks — e.g. a dev-server or proxy process. It forks a grandchild
// (level 2) that inherits its own stdout/stderr handles without waiting for
// it, then stays alive (like a real long-running server) until killed.
func runLevel1Helper() {
	self, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	cmd := exec.CommandContext(context.Background(), self)
	cmd.Env = append(os.Environ(), helperEnvVar+"=level2")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(1)
	}
	// Fire-and-forget: do NOT Wait() on the grandchild — it must outlive
	// this process's own death exactly like an orphaned worker would.
	fmt.Println("level1-alive")
	time.Sleep(30 * time.Second)
}

// runLevel2Helper models the orphaned grandchild: it inherited level 1's
// stdout/stderr handle and keeps it open long after level 1 is gone.
func runLevel2Helper() {
	fmt.Println("grandchild-alive")
	time.Sleep(60 * time.Second)
}

// TestBackgroundShellManager_Kill_TreeKillsOrphanedGrandchild_Windows proves
// killing a background shell tree-kills a grandchild the direct child
// spawned, instead of hanging.
//
// Before the exec_windows.go fix, cancelling ctx only called
// cmd.Process.Signal(os.Kill) on the DIRECT child (level 1 here). Since
// cmd.Stdout/Stderr are plain io.Writers, os/exec backs them with an OS
// pipe whose copy-goroutine cmd.Wait() joins — that goroutine only sees EOF
// once every handle holder closes it. The still-alive grandchild (level 2)
// keeps its inherited copy of the handle open even after level 1 dies, so
// cmd.Wait() — and therefore execCommon/runner.Run, and therefore
// bgShell.done — never unblocks on its own; against the old code this test
// would hang for the grandchild's full sleep (60s) instead of returning
// promptly. This is exactly the failure mode observed in production: a
// job_kill call against a proxy process that had forked a worker blocked
// for the full 15-minute tool watchdog instead of returning immediately.
func TestBackgroundShellManager_Kill_TreeKillsOrphanedGrandchild_Windows(t *testing.T) {
	t.Parallel()

	self, err := os.Executable()
	require.NoError(t, err)

	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Leading VAR=value is POSIX inline-assignment syntax: mvdan/sh scopes
	// it to this one command's environment without polluting the shell's
	// own. filepath.ToSlash avoids POSIX backslash-escape ambiguity in the
	// Windows path (see the package doc comment in shell.go).
	command := fmt.Sprintf("%s=level1 %s", helperEnvVar, filepath.ToSlash(self))

	bgShell, err := manager.Start(t.Context(), workingDir, nil, command, "")
	require.NoError(t, err)

	// Wait until BOTH helper levels have actually reported alive before
	// killing — otherwise we might kill before the grandchild even exists,
	// which wouldn't reproduce the bug.
	require.Eventually(t, func() bool {
		stdout, _, _, _ := bgShell.GetOutput()
		return strings.Contains(stdout, "level1-alive") && strings.Contains(stdout, "grandchild-alive")
	}, 5*time.Second, 50*time.Millisecond, "helper processes did not report alive")

	start := time.Now()
	err = manager.Kill(context.Background(), bgShell.ID)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Less(t, elapsed, 10*time.Second,
		"Kill must tree-kill the orphaned grandchild and return promptly; "+
			"without tree-kill it hangs until the grandchild's own sleep ends")
}
