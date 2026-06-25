package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/shell"
)

const (
	JobOutputToolName = "job_output"
)

//go:embed job_output.md
var jobOutputDescription string

// jobOutputMaxWait bounds a `wait:true` job_output call so it never blocks
// the agent turn indefinitely. Past it the tool returns the current status
// ("running") and the model can poll again. Well under the watchdog's
// tool-execution backstop so the wait returns gracefully, not via a kill.
var jobOutputMaxWait = 90 * time.Second

type JobOutputParams struct {
	ShellID string `json:"shell_id" description:"The ID of the background shell to retrieve output from"`
	Wait    bool   `json:"wait" description:"If true, wait up to ~90s for the background shell to complete before returning; if it's still running, returns the current output with Status: running so you can poll again (the wait never blocks the turn indefinitely)."`
}

type JobOutputResponseMetadata struct {
	ShellID          string        `json:"shell_id"`
	Command          string        `json:"command"`
	Description      string        `json:"description"`
	Done             bool          `json:"done"`
	WorkingDirectory string        `json:"working_directory"`
	Elapsed          time.Duration `json:"elapsed"`
}

func NewJobOutputTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		JobOutputToolName,
		jobOutputDescription,
		func(ctx context.Context, params JobOutputParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ShellID == "" {
				return fantasy.NewTextErrorResponse("missing shell_id"), nil
			}

			bgManager := shell.GetBackgroundShellManager()
			bgShell, ok := bgManager.Get(params.ShellID)
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("background shell not found: %s", params.ShellID)), nil
			}

			if params.Wait {
				baseSo, baseSe, _, _ := bgShell.GetOutput()
				baseLen := len(baseSo) + len(baseSe)
				waitCtx, cancelWait := context.WithTimeout(ctx, jobOutputMaxWait)
				bgShell.WaitForChange(waitCtx, baseLen)
				cancelWait()
			}

			stdout, stderr, done, err := bgShell.GetOutput()

			var outputParts []string
			if stdout != "" {
				outputParts = append(outputParts, stdout)
			}
			if stderr != "" {
				outputParts = append(outputParts, stderr)
			}

			elapsed := bgShell.Elapsed().Round(time.Second)
			status := "running"
			if done {
				status = "completed"
				exitCode := shell.ExitCode(err)
				if exitCode != 0 {
					outputParts = append(outputParts, fmt.Sprintf("Exit code %d", exitCode))
				}
			}

			output := strings.Join(outputParts, "\n")
			output = TruncateOutput(output)
			if params.Wait && !done {
				output = strings.TrimSpace(output)
				if output == "" {
					output = BashNoOutput
				}
				output = output + "\n\n(still running after the wait window — call job_output again to keep waiting)"
			}

			metadata := JobOutputResponseMetadata{
				ShellID:          params.ShellID,
				Command:          bgShell.Command,
				Description:      bgShell.Description,
				Done:             done,
				WorkingDirectory: bgShell.WorkingDir,
				Elapsed:          elapsed,
			}

			if output == "" {
				output = BashNoOutput
			}

			var header string
			if done {
				exitCode := shell.ExitCode(err)
				header = fmt.Sprintf("Status: %s (elapsed %s, exit %d)", status, elapsed, exitCode)
			} else {
				header = fmt.Sprintf("Status: %s (elapsed %s)", status, elapsed)
			}
			result := fmt.Sprintf("%s\n\n%s", header, output)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	)
}
