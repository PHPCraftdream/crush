package deploy

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultInstallPath(t *testing.T) {
	p, err := DefaultInstallPath()
	if err != nil {
		t.Fatalf("DefaultInstallPath: %v", err)
	}
	if p == "" {
		t.Fatal("DefaultInstallPath returned empty string")
	}
	base := filepath.Base(p)
	switch runtime.GOOS {
	case "windows":
		if base != "crush.exe" {
			t.Errorf("windows install path should end in crush.exe, got %q", p)
		}
		if filepath.Base(filepath.Dir(p)) != "crush" {
			t.Errorf("windows install path should live under a crush/ dir, got %q", p)
		}
	default:
		if base != "crush" {
			t.Errorf("unix install path should end in crush, got %q", p)
		}
		if filepath.Base(filepath.Dir(p)) != "bin" {
			t.Errorf("unix install path should live under .local/bin, got %q", p)
		}
	}
	if !filepath.IsAbs(p) {
		t.Errorf("DefaultInstallPath must return an absolute path, got %q", p)
	}
}

func TestDefaultInstallPath_WindowsUsesLocalAppData(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("LOCALAPPDATA layout only applies on windows")
	}
	t.Setenv("LOCALAPPDATA", `C:\Users\tester\AppData\Local`)
	p, err := DefaultInstallPath()
	if err != nil {
		t.Fatalf("DefaultInstallPath: %v", err)
	}
	want := filepath.Join(`C:\Users\tester\AppData\Local`, "Programs", "crush", "crush.exe")
	if p != want {
		t.Errorf("got %q, want %q", p, want)
	}
}

func TestDefaultInstallPath_UnixUsesHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME layout only applies on unix")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, err := DefaultInstallPath()
	if err != nil {
		t.Fatalf("DefaultInstallPath: %v", err)
	}
	want := filepath.Join(home, ".local", "bin", "crush")
	if p != want {
		t.Errorf("got %q, want %q", p, want)
	}
}

func TestIsReplaceableExe(t *testing.T) {
	dir := t.TempDir()

	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), mode); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	if runtime.GOOS == "windows" {
		exe := write("crush.exe", 0o644)
		if !IsReplaceableExe(exe) {
			t.Errorf(".exe should be replaceable on windows")
		}
		cmd := write("crush.cmd", 0o644)
		if IsReplaceableExe(cmd) {
			t.Errorf(".cmd shim should NOT be replaceable on windows")
		}
		return
	}

	bin := write("crush-bin", 0o755)
	if !IsReplaceableExe(bin) {
		t.Errorf("executable-mode file with no script extension should be replaceable")
	}
	script := write("crush.sh", 0o755)
	if IsReplaceableExe(script) {
		t.Errorf(".sh script should NOT be replaceable even if executable")
	}
	nonExec := write("crush-nonexec", 0o644)
	if IsReplaceableExe(nonExec) {
		t.Errorf("non-executable file should NOT be replaceable")
	}
	missing := filepath.Join(dir, "does-not-exist")
	if IsReplaceableExe(missing) {
		t.Errorf("missing file should NOT be replaceable")
	}
}

func TestSameFile(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	if SameFile(a, b) {
		t.Errorf("distinct files should not be SameFile")
	}
	if !SameFile(a, a) {
		t.Errorf("a file should be SameFile with itself")
	}
	if SameFile(a, filepath.Join(dir, "missing")) {
		t.Errorf("comparison against a missing path should be false, not error out")
	}
}

func TestPathListContains(t *testing.T) {
	sep := string(os.PathListSeparator)
	pathEnv := "/usr/bin" + sep + "/usr/local/bin" + sep + "/home/me/.local/bin"

	if !PathListContains(pathEnv, "/usr/local/bin") {
		t.Errorf("expected /usr/local/bin to be found")
	}
	if PathListContains(pathEnv, "/opt/nope") {
		t.Errorf("did not expect /opt/nope to be found")
	}

	if runtime.GOOS == "windows" {
		mixedCase := `C:\Users\Me\bin` + sep + `C:\Windows`
		if !PathListContains(mixedCase, `c:\users\me\bin`) {
			t.Errorf("PATH lookup should be case-insensitive on windows")
		}
	}
}

func TestAppendToPathList(t *testing.T) {
	sep := string(os.PathListSeparator)

	got := AppendToPathList("/usr/bin", "/home/me/.local/bin")
	want := "/usr/bin" + sep + "/home/me/.local/bin"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Already present: unchanged.
	existing := "/usr/bin" + sep + "/home/me/.local/bin"
	if got := AppendToPathList(existing, "/home/me/.local/bin"); got != existing {
		t.Errorf("appending an already-present dir should be a no-op, got %q", got)
	}

	// Empty PATH: dir becomes the whole value, no leading separator.
	if got := AppendToPathList("", "/home/me/.local/bin"); got != "/home/me/.local/bin" {
		t.Errorf("got %q, want bare dir for empty PATH", got)
	}
}

