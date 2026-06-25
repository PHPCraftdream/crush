// Package agent is the core orchestration layer for Crush AI agents.
//
// It provides session-based AI agent functionality for managing
// conversations, tool execution, and message handling. It coordinates
// interactions between language models, messages, sessions, and tools while
// handling features like automatic summarization, queuing, and token
// management.
package agent

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/charmbracelet/crush/internal/agent/cliprovider"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/stringext"
	"github.com/charmbracelet/crush/internal/version"
)

const (
	DefaultSessionName = "Untitled Session"

	// contextSlideRatio is the fraction of context window retained when the
	// sliding window kicks in (e.g. 0.70 = keep the newest 70% of tokens).
	contextSlideRatio = 0.70

	// contextSlideThreshold is the fraction of remaining context that triggers
	// the sliding window. When less than (1-contextSlideRatio) of the window is
	// left we trim the oldest messages so the next call fits within the budget.
	contextSlideThreshold = 1.0 - contextSlideRatio

	// Constants for auto-summarization thresholds (used only for background
	// summarisation triggered at the same time as the sliding window).
	largeContextWindowThreshold = 200_000
	largeContextWindowBuffer    = 20_000
	smallContextWindowRatio     = 0.2

	// streamIdleTimeoutDefault is the default tolerance for "no streaming
	// event for this long" before the watchdog cancels the LLM request.
	// Configurable per-app via Options.StreamIdleTimeoutSeconds, plumbed
	// through SessionAgentOptions.StreamIdleTimeout. Read at Run()-time
	// via effectiveStreamIdleTimeout below.
	//
	// Raised to 10 min on 2026-06-17. Extended-thinking models (GLM-5.2
	// on max effort, Opus 4.7+ with large thinking budgets, Sonnet 4.5
	// with thinking_budget ~32k) routinely go silent at the wire while
	// reasoning server-side — no reasoning_content deltas are streamed
	// until the final answer arrives. The previous 3-minute default
	// killed those runs prematurely. 10 minutes covers every observed
	// case so far without letting truly hung streams sit forever.
	// Operators who want the old behaviour can set the value back via
	// Options.StreamIdleTimeoutSeconds.
	streamIdleTimeoutDefault = 10 * time.Minute
	// streamWatchdogTick is how often the watchdog re-checks the
	// last-activity timestamp. Keep small enough that a stall is detected
	// promptly (well under streamIdleTimeout) but large enough not to
	// dominate logs.
	streamWatchdogTick = 30 * time.Second

	// defaultCheckpointInterval is the default coalescing interval for
	// mid-stream DB flushes of in-progress assistant text. When > 0,
	// the auto-checkpoint ticker writes the Parts to DB at most once
	// per interval, bounding the text lost to a SIGTERM during final
	// composition. 0 disables checkpointing. Overridden by
	// SessionAgentOptions.CheckpointInterval.
	// Fork patch: batch 8.
	defaultCheckpointInterval = 2 * time.Second
)

var userAgent = fmt.Sprintf("Charm-Crush/%s (https://charm.land/crush)", version.Version)

//go:embed templates/title.md
var titlePrompt []byte

//go:embed templates/summary.md
var summaryPrompt []byte

// Used to remove <think> tags from generated titles.
var (
	thinkTagRegex       = regexp.MustCompile(`(?s)<think>.*?</think>`)
	orphanThinkTagRegex = regexp.MustCompile(`</?think>`)
)

type SessionAgentCall struct {
	SessionID        string
	Prompt           string
	ProviderOptions  fantasy.ProviderOptions
	Attachments      []message.Attachment
	MaxOutputTokens  int64
	Temperature      *float64
	TopP             *float64
	TopK             *int64
	FrequencyPenalty *float64
	PresencePenalty  *float64
	NonInteractive   bool
	// SystemPromptOverride, if non-empty, replaces the agent's global system prompt
	// for this single call. Used to apply per-session system prompts from the DB.
	SystemPromptOverride string
	// MaxCost aborts the run if total session cost exceeds this value (0 = no cap).
	MaxCost float64
	// MaxTokens aborts the run if total prompt+completion tokens exceed this value
	// (0 = no cap).
	MaxTokens int64
}

type SessionAgent interface {
	Run(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)
	SetModels(large Model, small Model)
	SetTools(tools []fantasy.AgentTool)
	SetSystemPrompt(systemPrompt string)
	SetSystemPromptPrefix(prefix string)
	SystemPrompt() string
	Cancel(sessionID string)
	CancelAll()
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	ClearQueue(sessionID string)
	// QueueMessage appends a call to the session's pending queue without
	// starting a Run. Used by the "interrupt and send" path in the web
	// server: the caller queues, then Cancel()s the running turn, and the
	// in-flight Run() drains the queue from its cancel-handling branch.
	QueueMessage(call SessionAgentCall)
	// InjectMessage persists `call` as a regular user message in the DB
	// immediately (so the UI sees it the moment the operator clicks Inject)
	// AND — if the session is currently running — schedules the message to
	// be appended to `prepared.Messages` at the next PrepareStep boundary so
	// it lands in the next provider request without a restart. Returns the
	// persisted message. When the session is NOT busy, the message is just
	// persisted; the caller can decide whether to start a new Run.
	InjectMessage(ctx context.Context, call SessionAgentCall) (message.Message, error)
	// Summarize compresses the session history. If the session is currently
	// busy the request is queued; call TakeSummarizeQueue after the task
	// finishes to pick it up.  Returns ErrSummarizeQueued when queued.
	Summarize(context.Context, string, fantasy.ProviderOptions) error
	// SummarizeQueued reports whether a manual summarise is pending for the
	// given session.
	SummarizeQueued(sessionID string) bool
	// TakeSummarizeQueue atomically removes and returns the pending summarise
	// options for the session (if any).
	TakeSummarizeQueue(sessionID string) (fantasy.ProviderOptions, bool)
	// CancelQueuedSummarize removes a pending summarise from the queue.
	CancelQueuedSummarize(sessionID string)
	// SetTimeoutOptions configures the stream watchdog's deadline extension
	// behaviour for the next and subsequent Run() calls. Called from
	// RunNonInteractive when --timeout-extends-on-progress is set.
	// Fork patch: batch 8.
	SetTimeoutOptions(extendsOnProgress bool, hardCap time.Duration)
	Model() Model
}

type Model struct {
	Model      fantasy.LanguageModel
	CatwalkCfg catwalk.Model
	ModelCfg   config.SelectedModel
	FlatRate   bool
}

type sessionAgent struct {
	largeModel         *csync.Value[Model]
	smallModel         *csync.Value[Model]
	systemPromptPrefix *csync.Value[string]
	systemPrompt       *csync.Value[string]
	tools              *csync.Slice[fantasy.AgentTool]

	isSubAgent           bool
	sessions             session.Service
	messages             message.Service
	disableAutoSummarize bool
	isYolo               bool
	notify               pubsub.Publisher[notify.Notification]
	// streamIdleTimeout, when > 0, overrides streamIdleTimeoutDefault for
	// every Run() on this agent. Set from Options.StreamIdleTimeoutSeconds
	// via SessionAgentOptions at construction. 0 = use the default.
	streamIdleTimeout time.Duration
	// dataDir is the absolute path to .crush/, used for the per-session
	// inter-process file lock. Empty means locking is disabled (legacy
	// callers / tests). Plumbed from SessionAgentOptions.DataDirectory.
	dataDir string
	// checkpointInterval is plumbed from SessionAgentOptions.
	// When > 0 the Run method starts a coalescing ticker that flushes
	// in-memory streaming Parts to DB mid-step, bounding text loss on
	// SIGTERM. Fork patch: batch 8.
	checkpointInterval time.Duration
	// timeoutExtendsOnProgress, when true, makes the stream watchdog
	// extend its deadline every time streaming progress occurs.
	// Fork patch: batch 8.
	timeoutExtendsOnProgress bool
	// timeoutHardCap is the maximum wall-clock time the watchdog will
	// allow, even with continuous progress. 0 = no cap.
	// Fork patch: batch 8.
	timeoutHardCap time.Duration

	messageQueue *csync.Map[string, []SessionAgentCall]
	// injectQueue holds user messages that were ALREADY persisted to the DB
	// (visible in the UI immediately) and are waiting to be merged into the
	// next provider request via PrepareStep. Unlike messageQueue (where the
	// DB write happens at drain time), injectQueue entries are pre-created
	// rows from InjectMessage — the drain just adds them to prepared.Messages
	// so the in-flight Run() sees them without restart. Seamless injection.
	injectQueue    *csync.Map[string, []message.Message]
	activeRequests *csync.Map[string, context.CancelFunc]
	// summarizeQueue holds a pending manual-summarise request per session,
	// queued while the session was busy.
	summarizeQueue *csync.Map[string, fantasy.ProviderOptions]
}

type SessionAgentOptions struct {
	LargeModel           Model
	SmallModel           Model
	SystemPromptPrefix   string
	SystemPrompt         string
	IsSubAgent           bool
	DisableAutoSummarize bool
	IsYolo               bool
	Sessions             session.Service
	Messages             message.Service
	Tools                []fantasy.AgentTool
	Notify               pubsub.Publisher[notify.Notification]
	// StreamIdleTimeout overrides streamIdleTimeoutDefault when > 0.
	// Plumbed from Options.StreamIdleTimeoutSeconds in the coordinator.
	StreamIdleTimeout time.Duration
	// DataDirectory is the absolute path to .crush/. Used by Run() to
	// acquire an inter-process file lock per session (prevents two
	// crush processes from accidentally working on the same session
	// id — see internal/session/lock.go).
	DataDirectory string
	// CheckpointInterval controls how often in-progress streaming
	// text is flushed to the DB mid-step. When > 0, a coalescing
	// ticker writes the in-memory Parts to the message row (with
	// finished_at still NULL) at most once per interval — but only
	// when Parts have actually changed since the last flush. This
	// bounds the text lost to a SIGTERM during final composition.
	// 0 (default) disables mid-stream checkpointing entirely.
	// Fork patch: batch 8 — see CHANGELOG.fork.md section 6.
	CheckpointInterval time.Duration
	// TimeoutExtendsOnProgress, when true, makes the stream watchdog
	// reset its deadline every time streaming progress occurs. This
	// prevents killing healthy long compositions. Default: false.
	// Fork patch: batch 8.
	TimeoutExtendsOnProgress bool
	// TimeoutHardCap is the maximum wall-clock time the watchdog will
	// allow even with continuous progress. Default: 0 (no cap, but
	// callers typically set 4x the idle timeout when extending).
	// Fork patch: batch 8.
	TimeoutHardCap time.Duration
}

