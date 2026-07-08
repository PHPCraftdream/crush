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
	"hash/fnv"
	"io"
	"log/slog"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"charm.land/fantasy"
	"charm.land/fantasy/object"
	gopty "github.com/aymanbagabas/go-pty"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/platform"
	"github.com/charmbracelet/crush/internal/session"
)

// sessionIDContextKey is a private key type so it cannot collide with other packages.
type sessionIDContextKey struct{}

// SessionIDContextKey is the context key for the session ID, set by the agent
// before calling Stream so the MCP todos tool knows which session to update.
var SessionIDContextKey = sessionIDContextKey{}

// reasoningEffortContextKey is a private key type for the reasoning effort value.
type reasoningEffortContextKey struct{}

// ReasoningEffortContextKey is the context key for the reasoning effort level
// (e.g. "low", "medium", "high", "max"), set by the agent before calling
// Stream so CLI models can inject the --effort flag dynamically.
var ReasoningEffortContextKey = reasoningEffortContextKey{}

// Fork patch: batch 14 — non-interactive context propagation.
//
// nonInteractiveContextKey marks the request as coming from `crush run` (a
// non-interactive entry point with no human at the keyboard). When set, the
// CLI sub-process is launched with its own bypass-permissions flag
// (claude --dangerously-skip-permissions, codex --approval-mode yolo,
// gemini --yolo) regardless of the runtime yoloFn. Otherwise the inner
// CLI would block waiting for an interactive permission prompt that
// nobody is there to answer, and `crush run` would hang silently.
type nonInteractiveContextKey struct{}

// NonInteractiveContextKey is set by app.RunNonInteractive on the agent
// context so cliprovider.Stream can force bypass-permissions for the inner
// CLI when there is provably no human to confirm.
var NonInteractiveContextKey = nonInteractiveContextKey{}

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
	// AlwaysStdin forces the prompt to be delivered via stdin instead of a
	// CLI flag, and disables PTY mode (using a regular pipe instead).
	// Use this for CLIs that detect TTY on stdout and switch to interactive
	// mode rather than emitting JSON, even when --output-format stream-json
	// is specified.
	AlwaysStdin bool
	// NoPTY skips PTY mode and always uses pipe-based I/O, while still
	// passing the prompt via PromptFlag (unlike AlwaysStdin which also
	// forces stdin delivery). Use this for wrapper binaries like npx.cmd
	// that don't relay child-process output through ConPTY on Windows.
	NoPTY bool
	// QwenMCPIntegration starts crush's MCP server and registers it in
	// ~/.qwen/settings.json under a stable per-project ID stored in
	// <workingDir>/.crush/qwen-mcp-id. The entry is removed when the CLI
	// process exits. The MCP server runs without Bearer-token auth because
	// qwen's settings format does not support custom HTTP headers.
	QwenMCPIntegration bool
	// GeminiMCPIntegration starts crush's MCP server and registers it in
	// ~/.gemini/settings.json under a stable per-project ID stored in
	// <workingDir>/.crush/gemini-mcp-id. The entry is removed when the CLI
	// process exits. Uses Authorization: Bearer header and trust:true to
	// bypass Gemini's own confirmation prompts.
	GeminiMCPIntegration bool
	// CodexMCPIntegration starts crush's MCP server and passes its URL to
	// codex via a -c flag (inline config override), so no persistent changes
	// are made to ~/.codex/config.toml.
	CodexMCPIntegration bool
	// SupportsResume enables --resume <session_id> for CLI models that
	// support it (Claude CLI). This lets the CLI reload its own conversation
	// history from its local DB, enabling API-level prompt caching across
	// multiple messages in the same crush session.
	SupportsResume bool
}

// streamEvent is the JSON envelope for Claude CLI stream-json output.
// Only the fields relevant to text extraction are parsed.
type streamEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype,omitempty"`    // "init" for system init events
	SessionID string `json:"session_id,omitempty"` // CLI session ID from init/stream events
	// stream_event: raw Anthropic API SSE event forwarded by claude CLI (--verbose).
	// content_block_delta events carry text tokens (text_delta) or thinking (thinking_delta).
	Event struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type     string `json:"type"`     // "text_delta" or "thinking_delta"
			Text     string `json:"text"`     // text_delta content
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
	// Claude CLI result event usage (snake_case).
	Usage struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// geminiCLIEvent is the JSONL envelope emitted by `gemini --output-format stream-json`.
