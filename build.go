//go:build ignore

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func run(dir, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// CI=true puts pnpm into non-interactive frozen-lockfile mode and lets
	// it safely remove a stale node_modules directory from a prior npm run
	// without prompting. Harmless for other commands.
	cmd.Env = append(os.Environ(), "CI=true")
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "FAILED: %s %v\n", name, args)
		os.Exit(1)
	}
}

func main() {
	root, _ := os.Getwd()

	fmt.Println("→ Installing web dependencies...")
	run(root+"/web", "pnpm", "install")

	fmt.Println("→ Building web UI...")
	run(root+"/web", "pnpm", "run", "build")

	out := "crush"
	if runtime.GOOS == "windows" {
		out = "crush.exe"
	}

	fmt.Println("→ Building crush binary (production flags)...")
	// Fork merge note (origin/main 2026-05-16): upstream renamed BuildTime to
	// BuildID (commit 9e126c27). We keep the timestamp value — it satisfies
	// BuildID's "unique per build" contract — but write it into the new field.
	buildTime := time.Now().Format("2006-01-02_15-04-05")
	// -s -w and -trimpath match the flags the real release build uses
	// (.github/workflows/publish-fork-npm.yml, .goreleaser.yml) — strips
	// the symbol table/DWARF debug info and local filesystem paths, so a
	// `go run deploy.go` binary is structurally the same as what actually
	// ships. version.Version is deliberately left uninjected: the binary
	// should still report itself as a local dev build (commit hash +
	// fork base version, see internal/version/version.go), not spoof a
	// numbered release.
	//
	// Also stamp the commit hash and a readable build time so
	// `crush --version` can prove which source tree a binary came from.
	// Without them the version line is byte-identical for every build of a
	// release line, so "is the deployed binary actually current?" cannot be
	// answered from the binary itself — a question that got answered wrong
	// more than once. Note -trimpath drops the VCS metadata the toolchain
	// would otherwise embed, so the hash is injected explicitly instead of
	// relying on debug.ReadBuildInfo. Best-effort: a build outside a git
	// checkout still succeeds, just without the commit component.
	const versionPkg = "github.com/charmbracelet/crush/internal/version"
	ldflags := fmt.Sprintf(
		"-s -w -X=%s.BuildID=%s -X=%s.BuildTime=%s",
		versionPkg, buildTime,
		versionPkg, time.Now().Format("2006-01-02_15:04:05"),
	)
	if commit := gitShortCommit(root); commit != "" {
		ldflags += fmt.Sprintf(" -X=%s.Commit=%s", versionPkg, commit)
	}
	run(root, "go", "build", "-trimpath", "-ldflags", ldflags, "-o", out, ".")

	fmt.Printf("✓ Done → %s\n", out)
}

// gitShortCommit returns the short HEAD hash for the checkout at dir, with a
// "-dirty" suffix when the working tree has uncommitted changes — a dev build
// from a modified tree is NOT the same artifact as its commit, and silently
// labelling it as that commit is precisely the kind of false provenance this
// stamping exists to prevent.
//
// Returns "" on any failure (no git, not a checkout, git error) so the build
// still succeeds without the commit component rather than failing outright.
func gitShortCommit(dir string) string {
	out, err := gitOutput(dir, "rev-parse", "--short=8", "HEAD")
	if err != nil || out == "" {
		return ""
	}
	if status, statusErr := gitOutput(dir, "status", "--porcelain"); statusErr == nil && status != "" {
		out += "-dirty"
	}
	return out
}

// gitOutput runs a git command in dir and returns its trimmed stdout. Unlike
// run() it does not abort the build on failure — provenance is best-effort.
func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