func NewSessionAgent(
	opts SessionAgentOptions,
) SessionAgent {
	return &sessionAgent{
		largeModel:               csync.NewValue(opts.LargeModel),
		smallModel:               csync.NewValue(opts.SmallModel),
		systemPromptPrefix:       csync.NewValue(opts.SystemPromptPrefix),
		systemPrompt:             csync.NewValue(opts.SystemPrompt),
		isSubAgent:               opts.IsSubAgent,
		sessions:                 opts.Sessions,
		messages:                 opts.Messages,
		disableAutoSummarize:     opts.DisableAutoSummarize,
		tools:                    csync.NewSliceFrom(opts.Tools),
		isYolo:                   opts.IsYolo,
		notify:                   opts.Notify,
		messageQueue:             csync.NewMap[string, []SessionAgentCall](),
		injectQueue:              csync.NewMap[string, []message.Message](),
		activeRequests:           csync.NewMap[string, context.CancelFunc](),
		summarizeQueue:           csync.NewMap[string, fantasy.ProviderOptions](),
		streamIdleTimeout:        opts.StreamIdleTimeout,
		dataDir:                  opts.DataDirectory,
		checkpointInterval:       opts.CheckpointInterval,
		timeoutExtendsOnProgress: opts.TimeoutExtendsOnProgress,
		timeoutHardCap:           opts.TimeoutHardCap,
	}
}

// SetTimeoutOptions configures the stream watchdog deadline extension.
// Fork patch: batch 8.
func (a *sessionAgent) SetTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
	a.timeoutExtendsOnProgress = extendsOnProgress
	a.timeoutHardCap = hardCap
}

// logProviderWarnings emits each fantasy CallWarning from a step at WARN
// level. Without this, warnings such as malformed-tool-call input
// sanitization are silently dropped and never reach the logs. Optional
// fields (setting, tool, details) are attached only when present so the
// line stays terse for the common type+message case.
func logProviderWarnings(warnings []fantasy.CallWarning) {
	for _, w := range warnings {
		attrs := []any{"type", w.Type}
		if w.Message != "" {
			attrs = append(attrs, "message", w.Message)
		}
		if w.Setting != "" {
			attrs = append(attrs, "setting", w.Setting)
		}
		if w.Tool != nil && w.Tool.GetName() != "" {
			attrs = append(attrs, "tool", w.Tool.GetName())
		}
		if w.Details != "" {
			attrs = append(attrs, "details", w.Details)
		}
		slog.Warn("Provider warning", attrs...)
	}
}

