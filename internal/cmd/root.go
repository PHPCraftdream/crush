package cmd

// Fork patch: the upstream `rootCmd` launches the Bubble Tea TUI. In this fork
// it launches the embedded web server (`crush web`) by default, opens the
// browser, and exposes the `--host`, `--port`, `--no-open` flags. The TUI
// import tree (bubbletea, fang/v2 client wiring, internal/ui/model, etc.) and
// the `--host`-as-REST-client logic from upstream are intentionally removed
// here. See CHANGELOG.fork.md section 2 ("internal/cmd/root.go") and section
// 4.A ("WebSocket server") before resolving any merge conflict in this file.

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	crushlog "github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/projects"
	"github.com/charmbracelet/crush/internal/server"
	"github.com/charmbracelet/crush/internal/version"
	"charm.land/fang/v2"
	"github.com/charmbracelet/x/term"
	crushweb "github.com/charmbracelet/crush/web"
	"github.com/pkg/browser"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.PersistentFlags().StringP("cwd", "c", "", "Working directory crush operates in (absolute or relative). Applies to every subcommand; the .crush/ store and any tool-side relative paths resolve against it.")
	rootCmd.PersistentFlags().StringP("data-dir", "D", "", "Override the .crush/ data directory (sessions DB, logs, attachments). Defaults to <cwd>/.crush.")
	rootCmd.PersistentFlags().BoolP("debug", "d", false, "Debug")
	rootCmd.Flags().BoolP("help", "h", false, "Help")
	rootCmd.Flags().BoolP("yolo", "y", false, "Auto-approve every permission request (dangerous)")
	rootCmd.Flags().StringP("host", "H", "localhost", "Host to bind the web UI to")
	rootCmd.Flags().IntP("port", "p", 0, "Port to bind the web UI to (0 = pick a free one)")
	rootCmd.Flags().Bool("no-open", false, "Do not open the browser after the server starts")

	rootCmd.AddCommand(
		runCmd,
		dirsCmd,
		projectsCmd,
		updateProvidersCmd,
		logsCmd,
		schemaCmd,
		loginCmd,
		statsCmd,
	)
}

