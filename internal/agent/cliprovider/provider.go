// Package cliprovider implements a fantasy.Provider that invokes local CLI tools.
// Each CLISpec describes one hardcoded model: which binary to run and how to
// build its arguments from the prompt text and the yolo flag.
//
// To add a new CLI model, append a new CLISpec to the [All] slice.
package cliprovider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/object"
	gopty "github.com/aymanbagabas/go-pty"
	"github.com/charmbracelet/crush/internal/permission"
)

// ProviderType is the catwalk.Type value used for CLI providers.
const ProviderType = "cli"

// ProviderID is the catwalk.InferenceProvider ID for the built-in CLI provider.
const ProviderID = "local-cli"

// maxPromptArgLen is the maximum prompt length (in bytes) that will be passed
// as a CLI argument. Longer prompts are piped via stdin to avoid OS limits.
const maxPromptArgLen = 30 * 1024

// ansiEscape matches ANSI/VT escape sequences injected by PTY drivers:
//   - CSI sequences: ESC [ <params> <letter>  (e.g. \x1b[2J, \x1b[?25h)
//   - OSC sequences: ESC ] <text> BEL         (e.g. \x1b]0;title\a)
//   - other two-char escapes: ESC <char>
//
// Also strips bare \r so JSON lines from PTY output parse cleanly.
var ansiEscape = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[a-zA-Z]|\][^\x07]*\x07|[^\[])|\r`)

// CLISpec describes how to invoke a single CLI-based language model.
type CLISpec struct {
	// ModelID is the model identifier used in the crush UI (e.g. "cli-claude").
	ModelID string
	// ModelName is the human-readable display name.
	ModelName string
	// ContextWindow is the total context window size in tokens.
	// Used by crush to decide when to trigger auto-summarization.
	ContextWindow int64
	// Binary is the executable name resolved via PATH (e.g. "claude", "gemini").
	Binary string
	// PromptFlag is the CLI flag used to pass the prompt inline (e.g. "-p").
	// When the prompt exceeds maxPromptArgLen, it is piped via stdin instead.
	PromptFlag string
	// BuildArgs returns the CLI arguments for a given yolo flag.
	// The prompt is NOT included — it is added separately by Stream.
	BuildArgs func(yolo bool) []string
	// NewPartParser returns a stateful function that maps a JSON line to a
	// StreamPart. Supports text and reasoning (thinking) deltas. If nil, raw
	// text mode is used (lines are stripped of ANSI escapes and yielded as-is).
	NewPartParser func() func(line []byte) (fantasy.StreamPart, bool)
	// ParseUsageLine parses token usage from a single output line.
	// Called on every line; returns (usage, true) when usage data is found.
	// If nil, usage will be zero in the Finish stream part.
	ParseUsageLine func(line []byte) (fantasy.Usage, bool)
	// UseCrushMCP controls whether crush starts an internal MCP server and
	// passes it to the CLI process via --mcp-config.  When true and the
	// provider is running in non-yolo mode, tool calls are routed through
	// crush's permission system instead of the CLI's own permission handling.
	UseCrushMCP bool
}

// streamEvent is a generic JSON envelope for both Claude and Gemini
// stream-json output. Only the fields relevant to text extraction are parsed.
type streamEvent struct {
	Type string `json:"type"`
	// stream_event: raw Anthropic API SSE event forwarded by claude CLI (--verbose).
	// content_block_delta events carry text tokens (text_delta) or thinking (thinking_delta).
	Event struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type     string `json:"type"`    // "text_delta" or "thinking_delta"
			Text     string `json:"text"`    // text_delta content
			Thinking string `json:"thinking"` // thinking_delta content
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"` // "text" or "thinking"
		} `json:"content_block"`
	} `json:"event"`
	// assistant: accumulated text snapshot (--include-partial-messages)
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	// Gemini candidates
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Text  string `json:"text"`
	// Claude CLI result event usage (snake_case).
	Usage struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
	// Gemini CLI final chunk usage (camelCase).
	UsageMetadata struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		TotalTokenCount      int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// claudePartParser returns a stateful parser for Claude CLI stream-json output.
