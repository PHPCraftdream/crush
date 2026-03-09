//go:build ignore

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
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
	run(root, "go", "build", "-o", out, ".")

	fmt.Printf("✓ Done → %s\n", out)
}
