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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	crushlog "github.com/charmbracelet/crush/internal/log"
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

	// toolExecutionMaxDefault is the never-freeze backstop: the maximum
	// wall-clock a single tool may run while the stream watchdog is paused
	// (between OnToolCall and OnToolResult) before the watchdog force-
	// cancels the turn. Bash auto-backgrounds at 60s and Phase 1b bounds
	// job_output, so this only catches truly stuck tools (hung MCP tools,
	// blocking job_output --wait on a deadlocked process, or a sub-agent
	// delegation via the "agent" tool that never returns).
	// Configurable via Options.StreamToolTimeoutSeconds.
	//
	// One value applies uniformly to every tool, including a sub-agent
	// delegation — there used to be a separate, larger cap
	// (orchestratorToolExecutionMaxDefault, 45m) reserved for delegations
	// while plain tools kept a shorter one (15m). That split caused its own
	// false cutoffs: a sub-agent's OWN plain tool call (a slow build/test
	// inside ITS turn, not a delegation) still only got the short cap, so
	// legitimate long-running work inside a sub-agent kept getting killed
	// just as often as a parent waiting on a delegation used to be killed
	// by the old single 15m cap. Unifying to one generous value avoids
	// both directions of false cutoff at the cost of a genuinely wedged
	// tool taking longer to get caught — worth it since the point is to
	// bound the wait, not to bound it tightly.
	toolExecutionMaxDefault = 45 * time.Minute

	// toolCleanupGraceDefault is a fixed buffer added ON TOP of
	// toolMaxDuration before the stream watchdog is allowed to force-cancel
	// a tool-in-flight. It exists for the parent/child cancellation race
	// created by toolExecutionMaxDefault's unification above: a tool call
	// that is itself an `agent`-tool delegation runs a nested Run()/runTurn()
	// with its own stream watchdog, started strictly LATER than the
	// parent's (the parent starts timing from OnToolCall — the moment it
	// decided to delegate; the child starts timing only once its own turn
	// actually begins executing, after init and the DB preamble). Both
	// watchdogs share the same toolMaxDuration, so the parent's clock — with
	// its head start — would always reach the cap first and cancel genCtx,
	// which cascades into the child's ctx, before the child's own watchdog
	// ever gets a chance to fire on ITS cap and unwind cleanly (finish part,
	// cost transfer per task #197, goroutine dump). This grace is applied
	// uniformly to every tool-in-flight regardless of kind — it is not a
	// tool-name-keyed special case (that pattern was deliberately rejected
	// above); it just gives whichever tool is running a little extra runway
	// past its nominal cap, which is harmless for a plain tool and decisive
	// for a delegation. Configurable via Options.ToolCleanupGrace for tests;
	// 0 falls back to this default rather than disabling the grace, since an
	// accidental zero would silently reopen the race this constant exists
	// to close.
	//
	// HONEST SCOPE (found overstated by an @oh review of task #205's own
	// fix — corrected here rather than in a follow-up commit that could go
	// unread): 90s only guarantees the child wins the race against the
	// PARENT'S watchdog start skew — the gap between the parent's OnToolCall
	// and the child's own watchdog starting (init + DB preamble), which
	// really is sub-second to low seconds. It does NOT bound how long the
	// child may productively work before the tool call that actually wedges
	// starts. Concretely: parent fires at delegationStart + M + 90s; child
	// fires at (delegationStart + skew + workDuration) + M, where
	// workDuration is the child's own prior productive work before hitting
	// its stuck tool — unbounded, since the child's watchdog resets on every
	// tool-call boundary (see toolFinished's doc below). The child wins only
	// when skew + workDuration < 90s, i.e. only when the child wedges
	// EARLY (near-immediately after being delegated to) — the common "child
	// worked productively for minutes, THEN wedged deep into its turn" case
	// is NOT covered: the parent still force-cancels it first. What this
	// grace DOES guarantee unconditionally: the parent never fires before
	// its own toolMaxDuration+90s has elapsed since delegating, giving any
	// child that happens to wedge quickly (the case this was originally
	// observed in) real runway to clean up. A structural fix for the general
	// case would need the child to push progress signals the parent's own
	// watchdog resets on — not implemented; tracked as a follow-up, not a
	// release blocker, since the practical loss on the uncovered path is
	// diagnostic quality (finish part / goroutine dump), not correctness —
	// the parent still terminates, and task #197 already made the
	// cost-transfer step itself cancel-immune.
	//
	// Fork patch, task #205: this default is applied ONLY to a NON-sub-agent
	// (top-level) session — see effectiveToolCleanupGrace's doc. Applying it
	// to a sub-agent's own watchdog as well (task #200's original fix) made
	// the race worse, not better: with identical toolMaxDuration+grace on
	// both sides, the 90s cancels out of the "child must fire before parent"
	// inequality algebraically, leaving the parent's head start (from
	// OnToolCall, before the child's own watchdog even starts) as the only
	// deciding factor — so the parent always still won regardless of how
	// early the child wedged. A sub-agent can never itself be waiting on a
	// nested `agent`-tool delegation (the `agent` tool is excluded from
	// workerToolNames for sub-agents — see buildToolsAgentConfig), so it is
	// always the deepest watchdog in the chain and never needs runway to let
	// a nested watchdog go first.
	toolCleanupGraceDefault = 90 * time.Second

	// defaultCheckpointInterval is the default coalescing interval for
	// mid-stream DB flushes of in-progress assistant text. When > 0,
	// the auto-checkpoint ticker writes the Parts to DB at most once
	// per interval, bounding the text lost to a SIGTERM during final
	// composition. 0 disables checkpointing. Overridden by
	// SessionAgentOptions.CheckpointInterval.
	// Fork patch: batch 8.
	defaultCheckpointInterval = 2 * time.Second

	// peakHoursPollInterval is the mid-turn safety check cadence. The
	// OnStepFinish check catches normal step boundaries; this ticker catches
	// long streams, retries, and tool execution.
	peakHoursPollInterval = 10 * time.Second
)

// sessionPreambleMaxDurationDefault bounds the DB preamble at the top of
// Run() — sessions.Get, getSessionMessages, createUserMessage — all of which
// route through the single-writer sql.DB connection (SetMaxOpenConns(1) in
// internal/db/connect.go). The stream watchdog is not started until AFTER
// this preamble, so before this fix a stuck writer connection (a concurrent
// sub-agent's own preamble wedged on it, etc.) hung the whole turn invisibly
// and unboundedly: no watchdog running yet means no cancellation, no timeout
// log line, nothing — just a process that sits there with a "SessionAgent.Run:
// starting" log line and never another. Observed live: PID 28908 logged that
// line for a sub-agent session and then went silent for 10+ minutes while the
// lock heartbeat kept ticking (heartbeat is independent of actual progress —
// see task #192). This cap is generous — normal SQLite reads/writes take low
// milliseconds — so tripping it means something is genuinely wedged, not
// slow. Overridden per-agent via SessionAgentOptions.SessionPreambleMaxDuration,
// resolved at Run()-time via effectiveSessionPreambleMaxDuration below.
const sessionPreambleMaxDurationDefault = 60 * time.Second

// titleGenerationMaxDurationDefault bounds how long the background
// title-generation goroutine (launched from runTurn, awaited by its `defer
// wg.Wait()`) is allowed to run. Title generation is a best-effort cosmetic
// side call (a.generateTitle tries up to two models, each a blocking
// agent.Stream with no timeout of its own) and must never be able to hold
// runTurn — and therefore Run() — open past its own turn. Its context is
// derived from genCtx so the stream watchdog's cancellation already covers
// it; this timer is the independent backstop for the case where genCtx's
// cancellation, for whatever reason, doesn't propagate (e.g. a provider
// stuck outside of context-aware I/O). Generous relative to a title's actual
// cost (a handful of tokens) so it only ever trips when something is
// genuinely wedged. Overridden per-agent via
// SessionAgentOptions.TitleGenerationMaxDuration, resolved at runTurn-time
// via effectiveTitleGenerationMaxDuration below.
const titleGenerationMaxDurationDefault = 2 * time.Minute

// summaryCommitMaxDuration bounds the silent compaction's COMMIT phase — the
// SetSummaryAndUsage write plus the deletion of the messages it replaces
// (P0-4 review follow-up).
//
// That phase deliberately runs on context.WithoutCancel: interrupting it
// between "summary pointer written" and "old messages deleted" is how a
// session's history gets unrecoverably holed, so cancellation must not be
// able to land in the middle. Detaching from cancellation without a deadline
// would recreate the unbounded-operation-holds-Run shape P1-B removed, hence
// this bound. The provider stream is already finished by then; what remains
// is local single-writer DB work, so 30s is generous rather than tight.
const summaryCommitMaxDuration = 30 * time.Second

// titleJoinGrace bounds how long runTurn's deferred join waits for the
// title goroutine AFTER that goroutine's own deadline should already have
// fired (P1-B).
//
// The timer above only bounds a provider that honours context
// cancellation. One that does not — a hung connection, a transport stuck
// outside context-aware I/O — never returns, and the bare `defer
// wg.Wait()` this replaces then held runTurn (and with it Run, the
// session's mailbox ownership and its OS lock) open indefinitely, for a
// turn whose real work had already completed.
//
// Short on purpose: by the time this is consulted the title's own
// deadline has passed, so any further waiting is pure loss. Abandoning
// the goroutine is safe — it is not leaked so much as detached: it exits
// whenever its provider finally unblocks, and generateTitle's own
// deferred rename runs on a context.WithoutCancel, so a late completion
// still persists its result.
const titleJoinGrace = 5 * time.Second

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
	// ExistingMessageID, when non-empty, marks this call as referencing a
	// user message that already exists in the DB (created by another process,
	// e.g. `crush sessions inject --interrupt`). The queue-drain path in
	// Run's PrepareStep must then load that message by ID and splice it into
	// the prompt WITHOUT calling createUserMessage — otherwise the operator
	// would see the same message twice in history. Set by
	// QueueExistingMessage on the interrupt path.
	ExistingMessageID string

	// LargeModel/SmallModel/SystemPromptPrefix, when set, pin this call's
	// model and prompt configuration instead of reading the agent's shared,
	// mutable fields (task #265, P0-1).
	//
	// The agent's largeModel/smallModel/systemPromptPrefix are process-wide
	// and get REWRITTEN in place by coordinator.applyModelOverrides, which
	// any per-session model override triggers. Every turn re-reads them, so
	// with two sessions running concurrently — the whole point of this fork
	// — session A's override lands in session B's next turn, and a turn can
	// even observe a model change partway through its own run. These fields
	// let a caller hand the resolved values down with the call, so a run
	// stays self-consistent regardless of what another session does
	// meanwhile.
	//
	// Pointers, so "explicitly set" is distinguishable from "zero value":
	// nil means "use the agent's shared value", which is exactly what every
	// existing caller that never sets them keeps getting.
	LargeModel         *Model
	SmallModel         *Model
	SystemPromptPrefix *string

	// SystemPrompt pins the BASE system prompt — the one applyModelOverrides
	// rebuilds from the resolved provider/model. It is distinct from, and
	// lower precedence than, SystemPromptOverride: the override is the
	// per-session prompt persisted in the DB, which still wins when present.
	//
	// This one matters on the paths where the per-session prompt is empty
	// (resolveSessionSystemPrompt returning "" on a DB error, mainly) — the
	// turn then falls back to the agent's shared systemPrompt, which is
	// exactly the field another session's override rewrites.
	SystemPrompt *string
}

// turnConfig is the immutable per-call snapshot of everything model- and
// prompt-shaped a turn needs (task #265, P0-1). Resolved ONCE — from the
// call where it pins values, otherwise from the agent's shared fields —
// then passed by value, so nothing downstream can observe a mid-run change
// made by a concurrent session.
type turnConfig struct {
	largeModel   Model
	smallModel   Model
	systemPrompt string
	promptPrefix string
}

// resolveTurnConfig builds the snapshot for one call. Reads each shared
// field exactly once; a value pinned on the call wins over the shared one.
func (a *sessionAgent) resolveTurnConfig(call SessionAgentCall) turnConfig {
	cfg := turnConfig{
		largeModel:   a.largeModel.Get(),
		smallModel:   a.smallModel.Get(),
		systemPrompt: a.systemPrompt.Get(),
		promptPrefix: a.systemPromptPrefix.Get(),
	}
	if call.LargeModel != nil {
		cfg.largeModel = *call.LargeModel
	}
	if call.SmallModel != nil {
		cfg.smallModel = *call.SmallModel
	}
	if call.SystemPromptPrefix != nil {
		cfg.promptPrefix = *call.SystemPromptPrefix
	}
	if call.SystemPrompt != nil {
		cfg.systemPrompt = *call.SystemPrompt
	}
	// Per-session system prompt beats the global one — same precedence this
	// had when it lived inline in runTurn.
	if call.SystemPromptOverride != "" {
		cfg.systemPrompt = call.SystemPromptOverride
	}
	return cfg
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
	// InterruptAndReplace atomically records call as the replacement the
	// current owner runs next and cancels only the in-flight generation
	// (design §4). Replaces the QueueMessage+Cancel two-step that
	// deterministically wiped its own queued message (P0-2). Returns true
	// when a turn was actually interrupted; false when the session was idle
	// (the caller should then queue the call for the next Run itself).
	InterruptAndReplace(sessionID string, call SessionAgentCall) bool
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

	// shuttingDown latches on the first CancelAll and is checked by Run
	// before it claims any session. Round 14 closing review, blocker 1:
	// the per-mailbox `stopped` latch only stops an EXISTING turn loop from
	// picking up more work — it cannot stop a NEW owner from appearing.
	// After CancelAll's sweep, a mailbox that ends at mbIdle happily grants
	// ownership to the next Run(), which then runs a full provider turn
	// that the (already-finished) sweep will never cancel. Worse, mailboxes
	// are created lazily, so a Run for a session id the sweep never saw
	// gets a fresh mailbox with stopped == false — no race required.
	//
	// Entry points that can still call Run after the sweep are real and
	// several: coordinator.startDetachedRun (its own detached goroutine on
	// a WithoutCancel context), runSummarize's tail, Summarize, and the web
	// handlers — none of which observe per-mailbox state.
	//
	// Agent-level and one-way, for the same reason `stopped` is: the agent
	// belongs to a process that is exiting.
	shuttingDown atomic.Bool

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
	// toolMaxDuration bounds the watchdog's tool-pause (never-freeze
	// backstop). Past it the watchdog fires with a distinct "tool timeout"
	// reason so the agent turn ends instead of hanging on a stuck tool.
	// 0 = use toolExecutionMaxDefault. This is the EXPLICIT OPERATOR
	// OVERRIDE (Options.StreamToolTimeoutSeconds) and, when set, always
	// wins over the built-in default, in either direction.
	toolMaxDuration time.Duration
	// toolCleanupGrace is the buffer added on top of the resolved
	// toolMaxDuration before the watchdog force-cancels a tool-in-flight —
	// see toolCleanupGraceDefault's doc for why this exists (parent/child
	// delegation cancellation race) and effectiveToolCleanupGrace's doc for
	// full precedence. 0 = no explicit override: falls back to
	// toolCleanupGraceDefault for a top-level session, or to 0 (no grace)
	// for a sub-agent session (task #205). Tests may set a small explicit
	// value via SessionAgentOptions.ToolCleanupGrace, which always wins.
	toolCleanupGrace time.Duration
	// sessionPreambleMaxDuration bounds Run()'s DB preamble (sessions.Get,
	// getSessionMessages, createUserMessage) for THIS agent — see
	// sessionPreambleMaxDurationDefault's doc for the full rationale. 0 = use
	// sessionPreambleMaxDurationDefault; tests may override to a small value
	// via SessionAgentOptions.SessionPreambleMaxDuration instead of mutating
	// shared state.
	sessionPreambleMaxDuration time.Duration
	// titleGenerationMaxDuration bounds the background title-generation
	// goroutine for THIS agent — see titleGenerationMaxDurationDefault's doc
	// for the full rationale. 0 = use titleGenerationMaxDurationDefault;
	// tests may override to a small value via
	// SessionAgentOptions.TitleGenerationMaxDuration instead of mutating
	// shared state.
	titleGenerationMaxDuration time.Duration

	// messageQueue is a per-session FIFO queue. It uses csync.KeyedQueue
	// (not csync.Map[string, []T]) because every real usage here is a
	// composite read-modify-write (append-to-existing, or
	// read-then-delete-to-drain) — pairing Map.Get with a later Map.Set/Del
	// leaves a window where a concurrent Append/drain can interleave and
	// silently lose a queued message. KeyedQueue makes each of those
	// composite operations (Append, TakeAll, PopFront) a single atomic
	// critical section per session id.
	//
	// Still live: only tryReserveSession/releaseSessionReservation and
	// InterruptAndReplace have migrated to the mailbox (design §7 stages
	// 2.1-2.3); QueueMessage and its drain consumers
	// (drainOrReleaseMerged, PrepareStep, runSummarizeBody, QueuedPrompts,
	// ClearQueue, Run's re-queue defer) still read/write this queue
	// directly. Removed once those move to the mailbox.
	messageQueue *csync.KeyedQueue[SessionAgentCall]
	// activeRequests is retained for the per-turn cancel that
	// OnStepFinish's max-cost / max-tokens / peak-hours abort paths still
	// look up directly (Cancel/CancelAll now route through the mailbox
	// instead). The sessionID+"-summarize" synthetic key that used to live
	// here has been removed (#268/P0-4: compaction now owns the mailbox via
	// beginCompact, so its cancel lives in mailbox.current.cancel). Plain
	// sessionID entries are inert (already-fired cancelFuncs) and are no
	// longer consulted for busy state (IsSessionBusy reads the mailbox).
	activeRequests *csync.Map[string, context.CancelFunc]
	// summarizeQueue holds a pending manual-summarise request per session,
	// queued while the session was busy.
	summarizeQueue *csync.Map[string, fantasy.ProviderOptions]
	// mailboxes holds the per-session owner/mailbox state machine described
	// in docs/plans/2026-08-04-session-owner-mailbox-design.md. Stages
	// 2.1-2.4 migrated tryReserveSession/releaseSessionReservation,
	// InterruptAndReplace, the two-tier context split, and the inject path
	// onto the mailbox; injectQueue and sessionStartMu have been deleted as
	// fully dead. messageQueue (QueueMessage + its drain consumers) remains
	// live. activeRequests is retained for OnStepFinish abort-path cancel
	// lookups; the sessionID+"-summarize" synthetic key has been removed
	// (#268). Both are removed in later stages. One mailbox per
	// session id, created lazily on first touch via GetOrSet and never
	// explicitly deleted — same "one map, lazily populated, entries live
	// forever" lifetime as activeRequests today.
	mailboxes *csync.Map[string, *mailbox]
	// peakHoursCheck, when non-nil, is called once per step from
	// OnStepFinish to re-check whether the large model's provider has
	// entered its peak_hours refusal window mid-turn. Returns nil while
	// outside the window. Plumbed from coordinator.buildAgent, which is
	// the only layer with access to config.ProviderConfig — sessionAgent
	// itself only knows about Model (SelectedModel + catwalk metadata),
	// not the provider's peak_hours setting.
	peakHoursCheck func() error
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
	// ToolMaxDuration bounds the watchdog's tool-pause (never-freeze
	// backstop). Past it the watchdog fires with a "tool timeout" reason
	// so the turn ends instead of hanging on a stuck tool. 0 = use the
	// built-in toolExecutionMaxDefault (45m), applied uniformly to every
	// tool including sub-agent delegations. Explicitly set (> 0), this
	// ALWAYS wins over the built-in default — plumbed from
	// Options.StreamToolTimeoutSeconds in the coordinator.
	ToolMaxDuration time.Duration
	// ToolCleanupGrace overrides the resolved grace when > 0 — the buffer
	// added on top of the resolved tool-max-duration before the watchdog
	// force-cancels a tool-in-flight, giving a nested (child) watchdog
	// inside an `agent`-tool delegation a chance to fire on its own cap and
	// unwind cleanly first. See toolCleanupGraceDefault's and
	// effectiveToolCleanupGrace's docs for the full rationale. 0 = no
	// explicit override: the built-in default (toolCleanupGraceDefault)
	// applies only to a top-level (non-sub-agent) session; a sub-agent
	// session gets 0 (no grace) by default, since it can never itself be
	// waiting on a nested delegation (task #205). Primarily exposed for
	// tests that want a short, non-zero grace instead of waiting out the
	// real default.
	ToolCleanupGrace time.Duration
	// PeakHoursCheck, when non-nil, is called once per step to re-check
	// whether the large model's provider has entered its peak_hours
	// window mid-turn. See the field doc on sessionAgent.peakHoursCheck.
	PeakHoursCheck func() error
	// SessionPreambleMaxDuration overrides sessionPreambleMaxDurationDefault
	// when > 0 — the bound on Run()'s DB preamble before the stream watchdog
	// starts. See sessionPreambleMaxDurationDefault's doc for the full
	// rationale. 0 = use the built-in default; primarily exposed for tests
	// that want a short bound instead of waiting out the real one.
	SessionPreambleMaxDuration time.Duration
	// TitleGenerationMaxDuration overrides titleGenerationMaxDurationDefault
	// when > 0 — the bound on the background title-generation goroutine. See
	// titleGenerationMaxDurationDefault's doc for the full rationale. 0 = use
	// the built-in default; primarily exposed for tests that want a short
	// bound instead of waiting out the real one.
	TitleGenerationMaxDuration time.Duration
}