// With --verbose, claude CLI emits "stream_event" events wrapping raw Anthropic
// API SSE events. Handles both text tokens (text_delta) and thinking tokens
// (thinking_delta) so the reasoning box is visible during extended thinking.
func claudePartParser() func([]byte) (fantasy.StreamPart, bool) {
	const id = "0"
	var inThinking bool
	return func(line []byte) (fantasy.StreamPart, bool) {
		var ev streamEvent
		if json.Unmarshal(line, &ev) != nil {
			return fantasy.StreamPart{}, false
		}
		if ev.Type != "stream_event" {
			return fantasy.StreamPart{}, false
		}
		switch ev.Event.Type {
		case "content_block_start":
			slog.Debug("cliprovider: content_block_start", "block_type", ev.Event.ContentBlock.Type)
			if ev.Event.ContentBlock.Type == "thinking" {
				inThinking = true
				return fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: id}, true
			}
		case "content_block_stop":
			if inThinking {
				inThinking = false
				return fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: id}, true
			}
		case "content_block_delta":
			switch ev.Event.Delta.Type {
			case "thinking_delta":
				if ev.Event.Delta.Thinking != "" {
					return fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: id, Delta: ev.Event.Delta.Thinking}, true
				}
			case "text_delta":
				if ev.Event.Delta.Text != "" {
					return fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: ev.Event.Delta.Text}, true
				}
			}
		}
		return fantasy.StreamPart{}, false
	}
}

// geminiPartParser returns a parser for Gemini CLI stream-json output.
// Gemini emits per-chunk events; each contains only the new text delta.
func geminiPartParser() func([]byte) (fantasy.StreamPart, bool) {
	const id = "0"
	return func(line []byte) (fantasy.StreamPart, bool) {
		var ev streamEvent
		if json.Unmarshal(line, &ev) != nil {
			return fantasy.StreamPart{}, false
		}
		for _, c := range ev.Candidates {
			for _, p := range c.Content.Parts {
				if p.Text != "" {
					return fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: p.Text}, true
				}
			}
		}
		if ev.Text != "" {
			return fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: ev.Text}, true
		}
		return fantasy.StreamPart{}, false
	}
}

// claudeParseUsageLine extracts token usage from a Claude CLI "result" event.
// Claude emits a final {"type":"result",...,"usage":{...}} line that includes
// both direct and cached token counts. We sum all input variants so that
// cached conversations report accurate totals rather than just the tiny
// non-cached portion.
func claudeParseUsageLine(line []byte) (fantasy.Usage, bool) {
	var ev streamEvent
	if json.Unmarshal(line, &ev) != nil {
		return fantasy.Usage{}, false
	}
	if ev.Type != "result" {
		return fantasy.Usage{}, false
	}
	// Total input = direct + cache-creation + cache-read tokens.
	inputTotal := ev.Usage.InputTokens + ev.Usage.CacheCreationInputTokens + ev.Usage.CacheReadInputTokens
	if inputTotal == 0 && ev.Usage.OutputTokens == 0 {
		return fantasy.Usage{}, false
	}
	return fantasy.Usage{
		InputTokens:  inputTotal,
		OutputTokens: ev.Usage.OutputTokens,
		TotalTokens:  inputTotal + ev.Usage.OutputTokens,
	}, true
}

// geminiParseUsageLine extracts token usage from a Gemini CLI stream-json
// output. Gemini includes usageMetadata in the final chunk of the stream.
func geminiParseUsageLine(line []byte) (fantasy.Usage, bool) {
	var ev streamEvent
	if json.Unmarshal(line, &ev) != nil {
		return fantasy.Usage{}, false
	}
	if ev.UsageMetadata.TotalTokenCount == 0 {
		return fantasy.Usage{}, false
	}
	return fantasy.Usage{
		InputTokens:  ev.UsageMetadata.PromptTokenCount,
		OutputTokens: ev.UsageMetadata.CandidatesTokenCount,
		TotalTokens:  ev.UsageMetadata.TotalTokenCount,
	}, true
}

