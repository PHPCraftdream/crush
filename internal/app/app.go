// Package app wires together services, coordinates agents, and manages
// application lifecycle.
package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/format"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/shell"

	"github.com/charmbracelet/crush/internal/update"
	"github.com/charmbracelet/crush/internal/version"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

type App struct {
	Sessions    session.Service
	Messages    message.Service
	History     history.Service
	Permissions permission.Service
	FileTracker filetracker.Service

	AgentCoordinator agent.Coordinator

	LSPManager *lsp.Manager

	config *config.ConfigStore

	// global context and cleanup functions
	globalCtx          context.Context
	cleanupFuncs       []func(context.Context) error
	agentNotifications *pubsub.Broker[notify.Notification]
	events             *pubsub.Broker[any]

	// recoveryOrphanAge — internal test seam for recoverInterruptedTurns.
	// nil = use the production default (30s). Tests set it to 0 so they
	// don't have to sleep waiting for fresh messages to "age out" before
	// recovery considers them orphans.
	recoveryOrphanAge *time.Duration
}

// New initializes a new application instance.
func New(ctx context.Context, conn *sql.DB, store *config.ConfigStore) (*App, error) {
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)
	files := history.NewService(q, conn)
	cfg := store.Config()
	skipPermissionsRequests := store.Overrides().SkipPermissionRequests
	var allowedTools []string
	if cfg.Permissions != nil && cfg.Permissions.AllowedTools != nil {
		allowedTools = cfg.Permissions.AllowedTools
	}

	app := &App{
		Sessions:    sessions,
		Messages:    messages,
		History:     files,
		Permissions: permission.NewPermissionService(ctx, store.WorkingDir(), skipPermissionsRequests, allowedTools, q),
		FileTracker: filetracker.NewService(q),
		LSPManager:  lsp.NewManager(store),

		globalCtx: ctx,

		config:             store,
		agentNotifications: pubsub.NewBroker[notify.Notification](),
		events:            pubsub.NewBroker[any](),
	}

	// Check for updates in the background.
	go app.checkForUpdates(ctx)

	// Startup recovery: any assistant message left without a finish part
	// from a previous run is treated as an interrupted turn — we add a
	// FinishReasonError to it so the UI/non-interactive callers don't see
	// it as still in-flight. Backs the "Codec must surface control"
	// invariant: even when the previous process died ungracefully (kill,
	// power loss, panic) we release the session on next startup. See
	// the 162-promise-all post-mortem in CHANGELOG.fork.md section 4.D.
	app.recoverInterruptedTurns(ctx)

	go mcp.Initialize(ctx, app.Permissions, store)

	// Release the shared database connection on shutdown. The pool
	// closes the underlying *sql.DB when the last reference is released.
	dataDir := cfg.Options.DataDirectory
	app.cleanupFuncs = append(
		app.cleanupFuncs,
		func(context.Context) error { return db.Release(dataDir) },
		func(ctx context.Context) error { return mcp.Close(ctx) },
	)

	// TODO: remove the concept of agent config, most likely.
	if !cfg.IsConfigured() {
		slog.Warn("No agent configuration found")
		return app, nil
	}
	if err := app.InitCoderAgent(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize coder agent: %w", err)
	}

	// Set up callback for LSP state updates.
	app.LSPManager.SetCallback(func(name string, client *lsp.Client) {
		if client == nil {
			updateLSPState(name, lsp.StateUnstarted, nil, nil, 0)
			return
		}
		client.SetDiagnosticsCallback(updateLSPDiagnostics)
		updateLSPState(name, client.GetServerState(), nil, client, 0)
	})
	go app.LSPManager.TrackConfigured()

	return app, nil
}

// Config returns the pure-data configuration.
// disableSubAgentToolsInConfig drops the "agent" and "agentic_fetch"
// tools from the coder agent's AllowedTools list in the in-memory
// config. Used by RunNonInteractive when overrides.DisableSubAgents
// (`crush run --agents single`) is set. Mutation does not touch the
// on-disk config and only outlives this process if a future caller
// reloads the in-memory config from disk — `crush run` exits
// immediately after the agent turn so this is moot in practice.
//
// Fork patch (orchestrator UX): see CHANGELOG.fork.md (Section 4.J).
func (app *App) disableSubAgentToolsInConfig() {
	cfg := app.config.Config()
	if cfg == nil {
		return
	}
	coder, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return
	}
	filtered := coder.AllowedTools[:0:0]
	for _, t := range coder.AllowedTools {
		if t == "agent" || t == "agentic_fetch" {
			continue
		}
		filtered = append(filtered, t)
	}
	coder.AllowedTools = filtered
	cfg.Agents[config.AgentCoder] = coder
}