func NewSessionAgent(
	opts SessionAgentOptions,
) SessionAgent {
	return &sessionAgent{
		largeModel:                 csync.NewValue(opts.LargeModel),
		smallModel:                 csync.NewValue(opts.SmallModel),
		systemPromptPrefix:         csync.NewValue(opts.SystemPromptPrefix),
		systemPrompt:               csync.NewValue(opts.SystemPrompt),
		isSubAgent:                 opts.IsSubAgent,
		sessions:                   opts.Sessions,
		messages:                   opts.Messages,
		disableAutoSummarize:       opts.DisableAutoSummarize,
		tools:                      csync.NewSliceFrom(opts.Tools),
		isYolo:                     opts.IsYolo,
		notify:                     opts.Notify,
		messageQueue:               csync.NewKeyedQueue[SessionAgentCall](),
		activeRequests:             csync.NewMap[string, context.CancelFunc](),
		summarizeQueue:             csync.NewMap[string, fantasy.ProviderOptions](),
		mailboxes:                  csync.NewMap[string, *mailbox](),
		streamIdleTimeout:          opts.StreamIdleTimeout,
		dataDir:                    opts.DataDirectory,
		checkpointInterval:         opts.CheckpointInterval,
		timeoutExtendsOnProgress:   opts.TimeoutExtendsOnProgress,
		timeoutHardCap:             opts.TimeoutHardCap,
		toolMaxDuration:            opts.ToolMaxDuration,
		toolCleanupGrace:           opts.ToolCleanupGrace,
		peakHoursCheck:             opts.PeakHoursCheck,
		sessionPreambleMaxDuration: opts.SessionPreambleMaxDuration,
		titleGenerationMaxDuration: opts.TitleGenerationMaxDuration,
	}
}

// SetTimeoutOptions configures the stream watchdog deadline extension.
// Fork patch: batch 8.
func (a *sessionAgent) SetTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
	a.timeoutExtendsOnProgress = extendsOnProgress
	a.timeoutHardCap = hardCap
}

// effectiveToolMaxDuration resolves the stream watchdog's never-freeze
// backstop (the max wall-clock a single tool may run while the watchdog is
// paused between OnToolCall/OnToolResult) for THIS Run() call. One value
// applies to every tool, including a sub-agent delegation via the "agent"
// tool — see toolExecutionMaxDefault's doc for why a plain/orchestrator
// split was removed in favor of a single generous cap. Precedence:
//
//  1. toolExecutionMaxDefault (45m) — the baseline.
//  2. a.toolMaxDuration (> 0) — the EXPLICIT OPERATOR OVERRIDE, from
//     Options.StreamToolTimeoutSeconds. Applied last, unconditionally, so
//     it always wins over (1) in either direction.
func (a *sessionAgent) effectiveToolMaxDuration() time.Duration {
	toolMaxDuration := toolExecutionMaxDefault
	if a.toolMaxDuration > 0 {
		toolMaxDuration = a.toolMaxDuration
	}
	return toolMaxDuration
}

// effectiveToolCleanupGrace resolves the buffer added on top of
// effectiveToolMaxDuration before the stream watchdog force-cancels a
// tool-in-flight. See toolCleanupGraceDefault's doc for why this exists.
// Precedence:
//
//  1. a.toolCleanupGrace (> 0) — an EXPLICIT OPERATOR OVERRIDE (or test
//     override via SessionAgentOptions.ToolCleanupGrace). Checked FIRST and
//     applied unconditionally, so it wins even for a sub-agent that
//     explicitly opts back in.
//  2. Otherwise: toolCleanupGraceDefault (90s) for a top-level (non-sub-agent)
//     session, or 0 (no grace) for a sub-agent session.
//
// Fork patch, task #205 (reopens #200): the grace exists ONLY to let a
// nested (child) sub-agent watchdog fire on its OWN cap and unwind cleanly
// before the PARENT's watchdog (whose clock started earlier — at OnToolCall
// for the `agent`-tool delegation, before the child's own turn has even
// begun executing) force-cancels genCtx out from under it. Giving the
// SAME grace to the sub-agent's own watchdog (task #200's original,
// symmetric fix) defeated that purpose: with identical
// toolMaxDuration+grace on both sides, the 90s cancels out of the
// "child must fire before parent" inequality algebraically, so the
// parent's unconditional head start remained the only deciding factor and
// it still always won the race. A sub-agent can never itself be waiting on
// a nested `agent`-tool delegation — the `agent` tool is excluded from
// workerToolNames for sub-agents (see coordinator.go's
// buildToolsAgentConfig/workerToolNames), so a sub-agent is always the
// deepest watchdog in the chain and never needs runway for a nested one to
// go first. Only a top-level (!isSubAgent) session's watchdog is ever the
// one waiting on a delegation, so only it gets the default grace.
func (a *sessionAgent) effectiveToolCleanupGrace() time.Duration {
	if a.toolCleanupGrace > 0 {
		return a.toolCleanupGrace
	}
	if a.isSubAgent {
		return 0
	}
	return toolCleanupGraceDefault
}

// effectiveSessionPreambleMaxDuration resolves the bound on Run()'s DB
// preamble (sessions.Get, getSessionMessages, createUserMessage) for THIS
// agent. See sessionPreambleMaxDurationDefault's doc for why this exists.
// 0 falls back to the default.
func (a *sessionAgent) effectiveSessionPreambleMaxDuration() time.Duration {
	sessionPreambleMaxDuration := sessionPreambleMaxDurationDefault
	if a.sessionPreambleMaxDuration > 0 {
		sessionPreambleMaxDuration = a.sessionPreambleMaxDuration
	}
	return sessionPreambleMaxDuration
}