func TestLookPathExcludingCwd(t *testing.T) {
	cwd := t.TempDir()
	elsewhere := t.TempDir()

	exts := []string{""}
	if runtime.GOOS == "windows" {
		exts = []string{".exe"}
	}
	binName := "crush" + exts[0]

	// Only a copy in cwd: must be excluded, so lookup fails.
	writeExe(t, filepath.Join(cwd, binName))
	pathEnv := cwd
	if _, err := LookPathExcludingCwd("crush", cwd, pathEnv, exts); err == nil {
		t.Fatalf("expected error when the only candidate is in cwd")
	}

	// A copy elsewhere on PATH: found, cwd copy ignored.
	wantPath := writeExe(t, filepath.Join(elsewhere, binName))
	pathEnv = cwd + string(os.PathListSeparator) + elsewhere
	got, err := LookPathExcludingCwd("crush", cwd, pathEnv, exts)
	if err != nil {
		t.Fatalf("LookPathExcludingCwd: %v", err)
	}
	if got != wantPath {
		t.Errorf("got %q, want %q", got, wantPath)
	}
}

func TestWindowsPathExts(t *testing.T) {
	got := WindowsPathExts("")
	want := []string{".exe", ".cmd", ".bat", ".com"}
	if len(got) != len(want) {
		t.Fatalf("default PATHEXT: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("default PATHEXT: got %v, want %v", got, want)
		}
	}

	sep := string(os.PathListSeparator)
	custom := WindowsPathExts(".EXE" + sep + " .BAT ")
	if len(custom) != 2 || custom[0] != ".exe" || custom[1] != ".bat" {
		t.Errorf("custom PATHEXT not lowercased/trimmed correctly: %v", custom)
	}
}

func TestRenameAsideName(t *testing.T) {
	got := RenameAsideName(filepath.Join("C:", "bin", "crush.exe"), "12345")
	want := filepath.Join("C:", "bin", "crush.exe") + ".old-12345"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Distinct tokens must produce distinct names — the whole point is
	// that concurrent/successive deploys don't collide with each other's
	// still-live rename-aside targets.
	a := RenameAsideName("/bin/crush", "1")
	b := RenameAsideName("/bin/crush", "2")
	if a == b {
		t.Errorf("expected different tokens to produce different names, both got %q", a)
	}
}

func TestSweepRenameAsideLeftovers(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "crush.exe")

	// Nothing on disk at all: must not panic or error, and must report
	// nothing removed.
	if got := SweepRenameAsideLeftovers(dst); len(got) != 0 {
		t.Fatalf("expected no removals against an empty dir, got %v", got)
	}

	// Create one .old-* leftover, one legacy .bak-* leftover, and one
	// unrelated file that must NOT match either glob.
	oldLeftover := RenameAsideName(dst, "111")
	bakLeftover := dst + ".bak-b53789cc"
	unrelated := filepath.Join(dir, "some-other-file.txt")
	for _, p := range []string{oldLeftover, bakLeftover, unrelated} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	removed := SweepRenameAsideLeftovers(dst)
	if len(removed) != 2 {
		t.Fatalf("expected 2 removals, got %v", removed)
	}
	if _, err := os.Stat(oldLeftover); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed", oldLeftover)
	}
	if _, err := os.Stat(bakLeftover); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed", bakLeftover)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated file should not have been touched: %v", err)
	}

	// Calling again on an already-swept dir must be a safe no-op.
	if got := SweepRenameAsideLeftovers(dst); len(got) != 0 {
		t.Fatalf("expected no removals on second sweep, got %v", got)
	}
}

// TestSweepRenameAsideLeftovers_BusyFileIsIgnored simulates the "still
// held by a live process" case: on Windows, a file that is open for
// reading/execution cannot be removed, and SweepRenameAsideLeftovers must
// swallow that failure rather than erroring out. We approximate "busy" by
// holding our own open handle to the leftover file, which is enough to
// make os.Remove fail on Windows (POSIX systems allow unlinking open
// files, so this case is Windows-specific; on other OSes it's a no-op
// assertion that sweeping still doesn't error).
func TestSweepRenameAsideLeftovers_BusyFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "crush.exe")
	busy := RenameAsideName(dst, "999")
	if err := os.WriteFile(busy, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", busy, err)
	}

	f, err := os.Open(busy)
	if err != nil {
		t.Fatalf("open %s: %v", busy, err)
	}
	defer f.Close()

	// Must not panic; on Windows the removal will fail and be silently
	// skipped, on Unix it may succeed (unlink-while-open is legal there).
	_ = SweepRenameAsideLeftovers(dst)

	if runtime.GOOS == "windows" {
		if _, statErr := os.Stat(busy); statErr != nil {
			t.Errorf("expected busy file to survive the sweep on windows, but it's gone: %v", statErr)
		}
	}
}

func writeExe(t *testing.T, p string) string {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}
