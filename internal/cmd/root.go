package cmd

import (
	"context"
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
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/projects"
	"github.com/charmbracelet/crush/internal/server"
	"github.com/charmbracelet/crush/internal/version"
	"github.com/charmbracelet/fang"
	"github.com/charmbracelet/x/term"
	crushweb "github.com/charmbracelet/crush/web"
	"github.com/pkg/browser"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.PersistentFlags().StringP("cwd", "c", "", "Current working directory")
	rootCmd.PersistentFlags().StringP("data-dir", "D", "", "Custom crush data directory")
	rootCmd.PersistentFlags().BoolP("debug", "d", false, "Debug")
	rootCmd.Flags().BoolP("help", "h", false, "Help")
	rootCmd.Flags().BoolP("yolo", "y", false, "Automatically accept all permissions (dangerous mode)")
	rootCmd.Flags().StringP("host", "H", "localhost", "Web mode: host to listen on")
	rootCmd.Flags().IntP("port", "p", 0, "Web mode: port to listen on (0 = random free port)")
	rootCmd.Flags().Bool("no-open", false, "Web mode: do not open browser automatically")

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
	Short: "An AI assistant for software development",
	Long:  "An AI assistant for software development and similar tasks with direct access to the terminal",
	Example: `
# Run in interactive mode
crush

# Run with debug logging
crush -d

# Run with debug logging in a specific directory
crush -d -c /path/to/project

# Run with custom data directory
crush -D /path/to/custom/.crush

# Print version
crush -v

# Run a single non-interactive prompt
crush run "Explain the use of context in Go"

# Run in dangerous mode (auto-accept all permissions)
crush -y
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

	event.AppInitialized()

	addr := fmt.Sprintf("%s:%d", host, port)
	srv := server.New(a, addr, crushweb.FS())
	token := srv.Token()

	onReady := func(boundAddr string) {
		url := fmt.Sprintf("http://%s", boundAddr)
		fmt.Println()
		fmt.Printf("  crush web UI  →  %s\n", url)
		if err := clipboard.WriteAll(token); err == nil {
			fmt.Printf("  Access token  →  %s (copied to clipboard)\n", token)
		} else {
			fmt.Printf("  Access token  →  %s\n", token)
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

	cfg, err := config.Init(cwd, dataDir, debug)
	if err != nil {
		return nil, err
	}

	if cfg.Permissions == nil {
		cfg.Permissions = &config.Permissions{}
	}
	cfg.Permissions.SkipRequests = yolo

	if err := createDotCrushDir(cfg.Options.DataDirectory); err != nil {
		return nil, err
	}

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

	appInstance, err := app.New(ctx, conn, cfg)
	if err != nil {
		slog.Error("Failed to create app instance", "error", err)
		return nil, err
	}

	if shouldEnableMetrics(cfg) {
		event.Init()
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
	if _, err := os.Stat(gitIgnorePath); os.IsNotExist(err) {
		if err := os.WriteFile(gitIgnorePath, []byte("*\n"), 0o644); err != nil {
			return fmt.Errorf("failed to create .gitignore file: %q %w", gitIgnorePath, err)
		}
	}

	return nil
}