// effectiveTitleGenerationMaxDuration resolves the bound on the background
// title-generation goroutine for THIS agent. See
// titleGenerationMaxDurationDefault's doc for why this exists. 0 falls back
// to the default.
func (a *sessionAgent) effectiveTitleGenerationMaxDuration() time.Duration {
	titleGenerationMaxDuration := titleGenerationMaxDurationDefault
	if a.titleGenerationMaxDuration > 0 {
		titleGenerationMaxDuration = a.titleGenerationMaxDuration
	}
	return titleGenerationMaxDuration
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

// getMailbox returns the per-session mailbox for sessionID, creating it
// lazily on first touch. Mirrors activeRequests' "one map, lazily
// populated, entries live forever" lifetime — see mailbox.go and
// docs/plans/2026-08-04-session-owner-mailbox-design.md §1.
func (a *sessionAgent) getMailbox(sessionID string) *mailbox {
	return a.mailboxes.GetOrSet(sessionID, func() *mailbox { return &mailbox{} })
}

// tryReserveSession is now a thin wrapper around mailbox.submit (design §3,
// stage 2 step 1 of the migration — see
// docs/plans/2026-08-04-session-owner-mailbox-design.md §7). Historically
// this used sessionStartMu + activeRequests directly; that had a real
// P0-3 lost-wakeup defect at the OTHER end of the reservation's lifetime
// (see releaseSessionReservation's doc below) even though the reserve side
// itself was already atomic. Routing through the mailbox's own mutex keeps
// the reserve and release sides of the reservation on the same lock, which
// is what closes that window.
//
// call must be the REAL call Run() received, not a placeholder: when the
// mailbox is already owned, submit appends call verbatim to mb.submitted for
// the current owner's end-of-turn drain to pick up as the next turn's
// SessionAgentCall — a placeholder with an empty Prompt would silently
// replace the caller's actual prompt with an empty one once drained.
//
// When becomeOwner is true, epoch is this ownership era's id (round 9
// review, BLOCKER-2) — the caller MUST present it to every subsequent
// releaseSessionReservation/drainOrReleaseMerged/abandonOwnership call it
// makes for the lifetime of this Run() call, so a stale/duplicate release
// (Run's cleanup defer firing after the era already legitimately ended) can
// never be mistaken for "release MY still-current ownership" and clobber a
// different, later owner's state. See mailbox.epoch's doc for the full
// rationale.
func (a *sessionAgent) tryReserveSession(call SessionAgentCall, reserveCancel context.CancelFunc) (becomeOwner bool, epoch uint64) {
	return a.getMailbox(call.SessionID).submit(call, reserveCancel)
}

// releaseSessionReservation clears the busy slot claimed by
// tryReserveSession, atomically checking for and returning any call queued
// in the meantime via mailbox.drainOrRelease (design §3). Before this, the
// old activeRequests.Del(sessionID) was strictly a release with no way to
// observe a concurrent submit that raced into the gap between the caller's
// own final "nothing queued" check and this release running — that gap is
// exactly the P0-3 lost-wakeup window. Any call handed back here (which can
// only happen when a concurrent submit lands between this release and the
// caller's own preceding drain check) is a genuine hasNext case; callers
// that cannot start another turn synchronously (e.g. Run's pre-loop bail-out
// paths) must not discard it silently — see those call sites for how it is
// handled.
//
// epoch must be the era id tryReserveSession (or a prior drainOrRelease
// call within the same era) returned. See mailbox.drainOrRelease's doc for
// what a mismatch means.
//
// Not used by runTurn's own end-of-turn drain any more (see
// drainOrReleaseMerged/drainOrReleaseFinal, round 11 review HIGH-1) since it
// releases nothing but the in-process reservation — callers that also need
// the OS-level session lock released atomically with the mbIdle flip must go
// through drainOrReleaseFinal instead. Kept as a thin wrapper for direct
// mailbox-level testing and any future caller with no OS lock in play.
func (a *sessionAgent) releaseSessionReservation(sessionID string, epoch uint64) (SessionAgentCall, bool) {
	return a.getMailbox(sessionID).drainOrRelease(epoch)
}

// drainOrReleaseMerged is runTurn's end-of-turn drain: releaseSessionReservation's
// former composition with the STILL-LIVE legacy messageQueue (design §7,
// stage 2 step 1 explicitly migrates only tryReserveSession/
// releaseSessionReservation — QueueMessage itself, and its callers
// InterruptAndSend/requeueInterruptMessage in coordinator.go, are migrated in
// a LATER step and still append directly to a.messageQueue, not the
// mailbox), now ALSO folding in the OS-level session lock release (round 11
// review, HIGH-1).
//
// Before this, the mailbox flipped to mbIdle — making IsSessionBusy/
// IsBusy/submit(), all same-process, in-memory reads under mb.mu, observe
// "not busy" — strictly BEFORE lk (the OS-level inter-process session lock)
// was actually released: that only happened much later, once runTurn
// returned, its own deferred wg.Wait() (title generation, up to several
// seconds) finished, control returned to Run's loop, and Run itself decided
// to return, running ITS deferred lk.Release(). A same-process caller that
// saw "not busy" in that window and tried to become the new owner via
// submit() + TryAcquireSessionLock got a spurious SessionLockBusyError from
// its own prior turn — and since tryReserveSession's "someone already owns
// it" branch never re-queues on that error path, the message was silently
// dropped, not just delayed.
//
// lk is the SAME *session.SessionLock Run() acquired once for the whole
// call (nil when a.dataDir == ""), and runCancel is Run's own whole-call
// CancelFunc (design §4 / round 11 review MEDIUM-1): if the legacy queue
// reclaims ownership for another turn, dispatcherCancel must keep pointing
// at something live (runCancel, exactly what tryReserveSession/submit()
// already stores there for the "no live generation yet" window) rather than
// being left nil until the reclaimed turn's own beginGeneration call —
// otherwise a Cancel()/InterruptAndReplace() landing in that narrow window
// would silently no-op (Cancel) or falsely report success while cancelling
// nothing (InterruptAndReplace).
//
// The whole operation is one mailbox.drainOrReleaseFinal call, with exactly
// FOUR possible outcomes (corrected here — #297 review: an earlier version
// of this doc claimed only three cases and asserted "lk is simply never
// released for [a same-era reclaim] handoff" as the invariant covering
// every hasNext==true return, which #296/P1-C's first draft violated by
// adding a hand-back that returned hasNext==true AFTER lk had already been
// released):
//  1. mb.submitted (checked first) or mb.replacement is non-empty at the
//     time of the call, BEFORE any release attempt: pop and return it;
//     state stays mbOwned; lk is NOT touched (still held for the reclaimed
//     turn). The caller's loop runs it as the next turn under the SAME lk.
//  2. Both empty: check the legacy messageQueue (still under mb.mu — safe,
//     see drainOrReleaseFinal's doc on csync.KeyedQueue.PopFront having no
//     callbacks of its own). Non-empty: reclaim ownership for the SAME era
//     (no epoch bump — mirrors the old reclaimSameEra contract) and return
//     it; lk again NOT touched — "the OS lock is simply never released for
//     this handoff" is accurate for cases 1 and 2, and ONLY for these two.
//  3. All three empty: release lk (if non-nil) — now OUTSIDE mb.mu (#296/
//     P1-C: mb.state == mbReleasing stands in for the mutex so a hung
//     filesystem cannot stall the control plane) — then flip mb.state to
//     mbIdle. No same-process observer can see "not busy" until the OS lock
//     genuinely is gone (mbReleasing reads as busy throughout), closing the
//     HIGH-1 window described above.
//  4. Work (a submit()/interruptAndReplace()) races into mb.submitted/
//     mb.replacement WHILE case 3's release() is running. This is the ONLY
//     case where drainOrReleaseFinal itself CANNOT decide "keep this turn
//     loop running" — by the time it notices, release() has ALREADY run and
//     lk is gone (or its fate unknown, on error/panic). It therefore drains
//     that work out as `orphaned` and — critically — still ends at mbIdle
//     with hasNext==false, exactly like case 3. hasNext==true now means,
//     and only means, "lk is still held, keep the current turn loop going
//     on it" — cases 1 and 2 exclusively. `orphaned` is a DIFFERENT signal:
//     "this work has no lock and no turn loop; someone must start a fresh
//     one for it", handled below exactly like coordinator.startDetachedRun's
//     existing P0-B idle-session contract (mailbox.interruptAndReplace's own
//     doc: "the caller should instead start a fresh Run() with call
//     directly" — case 4 is that same contract, reached via a different
//     door). Never treat orphaned as a queue the CURRENT lk can serve.
//
// This is why checkLegacy is tried BEFORE releasing lk: reclaiming behind
// the SAME lock avoids the release-then-reacquire shape, which would open a
// real cross-process race (a different process could win the OS lock in the
// gap) — see the old reclaimSameEra doc (removed; superseded by this
// function) for the fuller rationale, still applicable here.
//
// ONLY safe to call from a live turn loop that will actually run the
// returned call as its next turn (runTurn's own end-of-turn drain). Run's
// cleanup defer, which has no turn loop left to hand a "yes, keep going"
// answer to, uses abandonOwnership instead (BLOCKER-2a) — calling THIS
// from there could reclaim the legacy queue into a brand new turn that
// then never runs, AND would release lk from the wrong place (Run's own
// deferred lk.Release() already owns that responsibility on that path).
func (a *sessionAgent) drainOrReleaseMerged(sessionID string, epoch uint64, lk *session.SessionLock, runCancel context.CancelFunc) (SessionAgentCall, bool) {
	checkLegacy := func() (SessionAgentCall, bool) {
		return a.messageQueue.PopFront(sessionID)
	}
	var release func() error
	if lk != nil {
		release = lk.Release
	}
	next, hasNext, releaseErr, orphaned := a.getMailbox(sessionID).drainOrReleaseFinal(epoch, checkLegacy, runCancel, release)
	if releaseErr != nil {
		slog.Debug("agent.drainOrReleaseMerged: release session lock failed", "session_id", sessionID, "err", releaseErr)
	}
	// Case 4 (see doc above): work raced in during release() and cannot be
	// handed to the current (now lock-less) turn loop. Each orphaned call
	// gets its own independent, detached restart — the SAME mechanism
	// coordinator.startDetachedRun already uses for InterruptAndSend/
	// requeueInterruptMessage's "session was idle" case, since that is
	// exactly what this now is: from orphaned's perspective the session is
	// idle (mbIdle, no lk held) the instant drainOrReleaseFinal returned.
	a.restartOrphaned(orphaned)
	if !hasNext {
		return SessionAgentCall{}, false
	}
	return next, true
}

// restartOrphaned starts one independent, detached a.Run call per entry in
// calls — the sessionAgent-level equivalent of coordinator.startDetachedRun
// (P0-B), needed here because drainOrReleaseMerged/drainOrReleaseFinal have
// no access to a *coordinator (agent and coordinator are separate layers;
// coordinator wraps sessionAgent, not the other way around).
//
// Each call gets its OWN goroutine and its own context.Background(): the
// caller (drainOrReleaseMerged, called from runTurn, called from Run's turn
// loop) is about to return control up its own call stack, possibly all the
// way out of Run() entirely — there is no request-scoped ctx left in scope
// whose cancellation would be meaningful to inherit, and even if there were,
// cancelling it would recreate the exact "accepted but never executed"
// outcome startDetachedRun's own doc warns against. a.Run performs its own
// full acquisition (mailbox.submit + a fresh session.TryAcquireSessionLock)
// for each one — none of these reuse the lk that drainOrReleaseFinal already
// released; see drainOrReleaseFinal's own doc for why reusing it is exactly
// the bug this function exists to close.
//
// Losing a race to a concurrent Run() that claims the mailbox first is safe
// and needs no coordination (same reasoning as startDetachedRun's doc): if
// another owner appears between orphaning and this call's own submit(), this
// call's submit() simply queues behind that new owner instead of becoming
// owner itself, and that owner's own end-of-turn drain picks it up. The one
// outcome that cannot happen is the call going unrun.
func (a *sessionAgent) restartOrphaned(calls []SessionAgentCall) {
	for _, call := range calls {
		call := call
		go func() {
			if _, err := a.Run(context.Background(), call); err != nil {
				slog.Error("agent: detached restart for a call orphaned by a concurrent session-lock release failed",
					"session_id", call.SessionID, "err", err)
			}
		}()
	}
}

// activityNotifyContextKey is the context key under which runTurn stores a
// composed "activity happened" callback (see withActivityNotify). It is
// unexported and local to this package: unlike tools.SessionIDContextKey,
// nothing outside package agent needs to read or set it directly — sub-agent
// delegation (the "agent" tool, agentic_fetch_tool) just forwards ctx
// unmodified, which is enough for the value to keep flowing.
type activityNotifyContextKey struct{}

// withActivityNotify returns a child of ctx carrying a composed
// activity-notify callback: calling the returned callback records activity
// on lk (nil-receiver-safe, see SessionLock.RecordActivity) AND forwards to
// any ancestor notify callback already present on ctx.
//
// The "read the ancestor before overwriting" composition is what makes
// multi-level delegation chains (grandparent -> parent -> child) work: each
// level's runTurn calls withActivityNotify once on its own ctx before
// deriving genCtx from it, so a grandchild session's activity notify walks
// all the way back up through every ancestor session's lock, not just its
// immediate parent's. This is how a parent session's heartbeat stays alive
// purely from a delegated sub-agent's progress while the parent itself is
// blocked inside the "agent"/agentic_fetch tool call, producing no stream
// callbacks of its own.
func withActivityNotify(ctx context.Context, lk *session.SessionLock) context.Context {
	ancestor, _ := ctx.Value(activityNotifyContextKey{}).(func())
	self := func() {
		lk.RecordActivity() // nil-receiver-safe: no-op when lk is nil (a.dataDir == "")
		if ancestor != nil {
			ancestor()
		}
	}
	return context.WithValue(ctx, activityNotifyContextKey{}, self)
}

// notifyActivity invokes the composed activity-notify callback stored on ctx
// by withActivityNotify, if any. Safe to call on a ctx that never had
// withActivityNotify applied (e.g. in tests, or any ctx not descended from a
// runTurn call) — it is then a harmless no-op.
func notifyActivity(ctx context.Context) {
	if notify, ok := ctx.Value(activityNotifyContextKey{}).(func()); ok && notify != nil {
		notify()
	}
}

// Run executes one or more agent turns for call.SessionID. It owns the
// session's busy reservation and the inter-process OS lock for the whole
// call, including any turns generated by queue-drain (a queued message
// picked up after the current turn ends, or after a mid-turn cancel, or
// after an in-turn /compact). Those used to be handled by Run calling
// itself recursively from three places deep in its own body — which, since
// the OS lock (see below) is not reentrant even within one process, deadlocked
// against itself: the recursive call tried to acquire a lock its own parent
// stack frame was still holding (the parent's `defer ipcLock.Release()`
// hadn't run yet), and got rejected with "already in use". runTurn below is
// the extracted single-turn body; Run just loops it.
func (a *sessionAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	if call.Prompt == "" && !message.ContainsTextAttachment(call.Attachments) {
		return nil, ErrEmptyPrompt
	}
	if call.SessionID == "" {
		return nil, ErrSessionMissing
	}
	// Refuse outright once shutdown has begun (closing review, blocker 1).
	// Checked BEFORE tryReserveSession so no ownership is claimed and no
	// mailbox is created: the per-mailbox `stopped` latch can only stop an
	// existing turn loop, and CancelAll's sweep cannot reach a mailbox that
	// does not exist yet. See the shuttingDown field's doc for the call
	// paths that reach here after the sweep.
	if a.shuttingDown.Load() {
		return nil, ErrAgentShuttingDown
	}

	// runCtx/runCancel span the WHOLE dispatcher (every turn + every
	// preamble). The loop derives a per-turn turnCtx from runCtx (#284) so
	// that cancelling a single turn's preamble — via InterruptAndReplace —
	// does NOT kill the dispatcher: runCancel is never the target of an
	// interrupt; it is invoked only by this defer and by CancelAll's
	// hardStop. tryReserveSession stores runCancel as dispatcherCancel, the
	// mailbox's durable fallback when no live generation cancel exists yet.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	// Atomically check-and-claim the busy slot via the session's mailbox
	// (mailbox.submit, design §3). If someone else already owns the
	// session, submit has already appended call to the mailbox's own
	// pending queue under the same lock — nothing left to do here.
	became, epoch := a.tryReserveSession(call, runCancel)
	if !became {
		return nil, nil
	}
	// We now own call.SessionID's reservation, under ownership era `epoch`,
	// for the entire loop below, including every queue-drain turn. Released
	// exactly once, whichever way the loop ends, via THIS defer.
	//
	// Round 9 review, BLOCKER-2: this defer fires on EVERY return from Run
	// — not just the pre-loop bail-out paths its old doc comment claimed —
	// including after runTurn's own final drain already released the era
	// (the common, correct case) and after any early-return inside runTurn
	// that skipped its own drain entirely (an error path). It must
	// therefore be safe to call unconditionally on every exit, which is
	// exactly what abandonOwnership (not drainOrReleaseMerged) provides:
	//   - If `epoch` no longer matches the mailbox's current epoch, the era
	//     already ended (either via runTurn's own drain, or a concurrent
	//     submit became a NEW owner in the meantime) — a safe no-op, since
	//     whatever is in the mailbox now belongs to that other owner. The
	//     OLD code called drainOrRelease/drainOrReleaseMerged unconditionally
	//     here with no such check, so on this path it could silently flip a
	//     DIFFERENT, still-live owner's session back to idle out from under
	//     it.
	//   - If `epoch` still matches, there is no live turn loop left (this
	//     defer IS the end of Run) to hand a "keep going" answer to, so
	//     unlike the loop's own drainOrReleaseMerged this always ends the
	//     era at idle, re-queueing (not silently keeping, and not silently
	//     dropping while leaving the session permanently owned) anything it
	//     finds still queued. The OLD code's "found something" branch left
	//     state == mbOwned with nobody running it — the session was wedged
	//     busy forever, since nothing else would ever drain it again.
	//
	// Round 9 review round 2, HIGH-2: an earlier version of this defer
	// logged dropped calls at slog.Error and discarded them — a silent
	// user-message loss reachable on any ordinary non-cancel turn error
	// (provider 5xx, a DB write failure) with a second message queued
	// concurrently, not just the rare pre-loop bail-out case. Pre-mailbox-
	// migration, a message in this position lived in messageQueue and was
	// picked up by the NEXT Run() call's own PrepareStep drain — not lost.
	// Re-queueing into messageQueue here (which is not epoch-scoped, so it
	// survives safely across ownership eras) restores exactly that
	// behavior instead of introducing a new one.
	defer func() {
		dropped, hadWork := a.getMailbox(call.SessionID).abandonOwnership(epoch)
		if hadWork {
			for _, d := range dropped {
				a.messageQueue.Append(call.SessionID, d)
			}
			slog.Error(
				"agent.Run: re-queued call(s) that were pending when ownership had to be abandoned — no turn loop left to run them this call; a future Run() will pick them up",
				"session_id", call.SessionID,
				"count", len(dropped),
			)
		}
	}()

	// Inter-process session lock. The reservation above is per-process (an
	// in-memory map); two crush processes wouldn't see each other's busy
	// state and could both start streaming into the same session id — the
	// accidental-double-spawn race documented in the parallel-process audit
	// (#6 CRITICAL). The OS-level lock auto-releases on process death, so a
	// crashed holder never leaves a stuck session id.
	//
	// Sub-agents are NOT exempt: a sub-agent runs under its own CHILD
	// session id (parentMessageID$$toolCallID, see
	// session.CreateAgentToolSessionID), which is a completely different
	// id from the parent's call.SessionID. The parent's lock only covers
	// the parent's own session id — the child id is otherwise unlocked,
	// so a second crush process opening that exact child session (e.g.
	// via `crush sessions pick`/`resume`) could acquire it and stream into
	// it concurrently with this in-process sub-agent run. Locking must
	// happen per session id, regardless of isSubAgent.
	//
	// This does not introduce false-positive "already in use" errors for
	// legitimate same-process reentrancy (e.g. the "agent" tool's
	// resume_session_id path racing a still-active run on that same child
	// id): the in-process reservation above (tryReserveSession/mailbox.submit)
	// already queues that case behind the current in-process owner and
	// returns before this point is ever reached — AS LONG AS the mailbox
	// still reports that owner as busy. Round 11 review, HIGH-1: before that
	// fix, drainOrReleaseMerged could flip the mailbox to mbIdle (making
	// IsSessionBusy/tryReserveSession see "not busy") strictly BEFORE this
	// same process's own prior Run() call had actually released ITS lk —
	// runTurn's deferred wg.Wait() and the rest of the call stack unwinding
	// still hadn't run. A second same-process Run() call for the identical
	// session id could then become the new in-process owner (submit()
	// legitimately returns becomeOwner=true) and reach TryAcquireSessionLock
	// here while the OS lock was still, transiently, held by its own
	// process's outgoing turn — a same-process false "already in use".
	// drainOrReleaseMerged now releases lk inside the same mailbox critical
	// section that flips state to mbIdle (see mailbox.drainOrReleaseFinal),
	// so "not busy" and "OS lock free" become true together — this window
	// is closed. Two sub-agent invocations
	// spawned in parallel by fantasy's ParallelAgentTool likewise never
	// collide here — each gets a distinct toolCallID and therefore a
	// distinct child session id.
	//
	// Acquired ONCE for the whole loop below — every queue-drain turn reuses
	// this same lock instead of each one acquiring (and needing to release)
	// its own, which is what made the old recursive-Run() shape deadlock.
	var lk *session.SessionLock
	if a.dataDir != "" {
		var lockErr error
		lk, lockErr = session.TryAcquireSessionLock(a.dataDir, call.SessionID)
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
			// Unidentified error (not "busy") — e.g. permission denied,
			// IO error, or any other failure that isn't "someone else
			// holds this lock". Previously this case logged a warning
			// and continued WITHOUT the inter-process guard, which
			// defeats the whole point: the in-process busy check only
			// protects against races inside this one process, not the
			// cross-process double-spawn this lock exists for. Fail
			// closed instead — refuse to run rather than silently
			// proceed unprotected.
			slog.Error("agent.Run: failed to acquire inter-process session lock, refusing to run unprotected",
				"session_id", call.SessionID, "err", lockErr)
			return nil, fmt.Errorf("session %q: could not acquire session lock: %w", call.SessionID, lockErr)
		}
		defer func() {
			if relErr := lk.Release(); relErr != nil {
				slog.Debug("agent.Run: release session lock failed", "session_id", call.SessionID, "err", relErr)
			}
		}()
	}

	// Turn loop: replaces the three recursive a.Run(ctx, ...) call sites
	// that used to live inside runTurn's body (cancel-drain, end-of-turn
	// drain, and the /compact drain in runSummarizeBody). Each iteration
	// runs exactly one provider turn; runTurn reports whether another
	// queued call should run next, using the reservation and OS lock
	// acquired once above instead of re-acquiring them. runCtx (not the
	// original ctx) is passed through so runCancel — registered as the
	// busy slot's CancelFunc — actually reaches every turn's genCtx.
	for {
		// Test-only seam (#289): fires strictly after any end-of-turn drain
		// has already released mb.mu and strictly before this iteration's
		// beginGeneration call below re-arms it. nil (a no-op) in every
		// production path — see mailbox.testLoopRearmSeam's own doc for why
		// this exists and what it lets a test do that it otherwise could
		// not: deterministically land a concurrent Cancel() inside the
		// window between a reclaim and the loop's own re-arm, instead of
		// racing the two for the next mb.mu acquisition.
		mb := a.getMailbox(call.SessionID)
		if mb.testLoopRearmSeam != nil {
			mb.testLoopRearmSeam()
		}
		// Per-turn context, derived from runCtx but independently
		// cancelable. An interrupt during this turn's DB preamble (before
		// runTurn creates genCtx) targets THIS cancel — not runCancel — so
		// the dispatcher (this loop, plus runCtx) survives the interrupt and
		// can atomically decide what to do next via runTurn's own
		// drainAfterCancel recovery. Before #284 the loop registered
		// runCancel for the preamble window, so an InterruptAndReplace
		// during the preamble killed the entire dispatcher and the
		// replacement was stranded in the legacy queue with no runner.
		turnCtx, turnCancel := context.WithCancel(runCtx)
		a.activeRequests.Set(call.SessionID, turnCancel)
		// Mirror the activeRequests re-arm into the mailbox's current
		// generation cancel (design §4): Cancel(sessionID) targets
		// mailbox.current.cancel, so it must be populated for the whole
		// preamble (until runTurn swaps in its own per-turn genCtx cancel
		// below). Using turnCancel (not runCancel) is the #284 fix: the
		// preamble is now part of a cancelable generation that is SEPARATE
		// from the durable dispatcher cancel.
		mb.beginGeneration(turnCancel)
		result, err, next, hasNext := a.runTurn(turnCtx, call, lk, epoch, runCancel)
		turnCancel()
		if !hasNext {
			return result, err
		}
		call = next
	}
}

