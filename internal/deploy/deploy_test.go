package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// TestSweepRenameAsideLeftovers_NewTempFiles verifies the age gate on
// ".new-<token>" temp-copy files: a fresh one (indistinguishable from a
// concurrently-running deploy's in-progress copy) must survive the sweep,
// while one old enough to only plausibly be a leftover from a crashed
// deploy must be removed.
func TestSweepRenameAsideLeftovers_NewTempFiles(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "crush.exe")

	fresh := TempBuildName(dst, "111")
	stale := TempBuildName(dst, "222")
	for _, p := range []string{fresh, stale} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	oldTime := time.Now().Add(-2 * staleTempFileAge)
	if err := os.Chtimes(stale, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes %s: %v", stale, err)
	}

	removed := SweepRenameAsideLeftovers(dst)
	if len(removed) != 1 || removed[0] != stale {
		t.Fatalf("expected exactly [%s] removed, got %v", stale, removed)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh .new-* file must survive the sweep (could belong to a concurrent deploy), got: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale .new-* file should have been removed, stat err: %v", err)
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

func TestTempBuildName(t *testing.T) {
	got := TempBuildName(filepath.Join("C:", "bin", "crush.exe"), "1234-567")
	want := filepath.Join("C:", "bin", "crush.exe") + ".new-1234-567"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Distinct tokens must yield distinct temp paths so concurrent
	// deploys never share a temp file.
	if TempBuildName("/bin/crush", "1") == TempBuildName("/bin/crush", "2") {
		t.Errorf("distinct tokens produced the same temp path")
	}
}

func TestSwapRenameAside_Success(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "crush.exe")
	aside := RenameAsideName(dst, "tok")
	tmp := TempBuildName(dst, "tok")
	writeFileBytes(t, dst, []byte("old-binary"))
	writeFileBytes(t, tmp, []byte("new-binary"))

	if err := SwapRenameAside(os.Rename, tmp, dst, aside); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	assertContents(t, dst, []byte("new-binary"))
	assertContents(t, aside, []byte("old-binary"))
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("expected temp file to be renamed into dst (gone), stat err: %v", err)
	}
}

// TestSwapRenameAside_SecondRenameFailsRestored is the core fault-
// injection test for issue #127: it makes the SECOND rename (tmp → dst,
// the one that runs AFTER the old binary was already moved to aside)
// fail, and asserts the old binary is rolled back to dst so the operator
// is never left with a missing binary. The first rename (dst → aside) is
// exercised via real os.Rename; only the second is faulted, matching the
// real-world cause (an antivirus transiently locking the freshly written
// tmp on Windows). rename is injected rather than relying on real OS file
// locking because making os.Rename fail at exactly this step is not
// deterministically reproducible across CI runners.
func TestSwapRenameAside_SecondRenameFailsRestored(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "crush.exe")
	aside := RenameAsideName(dst, "tok")
	tmp := TempBuildName(dst, "tok")
	writeFileBytes(t, dst, []byte("old-binary"))
	writeFileBytes(t, tmp, []byte("new-binary"))

	faulted := false
	rename := func(old, new string) error {
		if old == tmp && new == dst {
			faulted = true
			return errors.New("fault: antivirus locked the freshly written tmp")
		}
		return os.Rename(old, new)
	}

	err := SwapRenameAside(rename, tmp, dst, aside)
	if err == nil {
		t.Fatal("expected an error because the second rename was faulted")
	}
	if !faulted {
		t.Fatal("the faulted second-rename path was never exercised")
	}
	// CRITICAL assertion: dst must hold the OLD binary again, not be
	// missing. This is the exact regression #127 fixes.
	assertContents(t, dst, []byte("old-binary"))
	// aside was moved back to dst, so it must be gone.
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Fatalf("expected aside to be restored back onto dst (gone), stat err: %v", err)
	}
	if !strings.Contains(err.Error(), "restored") {
		t.Errorf("error should report the binary was restored, got: %v", err)
	}
}