var rootCmd = &cobra.Command{
	Use:   "crush",
	Short: "Run the Crush coding agent with a browser-based UI",
	Long: `Crush is an AI coding assistant. Running ` + "`crush`" + ` (or ` + "`crush web`" + `)
starts a local HTTP + WebSocket server, prints the URL and a one-time
access token, and opens your default browser to the UI.

The web UI lets you chat with the agent, switch models per session, inspect
and revoke tool permissions, browse logs, queue messages while the agent
is busy, and interrupt the running turn (yellow Interrupt button) to fold
a correction into the next step while keeping everything produced so far.

Companion CLI subcommands for scripting and CI:
  - ` + "`crush run`" + `             one-shot prompt; --session, --timeout, --max-cost,
                          --max-tokens, --on-finish, --json.
  - ` + "`crush sessions`" + `        list / show / delete / last / tail / locks / watch /
                          pick / grep / cost / diff / tree / fork / cancel / gc.
  - ` + "`crush queue`" + `           batch task queue — add / list / run / rm / clear / show.
  - ` + "`crush models`" + `          use / list / set / unset — atom-based model selection
                          with short codes (o47x, s46h, hl, etc.).
  - ` + "`crush claude-init`" + `     install 31 slash-commands + 31 sub-agents into
                          .claude/commands/ and .claude/agents/.
  - ` + "`crush system-prompt`" + `   print the system prompt that would be sent.
  - ` + "`crush ping`" + `            health-check (verify API connectivity).`,
	Example: `
# Start the web UI on a random free port and open the browser
crush

# Pin the port and bind to all interfaces (e.g. for a remote dev box)
crush --host 0.0.0.0 --port 9000

# Start the server without opening the browser (useful for IDE integrations)
crush --no-open --port 8080

# Auto-approve every permission request — only use in a disposable workspace
crush --yolo

# Run with debug logging from a specific working directory
crush --debug --cwd /path/to/project

# Use a non-default data directory for state (.crush/)
crush --data-dir /path/to/custom/.crush

# Non-interactive one-shot prompt (see "crush run --help" for more)
crush run "Summarise the changes on this branch"

# Pipe stdin into a one-shot prompt
cat README.md | crush run "Make this more glamorous" > GLAMOROUS_README.md

# With cost cap and timeout (900 = 900 seconds)
crush run --max-cost 0.50 --max-tokens 100k --timeout 900 "refactor storage"

# Idempotent CI invocation with structured output
crush run --session "pr-42" --json "Review the diff" | jq .final_text

# Session management
crush sessions list                    # list all sessions
crush sessions last pr-42 --n 5       # last 5 messages with timestamps
crush sessions tree                    # parent-child hierarchy
crush sessions cancel pr-42            # graceful cancel via DB flag
crush sessions fork pr-42 --at 10     # branch from message 10
crush sessions grep "import error"    # search message text
crush sessions cost --by model        # cost breakdown by model
crush sessions diff pr-42             # files touched (from ToolCalls)
crush sessions pick                   # interactive TUI picker

# Task queue
crush queue add --role smart --max-cost 0.20 < task.prompt
crush queue run --concurrent 2 --stop-on-fail

# Model selection with short codes
crush models use o47x h45l            # Opus 4.7 xhigh + Haiku low
crush models use oh sl                # top Opus high + top Sonnet low

# Install slash-commands & sub-agents for Claude Code
crush claude-init --global
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWebMode(cmd)
	},
}

func runWebMode(cmd *cobra.Command) error {
	host, _ := cmd.Flags().GetString("host")
	port, _ := cmd.Flags().GetInt("port")
	noOpen, _ := cmd.Flags().GetBool("no-open")

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()


	addr := fmt.Sprintf("%s:%d", host, port)
	srv := server.New(a, addr, crushweb.FS())
	token := srv.Token()

	onReady := func(boundAddr string) {
		url := fmt.Sprintf("http://%s", boundAddr)
		fmt.Println()
		fmt.Printf("  crush web UI  ΓåÆ  %s\n", url)
		if err := clipboard.WriteAll(token); err == nil {
			fmt.Printf("  Access token  ΓåÆ  %s (copied to clipboard)\n", token)
		} else {
			fmt.Printf("  Access token  ΓåÆ  %s\n", token)
		}
		fmt.Println()

		if !noOpen {
			go func() {
				time.Sleep(200 * time.Millisecond)
				if err := browser.OpenURL(url); err != nil {
					slog.Debug("web: could not open browser", "err", err)
				}
			}()
		}
	}

	return srv.Start(cmd.Context(), onReady)
}

func Execute() {
	if err := fang.Execute(
		context.Background(),
		rootCmd,
		fang.WithVersion(version.Version),
		fang.WithNotifySignal(os.Interrupt),
	); err != nil {
		os.Exit(1)
	}
}

// setupApp handles the common setup logic for both interactive and non-interactive modes.
// It returns the app instance, config, cleanup function, and any error.
func setupApp(cmd *cobra.Command) (*app.App, error) {
	debug, _ := cmd.Flags().GetBool("debug")
	yolo, _ := cmd.Flags().GetBool("yolo")
	dataDir, _ := cmd.Flags().GetString("data-dir")
	ctx := cmd.Context()

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil, err
	}

	store, err := config.Init(cwd, dataDir, debug)
	if err != nil {
		return nil, err
	}

	cfg := store.Config()
	if cfg.Permissions == nil {
		cfg.Permissions = &config.Permissions{}
	}
	cfg.Permissions.SkipRequests = yolo

	if err := createDotCrushDir(cfg.Options.DataDirectory); err != nil {
		return nil, err
	}

	// Fork merge note: when we dropped upstream's serverCmd + connectToServer
	// during the May-16 merge we accidentally dropped the only two callers
	// of crushlog.Setup(). The slog.Default() handler then stayed at the
	// terminal-writing default, so .crush/logs/crush.log silently stopped
	// receiving new entries for both `crush` (web) and `crush run`. Wiring
	// the call here in setupApp re-points the default logger at the same
	// file path the WUI/Logs modal already expects to read from.
	crushlog.Setup(filepath.Join(cfg.Options.DataDirectory, "logs", "crush.log"), debug)

	// Register this project in the centralized projects list.
	if err := projects.Register(cwd, cfg.Options.DataDirectory); err != nil {
		slog.Warn("Failed to register project", "error", err)
		// Non-fatal: continue even if registration fails
	}

	// Connect to DB; this will also run migrations.
	conn, err := db.Connect(ctx, cfg.Options.DataDirectory)
	if err != nil {
		return nil, err
	}

	appInstance, err := app.New(ctx, conn, store)
	if err != nil {
		slog.Error("Failed to create app instance", "error", err)
		return nil, err
	}

	if shouldEnableMetrics(cfg) {
	}

	return appInstance, nil
}

func shouldEnableMetrics(cfg *config.Config) bool {
	if v, _ := strconv.ParseBool(os.Getenv("CRUSH_DISABLE_METRICS")); v {
		return false
	}
	if v, _ := strconv.ParseBool(os.Getenv("DO_NOT_TRACK")); v {
		return false
	}
	if cfg.Options.DisableMetrics {
		return false
	}
	return true
}

func MaybePrependStdin(prompt string) (string, error) {
	if term.IsTerminal(os.Stdin.Fd()) {
		return prompt, nil
	}
	fi, err := os.Stdin.Stat()
	if err != nil {
		return prompt, err
	}
	// Check if stdin is a named pipe ( | ) or regular file ( < ).
	if fi.Mode()&os.ModeNamedPipe == 0 && !fi.Mode().IsRegular() {
		return prompt, nil
	}
	bts, err := io.ReadAll(os.Stdin)
	if err != nil {
		return prompt, err
	}
	return string(bts) + "\n\n" + prompt, nil
}

func ResolveCwd(cmd *cobra.Command) (string, error) {
	cwd, _ := cmd.Flags().GetString("cwd")
	if cwd != "" {
		err := os.Chdir(cwd)
		if err != nil {
			return "", fmt.Errorf("failed to change directory: %v", err)
		}
		return cwd, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %v", err)
	}
	return cwd, nil
}

func createDotCrushDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create data directory: %q %w", dir, err)
	}

	gitIgnorePath := filepath.Join(dir, ".gitignore")
	content, err := os.ReadFile(gitIgnorePath)

	// create or update if old version
	if os.IsNotExist(err) || string(content) == oldGitIgnore {
		if err := os.WriteFile(gitIgnorePath, []byte(defaultGitIgnore), 0o644); err != nil {
			return fmt.Errorf("failed to create .gitignore file: %q %w", gitIgnorePath, err)
		}
	}

	return nil
}

//go:embed gitignore/old
var oldGitIgnore string

//go:embed gitignore/default
var defaultGitIgnore string