// drainDueInjects is PrepareStep's mailbox-inject drain (design §5, stage
// 2.4), pulled out into a named method for the same reason handleWatchdogFire
// below was (task #243): a test must be able to drive the REAL production
// logic instead of a copy of it. The first version of this lived inline in
// the PrepareStep closure and its tests re-implemented the drain-and-dedup
// in a local helper — a mirror that passes whether or not production still
// matches it, which is exactly the shape #243 exists to stop.
//
// The rows are ALREADY in the DB (InjectMessage persisted them via
// createUserMessage), so callers only splice them into the current prompt —
// no second Create call.
//
// Two rules, one per half of P1-1:
//   - drainInjects(genID) returns only entries stamped at or before THIS
//     turn's generation, so an inject that landed after a previous turn's
//     last PrepareStep is picked up by this turn rather than stranded (the
//     LOSS half).
//   - historyIDs skips any inject whose row the preamble's
//     getSessionMessages already loaded, so a DB write that raced ahead of
//     the preamble does not appear both in history and in the splice (the
//     DUPLICATION half).
func (a *sessionAgent) drainDueInjects(sessionID string, genID uint64, historyIDs map[string]struct{}) []message.Message {
	due := a.getMailbox(sessionID).drainInjects(genID)
	if len(due) == 0 {
		return nil
	}
	spliced := make([]message.Message, 0, len(due))
	for _, inj := range due {
		if _, ok := historyIDs[inj.msg.ID]; ok {
			continue
		}
		spliced = append(spliced, inj.msg)
	}
	return spliced
}

// handleWatchdogFire is runTurn's stream-watchdog onFire callback, pulled out
// into a named method (task #243) so a unit test can invoke the REAL
// production logic directly — constructing a minimal *sessionAgent and
// calling this method with a synthetic watchdogCauseVal — instead of only
// exercising a test-local copy of this shape that could silently drift from
// what runTurn actually wires up. The onFire closure inside runTurn is now
// just a thin dispatch: `func(elapsed, cause) { a.handleWatchdogFire(...) }`.
//
// watchdogCauseVal is runTurn's local atomic.Int32 (not a sessionAgent
// field: it is genuinely per-turn state, reset fresh on every call), passed
// in by pointer so this method can store into the caller's copy.
// toolMaxDuration/idleTimeout are likewise runTurn locals (the resolved,
// possibly-overridden effective durations for THIS turn) rather than
// sessionAgent fields, so they are passed explicitly instead of read off a.
// largeModel is the SAME kind of runTurn-local snapshot (taken once at turn
// start, agent.go: `largeModel := a.largeModel.Get()`), NOT a fresh re-read
// of a.largeModel here: a.largeModel is mutable mid-turn via SetModels
// (coordinator.UpdateModels / web-UI override path), so re-reading at fire
// time would name the model the user SWITCHED TO after the hang started
// rather than the one that actually hung (task #252 — the #243 extraction
// regressed exactly this by re-reading a.largeModel.Get() here).
//
// INVARIANT (task #227 + #232, preserved verbatim by this extraction): the
// cause is stored FIRST, synchronously, before any other work in this
// method. startStreamWatchdog invokes onFire strictly before cancel(), so
// whatever this method does before returning is on the critical path to
// unblocking agent.Stream. If the OUTER ctx is also cancelled independently
// of this watchdog (Ctrl-C, a --timeout firing at the same moment) while
// this method is still running, the main goroutine reading watchdogCauseVal
// after observing stalled==true must never see the zero value
// (causeIdleStall) for what was actually a hard-cap/tool-timeout fire — that
// would misreport the cause via a DIFFERENT race than the one #227 fixed
// (external cancellation racing this callback, rather than this callback
// racing its own cancel()).
func (a *sessionAgent) handleWatchdogFire(
	cause watchdogCause,
	elapsed time.Duration,
	sessionID string,
	watchdogCauseVal *atomic.Int32,
	toolMaxDuration, idleTimeout time.Duration,
	largeModel Model,
) {
	watchdogCauseVal.Store(int32(cause))
	// The watchdog firing IS the hang, caught at the only moment the
	// evidence still exists. Capture every goroutine's stack now,
	// SYNCHRONOUSLY: pprof is gated behind CRUSH_PROFILE (so it can't be
	// turned on after the fact) and release builds strip symbols (so a
	// debugger attach yields nothing and merely kills the process). Without
	// this, every production hang is diagnosed by guesswork.
	//
	// CaptureGoroutineStack does no I/O — it's runtime.Stack plus a string
	// header — so it cannot block on a stuck disk, and running it
	// synchronously here is what guarantees the snapshot reflects the
	// actual moment of the hang. Capturing it from an async goroutine
	// instead (as an earlier version of this fix did) would let it run
	// AFTER cancel()/unwind had already started — or, if the process exited
	// or was force-killed first, never run at all — defeating the entire
	// point of a diagnostic taken "at the only moment it is still
	// available" (see the doc comment on CaptureGoroutineStack).
	stackDump := crushlog.CaptureGoroutineStack("stream watchdog fired")
	// Only the WRITE is dispatched async and NOT awaited: WriteGoroutineDump
	// does a synchronous os.WriteFile with no timeout of its own (see its
	// doc in internal/log/goroutine_dump.go). Since onFire now runs
	// strictly before cancel(), awaiting a write that hangs (e.g. the log
	// directory sits on a stuck network/SMB mount) would mean cancel()
	// never runs, agent.Stream never unblocks, and runTurn's deferred
	// <-wd.done blocks forever — a full process freeze, exactly the failure
	// mode this watchdog exists to prevent. The dump is best-effort
	// diagnostics only; nothing downstream needs the write to complete
	// before the turn can safely unwind, so firing it off and returning
	// immediately preserves the already-captured evidence without putting
	// an unbounded disk write on cancellation's critical path.
	go func() {
		if dumpPath, dumpErr := crushlog.WriteGoroutineDump(stackDump); dumpErr != nil {
			slog.Warn("agent: failed to write goroutine dump for watchdog fire", "err", dumpErr)
		} else {
			slog.Warn("agent: wrote goroutine dump for watchdog fire", "path", dumpPath)
		}
	}()
	switch cause {
	case causeToolTimeout:
		slog.Warn(
			"agent: watchdog firing — tool execution exceeded cap, force-cancelling",
			"session_id", sessionID,
			"provider", largeModel.ModelCfg.Provider,
			"model", largeModel.ModelCfg.Model,
			"elapsed", elapsed.String(),
			"cap", toolMaxDuration.String(),
		)
	case causeHardCap:
		slog.Warn(
			"agent: watchdog firing — turn exceeded --timeout-hard-cap, force-cancelling",
			"session_id", sessionID,
			"provider", largeModel.ModelCfg.Provider,
			"model", largeModel.ModelCfg.Model,
			"elapsed", elapsed.String(),
			"hard_cap", a.timeoutHardCap.String(),
		)
	default:
		slog.Warn(
			"agent: stream watchdog firing — no provider activity, force-cancelling",
			"session_id", sessionID,
			"provider", largeModel.ModelCfg.Provider,
			"model", largeModel.ModelCfg.Model,
			"idle_duration", elapsed.String(),
			"threshold", idleTimeout.String(),
		)
	}
}

