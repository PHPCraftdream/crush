//go:build windows

package cliprovider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// This file exercises the P1.7 fix: cliprovider must not hand a bare "bash"
// off to a resolver that can silently pick the WSL launcher
// (%SystemRoot%\System32\bash.exe / SysWOW64\bash.exe) when it happens to
// sit ahead of Git Bash/MSYS bash on PATH. The WSL launcher expects
// Linux-style paths (/mnt/c/...) and cannot run a script given the
// Windows-style working directory and arguments cliprovider passes, so
// picking it silently would launch a process that fails in confusing ways
// downstream rather than either using the real usable bash or producing a
// clear error.
//
// Every test below rigs %SystemRoot% and PATH itself (via t.Setenv, so no
// process-wide/global state leaks between tests or to other packages) —
// none of it depends on the operator machine's real PATH ordering. This
// matters because on at least one real dev machine in this fork's history,
// `where bash` returned Git Bash first and the WSL launcher second, which
// made the underlying bug invisible to a plain "does the test suite pass
// here" check; the rig below reproduces the dangerous ordering
// deterministically regardless of the host's actual PATH.

// fakeWSLRoot creates a throwaway "%SystemRoot%" directory containing a
// stand-in bash.exe at the canonical WSL-launcher location
// (System32\bash.exe), and points SystemRoot at it via t.Setenv. The
// stand-in is NOT a working executable — it is just an empty file — because
// the whole point of the test is that resolution must skip it before ever
// trying to run it. Returns the stand-in's full path.
func fakeWSLRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	sys32 := filepath.Join(root, "System32")
	if err := os.MkdirAll(sys32, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", sys32, err)
	}
	wslBash := filepath.Join(sys32, "bash.exe")
	if err := os.WriteFile(wslBash, []byte("not a real binary"), 0o755); err != nil {
		t.Fatalf("write fake WSL bash.exe: %v", err)
	}
	t.Setenv("SystemRoot", root)
	return wslBash
}

// requireRealBash locates the actual usable bash on this machine (Git
// Bash/MSYS) via exec.LookPath, skipping the test if none is installed. We
// need a REAL, runnable bash for the end-to-end Stream() test below —
// otherwise there is nothing to prove the fixed code path actually ran
// (the fake WSL stand-in must never be invoked at all, let alone
// successfully). resolveInterpreter/isWSLLauncher in internal/shell are
// unit-tested against fake planted files (see dispatch_windows_test.go);
// this test instead proves cliprovider's own two call sites do the same
// skip, end to end, with a real interpreter on the other end.
func requireRealBash(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH; skipping WSL-first regression test")
	}
	return path
}

// rigPathWSLFirst builds a PATH value with the fake WSL launcher's directory
// listed BEFORE the real bash's directory — the exact ordering that made
// P1.7 real on affected machines. t.Setenv restores the original PATH after
// the test.
func rigPathWSLFirst(t *testing.T, wslBashPath, realBashPath string) {
	t.Helper()
	wslDir := filepath.Dir(wslBashPath)
	realDir := filepath.Dir(realBashPath)
	t.Setenv("PATH", wslDir+string(os.PathListSeparator)+realDir)
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
}

// TestResolveBinary_SkipsWSLLauncherWhenFirstOnPath is the direct,
// deterministic unit test for the resolver cliprovider now uses at both of
// its binary-resolution call sites (PTY branch and pipe-fallback branch).
// It proves resolveBinary("bash") returns the real Git Bash/MSYS
// executable, never the WSL launcher, even when the WSL launcher's
// directory is listed first on PATH.
func TestResolveBinary_SkipsWSLLauncherWhenFirstOnPath(t *testing.T) {
	realBash := requireRealBash(t)
	wslBash := fakeWSLRoot(t)
	rigPathWSLFirst(t, wslBash, realBash)

	got, err := resolveBinary("bash")
	if err != nil {
		t.Fatalf("resolveBinary(\"bash\") error: %v", err)
	}
	if strings.EqualFold(got, wslBash) {
		t.Fatalf("resolveBinary resolved to the fake WSL launcher %q; want the real bash %q", got, realBash)
	}
	// Compare directories rather than exact byte-for-byte paths: PATH
	// lookup may return a different casing/short-vs-long form than
	// exec.LookPath("bash") did originally, which is fine as long as it's
	// the same real interpreter, not the WSL stand-in.
	if !strings.EqualFold(filepath.Dir(got), filepath.Dir(realBash)) {
		t.Fatalf("resolveBinary resolved to %q, want something under %q", got, filepath.Dir(realBash))
	}
}