func (app *App) Config() *config.Config {
	return app.config.Config()
}

// Store returns the config store.
func (app *App) Store() *config.ConfigStore {
	return app.config
}

// Events returns a per-caller subscription channel for application events.
// Each caller receives its own channel; all callers receive every event.
func (app *App) Events(ctx context.Context) <-chan pubsub.Event[any] {
	return app.events.Subscribe(ctx)
}

func (app *App) SendEvent(msg any) {
	app.events.Publish(pubsub.UpdatedEvent, msg)
}

// AgentNotifications returns the broker for agent notification events.
func (app *App) AgentNotifications() *pubsub.Broker[notify.Notification] {
	return app.agentNotifications
}

// resolveSession resolves which session to use for a non-interactive run
// If continueSessionID is set, it looks up that session by ID
// If useLast is set, it returns the most recently updated top-level session
// Otherwise, it creates a new session
func (app *App) resolveSession(ctx context.Context, continueSessionID string, useLast bool) (session.Session, error) {
	switch {
	case continueSessionID != "":
		if app.Sessions.IsAgentToolSession(continueSessionID) {
			return session.Session{}, fmt.Errorf("cannot continue an agent tool session: %s", continueSessionID)
		}
		sess, err := app.Sessions.Get(ctx, continueSessionID)
		if err == nil {
			if sess.ParentSessionID != "" {
				return session.Session{}, fmt.Errorf("cannot continue a child session: %s", continueSessionID)
			}
			return sess, nil
		}
		// Get-or-create semantics: --session <id> with an unknown id creates
		// a brand-new top-level session with that exact id. Lets CI / scripts
		// pick a deterministic key (e.g. an issue number) and re-run idempotently.
		created, createErr := app.Sessions.CreateWithID(ctx, continueSessionID, continueSessionID)
		if createErr != nil {
			return session.Session{}, fmt.Errorf("session %q not found and could not be created: %w", continueSessionID, createErr)
		}
		slog.Info("Created session on demand from --session id", "session_id", created.ID)
		return created, nil

	case useLast:
		sess, err := app.Sessions.GetLast(ctx)
		if err != nil {
			return session.Session{}, fmt.Errorf("no sessions found to continue")
		}
		return sess, nil

	default:
		return app.Sessions.Create(ctx, agent.DefaultSessionName)
	}
}

// RunNonInteractive runs the application in non-interactive mode with the
// given prompt, printing to stdout.
// runResult is the JSON shape emitted by `crush run --json`. Wire-stable:
// fields here are part of the public contract for wrapper scripts.
type runResult struct {
	SessionID  string `json:"session_id"`
	// ExitReason vocabulary:
	//   "stop","end_turn","tool_use","max_tokens","unknown"  — model-level
	//   "error"                                              — generic
	//   "canceled"                                           — caller-cancel
	//   "invalid_json" (fork-only)                           — --json /
	//       --format json was active and stripped output failed
	//       json.Valid; orchestrators that pipe final_text into jq
	//       SHOULD branch on this instead of treating exit_reason=stop
	//       as proof the content is valid JSON.
	ExitReason string `json:"exit_reason"`
	FinalText  string `json:"final_text"`
	// Fork patch (orchestrator UX): when --json or --format json
	// triggered the fence/preamble stripper and the model HAD wrapped
	// its answer in prose or a markdown fence, the unstripped original
	// is preserved here so the orchestrator can audit what the model
	// actually said. Empty when no stripping was applied or when the
	// model returned clean JSON already. When ExitReason="invalid_json",
	// AssistantNotes carries the strip attempt's (invalid) candidate
	// for side-by-side comparison.
	AssistantNotes string `json:"assistant_notes,omitempty"`
	// StrippedBytes is how many bytes the stripper removed from
	// final_text (0 when no strip happened or when validation failed
	// and we restored the original). Surfaces observability for the
	// "model keeps writing a preamble" failure mode — orchestrators
	// can graph this over time.
	StrippedBytes int    `json:"stripped_bytes,omitempty"`
	Error         string `json:"error,omitempty"`
	// Warnings are non-fatal observations about the run that an
	// orchestrator should know about even when exit_reason looks happy.
	// Examples: agent fan-out finished with empty final_text (model
	// dispatched sub-agents but never composed a final reply, so
	// orchestrators expecting structured output get nothing); write tool
	// hit a stdout-redirect target. Wrappers can ignore the field if
	// they don't care.
	Warnings   []string       `json:"warnings,omitempty"`
	ToolCalls  []toolCallStat `json:"tool_calls"`
	Usage      usageInfo      `json:"usage"`
	DurationMs int64          `json:"duration_ms"`
}