// TestSwapRenameAside_RestoreAlsoFails covers the catastrophic tail:
// both the second rename AND the rollback fail, leaving dst missing and
// the old binary stranded at aside. The function cannot recover, but it
// MUST return an error naming BOTH paths so the operator can manually
// move aside back to dst.
func TestSwapRenameAside_RestoreAlsoFails(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "crush.exe")
	aside := RenameAsideName(dst, "tok")
	tmp := TempBuildName(dst, "tok")
	writeFileBytes(t, dst, []byte("old-binary"))
	writeFileBytes(t, tmp, []byte("new-binary"))

	rename := func(old, new string) error {
		switch {
		case old == dst && new == aside:
			return os.Rename(old, new) // rename-aside succeeds
		case old == tmp && new == dst:
			return errors.New("fault: second rename blocked")
		case old == aside && new == dst:
			return errors.New("fault: restore blocked")
		}
		return os.Rename(old, new)
	}

	err := SwapRenameAside(rename, tmp, dst, aside)
	if err == nil {
		t.Fatal("expected an error when both rename and restore fail")
	}
	// dst is missing — nothing could be put back.
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("expected dst to be missing after restore failure, stat err: %v", statErr)
	}
	// The old binary is stranded at aside.
	assertContents(t, aside, []byte("old-binary"))
	// Error MUST name both paths for manual recovery.
	msg := err.Error()
	if !strings.Contains(msg, aside) {
		t.Errorf("error must name the aside path %q for manual recovery, got: %v", aside, err)
	}
	if !strings.Contains(msg, dst) {
		t.Errorf("error must name the dst path %q for manual recovery, got: %v", dst, err)
	}
}

func TestNpmNodeOS(t *testing.T) {
	cases := []struct {
		goos string
		want string
	}{
		{"windows", "win32"},
		{"linux", "linux"},
		{"darwin", "darwin"},
	}
	for _, c := range cases {
		if got := NpmNodeOS(c.goos); got != c.want {
			t.Errorf("NpmNodeOS(%q) = %q, want %q", c.goos, got, c.want)
		}
	}
}

func TestNpmNodeArch(t *testing.T) {
	cases := []struct {
		goarch string
		want   string
	}{
		{"amd64", "x64"},
		{"arm64", "arm64"},
		{"riscv64", "riscv64"}, // default: passthrough of unrecognized GOARCH
	}
	for _, c := range cases {
		if got := NpmNodeArch(c.goarch); got != c.want {
			t.Errorf("NpmNodeArch(%q) = %q, want %q", c.goarch, got, c.want)
		}
	}
}