// runTurn executes exactly one agent turn (one call into fantasy's
// agent.Stream, plus all of Run's surrounding bookkeeping: DB preamble,
// stream watchdog, checkpointing, error/cancel handling, and auto-summarize
// triggering). It assumes the caller (Run) already holds call.SessionID's
// busy reservation and, when configured, the inter-process OS lock — runTurn
// itself never acquires either.
//
// hasNext reports whether another turn should run immediately (a message was
// queued during this turn, e.g. via the "interrupt and send" flow, a normal
// end-of-turn queue check, or a /compact drain) with next set to that call;
// the caller's loop is expected to invoke runTurn(ctx, next) again in that
// case. When hasNext is false, result/err are Run's final return values.
func (a *sessionAgent) runTurn(ctx context.Context, call SessionAgentCall, lk *session.SessionLock, epoch uint64, runCancel context.CancelFunc) (res *fantasy.AgentResult, resErr error, next SessionAgentCall, hasNext bool) {
	// Copy mutable fields under lock to avoid races with SetTools/SetModels.
	agentTools := a.tools.Copy()
	// One immutable snapshot for the whole turn (task #265). Resolving these
	// individually here used to mean a concurrent session's
	// applyModelOverrides could land BETWEEN the reads, so a single turn ran
	// with a mismatched model/prompt pair — and the next turn silently
	// inherited another session's model.
	cfg := a.resolveTurnConfig(call)
	largeModel := cfg.largeModel
	systemPrompt := cfg.systemPrompt
	promptPrefix := cfg.promptPrefix

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

	// Bounded: see sessionPreambleMaxDurationDefault doc. No watchdog is
	// running yet at this point in Run(), so an unbounded ctx here can hang
	// the turn forever with zero diagnostics if the single DB writer
	// connection is wedged.
	preambleCtx, preambleCancel := context.WithTimeout(ctx, a.effectiveSessionPreambleMaxDuration())
	currentSession, err := a.sessions.Get(preambleCtx, call.SessionID)
	if err != nil {
		preambleCancel()
		// #284: the preamble runs inside a per-turn cancelable context
		// (turnCtx, derived from runCtx). An InterruptAndReplace during
		// the preamble cancels that context without killing the
		// dispatcher. If the preamble failed because of a cancellation,
		// the mailbox may hold a replacement that runTurn's own
		// drainAfterCancel path never reached (it only runs after
		// agent.Stream returns, and we never got there). Recover it now
		// so the loop runs it as the next turn.
		if errors.Is(err, context.Canceled) {
			if next, ok := a.getMailbox(call.SessionID).drainAfterCancel(); ok {
				return nil, nil, next, true
			}
		}
		return nil, fmt.Errorf("failed to get session: %w", err), SessionAgentCall{}, false
	}

	msgs, err := a.getSessionMessages(preambleCtx, currentSession)
	if err != nil {
		preambleCancel()
		if errors.Is(err, context.Canceled) {
			if next, ok := a.getMailbox(call.SessionID).drainAfterCancel(); ok {
				return nil, nil, next, true
			}
		}
		return nil, fmt.Errorf("failed to get session messages: %w", err), SessionAgentCall{}, false
	}

	// Generate the title on the first message — OR self-heal on a later turn
	// when the session is still nameless. Title generation is best-effort and
	// a transient provider blip (z.ai overload, a token-limit truncation) on
	// turn 1 used to doom the session to "Untitled Session" forever, since it
	// only ever fired at len(msgs)==0. Retrying while the title is still
	// empty/default lets the next message recover it; it stops the moment a
	// real title lands. needsTitle is decided here (before the preamble ctx
	// is cancelled below) but the goroutine itself is launched further down,
	// after genCtx exists — see the wg.Go call site near genCtx's creation.
	needsTitle := len(msgs) == 0 ||
		currentSession.Title == "" ||
		currentSession.Title == DefaultSessionName

	// Add the user message to the session. Skip creation when the call
	// references a message that already exists in the DB (interrupt-inject
	// path: `crush sessions inject --interrupt` created the row before
	// signalling this process). Creating it again would duplicate it in
	// history — the referenced message is already the newest user message.
	if call.ExistingMessageID == "" {
		_, err = a.createUserMessage(preambleCtx, call)
		if err != nil {
			preambleCancel()
			if errors.Is(err, context.Canceled) {
				if next, ok := a.getMailbox(call.SessionID).drainAfterCancel(); ok {
					return nil, nil, next, true
				}
			}
			return nil, err, SessionAgentCall{}, false
		}
	}
	preambleCancel()

	// Add the session to the context.
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, call.SessionID)
	ctx = context.WithValue(ctx, cliprovider.SessionIDContextKey, call.SessionID)
	ctx = context.WithValue(ctx, cliprovider.ReasoningEffortContextKey, currentSession.LargeModelReasoningEffort)
	// Compose this turn's activity-notify callback with any ancestor's (see
	// withActivityNotify) BEFORE deriving genCtx, so every fantasy stream
	// callback below — via bumpActivity -> notifyActivity(genCtx) — records
	// activity on this session's own lock AND propagates up the whole
	// delegation chain to every ancestor session currently blocked waiting
	// on this one (task #214, the "pulse on any activity of the agent OR
	// its sub-agent(s)" directive).
	ctx = withActivityNotify(ctx, lk)

	genCtx, cancel := context.WithCancel(ctx)
	// Overwrite the placeholder no-op CancelFunc that tryReserveSession
	// stored in Run() with the real one for this turn. The reservation
	// itself (i.e. the map entry existing at all under call.SessionID) is
	// owned and released by Run(), not per-turn — see the removed
	// `defer a.activeRequests.Del(call.SessionID)` note below.
	a.activeRequests.Set(call.SessionID, cancel)
	// Record this turn's genCtx cancel as the mailbox's current generation
	// cancel (design §4): InterruptAndReplace returns THIS func (so a
	// mid-stream interrupt cancels only this generation, leaving the Run
	// turn loop alive to drain the replacement), and Cancel(sessionID)
	// targets it for a bare abort. Redundant with Run's loop-level
	// beginGeneration(runCancel) only for the brief preamble window this
	// call closes; from here until the turn returns, THIS is the live
	// cancel an interrupt must hit.
	genID := a.getMailbox(call.SessionID).beginGeneration(cancel)

	// Closed by the title goroutine when it returns; nil when no title was
	// requested. The deferred bounded join below selects on it. A
	// sync.WaitGroup used to serve this role, but a WaitGroup can only be
	// waited on unconditionally — and an unconditional wait is exactly the
	// defect being fixed here (see the join's own comment).
	var titleDone chan struct{}
	if needsTitle {
		// Derived from genCtx (not the outer ctx runTurn was called with) so
		// the stream watchdog cancelling genCtx — idle timeout, tool
		// timeout, hard cap — also cuts off an in-flight title generation
		// instead of leaving it to run on an unbounded parent context. Also
		// independently capped by effectiveTitleGenerationMaxDuration as a
		// backstop: generateTitle's two model attempts are each a blocking
		// agent.Stream with no timeout of their own, so a provider that
		// never returns (hung connection, stream never closed) must not be
		// able to keep the deferred join below from returning even if
		// genCtx's own cancellation somehow doesn't unblock it.
		titleCtx, titleCancel := context.WithTimeout(genCtx, a.effectiveTitleGenerationMaxDuration())
		titleDone = make(chan struct{})
		go func() {
			defer close(titleDone)
			defer titleCancel()
			a.generateTitle(titleCtx, call.SessionID, call.Prompt, cfg)
		}()
	}
	// Bounded join (P1-B). This used to be a bare `defer wg.Wait()`, which
	// is only as bounded as the goroutine it waits on. generateTitle's two
	// attempts are each a blocking agent.Stream with no timeout of their
	// own, so the titleCtx deadline above only helps for a provider that
	// actually honours context cancellation. One that does not — a hung
	// connection, a transport stuck outside context-aware I/O — never
	// returns, and the bare Wait then held runTurn (and with it Run, the
	// session's mailbox ownership and its OS lock) open forever, on a turn
	// whose real work had already finished.
	//
	// The title is best-effort metadata; it must never be able to outlive
	// the turn it decorates. So we wait for it only up to a grace period
	// beyond its own deadline, and otherwise abandon it: titleCancel has
	// already fired via genCtx, the goroutine will exit whenever its
	// provider finally unblocks, and its own deferred Rename is written
	// against a detached context so a late completion still persists.
	defer func() {
		if titleDone == nil {
			return
		}
		select {
		case <-titleDone:
		case <-time.After(titleJoinGrace):
			slog.Warn(
				"agent: abandoning title generation that outlived its deadline — the turn is not held open for it",
				"session_id", call.SessionID,
				"grace", titleJoinGrace,
			)
		}
	}()

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
	toolMaxDuration := a.effectiveToolMaxDuration()
	toolCleanupGrace := a.effectiveToolCleanupGrace()
	var watchdogCauseVal atomic.Int32 // stores watchdogCause
	wd := startStreamWatchdog(
		genCtx, cancel, idleTimeout, streamWatchdogTick,
		// The closure itself is deliberately a thin dispatch to a named
		// method (task #243): a unit test can construct a minimal
		// *sessionAgent and call handleWatchdogFire directly, exercising
		// the REAL cause-store/dump-capture/dump-write ordering instead of
		// a test-local copy of this shape that could silently drift from
		// what agent.go actually does.
		func(elapsed time.Duration, cause watchdogCause) {
			a.handleWatchdogFire(cause, elapsed, call.SessionID, &watchdogCauseVal, toolMaxDuration, idleTimeout, largeModel)
		},
		a.timeoutExtendsOnProgress,        // Fork patch: batch 8
		a.timeoutHardCap,                  // Fork patch: batch 8
		toolMaxDuration,                   // never-freeze backstop, applies to every tool
		toolCleanupGrace,                  // buffer for a nested watchdog to unwind first
		func() { notifyActivity(genCtx) }, // task #222: keep the OS-lock heartbeat
		// alive from tool-in-flight ticks too, not just stream callbacks —
		// see startStreamWatchdog's recordActivity doc.
	)
	// The watchdog now calls recordActivity (== notifyActivity(genCtx))
	// internally on every bump(), so this wrapper can be just wd.bump.
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
	// NOTE: no `defer a.activeRequests.Del(call.SessionID)` here (unlike the
	// pre-turn-loop code). The busy reservation for call.SessionID is
	// claimed once and released once by Run(), covering every turn in the
	// loop — a per-turn Del here would drop the reservation between queued
	// turns, reopening the exact race tryReserveSession exists to close.
	// runTurn itself never calls a.activeRequests.Del(call.SessionID) at
	// all — only Run()'s own releaseSessionReservation does, exactly once,
	// after the whole turn loop ends (see Run()'s defer). Earlier revisions
	// of this comment described mid-loop Del(call.SessionID) calls inside
	// runTurn (a cancel-drain path and an end-of-turn queue-check); both
	// were removed when the turn loop was extracted into Run() — this
	// comment previously went stale describing code that no longer exists.
	//
	// Fork merge note (origin/main 6938dedd "perf: batch streaming message
	// updates"): upstream introduced a debounced flush layer in
	// message.Service. We removed that layer (see message/message.go fork
	// patch); our Notify() path goes through pubsub directly and Update()
	// writes synchronously, so there is nothing to flush here.

	history, files := a.preparePrompt(msgs, currentSession.Todos, call.Attachments...)

	// historyIDs is the dedup set for mailbox-injected messages (design §5):
	// an inject whose DB row was already loaded into msgs by this turn's
	// preamble must not be spliced again from the mailbox's injects queue.
	// drainInjects + this ID check together guarantee exactly-once delivery
	// regardless of whether the DB write landed before or after the
	// preamble's getSessionMessages call.
	historyIDs := make(map[string]struct{}, len(msgs))
	for _, m := range msgs {
		historyIDs[m.ID] = struct{}{}
	}

	var currentAssistant *message.Message
	var stepMessages []fantasy.Message
	var shouldSummarize bool
	// sanitizedToolCalls tracks tool call IDs whose input JSON was malformed
	// and got replaced with "{}" by sanitizeToolInput, so OnToolResult can
	// surface a clear error to the model instead of letting it silently
	// operate on empty args (or, worse, get stuck resending unparsable input
	// on every subsequent turn).
	sanitizedToolCalls := make(map[string]bool)

	// stepHistory accumulates every fantasy.StepResult seen by OnStepFinish,
	// in arrival order. fantasy's internal Run loop calls OnStepFinish for a
	// step BEFORE it evaluates StopWhen on that same step, so the
	// loop-detection StopWhen closure cannot set a flag in time for that
	// step's OnStepFinish. We therefore recompute loop detection directly in
	// OnStepFinish from our own history (the StopWhen closure still calls
	// hasRepeatedToolCalls independently to decide whether to break the loop
	// — a small amount of redundant compute is simpler than sharing state
	// across fantasy's OnStepFinish-before-StopWhen ordering boundary).
	var stepHistory []fantasy.StepResult

	// loopDetected / loopDetail are computed inside OnStepFinish (from
	// stepHistory) so the AddFinish call in the SAME callback invocation can
	// record a non-empty message/details. The finish REASON stays
	// FinishReasonEndTurn (a loop-detected stop is still a form of "done" and
	// must not be reclassified away from it — see the comment on loopDetail
	// in loop_detection.go); the distinction from a voluntary model finish
	// is carried in the Finish part's message/details text so an
	// operator/orchestrator can tell that a legitimate polling pattern may
	// have been truncated.
	var loopDetected bool
	var loopDetail loopDetail
	// peakHoursAbortErr is stashed by the peak-hours checks when they detect
	// the provider entered its window mid-turn. The checks must call
	// cancelFn() to break fantasy's agent loop (returning an error alone
	// doesn't stop it), but cancel() makes fantasy return context.Canceled —
	// swallowing the specific *PeakHoursError. After agent.Stream() returns,
	// Run() checks this and replaces the generic context.Canceled with the
	// real error so it reaches the coordinator and ultimately
	// RunNonInteractive's stderr output.
	var peakHoursAbortMu sync.Mutex
	var peakHoursAbortErr error
	setPeakHoursAbortErr := func(err error) bool {
		peakHoursAbortMu.Lock()
		defer peakHoursAbortMu.Unlock()
		if peakHoursAbortErr != nil {
			return false
		}
		peakHoursAbortErr = err
		return true
	}
	getPeakHoursAbortErr := func() error {
		peakHoursAbortMu.Lock()
		defer peakHoursAbortMu.Unlock()
		return peakHoursAbortErr
	}

	// silentCompactNeeded records that PrepareStep's sliding-window trim
	// fired this turn. The silent compact runs SYNCHRONOUSLY after the turn's
	// main work completes (under the turn's mailbox ownership), not as a
	// background goroutine — a goroutine that deleted messages concurrently
	// with the active turn was the P0-4 data-corruption bug (#268). Running
	// synchronously under the same ownership guarantees no concurrent turn or
	// compaction can touch the history while the compact's snapshot/delete
	// is in flight.
	var silentCompactNeeded bool

	// Fork patch: batch 8 — auto-checkpoint state for mid-stream
	// persistence. See CHANGELOG.fork.md section 6.
	//
	// Invariant: sessionLock (already declared above) protects EVERY
	// touch of currentAssistant — mutation, Clone(), and even a bare
	// len(Parts)/pointer read — because the checkpoint goroutine below
	// and the streaming callbacks (OnTextDelta, OnReasoningDelta,
	// OnToolInputStart, ...) run concurrently on separate goroutines.
	// message.Message.Clone() has no synchronization of its own, so a
	// snapshot must be taken while holding sessionLock.
	//
	// The lock must NEVER be held across a.messages.Update (the SQLite
	// write): every writer takes the lock, mutates/clones a private
	// snapshot, releases the lock, then calls Update on the snapshot
	// without the lock held. Otherwise each checkpoint tick or
	// DB-writing callback would stall the whole streaming loop for the
	// duration of a disk write. OnStepFinish drains the ticker and
	// stops the goroutine (via stopCheckpoint) before its final write;
	// the tail of Run() also calls stopCheckpoint() defensively before
	// touching currentAssistant, in case agent.Stream returned before
	// OnStepFinish ever ran (e.g. the very first provider call failed).
	var checkpointPartsLen int // last-flushed len(Parts), for coalescing
	// checkpointStop and checkpointDone are reborn on every step.
	// startCheckpoint allocates a fresh pair and launches the ticker
	// goroutine; stopCheckpoint closes checkpointStop — the goroutine's
	// dedicated exit signal — then waits on checkpointDone for it to
	// actually exit, then nils both so the next step starts clean. The
	// exit signal MUST be a dedicated channel, NOT genCtx.Done(): genCtx
	// stays alive for the whole body of Run (cancelled only by the
	// deferred cancel() at function return), so relying on it would force
	// stopCheckpoint to always hit its 5s backstop — the ~10s/turn stall
	// this code replaces. start/stop run on fantasy's single callback
	// goroutine / the Run goroutine (never concurrent with each other);
	// the ticker goroutine captures local channel refs at launch, so
	// nil-ing the outer vars after stop does not affect it. currentAssistant
	// access stays guarded by sessionLock below.
	var checkpointStop chan struct{}
	var checkpointDone chan struct{}
	startCheckpoint := func() {
		if a.checkpointInterval <= 0 || checkpointStop != nil {
			return
		}
		stop := make(chan struct{})
		done := make(chan struct{})
		checkpointStop = stop
		checkpointDone = done
		go func() {
			defer close(done)
			ticker := time.NewTicker(a.checkpointInterval)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-genCtx.Done():
					return
				case <-ticker.C:
					sessionLock.Lock()
					var snap message.Message
					haveSnap := false
					newPartsLen := checkpointPartsLen
					if currentAssistant != nil && len(currentAssistant.Parts) != checkpointPartsLen {
						snap = currentAssistant.Clone()
						snap.AddFinish(message.FinishReasonUnknown, "", "")
						for i := len(snap.Parts) - 1; i >= 0; i-- {
							if f, ok := snap.Parts[i].(message.Finish); ok {
								f.Partial = true
								snap.Parts[i] = f
								break
							}
						}
						newPartsLen = len(currentAssistant.Parts)
						haveSnap = true
					}
					sessionLock.Unlock()
					if haveSnap {
						if err := a.messages.Update(genCtx, snap); err != nil {
							slog.Debug(
								"agent: checkpoint flush failed",
								"session_id", call.SessionID,
								"message_id", snap.ID,
								"err", err,
							)
						} else {
							checkpointPartsLen = newPartsLen
						}
					}
				}
			}
		}()
	}
	stopCheckpoint := func() {
		if checkpointStop == nil {
			return
		}
		close(checkpointStop)
		checkpointStop = nil
		select {
		case <-checkpointDone:
		case <-time.After(5 * time.Second):
			slog.Warn(
				"agent: checkpoint goroutine did not exit within 5s of stop signal",
				"session_id", call.SessionID,
			)
		}
		checkpointDone = nil
	}

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
		sessionLock.Lock()
		if currentAssistant == nil {
			sessionLock.Unlock()
			return nil
		}
		msg := currentAssistant.Clone()
		sessionLock.Unlock()
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

	peakHoursWatchDone := make(chan struct{})
	if a.peakHoursCheck != nil {
		go func() {
			defer close(peakHoursWatchDone)
			ticker := time.NewTicker(peakHoursPollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-genCtx.Done():
					return
				case <-ticker.C:
					pErr := a.peakHoursCheck()
					if pErr == nil {
						continue
					}
					if !setPeakHoursAbortErr(pErr) {
						return
					}
					slog.Warn("agent: aborting — provider entered peak-hours mid-turn",
						"session_id", call.SessionID, "error", pErr)
					peakMsg, peakDetails := peakHoursStoppedFinishText(pErr)
					sessionLock.Lock()
					var snap message.Message
					haveSnap := currentAssistant != nil
					if haveSnap {
						currentAssistant.AddFinish(message.FinishReasonError, peakMsg, peakDetails)
						snap = currentAssistant.Clone()
					}
					sessionLock.Unlock()
					if haveSnap {
						flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
						if uErr := a.messages.Update(flushCtx, snap); uErr != nil {
							slog.Warn("agent: failed to persist peak-hours finish message", "error", uErr)
						}
						flushCancel()
					}
					if cancelFn, ok := a.activeRequests.Get(call.SessionID); ok {
						cancelFn()
					}
					return
				}
			}
		}()
		defer func() { cancel(); <-peakHoursWatchDone }()
	} else {
		close(peakHoursWatchDone)
	}

	// Don't send MaxOutputTokens if 0 — some providers (e.g. LM Studio) reject it
	var maxOutputTokens *int64
	if call.MaxOutputTokens > 0 {
		maxOutputTokens = &call.MaxOutputTokens
	}
	result, err := agent.Stream(genCtx, fantasy.AgentStreamCall{
		Prompt:           message.PromptWithTextAttachments(call.Prompt, call.Attachments),
		Files:            files,
		Messages:         history,
		Headers:          sessionHeaders(call.SessionID),
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

			queuedCalls := a.messageQueue.TakeAll(call.SessionID)
			for _, queued := range queuedCalls {
				// Interrupt-inject path: the message row already exists in the
				// DB (created by `crush sessions inject --interrupt`). Load it
				// by ID and splice it in — do NOT create a duplicate row.
				if queued.ExistingMessageID != "" {
					existingMsg, getErr := a.messages.Get(callContext, queued.ExistingMessageID)
					if getErr != nil {
						slog.Warn("queued interrupt inject references missing message, skipping",
							"session_id", call.SessionID, "message_id", queued.ExistingMessageID, "error", getErr)
						continue
					}
					prepared.Messages = append(prepared.Messages, existingMsg.ToAIMessage()...)
					continue
				}
				userMessage, createErr := a.createUserMessage(callContext, queued)
				if createErr != nil {
					return callContext, prepared, createErr
				}
				prepared.Messages = append(prepared.Messages, userMessage.ToAIMessage()...)
			}

			for _, inj := range a.drainDueInjects(call.SessionID, genID, historyIDs) {
				prepared.Messages = append(prepared.Messages, inj.ToAIMessage()...)
			}

			// Cross-process inject drain: rows written by another process
			// (`crush sessions inject`) into pending_injects. The message
			// row already exists in the DB (the CLI created it at inject
			// time for immediate web-UI visibility), so we only load it by
			// message_id and splice it in — no second Create, no dup row.
			// DrainPendingInjects deletes the consumed non-interrupt rows in
			// the same transaction (delete-after-read).
			pending, hasInterrupt, drainErr := a.sessions.DrainPendingInjects(callContext, call.SessionID)
			if drainErr != nil {
				return callContext, prepared, drainErr
			}
			if hasInterrupt {
				// Defensive: interrupt rows are meant to be consumed by the
				// interrupt ticker before PrepareStep runs. If one is still
				// here it is a race, not a normal path.
				slog.Warn("pending interrupt inject present during non-interrupt PrepareStep drain",
					"session_id", call.SessionID)
			}
			for _, inj := range pending {
				injMsg, getErr := a.messages.Get(callContext, inj.MessageID)
				if getErr != nil {
					// The referenced message vanished (e.g. cascade delete):
					// skip it rather than aborting the whole step.
					slog.Warn("pending inject references missing message, skipping",
						"session_id", call.SessionID, "message_id", inj.MessageID, "error", getErr)
					continue
				}
				prepared.Messages = append(prepared.Messages, injMsg.ToAIMessage()...)
				// The row was written by a foreign process (`crush sessions
				// inject`), so its Create() never published through THIS
				// process's message broker. If a web UI happens to be
				// attached to this process for the session, Notify pushes
				// the already-persisted message so it renders live instead
				// of waiting for a page reload.
				a.messages.Notify(injMsg)
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

						// Record that a silent compact is needed — it runs
						// synchronously AFTER the turn completes (under the
						// turn's mailbox ownership), not as a concurrent
						// goroutine. A goroutine deleting messages while the
						// turn is still streaming was the P0-4 data corruption
						// bug (#268).
						silentCompactNeeded = true
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
			sessionLock.Lock()
			currentAssistant = &assistantMsg
			sessionLock.Unlock()
			return callContext, prepared, err
		},
		OnReasoningStart: func(id string, reasoning fantasy.ReasoningContent) error {
			bumpActivity()
			slog.Debug("agent: OnReasoningStart called", "id", id)
			sessionLock.Lock()
			currentAssistant.AppendReasoningContent(reasoning.Text)
			snap := currentAssistant.Clone()
			sessionLock.Unlock()
			return a.messages.Update(genCtx, snap)
		},
		OnReasoningDelta: func(id string, text string) error {
			bumpActivity()
			slog.Debug("agent: OnReasoningDelta called", "len", len(text))
			sessionLock.Lock()
			currentAssistant.AppendReasoningContent(text)
			sessionLock.Unlock()
			return notifyUI()
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			bumpActivity()
			sessionLock.Lock()
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
			snap := currentAssistant.Clone()
			sessionLock.Unlock()
			return a.messages.Update(genCtx, snap)
		},
		OnTextDelta: func(id string, text string) error {
			bumpActivity()
			// Fork patch: batch 8 — start the checkpoint ticker on the
			// first text delta of this step (lazily, once only).
			startCheckpoint()
			sessionLock.Lock()
			// Fork patch: batch 8 — emit final-composition log at most
			// once per step, on the first text delta after a tool boundary.
			if sawToolBoundary && currentAssistant != nil {
				sawToolBoundary = false
				slog.Info(
					"agent: final composition started",
					"session_id", call.SessionID,
					"message_id", currentAssistant.ID,
					"chars_in_message_so_far", len(currentAssistant.FullText()),
				)
			}
			// Strip leading newline from initial text content. This is is
			// particularly important in non-interactive mode where leading
			// newlines are very visible.
			if len(currentAssistant.Parts) == 0 {
				text = strings.TrimPrefix(text, "\n")
			}

			currentAssistant.AppendContent(text)
			sessionLock.Unlock()
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
			sessionLock.Lock()
			currentAssistant.AddToolCall(toolCall)
			snap := currentAssistant.Clone()
			sessionLock.Unlock()
			// Use parent ctx instead of genCtx to ensure the update succeeds
			// even if the request is canceled mid-stream
			return a.messages.Update(ctx, snap)
		},
		OnToolInputDelta: func(id string, delta string) error {
			bumpActivity()
			sessionLock.Lock()
			currentAssistant.AppendToolCallInput(id, delta)
			sessionLock.Unlock()
			return nil // don't spam DB on every delta; ToolInputEnd will persist
		},
		OnToolInputEnd: func(id string) error {
			bumpActivity()
			sessionLock.Lock()
			currentAssistant.FinishToolCall(id)
			snap := currentAssistant.Clone()
			sessionLock.Unlock()
			return a.messages.Update(genCtx, snap)
		},
		OnRetry: func(err *fantasy.ProviderError, delay time.Duration) {
			bumpActivity()
			slog.Warn("Provider request failed, retrying", providerRetryLogFields(err, delay)...)
		},
		OnWarnings: func(warnings []fantasy.CallWarning) error {
			for _, w := range warnings {
				slog.Warn("Provider warning", "type", w.Type, "message", w.Message)
			}
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			bumpActivity()
			// A tool is about to execute — pause the stall watchdog until its
			// result arrives (OnToolResult). fantasy fires every OnToolCall
			// for a step before executing any tool, so the counter brackets
			// the whole executeTools window. The same toolMaxDuration cap
			// bounds every tool, including a sub-agent delegation (the
			// `agent` tool) — see toolExecutionMaxDefault's doc in agent.go.
			toolStarted()
			sawToolBoundary = true // Fork patch: batch 8
			input, wasSanitized := sanitizeToolInput(tc.ToolName, tc.ToolCallID, tc.Input)
			if wasSanitized {
				sanitizedToolCalls[tc.ToolCallID] = true
			}
			toolCall := message.ToolCall{
				ID:               tc.ToolCallID,
				Name:             tc.ToolName,
				Input:            input,
				ProviderExecuted: false,
				Finished:         true,
			}
			sessionLock.Lock()
			currentAssistant.AddToolCall(toolCall)
			snap := currentAssistant.Clone()
			sessionLock.Unlock()
			// Use parent ctx instead of genCtx to ensure the update succeeds
			// even if the request is canceled mid-stream
			return a.messages.Update(ctx, snap)
		},
		OnToolResult: func(result fantasy.ToolResultContent) error {
			bumpActivity()
			// Tool finished — resume the stall watchdog (and restart its idle
			// window so the tool's runtime isn't counted against the provider).
			toolFinished()
			sawToolBoundary = true // Fork patch: batch 8
			toolResult := a.convertToToolResult(result)
			if sanitizedToolCalls[result.ToolCallID] {
				toolResult.Content = "Tool call failed: arguments were not valid JSON. Please check your tool call format and try again."
				toolResult.IsError = true
			}
			sessionLock.Lock()
			sessionID := currentAssistant.SessionID
			sessionLock.Unlock()
			// Use parent ctx instead of genCtx to ensure the message is created
			// even if the request is canceled mid-stream
			_, createMsgErr := a.messages.Create(ctx, sessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					toolResult,
				},
			})
			return createMsgErr
		},
		OnStepFinish: func(stepResult fantasy.StepResult) error {
			bumpActivity()
			// Accumulate this step and recompute loop detection NOW, in this
			// callback invocation, so the AddFinish chain below can use the
			// result for THIS step. Fantasy calls OnStepFinish BEFORE
			// StopWhen for the same step, so relying on the StopWhen closure
			// to set loopDetected would read a stale (still-false) flag for
			// the very step that trips the detector — the loop would break
			// with empty finish text and no later OnStepFinish to fix it.
			// See the comment on stepHistory above for the ordering rationale.
			stepHistory = append(stepHistory, stepResult)
			loopDetected, loopDetail = hasRepeatedToolCalls(stepHistory, loopDetectionWindowSize, loopDetectionMaxRepeats)
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
			//
			// currentAssistant reads/mutations below are under sessionLock:
			// OnStepFinish never runs concurrently with the other streaming
			// callbacks (fantasy invokes them sequentially from one loop),
			// but it DOES run concurrently with the checkpoint ticker and
			// the peak-hours watcher goroutines, which also touch
			// currentAssistant.
			sessionLock.Lock()
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
			} else if loopDetected {
				// Loop detection force-stopped the turn. The reason stays
				// FinishReasonEndTurn (NOT a new distinct enum value) so
				// reclassifyCrashedAsDone / sessions-why keep treating this as
				// "done" — but the message/details are non-empty so an operator
				// or orchestrator can distinguish "model finished voluntarily"
				// from "we truncated a likely loop (possibly a legitimate poll)".
				loopMsg, loopDetails := loopDetectedFinishText(loopDetail)
				currentAssistant.AddFinish(finishReason, loopMsg, loopDetails)
			} else {
				currentAssistant.AddFinish(finishReason, "", "")
			}
			sessionLock.Unlock()
			// Drain any pending UI snapshot so the ticker goroutine does not
			// publish a stale state after messages.Update writes the final one.
			select {
			case <-latestMsgCh:
			default:
			}

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
			if sessionErr := a.sessions.SetUsage(ctx, updatedSession.ID, updatedSession.PromptTokens, updatedSession.CompletionTokens); sessionErr != nil {
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

			// Fork patch: peak-hours is normally only checked once, at the
			// START of a turn (coordinator.buildCall/runInternal) — an
			// already-in-flight turn was never re-checked, so a long turn
			// that started before the window opened ran straight through
			// it. Re-check here, once per step, so a turn stops as soon as
			// the provider enters its peak-hours window, not just on the
			// next NEW invocation.
			if a.peakHoursCheck != nil {
				if pErr := a.peakHoursCheck(); pErr != nil {
					if setPeakHoursAbortErr(pErr) {
						slog.Warn("agent: aborting — provider entered peak-hours mid-turn",
							"session_id", call.SessionID, "error", pErr)
						peakMsg, peakDetails := peakHoursStoppedFinishText(pErr)
						sessionLock.Lock()
						currentAssistant.AddFinish(message.FinishReasonError, peakMsg, peakDetails)
						snap := currentAssistant.Clone()
						sessionLock.Unlock()
						// Use the parent ctx (not genCtx) for the DB write —
						// genCtx dies as soon as we cancel below.
						if uErr := a.messages.Update(ctx, snap); uErr != nil {
							slog.Warn("agent: failed to persist peak-hours finish message", "error", uErr)
						}
						if cancelFn, ok := a.activeRequests.Get(call.SessionID); ok {
							cancelFn()
						}
					}
					// Stash the specific error so Run() can return it
					// AFTER fantasy's agent.Stream exits. We must call
					// cancelFn() to break fantasy's loop (returning an
					// error from OnStepFinish alone doesn't stop it), but
					// cancel() makes fantasy return context.Canceled —
					// swallowing our pErr. The stash lets Run() replace
					// that generic error with the real one.
					return pErr
				}
			}

			sessionLock.Lock()
			snap := currentAssistant.Clone()
			sessionLock.Unlock()
			return a.messages.Update(genCtx, snap)
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
				// StopWhen runs AFTER OnStepFinish for the same step, so by the
				// time this executes, OnStepFinish has already appended to
				// stepHistory and recomputed loopDetected/loopDetail. We only
				// need to return the boolean here to tell fantasy to break the
				// loop — do NOT mutate loopDetected/loopDetail here, OnStepFinish
				// owns them (mutating here would race for the last step's
				// finish text and re-introduce the stale-flag bug).
				detected, _ := hasRepeatedToolCalls(steps, loopDetectionWindowSize, loopDetectionMaxRepeats)
				return detected
			},
		},
	})
	// Defensive: normally OnStepFinish stops the checkpoint ticker (via
	// stopCheckpoint()) before its own final write. But if agent.Stream
	// returned an error before any step completed (e.g. the very first
	// provider call failed), OnStepFinish never ran and the ticker
	// goroutine may still be alive — it would otherwise race with the
	// unlocked currentAssistant touches below. stopCheckpoint() is safe to
	// call more than once: after the first call the stop channel is nil'd,
	// so subsequent calls hit the nil guard and return immediately (no
	// second wait, no double-close).
	stopCheckpoint()
	// If the peak-hours mid-turn check fired, it had to call cancelFn()
	// to break fantasy's loop (OnStepFinish errors alone don't stop it).
	// Depending on exactly when fantasy notices the cancellation relative
	// to finishing the in-flight step, agent.Stream can come back with
	// context.Canceled OR — if the step's own work had already fully
	// completed by the time cancelFn() fired — a nil error, as if the
	// turn ended cleanly. Either way, once peakHoursAbortErr is set it is
	// authoritative for this Run() call: force it in unconditionally so
	// the coordinator and RunNonInteractive never mistake this abort for
	// a successful completion or a bare, unexplained cancellation.
	if peakErr := getPeakHoursAbortErr(); peakErr != nil {
		err = peakErr
	}
	// The ask_question tool reports "agent asked a question" as the Go
	// error its Run() returns; fantasy's executeSingleTool treats a
	// non-nil tool error as critical and propagates it as the whole
	// Stream() call's error, so it surfaces here exactly like the
	// peak-hours abort err normalized just above. tools.AskQuestionError
	// (package tools) exists only because package tools cannot import
	// this package back (this package already imports tools — see the
	// comment on AskQuestionError in ask_question.go for the full import
	// cycle rationale); normalize it into AwaitingAnswerError here so
	// every downstream consumer — the errors.As(err, &awaitingErr) branch
	// immediately below, RunNonInteractive's exit_reason mapping, sessions
	// why/diff, … — only ever has to know about the one agent-level type.
	var askErr *tools.AskQuestionError
	if errors.As(err, &askErr) {
		err = &AwaitingAnswerError{
			Question:  askErr.Question,
			Options:   askErr.Options,
			SessionID: askErr.SessionID,
		}
	}
	if err != nil {
		isHyper := largeModel.ModelCfg.Provider == hyper.Name
		isCancelErr := errors.Is(err, context.Canceled)
		isWatchdogStall := isCancelErr && wd.stalled.Load()
		// `crush run --timeout` bounds the whole invocation via
		// context.WithTimeout on the root ctx (run.go); when it fires
		// mid-turn, ctx.Err() is context.DeadlineExceeded, NOT
		// context.Canceled, so isCancelErr above never catches it. Without
		// this branch it fell into the generic `else` below as "Provider
		// Error" with a bare "context deadline exceeded" — indistinguishable
		// from a real provider failure and useless to `sessions why`.
		isRunTimeout := errors.Is(err, context.DeadlineExceeded)
		// currentAssistant is only ever reassigned (never set back to nil)
		// by PrepareStep, under sessionLock. agent.Stream has already
		// returned by this point so no streaming callback can race this
		// read, but the peak-hours watcher goroutine may still be alive
		// (it only stops when genCtx is cancelled by the deferred cancel()
		// at the end of Run) and touches currentAssistant under the same
		// lock, so guard the read too.
		sessionLock.Lock()
		nilAssistant := currentAssistant == nil
		sessionLock.Unlock()
		if nilAssistant {
			return result, err, SessionAgentCall{}, false
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
		// From here to the final flush below, currentAssistant's Parts are
		// mutated in place; every touch (including the plain reads used to
		// build msgs/toolCalls) takes sessionLock to stay consistent with
		// the peak-hours watcher goroutine that may still be running.
		sessionLock.Lock()
		currentAssistant.FinishThinking()
		toolCalls := currentAssistant.ToolCalls()
		sessionID := currentAssistant.SessionID
		sessionLock.Unlock()
		msgs, createErr := a.messages.List(flushCtx, sessionID)
		if createErr != nil {
			return nil, createErr, SessionAgentCall{}, false
		}
		for _, tc := range toolCalls {
			if !tc.Finished {
				tc.Finished = true
				tc.Input = "{}"
				sessionLock.Lock()
				currentAssistant.AddToolCall(tc)
				snap := currentAssistant.Clone()
				sessionLock.Unlock()
				updateErr := a.messages.Update(flushCtx, snap)
				if updateErr != nil {
					return nil, updateErr, SessionAgentCall{}, false
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
				content = watchdogToolResultMessage(
					watchdogCause(watchdogCauseVal.Load()),
					toolMaxDuration,
					a.timeoutHardCap,
					idleTimeout,
					largeModel.ModelCfg.Provider,
				)
			} else if isCancelErr {
				content = "Error: user cancelled assistant tool calling"
			}
			toolResult := message.ToolResult{
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    content,
				IsError:    true,
			}
			_, createErr = a.messages.Create(flushCtx, sessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					toolResult,
				},
			})
			if createErr != nil {
				return nil, createErr, SessionAgentCall{}, false
			}
		}
		var fantasyErr *fantasy.Error
		var providerErr *fantasy.ProviderError
		var peakErr *PeakHoursError
		var awaitingErr *AwaitingAnswerError
		const defaultTitle = "Provider Error"
		// None of the branches below perform I/O — they only decide which
		// AddFinish to record based on err/isWatchdogStall/etc. — so the
		// whole chain can run under a single lock/unlock pair guarding the
		// currentAssistant mutation, matching the pattern used everywhere
		// else in Run().
		sessionLock.Lock()
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
			title, body := watchdogFinishMessage(
				watchdogCause(watchdogCauseVal.Load()),
				toolMaxDuration,
				a.timeoutHardCap,
				idleTimeout,
				largeModel.ModelCfg.Provider,
			)
			currentAssistant.AddFinish(message.FinishReasonError, title, body)
		} else if isCancelErr {
			currentAssistant.AddFinish(message.FinishReasonCanceled, "User canceled request", "")
		} else if isRunTimeout {
			currentAssistant.AddFinish(
				message.FinishReasonError,
				"Run timeout exceeded",
				"The run's --timeout deadline expired while this turn was still in flight (e.g. a long tool call or sub-agent delegation). Re-run into the same --session id with a larger --timeout to continue from here.",
			)
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
		} else if errors.As(err, &peakErr) {
			// Re-derive the same (msg, details) OnStepFinish's peak-hours
			// check already wrote (with the RESUME AT guidance) — this
			// path (err forced to peakHoursAbortErr) ALSO reaches here,
			// and AddFinish always replaces the prior finish part, so
			// without this branch the generic `else` below would
			// overwrite the useful message with a bare "Provider Error:
			// <terse text>" that drops the resume-time guidance entirely.
			peakMsg, peakDetails := peakHoursStoppedFinishText(err)
			currentAssistant.AddFinish(message.FinishReasonError, peakMsg, peakDetails)
		} else if errors.As(err, &awaitingErr) {
			// Same rationale as the peakErr branch above: without this,
			// the generic `else` below would overwrite the question/options/
			// resume-command guidance with a bare "Provider Error: <text>".
			awaitingMsg, awaitingDetails := awaitingAnswerStoppedFinishText(err)
			currentAssistant.AddFinish(message.FinishReasonError, awaitingMsg, awaitingDetails)
		} else {
			currentAssistant.AddFinish(message.FinishReasonError, defaultTitle, err.Error())
		}
		snap := currentAssistant.Clone()
		sessionLock.Unlock()
		// Detached flush (flushCtx is context.WithoutCancel + 15s timeout,
		// created at the top of this error block). This is the call that
		// MUST land on disk — without it the assistant message has tool
		// calls but no finish part, and the WUI/recovery sees it as still
		// in-flight forever.
		updateErr := a.messages.Update(flushCtx, snap)
		if updateErr != nil {
			slog.Error(
				"agent: failed to persist final finish part",
				"session_id", call.SessionID,
				"err", updateErr,
			)
			return nil, updateErr, SessionAgentCall{}, false
		}

		// Drain on cancel via the mailbox's generation-aware drain (design
		// §4): an interrupt-and-replace payload (mb.replacement) takes
		// precedence over a plain queued follow-up (mb.submitted). This
		// replaces the old messageQueue.PopFront: the interrupt path now
		// records its replacement in the mailbox, so a legacy-queue-only
		// drain would miss it entirely and silently drop the user's new
		// message — the P0-2 defect. The busy reservation itself stays
		// claimed (Run's loop is about to run another turn for the same
		// sessionID) and is only released by Run() once the loop has no
		// more queued work.
		if isCancelErr {
			if next, ok := a.getMailbox(call.SessionID).drainAfterCancel(); ok {
				cancel()
				return nil, nil, next, true
			}
		}
		return nil, err, SessionAgentCall{}, false
	}

	if shouldSummarize {
		// Run the compaction inline (runSummarizeBody, not the public
		// Summarize/runSummarize path) so it never calls back into Run():
		// Run() is still on the stack here, holding the OS lock and the
		// busy reservation for call.SessionID for the whole turn loop. This
		// call already holds the mailbox via the turn loop's submit, so
		// runSummarizeBody does not touch ownership at all — it only performs
		// the summarisation body itself. Anything queued during the summarize
		// stream simply stays parked in mailbox.submitted for THIS function's
		// own end-of-turn drainOrRelease call, a few lines below, to pick up
		// — the single true final drain point for the whole turn, summarize
		// included.
		summarizeErr := a.runSummarizeBody(genCtx, call.SessionID, call.ProviderOptions)
		if summarizeErr != nil {
			return nil, summarizeErr, SessionAgentCall{}, false
		}
		mb := a.getMailbox(call.SessionID)
		// If the agent wasn't done...
		sessionLock.Lock()
		hasPendingToolCalls := len(currentAssistant.ToolCalls()) > 0
		sessionLock.Unlock()
		if hasPendingToolCalls {
			call.Prompt = fmt.Sprintf("The previous session was interrupted because it got too long, the initial user request was: `%s`", call.Prompt)
			mb.submit(call, nil)
		}
	}

	// Silent compact of the oldest half (P0-4/#268): runs synchronously under
	// the turn's mailbox ownership — not as a background goroutine — so no
	// concurrent turn or compaction can delete/rewrite history while it is in
	// flight. Skipped when shouldSummarize already ran a full compaction
	// above (the two would be redundant, and running both would race for
	// SummaryMessageID). genCtx is still alive here (cancel() hasn't fired
	// yet), so Cancel(sessionID) can interrupt this if needed.
	if !shouldSummarize && silentCompactNeeded {
		if silentErr := a.runSummarizeSilent(genCtx, call.SessionID, call.ProviderOptions); silentErr != nil {
			slog.Warn("silent summarise failed", "session_id", call.SessionID, "err", silentErr)
		}
	}

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

	// Atomic final drain-or-release (design §3, closes P0-3). Before this,
	// the equivalent check was a.messageQueue.PopFront(call.SessionID) here,
	// with the actual reservation release happening SEPARATELY and LATER —
	// only once Run's own deferred releaseSessionReservation ran, after this
	// whole call had already returned hasNext=false. That gap was the lost-
	// wakeup window: a concurrent submit landing in it would see the session
	// still "busy" (activeRequests still held the entry), queue itself, and
	// never be drained by anyone, since this function had already decided
	// "nothing queued" and moved on. mailbox.drainOrRelease makes the
	// emptiness check and the ownership release one atomic operation under
	// the mailbox's own lock — no concurrent submit can land in a gap that no
	// longer exists. drainOrReleaseMerged additionally still checks the
	// legacy messageQueue (QueueMessage's callers are not migrated to the
	// mailbox until a later stage — see its doc) so a message queued via the
	// still-live QueueMessage path is not silently stranded.
	firstQueuedMessage, ok := a.drainOrReleaseMerged(call.SessionID, epoch, lk, runCancel)
	if !ok {
		return result, err, SessionAgentCall{}, false
	}
	// There are queued messages — the caller's loop runs another turn.
	return nil, nil, firstQueuedMessage, true
}