type toolCallStat struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type usageInfo struct {
	DeltaTokens  int64   `json:"delta_tokens"`
	DeltaCostUSD float64 `json:"delta_cost_usd"`
}

// buildRunResult assembles runResult from the bits collected during the
// run. exit_reason follows the same vocabulary the WUI uses (see
// message.FinishReason*) plus a synthetic "canceled" / "error" when the
// agent never finalised a message.
//
// finalErrTitle and finalErrDetails come from the assistant message's
// Finish part when Reason=error (e.g. "Stream stalled" /
// "Provider X stopped sending streaming data for over 3m0s..."). They
// surface into runResult.Error so orchestrators see WHY a turn errored,
// not just THAT it did.
// Fork patch (orchestrator UX): assistantNotes added. Carries the
// stripped prose/fence content when --json or --format json triggered
// stripJSONEnvelope and the model had wrapped the JSON; "" otherwise.
//
// strippedBytes / stripErrMsg / stripErrReason are populated by the
// JSON validation step (stripAndValidateJSON). stripErrReason, when
// non-empty, OVERRIDES the model's finalReason so the envelope tells
// the orchestrator "you asked for JSON, it didn't validate" instead of
// the model's optimistic "stop"/"end_turn".
func buildRunResult(sessionID, finalText, assistantNotes, finalReason string, err error, canceled bool, toolCounts map[string]int, deltaTokens int64, deltaCost float64, duration time.Duration, finalErrTitle, finalErrDetails string, strippedBytes int, stripErrMsg, stripErrReason string) runResult {
	reason := finalReason
	if reason == "" {
		switch {
		case canceled:
			reason = "canceled"
		case err != nil:
			reason = "error"
		default:
			reason = "unknown"
		}
	}
	calls := make([]toolCallStat, 0, len(toolCounts))
	for name, count := range toolCounts {
		calls = append(calls, toolCallStat{Name: name, Count: count})
	}
	// Stable ordering so the JSON diffs cleanly across runs.
	sort.Slice(calls, func(i, j int) bool { return calls[i].Name < calls[j].Name })

	// Warnings: non-fatal observations the orchestrator should see.
	var warnings []string
	// Fan-out without composition: model dispatched at least one sub-agent
	// (`agent`/`agentic_fetch`) but the turn ended with no final text. The
	// orchestrator asked for a structured answer and got an empty string,
	// which usually means the model expected the sub-agents to "be the
	// answer" — but `crush run` returns ONLY the top-level final_text, so
	// the actual content sits in the sub-session DB rows the orchestrator
	// can't easily see. Telling them to either prompt for a wrap-up
	// summary or fetch the sub-session data explicitly.
	if reason != "error" && reason != "canceled" && strings.TrimSpace(finalText) == "" {
		fanoutCalls := toolCounts["agent"] + toolCounts["agentic_fetch"]
		if fanoutCalls > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"final_text is empty after %d sub-agent fan-out call(s). The model dispatched sub-agents but did not compose a top-level reply — query the sub-session DB rows directly, or prompt the model to summarise into final_text.",
				fanoutCalls,
			))
		}
	}
	errMsg := ""
	switch {
	case err != nil && !canceled:
		errMsg = err.Error()
	case reason == "error":
		// In-band error: the agent finished its turn but the model's
		// Finish part says reason=error (e.g. watchdog stall, provider
		// error, empty stream). Surface title + details so wrappers
		// don't have to re-query the DB.
		switch {
		case finalErrTitle != "" && finalErrDetails != "":
			errMsg = finalErrTitle + ": " + finalErrDetails
		case finalErrTitle != "":
			errMsg = finalErrTitle
		case finalErrDetails != "":
			errMsg = finalErrDetails
		}
	}
	// Fork patch (orchestrator UX): bug 4 from the 2026-05-17 audit
	// feedback — when reason=="error" but the model's Finish part
	// carried no Message/Details (some providers emit a bare error
	// finish), errMsg stayed empty and the orchestrator had no clue
	// WHY the turn died. Provide an informative fallback that names
	// the most likely causes so the operator at least knows where to
	// start looking. Also flag a truncation-hint when final_text
	// looks unfinished (ends mid-sentence or with a leading-in
	// punctuation like ":") so the operator sees "model was about to
	// continue".
	if reason == "error" && errMsg == "" {
		errMsg = "unknown error (provider returned an error finish without a message — likely causes: provider HTTP error, stream stall before watchdog fired, OOM-kill, context-window overflow). Re-run with --verbose for stderr detail."
	}
	if reason == "error" {
		trimmed := strings.TrimRight(strings.TrimSpace(finalText), " \t")
		if n := len(trimmed); n > 0 {
			last := trimmed[n-1]
			if last == ':' || last == ',' || last == '-' {
				warnings = append(warnings, fmt.Sprintf(
					"final_text appears truncated (ends with %q) — model was likely composing more output when the error fired. Last 80 chars: %q",
					string(last), tailN(trimmed, 80),
				))
			}
		}
	}
	// Fork patch (orchestrator UX): strip-validation overrides reason
	// + error when the operator asked for JSON and the stripped output
	// did not parse. We DO want to clobber the model's optimistic
	// "end_turn" / "stop" here because from the orchestrator's point
	// of view this run failed its contract.
	if stripErrReason != "" {
		reason = stripErrReason
		if errMsg == "" {
			errMsg = stripErrMsg
		} else {
			errMsg = errMsg + "; " + stripErrMsg
		}
	}
	return runResult{
		SessionID:      sessionID,
		ExitReason:     reason,
		FinalText:      finalText,
		AssistantNotes: assistantNotes,
		StrippedBytes:  strippedBytes,
		Error:          errMsg,
		Warnings:       warnings,
		ToolCalls:      calls,
		Usage: usageInfo{
			DeltaTokens:  deltaTokens,
			DeltaCostUSD: deltaCost,
		},
		DurationMs: duration.Milliseconds(),
	}
}