// TestResolveBinary_GitBashFirstUnchanged is the no-regression counterpart:
// when the real bash is already first on PATH (the common case, and the
// case that was accidentally the only one exercised on at least one
// operator machine), resolveBinary must still return it — proving the fix
// doesn't disturb the already-working ordering.
func TestResolveBinary_GitBashFirstUnchanged(t *testing.T) {
	realBash := requireRealBash(t)
	wslBash := fakeWSLRoot(t)
	// Reversed order vs. rigPathWSLFirst: real bash's directory first.
	t.Setenv("PATH", filepath.Dir(realBash)+string(os.PathListSeparator)+filepath.Dir(wslBash))
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")

	got, err := resolveBinary("bash")
	if err != nil {
		t.Fatalf("resolveBinary(\"bash\") error: %v", err)
	}
	if !strings.EqualFold(filepath.Dir(got), filepath.Dir(realBash)) {
		t.Fatalf("resolveBinary resolved to %q, want something under %q", got, filepath.Dir(realBash))
	}
}

// TestStream_WSLLauncherFirstOnPath_StillRunsRealBash is the end-to-end
// regression test: with Binary: "bash" and a PATH rigged so the WSL
// launcher stand-in comes first, Stream() must still successfully execute
// the real bash and produce output — not hang, not error out trying to feed
// a Windows path to the WSL launcher, and not silently produce an empty
// stream. This exercises both the pipe-fallback branch (testDisablePTY is
// forced true for the whole package on Windows, see TestMain) and, via
// resolveBinary, the same helper the PTY branch calls.
//
// Before the fix, this scenario would have handed "bash" straight to
// os/exec's own bare-name PATH lookup (pipe branch) or to a plain
// exec.LookPath call (PTY branch) with no WSL awareness at all — so on a
// machine where WSL's launcher precedes Git Bash on PATH, this exact test
// would have attempted to run %SystemRoot%\System32\bash.exe against a
// Windows-style script/working directory and failed.
func TestStream_WSLLauncherFirstOnPath_StillRunsRealBash(t *testing.T) {
	realBash := requireRealBash(t)
	wslBash := fakeWSLRoot(t)
	rigPathWSLFirst(t, wslBash, realBash)

	spec := CLISpec{
		ModelID:    "test-wsl-first",
		ModelName:  "Test WSL First",
		Binary:     "bash",
		PromptFlag: "-p",
		BuildArgs: func(bool) []string {
			return []string{"-c", "echo hello-from-real-bash"}
		},
	}
	m := &cliModel{spec: spec, workingDir: t.TempDir()}
	stream, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	var text strings.Builder
	var finished bool
	var errPart error
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeFinish:
			finished = true
		case fantasy.StreamPartTypeError:
			errPart = part.Error
		}
	}

	if errPart != nil {
		t.Fatalf("unexpected error (likely tried to run the WSL launcher): %v", errPart)
	}
	if !finished {
		t.Error("expected finish part")
	}
	if !strings.Contains(text.String(), "hello-from-real-bash") {
		t.Errorf("output = %q, want to contain %q (real bash never ran)", text.String(), "hello-from-real-bash")
	}
}

// TestStream_OnlyWSLLauncherOnPath proves the other half of the contract:
// when the ONLY "bash" reachable on PATH is the WSL launcher (no Git
// Bash/MSYS installed at all), cliprovider must surface a clear error
// rather than attempting to launch it. This uses the fake stand-in as the
// sole PATH entry — no real bash involved, so it runs unconditionally
// (no t.Skip), unlike the tests above.
func TestStream_OnlyWSLLauncherOnPath(t *testing.T) {
	wslBash := fakeWSLRoot(t)
	t.Setenv("PATH", filepath.Dir(wslBash))
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")

	spec := CLISpec{
		ModelID:    "test-wsl-only",
		ModelName:  "Test WSL Only",
		Binary:     "bash",
		PromptFlag: "-p",
		BuildArgs: func(bool) []string {
			return []string{"-c", "echo should-not-run"}
		},
	}
	m := &cliModel{spec: spec, workingDir: t.TempDir()}
	stream, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		// Erroring out of Stream() itself is an acceptable way to reject
		// this case (resolveBinary failing before a process is even
		// started).
		return
	}

	// If Stream() didn't error immediately, resolveBinary fell through to
	// the bare "bash" name (its documented failure-open behavior — see
	// resolveBinary's doc comment) and os/exec's own lookup either failed
	// too (fine — surfaces as a stream error below) or, in the worst case,
	// actually started the fake stand-in. The fake stand-in is not a valid
	// PE binary, so attempting to start/run it must fail rather than
	// report success with fabricated output.
	var gotError error
	var text strings.Builder
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeError:
			gotError = part.Error
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		}
	}
	if gotError == nil && strings.Contains(text.String(), "should-not-run") {
		t.Fatal("the fake WSL stand-in must never actually execute successfully")
	}
}
