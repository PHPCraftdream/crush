// Fork addition: `gemini-init` installs the `crush`/`crush-fallback` slash
// commands as Gemini CLI custom commands (`.gemini/commands/*.toml`). Part
// of the `<tool>-init`/`<tool>-del` family alongside claude-init/claude-del
// and codex-init/codex-del; grok-init/grok-del and qwen-init/qwen-del follow
// the same pattern, converting from the same canonical source templates
// (claudeSlashCommandTemplate / claudeFallbackCommandTemplate, embedded in
// claude_init.go) via the helpers in multi_cli_convert.go.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

const geminiCommandsDir = ".gemini/commands" // relative to cwd (local) or $HOME (global)

// geminiSlashCommandSentinel is the substring toGeminiTOML embeds as its
// first line (`# crush-slash-command:v1`). We can't reuse the full
// claudeSlashCommandSentinel constant here (`<!-- crush-slash-command:v1
// -->`) verbatim as the ownership-check substring — TOML uses `#` comments,
// not HTML comments, so that exact string never appears in a generated
// .toml file. This narrower substring is what actually shows up in both,
// and is used consistently by both write (installGeminiCommands) and
// remove (removeGeminiCommands).
const geminiSlashCommandSentinel = "crush-slash-command:v1"

// resolveGeminiCommandsDir returns the directory Gemini CLI custom commands
// should be written to. When global is true it returns ~/.gemini/commands;
// otherwise <cwd>/.gemini/commands. Unlike Codex's Skills convention, Gemini
// custom commands are flat files (crush.toml, crush-fallback.toml) directly
// inside this directory, not nested under a per-command subdirectory.
func resolveGeminiCommandsDir(cwd string, global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		return filepath.Join(home, geminiCommandsDir), nil
	}
	return filepath.Join(cwd, geminiCommandsDir), nil
}

var geminiInitCmd = &cobra.Command{
	Use:   "gemini-init",
	Short: "Install the crush/crush-fallback custom commands for Gemini CLI",
	Long: `Set up crush's delegation custom commands in Gemini CLI.

Gemini CLI custom commands are written to ` + "`~/.gemini/commands/crush.toml`" + ` and
` + "`~/.gemini/commands/crush-fallback.toml`" + ` by default (the GLOBAL scope,
available in every project). Use --local (or --cwd, which implies it) to
scope them to the current project's ` + "`.gemini/commands/`" + ` instead.

Content is converted from the same canonical source used by
` + "`claude-init`" + ` — the two stay in sync automatically.

Skipped (with a warning) if a target .toml file exists without our sentinel
— we never overwrite a file we don't own.`,
	Example: `
# Install / refresh the Gemini custom commands globally — the default
crush gemini-init

# Install into the current project instead
crush gemini-init --local

# Scope to another project (implies --local)
crush gemini-init --cwd /path/to/project
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		global, _ := cmd.Flags().GetBool("global")
		local, _ := cmd.Flags().GetBool("local")
		hasCwd := cmd.Flags().Changed("cwd")
		localMode := local || hasCwd

		if global && localMode {
			return fmt.Errorf("--global and --local/--cwd are mutually exclusive")
		}

		var commandsDir string
		if localMode {
			cwd, err := ResolveCwd(cmd)
			if err != nil {
				return err
			}
			commandsDir, err = resolveGeminiCommandsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			commandsDir, err = resolveGeminiCommandsDir("", true)
			if err != nil {
				return err
			}
		}

		return installGeminiCommands(commandsDir)
	},
}

// installGeminiCommands writes both the crush and crush-fallback custom
// commands into commandsDir. Extracted so gemini_init_test.go can drive it
// directly.
func installGeminiCommands(commandsDir string) error {
	desc1, body1, err := parseSlashCommandSource(claudeSlashCommandTemplate)
	if err != nil {
		return fmt.Errorf("crush command: %w", err)
	}
	content1, err := toGeminiTOML(desc1, body1)
	if err != nil {
		return fmt.Errorf("crush command: %w", err)
	}
	if err := writeSentinelledFile(filepath.Join(commandsDir, "crush.toml"), geminiSlashCommandSentinel, content1); err != nil {
		return fmt.Errorf("crush command: %w", err)
	}

	desc2, body2, err := parseSlashCommandSource(claudeFallbackCommandTemplate)
	if err != nil {
		return fmt.Errorf("crush-fallback command: %w", err)
	}
	content2, err := toGeminiTOML(desc2, body2)
	if err != nil {
		return fmt.Errorf("crush-fallback command: %w", err)
	}
	if err := writeSentinelledFile(filepath.Join(commandsDir, "crush-fallback.toml"), geminiSlashCommandSentinel, content2); err != nil {
		return fmt.Errorf("crush-fallback command: %w", err)
	}
	return nil
}

func init() {
	geminiInitCmd.Flags().Bool("global", false, "Install into ~/.gemini/commands/ (available in every project). Default when neither --global nor --local is given.")
	geminiInitCmd.Flags().Bool("local", false, "Install into the current project's .gemini/commands/ instead of ~/.gemini/commands/.")
	rootCmd.AddCommand(geminiInitCmd)
}