func (a *sessionAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	if call.Prompt == "" && !message.ContainsTextAttachment(call.Attachments) {
		return nil, ErrEmptyPrompt
	}
	if call.SessionID == "" {
		return nil, ErrSessionMissing
	}

	// Queue the message if busy
	if a.IsSessionBusy(call.SessionID) {
		existing, ok := a.messageQueue.Get(call.SessionID)
		if !ok {
			existing = []SessionAgentCall{}
		}
		existing = append(existing, call)
		a.messageQueue.Set(call.SessionID, existing)
		return nil, nil
	}

	// Inter-process session lock. The IsSessionBusy check above is
	// per-process (in-memory map); two crush processes wouldn't see
	// each other's busy state and could both start streaming into the
	// same session id — the accidental-double-spawn race documented in
	// the parallel-process audit (#6 CRITICAL). The OS-level lock
	// auto-releases on process death, so a crashed holder never leaves
	// a stuck session id. Sub-agents skip the lock because they live
	// inside the parent process whose lock is already held.
	var ipcLock *session.SessionLock
	if !a.isSubAgent && a.dataDir != "" {
		lk, lockErr := session.TryAcquireSessionLock(a.dataDir, call.SessionID)
		if lockErr != nil {
			var busyErr *session.SessionLockBusyError
			if errors.As(lockErr, &busyErr) {
				slog.Warn(
					"agent.Run: rejected — session locked by another process",
					"session_id", call.SessionID,
					"holder_pid", busyErr.HolderPID,
					"lock_path", busyErr.Path,
				)
				return nil, fmt.Errorf("session %q is already in use: %w", call.SessionID, lockErr)
			}
			// Non-busy errors (IO, permission denied on locks dir) —
			// log and continue without the inter-process guard rather
			// than fail the whole run. The in-process IsSessionBusy
			// check still protects against intra-process races.
			slog.Warn("agent.Run: failed to acquire inter-process session lock, continuing without it",
				"session_id", call.SessionID, "err", lockErr)
		} else {
			ipcLock = lk
			defer func() {
				if relErr := ipcLock.Release(); relErr != nil {
					slog.Debug("agent.Run: release session lock failed", "session_id", call.SessionID, "err", relErr)
				}
			}()
		}
	}

	// Copy mutable fields under lock to avoid races with SetTools/SetModels.
	agentTools := a.tools.Copy()
	largeModel := a.largeModel.Get()
	systemPrompt := a.systemPrompt.Get()
	promptPrefix := a.systemPromptPrefix.Get()

	// Per-session system prompt overrides the global one when set.
	if call.SystemPromptOverride != "" {
		systemPrompt = call.SystemPromptOverride
	}

	slog.Info("SessionAgent.Run: starting", "sessionID", call.SessionID, "model", largeModel.ModelCfg.Model, "promptLen", len(systemPrompt))

	var instructions strings.Builder
	for _, server := range mcp.GetStates() {
		if server.State != mcp.StateConnected {
			continue
		}
		if s := server.Client.InitializeResult().Instructions; s != "" {
			instructions.WriteString(s)
			instructions.WriteString("\n\n")
		}
	}

	if s := instructions.String(); s != "" {
		systemPrompt += "\n\n<mcp-instructions>\n" + s + "\n</mcp-instructions>"
	}

	if len(agentTools) > 0 {
		// Add Anthropic caching to the last tool.
		agentTools[len(agentTools)-1].SetProviderOptions(a.getCacheControlOptions())
	}

	agent := fantasy.NewAgent(
		largeModel.Model,
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithTools(agentTools...),
		fantasy.WithUserAgent(userAgent),
	)

	sessionLock := sync.Mutex{}
	currentSession, err := a.sessions.Get(ctx, call.SessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return nil, fmt.Errorf("failed to get session messages: %w", err)
	}

	var wg sync.WaitGroup
	// Generate title if first message.
	if len(msgs) == 0 {
		titleCtx := ctx // Copy to avoid race with ctx reassignment below.
		wg.Go(func() {
			a.generateTitle(titleCtx, call.SessionID, call.Prompt)
		})
	}
	defer wg.Wait()

	// Add the user message to the session.
	_, err = a.createUserMessage(ctx, call)
	if err != nil {
		return nil, err
	}

	// Add the session to the context.
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, call.SessionID)
	ctx = context.WithValue(ctx, cliprovider.SessionIDContextKey, call.SessionID)
	ctx = context.WithValue(ctx, cliprovider.ReasoningEffortContextKey, currentSession.LargeModelReasoningEffort)

	genCtx, cancel := context.WithCancel(ctx)
	a.activeRequests.Set(call.SessionID, cancel)

	// Stream-progress watchdog (see streamWatchdog doc in stream_watchdog.go
	// for the invariant). Every fantasy stream callback below calls
	// bumpActivity(); if no callback fires for idleTimeout, the watchdog
	// cancels genCtx and the agent.Stream call below returns with
	// context.Canceled, routing into the error path that records
	// FinishReasonError("Stream stalled") on the assistant message.
	idleTimeout := streamIdleTimeoutDefault
	if a.streamIdleTimeout > 0 {
		idleTimeout = a.streamIdleTimeout
	}
	wd := startStreamWatchdog(
		genCtx, cancel, idleTimeout, streamWatchdogTick,
		func(idle time.Duration) {
			slog.Warn(
				"agent: stream watchdog firing — no provider activity, force-cancelling",
				"session_id", call.SessionID,
				"provider", largeModel.ModelCfg.Provider,
				"model", largeModel.ModelCfg.Model,
				"idle_duration", idle.String(),
				"threshold", idleTimeout.String(),
			)
		},
		a.timeoutExtendsOnProgress, // Fork patch: batch 8
		a.timeoutHardCap,           // Fork patch: batch 8
	)
	bumpActivity := wd.bump
	// toolStarted/toolFinished bracket tool execution so the watchdog pauses
	// its idle timer while a (possibly long) tool runs — see streamWatchdog.
	toolStarted := wd.toolStarted
	toolFinished := wd.toolFinished
	// Defer order matters: <-wd.done is deferred FIRST so it runs LAST
	// (LIFO), AFTER cancel() has signalled the goroutine to exit.
	// Without this the wait would deadlock the function return.
	defer func() { <-wd.done }()
	defer cancel()
	defer a.activeRequests.Del(call.SessionID)
	// Fork merge note (origin/main 6938dedd "perf: batch streaming message
	// updates"): upstream introduced a debounced flush layer in
	// message.Service. We removed that layer (see message/message.go fork
	// patch); our Notify() path goes through pubsub directly and Update()
	// writes synchronously, so there is nothing to flush here.

	history, files := a.preparePrompt(msgs, currentSession.Todos, call.Attachments...)

	var currentAssistant *message.Message
	var stepMessages []fantasy.Message
	var shouldSummarize bool

	// bgSummarizeLaunched ensures we launch at most one background
	// summarisation per Run() call (fired the first time we trim the window).
	var bgSummarizeLaunched bool

	// Fork patch: batch 8 — auto-checkpoint state for mid-stream
	// persistence. See CHANGELOG.fork.md section 6.
	//
	// Invariant: sessionLock (already declared above) protects
	// currentAssistant.Parts for all DB writes. The checkpoint
	// goroutine acquires sessionLock before reading Parts and
	// calling Update. The streaming callbacks that mutate Parts
	// (OnTextDelta, OnToolInputStart, etc.) also hold sessionLock
	// at their DB-write points. OnStepFinish drains the ticker
	// and stops the goroutine before its final write.
	var checkpointPartsLen int // last-flushed len(Parts), for coalescing
	checkpointDone := make(chan struct{})
	checkpointStarted := false
	startCheckpoint := func() {
		if a.checkpointInterval <= 0 || checkpointStarted {
			return
		}
		checkpointStarted = true
		go func() {
			defer close(checkpointDone)
			ticker := time.NewTicker(a.checkpointInterval)
			defer ticker.Stop()
			for {
				select {
				case <-genCtx.Done():
					return
				case <-ticker.C:
					sessionLock.Lock()
					if currentAssistant != nil && len(currentAssistant.Parts) != checkpointPartsLen {
						// Snapshot the current Parts into a clone
						// with a Partial Finish marker so the row
						// is recognisable as mid-stream on recovery.
						snap := currentAssistant.Clone()
						snap.AddFinish(message.FinishReasonUnknown, "", "")
						// Mark Partial on the just-added finish.
						for i := len(snap.Parts) - 1; i >= 0; i-- {
							if f, ok := snap.Parts[i].(message.Finish); ok {
								f.Partial = true
								snap.Parts[i] = f
								break
							}
						}
						if err := a.messages.Update(genCtx, snap); err != nil {
							slog.Debug(
								"agent: checkpoint flush failed",
								"session_id", call.SessionID,
								"message_id", snap.ID,
								"err", err,
							)
						} else {
							checkpointPartsLen = len(currentAssistant.Parts)
						}
					}
					sessionLock.Unlock()
				}
			}
		}()
	}
	stopCheckpoint := func() {
		if checkpointStarted {
			// The ticker goroutine exits on genCtx.Done (which
			// fires on cancel() or natural completion). Just wait.
			select {
			case <-checkpointDone:
			case <-time.After(5 * time.Second):
				// Defensive: don't block the critical path.
			}
		}
	}
	_ = stopCheckpoint // used in OnStepFinish below

	// latestMsgCh holds at most one pending UI snapshot (latest-value semantics).
	// A ticker goroutine drains it at ~20fps, decoupling the token arrival rate
	// from the bubbletea render rate so streaming is visible in the UI.
	latestMsgCh := make(chan message.Message, 1)
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-genCtx.Done():
				// Flush any final pending snapshot before exiting.
				select {
				case msg := <-latestMsgCh:
					a.messages.Notify(msg)
				default:
				}
				return
			case <-ticker.C:
				select {
				case msg := <-latestMsgCh:
					a.messages.Notify(msg)
				default:
				}
			}
		}
	}()

	// notifyUI enqueues the latest assistant snapshot for the ticker goroutine.
	// It never blocks: if the channel already has a pending snapshot, the old
	// one is discarded and replaced with the newest state.
	notifyUI := func() error {
		if currentAssistant == nil {
			return nil
		}
		msg := currentAssistant.Clone()
		select {
		case latestMsgCh <- msg:
		default:
			// Channel full — discard stale snapshot and enqueue fresh one.
			select {
			case <-latestMsgCh:
			default:
			}
			select {
			case latestMsgCh <- msg:
			default:
			}
		}
		return nil
	}

	// Fork patch: batch 8 — track final composition phase for forensic
	// logging. Set to true on each tool boundary; OnTextDelta checks and
	// resets it to emit at most once per step.
	sawToolBoundary := true

	// Don't send MaxOutputTokens if 0 — some providers (e.g. LM Studio) reject it
	var maxOutputTokens *int64
	if call.MaxOutputTokens > 0 {
		maxOutputTokens = &call.MaxOutputTokens
	}
	result, err := agent.Stream(genCtx, fantasy.AgentStreamCall{
		Prompt:           message.PromptWithTextAttachments(call.Prompt, call.Attachments),
		Files:            files,
		Messages:         history,
		ProviderOptions:  call.ProviderOptions,
		MaxOutputTokens:  maxOutputTokens,
		TopP:             call.TopP,
		Temperature:      call.Temperature,
		PresencePenalty:  call.PresencePenalty,
		TopK:             call.TopK,
		FrequencyPenalty: call.FrequencyPenalty,
		PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			// PrepareStep runs before the first token of the step and can
			// take non-trivial time (sliding-window trim, background
			// summarise kickoff, cache-control wiring). Bump first so a
			// slow prepare doesn't trip the watchdog before the stream
			// even starts.
			bumpActivity()
			prepared.Messages = options.Messages
			for i := range prepared.Messages {
				prepared.Messages[i].ProviderOptions = nil
			}

			// Use latest tools (updated by SetTools when MCP tools change).
			prepared.Tools = a.tools.Copy()

			queuedCalls, _ := a.messageQueue.Get(call.SessionID)
			a.messageQueue.Del(call.SessionID)
			for _, queued := range queuedCalls {
				userMessage, createErr := a.createUserMessage(callContext, queued)
				if createErr != nil {
					return callContext, prepared, createErr
				}
				prepared.Messages = append(prepared.Messages, userMessage.ToAIMessage()...)
			}

			// Drain InjectMessage queue: these rows are ALREADY in the DB
			// (persisted at click time by InjectMessage), so we only need
			// to splice them into the current prompt — no second Create
			// call, no duplicate rows in history.
			injected, _ := a.injectQueue.Get(call.SessionID)
			a.injectQueue.Del(call.SessionID)
			for _, inj := range injected {
				prepared.Messages = append(prepared.Messages, inj.ToAIMessage()...)
			}

			// Sliding-window context management: when the context is nearly
			// full, trim old messages so the agent can keep running without
			// blocking on a synchronous summarisation call.
			if !a.disableAutoSummarize {
				cw := int64(largeModel.CatwalkCfg.ContextWindow)
				if cw > 0 {
					usedTokens := currentSession.CompletionTokens + currentSession.PromptTokens
					remaining := cw - usedTokens
					var slideThreshold int64
					if cw > largeContextWindowThreshold {
						slideThreshold = largeContextWindowBuffer
					} else {
						slideThreshold = int64(float64(cw) * smallContextWindowRatio)
					}
					if remaining <= slideThreshold {
						targetTokens := int64(float64(cw) * contextSlideRatio)
						prepared.Messages = trimMessagesToWindow(prepared.Messages, targetTokens)

						// Silently compact the oldest 50% of messages in the
						// background so the main task keeps running uninterrupted.
						if !bgSummarizeLaunched {
							bgSummarizeLaunched = true
							bgCtx, bgCancel := context.WithTimeout(
								context.WithoutCancel(callContext),
								10*time.Minute,
							)
							bgOpts := call.ProviderOptions
							bgSessionID := call.SessionID
							go func() {
								defer bgCancel()
								if bgErr := a.runSummarizeSilent(bgCtx, bgSessionID, bgOpts); bgErr != nil {
									slog.Warn("background silent summarise failed", "session_id", bgSessionID, "err", bgErr)
								}
							}()
						}
					}
				}
			}

			prepared.Messages = a.workaroundProviderMediaLimitations(prepared.Messages, largeModel)

			lastSystemRoleInx := 0
			systemMessageUpdated := false
			for i, msg := range prepared.Messages {
				// Only add cache control to the last message.
				if msg.Role == fantasy.MessageRoleSystem {
					lastSystemRoleInx = i
				} else if !systemMessageUpdated {
					prepared.Messages[lastSystemRoleInx].ProviderOptions = a.getCacheControlOptions()
					systemMessageUpdated = true
				}
				// Than add cache control to the last 2 messages.
				if i > len(prepared.Messages)-3 {
					prepared.Messages[i].ProviderOptions = a.getCacheControlOptions()
				}
			}

			if promptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(promptPrefix)}, prepared.Messages...)
			}

			sessionLock.Lock()
			stepMessages = cloneFantasyMessages(prepared.Messages)
			sessionLock.Unlock()

			var assistantMsg message.Message
			assistantMsg, err = a.messages.Create(callContext, call.SessionID, message.CreateMessageParams{
				Role:            message.Assistant,
				Parts:           []message.ContentPart{},
				Model:           largeModel.ModelCfg.Model,
				Provider:        largeModel.ModelCfg.Provider,
				ReasoningEffort: currentSession.LargeModelReasoningEffort,
			})
			if err != nil {
				return callContext, prepared, err
			}
			callContext = context.WithValue(callContext, tools.MessageIDContextKey, assistantMsg.ID)
			callContext = context.WithValue(callContext, tools.SupportsImagesContextKey, largeModel.CatwalkCfg.SupportsImages)
			callContext = context.WithValue(callContext, tools.ModelNameContextKey, largeModel.CatwalkCfg.Name)
			currentAssistant = &assistantMsg
			return callContext, prepared, err
		},
		OnReasoningStart: func(id string, reasoning fantasy.ReasoningContent) error {
			bumpActivity()
			slog.Debug("agent: OnReasoningStart called", "id", id)
			currentAssistant.AppendReasoningContent(reasoning.Text)
			return a.messages.Update(genCtx, *currentAssistant)
		},
		OnReasoningDelta: func(id string, text string) error {
			bumpActivity()
			slog.Debug("agent: OnReasoningDelta called", "len", len(text))
			currentAssistant.AppendReasoningContent(text)
			return notifyUI()
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			bumpActivity()
			// handle anthropic signature
			if anthropicData, ok := reasoning.ProviderMetadata[anthropic.Name]; ok {
				if reasoning, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok {
					currentAssistant.AppendReasoningSignature(reasoning.Signature)
				}
			}
			if googleData, ok := reasoning.ProviderMetadata[google.Name]; ok {
				if reasoning, ok := googleData.(*google.ReasoningMetadata); ok {
					currentAssistant.AppendThoughtSignature(reasoning.Signature, reasoning.ToolID)
				}
			}
			if openaiData, ok := reasoning.ProviderMetadata[openai.Name]; ok {
				if reasoning, ok := openaiData.(*openai.ResponsesReasoningMetadata); ok {
					currentAssistant.SetReasoningResponsesData(reasoning)
				}
			}
			currentAssistant.FinishThinking()
			return a.messages.Update(genCtx, *currentAssistant)
		},
		OnTextDelta: func(id string, text string) error {
			bumpActivity()
			// Fork patch: batch 8 — start the checkpoint ticker on the
			// first text delta of this step (lazily, once only).
			startCheckpoint()
			// Fork patch: batch 8 — emit final-composition log at most
			// once per step, on the first text delta after a tool boundary.
			if sawToolBoundary && currentAssistant != nil {
				sawToolBoundary = false
				sessionLock.Lock()
				slog.Info(
					"agent: final composition started",
					"session_id", call.SessionID,
					"message_id", currentAssistant.ID,
					"chars_in_message_so_far", len(currentAssistant.FullText()),
				)
				sessionLock.Unlock()
			}
			// Strip leading newline from initial text content. This is is
			// particularly important in non-interactive mode where leading
			// newlines are very visible.
			if len(currentAssistant.Parts) == 0 {
				text = strings.TrimPrefix(text, "\n")
			}

			currentAssistant.AppendContent(text)
			return notifyUI()
		},
		OnToolInputStart: func(id string, toolName string) error {
			bumpActivity()
			sawToolBoundary = true // Fork patch: batch 8
			toolCall := message.ToolCall{
				ID:               id,
				Name:             toolName,
				ProviderExecuted: false,
				Finished:         false,
			}
			currentAssistant.AddToolCall(toolCall)
			// Use parent ctx instead of genCtx to ensure the update succeeds
			// even if the request is canceled mid-stream
			return a.messages.Update(ctx, *currentAssistant)
		},
		OnToolInputDelta: func(id string, delta string) error {
			bumpActivity()
			currentAssistant.AppendToolCallInput(id, delta)
			return nil // don't spam DB on every delta; ToolInputEnd will persist
		},
		OnToolInputEnd: func(id string) error {
			bumpActivity()
			currentAssistant.FinishToolCall(id)
			return a.messages.Update(genCtx, *currentAssistant)
		},
		OnRetry: func(err *fantasy.ProviderError, delay time.Duration) {
			bumpActivity()
			slog.Warn("Provider request failed, retrying", providerRetryLogFields(err, delay)...)
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			bumpActivity()
			// A tool is about to execute — pause the stall watchdog until its
			// result arrives (OnToolResult). fantasy fires every OnToolCall
			// for a step before executing any tool, so the counter brackets
			// the whole executeTools window.
			toolStarted()
			sawToolBoundary = true // Fork patch: batch 8
			toolCall := message.ToolCall{
				ID:               tc.ToolCallID,
				Name:             tc.ToolName,
				Input:            tc.Input,
				ProviderExecuted: false,
				Finished:         true,
			}
			currentAssistant.AddToolCall(toolCall)
			// Use parent ctx instead of genCtx to ensure the update succeeds
			// even if the request is canceled mid-stream
			return a.messages.Update(ctx, *currentAssistant)
		},
		OnToolResult: func(result fantasy.ToolResultContent) error {
			bumpActivity()
			// Tool finished — resume the stall watchdog (and restart its idle
			// window so the tool's runtime isn't counted against the provider).
			toolFinished()
			sawToolBoundary = true // Fork patch: batch 8
			toolResult := a.convertToToolResult(result)
			// Use parent ctx instead of genCtx to ensure the message is created
			// even if the request is canceled mid-stream
			_, createMsgErr := a.messages.Create(ctx, currentAssistant.SessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					toolResult,
				},
			})
			return createMsgErr
		},
		OnStepFinish: func(stepResult fantasy.StepResult) error {
			bumpActivity()
			// Surface provider CallWarnings (malformed tool-call sanitization,
			// unsupported settings, etc.) that fantasy otherwise discards
			// silently. Visible in logs only — does not interrupt the turn.
			logProviderWarnings(stepResult.Warnings)
			// Fork patch: batch 8 — stop the checkpoint ticker BEFORE the
			// final write so the ticker doesn't race with OnStepFinish.
			stopCheckpoint()
			sawToolBoundary = true // Fork patch: batch 8 — reset for next step
			finishReason := message.FinishReasonUnknown
			switch stepResult.FinishReason {
			case fantasy.FinishReasonLength:
				finishReason = message.FinishReasonMaxTokens
			case fantasy.FinishReasonStop:
				finishReason = message.FinishReasonEndTurn
			case fantasy.FinishReasonToolCalls:
				finishReason = message.FinishReasonToolUse
			}
			// If a tool result halted the turn (e.g. a hook halt or a
			// permission denial), the step ends on FinishReasonToolCalls but
			// the model will not be called again. Treat it as the end of the
			// turn so the UI can render the assistant footer.
			if finishReason == message.FinishReasonToolUse {
				for _, tr := range stepResult.Content.ToolResults() {
					if tr.StopTurn {
						finishReason = message.FinishReasonEndTurn
						break
					}
				}
			}
			// Fork patch: surface empty-stream as a visible error.
			// Some providers (e.g. z.ai) sometimes close the stream without
			// sending any content (no text, no tool_call, no reasoning) and
			// without an explicit finish reason. The upstream code records this
			// as FinishReasonUnknown with empty parts, which the WUI renders as
			// a blank assistant block — looking like a session lockup. Convert
			// this case to an error so both the WUI fallback and the user see
			// an actionable message. See CHANGELOG.fork.md section 4.D.
			if finishReason == message.FinishReasonUnknown &&
				currentAssistant.FullText() == "" &&
				currentAssistant.ReasoningContent().Thinking == "" &&
				len(currentAssistant.ToolCalls()) == 0 {
				slog.Warn(
					"agent: empty stream from provider — recording as error",
					"sessionID", call.SessionID,
					"provider", largeModel.ModelCfg.Provider,
					"model", largeModel.ModelCfg.Model,
				)
				currentAssistant.AddFinish(
					message.FinishReasonError,
					"Empty response",
					fmt.Sprintf(
						"Provider %q closed the stream for model %q without returning any content. This is usually a transient provider/network issue — please retry.",
						largeModel.ModelCfg.Provider, largeModel.ModelCfg.Model,
					),
				)
			} else {
				currentAssistant.AddFinish(finishReason, "", "")
			}
			// Drain any pending UI snapshot so the ticker goroutine does not
			// publish a stale state after messages.Update writes the final one.
			select {
			case <-latestMsgCh:
			default:
			}
			sessionLock.Lock()
			defer sessionLock.Unlock()

			updatedSession, getSessionErr := a.sessions.Get(ctx, call.SessionID)
			if getSessionErr != nil {
				return getSessionErr
			}
			// Fork merge note (origin/main 6ed8852b "fix(agent): estimate
			// missing streamed usage"): if the provider omits the final
			// usage chunk, use upstream's token estimator so our sliding
			// context window stays accurate. We drop the "estimated" flag
			// (TUI marker — see CHANGELOG.fork.md Section 2).
			usage, _ := fallbackStepUsage(stepMessages, stepResult)
			costDelta := a.updateSessionUsage(largeModel, &updatedSession, usage, a.openrouterCost(stepResult.ProviderMetadata))
			if costDelta != 0 {
				if _, costErr := a.sessions.IncrementCost(ctx, updatedSession.ID, costDelta); costErr != nil {
					return costErr
				}
			}
			_, sessionErr := a.sessions.Save(ctx, updatedSession)
			if sessionErr != nil {
				return sessionErr
			}
			currentSession = updatedSession

			// Fork patch: batch 30 — cancel + runaway protection.
			// Check DB cancel flag (cross-process signal) and cost/token caps.
			if canc, cancErr := a.sessions.IsCancelRequested(ctx, call.SessionID); cancErr == nil && canc {
				if cancelFn, ok := a.activeRequests.Get(call.SessionID); ok {
					cancelFn()
				}
				return fmt.Errorf("session %s cancelled by user", call.SessionID)
			}
			if call.MaxCost > 0 && updatedSession.Cost > call.MaxCost {
				slog.Warn(
					"agent: aborting — max-cost exceeded",
					"session_id", call.SessionID,
					"cost", updatedSession.Cost,
					"max", call.MaxCost,
				)
				if cancelFn, ok := a.activeRequests.Get(call.SessionID); ok {
					cancelFn()
				}
				return fmt.Errorf("session %s aborted: cost $%.4f exceeds max $%.4f",
					call.SessionID, updatedSession.Cost, call.MaxCost)
			}
			totalTokens := updatedSession.PromptTokens + updatedSession.CompletionTokens
			if call.MaxTokens > 0 && totalTokens > call.MaxTokens {
				slog.Warn(
					"agent: aborting — max-tokens exceeded",
					"session_id", call.SessionID,
					"tokens", totalTokens,
					"max", call.MaxTokens,
				)
				if cancelFn, ok := a.activeRequests.Get(call.SessionID); ok {
					cancelFn()
				}
				return fmt.Errorf("session %s aborted: %d tokens exceeds max %d",
					call.SessionID, totalTokens, call.MaxTokens)
			}

			return a.messages.Update(genCtx, *currentAssistant)
		},
		StopWhen: []fantasy.StopCondition{
			func(_ []fantasy.StepResult) bool {
				cw := int64(largeModel.CatwalkCfg.ContextWindow)
				// If context window is unknown (0), skip auto-summarize
				// to avoid immediately truncating custom/local models.
				if cw == 0 {
					return false
				}
				tokens := currentSession.CompletionTokens + currentSession.PromptTokens
				remaining := cw - tokens
				var threshold int64
				if cw > largeContextWindowThreshold {
					threshold = largeContextWindowBuffer
				} else {
					threshold = int64(float64(cw) * smallContextWindowRatio)
				}
				if (remaining <= threshold) && !a.disableAutoSummarize {
					shouldSummarize = true
					return true
				}
				return false
			},
			func(steps []fantasy.StepResult) bool {
				return hasRepeatedToolCalls(steps, loopDetectionWindowSize, loopDetectionMaxRepeats)
			},
		},
	})
	if err != nil {
		isHyper := largeModel.ModelCfg.Provider == hyper.Name
		isCancelErr := errors.Is(err, context.Canceled)
		isWatchdogStall := isCancelErr && wd.stalled.Load()
		if currentAssistant == nil {
			return result, err
		}
		// All DB writes in the error path use a detached context. The outer
		// ctx may itself be cancelled — in `crush run` it's the
		// signal.NotifyContext from fang, so Ctrl-C cancels it too; in the
		// web UI a request abort cancels it; the stream watchdog above
		// cancels genCtx (whose parent is ctx, so it doesn't cancel ctx,
		// but defensively we still detach). Without a detached ctx the
		// finish part Update fails with context.Canceled and the assistant
		// ends up half-saved in the DB — the "silent dying" pattern
		// observed in 162-promise-all. Codec must surface control: the
		// finish part MUST land on disk before we return.
		flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer flushCancel()
		// Ensure we finish thinking on error to close the reasoning state.
		currentAssistant.FinishThinking()
		toolCalls := currentAssistant.ToolCalls()
		msgs, createErr := a.messages.List(flushCtx, currentAssistant.SessionID)
		if createErr != nil {
			return nil, createErr
		}
		for _, tc := range toolCalls {
			if !tc.Finished {
				tc.Finished = true
				tc.Input = "{}"
				currentAssistant.AddToolCall(tc)
				updateErr := a.messages.Update(flushCtx, *currentAssistant)
				if updateErr != nil {
					return nil, updateErr
				}
			}

			found := false
			for _, msg := range msgs {
				if msg.Role == message.Tool {
					for _, tr := range msg.ToolResults() {
						if tr.ToolCallID == tc.ID {
							found = true
							break
						}
					}
				}
				if found {
					break
				}
			}
			if found {
				continue
			}
			content := "There was an error while executing the tool"
			if isWatchdogStall {
				content = fmt.Sprintf("Tool call was cancelled: the provider stream stalled for >%s and the watchdog aborted the turn.", idleTimeout)
			} else if isCancelErr {
				content = "Error: user cancelled assistant tool calling"
			}
			toolResult := message.ToolResult{
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    content,
				IsError:    true,
			}
			_, createErr = a.messages.Create(flushCtx, currentAssistant.SessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					toolResult,
				},
			})
			if createErr != nil {
				return nil, createErr
			}
		}
		var fantasyErr *fantasy.Error
		var providerErr *fantasy.ProviderError
		const defaultTitle = "Provider Error"
		if isWatchdogStall {
			// Close the observability loop: the watchdog goroutine already
			// emitted its slog.Warn at fire-time, but a log reader
			// chasing the trail needs to see that the stall actually
			// made it into the user-visible finish part on this session.
			slog.Info(
				"agent: watchdog stall surfaced as FinishReasonError",
				"session_id", call.SessionID,
				"provider", largeModel.ModelCfg.Provider,
			)
			currentAssistant.AddFinish(
				message.FinishReasonError,
				"Stream stalled",
				fmt.Sprintf(
					"Provider %q stopped sending streaming data for over %s — the request was auto-cancelled by the stream watchdog. Retry the prompt; if it keeps happening, try a different model or provider.",
					largeModel.ModelCfg.Provider, idleTimeout,
				),
			)
		} else if isCancelErr {
			currentAssistant.AddFinish(message.FinishReasonCanceled, "User canceled request", "")
		} else if isHyper && errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized {
			currentAssistant.AddFinish(message.FinishReasonError, "Unauthorized", `Please re-authenticate with Hyper. You can also run "crush auth" to re-authenticate.`)
		} else if isHyper && errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusPaymentRequired {
			url := hyper.BaseURL()
			currentAssistant.AddFinish(message.FinishReasonError, "No credits", "You're out of credits. Add more at "+url)
		} else if errors.As(err, &providerErr) {
			if providerErr.Message == "The requested model is not supported." {
				url := "https://github.com/settings/copilot/features"
				currentAssistant.AddFinish(
					message.FinishReasonError,
					"Copilot model not enabled",
					fmt.Sprintf("%q is not enabled in Copilot. Go to the following page to enable it. Then, wait 5 minutes before trying again. %s", largeModel.CatwalkCfg.Name, url),
				)
			} else {
				currentAssistant.AddFinish(message.FinishReasonError, cmp.Or(stringext.Capitalize(providerErr.Title), defaultTitle), providerErr.Message)
			}
		} else if errors.As(err, &fantasyErr) {
			currentAssistant.AddFinish(message.FinishReasonError, cmp.Or(stringext.Capitalize(fantasyErr.Title), defaultTitle), fantasyErr.Message)
		} else {
			currentAssistant.AddFinish(message.FinishReasonError, defaultTitle, err.Error())
		}
		// Detached flush (flushCtx is context.WithoutCancel + 15s timeout,
		// created at the top of this error block). This is the call that
		// MUST land on disk — without it the assistant message has tool
		// calls but no finish part, and the WUI/recovery sees it as still
		// in-flight forever.
		updateErr := a.messages.Update(flushCtx, *currentAssistant)
		if updateErr != nil {
			slog.Error(
				"agent: failed to persist final finish part",
				"session_id", call.SessionID,
				"err", updateErr,
			)
			return nil, updateErr
		}

		// Drain the message queue on cancel. The "interrupt and send" web
		// flow queues a user message and then cancels the turn; without
		// this drain that message would sit in the queue until another
		// /send arrives. Release the active-request slot and the goroutine
		// cancel func first so the recursive Run() doesn't see the session
		// as busy.
		if isCancelErr {
			if queuedMessages, ok := a.messageQueue.Get(call.SessionID); ok && len(queuedMessages) > 0 {
				firstQueuedMessage := queuedMessages[0]
				a.messageQueue.Set(call.SessionID, queuedMessages[1:])
				a.activeRequests.Del(call.SessionID)
				cancel()
				return a.Run(ctx, firstQueuedMessage)
			}
		}
		return nil, err
	}

	if shouldSummarize {
		a.activeRequests.Del(call.SessionID)
		if summarizeErr := a.Summarize(genCtx, call.SessionID, call.ProviderOptions); summarizeErr != nil {
			return nil, summarizeErr
		}
		// If the agent wasn't done...
		if len(currentAssistant.ToolCalls()) > 0 {
			existing, ok := a.messageQueue.Get(call.SessionID)
			if !ok {
				existing = []SessionAgentCall{}
			}
			call.Prompt = fmt.Sprintf("The previous session was interrupted because it got too long, the initial user request was: `%s`", call.Prompt)
			existing = append(existing, call)
			a.messageQueue.Set(call.SessionID, existing)
		}
	}

	// Release active request before processing queued messages.
	a.activeRequests.Del(call.SessionID)
	cancel()

	// Send notification that agent has finished its turn (skip for
	// nested/non-interactive sessions).
	if !call.NonInteractive && a.notify != nil {
		a.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			SessionID:    call.SessionID,
			SessionTitle: currentSession.Title,
			Type:         notify.TypeAgentFinished,
		})
	}

	queuedMessages, ok := a.messageQueue.Get(call.SessionID)
	if !ok || len(queuedMessages) == 0 {
		return result, err
	}
	// There are queued messages restart the loop.
	firstQueuedMessage := queuedMessages[0]
	a.messageQueue.Set(call.SessionID, queuedMessages[1:])
	return a.Run(ctx, firstQueuedMessage)
}