// tailN returns the last n runes of s (or the whole s if shorter). Used
// to put a small "what was the model writing when it died" snippet into
// the truncation warning without dumping kilobytes into the envelope.
func tailN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// RunMode picks the output format for RunNonInteractive.
type RunMode int

const (
	// RunModeTerse: tool-call names on stderr, final assistant message on
	// stdout. Default — small output, friendly to wrapper scripts.
	RunModeTerse RunMode = iota
	// RunModeStream: every assistant token streams to stdout as it arrives.
	// Legacy behaviour; useful when a human is watching.
	RunModeStream
	// RunModeJSON: stdout gets exactly one JSON object summarising the run
	// (session id, final text, tool-call counts, token usage, duration,
	// exit reason). Tool-call heartbeat still goes to stderr so wrappers
	// can show progress without parsing JSON deltas.
	RunModeJSON
)

// RunOverrides bundles the optional per-invocation overrides for
// RunNonInteractive so the signature doesn't keep growing.
//
// Persistence: every non-empty field is written to the session BEFORE
// the agent runs, so a subsequent `crush run --session <same>` without
// those flags continues with the same overrides. Empty fields are
// left alone (they don't reset what's already on the session).
type RunOverrides struct {
	LargeModel   string // "model" or "provider/model"; overrides selected large
	SmallModel   string // same as LargeModel, for the small slot
	SystemPrompt string // persisted on the session (Sessions.UpdateSystemPrompt)
	// ReasoningEffort applies to whichever slot is "active" for this run —
	// the large one if RoleLarge is true, the small one otherwise. Persisted
	// via Sessions.UpdateReasoningEffort.
	ReasoningEffort string
	RoleLarge       bool
	// Fork patch (orchestrator UX): DisableSubAgents drops the `agent`
	// and `agentic_fetch` tools from the coder agent for this run so a
	// `crush run --agents single` invocation cannot fan out. Mutation
	// is per-process — `crush run` is single-shot, so the change does
	// not leak across invocations. StripJSONFences asks
	// RunNonInteractive to post-process the envelope's final_text
	// (markdown fence + prose preamble removal); the unstripped
	// original is preserved in runResult.AssistantNotes.
	DisableSubAgents bool
	StripJSONFences  bool
}