// claudeArgs returns a BuildArgs func for a claude CLI model.
// extra allows passing additional static flags (e.g. "--effort", "high").
func claudeArgs(model string, extra ...string) func(bool) []string {
	return func(yolo bool) []string {
		args := []string{
			"--model", model,
			"--output-format", "stream-json",
			"--verbose",
			"--include-partial-messages",
		}
		args = append(args, extra...)
		if yolo {
			args = append(args, "--dangerously-skip-permissions")
		}
		return args
	}
}

// geminiArgs returns a BuildArgs func for a gemini CLI model.
func geminiArgs(model string) func(bool) []string {
	return func(yolo bool) []string {
		args := []string{
			"-m", model,
			"--output-format", "stream-json",
		}
		if yolo {
			args = append(args, "-y")
		}
		return args
	}
}

// All is the list of hardcoded CLI model specs.
// Add new entries here to register additional CLI-backed models.
var All = []CLISpec{
	{
		ModelID:        "cli-claude-sonnet",
		ModelName:      "Claude Sonnet (CLI)",
		ContextWindow:  200_000,
		Binary:         "claude",
		PromptFlag:     "-p",
		BuildArgs:      claudeArgs("sonnet"),
		NewPartParser:  claudePartParser,
		ParseUsageLine: claudeParseUsageLine,
		UseCrushMCP:    true,
	},
	{
		ModelID:        "cli-claude-opus",
		ModelName:      "Claude Opus (CLI)",
		ContextWindow:  1_000_000,
		Binary:         "claude",
		PromptFlag:     "-p",
		BuildArgs:      claudeArgs("opus"),
		NewPartParser:  claudePartParser,
		ParseUsageLine: claudeParseUsageLine,
		UseCrushMCP:    true,
	},
	{
		ModelID:        "cli-claude-sonnet-thinking",
		ModelName:      "Claude Sonnet Thinking (CLI)",
		ContextWindow:  200_000,
		Binary:         "claude",
		PromptFlag:     "-p",
		BuildArgs:      claudeArgs("sonnet", "--effort", "high"),
		NewPartParser:  claudePartParser,
		ParseUsageLine: claudeParseUsageLine,
		UseCrushMCP:    true,
	},
	{
		ModelID:        "cli-claude-opus-thinking",
		ModelName:      "Claude Opus Thinking (CLI)",
		ContextWindow:  1_000_000,
		Binary:         "claude",
		PromptFlag:     "-p",
		BuildArgs:      claudeArgs("opus", "--effort", "high"),
		NewPartParser:  claudePartParser,
		ParseUsageLine: claudeParseUsageLine,
		UseCrushMCP:    true,
	},
	{
		ModelID:        "cli-gemini-flash",
		ModelName:      "Gemini 3 Flash (CLI)",
		ContextWindow:  1_000_000,
		Binary:         "gemini",
		PromptFlag:     "-p",
		BuildArgs:      geminiArgs("gemini-3-flash"),
		NewPartParser:  geminiPartParser,
		ParseUsageLine: geminiParseUsageLine,
	},
	{
		ModelID:        "cli-gemini-pro",
		ModelName:      "Gemini 3.1 Pro (CLI)",
		ContextWindow:  1_000_000,
		Binary:         "gemini",
		PromptFlag:     "-p",
		BuildArgs:      geminiArgs("gemini-3.1-pro-preview"),
		NewPartParser:  geminiPartParser,
		ParseUsageLine: geminiParseUsageLine,
	},
}

// Available returns the subset of All whose Binary is found in PATH.
func Available() []CLISpec {
	seen := make(map[string]bool)
	var result []CLISpec
	for _, spec := range All {
		if !seen[spec.Binary] {
			_, err := exec.LookPath(spec.Binary)
			seen[spec.Binary] = err == nil
		}
		if seen[spec.Binary] {
			result = append(result, spec)
		}
	}
	return result
}

type cliProvider struct {
	workingDir string
	yoloFn     func() bool
	perms      permission.Service
	specs      map[string]CLISpec
}

// New creates a CLI provider that runs all specs from [All].
// workingDir is set as the working directory for every CLI invocation.
// yoloFn is called at request time to decide whether to pass the auto-accept flag.
// perms is used to show crush's permission dialog when UseCrushMCP specs are invoked.
func New(workingDir string, yoloFn func() bool, perms permission.Service) fantasy.Provider {
	specs := make(map[string]CLISpec, len(All))
	for _, s := range All {
		specs[s.ModelID] = s
	}
	return &cliProvider{workingDir: workingDir, yoloFn: yoloFn, perms: perms, specs: specs}
}