// ErrSummarizeQueued is returned by Summarize when the session is busy and
// the request has been queued for execution after the current task finishes.
var ErrSummarizeQueued = errors.New("summarize queued")

func (a *sessionAgent) Summarize(ctx context.Context, sessionID string, opts fantasy.ProviderOptions) error {
	if a.IsSessionBusy(sessionID) {
		a.summarizeQueue.Set(sessionID, opts)
		return ErrSummarizeQueued
	}
	return a.runSummarize(ctx, sessionID, opts)
}

func (a *sessionAgent) SummarizeQueued(sessionID string) bool {
	_, ok := a.summarizeQueue.Get(sessionID)
	return ok
}

func (a *sessionAgent) TakeSummarizeQueue(sessionID string) (fantasy.ProviderOptions, bool) {
	opts, ok := a.summarizeQueue.Take(sessionID)
	return opts, ok
}

func (a *sessionAgent) CancelQueuedSummarize(sessionID string) {
	a.summarizeQueue.Del(sessionID)
}

// runSummarize performs the actual summarisation without a busy-check.
// It uses the sessionID+"-summarize" key in activeRequests so it can run
// concurrently with a regular Run() call on the same session.
func (a *sessionAgent) runSummarize(ctx context.Context, sessionID string, opts fantasy.ProviderOptions) error {
	// Copy mutable fields under lock to avoid races with SetModels.
	largeModel := a.largeModel.Get()
	systemPromptPrefix := a.systemPromptPrefix.Get()

	currentSession, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		// Nothing to summarize.
		return nil
	}

	aiMsgs, _ := a.preparePrompt(msgs, nil)

	summarizeKey := sessionID + "-summarize"
	genCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	a.activeRequests.Set(summarizeKey, cancel)
	defer a.activeRequests.Del(summarizeKey)
	defer cancel()
	// Fork merge note: FlushAll deleted with the debounced layer — see the
	// Run() entry point above for context.

	agent := fantasy.NewAgent(
		largeModel.Model,
		fantasy.WithSystemPrompt(string(summaryPrompt)),
		fantasy.WithUserAgent(userAgent),
	)
	summaryMessage, err := a.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		Model:            largeModel.Model.Model(),
		Provider:         largeModel.Model.Provider(),
		ReasoningEffort:  currentSession.LargeModelReasoningEffort,
		IsSummaryMessage: true,
	})
	if err != nil {
		return err
	}

	summaryPromptText := buildSummaryPrompt(currentSession.Todos)

	resp, err := agent.Stream(genCtx, fantasy.AgentStreamCall{
		Prompt:          summaryPromptText,
		Messages:        aiMsgs,
		ProviderOptions: opts,
		PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = options.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(systemPromptPrefix)}, prepared.Messages...)
			}
			return callContext, prepared, nil
		},
		OnReasoningDelta: func(id string, text string) error {
			summaryMessage.AppendReasoningContent(text)
			return a.messages.Update(genCtx, summaryMessage)
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			// Handle anthropic signature.
			if anthropicData, ok := reasoning.ProviderMetadata["anthropic"]; ok {
				if signature, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok && signature.Signature != "" {
					summaryMessage.AppendReasoningSignature(signature.Signature)
				}
			}
			summaryMessage.FinishThinking()
			return a.messages.Update(genCtx, summaryMessage)
		},
		OnTextDelta: func(id, text string) error {
			summaryMessage.AppendContent(text)
			return a.messages.Update(genCtx, summaryMessage)
		},
	})
	if err != nil {
		isCancelErr := errors.Is(err, context.Canceled)
		if isCancelErr {
			// User cancelled summarize we need to remove the summary message.
			deleteErr := a.messages.Delete(ctx, summaryMessage.ID)
			return deleteErr
		}
		// Mark the summary message as finished with an error so the UI
		// stops spinning.
		summaryMessage.AddFinish(message.FinishReasonError, "Summarization Error", err.Error())
		if updateErr := a.messages.Update(ctx, summaryMessage); updateErr != nil {
			return updateErr
		}
		return err
	}

	summaryMessage.AddFinish(message.FinishReasonEndTurn, "", "")
	err = a.messages.Update(genCtx, summaryMessage)
	if err != nil {
		return err
	}

	var openrouterCost *float64
	for _, step := range resp.Steps {
		stepCost := a.openrouterCost(step.ProviderMetadata)
		if stepCost != nil {
			newCost := *stepCost
			if openrouterCost != nil {
				newCost += *openrouterCost
			}
			openrouterCost = &newCost
		}
	}

	// Re-fetch the session to pick up any user edits (e.g. todo changes) that
	// happened while the summary was streaming, then overlay our own fields.
	freshSession, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to re-fetch session before save: %w", err)
	}
	costDelta := a.updateSessionUsage(largeModel, &freshSession, resp.TotalUsage, openrouterCost)
	if costDelta != 0 {
		if _, costErr := a.sessions.IncrementCost(genCtx, freshSession.ID, costDelta); costErr != nil {
			return costErr
		}
	}

	// Just in case, get just the last usage info. Use upstream's
	// summaryCompletionTokens helper (origin/main 6ed8852b) so we fall back
	// to an approximate count when the provider omits OutputTokens on the
	// summary stream's final usage chunk.
	usage := resp.Response.Usage
	freshSession.SummaryMessageID = summaryMessage.ID
	freshSession.CompletionTokens = summaryCompletionTokens(usage, summaryMessage)
	freshSession.PromptTokens = 0
	if _, err = a.sessions.Save(genCtx, freshSession); err != nil {
		return err
	}

	// Fork merge note (origin/main 61f49b23 "drain queued messages after manual
	// session summarize"): upstream added this drain to keep the user's queued
	// messages flowing after a manual /compact. Our Summarize() outer wrapper
	// uses a separate summarizeQueue keyed by sessionID, so the busy state
	// here is the "-summarize" key — releasing it does NOT release the main
	// Run()'s lock. The drain below runs only if a user message landed in
	// messageQueue during summarisation.
	a.activeRequests.Del(sessionID + "-summarize")
	cancel()
	queuedMessages, ok := a.messageQueue.Get(sessionID)
	if !ok || len(queuedMessages) == 0 {
		return nil
	}
	firstQueuedMessage := queuedMessages[0]
	a.messageQueue.Set(sessionID, queuedMessages[1:])
	_, qErr := a.Run(ctx, firstQueuedMessage)
	return qErr
}