// RunNonInteractive runs a single agent turn and writes its result to
// `output`. See RunMode for the available output shapes.
func (app *App) RunNonInteractive(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) error {
	largeModel := overrides.LargeModel
	smallModel := overrides.SmallModel
	systemPrompt := overrides.SystemPrompt
	slog.Info("Running in non-interactive mode")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if largeModel != "" || smallModel != "" {
		if err := app.overrideModelsForNonInteractive(ctx, largeModel, smallModel); err != nil {
			return fmt.Errorf("failed to override models: %w", err)
		}
	}

	var (
		spinner   *format.Spinner
		stderrTTY bool
		progress  bool
	)

	stderrTTY = term.IsTerminal(os.Stderr.Fd())
	progress = app.config.Config().Options.Progress == nil || *app.config.Config().Options.Progress

	if !hideSpinner && stderrTTY {
		spinner = format.NewSpinner()
		spinner.Start()
	}

	// Helper function to stop spinner once.
	stopSpinner := func() {
		if !hideSpinner && spinner != nil {
			spinner.Stop()
			spinner = nil
		}
	}

	// Wait for MCP initialization to complete before reading MCP tools.
	if err := mcp.WaitForInit(ctx); err != nil {
		return fmt.Errorf("failed to wait for MCP initialization: %w", err)
	}

	// Fork patch (orchestrator UX): --agents single. Drop the agent /
	// agentic_fetch tools from the coder agent's AllowedTools BEFORE
	// UpdateModels rebuilds the toolset so the model literally cannot
	// fan out. Mutation is in-process only (crush run is a single-shot
	// process — exit drops the change), so this is safe even though
	// it touches the global config. See run.go and run_format.go.
	if overrides.DisableSubAgents {
		app.disableSubAgentToolsInConfig()
	}

	// force update of agent models before running so mcp tools are loaded
	app.AgentCoordinator.UpdateModels(ctx)

	defer stopSpinner()

	sess, err := app.resolveSession(ctx, continueSessionID, useLast)
	if err != nil {
		return fmt.Errorf("failed to create session for non-interactive mode: %w", err)
	}

	if continueSessionID != "" || useLast {
		slog.Info("Continuing session for non-interactive run", "session_id", sess.ID)
	} else {
		slog.Info("Created session for non-interactive run", "session_id", sess.ID)
	}

	// Persist the requested system prompt for this session. Coordinator's
	// resolveSessionSystemPrompt will pick it up on the next Run(); leaving
	// systemPrompt empty preserves whatever was previously stored (or causes
	// the default prompt to be built and stored on first run).
	if systemPrompt != "" {
		if err := app.Sessions.UpdateSystemPrompt(ctx, sess.ID, systemPrompt); err != nil {
			return fmt.Errorf("failed to set system prompt for session: %w", err)
		}
	}

	// Persist reasoning effort onto the active slot. We pass the current
	// stored value for the *other* slot through so we don't clobber it —
	// UpdateReasoningEffort takes both fields as a single transaction.
	if overrides.ReasoningEffort != "" {
		large := sess.LargeModelReasoningEffort
		small := sess.SmallModelReasoningEffort
		if overrides.RoleLarge {
			large = overrides.ReasoningEffort
		} else {
			small = overrides.ReasoningEffort
		}
		if err := app.Sessions.UpdateReasoningEffort(ctx, sess.ID, large, small); err != nil {
			return fmt.Errorf("failed to set reasoning effort: %w", err)
		}
	}

	// Automatically approve all permission requests for this non-interactive
	// session.
	app.Permissions.AutoApproveSession(sess.ID)

	type response struct {
		result *fantasy.AgentResult
		err    error
	}
	done := make(chan response, 1)
	runStart := time.Now()
	tokensBefore := sess.PromptTokens + sess.CompletionTokens
	costBefore := sess.Cost

	go func(ctx context.Context, sessionID, prompt string) {
		result, err := app.AgentCoordinator.Run(ctx, sess.ID, prompt)
		if err != nil {
			done <- response{
				err: fmt.Errorf("failed to start agent processing stream: %w", err),
			}
			return
		}
		done <- response{
			result: result,
		}
	}(ctx, sess.ID, prompt)

	messageEvents := app.Messages.Subscribe(ctx)
	messageReadBytes := make(map[string]int)
	seenToolCalls := make(map[string]bool)
	toolCallCounts := make(map[string]int) // name → count, for JSON output
	printedFinal := make(map[string]bool)  // for terse mode: print once per finished assistant msg
	var finalText string                    // last assistant FullText seen, for JSON output
	var finalReason string                  // last assistant Finish.Reason seen, for JSON output
	var finalErrTitle, finalErrDetails string // Finish.Message + Finish.Details, surfaced into envelope.Error when reason=error
	var printed bool

	defer func() {
		if progress && stderrTTY {
			_, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar)
		}

		// JSON mode emits its own trailing newline via json.Encoder; the
		// terse/stream modes need a bare \n so a follow-up shell prompt
		// doesn't overwrite the last token.
		if mode != RunModeJSON {
			_, _ = fmt.Fprintln(output)
		}
	}()

	for {
		if progress && stderrTTY {
			// HACK: Reinitialize the terminal progress bar on every iteration
			// so it doesn't get hidden by the terminal due to inactivity.
			_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
		}

		select {
		case result := <-done:
			stopSpinner()
			runErr := result.err
			isCanceled := runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, agent.ErrRequestCancelled))

			if mode == RunModeJSON {
				// Re-fetch the session row so the usage delta reflects
				// the writes the agent made during the run.
				freshSess, _ := app.Sessions.Get(ctx, sess.ID)
				// Fork patch (orchestrator UX): when the caller asked
				// for JSON, defang the persistent "model wrapped its
				// final JSON in a ```json fence and added prose" case
				// here so wrappers can pipe final_text straight into
				// jq. The original is preserved in assistant_notes.
				//
				// stripAndValidateJSON ALSO runs json.Valid on the
				// stripped output; on failure it surfaces the original
				// in final_text + the (invalid) candidate in
				// assistant_notes + a wrapped ErrInvalidStripJSON so
				// the envelope reports exit_reason=invalid_json. This
				// closes the silent-success failure mode reported in
				// the 2026-05-17 audit feedback (model emitted a JSON
				// missing a closing bracket, walker said "balanced",
				// orchestrator spent an hour in node-repl).
				finalTextOut := finalText
				assistantNotes := ""
				strippedBytes := 0
				stripErr := ""
				stripErrReason := ""
				if overrides.StripJSONFences && finalReason != "error" && finalReason != "canceled" {
					cleaned, notes, vErr := stripAndValidateJSON(finalText)
					finalTextOut = cleaned
					assistantNotes = notes
					strippedBytes = len(finalText) - len(cleaned)
					if strippedBytes < 0 {
						strippedBytes = 0
					}
					if vErr != nil {
						stripErr = vErr.Error()
						stripErrReason = "invalid_json"
					}
				}
				summary := buildRunResult(
					sess.ID, finalTextOut, assistantNotes, finalReason, runErr, isCanceled,
					toolCallCounts,
					freshSess.PromptTokens+freshSess.CompletionTokens-tokensBefore,
					freshSess.Cost-costBefore,
					time.Since(runStart),
					finalErrTitle, finalErrDetails,
					strippedBytes, stripErr, stripErrReason,
				)
				enc := json.NewEncoder(output)
				if encErr := enc.Encode(summary); encErr != nil {
					return fmt.Errorf("failed to encode JSON result: %w", encErr)
				}
				if isCanceled || runErr == nil {
					return nil
				}
				// Non-cancel error: JSON already carries it; surface a
				// non-zero exit code by returning the err.
				return runErr
			}

			if runErr != nil {
				if isCanceled {
					slog.Debug("Non-interactive: agent processing cancelled", "session_id", sess.ID)
					return nil
				}
				return fmt.Errorf("agent processing failed: %w", runErr)
			}
			return nil

		case event := <-messageEvents:
			msg := event.Payload
			if msg.SessionID == sess.ID && msg.Role == message.Assistant && len(msg.Parts) > 0 {
				stopSpinner()

				// Tool-call names always go to stderr — one short line per
				// new call. This gives wrappers and humans a heartbeat
				// without exposing inputs / outputs.
				for _, p := range msg.Parts {
					if tc, ok := p.(message.ToolCall); ok && tc.Name != "" && !seenToolCalls[tc.ID] {
						seenToolCalls[tc.ID] = true
						toolCallCounts[tc.Name]++
						prefix := ""
						if stderrTTY {
							prefix = "\r" + ansi.EraseEntireLine
						}
						fmt.Fprintf(os.Stderr, prefix+"▶ %s\n", tc.Name)
					}
				}

				// Track final state for JSON mode regardless of which
				// output mode is active — JSON output materialises after
				// the run completes, so we accumulate as we go.
				if msg.IsFinished() {
					finalText = msg.FullText()
					for _, p := range msg.Parts {
						if f, ok := p.(message.Finish); ok {
							finalReason = string(f.Reason)
							finalErrTitle = f.Message
							finalErrDetails = f.Details
							break
						}
					}
				}

				switch mode {
				case RunModeJSON:
					// Suppress per-message stdout entirely; the summary is
					// printed below after `done` fires.
				case RunModeTerse:
					if !msg.IsFinished() || printedFinal[msg.ID] {
						continue
					}
					text := strings.TrimLeft(msg.FullText(), " \t\n")
					if text != "" {
						printedFinal[msg.ID] = true
						printed = true
						fmt.Fprint(output, text)
					}
				case RunModeStream:
					content := msg.FullText()
					readBytes := messageReadBytes[msg.ID]
					if len(content) < readBytes {
						slog.Error("Non-interactive: message content is shorter than read bytes", "message_length", len(content), "read_bytes", readBytes)
						return fmt.Errorf("message content is shorter than read bytes: %d < %d", len(content), readBytes)
					}
					part := content[readBytes:]
					if readBytes == 0 {
						part = strings.TrimLeft(part, " \t")
					}
					if printed || strings.TrimSpace(part) != "" {
						printed = true
						fmt.Fprint(output, part)
					}
					messageReadBytes[msg.ID] = len(content)
				}
			}

		case <-ctx.Done():
			stopSpinner()
			return ctx.Err()
		}
	}
}

