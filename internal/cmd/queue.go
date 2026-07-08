package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/crush/internal/platform"
	"github.com/charmbracelet/crush/internal/queue"
	"github.com/spf13/cobra"
)

var queueCmd = &cobra.Command{
	Use:   "queue",
	Short: "Manage a persistent task queue for batched crush run invocations",
	Long: `The queue system allows batching multiple crush run prompts and
executing them sequentially or concurrently.

Workflow:
  1. crush queue add < prompt     — enqueue a task
  2. crush queue list              — inspect the queue
  3. crush queue run               — process pending tasks
  4. crush queue list              — see results`,
}

var queueAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a task to the queue (reads prompt from stdin)",
	Long:  `Read a prompt from stdin and add it as a pending task to the queue.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		sessionID, _ := cmd.Flags().GetString("session")
		role, _ := cmd.Flags().GetString("role")
		maxCost, _ := cmd.Flags().GetFloat64("max-cost")
		maxTokens, _ := cmd.Flags().GetInt64("max-tokens")
		timeout, _ := cmd.Flags().GetDuration("timeout")
		promptFile, _ := cmd.Flags().GetString("prompt-file")

		var prompt string
		if promptFile != "" {
			bts, err := os.ReadFile(promptFile)
			if err != nil {
				return fmt.Errorf("failed to read prompt file: %w", err)
			}
			prompt = string(bts)
		} else {
			bts, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("failed to read stdin: %w", err)
			}
			prompt = string(bts)
		}
		prompt = strings.TrimSpace(prompt)
		if prompt == "" {
			return fmt.Errorf("no prompt provided (pipe to stdin or use --prompt-file)")
		}

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		q := queue.NewService(a.DB())
		var timeoutSec int64
		if timeout > 0 {
			timeoutSec = int64(timeout.Seconds())
		}
		id, err := q.Add(cmd.Context(), sessionID, prompt, role, maxCost, maxTokens, timeoutSec)
		if err != nil {
			return err
		}
		fmt.Println(id)
		return nil
	},
}

var queueListCmd = &cobra.Command{
	Use:   "list",
	Short: "List queue tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		status, _ := cmd.Flags().GetString("status")
		asJSON, _ := cmd.Flags().GetBool("json")

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		q := queue.NewService(a.DB())
		tasks, err := q.List(cmd.Context(), queue.TaskStatus(status))
		if err != nil {
			return err
		}

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			for _, t := range tasks {
				_ = enc.Encode(t)
			}
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATUS\tSESSION\tCOST\tTOKENS\tCREATED")
		for _, t := range tasks {
			fmt.Fprintf(tw, "%s\t%s\t%s\t$%.4f\t%d\t%s\n",
				t.ID,
				t.Status,
				t.SessionID,
				t.Cost,
				t.Tokens,
				time.Unix(t.CreatedAt, 0).Format("2006-01-02 15:04"),
			)
		}
		return tw.Flush()
	},
}

var queueShowCmd = &cobra.Command{
	Use:   "show <task-id>",
	Short: "Show details of a single queue task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		q := queue.NewService(a.DB())
		t, err := q.Get(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("task not found: %w", err)
		}

		fmt.Printf("ID:          %s\n", t.ID)
		fmt.Printf("Status:      %s\n", t.Status)
		fmt.Printf("Session:     %s\n", t.SessionID)
		fmt.Printf("Role:        %s\n", t.Role)
		fmt.Printf("Prompt:      %s\n", truncate(t.Prompt, 200))
		fmt.Printf("Max Cost:    %.4f\n", t.MaxCost)
		fmt.Printf("Max Tokens:  %d\n", t.MaxTokens)
		fmt.Printf("Timeout:     %ds\n", t.TimeoutSec)
		fmt.Printf("Cost:        $%.4f\n", t.Cost)
		fmt.Printf("Tokens:      %d\n", t.Tokens)
		fmt.Printf("Exit Reason: %s\n", t.ExitReason)
		fmt.Printf("Created:     %s\n", time.Unix(t.CreatedAt, 0).Format(time.RFC3339))
		if t.StartedAt.Valid {
			fmt.Printf("Started:     %s\n", time.Unix(t.StartedAt.Int64, 0).Format(time.RFC3339))
		}
		if t.FinishedAt.Valid {
			fmt.Printf("Finished:    %s\n", time.Unix(t.FinishedAt.Int64, 0).Format(time.RFC3339))
		}
		return nil
	},
}

var queueRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Process pending queue tasks by spawning crush run subprocesses",
	Long: `Acquire an exclusive lock and process pending tasks from the queue.

