package app

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// runOnFinishHook executes a shell command after the agent run completes.
// Errors from the hook are printed to stderr but don't affect the exit code.
func runOnFinishHook(hook, sessionID, exitReason string, cost float64, tokens int64, duration time.Duration) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", hook)
	default:
		cmd = exec.Command("bash", "-c", hook)
	}

	cmd.Env = append(os.Environ(),
		"CRUSH_SESSION_ID="+sessionID,
		"CRUSH_EXIT_REASON="+exitReason,
		fmt.Sprintf("CRUSH_COST_USD=%.6f", cost),
		fmt.Sprintf("CRUSH_TOKENS=%d", tokens),
		fmt.Sprintf("CRUSH_DURATION_SEC=%.0f", duration.Seconds()),
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "on-finish hook error: %v\n%s\n", err, output)
		return
	}
	if len(output) > 0 {
		slog.Debug("on-finish hook output", "output", string(output))
	}
}