// runSummarizeSilent compacts the oldest half of the session's messages in
// the background without any visible change in the UI. It:
//  1. Loads all current messages, splits them at the midpoint.
//  2. Sends the older half to the LLM for summarisation.
//  3. Creates a hidden summary message (not rendered in the UI).
//  4. Deletes all non-pinned messages that were summarised.
//  5. Updates session.SummaryMessageID so future runs start from the summary.
//
// Pinned messages are never deleted.
func (a *sessionAgent) runSummarizeSilent(ctx context.Context, sessionID string, opts fantasy.ProviderOptions) error {
	largeModel := a.largeModel.Get()
	systemPromptPrefix := a.systemPromptPrefix.Get()

	currentSession, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return err
	}
	if len(msgs) < 4 {
		// Too few messages to bother summarising.
		return nil
	}

	// Split at midpoint: summarise the older half.
	mid := len(msgs) / 2
	oldMsgs := msgs[:mid]
	// Separate pinned from non-pinned in the old half.
	var toSummarise, pinnedOld []message.Message
	for _, m := range oldMsgs {
		if m.Pinned {
			pinnedOld = append(pinnedOld, m)
		} else {
			toSummarise = append(toSummarise, m)
		}
	}
	if len(toSummarise) == 0 {
		return nil
	}

	aiMsgs, _ := a.preparePrompt(toSummarise, nil)

	summarizeKey := sessionID + "-summarize"
	genCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	a.activeRequests.Set(summarizeKey, cancel)
	defer a.activeRequests.Del(summarizeKey)
	defer cancel()

	agent := fantasy.NewAgent(
		largeModel.Model,
		fantasy.WithSystemPrompt(string(summaryPrompt)),
	)
	// Create the summary message as hidden so it is invisible in the UI.
	summaryMessage, err := a.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		Model:            largeModel.Model.Model(),
		Provider:         largeModel.Model.Provider(),
		ReasoningEffort:  currentSession.LargeModelReasoningEffort,
		IsSummaryMessage: true,
		Hidden:           true,
	})
	if err != nil {
		return err
	}

	summaryPromptText := buildSummaryPrompt(currentSession.Todos)
	resp, err := agent.Stream(genCtx, fantasy.AgentStreamCall{
		Prompt:          summaryPromptText,
		Messages:        aiMsgs,
		ProviderOptions: opts,
		PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = options.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(systemPromptPrefix)}, prepared.Messages...)
			}
			return callContext, prepared, nil
		},
		OnReasoningDelta: func(id string, text string) error {
			summaryMessage.AppendReasoningContent(text)
			return a.messages.Update(genCtx, summaryMessage)
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			if anthropicData, ok := reasoning.ProviderMetadata["anthropic"]; ok {
				if signature, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok && signature.Signature != "" {
					summaryMessage.AppendReasoningSignature(signature.Signature)
				}
			}
			summaryMessage.FinishThinking()
			return a.messages.Update(genCtx, summaryMessage)
		},
		OnTextDelta: func(id, text string) error {
			summaryMessage.AppendContent(text)
			return a.messages.Update(genCtx, summaryMessage)
		},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			_ = a.messages.Delete(ctx, summaryMessage.ID)
		}
		return err
	}

	summaryMessage.AddFinish(message.FinishReasonEndTurn, "", "")
	if err = a.messages.Update(genCtx, summaryMessage); err != nil {
		return err
	}

	// Delete the non-pinned old messages that were replaced by the summary.
	for _, m := range toSummarise {
		if delErr := a.messages.Delete(ctx, m.ID); delErr != nil {
			slog.Warn("silent summarise: failed to delete old message", "id", m.ID, "err", delErr)
		}
	}
	_ = pinnedOld // pinned messages stay in the DB untouched

	// Update session: point SummaryMessageID to the new hidden summary and
	// reset token counters so the next call gets an accurate remaining-context
	// estimate.
	freshSession, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("silent summarise: failed to re-fetch session: %w", err)
	}
	var openrouterCost *float64
	for _, step := range resp.Steps {
		if stepCost := a.openrouterCost(step.ProviderMetadata); stepCost != nil {
			if openrouterCost == nil {
				openrouterCost = new(float64)
			}
			*openrouterCost += *stepCost
		}
	}
	costDelta := a.updateSessionUsage(largeModel, &freshSession, resp.TotalUsage, openrouterCost)
	if costDelta != 0 {
		if _, costErr := a.sessions.IncrementCost(genCtx, freshSession.ID, costDelta); costErr != nil {
			return costErr
		}
	}
	freshSession.SummaryMessageID = summaryMessage.ID
	freshSession.CompletionTokens = resp.Response.Usage.OutputTokens
	freshSession.PromptTokens = 0
	_, err = a.sessions.Save(genCtx, freshSession)
	return err
}