Each task is executed by spawning a ` + "`crush run`" + ` subprocess with the
task's prompt, role, cost limit, and timeout. Results (cost, tokens, exit
reason) are written back to the queue.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		concurrent, _ := cmd.Flags().GetInt("concurrent")
		stopOnFail, _ := cmd.Flags().GetBool("stop-on-fail")
		maxTasks, _ := cmd.Flags().GetInt("max-tasks")

		if concurrent < 1 {
			return fmt.Errorf("--concurrent must be >= 1")
		}

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}
		dataDir, _ := cmd.Flags().GetString("data-dir")
		if dataDir == "" {
			dataDir = filepath.Join(cwd, ".crush")
		}

		// Acquire the queue lock.
		lockPath := filepath.Join(dataDir, "queue.lock")
		if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
			return err
		}
		release, err := acquireSpawnLock(lockPath)
		if err != nil {
			return fmt.Errorf("queue lock: %w (another runner may be active)", err)
		}
		defer release()

		q := queue.NewService(a.DB())
		ctx := cmd.Context()
		processed := 0

		for maxTasks <= 0 || processed < maxTasks {

			limit := concurrent
			if maxTasks > 0 {
				remaining := maxTasks - processed
				if remaining < limit {
					limit = remaining
				}
			}

			tasks, err := q.ClaimPending(ctx, limit)
			if err != nil {
				return fmt.Errorf("failed to claim tasks: %w", err)
			}
			if len(tasks) == 0 {
				break
			}

			type taskResult struct {
				task       queue.Task
				cost       float64
				tokens     int64
				exitReason string
				err        error
			}
			ch := make(chan taskResult, len(tasks))

			batchErr := func() error {
				runCtx, runCancel := context.WithCancel(ctx)
				defer runCancel()

				for _, t := range tasks {
					go func(task queue.Task) {
						cost, tokens, exitReason, runErr := runQueueTask(runCtx, cwd, task)
						ch <- taskResult{task: task, cost: cost, tokens: tokens, exitReason: exitReason, err: runErr}
					}(t)
				}

				var firstErr error
				for range tasks {
					r := <-ch
					processed++
					if r.err != nil {
						slog.Error("queue task failed", "id", r.task.ID, "err", r.err)
						_ = q.UpdateStatus(ctx, r.task.ID, queue.StatusFailed, r.cost, r.tokens, r.exitReason)
						if stopOnFail && firstErr == nil {
							firstErr = fmt.Errorf("task %s failed: %w", r.task.ID, r.err)
							runCancel()
						}
					} else {
						_ = q.UpdateStatus(ctx, r.task.ID, queue.StatusDone, r.cost, r.tokens, r.exitReason)
					}
				}
				return firstErr
			}()
			if batchErr != nil {
				return batchErr
			}
		}

		fmt.Fprintf(os.Stderr, "processed %d task(s)\n", processed)
		return nil
	},
}

var queueRmCmd = &cobra.Command{
	Use:   "rm <task-id>",
	Short: "Remove a task from the queue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		q := queue.NewService(a.DB())
		if err := q.Remove(cmd.Context(), args[0]); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "removed task %s\n", args[0])
		return nil
	},
}

var queueClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Clear tasks from the queue",
	RunE: func(cmd *cobra.Command, args []string) error {
		status, _ := cmd.Flags().GetString("status")

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		q := queue.NewService(a.DB())
		var statuses []queue.TaskStatus
		if status != "" {
			statuses = []queue.TaskStatus{queue.TaskStatus(status)}
		}
		if err := q.Clear(cmd.Context(), statuses...); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "cleared queue\n")
		return nil
	},
}

// runQueueTask executes a single queue task by spawning crush run.
// Returns cost, tokens, exit reason, and any error.
func runQueueTask(ctx context.Context, cwd string, task queue.Task) (float64, int64, string, error) {
	sessionID := task.SessionID
	if sessionID == "" {
		sessionID = task.ID
	}

	crushBin, err := os.Executable()
	if err != nil {
		return 0, 0, "", err
	}

	role := "smart"
	if task.Role != "" {
		role = task.Role
	}
	cmdArgs := []string{
		"run",
		"--session", sessionID,
		"--json",
		"--role", role,
	}
	if task.MaxCost > 0 {
		cmdArgs = append(cmdArgs, "--max-cost", fmt.Sprintf("%.4f", task.MaxCost))
	}
	if task.MaxTokens > 0 {
		cmdArgs = append(cmdArgs, "--max-tokens", fmt.Sprintf("%d", task.MaxTokens))
	}
	if task.TimeoutSec > 0 {
		cmdArgs = append(cmdArgs, "--timeout", fmt.Sprintf("%ds", task.TimeoutSec))
	}

	execCmd := exec.CommandContext(ctx, crushBin, cmdArgs...)
	platform.HideConsoleWindow(execCmd)
	execCmd.Dir = cwd
	execCmd.Stdin = strings.NewReader(task.Prompt)

	output, err := execCmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return 0, 0, fmt.Sprintf("exit %d", exitErr.ExitCode()), fmt.Errorf("%s", string(exitErr.Stderr))
		}
		return 0, 0, "", err
	}

	// Parse JSON output to extract cost/tokens.
	var result struct {
		CostUSD    float64 `json:"cost_usd"`
		Tokens     int64   `json:"tokens"`
		ExitReason string  `json:"exit_reason"`
	}
	if jsonErr := json.Unmarshal(output, &result); jsonErr != nil {
		return 0, 0, "", nil
	}
	return result.CostUSD, result.Tokens, result.ExitReason, nil
}

func init() {
	queueAddCmd.Flags().String("session", "", "Session ID for the run (default: auto-generated from task ID)")
	queueAddCmd.Flags().String("role", "", "Model role: smart|fast (default: smart)")
	queueAddCmd.Flags().Float64("max-cost", 0, "Abort if cost exceeds this value (USD)")
	queueAddCmd.Flags().Int64("max-tokens", 0, "Abort if tokens exceed this value")
	queueAddCmd.Flags().Duration("timeout", 0, "Timeout for the run (e.g. 10m)")
	queueAddCmd.Flags().String("prompt-file", "", "Read prompt from this file instead of stdin")

	queueListCmd.Flags().String("status", "", "Filter by status: pending|running|done|failed|all")
	queueListCmd.Flags().Bool("json", false, "Emit NDJSON (one object per line)")

	queueRunCmd.Flags().Int("concurrent", 1, "Number of tasks to run concurrently")
	queueRunCmd.Flags().Bool("stop-on-fail", false, "Stop processing if any task fails")
	queueRunCmd.Flags().Int("max-tasks", 0, "Maximum number of tasks to process (0 = unlimited)")

	queueClearCmd.Flags().String("status", "", "Only clear tasks with this status")

	queueCmd.AddCommand(queueAddCmd, queueListCmd, queueShowCmd, queueRunCmd, queueRmCmd, queueClearCmd)
	rootCmd.AddCommand(queueCmd)
}