// ErrSummarizeQueued is returned by Summarize when the session is busy and
// the request has been queued for execution after the current task finishes.
var ErrSummarizeQueued = errors.New("summarize queued")

func (a *sessionAgent) Summarize(ctx context.Context, sessionID string, opts fantasy.ProviderOptions) error {
	// Atomic check-and-reserve (#268/P0-4, design §6): beginCompact makes
	// us the sole owner of sessionID's mailbox or returns false if a turn
	// or another compaction already owns it. This replaces the old
	// non-atomic IsSessionBusy + runSummarize pair that had a TOCTOU window
	// between the busy check and the compaction's DB writes — a concurrent
	// Run could start between the check and the first delete, corrupting
	// history.
	genCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	mb := a.getMailbox(sessionID)
	epoch, ok := mb.beginCompact(cancel)
	if !ok {
		cancel()
		a.summarizeQueue.Set(sessionID, opts)
		return ErrSummarizeQueued
	}
	return a.runSummarize(ctx, genCtx, sessionID, opts, mb, epoch, cancel)
}

// runSummarize runs a manual /compact under mailbox ownership acquired by
// Summarize's beginCompact call. After the compaction body completes, it
// releases ownership via abandonOwnership (handing back any Run that queued
// behind it via mailbox.submit during the stream) and starts a fresh Run for
// the first queued call — the same drain shape the old runSummarizeCore tail
// had, now operating on mailbox ownership instead of a synthetic
// activeRequests key.
func (a *sessionAgent) runSummarize(ctx context.Context, genCtx context.Context, sessionID string, opts fantasy.ProviderOptions, mb *mailbox, epoch uint64, cancel context.CancelFunc) error {
	defer cancel()
	err := a.runSummarizeBody(genCtx, sessionID, opts)
	// Release mailbox ownership. Any work queued via mb.submit during the
	// compaction is collected here; re-queue it to the legacy messageQueue so
	// the Run started below (or a future Run) drains it via checkLegacy.
	dropped, _ := mb.abandonOwnership(epoch)
	for _, d := range dropped {
		a.messageQueue.Append(d.SessionID, d)
	}
	if err != nil {
		return err
	}
	// Drain legacy queue (covers both mailbox-drained work above and direct
	// QueueMessage callers not yet migrated — #308) and start a Run for the
	// first queued call.
	firstQueued, hasNext := a.messageQueue.PopFront(sessionID)
	if !hasNext {
		return nil
	}
	_, runErr := a.Run(ctx, firstQueued)
	return runErr
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

// runSummarizeBody performs the actual summarisation: load messages, snapshot
// non-pinned IDs for deletion, create the summary message, stream the
// summary, persist it, wire up SummaryMessageID, and delete the old messages.
// It performs NO busy-check and NO ownership management — the caller must
// already hold sessionID's mailbox ownership (via beginCompact for
// standalone /compact, or via the turn loop's submit for inline
// auto-summarize). The caller's cancel is already registered in the mailbox
// (current.cancel), so Cancel(sessionID) naturally targets this compaction
// — no synthetic "sessionID-summarize" key.
//
// genCtx is the caller's already-cancel-scoped context (beginCompact's
// cancel for manual /compact, or the turn's genCtx for inline
// auto-summarize).
func (a *sessionAgent) runSummarizeBody(ctx context.Context, sessionID string, opts fantasy.ProviderOptions) error {
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

	// Snapshot non-pinned message IDs for deletion AFTER the summary stream
	// completes. The summary message itself is created BELOW, after this
	// snapshot, so it is never in toDelete. Pinned messages stay.
	var toDelete []message.Message
	for _, m := range msgs {
		if !m.Pinned {
			toDelete = append(toDelete, m)
		}
	}

	aiMsgs, _ := a.preparePrompt(msgs, nil)

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

	resp, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Prompt:          summaryPromptText,
		Messages:        aiMsgs,
		Headers:         sessionHeaders(sessionID),
		ProviderOptions: opts,
		PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = options.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(systemPromptPrefix)}, prepared.Messages...)
			}
			return callContext, prepared, nil
		},
		OnReasoningDelta: func(id string, text string) error {
			notifyActivity(ctx)
			summaryMessage.AppendReasoningContent(text)
			return a.messages.Update(ctx, summaryMessage)
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			notifyActivity(ctx)
			// Handle anthropic signature.
			if anthropicData, ok := reasoning.ProviderMetadata["anthropic"]; ok {
				if signature, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok && signature.Signature != "" {
					summaryMessage.AppendReasoningSignature(signature.Signature)
				}
			}
			summaryMessage.FinishThinking()
			return a.messages.Update(ctx, summaryMessage)
		},
		OnTextDelta: func(id, text string) error {
			notifyActivity(ctx)
			summaryMessage.AppendContent(text)
			return a.messages.Update(ctx, summaryMessage)
		},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// User cancelled summarize; remove the summary message.
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
	if err = a.messages.Update(ctx, summaryMessage); err != nil {
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

	// Re-fetch the session to pick up any user edits that happened while the
	// summary was streaming, then overlay our own fields.
	freshSession, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to re-fetch session before save: %w", err)
	}
	costDelta := a.updateSessionUsage(largeModel, &freshSession, resp.TotalUsage, openrouterCost)
	if costDelta != 0 {
		if _, costErr := a.sessions.IncrementCost(ctx, freshSession.ID, costDelta); costErr != nil {
			return costErr
		}
	}

	usage := resp.Response.Usage
	if err := a.sessions.SetSummaryAndUsage(ctx, freshSession.ID, summaryMessage.ID, 0, summaryCompletionTokens(usage, summaryMessage)); err != nil {
		return err
	}

	// Now that the summary is persisted and SummaryMessageID is wired up,
	// drop the historical non-pinned messages. The summary message itself was
	// created AFTER the snapshot above so it is not in toDelete.
	for _, m := range toDelete {
		if delErr := a.messages.Delete(ctx, m.ID); delErr != nil {
			slog.Warn("summarise: failed to delete old message", "id", m.ID, "err", delErr)
		}
	}

	return nil
}