func (a *sessionAgent) getCacheControlOptions() fantasy.ProviderOptions {
	if t, _ := strconv.ParseBool(os.Getenv("CRUSH_DISABLE_ANTHROPIC_CACHE")); t {
		return fantasy.ProviderOptions{}
	}
	return fantasy.ProviderOptions{
		anthropic.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
		bedrock.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
		vercel.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
	}
}

func (a *sessionAgent) createUserMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	parts := []message.ContentPart{message.TextContent{Text: call.Prompt}}
	var attachmentParts []message.ContentPart
	for _, attachment := range call.Attachments {
		attachmentParts = append(attachmentParts, message.BinaryContent{Path: attachment.FilePath, MIMEType: attachment.MimeType, Data: attachment.Content})
	}
	parts = append(parts, attachmentParts...)
	msg, err := a.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: parts,
	})
	if err != nil {
		return message.Message{}, fmt.Errorf("failed to create user message: %w", err)
	}
	return msg, nil
}

func (a *sessionAgent) preparePrompt(msgs []message.Message, todos []session.Todo, attachments ...message.Attachment) ([]fantasy.Message, []fantasy.FilePart) {
	var history []fantasy.Message
	if !a.isSubAgent {
		// Fork merge note: we already extended this block to also surface the
		// CURRENT todo list when non-empty (originally only handled empty).
		// Upstream's small reword to the empty-case text is not worth the churn.
		var reminderText string
		if len(todos) == 0 {
			reminderText = `This is a reminder that your todo list is currently empty — all previous tasks have been completed or deleted. DO NOT recreate any old tasks from memory. DO NOT mention this to the user explicitly because they are already aware.
If you are working on tasks that would benefit from a todo list please use the "todos" tool to create one.
If not, please feel free to ignore. Again do not mention this message to the user.`
		} else {
			var sb strings.Builder
			sb.WriteString("This is a reminder of your CURRENT todo list. This is the authoritative ground truth — it overrides anything in your conversation history:\n\n")
			for _, t := range todos {
				fmt.Fprintf(&sb, "- [%s] %s\n", t.Status, t.Content)
			}
			sb.WriteString("\nIMPORTANT: Tasks NOT in this list have been DELETED (by the user or by you). Do NOT add them back. Only manage the tasks listed above, plus any new ones the user explicitly requests. DO NOT mention this reminder to the user.")
			reminderText = sb.String()
		}
		history = append(history, fantasy.NewUserMessage(
			fmt.Sprintf("<system_reminder>%s</system_reminder>", reminderText),
		))
	}
	// Collect all tool call IDs present in assistant messages and all tool
	// result IDs present in tool messages. This lets us detect both orphaned
	// tool results (result without a call) and orphaned tool calls (call
	// without a result).
	knownToolCallIDs := make(map[string]struct{})
	knownToolResultIDs := make(map[string]struct{})
	for _, m := range msgs {
		switch m.Role {
		case message.Assistant:
			for _, tc := range m.ToolCalls() {
				knownToolCallIDs[tc.ID] = struct{}{}
			}
		case message.Tool:
			for _, tr := range m.ToolResults() {
				knownToolResultIDs[tr.ToolCallID] = struct{}{}
			}
		}
	}

	for _, m := range msgs {
		if len(m.Parts) == 0 {
			continue
		}
		// Assistant message without content or tool calls (cancelled before it returned anything).
		if m.Role == message.Assistant && len(m.ToolCalls()) == 0 && m.Content().Text == "" && m.ReasoningContent().String() == "" {
			continue
		}
		if m.Role == message.Tool {
			if msg, ok := filterOrphanedToolResults(m, knownToolCallIDs); ok {
				history = append(history, msg)
			}
			continue
		}
		aiMsgs := m.ToAIMessage()
		// Fork merge note (origin/main 6d95ecc5 "skip image attachments in
		// history when model doesn't support them"): we intentionally skip
		// upstream's per-message filter here — the same scrub happens in
		// workaroundProviderMediaLimitations() which runs once per Stream
		// call inside PrepareStep, so doing it twice would just walk the
		// history twice.
		history = append(history, aiMsgs...)

		if m.Role == message.Assistant {
			if msg, ok := syntheticToolResultsForOrphanedCalls(m, knownToolResultIDs); ok {
				history = append(history, msg)
			}
		}
	}

	var files []fantasy.FilePart
	for _, attachment := range attachments {
		if attachment.IsText() {
			continue
		}
		files = append(files, fantasy.FilePart{
			Filename:  attachment.FileName,
			Data:      attachment.Content,
			MediaType: attachment.MimeType,
		})
	}

	return history, files
}

// filterFileParts removes fantasy.FilePart entries from a slice of message
// parts. Used to strip image attachments from historical user messages when
// the current model does not support them.
func filterFileParts(parts []fantasy.MessagePart) []fantasy.MessagePart {
	filtered := make([]fantasy.MessagePart, 0, len(parts))
	for _, part := range parts {
		if _, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
			continue
		}
		filtered = append(filtered, part)
	}
	return filtered
}

