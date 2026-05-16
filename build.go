//go:build ignore

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

func run(dir, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "FAILED: %s %v\n", name, args)
		os.Exit(1)
	}
}

func main() {
	root, _ := os.Getwd()

	fmt.Println("→ Installing web dependencies...")
	run(root+"/web", "npm", "install")

	fmt.Println("→ Building web UI...")
	run(root+"/web", "npm", "run", "build")

	out := "crush"
	if runtime.GOOS == "windows" {
		out = "crush.exe"
	}

	fmt.Println("→ Building crush binary...")
	// Fork merge note (origin/main 2026-05-16): upstream renamed BuildTime to
	// BuildID (commit 9e126c27). We keep the timestamp value — it satisfies
	// BuildID's "unique per build" contract — but write it into the new field.
	buildTime := time.Now().Format("2006-01-02_15-04-05")
	ldflags := fmt.Sprintf("-X=github.com/charmbracelet/crush/internal/version.BuildID=%s", buildTime)
	run(root, "go", "build", "-ldflags", ldflags, "-o", out, ".")

	fmt.Printf("✓ Done → %s\n", out)
}