func (app *App) UpdateAgentModel(ctx context.Context) error {
	if app.AgentCoordinator == nil {
		return fmt.Errorf("agent configuration is missing")
	}
	return app.AgentCoordinator.UpdateModels(ctx)
}

// overrideModelsForNonInteractive parses the model strings and temporarily
// overrides the model configurations, then rebuilds the agent.
// Format: "model-name" (searches all providers) or "provider/model-name".
// Model matching is case-insensitive.
// If largeModel is provided but smallModel is not, the small model defaults to
// the provider's default small model.
func (app *App) overrideModelsForNonInteractive(ctx context.Context, largeModel, smallModel string) error {
	providers := app.config.Config().Providers.Copy()

	largeMatches, smallMatches, err := findModels(providers, largeModel, smallModel)
	if err != nil {
		return err
	}

	var largeProviderID string

	// Override large model.
	if largeModel != "" {
		found, err := validateMatches(largeMatches, largeModel, "large")
		if err != nil {
			return err
		}
		largeProviderID = found.provider
		slog.Info("Overriding large model for non-interactive run", "provider", found.provider, "model", found.modelID)
		app.config.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
			Provider: found.provider,
			Model:    found.modelID,
		}
	}

	// Override small model.
	switch {
	case smallModel != "":
		found, err := validateMatches(smallMatches, smallModel, "small")
		if err != nil {
			return err
		}
		slog.Info("Overriding small model for non-interactive run", "provider", found.provider, "model", found.modelID)
		app.config.Config().Models[config.SelectedModelTypeSmall] = config.SelectedModel{
			Provider: found.provider,
			Model:    found.modelID,
		}

	case largeModel != "":
		// No small model specified, but large model was - use provider's default.
		smallCfg := app.GetDefaultSmallModel(largeProviderID)
		app.config.Config().Models[config.SelectedModelTypeSmall] = smallCfg
	}

	return app.AgentCoordinator.UpdateModels(ctx)
}

