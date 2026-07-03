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

func writeExe(t *testing.T, p string) string {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}
