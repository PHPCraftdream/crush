//go:build windows

package shell

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveInterpreter_PermissiveFallback_Windows is the Windows-native
// counterpart to the POSIX permissive-fallback test. It proves the one
// behavior that makes `#!/bin/bash` hooks work on a stock Windows box
// with Git for Windows installed: when the literal interpreter path does
// not exist, we fall back to a PATH-lookup on the basename and that
// lookup accepts any executable extension Windows honors (here, `.bat`).
//
// We plant a bash.bat in a tempdir rather than a .exe because producing
// a .exe would require a toolchain step; LookPath on Windows resolves
// PATHEXT extensions, so .bat is just as valid for the lookup codepath.
func TestResolveInterpreter_PermissiveFallback_Windows(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "bash.bat")
	contents := "@echo off\r\nexit /b 0\r\n"
	if err := os.WriteFile(fake, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake bash.bat: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("PATHEXT", ".BAT;.CMD;.EXE")

	// Literal path must be absent so the stat fails with ENOENT.
	missing := filepath.Join(dir, "definitely-not-here-"+randSuffix(), "bash")
	resolved, err := resolveInterpreter(missing)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got: %v", err)
	}
	if resolved != fake {
		t.Fatalf("resolved = %q, want %q", resolved, fake)
	}
}

// TestIsWSLLauncher_KnownPaths checks the pure path-based WSL detection
// against the canonical launcher locations and confirms Git Bash is NOT
// mistaken for WSL. No PATH access — fully deterministic on any host.
func TestIsWSLLauncher_KnownPaths(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`C:\Windows\System32\bash.exe`, true},
		{`C:\Windows\System32\wsl.exe`, true},
		{`C:\Windows\SysWOW64\bash.exe`, true},
		{`c:\windows\system32\BASH.EXE`, true},  // case-insensitive
		{`C:/Windows/System32/bash.exe`, true},  // forward slashes
		{`D:\Program Files\Git\usr\bin\bash.exe`, false}, // Git Bash
		{`C:\Users\me\bin\bash.exe`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := isWSLLauncher(c.in); got != c.want {
			t.Errorf("isWSLLauncher(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// plantBash creates <dir>/bash.bat and returns its path. We use .bat (with
// PATHEXT) rather than a real .exe because generating a PE binary needs a
// toolchain; both exec.LookPath and our stat-based walk only require a
// regular non-directory file. Whether a planted candidate counts as "WSL"
// in these tests is decided by the skip predicate we inject into
// lookPathSkipping, so the planted path never has to be a real System32
// path — this keeps the tests deterministic regardless of the host's PATH.
func plantBash(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "bash.bat")
	if err := os.WriteFile(p, []byte("@echo off\r\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// inDir reports whether p lives in dir, compared case-insensitively and
// slash-agnostically so path-form differences from exec.LookPath cannot
// break the predicate.
func inDir(p, dir string) bool {
	return normPath(filepath.Dir(p)) == normPath(dir)
}

// TestLookPathSkipping_PrefersNonWSLWhenWSLFirst is the core bug scenario:
// PATH leads with the WSL launcher dir and Git Bash comes later. We must
// skip WSL and return Git Bash rather than handing back a launcher that
// would choke on the Windows script path.
func TestLookPathSkipping_PrefersNonWSLWhenWSLFirst(t *testing.T) {
	wslDir := t.TempDir()
	gitDir := t.TempDir()
	wsl := plantBash(t, wslDir)
	git := plantBash(t, gitDir)
	t.Setenv("PATH", wslDir+string(os.PathListSeparator)+gitDir)
	t.Setenv("PATHEXT", ".BAT")

	skip := func(p string) bool { return inDir(p, wslDir) }
	got, err := lookPathSkipping("bash", skip)
	if err != nil {
		t.Fatalf("lookPathSkipping: %v", err)
	}
	if !strings.EqualFold(got, git) {
		t.Fatalf("got %q, want Git Bash %q (WSL %q should have been skipped)", got, git, wsl)
	}
}

// TestLookPathSkipping_GitBashFirstUnchanged confirms the "no regression"
// case: when the first match is not WSL, behavior is identical to plain
// exec.LookPath (fast path, no reordering).
func TestLookPathSkipping_GitBashFirstUnchanged(t *testing.T) {
	gitDir := t.TempDir()
	wslDir := t.TempDir()
	git := plantBash(t, gitDir)
	_ = plantBash(t, wslDir)
	t.Setenv("PATH", gitDir+string(os.PathListSeparator)+wslDir)
	t.Setenv("PATHEXT", ".BAT")

	skip := func(p string) bool { return inDir(p, wslDir) }
	got, err := lookPathSkipping("bash", skip)
	if err != nil {
		t.Fatalf("lookPathSkipping: %v", err)
	}
	if !strings.EqualFold(got, git) {
		t.Fatalf("got %q, want Git Bash %q", got, git)
	}
}

// TestLookPathSkipping_WSLOnlyErrors: when the only candidate on PATH is
// the WSL launcher, we must NOT return it (it would fail on the Windows
// script path); instead surface a clear, actionable error wrapping
// errWSLLauncherOnly.
func TestLookPathSkipping_WSLOnlyErrors(t *testing.T) {
	wslDir := t.TempDir()
	wsl := plantBash(t, wslDir)
	t.Setenv("PATH", wslDir)
	t.Setenv("PATHEXT", ".BAT")

	skip := func(p string) bool { return inDir(p, wslDir) }
	_, err := lookPathSkipping("bash", skip)
	if err == nil {
		t.Fatal("expected WSL-only error, got nil")
	}
	if !errors.Is(err, errWSLLauncherOnly) {
		t.Fatalf("expected errWSLLauncherOnly, got: %v", err)
	}
	if !strings.Contains(err.Error(), "WSL") {
		t.Fatalf("expected the error to mention WSL, got: %v", err)
	}
	// Sanity: the planted path appears in the message.
	if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(wsl)) {
		t.Fatalf("expected error to reference %q, got: %v", wsl, err)
	}
}

// TestLookPathSkipping_NilSkipDelegatesToLookPath: with a nil predicate the
// helper is a plain exec.LookPath (the off-Windows / no-skip guard).
func TestLookPathSkipping_NilSkipDelegatesToLookPath(t *testing.T) {
	dir := t.TempDir()
	b := plantBash(t, dir)
	t.Setenv("PATH", dir)
	t.Setenv("PATHEXT", ".BAT")

	got, err := lookPathSkipping("bash", nil)
	if err != nil {
		t.Fatalf("lookPathSkipping: %v", err)
	}
	if !strings.EqualFold(got, b) {
		t.Fatalf("got %q, want %q", got, b)
	}
}