//
// Actual format (verified against @google/gemini-cli v0.32+):
//
//	{"type":"init","session_id":"...","model":"..."}
//	{"type":"message","role":"user","content":"..."}
//	{"type":"message","role":"assistant","content":"<delta text>","delta":true}
//	{"type":"result","status":"success","stats":{"total_tokens":N,"input_tokens":N,"output_tokens":N}}
type geminiCLIEvent struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content string `json:"content"`
	Delta   bool   `json:"delta"`
	Status  string `json:"status"`
	Stats   struct {
		TotalTokens  int64 `json:"total_tokens"`
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"stats"`
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
//
// Gemini CLI (--output-format stream-json) emits JSONL events where assistant
// text arrives as:
//
//	{"type":"message","role":"assistant","content":"<delta>","delta":true}
//
// Each event carries an incremental text delta.  Non-assistant events
// (init, user message echo, result) are silently skipped.
func geminiPartParser() func([]byte) (fantasy.StreamPart, bool) {
	const id = "0"
	return func(line []byte) (fantasy.StreamPart, bool) {
		var ev geminiCLIEvent
		if json.Unmarshal(line, &ev) != nil {
			return fantasy.StreamPart{}, false
		}
		if ev.Type == "message" && ev.Role == "assistant" && ev.Content != "" {
			return fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: ev.Content}, true
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
	// Log cache statistics for visibility.
	if inputTotal > 0 {
		cacheHitPct := float64(0)
		if inputTotal > 0 {
			cacheHitPct = float64(ev.Usage.CacheReadInputTokens) / float64(inputTotal) * 100
		}
		slog.Info(
			"cliprovider: token usage",
			"input", ev.Usage.InputTokens,
			"cache_create", ev.Usage.CacheCreationInputTokens,
			"cache_read", ev.Usage.CacheReadInputTokens,
			"output", ev.Usage.OutputTokens,
			"cache_hit_pct", fmt.Sprintf("%.1f%%", cacheHitPct),
		)
	}
	return fantasy.Usage{
		InputTokens:  inputTotal,
		OutputTokens: ev.Usage.OutputTokens,
		TotalTokens:  inputTotal + ev.Usage.OutputTokens,
	}, true
}

// geminiParseUsageLine extracts token usage from the Gemini CLI result event.
//
// The Gemini CLI emits a final event:
//
//	{"type":"result","status":"success","stats":{"total_tokens":N,"input_tokens":N,"output_tokens":N}}
func geminiParseUsageLine(line []byte) (fantasy.Usage, bool) {
	var ev geminiCLIEvent
	if json.Unmarshal(line, &ev) != nil {
		return fantasy.Usage{}, false
	}
	if ev.Type != "result" || ev.Stats.TotalTokens == 0 {
		return fantasy.Usage{}, false
	}
	return fantasy.Usage{
		InputTokens:  ev.Stats.InputTokens,
		OutputTokens: ev.Stats.OutputTokens,
		TotalTokens:  ev.Stats.TotalTokens,
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

// npxClaudeArgs was used for the cli-npx-claude-* family. Removed in the
// 2026-06-17 cleanup — the variants doubled the model list with no real
// benefit (anyone with `claude` on PATH gets identical behaviour faster,
// and Windows ConPTY relay through npx.cmd was unreliable anyway).

// codexEvent is the top-level JSONL envelope emitted by `codex exec --json`.
type codexEvent struct {
	Type string `json:"type"`
	// item.started / item.completed
	Item struct {
		Type             string `json:"type"`              // "agent_message" | "command_execution" | "reasoning" | ...
		Text             string `json:"text"`              // agent_message: full response text
		Command          string `json:"command"`           // command_execution: command string
		AggregatedOutput string `json:"aggregated_output"` // command_execution: combined stdout+stderr
	} `json:"item"`
	// turn.completed usage
	Usage struct {
		InputTokens       int64 `json:"input_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
	} `json:"usage"`
}

// codexPartParser returns a stateful parser for `codex exec --json` JSONL output.
// Text is NOT streamed token-by-token; the full response arrives in a single
// item.completed event with type "agent_message". We emit it as one TextDelta.
func codexPartParser() func([]byte) (fantasy.StreamPart, bool) {
	const id = "0"
	return func(line []byte) (fantasy.StreamPart, bool) {
		var ev codexEvent
		if json.Unmarshal(line, &ev) != nil {
			return fantasy.StreamPart{}, false
		}
		if ev.Type == "item.completed" && ev.Item.Type == "agent_message" && ev.Item.Text != "" {
			return fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: ev.Item.Text}, true
		}
		return fantasy.StreamPart{}, false
	}
}

// codexParseUsageLine extracts token usage from a Codex `turn.completed` event.
func codexParseUsageLine(line []byte) (fantasy.Usage, bool) {
	var ev codexEvent
	if json.Unmarshal(line, &ev) != nil {
		return fantasy.Usage{}, false
	}
	if ev.Type != "turn.completed" {
		return fantasy.Usage{}, false
	}
	inputTotal := ev.Usage.InputTokens + ev.Usage.CachedInputTokens
	if inputTotal == 0 && ev.Usage.OutputTokens == 0 {
		return fantasy.Usage{}, false
	}
	return fantasy.Usage{
		InputTokens:  inputTotal,
		OutputTokens: ev.Usage.OutputTokens,
		TotalTokens:  inputTotal + ev.Usage.OutputTokens,
	}, true
}

// codexArgs returns a BuildArgs func for a codex CLI model.
func codexArgs(model string) func(bool) []string {
	return func(yolo bool) []string {
		args := []string{"exec", "--json", "-m", model}
		if yolo {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		}
		return args
	}
}