func (p *cliProvider) Name() string { return ProviderID }

func (p *cliProvider) LanguageModel(_ context.Context, modelID string) (fantasy.LanguageModel, error) {
	spec, ok := p.specs[modelID]
	if !ok {
		return nil, fmt.Errorf("unknown CLI model: %q", modelID)
	}
	return &cliModel{spec: spec, workingDir: p.workingDir, yoloFn: p.yoloFn, perms: p.perms}, nil
}

type cliModel struct {
	spec       CLISpec
	workingDir string
	yoloFn     func() bool
	perms      permission.Service
}

func (m *cliModel) Provider() string { return ProviderID }
func (m *cliModel) Model() string    { return m.spec.ModelID }

func (m *cliModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return object.GenerateWithTool(ctx, m, call)
}

func (m *cliModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return object.StreamWithTool(ctx, m, call)
}

func (m *cliModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	var text strings.Builder
	var usage fantasy.Usage
	stream, err := m.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			return nil, part.Error
		}
		if part.Type == fantasy.StreamPartTypeTextDelta {
			text.WriteString(part.Delta)
		}
		if part.Type == fantasy.StreamPartTypeFinish {
			usage = part.Usage
		}
	}
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: text.String()}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        usage,
	}, nil
}

func (m *cliModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	yolo := m.yoloFn != nil && m.yoloFn()
	prompt := formatPrompt(call.Prompt)

	args := m.spec.BuildArgs(yolo)

	// When running in non-yolo mode with a spec that opts into crush's MCP
	// server, start an in-process MCP server and pass its config to the CLI
	// so tool calls go through crush's permission dialog instead of the CLI's
	// own (invisible) permission prompts.
	// mcpSrv and mcpTmpCfg are cleaned up inside the returned closure, not
	// via defer here — defer in Stream() would fire when Stream() returns
	// (before the closure runs), deleting the config file before claude CLI
	// can read it.
	var mcpSrv *crushMCPServer
	var mcpTmpCfg string // path to temp MCP config file; "" if not used
	if m.spec.UseCrushMCP && !yolo && m.perms != nil {
		var err error
		mcpSrv, err = newCrushMCPServer(ctx, m.perms, m.workingDir)
		if err != nil {
			slog.Warn("cliprovider: failed to start MCP server, falling back to CLI permissions", "err", err)
		} else {
			cfgJSON, jsonErr := mcpSrv.mcpConfigJSON()
			if jsonErr != nil {
				slog.Warn("cliprovider: failed to marshal MCP config", "err", jsonErr)
				mcpSrv.stop()
				mcpSrv = nil
			} else {
				// Write the config to a temp file; the claude CLI reads it via --mcp-config.
				tmpFile, tmpErr := os.CreateTemp("", "crush-mcp-*.json")
				if tmpErr != nil {
					slog.Warn("cliprovider: failed to create MCP config temp file", "err", tmpErr)
					mcpSrv.stop()
					mcpSrv = nil
				} else {
					if _, werr := tmpFile.Write(cfgJSON); werr != nil {
						slog.Warn("cliprovider: failed to write MCP config", "err", werr)
						_ = tmpFile.Close()
						_ = os.Remove(tmpFile.Name())
						mcpSrv.stop()
						mcpSrv = nil
					} else {
						_ = tmpFile.Close()
						mcpTmpCfg = tmpFile.Name()
						args = append(args, "--mcp-config", mcpTmpCfg)
						slog.Info("cliprovider: MCP config written", "path", mcpTmpCfg, "addr", mcpSrv.addr)
					}
				}
			}
		}
	}

	// When crush's own MCP server is active, tell the CLI to only allow our
	// MCP tools. This pre-approves them inside the CLI's own permission layer
	// (so calls reach our handlers), while the CLI's built-in tools remain
	// blocked. Crush still shows its own permission dialog in the UI for each
	// tool call via perms.Request() inside the MCP handlers.
	if mcpSrv != nil {
		args = append(args,
			"--allowedTools",
			"mcp__crush__Bash,mcp__crush__Read,mcp__crush__Write,mcp__crush__Glob,mcp__crush__Grep",
		)
	}

	useStdin := len(prompt) > maxPromptArgLen
	if !useStdin {
		args = append(args, m.spec.PromptFlag, prompt)
	}

	// procHandle abstracts PTY-backed and pipe-backed processes behind a
	// uniform interface so the streaming loop below is platform-agnostic.
	type procHandle struct {
		stdout   io.Reader
		usingPTY bool
		// kill aborts the process and blocks until all resources are freed.
		kill func()
		// wait blocks until the process exits; returns (stderr output, error).
		// In PTY mode stderr is merged so it is always "".
		wait func() (string, error)
	}

	var proc procHandle

	if !useStdin {
		// Use a PTY so the subprocess (e.g. Node.js claude CLI) sees a TTY on
		// stdout and does not buffer output internally. go-pty supports both
		// Unix PTY and Windows ConPTY transparently.
		//
		// On Windows, ClosePseudoConsole (called by p.Close) is what signals
		// EOF on the output pipe — the process exiting alone does not do it.
		// We therefore run Wait in a goroutine and close the PTY afterwards,
		// which guarantees the scanner always sees EOF on both platforms.
		p, ptyErr := gopty.New()
		if ptyErr == nil {
			// Resize to a very wide terminal to prevent the PTY from hard-wrapping
			// long JSON lines. Claude CLI emits lines that can be many KB; wrapping
			// at the default 80-column width splits them across scanner tokens,
			// causing json.Unmarshal to fail on every partial line.
			_ = p.Resize(8192, 50)
			// Resolve the binary to an absolute path before passing to go-pty.
			// On Windows, go-pty/ConPTY may resolve binary names relative to
			// cmd.Dir instead of PATH, so we do the PATH lookup ourselves.
			binaryPath := m.spec.Binary
			if resolved, lookErr := exec.LookPath(m.spec.Binary); lookErr == nil {
				binaryPath = resolved
			}
			ptycmd := p.CommandContext(ctx, binaryPath, args...)
			ptycmd.Dir = m.workingDir
			if startErr := ptycmd.Start(); startErr == nil {
				slog.Info("cliprovider: using PTY", "binary", binaryPath)
				waitCh := make(chan error, 1)
				go func() {
					waitCh <- ptycmd.Wait()
					_ = p.Close() // EOF for scanner (required on Windows ConPTY)
				}()
				proc = procHandle{
					stdout:   p,
					usingPTY: true,
					kill: func() {
						if ptycmd.Process != nil {
							_ = ptycmd.Process.Kill()
						}
						<-waitCh // drain so goroutine can finish
					},
					wait: func() (string, error) {
						return "", <-waitCh
					},
				}
			} else {
				_ = p.Close()
				slog.Info("cliprovider: PTY start failed, falling back to pipe", "err", startErr)
			}
		} else {
			slog.Info("cliprovider: PTY unavailable, falling back to pipe", "err", ptyErr)
		}
	}

	if proc.stdout == nil {
		// Pipe fallback: large prompt (stdin required) or PTY unavailable.
		cmd := exec.CommandContext(ctx, m.spec.Binary, args...)
		cmd.Dir = m.workingDir
		if useStdin {
			cmd.Stdin = strings.NewReader(prompt)
		}
		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf
		pipe, pipeErr := cmd.StdoutPipe()
		if pipeErr != nil {
			return nil, fmt.Errorf("stdout pipe: %w", pipeErr)
		}
		if startErr := cmd.Start(); startErr != nil {
			return nil, fmt.Errorf("start %s: %w", m.spec.Binary, startErr)
		}
		proc = procHandle{
			stdout:   pipe,
			usingPTY: false,
			kill: func() {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				_ = cmd.Wait()
			},
			wait: func() (string, error) {
				return strings.TrimSpace(stderrBuf.String()), cmd.Wait()
			},
		}
	}

	var parsePart func([]byte) (fantasy.StreamPart, bool)
	if m.spec.NewPartParser != nil {
		parsePart = m.spec.NewPartParser()
	}

	return func(yield func(fantasy.StreamPart) bool) {
		// Cleanup MCP resources when the stream ends (cannot use defer in
		// Stream() because that fires before the closure executes).
		if mcpSrv != nil {
			defer mcpSrv.stop()
		}
		if mcpTmpCfg != "" {
			defer os.Remove(mcpTmpCfg)
		}

		const textID = "0"
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: textID}) {
			proc.kill()
			return
		}

		scanner := bufio.NewScanner(proc.stdout)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

		var finalUsage fantasy.Usage
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				proc.kill()
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()}) //nolint:errcheck
				return
			default:
			}

			// Strip ANSI/VT sequences that PTY drivers (especially Windows ConPTY)
			// inject into the output stream. JSON parsers need clean bytes.
			raw := scanner.Bytes()
			slog.Debug("cliprovider: raw line", "raw", string(raw))
			line := bytes.TrimSpace(ansiEscape.ReplaceAll(raw, nil))

			if m.spec.ParseUsageLine != nil {
				if u, ok := m.spec.ParseUsageLine(line); ok {
					finalUsage = u
				}
			}

			var part fantasy.StreamPart
			if parsePart != nil {
				var ok bool
				part, ok = parsePart(line)
				if !ok {
					continue
				}
			} else {
				clean := strings.TrimSpace(string(line))
				if clean == "" {
					continue
				}
				part = fantasy.StreamPart{
					Type:  fantasy.StreamPartTypeTextDelta,
					ID:    textID,
					Delta: clean + "\n",
				}
			}

			if !yield(part) {
				proc.kill()
				return
			}
		}

		if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
			// PTY master returns EIO (Unix) or similar when child exits.
			// Treat any scanner error in PTY mode as normal end-of-stream.
			if !proc.usingPTY {
				_, _ = proc.wait()
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err}) //nolint:errcheck
				return
			}
		}

		stderr, waitErr := proc.wait()
		if waitErr != nil {
			var exitErr error
			if stderr != "" {
				exitErr = fmt.Errorf("%s failed: %w\nstderr: %s", m.spec.Binary, waitErr, stderr)
			} else {
				exitErr = fmt.Errorf("%s failed: %w", m.spec.Binary, waitErr)
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: exitErr}) //nolint:errcheck
			return
		}

		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: textID}) //nolint:errcheck
		yield(fantasy.StreamPart{                                                  //nolint:errcheck
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        finalUsage,
		})
	}, nil
}