// runSummarizeSilent compacts the oldest half of the session's messages
// synchronously under the caller's mailbox ownership without any visible
// change in the UI. It:
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
	resp, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Prompt:          summaryPromptText,
		Messages:        aiMsgs,
		Headers:         sessionHeaders(sessionID),
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
			return a.messages.Update(ctx, summaryMessage)
		},
		OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
			if anthropicData, ok := reasoning.ProviderMetadata["anthropic"]; ok {
				if signature, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok && signature.Signature != "" {
					summaryMessage.AppendReasoningSignature(signature.Signature)
				}
			}
			summaryMessage.FinishThinking()
			return a.messages.Update(ctx, summaryMessage)
		},
		OnTextDelta: func(id, text string) error {
			summaryMessage.AppendContent(text)
			return a.messages.Update(ctx, summaryMessage)
		},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			_ = a.messages.Delete(ctx, summaryMessage.ID)
		}
		return err
	}

	summaryMessage.AddFinish(message.FinishReasonEndTurn, "", "")
	if err = a.messages.Update(ctx, summaryMessage); err != nil {
		return err
	}

	// COMMIT PHASE — the order and the context were both wrong here, and
	// P0-4's own change is what made that reachable.
	//
	// Order: wire up SummaryMessageID BEFORE deleting the messages it
	// replaces, exactly as runSummarizeBody does (see its comment at the
	// equivalent point). This function did the opposite. Interrupted between
	// the two, that leaves the old half deleted with no summary pointer
	// standing in for it — getSessionMessages then finds neither, and the
	// history is unrecoverably holed.
	//
	// Context: the commit must not be tearable by cancellation. While the
	// silent compaction was a detached goroutine on context.WithoutCancel,
	// this window was unreachable. Making it synchronous (P0-4) put it on the
	// turn's cancellable genCtx and turned a long-standing latent ordering
	// bug into a live one, reachable via Stop in the web UI, CancelAll on
	// shutdown, --timeout, or the turn's own stream watchdog. Bounded rather
	// than unbounded: the provider stream is already finished, so what
	// remains is local DB work.
	commitCtx, commitCancel := context.WithTimeout(context.WithoutCancel(ctx), summaryCommitMaxDuration)
	defer commitCancel()

	// Point SummaryMessageID at the new hidden summary and reset token
	// counters so the next call gets an accurate remaining-context estimate.
	freshSession, err := a.sessions.Get(commitCtx, sessionID)
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
		if _, costErr := a.sessions.IncrementCost(commitCtx, freshSession.ID, costDelta); costErr != nil {
			return costErr
		}
	}
	if err := a.sessions.SetSummaryAndUsage(commitCtx, freshSession.ID, summaryMessage.ID, 0, resp.Response.Usage.OutputTokens); err != nil {
		// Nothing has been deleted yet, so the session is still whole: the
		// compaction simply did not happen, and the next turn retries it.
		return err
	}

	// Only now, with the summary persisted AND pointed at, drop what it
	// replaces. A failure here is survivable — the summary already stands in
	// for these rows, so the worst case is redundant history, not a hole.
	for _, m := range toSummarise {
		if delErr := a.messages.Delete(commitCtx, m.ID); delErr != nil {
			slog.Warn("silent summarise: failed to delete old message", "id", m.ID, "err", delErr)
		}
	}
	_ = pinnedOld // pinned messages stay in the DB untouched
	return nil
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

// sessionHeaders returns the HTTP headers we use for cache affinity on
// every LLM request for a given session.
//
// We use the session hash instead of the raw UUID so the header value
// is deterministic and opaque.
func sessionHeaders(sessionID string) map[string]string {
	hash := session.HashID(sessionID)
	return map[string]string{
		"x-session-id":       hash,
		"x-session-affinity": hash,
	}
}

// autoResumedCtxKey tags a context so that createUserMessage marks the
// resulting user message as AutoResumed. Set only on the Phase 4 idle-resume
// path in coordinator.notifyBackgroundJobDone; human and InjectMessage paths
// leave it unset (false).
type autoResumedCtxKey struct{}

// backgroundJobNoticeCtxKey tags a context so that createUserMessage marks
// the resulting user message as a BackgroundJobNotice. Set on both delivery
// paths in coordinator.notifyBackgroundJobDone so the web can render the
// injected completion summary as a notice rather than a human message.
type backgroundJobNoticeCtxKey struct{}

func (a *sessionAgent) createUserMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	parts := []message.ContentPart{message.TextContent{Text: call.Prompt}}
	var attachmentParts []message.ContentPart
	for _, attachment := range call.Attachments {
		attachmentParts = append(attachmentParts, message.BinaryContent{Path: attachment.FilePath, MIMEType: attachment.MimeType, Data: attachment.Content})
	}
	parts = append(parts, attachmentParts...)
	autoResumed, _ := ctx.Value(autoResumedCtxKey{}).(bool)
	backgroundJobNotice, _ := ctx.Value(backgroundJobNoticeCtxKey{}).(bool)
	msg, err := a.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
		Role:                message.User,
		Parts:               parts,
		AutoResumed:         autoResumed,
		BackgroundJobNotice: backgroundJobNotice,
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

// cleanTitle normalises a raw model title response: collapse newlines, strip
// any (orphan) think tags, and trim. Returns "" when nothing usable remains
// (e.g. a pure-reasoning response with no actual title text).
func cleanTitle(raw string) string {
	t := strings.ReplaceAll(raw, "\n", " ")
	t = thinkTagRegex.ReplaceAllString(t, "")
	t = orphanThinkTagRegex.ReplaceAllString(t, "")
	return strings.TrimSpace(t)
}