// filterOrphanedToolResults converts a tool message to a fantasy.Message,
// dropping any tool result parts whose tool_call_id has no matching tool call
// in the known set. An orphaned result causes API validation to fail on every
// subsequent turn, permanently locking the session. Returns the filtered
// message and true if at least one valid part remains.
func filterOrphanedToolResults(m message.Message, knownToolCallIDs map[string]struct{}) (fantasy.Message, bool) {
	aiMsgs := m.ToAIMessage()
	if len(aiMsgs) == 0 {
		return fantasy.Message{}, false
	}
	var validParts []fantasy.MessagePart
	for _, part := range aiMsgs[0].Content {
		tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
		if !ok {
			validParts = append(validParts, part)
			continue
		}
		if _, known := knownToolCallIDs[tr.ToolCallID]; known {
			validParts = append(validParts, part)
		} else {
			slog.Warn(
				"Dropping orphaned tool result with no matching tool call",
				"tool_call_id", tr.ToolCallID,
			)
		}
	}
	if len(validParts) == 0 {
		return fantasy.Message{}, false
	}
	msg := aiMsgs[0]
	msg.Content = validParts
	return msg, true
}

// syntheticToolResultsForOrphanedCalls returns a tool message containing
// synthetic tool results for any tool calls in the assistant message that
// have no matching result in knownToolResultIDs. LLM APIs require every
// tool_use to be immediately followed by a tool_result; an interrupted
// session can leave orphaned tool_use blocks that permanently lock the
// conversation. Returns the message and true if any synthetic results were
// produced.
func syntheticToolResultsForOrphanedCalls(m message.Message, knownToolResultIDs map[string]struct{}) (fantasy.Message, bool) {
	var syntheticParts []fantasy.MessagePart
	for _, tc := range m.ToolCalls() {
		if _, hasResult := knownToolResultIDs[tc.ID]; hasResult {
			continue
		}
		slog.Warn(
			"Injecting synthetic tool result for orphaned tool call",
			"tool_call_id", tc.ID,
			"tool_name", tc.Name,
		)
		syntheticParts = append(syntheticParts, fantasy.ToolResultPart{
			ToolCallID: tc.ID,
			Output: fantasy.ToolResultOutputContentError{
				Error: errors.New("tool call was interrupted and did not produce a result, you may retry this call if the result is still needed"),
			},
		})
	}
	if len(syntheticParts) == 0 {
		return fantasy.Message{}, false
	}
	return fantasy.Message{
		Role:    fantasy.MessageRoleTool,
		Content: syntheticParts,
	}, true
}

func (a *sessionAgent) getSessionMessages(ctx context.Context, session session.Session) ([]message.Message, error) {
	msgs, err := a.messages.List(ctx, session.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list messages: %w", err)
	}

	if session.SummaryMessageID != "" {
		summaryMsgIndex := -1
		for i, msg := range msgs {
			if msg.ID == session.SummaryMessageID {
				summaryMsgIndex = i
				break
			}
		}
		if summaryMsgIndex != -1 {
			// Collect pinned messages that appear before the summary
			var pinned []message.Message
			for _, msg := range msgs[:summaryMsgIndex] {
				if msg.Pinned {
					pinned = append(pinned, msg)
				}
			}
			msgs = msgs[summaryMsgIndex:]
			msgs[0].Role = message.User
			if len(pinned) > 0 {
				msgs = append(pinned, msgs...)
			}
		}
	}
	return msgs, nil
}

// generateTitle generates a session titled based on the initial prompt.
func (a *sessionAgent) generateTitle(ctx context.Context, sessionID string, userPrompt string) {
	if userPrompt == "" {
		return
	}

	// Ensure the session always gets a title even if every path below
	// fails or the context is cancelled before we finish. WithoutCancel so
	// the fallback still lands when the caller's ctx is already done.
	var titleSaved bool
	defer func() {
		if !titleSaved {
			fallbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := a.sessions.Rename(fallbackCtx, sessionID, DefaultSessionName); err != nil {
				slog.Error("Failed to save fallback session title", "error", err)
			}
		}
	}()

	smallModel := a.smallModel.Get()
	largeModel := a.largeModel.Get()
	systemPromptPrefix := a.systemPromptPrefix.Get()

	newAgent := func(m fantasy.LanguageModel, p []byte, tok int64) fantasy.Agent {
		return fantasy.NewAgent(
			m,
			fantasy.WithSystemPrompt(string(p)+"\n /no_think"),
			fantasy.WithMaxOutputTokens(tok),
			fantasy.WithUserAgent(userAgent),
		)
	}

	streamCall := fantasy.AgentStreamCall{
		Prompt: fmt.Sprintf("Generate a concise title for the following content:\n\n%s\n <think>\n\n</think>", userPrompt),
		PrepareStep: func(callCtx context.Context, opts fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = opts.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{
					fantasy.NewSystemMessage(systemPromptPrefix),
				}, prepared.Messages...)
			}
			return callCtx, prepared, nil
		},
	}

	// Try the small model first, then fall back to the large one. A
	// response that hit the token limit (FinishReasonLength) is treated as
	// a failure so we retry rather than save a truncated title.
	type modelAttempt struct {
		name  string
		model Model
	}
	attempts := []modelAttempt{
		{"small", smallModel},
		{"large", largeModel},
	}

	var resp *fantasy.AgentResult
	var err error
	var model Model
	var success bool
	for _, attempt := range attempts {
		tok := int64(40)
		if attempt.model.CatwalkCfg.CanReason {
			tok = attempt.model.CatwalkCfg.DefaultMaxTokens
		}
		agent := newAgent(attempt.model.Model, titlePrompt, tok)
		resp, err = agent.Stream(ctx, streamCall)
		if err == nil && resp != nil && resp.Response.FinishReason != fantasy.FinishReasonLength {
			model = attempt.model
			slog.Debug("Generated title with " + attempt.name + " model")
			success = true
			break
		}
		switch {
		case err != nil:
			slog.Error("Error generating title with "+attempt.name+" model; trying next", "err", err)
		case resp == nil:
			slog.Error("Title generation returned nil response with " + attempt.name + " model; trying next")
		default:
			slog.Error("Title generation hit token limit with " + attempt.name + " model; trying next")
		}
	}
	if !success {
		// The deferred fallback will save the default session name.
		return
	}

	// Clean up title.
	var title string
	title = strings.ReplaceAll(resp.Response.Content.Text(), "\n", " ")

	// Remove thinking tags if present.
	title = thinkTagRegex.ReplaceAllString(title, "")
	title = orphanThinkTagRegex.ReplaceAllString(title, "")

	title = strings.TrimSpace(title)
	title = cmp.Or(title, DefaultSessionName)

	// Calculate usage and cost.
	var openrouterCost *float64
	for _, step := range resp.Steps {
		stepCost := a.openrouterCost(step.ProviderMetadata)
		if stepCost != nil {
			newCost := *stepCost
			if openrouterCost != nil {
				newCost += *openrouterCost
			}
			openrouterCost = &newCost
		}
	}

	modelConfig := model.CatwalkCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(resp.TotalUsage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(resp.TotalUsage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(resp.TotalUsage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(resp.TotalUsage.OutputTokens)

	// Use override cost if available (e.g., from OpenRouter).
	if openrouterCost != nil {
		cost = *openrouterCost
	}

	// Skip cost accumulation
	if model.FlatRate {
		cost = 0
	}

	promptTokens := resp.TotalUsage.InputTokens + resp.TotalUsage.CacheCreationTokens
	completionTokens := resp.TotalUsage.OutputTokens

	// Atomically update only title and usage fields to avoid overriding other
	// concurrent session updates.
	saveErr := a.sessions.UpdateTitleAndUsage(ctx, sessionID, title, promptTokens, completionTokens, cost)
	if saveErr != nil {
		slog.Error("Failed to save session title and usage", "error", saveErr)
		return
	}
	titleSaved = true
}

func (a *sessionAgent) openrouterCost(metadata fantasy.ProviderMetadata) *float64 {
	openrouterMetadata, ok := metadata[openrouter.Name]
	if !ok {
		return nil
	}

	opts, ok := openrouterMetadata.(*openrouter.ProviderMetadata)
	if !ok {
		return nil
	}
	return &opts.Usage.Cost
}

// updateSessionUsage computes the cost delta for this step, applies the
// new token snapshot to session in-place (token fields are last-snapshot
// overwrite semantics), and returns the cost delta. The caller MUST
// persist the cost delta via sessions.IncrementCost (race-safe additive
// UPDATE) rather than relying on Save, because Save no longer writes the
// cost column.
//
// Fork patch (concurrency): upstream version was void; we now return
// the delta and rely on the caller to drive IncrementCost. See
// CHANGELOG.fork.md (Section 4.I).
//
// Fork merge note (origin/main 6ed8852b / 2e9c6505 / 74e6e378 "fix(agent):
// estimate/harden fallback usage accounting"): adopted upstream's
// updateSessionTokenCounters helper so partial-zero usage chunks no longer
// overwrite accumulated counters with zero. Rejected their `estimated bool`
// parameter (drives session.EstimatedUsage marker — a TUI widget we do not
// ship, see CHANGELOG.fork.md Section 2) and their eventTokensUsed publish
// (no consumer in our WebSocket fan-out).
func (a *sessionAgent) updateSessionUsage(model Model, session *session.Session, usage fantasy.Usage, overrideCost *float64) float64 {
	modelConfig := model.CatwalkCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(usage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(usage.OutputTokens)

	// Use override cost if available (e.g., from OpenRouter).
	if overrideCost != nil {
		cost = *overrideCost
	}

	// Skip cost accumulation
	if model.FlatRate {
		cost = 0
	}

	session.Cost += cost
	updateSessionTokenCounters(session, usage)
	return cost
}

// updateSessionTokenCounters writes a new usage snapshot into the session
// without overwriting accumulated counters with zero. Fork merge note: from
// origin/main 74e6e378 "fix(agent): harden fallback usage accounting".
func updateSessionTokenCounters(session *session.Session, usage fantasy.Usage) {
	if usage.OutputTokens != 0 {
		session.CompletionTokens = usage.OutputTokens
	}
	if promptTokens := usage.InputTokens + usage.CacheReadTokens; promptTokens != 0 {
		session.PromptTokens = promptTokens
	}
}

// summaryCompletionTokens returns OutputTokens when the provider reported
// them, otherwise falls back to an approximate count from the rendered
// summary message. Fork merge note: from origin/main 6ed8852b
// "fix(agent): estimate missing streamed usage" — used in Summarize when
// the provider omits final usage on the summary stream.
func summaryCompletionTokens(usage fantasy.Usage, summaryMessage message.Message) int64 {
	if usage.OutputTokens != 0 {
		return usage.OutputTokens
	}
	return approxTokenCount(summaryMessage.Content().Text) + approxTokenCount(summaryMessage.ReasoningContent().String())
}

func (a *sessionAgent) Cancel(sessionID string) {
	// Cancel regular requests. Don't use Take() here - we need the entry to
	// remain in activeRequests so IsBusy() returns true until the goroutine
	// fully completes (including error handling that may access the DB).
	// The defer in processRequest will clean up the entry.
	if cancel, ok := a.activeRequests.Get(sessionID); ok && cancel != nil {
		slog.Debug("Request cancellation initiated", "session_id", sessionID)
		cancel()
	}

	// Also check for summarize requests.
	if cancel, ok := a.activeRequests.Get(sessionID + "-summarize"); ok && cancel != nil {
		slog.Debug("Summarize cancellation initiated", "session_id", sessionID)
		cancel()
	}

	if a.QueuedPrompts(sessionID) > 0 {
		slog.Debug("Clearing queued prompts", "session_id", sessionID)
		a.messageQueue.Del(sessionID)
	}
	a.injectQueue.Del(sessionID)
}

func (a *sessionAgent) ClearQueue(sessionID string) {
	if a.QueuedPrompts(sessionID) > 0 {
		slog.Debug("Clearing queued prompts", "session_id", sessionID)
		a.messageQueue.Del(sessionID)
	}
	a.injectQueue.Del(sessionID)
}

func (a *sessionAgent) QueueMessage(call SessionAgentCall) {
	existing, _ := a.messageQueue.Get(call.SessionID)
	a.messageQueue.Set(call.SessionID, append(existing, call))
}

// InjectMessage — see SessionAgent interface comment. Persists immediately
// (UI updates via the same pubsub path that handleSendMessage uses) and, if
// the session is currently running, latches the persisted row into
// injectQueue so the next PrepareStep dredges it into prepared.Messages
// without duplicating the DB write.
func (a *sessionAgent) InjectMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	msg, err := a.createUserMessage(ctx, call)
	if err != nil {
		return message.Message{}, err
	}
	if a.IsSessionBusy(call.SessionID) {
		existing, _ := a.injectQueue.Get(call.SessionID)
		a.injectQueue.Set(call.SessionID, append(existing, msg))
	}
	return msg, nil
}

func (a *sessionAgent) CancelAll() {
	if !a.IsBusy() {
		return
	}
	for key := range a.activeRequests.Seq2() {
		a.Cancel(key) // key is sessionID
	}

	timeout := time.After(5 * time.Second)
	for a.IsBusy() {
		select {
		case <-timeout:
			return
		default:
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func (a *sessionAgent) IsBusy() bool {
	var busy bool
	for cancelFunc := range a.activeRequests.Seq() {
		if cancelFunc != nil {
			busy = true
			break
		}
	}
	return busy
}

func (a *sessionAgent) IsSessionBusy(sessionID string) bool {
	_, busy := a.activeRequests.Get(sessionID)
	return busy
}

func (a *sessionAgent) QueuedPrompts(sessionID string) int {
	l, ok := a.messageQueue.Get(sessionID)
	if !ok {
		return 0
	}
	return len(l)
}

func (a *sessionAgent) QueuedPromptsList(sessionID string) []string {
	l, ok := a.messageQueue.Get(sessionID)
	if !ok {
		return nil
	}
	prompts := make([]string, len(l))
	for i, call := range l {
		prompts[i] = call.Prompt
	}
	return prompts
}

func (a *sessionAgent) SetModels(large Model, small Model) {
	a.largeModel.Set(large)
	a.smallModel.Set(small)
}

func (a *sessionAgent) SetTools(tools []fantasy.AgentTool) {
	a.tools.SetSlice(tools)
}

func (a *sessionAgent) SetSystemPrompt(systemPrompt string) {
	a.systemPrompt.Set(systemPrompt)
}

func (a *sessionAgent) SetSystemPromptPrefix(prefix string) {
	a.systemPromptPrefix.Set(prefix)
}

func (a *sessionAgent) SystemPrompt() string {
	return a.systemPrompt.Get()
}

func (a *sessionAgent) Model() Model {
	return a.largeModel.Get()
}

// convertToToolResult converts a fantasy tool result to a message tool result.
func (a *sessionAgent) convertToToolResult(result fantasy.ToolResultContent) message.ToolResult {
	baseResult := message.ToolResult{
		ToolCallID: result.ToolCallID,
		Name:       result.ToolName,
		Metadata:   result.ClientMetadata,
	}

	switch result.Result.GetType() {
	case fantasy.ToolResultContentTypeText:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Result); ok {
			baseResult.Content = r.Text
		}
	case fantasy.ToolResultContentTypeError:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Result); ok {
			baseResult.Content = r.Error.Error()
			baseResult.IsError = true
		}
	case fantasy.ToolResultContentTypeMedia:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result.Result); ok {
			if !stringext.IsValidBase64(r.Data) {
				slog.Warn(
					"Tool returned media with invalid base64 data, discarding image",
					"tool", result.ToolName,
					"tool_call_id", result.ToolCallID,
				)
				baseResult.Content = "Tool returned image data with invalid encoding"
				baseResult.IsError = true
			} else {
				content := r.Text
				if content == "" {
					content = fmt.Sprintf("Loaded %s content", r.MediaType)
				}
				baseResult.Content = content
				baseResult.Data = r.Data
				baseResult.MIMEType = r.MediaType
			}
		}
	}

	return baseResult
}

// workaroundProviderMediaLimitations converts media content in tool results to
// user messages for providers that don't natively support images in tool results.
//
// Problem: OpenAI, Google, OpenRouter, and other OpenAI-compatible providers
// don't support sending images/media in tool result messages - they only accept
// text in tool results. However, they DO support images in user messages.
//
// If we send media in tool results to these providers, the API returns an error.
//
// Solution: For these providers, we:
//  1. Replace the media in the tool result with a text placeholder
//  2. Inject a user message immediately after with the image as a file attachment
//  3. This maintains the tool execution flow while working around API limitations
//
// Anthropic and Bedrock support images natively in tool results, so we skip
// this workaround for them.
//
// Example transformation:
//
//	BEFORE: [tool result: image data]
//	AFTER:  [tool result: "Image loaded - see attached"], [user: image attachment]
func (a *sessionAgent) workaroundProviderMediaLimitations(messages []fantasy.Message, largeModel Model) []fantasy.Message {
	providerSupportsMedia := largeModel.ModelCfg.Provider == string(catwalk.InferenceProviderAnthropic) ||
		largeModel.ModelCfg.Provider == string(catwalk.InferenceProviderBedrock)

	if providerSupportsMedia {
		return messages
	}

	convertedMessages := make([]fantasy.Message, 0, len(messages))

	for _, msg := range messages {
		if msg.Role != fantasy.MessageRoleTool {
			convertedMessages = append(convertedMessages, msg)
			continue
		}

		textParts := make([]fantasy.MessagePart, 0, len(msg.Content))
		var mediaFiles []fantasy.FilePart

		for _, part := range msg.Content {
			toolResult, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				textParts = append(textParts, part)
				continue
			}

			if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](toolResult.Output); ok {
				decoded, err := base64.StdEncoding.DecodeString(media.Data)
				if err != nil {
					slog.Warn("Failed to decode media data", "error", err)
					textParts = append(textParts, part)
					continue
				}

				mediaFiles = append(mediaFiles, fantasy.FilePart{
					Data:      decoded,
					MediaType: media.MediaType,
					Filename:  fmt.Sprintf("tool-result-%s", toolResult.ToolCallID),
				})

				textParts = append(textParts, fantasy.ToolResultPart{
					ToolCallID: toolResult.ToolCallID,
					Output: fantasy.ToolResultOutputContentText{
						Text: "[Image/media content loaded - see attached file]",
					},
					ProviderOptions: toolResult.ProviderOptions,
				})
			} else {
				textParts = append(textParts, part)
			}
		}

		convertedMessages = append(convertedMessages, fantasy.Message{
			Role:    fantasy.MessageRoleTool,
			Content: textParts,
		})

		if len(mediaFiles) > 0 {
			convertedMessages = append(convertedMessages, fantasy.NewUserMessage(
				"Here is the media content from the tool result:",
				mediaFiles...,
			))
		}
	}

	return convertedMessages
}