// GetDefaultSmallModel returns the default small model for the given
// provider. Falls back to the large model if no default is found.
func (app *App) GetDefaultSmallModel(providerID string) config.SelectedModel {
	cfg := app.config.Config()
	largeModelCfg := cfg.Models[config.SelectedModelTypeLarge]

	// Find the provider in the known providers list to get its default small model.
	knownProviders, _ := config.Providers(cfg)
	var knownProvider *catwalk.Provider
	for _, p := range knownProviders {
		if string(p.ID) == providerID {
			knownProvider = &p
			break
		}
	}

	// For unknown/local providers, use the large model as small.
	if knownProvider == nil {
		slog.Warn("Using large model as small model for unknown provider", "provider", providerID, "model", largeModelCfg.Model)
		return largeModelCfg
	}

	defaultSmallModelID := knownProvider.DefaultSmallModelID
	model := cfg.GetModel(providerID, defaultSmallModelID)
	if model == nil {
		slog.Warn("Default small model not found, using large model", "provider", providerID, "model", largeModelCfg.Model)
		return largeModelCfg
	}

	slog.Info("Using provider default small model", "provider", providerID, "model", defaultSmallModelID)
	return config.SelectedModel{
		Provider:        providerID,
		Model:           defaultSmallModelID,
		MaxTokens:       model.DefaultMaxTokens,
		ReasoningEffort: model.DefaultReasoningEffort,
	}
}

func (app *App) InitCoderAgent(ctx context.Context) error {
	coderAgentCfg := app.config.Config().Agents[config.AgentCoder]
	if coderAgentCfg.ID == "" {
		return fmt.Errorf("coder agent configuration is missing")
	}
	var err error
	app.AgentCoordinator, err = agent.NewCoordinator(
		ctx,
		app.config,
		app.Sessions,
		app.Messages,
		app.Permissions,
		app.History,
		app.FileTracker,
		app.LSPManager,
		app.agentNotifications,
	)
	if err != nil {
		slog.Error("Failed to create coder agent", "err", err)
		return err
	}
	return nil
}