func TestNpmPlatformBinaryPath(t *testing.T) {
	cases := []struct {
		npmDir, goos, goarch, binaryName string
	}{
		{filepath.Join("/some", "npm", "dir"), "windows", "amd64", "crush.exe"},
		{filepath.Join("/some", "npm", "dir"), "linux", "arm64", "crush"},
	}
	for _, c := range cases {
		got := NpmPlatformBinaryPath(c.npmDir, c.goos, c.goarch, c.binaryName)
		want := filepath.Join(c.npmDir, "node_modules", "@phpcraftdream",
			"crush-"+NpmNodeOS(c.goos)+"-"+NpmNodeArch(c.goarch), "bin", c.binaryName)
		if got != want {
			t.Errorf("NpmPlatformBinaryPath(%q, %q, %q, %q) = %q, want %q",
				c.npmDir, c.goos, c.goarch, c.binaryName, got, want)
		}
	}

	// Concrete, spelled-out example matching the docstring, independent
	// of NpmNodeOS/NpmNodeArch helpers, for windows/amd64.
	got := NpmPlatformBinaryPath(filepath.Join("/some", "npm", "dir"), "windows", "amd64", "crush.exe")
	want := filepath.Join("/some", "npm", "dir", "node_modules", "@phpcraftdream", "crush-win32-x64", "bin", "crush.exe")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Concrete example for linux/arm64.
	got = NpmPlatformBinaryPath(filepath.Join("/some", "npm", "dir"), "linux", "arm64", "crush")
	want = filepath.Join("/some", "npm", "dir", "node_modules", "@phpcraftdream", "crush-linux-arm64", "bin", "crush")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestDeployLockPath_Stable asserts DeployLockPath returns the same path
// across calls (it must — it's the whole point of using a single
// well-known path instead of a per-dst sidecar, so two independent `go
// run deploy.go` invocations agree on where to contend) and that it
// lives under os.TempDir(), not next to any particular destination.
func TestDeployLockPath_Stable(t *testing.T) {
	a := DeployLockPath()
	b := DeployLockPath()
	if a != b {
		t.Fatalf("DeployLockPath must be stable across calls, got %q then %q", a, b)
	}
	wantDir := filepath.Clean(os.TempDir())
	if gotDir := filepath.Dir(a); gotDir != wantDir {
		t.Errorf("DeployLockPath should live directly under os.TempDir() (%q), got %q", wantDir, gotDir)
	}
}

// TestAcquireDeployLock_SerializesConcurrentHolders is the core regression
// test for the mixed-version-install race (review finding P2.6): it starts
// N goroutines all racing to AcquireDeployLock on the SAME lockPath, each
// simulating a deploy's replace-all-destinations critical section by
// incrementing a shared counter, sleeping briefly, then decrementing it.
// If two "deploys" ever ran their critical section concurrently, the
// counter would observe a value > 1 at some point during the sleep — that
// is exactly the interleaving the deploy lock exists to rule out. The test
// asserts the counter never exceeds 1, i.e. every acquire/release pair
// runs strictly sequentially, matching the "second deploy waits for the
// first to fully finish, never interleaves" contract AcquireDeployLock
// documents.
func TestAcquireDeployLock_SerializesConcurrentHolders(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "deploy.lock")

	const holders = 6
	var (
		wg          sync.WaitGroup
		inSection   int32
		maxObserved int32
	)

	for i := 0; i < holders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			lock, err := AcquireDeployLock(ctx, lockPath)
			if err != nil {
				t.Errorf("AcquireDeployLock: %v", err)
				return
			}
			defer lock.Release()

			cur := atomic.AddInt32(&inSection, 1)
			for {
				prevMax := atomic.LoadInt32(&maxObserved)
				if cur <= prevMax || atomic.CompareAndSwapInt32(&maxObserved, prevMax, cur) {
					break
				}
			}
			// Simulate the replace-all-destinations critical section taking
			// measurable time, so a would-be second concurrent holder has a
			// real window to observe/collide with this one.
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&inSection, -1)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxObserved); got > 1 {
		t.Fatalf("deploy lock failed to serialize holders: observed %d concurrent holders in the critical section, want at most 1 (this is exactly the mixed-version-install race the lock exists to prevent)", got)
	}
}

// TestAcquireDeployLock_TimesOutWhenContended confirms AcquireDeployLock
// respects ctx: a second caller contending on an already-held lock must
// return a wrapped context error once ctx's deadline passes, rather than
// blocking forever — mirroring the behavioral contract already tested for
// the underlying primitive in internal/session/file_lock_test.go.
func TestAcquireDeployLock_TimesOutWhenContended(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "deploy.lock")

	holder, err := AcquireDeployLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("initial AcquireDeployLock: %v", err)
	}
	defer holder.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = AcquireDeployLock(ctx, lockPath)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected AcquireDeployLock to fail while the lock is held by another caller")
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("AcquireDeployLock returned too quickly (%v) — contention should be retried until ctx's deadline, not failed immediately", elapsed)
	}
}

// TestAcquireDeployLock_SequentialAcquireRelease confirms that after a
// holder releases, a new caller can acquire the same lockPath immediately
// — i.e. two deploys running one after another (the common case: a
// developer deploys, waits, deploys again) are never blocked by a stale
// lock.
func TestAcquireDeployLock_SequentialAcquireRelease(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "deploy.lock")

	first, err := AcquireDeployLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("first AcquireDeployLock: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	second, err := AcquireDeployLock(ctx, lockPath)
	if err != nil {
		t.Fatalf("second AcquireDeployLock after release should succeed promptly, got: %v", err)
	}
	defer second.Release()
}

func writeExe(t *testing.T, p string) string {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// writeFileBytes writes contents to p (or fails the test). Test helper.
func writeFileBytes(t *testing.T, p string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(p, contents, 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// assertContents fails the test unless p holds exactly want. Test helper.
func assertContents(t *testing.T, p string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s holds %q, want %q", p, got, want)
	}
}