// trimMessagesToWindow returns a suffix of msgs whose estimated token count
// fits within targetTokens (1 token ≈ 4 characters).  It always starts on a
// user-role message so the conversation stays well-formed.
func trimMessagesToWindow(msgs []fantasy.Message, targetTokens int64) []fantasy.Message {
	if len(msgs) == 0 || targetTokens <= 0 {
		return msgs
	}
	const charsPerToken = 4
	budget := int(targetTokens) * charsPerToken

	var accumulated int
	cutIdx := 0 // by default keep everything
	for i := len(msgs) - 1; i >= 0; i-- {
		accumulated += estimateMsgChars(msgs[i])
		if accumulated >= budget {
			cutIdx = i + 1
			break
		}
	}
	if cutIdx == 0 {
		return msgs // all messages fit
	}
	// Advance to the next user-role message to keep the history well-formed.
	for cutIdx < len(msgs) && msgs[cutIdx].Role != fantasy.MessageRoleUser {
		cutIdx++
	}
	if cutIdx >= len(msgs) {
		return msgs // can't trim without losing all context
	}
	return msgs[cutIdx:]
}

// estimateMsgChars returns a rough character count for a fantasy.Message,
// used to estimate its token footprint for window trimming.
func estimateMsgChars(msg fantasy.Message) int {
	total := 0
	for _, part := range msg.Content {
		switch p := part.(type) {
		case fantasy.TextPart:
			total += len(p.Text)
		case fantasy.ToolCallPart:
			total += len(p.ToolName) + len(p.Input)
		case fantasy.ToolResultPart:
			switch o := p.Output.(type) {
			case fantasy.ToolResultOutputContentText:
				total += len(o.Text)
			case fantasy.ToolResultOutputContentError:
				total += len(fmt.Sprintf("%v", o.Error))
			}
		}
	}
	if total == 0 {
		total = 64 // minimum for empty / binary messages
	}
	return total
}

// buildSummaryPrompt constructs the prompt text for session summarization.
func buildSummaryPrompt(todos []session.Todo) string {
	var sb strings.Builder
	sb.WriteString("Provide a detailed summary of our conversation above.")
	if len(todos) > 0 {
		sb.WriteString("\n\n## Current Todo List\n\n")
		for _, t := range todos {
			fmt.Fprintf(&sb, "- [%s] %s\n", t.Status, t.Content)
		}
		sb.WriteString("\nInclude these tasks and their statuses in your summary. ")
		sb.WriteString("Instruct the resuming assistant to use the `todos` tool to continue tracking progress on these tasks.")
	}
	return sb.String()
}

func providerRetryLogFields(err *fantasy.ProviderError, delay time.Duration) []any {
	fields := []any{
		"retry_delay", delay.String(),
	}
	if err == nil {
		return fields
	}
	fields = append(fields, "status_code", err.StatusCode)
	if err.Title != "" {
		fields = append(fields, "title", err.Title)
	}
	if err.Message != "" {
		fields = append(fields, "message", err.Message)
	}
	return fields
}