// recoverInterruptedTurns is the startup safety net for the "silent dying"
// pattern: a previous crush process that died ungracefully (kill -9, power
// loss, OS reboot, panic without recovery, or even a graceful Ctrl-C during
// the brief window where ctx.Canceled would bypass the in-flight Update)
// can leave assistant messages in the DB with tool calls but no finish
// part. Without recovery, the WUI renders those as "still streaming"
// forever, and `crush sessions reset` is the only escape.
//
// This sweep runs once at app start, before the coordinator is wired up.
// For every session, it finds the LAST assistant message and, if it has no
// finish part, adds a FinishReasonError marking it as a process-restart
// interruption. Cheap (O(sessions × 1 query each)), non-fatal on error,
// silent when there is nothing to recover.
func (app *App) recoverInterruptedTurns(ctx context.Context) {
	// Bound the whole sweep so a slow disk (network mount, AV scan,
	// fsync stall) cannot block app startup. 10s is generous for a
	// linear scan of sessions + targeted updates against SQLite; if it
	// trips we'd rather skip recovery than hang the user's first
	// `crush run`.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	// Age threshold for "this orphan is really orphan" vs "this is a
	// fresh assistant another concurrent process just created". 30s is
	// long enough to cover startup + the first inter-process
	// notification roundtrip; short enough that recovery doesn't wait
	// 5 minutes for stale orphans. See bug-analyzer audit, #7
	// "Recovery vs new turn race".
	orphanAgeThreshold := 30 * time.Second
	if app.recoveryOrphanAge != nil {
		orphanAgeThreshold = *app.recoveryOrphanAge
	}
	staleBefore := start.Add(-orphanAgeThreshold).Unix()
	sessions, err := app.Sessions.List(ctx)
	if err != nil {
		slog.Warn("startup recovery: failed to list sessions", "err", err)
		return
	}
	var recovered, skippedFresh int
	for _, sess := range sessions {
		msgs, err := app.Messages.List(ctx, sess.ID)
		if err != nil {
			slog.Debug("startup recovery: skipping session, list failed",
				"session_id", sess.ID, "err", err)
			continue
		}
		var lastAssistant *message.Message
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == message.Assistant {
				m := msgs[i]
				lastAssistant = &m
				break
			}
		}
		if lastAssistant == nil || lastAssistant.IsFinished() {
			continue
		}
		// Age filter: skip messages another concurrent crush process
		// might have just created. Without this, recovery would mark a
		// fresh streaming assistant as "Process restarted" mid-stream.
		if lastAssistant.CreatedAt > staleBefore {
			skippedFresh++
			continue
		}
		lastAssistant.AddFinish(
			message.FinishReasonError,
			"Process restarted",
			"The previous crush process exited before this turn completed (silent dying — see CHANGELOG.fork.md section 4.D). The assistant message had tool calls but no finish part. Cleanly recovered on startup; you can retry from the previous user message.",
		)
		if err := app.Messages.Update(ctx, *lastAssistant); err != nil {
			slog.Warn("startup recovery: failed to mark orphan assistant",
				"session_id", sess.ID,
				"message_id", lastAssistant.ID,
				"err", err,
			)
			continue
		}
		recovered++
	}
	elapsed := time.Since(start)
	if recovered > 0 || skippedFresh > 0 {
		slog.Info("startup recovery: completed",
			"recovered", recovered,
			"skipped_fresh", skippedFresh,
			"total_sessions_scanned", len(sessions),
			"elapsed", elapsed.String(),
		)
	} else if elapsed > time.Second {
		// Silent normally, but if the sweep took noticeable time
		// (10k+ sessions on slow disk), surface it so the user can
		// diagnose a slow startup without enabling debug logs.
		slog.Info("startup recovery: nothing to recover",
			"total_sessions_scanned", len(sessions),
			"elapsed", elapsed.String(),
		)
	}
}

// Shutdown performs a graceful shutdown of the application.
func (app *App) Shutdown() {
	start := time.Now()
	defer func() { slog.Debug("Shutdown took " + time.Since(start).String()) }()

	// First, cancel all agents and wait for them to finish. This must complete
	// before closing the DB so agents can finish writing their state.
	if app.AgentCoordinator != nil {
		app.AgentCoordinator.CancelAll()
	}

	// Shared shutdown context for all timeout-bounded cleanup.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Fork merge note: upstream 6938dedd added FlushAll for its debounced
	// message-update layer. We removed that layer (see message/message.go);
	// Update() writes synchronously, so there is nothing to drain here.
	_ = shutdownCtx

	// Now run remaining cleanup tasks in parallel.
	var wg sync.WaitGroup

	// Send exit event
	wg.Go(func() {
	})

	// Kill all background shells.
	wg.Go(func() {
		shell.GetBackgroundShellManager().KillAll(shutdownCtx)
	})

	// Shutdown all LSP clients.
	wg.Go(func() {
		app.LSPManager.KillAll(shutdownCtx)
	})

	// Call all cleanup functions.
	for _, cleanup := range app.cleanupFuncs {
		if cleanup != nil {
			wg.Go(func() {
				if err := cleanup(shutdownCtx); err != nil {
					slog.Error("Failed to cleanup app properly on shutdown", "error", err)
				}
			})
		}
	}
	wg.Wait()
}

// checkForUpdates checks for available updates.
func (app *App) checkForUpdates(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	info, err := update.Check(checkCtx, version.Version, update.Default)
	if err != nil || !info.Available() {
		return
	}
}