// qwenArgs returns a BuildArgs func for the qwen CLI model.
func qwenArgs() func(bool) []string {
	return func(yolo bool) []string {
		args := []string{
			"--output-format", "stream-json",
			"--include-partial-messages",
		}
		if yolo {
			args = append(args, "--approval-mode", "yolo")
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

// claudeSpec builds a CLI-Claude entry. The model argument is what gets
// passed to `claude --model <X>` — either an alias ("sonnet", "haiku",
// "opus", "fable", which the CLI resolves to its current default) or a
// pinned model id ("claude-opus-4-7", "claude-opus-4-8", …). We no longer
// generate per-effort -thinking variants: the UI's reasoning-effort
// selector (low/medium/high/xhigh/max) is forwarded via context at call
// time and rewrites the `--effort` flag in BuildArgs.
func claudeSpec(modelID, modelName, modelArg string, ctxWindow int64) CLISpec {
	return CLISpec{
		ModelID:        modelID,
		ModelName:      modelName,
		ContextWindow:  ctxWindow,
		Binary:         "claude",
		PromptFlag:     "-p",
		BuildArgs:      claudeArgs(modelArg),
		NewPartParser:  claudePartParser,
		ParseUsageLine: claudeParseUsageLine,
		UseCrushMCP:    true,
		SupportsResume: true,
	}
}

// All is the list of hardcoded CLI model specs.
// Add new entries here to register additional CLI-backed models.
var All = []CLISpec{
	// Anthropic's `claude` CLI on PATH. One entry per model family;
	// pinned Opus versions because the operator usually wants a specific
	// generation (4.6/4.7/4.8 differ meaningfully). Sonnet / Haiku /
	// Fable use the aliases so each tab auto-tracks the latest.
	claudeSpec("cli-claude-haiku", "Claude Haiku 4.5 (CLI)", "haiku", 200_000),
	claudeSpec("cli-claude-sonnet", "Claude Sonnet 4.6 (CLI)", "sonnet", 1_000_000),
	// Opus: alias entry kept so DB rows / atoms (`opus`) referencing the
	// classic ModelID don't dangle, plus three pinned variants the operator
	// can pick explicitly.
	claudeSpec("cli-claude-opus", "Claude Opus (CLI, latest)", "opus", 1_000_000),
	claudeSpec("cli-claude-opus-4-6", "Claude Opus 4.6 (CLI)", "claude-opus-4-6", 1_000_000),
	claudeSpec("cli-claude-opus-4-7", "Claude Opus 4.7 (CLI)", "claude-opus-4-7", 1_000_000),
	claudeSpec("cli-claude-opus-4-8", "Claude Opus 4.8 (CLI)", "claude-opus-4-8", 1_000_000),
	claudeSpec("cli-claude-fable", "Claude Fable 5 (CLI)", "fable", 1_000_000),
	{
		ModelID:              "cli-gemini-flash",
		ModelName:            "Gemini 3 Flash (CLI)",
		ContextWindow:        1_000_000,
		Binary:               "gemini",
		BuildArgs:            geminiArgs("gemini-3-flash"),
		NewPartParser:        geminiPartParser,
		ParseUsageLine:       geminiParseUsageLine,
		AlwaysStdin:          true,
		GeminiMCPIntegration: true,
	},
	{
		ModelID:              "cli-gemini-pro",
		ModelName:            "Gemini 3.1 Pro (CLI)",
		ContextWindow:        1_000_000,
		Binary:               "gemini",
		BuildArgs:            geminiArgs("gemini-3.1-pro-preview"),
		NewPartParser:        geminiPartParser,
		ParseUsageLine:       geminiParseUsageLine,
		AlwaysStdin:          true,
		GeminiMCPIntegration: true,
	},
	{
		ModelID:            "cli-qwen",
		ModelName:          "Qwen 3.5 Plus (CLI)",
		ContextWindow:      1_000_000,
		Binary:             "qwen",
		BuildArgs:          qwenArgs(),
		NewPartParser:      claudePartParser,
		ParseUsageLine:     claudeParseUsageLine,
		AlwaysStdin:        true,
		QwenMCPIntegration: true,
	},
	{
		ModelID:             "cli-codex",
		ModelName:           "Codex (gpt-5.3-codex, CLI)",
		ContextWindow:       400_000,
		Binary:              "codex",
		BuildArgs:           codexArgs("gpt-5.3-codex"),
		NewPartParser:       codexPartParser,
		ParseUsageLine:      codexParseUsageLine,
		AlwaysStdin:         true,
		CodexMCPIntegration: true,
	},
	{
		ModelID:             "cli-codex-gpt-5-4",
		ModelName:           "Codex (gpt-5.4, CLI)",
		ContextWindow:       272_000,
		Binary:              "codex",
		BuildArgs:           codexArgs("gpt-5.4"),
		NewPartParser:       codexPartParser,
		ParseUsageLine:      codexParseUsageLine,
		AlwaysStdin:         true,
		CodexMCPIntegration: true,
	},
	{
		ModelID:             "cli-codex-gpt-5-2",
		ModelName:           "Codex (gpt-5.2-codex, CLI)",
		ContextWindow:       400_000,
		Binary:              "codex",
		BuildArgs:           codexArgs("gpt-5.2-codex"),
		NewPartParser:       codexPartParser,
		ParseUsageLine:      codexParseUsageLine,
		AlwaysStdin:         true,
		CodexMCPIntegration: true,
	},
	{
		ModelID:             "cli-codex-max",
		ModelName:           "Codex Max (gpt-5.1-codex-max, CLI)",
		ContextWindow:       400_000,
		Binary:              "codex",
		BuildArgs:           codexArgs("gpt-5.1-codex-max"),
		NewPartParser:       codexPartParser,
		ParseUsageLine:      codexParseUsageLine,
		AlwaysStdin:         true,
		CodexMCPIntegration: true,
	},
	{
		ModelID:             "cli-codex-gpt-5-2-base",
		ModelName:           "Codex (gpt-5.2, CLI)",
		ContextWindow:       400_000,
		Binary:              "codex",
		BuildArgs:           codexArgs("gpt-5.2"),
		NewPartParser:       codexPartParser,
		ParseUsageLine:      codexParseUsageLine,
		AlwaysStdin:         true,
		CodexMCPIntegration: true,
	},
	{
		ModelID:             "cli-codex-mini",
		ModelName:           "Codex Mini (gpt-5.1-codex-mini, CLI)",
		ContextWindow:       400_000,
		Binary:              "codex",
		BuildArgs:           codexArgs("gpt-5.1-codex-mini"),
		NewPartParser:       codexPartParser,
		ParseUsageLine:      codexParseUsageLine,
		AlwaysStdin:         true,
		CodexMCPIntegration: true,
	},
}

// Available returns the subset of All whose Binary is found in PATH.
// AvailableFunc returns the locally-installed CLI specs. It is a package
// var so tests can stub CLI detection deterministically — otherwise the set
// depends on whatever binaries (claude, gemini, npx, …) happen to be on the
// runner's PATH, which makes provider-count assertions environment-dependent.
var AvailableFunc = detectAvailable

// testDisablePTY, when true, forces pipe mode regardless of spec. The
// cliprovider test suite sets it on Windows, where go-pty's ConPTY path has
// an internal data race the -race detector flags. Always false in production.
var testDisablePTY bool

// Available reports which CLI model specs are usable on this machine.
func Available() []CLISpec { return AvailableFunc() }

func detectAvailable() []CLISpec {
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

// cliSessionEntry stores a CLI session ID along with a hash of the conversation
// prefix (all messages except the last), so we can detect edits/deletes that
// would make the CLI session's history stale.
type cliSessionEntry struct {
	CLISessionID string
	PrefixHash   uint64
}

type cliProvider struct {
	workingDir  string
	yoloFn      func() bool
	perms       permission.Service
	sessions    session.Service
	mcpProxy    ExternalMCPProxy
	specs       map[string]CLISpec
	cliSessions *csync.Map[string, cliSessionEntry] // crush session key → CLI session entry
}

// ExternalMCPTool describes an external MCP tool to expose through the crush MCP bridge.
type ExternalMCPTool struct {
	ServerName  string
	Name        string
	Description string
	InputSchema any // JSON schema
}

// ExternalMCPProxy provides access to external MCP tools and the ability
// to call them. Implemented by the coordinator to avoid circular imports.
type ExternalMCPProxy interface {
	// ListTools returns all enabled external MCP tools.
	ListTools() []ExternalMCPTool
	// CallTool invokes a tool on the named MCP server and returns the text result.
	CallTool(ctx context.Context, serverName, toolName, inputJSON string) (string, error)
}

// New creates a CLI provider that runs all specs from [All].
// workingDir is set as the working directory for every CLI invocation.
// yoloFn is called at request time to decide whether to pass the auto-accept flag.
// perms is used to show crush's permission dialog when UseCrushMCP specs are invoked.
// sessions is used by the todos MCP tool to persist task lists.
// mcpProxy, if non-nil, is used for proxying external MCP tools to CLI models.
func New(workingDir string, yoloFn func() bool, perms permission.Service, sessions session.Service, mcpProxy ExternalMCPProxy) fantasy.Provider {
	specs := make(map[string]CLISpec, len(All))
	for _, s := range All {
		specs[s.ModelID] = s
	}
	return &cliProvider{
		workingDir:  workingDir,
		yoloFn:      yoloFn,
		perms:       perms,
		sessions:    sessions,
		mcpProxy:    mcpProxy,
		specs:       specs,
		cliSessions: csync.NewMap[string, cliSessionEntry](),
	}
}

func (p *cliProvider) Name() string { return ProviderID }

func (p *cliProvider) LanguageModel(_ context.Context, modelID string) (fantasy.LanguageModel, error) {
	spec, ok := p.specs[modelID]
	if !ok {
		return nil, fmt.Errorf("unknown CLI model: %q", modelID)
	}
	return &cliModel{spec: spec, workingDir: p.workingDir, yoloFn: p.yoloFn, perms: p.perms, sessions: p.sessions, mcpProxy: p.mcpProxy, cliSessions: p.cliSessions}, nil
}

type cliModel struct {
	spec        CLISpec
	mcpProxy    ExternalMCPProxy
	cliSessions *csync.Map[string, cliSessionEntry]
	workingDir  string
	yoloFn      func() bool
	perms       permission.Service
	sessions    session.Service
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
	// Fork patch: batch 14 — `crush run` has no human at the keyboard, so any
	// inner CLI process MUST get bypass-permissions or it will hang on the
	// interactive permission prompt. RunNonInteractive sets this context key.
	if !yolo {
		if v, ok := ctx.Value(NonInteractiveContextKey).(bool); ok && v {
			yolo = true
		}
	}

	// Save any attached files (images, etc.) to temp dir so the CLI agent
	// can access them via its file-reading tools.
	attachTmpDir, filePaths, fileErr := saveFileParts(call.Prompt)
	if fileErr != nil {
		slog.Warn("cliprovider: failed to save attachments", "err", fileErr)
	}
	if len(filePaths) > 0 {
		slog.Info("cliprovider: saved attachments to temp dir", "dir", attachTmpDir, "count", len(filePaths))
	}

	prompt := formatPrompt(call.Prompt, filePaths)

	// Will be overridden below if resuming (only the new user message is needed).
	var resumePrompt string

	args := m.spec.BuildArgs(yolo)

	// Apply dynamic reasoning effort from context, replacing any hardcoded
	// --effort value from BuildArgs so the UI toggle actually takes effect.
	if effort, ok := ctx.Value(ReasoningEffortContextKey).(string); ok && effort != "" {
		replaced := false
		for i, a := range args {
			if a == "--effort" && i+1 < len(args) {
				args[i+1] = effort
				replaced = true
				break
			}
		}
		if !replaced {
			args = append(args, "--effort", effort)
		}
	}

	// Extract session ID from context (set by agent.go before calling Stream).
	sessionID, _ := ctx.Value(SessionIDContextKey).(string)

	// Resume a previous CLI session if available, to leverage API prompt caching.
	// The key includes the model ID so switching models starts a fresh session.
	// We also hash the conversation prefix (all messages except the last user
	// message) and only resume if the hash matches — this detects edits/deletes
	// that would make the CLI session's history stale.
	var resuming bool
	var cliSessionKey string
	var prefixHash uint64
	if m.spec.SupportsResume && sessionID != "" {
		cliSessionKey = sessionID + ":" + m.spec.ModelID
		prefixHash = hashPromptPrefix(call.Prompt)
		if entry, ok := m.cliSessions.Get(cliSessionKey); ok {
			if entry.PrefixHash == prefixHash {
				args = append(args, "--resume", entry.CLISessionID)
				resuming = true
				resumePrompt = extractLatestUserMessage(call.Prompt, filePaths)
				slog.Info("cliprovider: resuming CLI session", "crushSession", sessionID, "cliSession", entry.CLISessionID, "resumePromptLen", len(resumePrompt))
			} else {
				// History was edited/deleted — start fresh CLI session.
				m.cliSessions.Del(cliSessionKey)
				slog.Info("cliprovider: conversation prefix changed, starting fresh CLI session", "crushSession", sessionID)
			}
		}
	}
	if resuming {
		prompt = resumePrompt
	}

	// When running in non-yolo mode with a spec that opts into crush's MCP
	// server, start an in-process MCP server and pass its config to the CLI
	// so tool calls go through crush's permission dialog instead of the CLI's
	// own (invisible) permission prompts.
	// mcpSrv and mcpTmpCfg are cleaned up inside the returned closure, not
	// via defer here — defer in Stream() would fire when Stream() returns
	// (before the closure runs), deleting the config file before claude CLI
	// can read it.
	var mcpSrv *crushMCPServer
	var mcpTmpCfg string     // path to temp MCP config file (claude-style); "" if not used
	var qwenMCPName string   // registered name in ~/.qwen/settings.json; "" if not used
	var geminiMCPName string // registered name in ~/.gemini/settings.json; "" if not used
	// Fork patch: batch 20 — keep the MCP bridge active even in yolo/bypass mode.
	// Before this fix, `!yolo` here meant that `crush run` (which sets yolo=true via
	// NonInteractiveContextKey in batch 14) would skip MCP setup entirely. Inner
	// claude then ran with no --allowedTools and could reach for its native Bash /
	// Write / Task tools, bypassing agentguard (batch 16) and the MCP permission
	// dialog. yolo only controls whether claude needs the bypass-permissions flag,
	// not whether our MCP bridge sits in the loop.
	if m.spec.UseCrushMCP && m.perms != nil {
		var err error
		mcpSrv, err = newCrushMCPServer(ctx, m.perms, m.sessions, sessionID, m.workingDir, "", m.mcpProxy)
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
	// We also explicitly disallow TodoWrite so the model uses mcp__crush__todos
	// (which persists tasks to the crush session) instead of the CLI-native
	// TodoWrite tool that writes to a local file unknown to the crush UI.
	if mcpSrv != nil {
		allowed := []string{
			// Crush MCP bridge tools (go through crush's permission system).
			"mcp__crush__Bash",
			"mcp__crush__Read",
			"mcp__crush__Write",
			"mcp__crush__Glob",
			"mcp__crush__Grep",
			"mcp__crush__todos",
			// CLI built-in tools that crush doesn't replicate.
			// These are safe read-only or internal tools that don't need
			// crush's permission system.
			"WebSearch",
			"WebFetch",
			"Task",
			"Agent",
		}
		// Include external MCP tools registered on the crush MCP bridge.
		if m.mcpProxy != nil {
			for _, ext := range m.mcpProxy.ListTools() {
				allowed = append(allowed, "mcp__crush__"+ext.ServerName+"__"+ext.Name)
			}
		}
		args = append(
			args,
			"--allowedTools",
			strings.Join(allowed, ","),
			"--disallowedTools",
			"TodoWrite",
		)
	}

	// Qwen MCP integration: register crush's MCP server in ~/.qwen/settings.json
	// using a stable per-project ID stored in <workingDir>/.crush/qwen-mcp-id.
	// Qwen doesn't support --mcp-config, so we write the settings directly.
	// No Bearer token is used (qwen's format doesn't support custom headers);
	// the server is localhost-only with a random port.
	if m.spec.QwenMCPIntegration && m.perms != nil {
		id, idErr := qwenMCPID(m.workingDir)
		if idErr != nil {
			slog.Warn("cliprovider: failed to get qwen MCP ID", "err", idErr)
		} else {
			// Use the stable project ID as the token — it's unique per project and
			// already stored in .crush/qwen-mcp-id, so no separate secret is needed.
			var err error
			mcpSrv, err = newCrushMCPServer(ctx, m.perms, m.sessions, sessionID, m.workingDir, id, m.mcpProxy)
			if err != nil {
				slog.Warn("cliprovider: failed to start qwen MCP server", "err", err)
			} else {
				// Remove stale entry first, then register with current URL+token.
				deregisterQwenMCP(id)
				if regErr := registerQwenMCP(id, mcpSrv.mcpURL()); regErr != nil {
					slog.Warn("cliprovider: failed to register qwen MCP server", "err", regErr)
					mcpSrv.stop()
					mcpSrv = nil
				} else {
					qwenMCPName = id
					args = append(args, "--allowed-mcp-server-names", id)
					// Restrict qwen to only crush MCP tools so its built-in
					// tools (read_file, glob, etc.) cannot bypass crush's
					// permission system.
					// Also block the native todo_write so the model uses
					// mcp__crush__todos which persists tasks to the crush session.
					args = append(
						args,
						"--allowed-tools",
						"mcp__"+id+"__Bash",
						"mcp__"+id+"__Read",
						"mcp__"+id+"__Write",
						"mcp__"+id+"__Glob",
						"mcp__"+id+"__Grep",
						"mcp__"+id+"__todos",
						"--exclude-tools",
						"todo_write",
					)
				}
			}
		}
	}

	// Gemini MCP integration: register crush's MCP server in ~/.gemini/settings.json
	// using a stable per-project ID. Gemini supports Authorization: Bearer headers and
	// a trust:true flag to bypass its own confirmation prompts, so tool calls go
	// directly to our MCP server which shows crush's permission dialog.
	if m.spec.GeminiMCPIntegration && m.perms != nil {
		id, idErr := geminiMCPID(m.workingDir)
		if idErr != nil {
			slog.Warn("cliprovider: failed to get gemini MCP ID", "err", idErr)
		} else {
			var err error
			mcpSrv, err = newCrushMCPServer(ctx, m.perms, m.sessions, sessionID, m.workingDir, "", m.mcpProxy)
			if err != nil {
				slog.Warn("cliprovider: failed to start gemini MCP server", "err", err)
			} else {
				deregisterGeminiMCP(id)
				if regErr := registerGeminiMCP(id, mcpSrv.addr, mcpSrv.token); regErr != nil {
					slog.Warn("cliprovider: failed to register gemini MCP server", "err", regErr)
					mcpSrv.stop()
					mcpSrv = nil
				} else {
					geminiMCPName = id
					args = append(args, "--allowed-mcp-server-names", id)
					slog.Info("cliprovider: gemini MCP registered", "name", id, "addr", mcpSrv.addr)
				}
			}
		}
	}

	// Codex MCP integration: pass crush's MCP server URL to codex via -c flag
	// (inline config override). No persistent changes to ~/.codex/config.toml.
	// The token is embedded in the URL as a query parameter so the server can
	// authenticate requests without needing env-var injection.
	if m.spec.CodexMCPIntegration && m.perms != nil {
		var err error
		mcpSrv, err = newCrushMCPServer(ctx, m.perms, m.sessions, sessionID, m.workingDir, "", m.mcpProxy)
		if err != nil {
			slog.Warn("cliprovider: failed to start codex MCP server", "err", err)
		} else {
			args = append(args, "-c", fmt.Sprintf("mcp_servers.crush.url=%q", mcpSrv.mcpURL()))
			slog.Info("cliprovider: codex MCP configured", "addr", mcpSrv.addr)
		}
	}

	noPTY := m.spec.NoPTY || testDisablePTY
	useStdin := m.spec.AlwaysStdin || noPTY || len(prompt) > maxPromptArgLen
	if !useStdin && m.spec.PromptFlag != "" {
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

	if !useStdin && !noPTY {
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
				// Fork patch (operator UX, debug): log the full command-line
				// + prompt length + first/last 200 chars of the prompt so
				// silent-claude-exit cases ("claude died in 68ms with empty
				// stderr") can be reproduced post-mortem from the log.
				slog.Info(
					"cliprovider: using PTY",
					"binary", binaryPath,
					"args", strings.Join(args, " "),
					"argsCount", len(args),
					"argsByteLen", argsByteLen(args),
					"promptLen", len(prompt),
					"promptHead", clipString(prompt, 200),
					"promptTail", tailString(prompt, 200),
				)
				waitCh := make(chan error, 1)
				go func() {
					waitCh <- ptycmd.Wait()
					_ = p.Close() // EOF for scanner (required on Windows ConPTY)
				}()
				var ptyKillOnce sync.Once
				proc = procHandle{
					stdout:   p,
					usingPTY: true,
					kill: func() {
						// Use sync.Once so this is safe to call from multiple goroutines
						// (context-cancel watcher + scanner loop) without double-draining waitCh.
						ptyKillOnce.Do(func() {
							if ptycmd.Process != nil {
								_ = session.KillProcess(ptycmd.Process.Pid)
							}
						})
					},
					wait: func() (string, error) {
						// Bound the wait against ctx.Done() for parity with the
						// pipe branch: a grandchild holding the PTY's underlying
						// handles can keep ptycmd.Wait() from returning even after
						// the direct child exits. PTY mode merges stderr into the
						// tty, so there is no stderrBuf to race on here.
						select {
						case waitErr := <-waitCh:
							return "", waitErr
						case <-ctx.Done():
							slog.Warn("cliprovider: PTY wait aborted on ctx cancellation", "binary", m.spec.Binary)
							return "", ctx.Err()
						}
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
		platform.HideConsoleWindow(cmd)
		if useStdin {
			cmd.Stdin = strings.NewReader(prompt)
		}
		slog.Info(
			"cliprovider: launching pipe mode",
			"binary", m.spec.Binary,
			"args", strings.Join(args, " "),
			"argsCount", len(args),
			"argsByteLen", argsByteLen(args),
			"useStdin", useStdin,
			"promptLen", len(prompt),
			"promptHead", clipString(prompt, 200),
			"promptTail", tailString(prompt, 200),
			"noPTY", m.spec.NoPTY,
		)

		// For NoPTY models (e.g. npx wrappers), merge stdout and stderr
		// into a single reader via io.Pipe + concurrent copy goroutines.
		// Claude CLI may send JSON to stderr when the prompt is delivered
		// via stdin instead of -p flag. We use cmd.StdoutPipe/StderrPipe
		// (not os.Pipe) because Go's exec package handles Windows handle
		// inheritance correctly for those.
		var reader io.Reader
		var stderrBuf bytes.Buffer
		if m.spec.NoPTY {
			stdoutPipe, pipeErr := cmd.StdoutPipe()
			if pipeErr != nil {
				return nil, fmt.Errorf("stdout pipe: %w", pipeErr)
			}
			stderrPipe, pipeErr := cmd.StderrPipe()
			if pipeErr != nil {
				return nil, fmt.Errorf("stderr pipe: %w", pipeErr)
			}
			if startErr := cmd.Start(); startErr != nil {
				return nil, fmt.Errorf("start %s: %w", m.spec.Binary, startErr)
			}
			pr, pw := io.Pipe()
			var copyWg sync.WaitGroup
			copyWg.Add(2)
			go func() { _, _ = io.Copy(pw, stdoutPipe); copyWg.Done() }()
			go func() { _, _ = io.Copy(pw, stderrPipe); copyWg.Done() }()
			go func() { copyWg.Wait(); pw.Close() }()
			reader = pr
		} else {
			cmd.Stderr = &stderrBuf
			pipe, pipeErr := cmd.StdoutPipe()
			if pipeErr != nil {
				return nil, fmt.Errorf("stdout pipe: %w", pipeErr)
			}
			if startErr := cmd.Start(); startErr != nil {
				return nil, fmt.Errorf("start %s: %w", m.spec.Binary, startErr)
			}
			reader = pipe
		}
		slog.Info("cliprovider: process started", "binary", m.spec.Binary, "pid", cmd.Process.Pid)
		waitCh := make(chan error, 1)
		go func() { waitCh <- cmd.Wait() }()
		proc = procHandle{
			stdout:   reader,
			usingPTY: false,
			kill: func() {
				if cmd.Process != nil {
					_ = session.KillProcess(cmd.Process.Pid)
				}
			},
			wait: func() (string, error) {
				// cmd.Wait() blocks until EVERY process holding the stderr
				// write handle exits, not just the direct child — and the
				// external CLI binaries this launches routinely spawn
				// grandchildren (claude.cmd → cmd.exe → node.exe, MCP
				// servers) that inherit stderr. If the direct child dies
				// (stdout EOFs, scanner loop ends) but a grandchild is still
				// alive holding stderr, an unbounded cmd.Wait() would block
				// forever with no ctx check on this path. Bound it: run
				// Wait in its own goroutine and select against ctx.Done().
				select {
				case waitErr := <-waitCh:
					// Only safe to read stderrBuf after Wait truly completed;
					// os/exec's stderr-copy goroutine is joined by Wait.
					stderr := strings.TrimSpace(stderrBuf.String())
					if stderr != "" {
						slog.Warn("cliprovider: process stderr", "binary", m.spec.Binary, "stderr", stderr)
					}
					return stderr, waitErr
				case <-ctx.Done():
					// ctx cancelled before Wait returned — stderrBuf may
					// still be written to concurrently by the not-yet-joined
					// stderr-copy goroutine, so do NOT read it (would race).
					// The stderr-holding grandchild is the very thing ctx
					// cancellation (via kill()) is trying to tear down.
					slog.Warn("cliprovider: wait aborted on ctx cancellation; stderr discarded (may be incomplete)", "binary", m.spec.Binary)
					return "", ctx.Err()
				}
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
		if attachTmpDir != "" {
			defer os.RemoveAll(attachTmpDir)
		}
		if qwenMCPName != "" {
			defer deregisterQwenMCP(qwenMCPName)
		}
		if geminiMCPName != "" {
			defer deregisterGeminiMCP(geminiMCPName)
		}

		// Kill the subprocess immediately when ctx is cancelled, even while
		// scanner.Scan() is blocking between CLI output lines (e.g. during a
		// long MCP tool call). Without this goroutine the cancellation would
		// only be observed at the next ctx.Done() check inside the scan loop,
		// which requires a new line to arrive first.
		killDone := make(chan struct{})
		defer close(killDone)
		go func() {
			select {
			case <-ctx.Done():
				proc.kill()
			case <-killDone:
			}
		}()

		const textID = "0"
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: textID}) {
			proc.kill()
			return
		}

		// scanResult carries one scanner event: a raw line, or a terminal
		// signal with any scanner error.
		type scanResult struct {
			raw  []byte
			done bool
			err  error
		}
		scanCh := make(chan scanResult, 64)
		go func() {
			scanner := bufio.NewScanner(proc.stdout)
			scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
			for scanner.Scan() {
				b := scanner.Bytes()
				cp := make([]byte, len(b))
				copy(cp, b)
				select {
				case scanCh <- scanResult{raw: cp}:
				case <-ctx.Done():
					return
				}
			}
			scanCh <- scanResult{done: true, err: scanner.Err()}
		}()

		// toolCh is the read side of the MCP tool-event channel.
		// When nil (no MCP server), selecting on it never fires.
		var toolCh <-chan mcpToolEvent
		if mcpSrv != nil {
			toolCh = mcpSrv.toolCh
		}

		var finalUsage fantasy.Usage
		scanDone := false
		var scanErr error
		var linesSeen int
		for !scanDone {
			select {
			case <-ctx.Done():
				proc.kill()
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()}) //nolint:errcheck
				return

			case ev := <-toolCh:
				// Emit ToolInputStart + Delta + End from the MCP tool event.
				id := ev.id
				if ev.name != "" {
					// start event
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: ev.name}) {
						proc.kill()
						return
					}
					if ev.input != "" {
						if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: ev.input}) {
							proc.kill()
							return
						}
					}
				} else {
					// end event
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: id}) {
						proc.kill()
						return
					}
				}

			case res := <-scanCh:
				if res.done {
					scanDone = true
					scanErr = res.err
					break
				}
				linesSeen++

				// Strip ANSI/VT sequences that PTY drivers (especially Windows ConPTY)
				// inject into the output stream. JSON parsers need clean bytes.
				raw := res.raw
				slog.Debug("cliprovider: raw line", "raw", string(raw))
				line := bytes.TrimSpace(ansiEscape.ReplaceAll(raw, nil))

				// Capture CLI session ID from the system init event for --resume.
				if m.spec.SupportsResume && cliSessionKey != "" {
					var initEv streamEvent
					if json.Unmarshal(line, &initEv) == nil && initEv.Type == "system" && initEv.Subtype == "init" && initEv.SessionID != "" {
						m.cliSessions.Set(cliSessionKey, cliSessionEntry{CLISessionID: initEv.SessionID, PrefixHash: prefixHash})
						slog.Info("cliprovider: captured CLI session ID", "key", cliSessionKey, "cliSession", initEv.SessionID)
					}
				}

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
		}

		if scanErr != nil && !errors.Is(scanErr, io.EOF) {
			// PTY master returns EIO (Unix) or similar when child exits.
			// Treat any scanner error in PTY mode as normal end-of-stream.
			if !proc.usingPTY {
				_, _ = proc.wait()
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: scanErr}) //nolint:errcheck
				return
			}
		}

		stderr, waitErr := proc.wait()
		// Fork patch (operator UX): also log stderr content (clipped) and
		// total lines seen on stdout. The original log said "err=null
		// stderrLen=0" for the silent-claude-exit bug which gave the
		// operator nothing actionable. Now they see if anything was
		// emitted at all and what the stderr tail looked like.
		slog.Info(
			"cliprovider: process finished",
			"binary", m.spec.Binary,
			"err", waitErr,
			"stderrLen", len(stderr),
			"stderrTail", tailString(stderr, 500),
			"linesSeen", linesSeen,
		)
		// If resume failed, clear the stale CLI session mapping so next call starts fresh.
		if waitErr != nil && resuming && cliSessionKey != "" {
			m.cliSessions.Del(cliSessionKey)
			slog.Warn("cliprovider: resume failed, cleared CLI session mapping", "key", cliSessionKey)
		}
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

// saveFileParts extracts FilePart entries from messages, writes them to a temp
// directory on disk, and returns the directory path and a per-message list of
// saved file paths. The caller must os.RemoveAll(tempDir) when done.
// Returns ("", nil, nil) if no file parts are found.
func saveFileParts(msgs fantasy.Prompt) (tempDir string, filePaths map[int][]string, err error) {
	// Collect file parts with their message indices.
	type entry struct {
		msgIdx int
		fp     fantasy.FilePart
	}
	var entries []entry
	for i, msg := range msgs {
		for _, part := range msg.Content {
			if fp, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
				slog.Debug("cliprovider: found FilePart", "msgIdx", i, "filename", fp.Filename, "mediaType", fp.MediaType, "dataLen", len(fp.Data))
				entries = append(entries, entry{msgIdx: i, fp: fp})
			}
		}
	}
	slog.Debug("cliprovider: saveFileParts scan", "totalMessages", len(msgs), "filePartsFound", len(entries))
	if len(entries) == 0 {
		return "", nil, nil
	}

	tempDir, err = os.MkdirTemp("", "crush-attachments-*")
	if err != nil {
		return "", nil, fmt.Errorf("create attachment temp dir: %w", err)
	}

	filePaths = make(map[int][]string)
	for seq, e := range entries {
		name := e.fp.Filename
		if name == "" {
			ext := ".bin"
			if exts, _ := mime.ExtensionsByType(e.fp.MediaType); len(exts) > 0 {
				ext = exts[0]
			}
			name = fmt.Sprintf("attachment-%d%s", seq, ext)
		}
		// Sanitize: keep only the base name.
		name = filepath.Base(name)
		path := filepath.Join(tempDir, name)
		if werr := os.WriteFile(path, e.fp.Data, 0o644); werr != nil {
			slog.Warn("cliprovider: failed to write attachment", "path", path, "err", werr)
			continue
		}
		filePaths[e.msgIdx] = append(filePaths[e.msgIdx], path)
	}
	return tempDir, filePaths, nil
}

