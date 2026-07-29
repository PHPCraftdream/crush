// Command sleeper is a tiny helper binary used only by
// internal/deploy's live-process integration test
// (TestReplaceFile_LiveWindowsProcess in deploy_live_test.go). It has no
// purpose outside that test.
//
// On start it prints "ready" followed by a newline and flushes, then
// polls for the presence of a "stop" file passed as argv[1]. Once that
// file appears it exits 0. This lets the test park a genuinely running,
// genuinely loaded executable image on disk, apply deploy.go's
// rename-aside sequence to that exact exe file while the process is
// alive, and then signal a clean exit once it's done asserting the
// running process survived the swap.
package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sleeper <stop-file-path>")
		os.Exit(2)
	}
	stopFile := os.Args[1]

	fmt.Println("ready")
	_ = os.Stdout.Sync()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stopFile); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