// formatPrompt converts a fantasy.Prompt into a single text string for the CLI.
// The full conversation (system prompt + message history) is formatted so the
// CLI model receives as much context as possible.
func formatPrompt(msgs fantasy.Prompt) string {
	var sb strings.Builder
	for _, msg := range msgs {
		text := extractText(msg)
		if text == "" {
			continue
		}
		switch msg.Role {
		case fantasy.MessageRoleSystem:
			sb.WriteString("<system>\n")
			sb.WriteString(text)
			sb.WriteString("\n</system>\n\n")
		case fantasy.MessageRoleUser:
			sb.WriteString("User: ")
			sb.WriteString(text)
			sb.WriteString("\n\n")
		case fantasy.MessageRoleAssistant:
			sb.WriteString("Assistant: ")
			sb.WriteString(text)
			sb.WriteString("\n\n")
		case fantasy.MessageRoleTool:
			sb.WriteString("Tool: ")
			sb.WriteString(text)
			sb.WriteString("\n\n")
		default:
			slog.Warn("cliprovider: unknown message role, skipping", "role", msg.Role)
		}
	}
	return strings.TrimSpace(sb.String())
}

// extractText collects all TextPart strings from a message's content.
// Non-text parts (tool calls, files, etc.) are silently skipped with a debug log.
func extractText(msg fantasy.Message) string {
	var sb strings.Builder
	for _, part := range msg.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			sb.WriteString(tp.Text)
		} else {
			slog.Debug("cliprovider: skipping non-text content part", "type", part.GetType(), "model_role", msg.Role)
		}
	}
	return sb.String()
}