// formatPrompt converts a fantasy.Prompt into a single text string for the CLI.
// The full conversation (system prompt + message history) is formatted so the
// CLI model receives as much context as possible.
// filePaths maps message indices to on-disk file paths for attached files;
// nil means no files were attached.
// hashPromptPrefix returns a hash of all messages except the last user message.
// Used to detect conversation edits/deletes that would make a CLI session stale.
func hashPromptPrefix(msgs fantasy.Prompt) uint64 {
	h := fnv.New64a()
	// Find the last user message index.
	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == fantasy.MessageRoleUser {
			lastUser = i
			break
		}
	}
	// Hash everything before the last user message.
	for i := 0; i < len(msgs); i++ {
		if i == lastUser {
			break
		}
		fmt.Fprintf(h, "%d:%s:%s\n", i, msgs[i].Role, extractText(msgs[i]))
	}
	return h.Sum64()
}

// extractLatestUserMessage returns only the text of the last user message
// from the prompt, for use with --resume where the CLI already has history.
func extractLatestUserMessage(msgs fantasy.Prompt, filePaths map[int][]string) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == fantasy.MessageRoleUser {
			text := extractText(msgs[i])
			files := filePaths[i]
			if len(files) > 0 {
				var sb strings.Builder
				sb.WriteString(text)
				for _, f := range files {
					sb.WriteString("\n[Attached file: ")
					sb.WriteString(f)
					sb.WriteString("]")
				}
				return sb.String()
			}
			return text
		}
	}
	// Fallback: full prompt if no user message found.
	return formatPrompt(msgs, filePaths)
}

func formatPrompt(msgs fantasy.Prompt, filePaths map[int][]string) string {
	var sb strings.Builder
	for i, msg := range msgs {
		text := extractText(msg)
		files := filePaths[i]
		if text == "" && len(files) == 0 {
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
			for _, f := range files {
				sb.WriteString("\n[Attached file: ")
				sb.WriteString(f)
				sb.WriteString("]")
			}
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

// argsByteLen totals the bytes of all args — useful when diagnosing
// command-line-length issues on Windows (CreateProcessW has a 32K limit).
func argsByteLen(args []string) int {
	n := 0
	for _, a := range args {
		n += len(a) + 1 // +1 for the separator
	}
	return n
}

// clipString returns the first n chars of s with a "(+K more)" suffix when
// truncated. Used in slog fields to keep a sample of long prompts without
// blowing up the log file.
func clipString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("…(+%d more)", len(s)-n)
}

// tailString returns the last n chars of s with a "(+K skipped)" prefix when
// truncated. Pair with clipString to see prompt boundaries.
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("(+%d skipped)…", len(s)-n) + s[len(s)-n:]
}