// generateTitle generates a session titled based on the initial prompt.
func (a *sessionAgent) generateTitle(ctx context.Context, sessionID string, userPrompt string, cfg turnConfig) {
	if userPrompt == "" {
		return
	}

	// Ensure the session always gets a title even if every path below
	// fails or the context is cancelled before we finish. WithoutCancel so
	// the fallback still lands when the caller's ctx is already done.
	var titleSaved bool
	defer func() {
		if titleSaved {
			return
		}
		fallbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		// Only stamp the default when the session STILL has no real title.
		//
		// This used to be unconditional, which was safe only because runTurn
		// waited unconditionally for this goroutine: the write always landed
		// before the turn returned, so no later turn could have set a title
		// yet. Bounding that wait (P1-B) removed the ordering — a goroutine
		// whose provider ignores cancellation can now outlive its turn by
		// minutes. Unconditional, it would then overwrite a perfectly good
		// title a LATER turn had since generated; and because runTurn's
		// needsTitle test counts DefaultSessionName as "still needs one",
		// that becomes a loop, with every following turn spawning another
		// attempt for the abandoned one to clobber again.
		//
		// The re-read is what makes abandonment safe: a late finisher either
		// finds the slot still empty and fills it (the original intent) or
		// finds it taken and leaves it alone.
		// Fail CLOSED: if the slot cannot be VERIFIED empty, do not stamp it.
		// An earlier draft fell through to the rename whenever this read
		// errored, which reinstates the very clobber-and-loop being fixed —
		// and the read is most likely to fail exactly when it matters (the
		// 5s budget expiring, or the DB closing during shutdown, both of
		// which mean this goroutine has been abandoned and has no business
		// writing).
		sess, err := a.sessions.Get(fallbackCtx, sessionID)
		if err != nil {
			slog.Debug("agent: skipping fallback session title — could not confirm the title is still unset",
				"session_id", sessionID, "err", err)
			return
		}
		if sess.Title != "" && sess.Title != DefaultSessionName {
			return
		}
		if err := a.sessions.Rename(fallbackCtx, sessionID, DefaultSessionName); err != nil {
			slog.Error("Failed to save fallback session title", "error", err)
		}
	}()

	// From the turn's snapshot, not a fresh read of the shared fields — a
	// title generated for session A must not pick up session B's model just
	// because B applied an override while this goroutine was starting
	// (task #265).
	smallModel := cfg.smallModel
	largeModel := cfg.largeModel
	systemPromptPrefix := cfg.promptPrefix

	newAgent := func(m fantasy.LanguageModel, p []byte, tok int64) fantasy.Agent {
		return fantasy.NewAgent(
			m,
			fantasy.WithSystemPrompt(string(p)+"\n /no_think"),
			fantasy.WithMaxOutputTokens(tok),
			fantasy.WithUserAgent(userAgent),
		)
	}

	streamCall := fantasy.AgentStreamCall{
		Prompt:  fmt.Sprintf("Generate a concise title for the following content:\n\n%s\n <think>\n\n</think>", userPrompt),
		Headers: sessionHeaders(sessionID),
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
	var title string
	var success bool
	for _, attempt := range attempts {
		// Non-reasoning models: a title is a handful of tokens, but GLM-style
		// models don't always honour /no_think and leak a short preamble — 40
		// tokens then truncates the title itself. 96 gives headroom while
		// staying cheap. Reasoning models get their full budget (the think
		// block is suppressed but still counts against the cap).
		tok := int64(96)
		if attempt.model.CatwalkCfg.CanReason {
			tok = attempt.model.CatwalkCfg.DefaultMaxTokens
		}
		agent := newAgent(attempt.model.Model, titlePrompt, tok)
		resp, err = agent.Stream(ctx, streamCall)
		if err != nil {
			slog.Error("Error generating title with "+attempt.name+" model; trying next", "err", err)
			continue
		}
		if resp == nil {
			slog.Error("Title generation returned nil response with " + attempt.name + " model; trying next")
			continue
		}
		// A length-truncated response usually still carries a usable title —
		// only a genuinely empty one (pure reasoning, no text) is a real miss.
		// Discarding a truncated-but-good title just retries the same tiny
		// budget on the next model, typically fails the same way, and leaves
		// the session "Untitled" for a transient reason.
		candidate := cleanTitle(resp.Response.Content.Text())
		if candidate == "" {
			slog.Error("Title generation produced no usable text with " + attempt.name + " model; trying next")
			continue
		}
		if resp.Response.FinishReason == fantasy.FinishReasonLength {
			slog.Debug("Title truncated (FinishReasonLength) but usable with " + attempt.name + " model")
		} else {
			slog.Debug("Generated title with " + attempt.name + " model")
		}
		title = candidate
		model = attempt.model
		success = true
		break
	}
	if !success {
		// The deferred fallback will save the default session name.
		return
	}

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

	// Rename and cost-accrual are intentionally two separate atomic calls,
	// not a single combined update. Title generation is a small side LLM
	// call that runs concurrently with the main turn (see the wg.Go call
	// site in Run): it must NOT add to prompt_tokens/completion_tokens,
	// since those columns are a snapshot of the main conversation's current
	// context-window size (see Service.SetUsage's doc comment) that the
	// main turn overwrites, not a cumulative counter. If title generation
	// added to them here, whichever of the two goroutines finishes last
	// would nondeterministically win: SetUsage's overwrite would erase this
	// addition, or this addition would land on top of a stale snapshot.
	// Cost, on the other hand, IS real money spent on a real API call, and
	// already has a dedicated atomic-additive path (IncrementCost) built
	// exactly for this class of "charge the session from a concurrent
	// goroutine" problem — so only cost is added here.
	//
	// Rename runs first: a title is the primary purpose of this function,
	// and the deferred fallback above only fires when titleSaved is still
	// false, so a Rename failure must leave titleSaved false to trigger the
	// "Untitled Session" fallback. IncrementCost runs regardless of the
	// Rename outcome so a title-generation API call is never left unbilled
	// just because the rename itself failed for an unrelated reason (e.g. a
	// transient DB error on that specific statement).
	renameErr := a.sessions.Rename(ctx, sessionID, title)
	if renameErr != nil {
		slog.Error("Failed to save session title", "error", renameErr)
	} else {
		titleSaved = true
	}

	if _, err := a.sessions.IncrementCost(ctx, sessionID, cost); err != nil {
		slog.Error("Failed to accrue title generation cost", "error", err)
	}
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
	// Cancel only the in-flight generation (design §4): a bare interrupt
	// (Ctrl-C, sessions kill, cost/token cap) must NOT discard durable
	// queued user intent. Previously Cancel unconditionally cleared
	// messageQueue/injectQueue — a latent second bug riding along with
	// P0-2 that silently dropped anything a caller had queued moments
	// earlier via QueueMessage for an unrelated reason. ClearQueue remains
	// the one intentional "drop everything queued" operation. The mailbox
	// (whose current.cancel is populated by beginGeneration in Run's loop
	// and runTurn, and by beginCompact for synchronous compactions) is now
	// the cancel target instead of activeRequests.
	//
	// Falls back to dispatcherCancel when no generation is live yet: Run
	// claims the mailbox (submit stores runCancel as dispatcherCancel) and
	// only calls beginGeneration once it reaches its turn loop, so a Cancel
	// landing in between — while the inter-process OS lock is still being
	// acquired — would otherwise find current.cancel nil and silently
	// no-op. Before the mailbox migration tryReserveSession wrote runCancel
	// straight into activeRequests, so that window WAS covered; keeping the
	// fallback preserves it (the task #206 "Cancel must never find a dead
	// placeholder" invariant).
	mb := a.getMailbox(sessionID)
	mb.mu.Lock()
	genCancel := mb.current.cancel
	if genCancel == nil {
		genCancel = mb.dispatcherCancel
	}
	mb.mu.Unlock()
	if genCancel != nil {
		slog.Debug("Request cancellation initiated", "session_id", sessionID)
		genCancel()
	}
}

func (a *sessionAgent) ClearQueue(sessionID string) {
	// The single intentional "drop everything queued" operation (design
	// §4): clears the mailbox's submitted/replacement/injects atomically
	// under its lock, plus the still-live legacy messageQueue (QueueMessage
	// is not yet migrated to the mailbox — see drainOrReleaseMerged's doc).
	// Cancel no longer touches any of these; only this method does.
	slog.Debug("Clearing queued prompts", "session_id", sessionID)
	a.getMailbox(sessionID).clearAll()
	a.messageQueue.Clear(sessionID)
}

func (a *sessionAgent) QueueMessage(call SessionAgentCall) {
	a.messageQueue.Append(call.SessionID, call)
}

// InterruptAndReplace is the coordinator's single entry point for "interrupt
// and replace" (design §4), replacing the QueueMessage+Cancel two-step that
// P0-2 made self-defeating: Cancel deterministically wiped the very message
// QueueMessage had just queued the line before. It atomically records call
// as the replacement the current owner must run next, and cancels ONLY the
// in-flight generation — leaving the dispatcher (Run's turn loop) alive to
// drain the replacement via drainAfterCancel. Returns true when a turn was
// actually interrupted; false when the session was idle (nothing to cancel —
// the caller should queue call for the next Run itself).
func (a *sessionAgent) InterruptAndReplace(sessionID string, call SessionAgentCall) bool {
	cancelFn, hadOwner := a.getMailbox(sessionID).interruptAndReplace(call)
	if !hadOwner {
		return false
	}
	if cancelFn != nil {
		cancelFn()
	}
	return true
}

// InjectMessage — see SessionAgent interface comment. Persists immediately
// (UI updates via the same pubsub path that handleSendMessage uses) and, if
// the session is currently running, atomically queues the persisted row into
// the mailbox's injects list (stamped with the current generation id) so the
// next PrepareStep splices it into prepared.Messages without duplicating the
// DB write. The atomic busy-check + inject (mailbox.injectIfBusy, design §5
// stage 2.4) replaces the old non-atomic IsSessionBusy + injectQueue.Append
// pair.
func (a *sessionAgent) InjectMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	msg, err := a.createUserMessage(ctx, call)
	if err != nil {
		return message.Message{}, err
	}
	// Atomic busy-check + inject under one mb.mu hold (design §5, stage 2.4):
	// replaces the non-atomic IsSessionBusy + injectQueue.Append pair that
	// had a window between check and append where the owner could finish and
	// release to mbIdle. When the session is idle, the message lives only in
	// the DB and is picked up by the next Run's preamble naturally.
	a.getMailbox(call.SessionID).injectIfBusy(msg)
	return msg, nil
}

func (a *sessionAgent) CancelAll() {
	// Refuse all FUTURE Run() calls before touching anything else (closing
	// review, blocker 1). This must come first and must live on the agent
	// rather than the mailboxes: the sweep below can only latch mailboxes
	// that already exist, and mailboxes are created lazily, so a Run for a
	// session id the sweep never saw would otherwise get a fresh mailbox
	// with stopped == false and run a full turn nothing will cancel.
	a.shuttingDown.Store(true)

	// Latch EVERY mailbox closed FIRST, before cancelling anything (round
	// 14 review, P0-C). Ordering is what makes the shutdown terminal: a
	// turn cancelled below immediately runs its cancel-handling branch,
	// and that branch drains replacement/submitted and starts the NEXT
	// turn. With `stopped` already latched, every drain refuses instead —
	// so no new provider request can begin while we are trying to exit.
	//
	// This is also why it hard-stops rather than reusing Cancel(): Cancel
	// deliberately targets only the current generation, leaving the
	// dispatcher (runCancel, the whole-Run() context) alive precisely so a
	// turn loop survives to run a replacement. Shutdown needs the opposite,
	// so hardStop hands back the DISPATCHER cancel too and we fire it.
	//
	// Latching is unconditional rather than gated on the mailbox currently
	// being owned: a Run() sitting between turns, or one that reaches its
	// drain in the window between this sweep and process exit, must be
	// refused as well.
	for _, mb := range a.mailboxes.Seq2() {
		dispatcherCancel, genCancel := mb.hardStop()
		// Generation first, then dispatcher: the generation cancel is what
		// promptly unblocks an in-flight provider stream, and cancelling
		// the dispatcher afterwards tears down the Run() that owns it.
		// Both are invoked outside mb.mu — hardStop has already returned.
		if genCancel != nil {
			genCancel()
		}
		if dispatcherCancel != nil {
			dispatcherCancel()
		}
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

// IsBusy reports whether ANY session this agent knows about currently has a
// live owner. Task #206-followup (round 9 review, BLOCKER-1): this used to
// read activeRequests directly, but releaseSessionReservation (mailbox.
// drainOrRelease) stopped clearing the plain-sessionID activeRequests entry
// once the mailbox migration (P0-3, task #282) landed — tryReserveSession/
// Run's loop still WRITE it every turn (agent.go:949, :1194) via
// activeRequests.Set, so after the FIRST turn any session ever ran,
// activeRequests permanently holds a non-nil (already-fired, inert)
// cancelFunc for it. The old activeRequests-based IsBusy therefore returned
// true forever after any session's first turn completed, which meant
// CancelAll's 5-second drain loop (App.Shutdown, reached by every `crush
// run` via `defer a.Shutdown()`) always ran to its full timeout instead of
// returning immediately once genuinely idle. mailboxes.state is the
// post-migration source of truth for "does this session have a live
// owner" (see IsSessionBusy's doc) and is correctly reset to mbIdle on
// release, so it does not have this staleness problem.
//
// `mb.state != mbIdle` (NOT `mb.state == mbOwned`) is deliberate as of
// #296/P1-C: mbReleasing means drainOrReleaseFinal's release() — the OS
// session-lock teardown — is running with mb.mu NOT held, on some OTHER
// goroutine than this one. If IsBusy() treated mbReleasing as "not busy",
// CancelAll's 5-second drain loop could see every mailbox as idle and
// return WHILE that release() disk I/O (and the whole-process DB teardown
// App.Shutdown runs right after CancelAll returns) is still in flight —
// reopening the exact class of race HIGH-1 closed, just observed through
// the shutdown path instead of a same-process "become the new owner" path.
func (a *sessionAgent) IsBusy() bool {
	for _, mb := range a.mailboxes.Seq2() {
		mb.mu.Lock()
		busy := mb.state != mbIdle
		mb.mu.Unlock()
		if busy {
			return true
		}
	}
	return false
}

// IsSessionBusy reports whether sessionID currently has an owner (design §2's
// mapping table: mailbox.state != mbIdle replaces the old
// activeRequests.Get(sessionID) busy check). This is now the ONLY source of
// truth for the main-session busy state: releaseSessionReservation (via
// mailbox.drainOrRelease) no longer touches activeRequests at all, so
// activeRequests entries for a plain sessionID key would otherwise never be
// cleared. activeRequests itself is untouched by this migration and remains
// the cancel-target lookup Cancel/CancelAll/the peak-hours abort path use —
// call-site migration happens incrementally, one piece at a time (see the
// design doc's §7 migration plan).
func (a *sessionAgent) IsSessionBusy(sessionID string) bool {
	mb := a.getMailbox(sessionID)
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return mb.state != mbIdle
}

// QueuedPrompts reports how many calls are waiting for sessionID's current
// owner to finish, across BOTH the legacy messageQueue (still populated
// directly by QueueMessage, which is not yet migrated to the mailbox — see
// drainOrReleaseMerged's doc) and the mailbox's own submitted queue (now
// populated by tryReserveSession/mailbox.submit for a concurrent Run() call
// — design §3). Reporting only one of the two would undercount: a message
// queued via the now-mailbox-backed path would silently show as "0 queued"
// even though a turn IS waiting to run it.
func (a *sessionAgent) QueuedPrompts(sessionID string) int {
	mb := a.getMailbox(sessionID)
	mb.mu.Lock()
	mailboxQueued := len(mb.submitted)
	mb.mu.Unlock()
	return a.messageQueue.Len(sessionID) + mailboxQueued
}

// QueuedPromptsList is QueuedPrompts' list counterpart — see its doc for why
// both sources are merged. Legacy-queue items are listed first, in their
// existing order, followed by mailbox-queued items in their FIFO order;
// relative ordering BETWEEN the two sources is not meaningful (they are
// drained from different structures by different code paths today — see
// drainOrReleaseMerged), only each source's own internal order is preserved.
func (a *sessionAgent) QueuedPromptsList(sessionID string) []string {
	l := a.messageQueue.Snapshot(sessionID)
	mb := a.getMailbox(sessionID)
	mb.mu.Lock()
	mailboxCalls := append([]SessionAgentCall(nil), mb.submitted...)
	mb.mu.Unlock()

	if len(l) == 0 && len(mailboxCalls) == 0 {
		return nil
	}
	prompts := make([]string, 0, len(l)+len(mailboxCalls))
	for _, call := range l {
		prompts = append(prompts, call.Prompt)
	}
	for _, call := range mailboxCalls {
		prompts = append(prompts, call.Prompt)
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

	supportsImages := largeModel.CatwalkCfg.SupportsImages

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
				if !supportsImages {
					// Model cannot process images. Replace with a text
					// placeholder and skip creating a synthetic user
					// message with FilePart, which would brick the
					// session on text-only models.
					textParts = append(textParts, fantasy.ToolResultPart{
						ToolCallID: toolResult.ToolCallID,
						Output: fantasy.ToolResultOutputContentText{
							Text: "[Image/media content not supported by this model]",
						},
						ProviderOptions: toolResult.ProviderOptions,
					})
					continue
				}

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

// sanitizeToolInput validates tool call JSON from the provider.
// Malformed input is replaced with an empty object to prevent
// stuck conversations from truncated or malformed model output.
// The second return value indicates whether sanitization occurred.
func sanitizeToolInput(toolName, toolCallID, input string) (string, bool) {
	if !json.Valid([]byte(input)) {
		slog.Warn("Malformed tool call JSON from provider, replacing with empty object",
			"tool", toolName,
			"id", toolCallID,
			"input_len", len(input),
		)
		return "{}", true
	}
	return input, false
}
