package agent

// Fork patch: this Coordinator drives N concurrent web sessions, not a single
// TUI run. The fork-specific additions visible in the diff against upstream
// include:
//
//   - ModelOverride struct + RunWithOverrides path used by `handleSendMessage`
//     in `internal/server/handlers.go` so the WUI can pick a model per turn.
//   - TakeSummarizeQueue + queued background summarisation that does not
//     block the user's next message (paired with agent.go's sliding window).
//   - Wiring to `internal/agent/cliprovider` for npx-claude-code / Gemini /
//     Codex CLI providers, including MCP bridge initialisation.
//
// Upstream's `copilotResponsesModels` table and per-model Responses-API
// special-casing were removed when the dispatch was refactored. Keep an eye
// on that during merges: if upstream adds a new Responses-only model, the
// adapter selection in this file is where it needs to land.
//
// See CHANGELOG.fork.md sections 4.D (agent extensions) and 4.E (CLI
// providers) before resolving a merge conflict.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	mcp "github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/discover"
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/skills"
	"golang.org/x/sync/errgroup"

	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/charmbracelet/crush/internal/agent/cliprovider"
	openaisdk "github.com/charmbracelet/openai-go/option"
	"github.com/qjebbs/go-jsons"
)

// ModelOverride allows callers to specify per-run model overrides (provider + model ID).
type ModelOverride struct {
	Provider        string
	Model           string
	ReasoningEffort string
}

// Coordinator errors.
var (
	errCoderAgentNotConfigured         = errors.New("coder agent not configured")
	errModelProviderNotConfigured      = errors.New("model provider not configured")
	errLargeModelNotSelected           = errors.New("large model not selected")
	errSmallModelNotSelected           = errors.New("small model not selected")
	errLargeModelProviderNotConfigured = errors.New("large model provider not configured")
	errSmallModelProviderNotConfigured = errors.New("small model provider not configured")
	errLargeModelNotFound              = errors.New("large model not found in provider config")
	errSmallModelNotFound              = errors.New("small model not found in provider config")
	// errProviderPeakHours is returned when a provider's peak_hours
	// window refuses the request. It is operator-actionable (the user
	// configured the window on purpose) and MUST NOT be retried: the
	// condition only clears when the wall clock leaves the window,
	// which the backoff loop cannot accelerate. classifyProviderError
	// pins it to classTerminal as defense-in-depth.
	errProviderPeakHours = errors.New("provider is inside its configured peak-hours window")
)

// PeakHoursError is the concrete error checkPeakHours returns while a
// provider is inside its configured peak_hours window. It carries the exact
// reopen time as a time.Time (not just formatted into the error string) so
// callers that need to act on it precisely — e.g. an orchestrating agent
// scheduling a resume — don't have to parse Error()'s text.
type PeakHoursError struct {
	ProviderID string
	Start, End string // HH:MM, as configured
	ReopensAt  time.Time
}

func (e *PeakHoursError) Error() string {
	return fmt.Sprintf(
		"provider %s is in peak hours (%s–%s), refusing until %s",
		e.ProviderID, e.Start, e.End, e.ReopensAt.Format("15:04"),
	)
}

// Unwrap lets errors.Is(err, errProviderPeakHours) keep working for callers
// that only care about the error class, not the structured detail.
func (e *PeakHoursError) Unwrap() error { return errProviderPeakHours }

// errAwaitingAnswer classifies AwaitingAnswerError the same way
// errProviderPeakHours classifies PeakHoursError: it lets callers that only
// care about the error class (not the structured question/options/session
// detail) use errors.Is without knowing the concrete type.
var errAwaitingAnswer = errors.New("agent asked a question and is awaiting an operator/orchestrator answer")

// AwaitingAnswerError is the sentinel error the (forthcoming) question tool
// returns to force-stop the current turn instead of blocking on a synchronous
// answer — this fork's `crush run` has no code path that can wait mid-turn
// for operator input (both headless and web sessions auto-approve
// permissions unconditionally, see internal/server/handlers.go:163-171), so
// "ask a question" has to mean "stop the turn cleanly and hand the operator
// an unambiguous resume command" rather than "block until answered".
//
// Structurally this mirrors PeakHoursError: a concrete, typed error carrying
// exactly what the orchestrator-facing guidance needs (the question, any
// suggested answer options, and the session id to resume), so callers don't
// have to parse Error()'s prose to act on it.
type AwaitingAnswerError struct {
	Question  string
	Options   []string
	SessionID string
}

func (e *AwaitingAnswerError) Error() string {
	return fmt.Sprintf(
		"agent asked a question and is awaiting an answer (session %s): %s",
		e.SessionID, e.Question,
	)
}

// Unwrap lets errors.Is(err, errAwaitingAnswer) keep working for callers
// that only care about the error class, not the structured detail.
func (e *AwaitingAnswerError) Unwrap() error { return errAwaitingAnswer }

// Copilot models that use the Responses API instead of Chat Completions.
var copilotResponsesModels = map[string]bool{
	"gpt-5.2":       true,
	"gpt-5.2-codex": true,
	"gpt-5.3-codex": true,
	"gpt-5.4":       true,
	"gpt-5.4-mini":  true,
	"gpt-5.5":       true,
	"gpt-5-mini":    true,
}

// OpenCode models that use Anthropic Messages API instead of Chat Completions.
// Ported from upstream b7f4ad6c (#3040).
var opencodeMessagesModels = map[string]bool{
	"qwen3.7-max": true,
}

const (
	// streamStallRetriesDefault is the default Options.StreamStallRetries
	// when the config key is absent or zero. We default to 2 (3 total
	// attempts per turn) rather than 0 because the user-visible failure
	// mode of a single transient provider error is a hard turn-error that
	// the orchestrator then has to handle — silently absorbing 1-2
	// provider hiccups is almost always what an operator wants. This bounds
	// retries for ALL transient turn failures (stream stall, empty stream,
	// overload, 5xx, network), not just stalls.
	streamStallRetriesDefault = 2
	// streamStallRetryBaseBackoff and streamStallRetryBackoffMultiplier
	// shape exponential backoff: 10s → 30s → 90s. Long enough to let a
	// rate-limit window roll over, short enough to keep one turn under
	// ~5 min including the prior watchdog timeout.
	streamStallRetryBaseBackoff       = 10 * time.Second
	streamStallRetryBackoffMultiplier = 3.0
	// streamStalledFinishTitle is the canonical Message field that
	// agent.Run writes on a watchdog stall. Match against this exact
	// string when deciding whether to retry.
	//
	// CROSS-LANGUAGE SYNC: a fifth copy of this literal lives in the
	// web UI, since TypeScript can't import this Go constant. See the
	// `part.Message === "Stream stalled"` check in
	// web/src/components/Message.tsx — it renders a stalled
	// turn-after-partial-work as a soft amber StreamPausedBlock
	// instead of a red failure block. If this literal ever changes,
	// update that TS check too. The two are intentionally not wired
	// through the WS/JSON protocol (LOW severity; a comment
	// cross-reference is the agreed sync mechanism).
	streamStalledFinishTitle = "Stream stalled"
)

// interruptInjectTick is how often the interrupt-inject ticker polls
// pending_injects for interrupt=true rows during an active turn. 3s is a
// deliberate middle ground: fast enough that `crush sessions inject
// --interrupt` feels near-immediate to an operator (worst case one tick of
// latency), slow enough that the extra SELECT is negligible even across a
// long multi-step turn. The ticker only lives for the duration of a turn (see
// startInterruptTicker), so there is no idle-process polling.
const interruptInjectTick = 3 * time.Second

// interruptTickOperationTimeout is the per-tick deadline for the
// handleInterruptTick operation. If a tick's DB operations or downstream
// calls block longer than this, the tick is abandoned with a timeout error
// and the goroutine returns to the select loop to observe ctx.Done().
// P1-3 fix: prevents a single blocking tick from permanently hanging
// coordinator shutdown when the parent ctx is cancelled.
// 10s is chosen as ~3x the tick interval — long enough that normal operation
// never times out (a healthy tick completes in <<1s), but short enough that
// a genuinely stuck tick doesn't block shutdown for an unreasonable duration.
const interruptTickOperationTimeout = 10 * time.Second

// maxConsecutiveAutoResumes bounds Phase 4 autonomous idle-resumes per session
// without human involvement (reset by any human message). Anti-runaway: an
// agent that keeps backgrounding self-completing jobs cannot loop forever.
const maxConsecutiveAutoResumes = 5

type Coordinator interface {
	// INFO: (kujtim) this is not used yet we will use this when we have multiple agents
	// SetMainAgent(string)
	Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	// RunWithOverrides is like Run but allows overriding the large and/or small model for this call.
	RunWithOverrides(ctx context.Context, sessionID, prompt string, large, small *ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	Cancel(sessionID string)
	CancelAll() (stillBusy bool)
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	ClearQueue(sessionID string)
	// InterruptAndSend queues a new user message and cancels the running
	// turn so the queued message picks up immediately with everything
	// produced so far retained in history.
	InterruptAndSend(ctx context.Context, sessionID, prompt string, large, small *ModelOverride, attachments ...message.Attachment) error
	// InjectMessage persists a user message and, if the session is currently
	// running, schedules it to be merged into the next provider request
	// without cancelling the in-flight turn. See SessionAgent.InjectMessage.
	InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error)
	// Summarize compresses the session history. If the session is currently
	// busy the request is queued; call TakeSummarizeQueue after the task
	// finishes to pick it up. Returns ErrSummarizeQueued when queued.
	//
	// The snapshot contains the model, provider options, and prompt prefix
	// resolved from the target session (or shared state for sessions without
	// overrides), ensuring the entire summarize operation uses consistent
	// configuration regardless of concurrent SetModels calls (task #341).
	Summarize(context.Context, string, *SummarizeSnapshot) error
	SummarizeQueued(sessionID string) bool
	TakeSummarizeQueue(sessionID string) (*SummarizeSnapshot, bool)
	CancelQueuedSummarize(sessionID string)
	Model() Model
	UpdateModels(ctx context.Context) error
	GetSystemPrompt() string
	BuildSystemPrompt(ctx context.Context) (string, error)
	BuildSystemPromptForSession(ctx context.Context, sessionID string) (string, error)
	UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error
	// SetAgentTimeoutOptions configures the stream watchdog's deadline
	// extension on the current agent. Called from RunNonInteractive when
	// --timeout-extends-on-progress is set. Fork patch: batch 8.
	SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration)
	// SetRunLimits sets cost and token caps for the next Run call.
	// Fork patch: batch 30.
	SetRunLimits(maxCost float64, maxTokens int64)
	// SetActiveModelRole records which named model slot (large, small,
	// worker, reviewer) is driving the CURRENT top-level run, so sub-agent
	// spawns can decide whether to prefer the cheaper Worker slot instead of
	// blindly inheriting the parent's Large model. An empty/unset value
	// means "unknown — treat as smart": the interactive TUI/web path never
	// calls this, and for those the default behavior — use Large for
	// everything, i.e. exactly today's behavior — is correct, since
	// "Smart = large/default". Fork patch (reviewer/worker roles).
	SetActiveModelRole(role config.SelectedModelType)
	// SetAllowPeakHours arms a one-shot bypass of the peak-hours refusal
	// for the next Run call. It exists so `crush run --allow-peak-hours`
	// can override an operator-configured peak_hours window for a single
	// conscious invocation without introducing a persistent "always
	// allow" config setting. Fork patch (peak-hours bypass).
	SetAllowPeakHours(allow bool)
	// SetPersistentMode marks this coordinator as the long-lived web/interactive
	// server (enables Phase 4 autonomous idle-resume eligibility). crush run
	// leaves it false.
	SetPersistentMode(persistent bool)
	// ResetAutoResumeCounter clears the Phase 4 consecutive-auto-resume bound
	// for a session. Called from the human send path so a human re-entering the
	// loop re-arms autonomy.
	ResetAutoResumeCounter(sessionID string)
	// RebuildSessionAgentCall reconstructs a full SessionAgentCall from SessionAgentCallData
	// for run queue pump execution (task #340, ROUND 3 migration).
	RebuildSessionAgentCall(ctx context.Context, data session.SessionAgentCallData) (SessionAgentCall, error)
	// RunSessionAgentCall executes a SessionAgentCall directly for run queue pump execution
	// (task #340, ROUND 3 migration). This bypasses the normal buildCall path since the
	// call is already fully reconstructed with all necessary data.
	RunSessionAgentCall(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error)
}

type coordinator struct {
	cfg         *config.ConfigStore
	sessions    session.Service
	messages    message.Service
	permissions permission.Service
	history     history.Service
	filetracker filetracker.Service
	prompt      *prompt.Prompt
	notify      pubsub.Publisher[notify.Notification]

	currentAgent SessionAgent
	agents       map[string]SessionAgent

	// Skills discovery results (session-start snapshot).
	allSkills    []*skills.Skill // Pre-filter: all discovered after dedup.
	activeSkills []*skills.Skill // Post-filter: active skills only.
	skillTracker *skills.Tracker

	readyWg errgroup.Group

	// Per-run limits. Set via SetRunLimits before Run(). Reset after use.
	// Fork patch: batch 30. Mutex added in review-fix (data race: SetRunLimits
	// called from HTTP handler, read in runInternal on agent goroutine).
	runLimitsMu sync.Mutex
	maxCost     float64
	maxTokens   int64

	// allowPeakHours is a one-shot bypass for the peak-hours refusal,
	// armed by SetAllowPeakHours from `crush run --allow-peak-hours`.
	// Reset to false after the next Run. Fork patch (peak-hours bypass).
	allowPeakHours bool

	// activeModelRole records which named model slot is driving the current
	// top-level run, set via SetActiveModelRole. Static per-process (unlike
	// maxCost/maxTokens above, there is no reset-after-use — `crush run` is
	// single-shot). Mutex guards the same race shape as runLimitsMu:
	// SetActiveModelRole is called from RunNonInteractive before the agent
	// goroutine starts, and buildAgentModels reads it from the agent
	// goroutine. Fork patch (reviewer/worker roles).
	activeModelRoleMu sync.Mutex
	activeModelRole   config.SelectedModelType

	// Phase 4 autonomous idle-resume guardrails.
	// persistentMode: true only for the long-lived web server; false for
	// crush run. Currently written exactly once at process start (no real
	// race today), but every sibling field here (allowPeakHours,
	// activeModelRole, maxCost) is already lock/atomic-guarded, so a plain
	// bool would be a silent trap for the next caller who adds a second
	// SetPersistentMode call path — atomic.Bool costs nothing and keeps
	// this field consistent with its neighbors under `go test -race`.
	persistentMode         atomic.Bool
	autoResumeMu           sync.Mutex     // guards consecutiveAutoResumes.
	consecutiveAutoResumes map[string]int // sessionID -> consecutive auto-resumes since last human message.

	// modelCache caches resolved (large, small) Model pairs keyed by their
	// combined provider+model+reasoning_effort tuple. Used by
	// resolveSessionModels to avoid rebuilding the same pair repeatedly.
	// Cached as a pair (not two independent per-slot entries) so a single
	// buildModelsFromCfg call always fills both roles together — see
	// resolveSessionModels's own comment for why a per-slot cache
	// previously mismatched large/small roles.
	modelCache *csync.Map[string, cachedModelPair]
}

// cachedModelPair holds a resolved (large, small) Model pair as built
// together by a single buildModelsFromCfg call.
type cachedModelPair struct {
	large Model
	small Model
}

func NewCoordinator(
	ctx context.Context,
	cfg *config.ConfigStore,
	sessions session.Service,
	messages message.Service,
	permissions permission.Service,
	history history.Service,
	filetracker filetracker.Service,
	notify pubsub.Publisher[notify.Notification],
) (Coordinator, error) {
	p, err := coderPrompt(prompt.WithWorkingDir(cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	// Discover skills once at session start.
	allSkills, activeSkills := discoverSkills(cfg)
	skillTracker := skills.NewTracker(activeSkills)

	c := &coordinator{
		cfg:                    cfg,
		sessions:               sessions,
		messages:               messages,
		permissions:            permissions,
		history:                history,
		filetracker:            filetracker,
		prompt:                 p,
		notify:                 notify,
		agents:                 make(map[string]SessionAgent),
		allSkills:              allSkills,
		activeSkills:           activeSkills,
		skillTracker:           skillTracker,
		consecutiveAutoResumes: make(map[string]int),
		modelCache:             csync.NewMap[string, cachedModelPair](),
	}

	agentCfg, ok := cfg.Config().Agents[config.AgentCoder]
	if !ok || agentCfg.ID == "" {
		// Self-heal: config.Load/reload always call SetupAgents once
		// IsConfigured() becomes true, but a caller that mutates
		// Providers/SelectedModel directly on an already-published config
		// (bypassing Load/reload entirely — a test-only pattern; found via
		// a CI-only failure this exact class of gap caused,
		// errCoderAgentNotConfigured, that never reproduced on a dev
		// machine because cliprovider.Available() synthesizes a local-cli
		// provider whenever claude/gemini/codex/qwen is on PATH, making
		// IsConfigured() true at initial Init regardless of any
		// CRUSH_GLOBAL_*/XDG_* isolation — confirmed by the sixth @oh
		// review pass) never triggers that population. Also guards against
		// Agents[AgentCoder] being present but zero-value (ok==true,
		// ID=="") — the bare `!ok` check alone would skip self-heal for
		// that case and build a coder agent with an empty ID/AllowedTools.
		// SetupAgents is idempotent (derives Agents purely from Options/
		// DisabledTools, no I/O), so re-deriving it here on a genuine miss
		// is safe. See
		// p350_coder_agent_selfheal_test.go for a deterministic
		// regression test that does not depend on any environment leak.
		cfg.SetupAgents()
		agentCfg, ok = cfg.Config().Agents[config.AgentCoder]
	}
	if !ok {
		return nil, errCoderAgentNotConfigured
	}

	agent, err := c.buildAgent(ctx, p, agentCfg, false)
	if err != nil {
		return nil, err
	}
	c.currentAgent = agent
	c.agents[config.AgentCoder] = agent
	return c, nil
}

// Run implements Coordinator.
func (c *coordinator) Run(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}

	// Resolve the session's model configuration from the DB or config defaults.
	// This always returns a valid snapshot (never nil), ensuring that every turn
	// runs with a complete, self-contained model configuration.
	pinned, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve session models: %w", err)
	}

	return c.runInternal(ctx, sessionID, prompt, pinned, attachments...)
}

// SetRunLimits stores cost and token caps for the next Run call.
// Fork patch: batch 30.
func (c *coordinator) SetRunLimits(maxCost float64, maxTokens int64) {
	c.runLimitsMu.Lock()
	c.maxCost = maxCost
	c.maxTokens = maxTokens
	c.runLimitsMu.Unlock()
}

// SetActiveModelRole records which named model slot is driving the current
// top-level run. Fork patch (reviewer/worker roles).
func (c *coordinator) SetActiveModelRole(role config.SelectedModelType) {
	c.activeModelRoleMu.Lock()
	c.activeModelRole = role
	c.activeModelRoleMu.Unlock()
}

// SetAllowPeakHours arms a one-shot bypass of the peak-hours refusal
// for the next Run call. Fork patch (peak-hours bypass).
func (c *coordinator) SetAllowPeakHours(allow bool) {
	c.runLimitsMu.Lock()
	c.allowPeakHours = allow
	c.runLimitsMu.Unlock()
}

// SetPersistentMode marks this coordinator as the long-lived web/interactive
// server (Phase 4 autonomous idle-resume eligibility). crush run leaves it
// false.
func (c *coordinator) SetPersistentMode(persistent bool) {
	c.persistentMode.Store(persistent)
}

// autonomyEnabled reports whether Phase 4 auto-resume is opted in via config.
func (c *coordinator) autonomyEnabled() bool {
	opts := c.cfg.Config().Options
	return opts != nil && opts.AutoResumeOnJobDone != nil && *opts.AutoResumeOnJobDone
}

// consecutiveResume returns the number of auto-resumes for sessionID since the
// last human message.
func (c *coordinator) consecutiveResume(sessionID string) int {
	c.autoResumeMu.Lock()
	defer c.autoResumeMu.Unlock()
	return c.consecutiveAutoResumes[sessionID]
}

// bumpConsecutiveResume increments the auto-resume counter for sessionID.
func (c *coordinator) bumpConsecutiveResume(sessionID string) {
	c.autoResumeMu.Lock()
	defer c.autoResumeMu.Unlock()
	c.consecutiveAutoResumes[sessionID]++
}

// resetConsecutiveResume clears the auto-resume counter for sessionID. Called
// from the human send path so a human re-entering the loop re-arms autonomy.
func (c *coordinator) resetConsecutiveResume(sessionID string) {
	c.autoResumeMu.Lock()
	defer c.autoResumeMu.Unlock()
	delete(c.consecutiveAutoResumes, sessionID)
}

// ResetAutoResumeCounter is the exported wrapper around resetConsecutiveResume
// for the server package's human send path.
func (c *coordinator) ResetAutoResumeCounter(sessionID string) {
	c.resetConsecutiveResume(sessionID)
}

// autoResumeEligible reports whether a finished background job should
// autonomously resume the (idle-or-busy; Run handles that) owning session.
// Pure autonomy policy: opt-in config, persistent (web) coordinator only, and
// under the consecutive-resume runaway bound. Per-turn cost/token caps are
// still enforced by the normal Run path; a Cancel aborts the auto-turn like any
// other. NEVER eligible for crush run (persistentMode stays false there).
func (c *coordinator) autoResumeEligible(sessionID string) bool {
	return c.autonomyEnabled() &&
		c.persistentMode.Load() &&
		c.consecutiveResume(sessionID) < maxConsecutiveAutoResumes
}

// resolvedOverrides is what applyModelOverrides computed, captured so the
// caller can pin it onto the SessionAgentCall (task #265, P0-1).
//
// Without this, the sequence was: applyModelOverrides writes the resolved
// values into the SHARED agent, then runInternal reads them back out of that
// same shared agent a few dozen lines later, then the turn reads them AGAIN
// when it actually starts. Every one of those gaps is a window where another
// session's applyModelOverrides can land, and this fork's whole premise is N
// concurrent sessions. Returning the values means the caller never has to
// read them back.
type resolvedOverrides struct {
	large        Model
	small        Model
	promptPrefix string
	systemPrompt string
	// providerCfg is the large model's provider config, resolved from the
	// SAME Snapshot()/Config() call that built `large` above. Callers that
	// need provider options/credentials for this same model later in one
	// logical resolve/build operation (runInternal's 401 rebuildCall) MUST
	// read this field instead of taking their own, separately-timed
	// snapshot — otherwise a reload landing between "model resolved" and
	// "provider options computed" can mix a model from one config
	// generation with credentials/options from another (task #341, P1-1).
	// Zero-value (config.ProviderConfig{}) when the resolving path never
	// populated it (e.g. applyModelOverrides callers that don't need it);
	// always populated by resolveSessionModels.
	providerCfg config.ProviderConfig
}

// SummarizeSnapshot holds an immutable snapshot of all configuration needed
// for a single manual/queued summarize operation. It is computed ONCE from the
// target session's persisted models (or from shared state for sessions without
// overrides) and passed through the entire summarize path, ensuring the provider
// options, model, and prompt prefix never diverge due to concurrent SetModels
// calls (task #341, P1-1).
//
// This mirrors the resolvedOverrides pattern used for normal turns, but
// specialized for summarize which doesn't need smallModel or systemPrompt.
type SummarizeSnapshot struct {
	model           Model
	providerOptions fantasy.ProviderOptions
	promptPrefix    string
}

// resolveSessionModels builds an immutable snapshot of model configuration for a session.
// It reads from the session DB if overrides are present, otherwise falls back to
// the global config defaults. This method NEVER writes to shared state (c.currentAgent),
// ensuring that per-session model choices don't affect other concurrent sessions.
//
// The returned snapshot includes both large and small models, the provider's system
// prompt prefix, and the built system prompt (if a prompt template is available).
//
// Results are cached per unique (config generation, provider+model+reasoning_effort) pair.
// The config generation is included so that any config change (reload, credential update,
// etc.) invalidates the cache, preventing stale clients from being reused (task #341, P1-3).
//
// All config reads use a single atomic Snapshot() call to prevent reading config fields
// from different generations (task #341, P1-3).
func (c *coordinator) resolveSessionModels(ctx context.Context, sessionID string) (*resolvedOverrides, error) {
	// Load the session to check for model overrides.
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session %q: %w", sessionID, err)
	}

	// Atomically capture config and generation in a single snapshot to prevent
	// torn reads across reloads (task #341, P1-3).
	cfg, gen := c.cfg.Snapshot()

	// Start with the global config defaults.
	largeCfg := cfg.Models[config.SelectedModelTypeLarge]
	smallCfg := cfg.Models[config.SelectedModelTypeSmall]

	// Apply session-level overrides from the DB if present.
	var largeOverride, smallOverride *ModelOverride
	if sess.LargeModelID != "" {
		largeOverride = &ModelOverride{
			Provider:        sess.LargeModelProvider,
			Model:           sess.LargeModelID,
			ReasoningEffort: sess.LargeModelReasoningEffort,
		}
	}
	if sess.SmallModelID != "" {
		smallOverride = &ModelOverride{
			Provider:        sess.SmallModelProvider,
			Model:           sess.SmallModelID,
			ReasoningEffort: sess.SmallModelReasoningEffort,
		}
	}

	// Merge overrides into the config copies.
	if largeOverride != nil {
		if largeCfg.Provider != largeOverride.Provider || largeCfg.Model != largeOverride.Model {
			largeCfg.Think = false
			largeCfg.ReasoningEffort = ""
		}
		largeCfg.Provider = largeOverride.Provider
		largeCfg.Model = largeOverride.Model
		if largeOverride.ReasoningEffort != "" {
			largeCfg.ReasoningEffort = largeOverride.ReasoningEffort
		}
	}
	if smallOverride != nil {
		if smallCfg.Provider != smallOverride.Provider || smallCfg.Model != smallOverride.Model {
			smallCfg.Think = false
			smallCfg.ReasoningEffort = ""
		}
		smallCfg.Provider = smallOverride.Provider
		smallCfg.Model = smallOverride.Model
		if smallOverride.ReasoningEffort != "" {
			smallCfg.ReasoningEffort = smallOverride.ReasoningEffort
		}
	}

	// Build (or reuse from cache) both models TOGETHER in a single
	// buildModelsFromCfg call, keyed by the combined large+small
	// provider+model+reasoning_effort tuple PLUS the config generation.
	// The generation is included so that any config change (reload, credential
	// update, etc.) invalidates the cache, preventing stale clients from being
	// reused (task #341, P1-3).
	//
	// An earlier version of this cache called buildModelsFromCfg once per
	// slot, swapping (largeCfg, smallCfg) argument order to "select" which
	// half of the pair to keep for the small slot. That swap silently
	// mismatched roles: buildModelsFromCfg(ctx, smallCfg, largeCfg, false)
	// returns (ModelBuiltFromSmallCfg, ModelBuiltFromLargeCfg) — i.e. its
	// SECOND return value (labeled "small" only by the caller's own local
	// variable name) is actually built from largeCfg. The old code then
	// picked that second value as the small-model result, so
	// resolved.small ended up holding a Model built from the LARGE
	// config's provider/model whenever large and small differed — pinned
	// onto every call's SmallModel (title generation and any other
	// small-model-driven path) via resolvedOverrides.pin. Caching the pair
	// from one call, in the caller-supplied role order, removes the
	// swap entirely.
	//
	// Use the atomic generation from Snapshot(), not a separate Generation()
	// call, to ensure consistency (task #341, P1-3).
	pairCacheKey := fmt.Sprintf("gen:%d|%s:%s:%s|%s:%s:%s",
		gen,
		largeCfg.Provider, largeCfg.Model, largeCfg.ReasoningEffort,
		smallCfg.Provider, smallCfg.Model, smallCfg.ReasoningEffort)

	// c.modelCache is nil for any *coordinator built as a struct literal
	// instead of via NewCoordinator (several existing test fixtures in this
	// package do exactly that — see e.g. newWorkerToolTestCoordinator).
	// csync.Map's methods dereference the receiver's mutex, so calling
	// Get/Set on a nil *csync.Map panics; treat a nil cache as "caching
	// disabled" rather than requiring every coordinator constructor to
	// remember to initialize it.
	var largeModel, smallModel Model
	var cacheHit bool
	if c.modelCache != nil {
		if cached, ok := c.modelCache.Get(pairCacheKey); ok {
			largeModel, smallModel, cacheHit = cached.large, cached.small, true
		}
	}
	if !cacheHit {
		largeModel, smallModel, err = c.buildModelsFromCfg(ctx, cfg, largeCfg, smallCfg, false)
		if err != nil {
			return nil, fmt.Errorf("failed to build models: %w", err)
		}
		if c.modelCache != nil {
			c.modelCache.Set(pairCacheKey, cachedModelPair{large: largeModel, small: smallModel})
		}
	}

	resolved := &resolvedOverrides{
		large: largeModel,
		small: smallModel,
	}

	// Resolve prompt prefix from provider config using the same atomic snapshot.
	largeProviderCfg, ok := cfg.Providers.Get(largeModel.ModelCfg.Provider)
	if !ok {
		return nil, fmt.Errorf("large model provider %s not configured", largeModel.ModelCfg.Provider)
	}
	if largeProviderCfg.SystemPromptPrefix != "" {
		resolved.promptPrefix = largeProviderCfg.SystemPromptPrefix
	}
	// Carry the provider config resolved from THIS SAME snapshot (cfg/gen
	// above) so callers that need provider options/credentials later in the
	// same logical operation (runInternal's 401 rebuildCall, in particular)
	// don't have to take a second, independently-timed Snapshot() call that
	// could observe a different generation than the model was built from
	// (task #341, P1-1).
	resolved.providerCfg = largeProviderCfg

	// Build system prompt if a template is available. workerSubAgentActive
	// takes the SAME pinned cfg used for largeModel/largeProviderCfg above
	// (task #341, P1-1) — it used to read c.cfg.Config() live here, which
	// meant a reload landing between the Snapshot() at the top of this
	// function and this Build call could make the system prompt's
	// WorkerAvailable flag disagree with the model/prefix this call already
	// resolved from an earlier generation.
	if c.prompt != nil {
		newSystemPrompt, err := c.prompt.Build(ctx, largeModel.ModelCfg.Provider, largeModel.ModelCfg.Model, c.cfg, c.workerSubAgentActive(cfg))
		if err != nil {
			// Leave resolved.systemPrompt empty rather than guessing: the
			// caller treats "" as "nothing to pin", so the turn falls back to
			// the agent's shared prompt exactly as it did before this returned
			// anything at all.
			slog.Error("resolveSessionModels: failed to rebuild system prompt", "err", err)
		} else {
			resolved.systemPrompt = newSystemPrompt
		}
	}

	return resolved, nil
}

// applyModelOverrides builds a resolvedOverrides snapshot from explicit override parameters.
// This is used by RunWithOverrides which receives overrides directly from the caller
// (rather than from the session DB).
//
// IMPORTANT: This method does NOT write to shared state. The old behavior of writing
// to c.currentAgent.SetModels/SetSystemPromptPrefix/SetSystemPrompt has been removed
// to prevent per-session model changes from affecting other concurrent sessions.
//
// The returned snapshot is what makes a TURN immune to the shared state moving
// underneath it — no turn reads shared state after this point.
func (c *coordinator) applyModelOverrides(ctx context.Context, large, small *ModelOverride) (*resolvedOverrides, error) {
	// Atomically capture config and generation up front (task #341, P1-1) so
	// largeCfg/smallCfg below, the buildModelsFromCfg call further down, and
	// the provider/prompt reads at the end of this function all agree on one
	// generation. This function used to read Models[Large]/Models[Small] via
	// a live c.cfg.Config() call here and take a SEPARATE Snapshot() call
	// later just for buildModelsFromCfg's provider lookups — a reload
	// landing between the two could hand back a large/small model selection
	// from one generation built against provider config from another.
	cfg, _ := c.cfg.Snapshot()
	largeCfg := cfg.Models[config.SelectedModelTypeLarge]
	smallCfg := cfg.Models[config.SelectedModelTypeSmall]

	if large != nil {
		if largeCfg.Provider != large.Provider || largeCfg.Model != large.Model {
			largeCfg.Think = false
			largeCfg.ReasoningEffort = ""
		}
		largeCfg.Provider = large.Provider
		largeCfg.Model = large.Model
		if large.ReasoningEffort != "" {
			largeCfg.ReasoningEffort = large.ReasoningEffort
		}
	}
	if small != nil {
		if smallCfg.Provider != small.Provider || smallCfg.Model != small.Model {
			smallCfg.Think = false
			smallCfg.ReasoningEffort = ""
		}
		smallCfg.Provider = small.Provider
		smallCfg.Model = small.Model
		if small.ReasoningEffort != "" {
			smallCfg.ReasoningEffort = small.ReasoningEffort
		}
	}

	// Build models directly without using the cache — model overrides are
	// transient, per-run values that don't benefit from caching (each override
	// is unique to the caller's request), and the cache key pattern requires
	// the config generation which we'd need to recompute here. The cost of
	// building the fantasy.LanguageModel client is paid once per override use,
	// which is acceptable since overrides are explicitly opt-in per-call.
	// Reuse the cfg captured at the top of this function (task #341, P1-1)
	// instead of taking a second, separately-timed Snapshot() here.
	largeModel, smallModel, err := c.buildModelsFromCfg(ctx, cfg, largeCfg, smallCfg, false)
	if err != nil {
		return nil, fmt.Errorf("failed to build override models: %w", err)
	}

	resolved := &resolvedOverrides{large: largeModel, small: smallModel}

	if largeProviderCfg, ok := cfg.Providers.Get(largeModel.ModelCfg.Provider); ok {
		resolved.promptPrefix = largeProviderCfg.SystemPromptPrefix
		resolved.providerCfg = largeProviderCfg
	}
	// workerSubAgentActive takes the SAME pinned cfg used for largeModel
	// above (task #341, P1-1) rather than re-reading c.cfg.Config() live.
	if c.prompt != nil {
		newSystemPrompt, err := c.prompt.Build(ctx, largeModel.ModelCfg.Provider, largeModel.ModelCfg.Model, c.cfg, c.workerSubAgentActive(cfg))
		if err != nil {
			slog.Error("applyModelOverrides: failed to rebuild system prompt", "err", err)
		} else {
			resolved.systemPrompt = newSystemPrompt
		}
	}
	return resolved, nil
}

// pin copies the resolved values onto a call so the turn runs on them
// regardless of what another session does to the shared agent meanwhile.
// nil receiver = no overrides were applied, so nothing to pin — the call
// keeps whatever runInternal/buildCall already put there.
func (r *resolvedOverrides) pin(call *SessionAgentCall) {
	if r == nil {
		return
	}
	large, small := r.large, r.small
	call.LargeModel = &large
	call.SmallModel = &small
	if r.promptPrefix != "" {
		prefix := r.promptPrefix
		call.SystemPromptPrefix = &prefix
	}
	if r.systemPrompt != "" {
		prompt := r.systemPrompt
		call.SystemPrompt = &prompt
	}
}

// resolveSessionSystemPrompt loads the per-session system prompt from the DB,
// building and persisting one on the fly when missing. Shared by runInternal
// and buildCall.
func (c *coordinator) resolveSessionSystemPrompt(ctx context.Context, sessionID string) string {
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return ""
	}
	if sess.SystemPrompt != "" {
		return sess.SystemPrompt
	}
	if c.prompt == nil {
		return ""
	}

	// Resolve the session's model to use for prompt building.
	// Use the large model since that's what the turn runs on.
	resolved, resolveErr := c.resolveSessionModels(ctx, sessionID)
	if resolveErr != nil {
		slog.Warn("coordinator: failed to resolve models for system prompt", "sessionID", sessionID, "err", resolveErr)
		return ""
	}

	// Reuse the system prompt resolveSessionModels already built from its
	// OWN single pinned config snapshot (task #341, P1-1), instead of
	// rebuilding it here from a second, separately-timed live cfg read
	// (c.workerSubAgentActive() with no argument, and c.cfg.Config()
	// implicitly via prompt.Build's store). A reload landing between the
	// two builds could otherwise make this second build's WorkerAvailable
	// flag disagree with resolved.large/resolved.promptPrefix, which were
	// pinned from an earlier generation.
	built := resolved.systemPrompt
	if built == "" {
		return ""
	}
	if saveErr := c.sessions.UpdateSystemPrompt(ctx, sessionID, built); saveErr != nil {
		slog.Warn("coordinator: failed to save system prompt to session", "sessionID", sessionID, "err", saveErr)
	}
	return built
}

// buildCall assembles the SessionAgentCall for the current agent + model
// state. Extracted so InterruptAndSend can queue a call shaped exactly like
// runInternal would.
//
// pinned is ALWAYS non-nil: every caller resolves from the session DB or
// config defaults before calling buildCall. This ensures session-scoped model
// overrides are respected even on cross-process paths (e.g., interrupt requeue).
func (c *coordinator) buildCall(ctx context.Context, sessionID, prompt string, pinned *resolvedOverrides, attachments []message.Attachment) (SessionAgentCall, error) {
	if pinned == nil {
		return SessionAgentCall{}, errors.New("buildCall: pinned is required; caller must resolve session models first")
	}

	model := pinned.large

	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	if !model.CatwalkCfg.SupportsImages && attachments != nil {
		filteredAttachments := make([]message.Attachment, 0, len(attachments))
		for _, att := range attachments {
			if att.IsText() {
				filteredAttachments = append(filteredAttachments, att)
			}
		}
		attachments = filteredAttachments
	}

	// pinned.providerCfg was resolved from the SAME snapshot pinned.large
	// came from (task #341/P1-1) -- a live c.cfg.Config() read here would
	// reintroduce the torn-read this whole `pinned` threading exists to
	// close, since buildCall may run well after resolveSessionModels did
	// (e.g. for a queued replacement call, per the comment below).
	providerCfg := pinned.providerCfg
	if providerCfg.ID == "" {
		return SessionAgentCall{}, errModelProviderNotConfigured
	}
	if err := checkPeakHours(providerCfg); err != nil {
		return SessionAgentCall{}, err
	}

	mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(model, providerCfg)
	sessionSystemPrompt := c.resolveSessionSystemPrompt(ctx, sessionID)

	pinnedLarge := model
	call := SessionAgentCall{
		SessionID:            sessionID,
		Prompt:               prompt,
		Attachments:          attachments,
		MaxOutputTokens:      maxTokens,
		ProviderOptions:      mergedOptions,
		Temperature:          temp,
		TopP:                 topP,
		TopK:                 topK,
		FrequencyPenalty:     freqPenalty,
		PresencePenalty:      presPenalty,
		SystemPromptOverride: sessionSystemPrompt,
		LargeModel:           &pinnedLarge,
		LogicalCallID:        uuid.New().String(), // P2-1: generate stable ID once
	}
	// Pinning matters more here than in runInternal: this call is QUEUED as a
	// replacement and may not start for a while, so the gap between "options
	// computed" and "turn starts" is unbounded rather than a few lines.
	pinned.pin(&call)
	return call, nil
}

// runInternal executes the agent, handling 401 retries.
//
// pinned carries what the caller's resolveSessionModels just resolved.
// pinned is ALWAYS non-nil: every Run path resolves from the session DB or
// config defaults before calling runInternal. Everything below —
// maxOutputTokens, providerCfg, the peak-hours check, mergeCallOptions —
// is derived from ONE model value that also travels with the call, so the
// turn cannot end up running a different model than the options were
// computed for (task #265).
func (c *coordinator) runInternal(ctx context.Context, sessionID string, prompt string, pinned *resolvedOverrides, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if pinned == nil {
		return nil, errors.New("runInternal: pinned is required; caller must resolve session models first")
	}

	model := pinned.large
	slog.Debug("Coordinator: running with model", "sessionID", sessionID, "model", model.ModelCfg.Model)

	maxOutputTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxOutputTokens = model.ModelCfg.MaxTokens
	}

	if !model.CatwalkCfg.SupportsImages && attachments != nil {
		filteredAttachments := make([]message.Attachment, 0, len(attachments))
		for _, att := range attachments {
			if att.IsText() {
				filteredAttachments = append(filteredAttachments, att)
			}
		}
		attachments = filteredAttachments
	}

	// pinned.providerCfg was resolved from the SAME snapshot pinned.large
	// came from (task #341/P1-1) -- reuse it rather than a second, live
	// c.cfg.Config() read, which would reopen the exact torn-read gap the
	// 401-rebuild path below already fixed, on the far more common
	// (every-run, not just every-401) path.
	providerCfg := pinned.providerCfg
	if providerCfg.ID == "" {
		return nil, errModelProviderNotConfigured
	}
	// Fork patch (peak-hours bypass): consume the one-shot allow flag
	// armed by SetAllowPeakHours (`crush run --allow-peak-hours`). Reset
	// immediately so a subsequent Run on the same coordinator does not
	// inherit the bypass.
	c.runLimitsMu.Lock()
	allowPeak := c.allowPeakHours
	c.allowPeakHours = false
	c.runLimitsMu.Unlock()
	if !allowPeak {
		if err := checkPeakHours(providerCfg); err != nil {
			return nil, err
		}
	}

	mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(model, providerCfg)

	if err := c.refreshTokenIfExpired(ctx, providerCfg); err != nil {
		// NOTE(@andreynering): We don't return here because the event handling to ask the user to reauthenticate
		// depends on the flow below. If refresh fails, proceed with the token we have.
		slog.Error("Failed to refresh OAuth2 token. Proceeding with existing token.", "error", err)
	}

	sessionSystemPrompt := c.resolveSessionSystemPrompt(ctx, sessionID)

	// Fork patch: batch 30 — per-run limits, pass through to the agent.
	c.runLimitsMu.Lock()
	maxCost := c.maxCost
	c.maxCost = 0
	maxTokensRunLimit := c.maxTokens
	c.maxTokens = 0
	c.runLimitsMu.Unlock()

	// Pin the model this call already resolved (task #265). Everything above
	// — maxOutputTokens, mergedOptions, temp/topP/topK/penalties — was
	// derived from THIS `model` value. Without pinning it, the agent
	// re-reads its shared largeModel when the turn actually starts, so a
	// concurrent session's applyModelOverrides landing in between makes the
	// turn run one model with another model's options. Passing it down keeps
	// the call internally consistent.
	pinnedLarge := model
	agentCall := SessionAgentCall{
		SessionID:            sessionID,
		Prompt:               prompt,
		Attachments:          attachments,
		MaxOutputTokens:      maxOutputTokens,
		ProviderOptions:      mergedOptions,
		Temperature:          temp,
		TopP:                 topP,
		TopK:                 topK,
		FrequencyPenalty:     freqPenalty,
		PresencePenalty:      presPenalty,
		SystemPromptOverride: sessionSystemPrompt,
		MaxCost:              maxCost,
		MaxTokens:            maxTokensRunLimit,
		LargeModel:           &pinnedLarge,
		LogicalCallID:        uuid.New().String(), // P2-1: generate stable ID once
	}
	// Overrides pin small model / prefix / base prompt too; pin() leaves
	// LargeModel as set above when pinned is nil, and rewrites it to the same
	// value when it isn't (model was taken FROM pinned.large).
	pinned.pin(&agentCall)

	// trackCall is a pointer to the current call so we can rebuild it after
	// a 401 credential refresh (task #341, P1-2).
	trackCall := &agentCall
	run := func() (*fantasy.AgentResult, error) {
		return c.currentAgent.Run(ctx, *trackCall)
	}

	// rebuildCall reconstructs the call after credential refresh, preserving
	// the logical request ID and other fields but using fresh models with new
	// credentials (task #341, P1-2).
	rebuildCall := func() error {
		// Resolve fresh models with updated credentials.
		pinned, err := c.resolveSessionModels(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("failed to resolve models after credential refresh: %w", err)
		}

		// Rebuild the call with the new models, preserving all logical fields.
		model := pinned.large
		maxOutputTokens := model.CatwalkCfg.DefaultMaxTokens
		if model.ModelCfg.MaxTokens != 0 {
			maxOutputTokens = model.ModelCfg.MaxTokens
		}

		// Re-filter attachments for the new model's image support.
		if !model.CatwalkCfg.SupportsImages && attachments != nil {
			filteredAttachments := make([]message.Attachment, 0, len(attachments))
			for _, att := range attachments {
				if att.IsText() {
					filteredAttachments = append(filteredAttachments, att)
				}
			}
			attachments = filteredAttachments
		}

		// Use the provider config resolveSessionModels already resolved from
		// the SAME snapshot it built `model` from above (task #341, P1-1).
		// This used to take a SEPARATE, freshly-timed c.cfg.Snapshot() here
		// — despite the old comment claiming that was "consistent with
		// resolveSessionModels above", a reload landing in the gap between
		// resolveSessionModels returning and this second Snapshot() call
		// could hand back provider options/credentials from a DIFFERENT
		// generation than the model pinned.large was built from, i.e.
		// exactly the torn-read this whole rebuildCall path exists to
		// avoid. pinned.providerCfg removes the second snapshot entirely.
		providerCfg := pinned.providerCfg
		if providerCfg.ID == "" {
			return errModelProviderNotConfigured
		}

		mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(model, providerCfg)

		pinnedLarge := model
		newCall := SessionAgentCall{
			SessionID:            trackCall.SessionID,
			Prompt:               trackCall.Prompt,
			Attachments:          attachments,
			MaxOutputTokens:      maxOutputTokens,
			ProviderOptions:      mergedOptions,
			Temperature:          temp,
			TopP:                 topP,
			TopK:                 topK,
			FrequencyPenalty:     freqPenalty,
			PresencePenalty:      presPenalty,
			SystemPromptOverride: trackCall.SystemPromptOverride,
			MaxCost:              trackCall.MaxCost,
			MaxTokens:            trackCall.MaxTokens,
			LargeModel:           &pinnedLarge,
			LogicalCallID:        trackCall.LogicalCallID, // Preserve logical ID
			ExistingMessageID:    trackCall.ExistingMessageID,
			InjectID:             trackCall.InjectID,
			FromDurableQueue:     trackCall.FromDurableQueue,
		}
		pinned.pin(&newCall)
		*trackCall = newCall
		return nil
	}

	// Interrupt-inject ticker: watches pending_injects for interrupt=true rows
	// written by `crush sessions inject --interrupt` in another process, and
	// (on the first hit) cancels the running turn and requeues the referenced
	// message so it picks up immediately. Bound to this turn's lifetime via
	// tickerCtx — stopped by the defer as soon as run() returns, so no
	// idle-process polling. Runs for BOTH the initial turn and every retry
	// re-run below (each run() sees a fresh ticker via this closure). The
	// defer ensures the ticker goroutine has joined before runInternal returns.
	tickerCtx, stopTicker := context.WithCancel(ctx)
	tickerDone := c.startInterruptTicker(tickerCtx, sessionID)
	// defers run LIFO: stopTicker (cancel tickerCtx) must fire before we
	// wait on tickerDone, or the join blocks forever waiting for a
	// goroutine that's still parked on <-ctx.Done(). Combine both into one
	// deferred func so the order is explicit and can't be silently
	// reordered by a future defer inserted between the two.
	defer func() {
		stopTicker()
		<-tickerDone
	}()

	beforeLoaded := c.skillTracker.LoadedNames()
	var result *fantasy.AgentResult
	originalErr := c.runWithUnauthorizedRetry(ctx, providerCfg, func() error {
		var err error
		result, err = run()
		return err
	}, rebuildCall)
	logTurnSkillUsage(sessionID, prompt, c.activeSkills, c.skillTracker, beforeLoaded)

	// Notify only if still unauthorized after retry — a successful
	// retry means the user doesn't need to re-authenticate.
	if originalErr != nil && c.isUnauthorized(originalErr) && c.notify != nil && model.ModelCfg.Provider == hyper.Name {
		c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: model.ModelCfg.Provider,
		})
	}

	// Auto-retry on transient provider failures. The agent may have written
	// a FinishReasonError message (stream stall, empty stream) or returned a
	// transient error (429 overload, 5xx, GOAWAY/EOF, network drop); we
	// re-run the turn with the same prompt after exponential backoff as long
	// as it produced NO content (so a re-run cannot clobber a partial answer)
	// AND the failure is not operator-actionable (quota wall, auth, context
	// overflow, bad request, user cancel).
	// "Solve it ourselves before bothering the user" — provider hiccups
	// (rate limits, HTTP/2 stalls, brief capacity drops) usually clear
	// within tens of seconds, so 2 retries after 10s + 30s of backoff
	// absorb the common cases without the orchestrator having to know.
	// The retried turn appears in session history as a fresh user+
	// assistant pair, which the model sees alongside the previous
	// failed attempt — slightly noisy but functionally correct.
	maxRetries := streamStallRetriesDefault
	if opts := c.cfg.Config().Options; opts != nil && opts.StreamStallRetries != nil {
		// Explicit override (including explicit 0 to disable entirely).
		maxRetries = *opts.StreamStallRetries
		if maxRetries < 0 {
			maxRetries = 0
		}
	}
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if !c.shouldRetryTurn(ctx, sessionID, originalErr) {
			break
		}
		backoff := streamStallRetryBaseBackoff
		for i := 1; i < attempt; i++ {
			backoff = time.Duration(float64(backoff) * streamStallRetryBackoffMultiplier)
		}
		slog.Warn(
			"coordinator: retrying transient turn failure",
			"session_id", sessionID,
			"attempt", attempt+1,
			"max_attempts", maxRetries+1,
			"backoff", backoff.String(),
		)
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(backoff):
		}
		result, originalErr = run()
	}

	return result, originalErr
}

// retryClass partitions a turn-terminating failure into "surface it" vs
// "transparently re-run it". See shouldRetryTurn for the policy.
type retryClass int

const (
	// classTerminal is an operator-actionable failure (quota wall, auth,
	// context overflow, bad request, user cancel) that must surface.
	classTerminal retryClass = iota
	// classTransient is a provider/network hiccup worth a re-run.
	classTransient
)

// classifyProviderError classifies a NON-NIL turn-terminating error.
// context cancellation is terminal here — watchdog stalls are matched
// separately by their persisted finish title in shouldRetryTurn, because
// a stall surfaces only as context.Canceled.
func classifyProviderError(err error) retryClass {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return classTerminal
	}
	// Peak-hours refusal is operator policy, not a transient hiccup: the
	// condition only clears when the wall clock leaves the window, so a
	// backoff retry would just burn the backoff and fail identically.
	if errors.Is(err, errProviderPeakHours) {
		return classTerminal
	}
	var providerErr *fantasy.ProviderError
	if errors.As(err, &providerErr) {
		if providerErr.IsContextTooLarge() {
			return classTerminal // the auto-summarize path owns this
		}
		switch providerErr.StatusCode {
		case http.StatusUnauthorized, http.StatusPaymentRequired:
			return classTerminal
		case http.StatusForbidden:
			// 403 is ambiguous: it can be a real auth/geo wall (retry pointless)
			// or a CDN/anti-abuse banner from a fronting balancer that clears in
			// tens of seconds (z.ai "Forbidden ZS", Cloudflare-fronted providers).
			// Treat as transient: the worst case is ~40s of bounded backoff on a
			// truly bad key vs. losing a long agent run on a momentary block.
			return classTransient
		case http.StatusTooManyRequests:
			if isQuotaLimit(providerErr) {
				return classTerminal // multi-hour usage wall — operator accepts a fast fail
			}
			return classTransient // momentary overload
		case http.StatusRequestTimeout, http.StatusConflict:
			return classTransient
		}
		if providerErr.StatusCode >= 500 {
			return classTransient
		}
		if providerErr.StatusCode >= 400 {
			return classTerminal // genuine client error (400, 404, ...)
		}
		// No HTTP status (status 0): EOF / network wrapped as ProviderError.
		if providerErr.IsRetryable() {
			return classTransient
		}
		return classTerminal
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return classTransient
	}
	return classTerminal
}

// isQuotaLimit reports whether a 429 is a hard usage/quota wall (resets on
// the order of hours) rather than a momentary overload. The two share
// status 429, so we discriminate on the provider's message text.
func isQuotaLimit(providerErr *fantasy.ProviderError) bool {
	msg := strings.ToLower(providerErr.Title + " " + providerErr.Message)
	return strings.Contains(msg, "usage limit") ||
		strings.Contains(msg, "limit will reset") ||
		strings.Contains(msg, "reset at") ||
		strings.Contains(msg, "quota")
}

// turnMadeProgress reports whether the assistant message carries any real
// output — text, reasoning, or a tool call (even a partial one). A turn
// that made progress must never be re-run: it would duplicate work the
// user already has.
func turnMadeProgress(msg message.Message) bool {
	return strings.TrimSpace(msg.FullText()) != "" ||
		strings.TrimSpace(msg.ReasoningContent().Thinking) != "" ||
		len(msg.ToolCalls()) > 0
}

// shouldRetryStalledMessage decides whether a watchdog-stalled assistant
// message warrants re-running the turn. The retry exists to recover from
// turns where the provider never delivered anything; ANY content reaching
// the assistant — text, reasoning, even a half-emitted tool call — proves
// the server received and processed the prompt, and re-running would just
// duplicate the user message in the DB and burn tokens redoing work the
// user already (partially) has.
//
// Returns false for any non-stalled finish reason (including nil), so the
// caller can pass the last assistant message unconditionally.
func shouldRetryStalledMessage(msg message.Message) bool {
	fp := msg.FinishPart()
	if fp == nil {
		return false
	}
	if fp.Reason != message.FinishReasonError || fp.Message != streamStalledFinishTitle {
		return false
	}
	return !turnMadeProgress(msg)
}

// lastAssistantMessage returns the most recent assistant message in the
// session. ok is false on a DB error or when there is no assistant message
// yet — callers treat that as "nothing to retry".
func (c *coordinator) lastAssistantMessage(ctx context.Context, sessionID string) (message.Message, bool) {
	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		return message.Message{}, false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.Assistant {
			return msgs[i], true
		}
	}
	return message.Message{}, false
}

// shouldRetryTurn decides whether a finished turn should be transparently
// re-run. A turn qualifies ONLY if it ended in error WITHOUT producing any
// content (so a re-run cannot clobber a partial answer) AND the failure is
// a transient provider/network hiccup rather than an operator-actionable
// condition. This generalizes the original stall-only retry: a watchdog
// stall is one transient class among several.
//
// Decision order:
//   - no assistant message / clean finish / user-cancel finish → don't retry
//   - turn produced any content                                 → don't retry
//   - persisted "Stream stalled" title (err is context.Canceled) → retry
//   - turn returned no error (empty-stream close)                → retry
//   - otherwise classify the returned error                      → transient?
func (c *coordinator) shouldRetryTurn(ctx context.Context, sessionID string, err error) bool {
	msg, ok := c.lastAssistantMessage(ctx, sessionID)
	if !ok {
		return false
	}
	fp := msg.FinishPart()
	if fp == nil || fp.Reason != message.FinishReasonError {
		return false
	}
	if turnMadeProgress(msg) {
		return false
	}
	if fp.Message == streamStalledFinishTitle {
		return true
	}
	if err == nil {
		return true
	}
	return classifyProviderError(err) == classTransient
}

// RunWithOverrides implements Coordinator. It is like Run but uses the given
// large/small model overrides instead of the global config defaults.
func (c *coordinator) RunWithOverrides(ctx context.Context, sessionID, prompt string, large, small *ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}

	// Carry session-level reasoning effort into the overrides so that
	// applyModelOverrides restores it after resetting the model config.
	if sess, err := c.sessions.Get(ctx, sessionID); err == nil {
		if large != nil && large.ReasoningEffort == "" && sess.LargeModelReasoningEffort != "" {
			large.ReasoningEffort = sess.LargeModelReasoningEffort
		}
		if small != nil && small.ReasoningEffort == "" && sess.SmallModelReasoningEffort != "" {
			small.ReasoningEffort = sess.SmallModelReasoningEffort
		}
	}

	pinned, err := c.applyModelOverrides(ctx, large, small)
	if err != nil {
		return nil, err
	}

	return c.runInternal(ctx, sessionID, prompt, pinned, attachments...)
}

// effectiveReasoningEffort returns the reasoning effort to apply for provider calls.
// It prefers the user-selected effort when valid, otherwise the model default when
// valid, and finally falls back to the first configured reasoning level.
func effectiveReasoningEffort(model Model) string {
	if !model.CatwalkCfg.CanReason {
		return ""
	}

	if effort := model.ModelCfg.ReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if effort := model.CatwalkCfg.DefaultReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if len(model.CatwalkCfg.ReasoningLevels) > 0 {
		return model.CatwalkCfg.ReasoningLevels[0]
	}
	return ""
}

func getProviderOptions(model Model, providerCfg config.ProviderConfig) fantasy.ProviderOptions {
	options := fantasy.ProviderOptions{}

	cfgOpts := []byte("{}")
	providerCfgOpts := []byte("{}")
	catwalkOpts := []byte("{}")

	if model.ModelCfg.ProviderOptions != nil {
		data, err := json.Marshal(model.ModelCfg.ProviderOptions)
		if err == nil {
			cfgOpts = data
		}
	}

	if providerCfg.ProviderOptions != nil {
		data, err := json.Marshal(providerCfg.ProviderOptions)
		if err == nil {
			providerCfgOpts = data
		}
	}

	if model.CatwalkCfg.Options.ProviderOptions != nil {
		data, err := json.Marshal(model.CatwalkCfg.Options.ProviderOptions)
		if err == nil {
			catwalkOpts = data
		}
	}

	readers := []io.Reader{
		bytes.NewReader(catwalkOpts),
		bytes.NewReader(providerCfgOpts),
		bytes.NewReader(cfgOpts),
	}

	got, err := jsons.Merge(readers)
	if err != nil {
		slog.Error("Could not merge call config", "err", err)
		return options
	}

	mergedOptions := make(map[string]any)

	err = json.Unmarshal([]byte(got), &mergedOptions)
	if err != nil {
		slog.Error("Could not create config for call", "err", err)
		return options
	}

	reasoningEffort := effectiveReasoningEffort(model)
	shouldSetEffort := model.CatwalkCfg.CanReason &&
		reasoningEffort != "" &&
		slices.Contains(model.CatwalkCfg.ReasoningLevels, reasoningEffort)

	switch providerCfg.Type {
	case openai.Name, azure.Name:
		_, hasReasoningEffort := mergedOptions["reasoning_effort"]
		if !hasReasoningEffort && shouldSetEffort {
			mergedOptions["reasoning_effort"] = reasoningEffort
		}
		if openai.IsResponsesModel(model.CatwalkCfg.ID) {
			if openai.IsResponsesReasoningModel(model.CatwalkCfg.ID) {
				mergedOptions["reasoning_summary"] = "auto"
				mergedOptions["include"] = []openai.IncludeType{openai.IncludeReasoningEncryptedContent}
			}
			parsed, err := openai.ParseResponsesOptions(mergedOptions)
			if err == nil {
				options[openai.Name] = parsed
			}
		} else {
			parsed, err := openai.ParseOptions(mergedOptions)
			if err == nil {
				options[openai.Name] = parsed
			}
		}
	case anthropic.Name, bedrock.Name:
		var (
			_, hasEffort = mergedOptions["effort"]
			_, hasThink  = mergedOptions["thinking"]
			extraBody    = make(map[string]any)
		)

		switch providerCfg.ID {
		case string(catwalk.InferenceProviderAlibabaSingapore):
			switch {
			case !hasEffort && shouldSetEffort:
				extraBody["reasoning_effort"] = reasoningEffort
			case !hasThink && model.CatwalkCfg.CanReason:
				if model.ModelCfg.Think {
					extraBody["thinking"] = map[string]any{"type": "enabled"}
				} else {
					extraBody["thinking"] = map[string]any{"type": "disabled"}
				}
			}
			mergedOptions["extra_body"] = extraBody

		default:
			switch {
			case !hasEffort && shouldSetEffort:
				mergedOptions["effort"] = reasoningEffort
			case !hasThink && model.ModelCfg.Think:
				mergedOptions["thinking"] = map[string]any{"budget_tokens": 2000}
			}
		}

		parsed, err := anthropic.ParseOptions(mergedOptions)
		if err == nil {
			options[anthropic.Name] = parsed
		}

	case openrouter.Name:
		_, hasReasoning := mergedOptions["reasoning"]
		if !hasReasoning && shouldSetEffort {
			mergedOptions["reasoning"] = map[string]any{
				"enabled": true,
				"effort":  reasoningEffort,
			}
		}
		parsed, err := openrouter.ParseOptions(mergedOptions)
		if err == nil {
			options[openrouter.Name] = parsed
		}
	case vercel.Name:
		_, hasReasoning := mergedOptions["reasoning"]
		if !hasReasoning && shouldSetEffort {
			mergedOptions["reasoning"] = map[string]any{
				"enabled": true,
				"effort":  reasoningEffort,
			}
		}
		parsed, err := vercel.ParseOptions(mergedOptions)
		if err == nil {
			options[vercel.Name] = parsed
		}
	case google.Name:
		_, hasReasoning := mergedOptions["thinking_config"]
		if !hasReasoning {
			if strings.HasPrefix(model.CatwalkCfg.ID, "gemini-2") {
				mergedOptions["thinking_config"] = map[string]any{
					"thinking_budget":  2000,
					"include_thoughts": true,
				}
			} else {
				mergedOptions["thinking_config"] = map[string]any{
					"thinking_level":   reasoningEffort,
					"include_thoughts": true,
				}
			}
		}
		parsed, err := google.ParseOptions(mergedOptions)
		if err == nil {
			options[google.Name] = parsed
		}
	case openaicompat.Name, hyper.Name:
		extraBody := make(map[string]any)

		_, hasReasoningEffort := mergedOptions["reasoning_effort"]
		if !hasReasoningEffort && shouldSetEffort {
			switch providerCfg.ID {
			case string(catwalk.InferenceProviderIoNet):
				extraBody["reasoning"] = map[string]string{"effort": reasoningEffort}
			default:
				mergedOptions["reasoning_effort"] = reasoningEffort
			}
		}

		// "reasoning effort" is a standard OpenAI field, but "thinking" is not.
		// Setting it in the right way for each provider.
		// TODO: Abstract this in Fantasy somehow?
		// TODO: Allow custom providers to specify how to set this?
		//
		// SYNC WARNING: the per-provider effort/thinking mapping below (ZAI,
		// DeepSeek, io.net, Alibaba Singapore, hyper) is restated in prose for
		// users by `crush models efforts` (providerEffortDocs in
		// internal/cmd/models_efforts.go). That prose is NOT derived from this
		// switch — if you change a mapping here, update the matching entry
		// there too, or the CLI help will describe stale behavior.
		switch providerCfg.ID {
		case hyper.Name:
			extraBody["thinking"] = model.ModelCfg.Think
		case string(catwalk.InferenceProviderIoNet):
			if _, ok := extraBody["reasoning"]; !ok && model.CatwalkCfg.CanReason {
				if model.ModelCfg.Think {
					extraBody["reasoning"] = map[string]string{"effort": "medium"}
				} else {
					extraBody["reasoning"] = map[string]string{"effort": "none"}
				}
			}
		case string(catwalk.InferenceProviderZAI):
			// GLM-5.x exposes two thinking-effort levels (high / max) and no
			// "unset" state on z.ai's side — reasoning is either on at some
			// level or off. Z.AI recommends max for hard coding/math tasks,
			// so an unset effort defaults to thinking ON at "high" rather
			// than silently disabling reasoning. Explicitly opt out with
			// ReasoningEffort == "off" (e.g. `crush models use
			// zai/glm-5.2@off <small>` — the raw provider/model@effort
			// syntax accepts any suffix, unvalidated against ReasoningLevels).
			//
			// We forward via `extra_body.reasoning_effort` — the
			// OpenAI-compat field z.ai already accepts on its Anthropic-
			// compatible coding endpoint. Mapping mirrors z.ai's own
			// "Claude Code selected effort → GLM-5.2 actual mapped effort"
			// table from docs.z.ai/devpack/latest-model:
			//   (unset), low, medium, high (default) → high
			//   xhigh, max, ultracode                → max
			//   off                                  → thinking disabled
			// Older GLM-4.x ignore the field harmlessly.
			effort := strings.ToLower(model.ModelCfg.ReasoningEffort)
			if effort == "off" {
				extraBody["thinking"] = map[string]any{
					"type": "disabled",
				}
			} else {
				extraBody["thinking"] = map[string]any{
					"type": "enabled",
				}
				switch effort {
				case "xhigh", "max", "ultracode":
					extraBody["reasoning_effort"] = "max"
				default:
					extraBody["reasoning_effort"] = "high"
				}
			}
		case string(catwalk.InferenceProviderDeepSeek):
			// DeepSeek keeps the fork's original "unset reasoning = thinking
			// off" default. Reasoning is only enabled when the user opts in
			// (via Think or an explicit ReasoningEffort); the ZAI-only default
			// of turning thinking ON at "high" for an unset effort deliberately
			// does NOT apply here.
			if model.ModelCfg.Think || model.ModelCfg.ReasoningEffort != "" {
				extraBody["thinking"] = map[string]any{
					"type": "enabled",
				}
			} else {
				extraBody["thinking"] = map[string]any{
					"type": "disabled",
				}
			}
			// When reasoning is enabled, map the selected effort onto the two
			// effort levels the endpoint exposes (high / max). Mirrors the ZAI
			// table above:
			//   low, medium, high (default) → high
			//   xhigh, max, ultracode       → max
			if model.ModelCfg.ReasoningEffort != "" {
				switch strings.ToLower(model.ModelCfg.ReasoningEffort) {
				case "xhigh", "max", "ultracode":
					extraBody["reasoning_effort"] = "max"
				default:
					extraBody["reasoning_effort"] = "high"
				}
			}
		case string(catwalk.InferenceProviderAlibabaSingapore):
			if model.CatwalkCfg.CanReason {
				extraBody["enable_thinking"] = model.ModelCfg.Think
			}
		}

		mergedOptions["extra_body"] = extraBody

		parsed, err := openaicompat.ParseOptions(mergedOptions)
		if err == nil {
			options[openaicompat.Name] = parsed
		}
	default:
		// Known custom providers (litellm, ollama, omlx, lmstudio) speak
		// openai-compat under the hood, so route their options through the
		// openai-compat parser too.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			parsed, err := openaicompat.ParseOptions(mergedOptions)
			if err == nil {
				options[openaicompat.Name] = parsed
			}
		}
	}

	return options
}

func mergeCallOptions(model Model, cfg config.ProviderConfig) (fantasy.ProviderOptions, *float64, *float64, *int64, *float64, *float64) {
	modelOptions := getProviderOptions(model, cfg)
	temp := cmp.Or(model.ModelCfg.Temperature, model.CatwalkCfg.Options.Temperature)
	topP := cmp.Or(model.ModelCfg.TopP, model.CatwalkCfg.Options.TopP)
	topK := cmp.Or(model.ModelCfg.TopK, model.CatwalkCfg.Options.TopK)
	freqPenalty := cmp.Or(model.ModelCfg.FrequencyPenalty, model.CatwalkCfg.Options.FrequencyPenalty)
	presPenalty := cmp.Or(model.ModelCfg.PresencePenalty, model.CatwalkCfg.Options.PresencePenalty)
	return modelOptions, temp, topP, topK, freqPenalty, presPenalty
}

func (c *coordinator) buildAgent(ctx context.Context, prompt *prompt.Prompt, agent config.Agent, isSubAgent bool) (SessionAgent, error) {
	large, small, err := c.buildAgentModels(ctx, isSubAgent)
	if err != nil {
		return nil, err
	}

	largeProviderCfg, _ := c.cfg.Config().Providers.Get(large.ModelCfg.Provider)
	opts := c.cfg.Config().Options
	var streamIdleTimeout time.Duration
	if opts != nil && opts.StreamIdleTimeoutSeconds > 0 {
		streamIdleTimeout = time.Duration(opts.StreamIdleTimeoutSeconds) * time.Second
	}
	// Never-freeze backstop: bound the watchdog's tool-pause.
	var toolMaxDuration time.Duration
	if opts != nil && opts.StreamToolTimeoutSeconds > 0 {
		toolMaxDuration = time.Duration(opts.StreamToolTimeoutSeconds) * time.Second
	}
	// Fork patch: batch 8 — mid-stream checkpoint interval.
	var checkpointInterval time.Duration
	if opts != nil {
		switch {
		case opts.CheckpointIntervalSeconds > 0:
			checkpointInterval = time.Duration(opts.CheckpointIntervalSeconds) * time.Second
		case opts.CheckpointIntervalSeconds == -1:
			checkpointInterval = 0 // explicitly disabled
		default:
			checkpointInterval = defaultCheckpointInterval
		}
	}
	result := NewSessionAgent(SessionAgentOptions{
		LargeModel:           large,
		SmallModel:           small,
		SystemPromptPrefix:   largeProviderCfg.SystemPromptPrefix,
		SystemPrompt:         "",
		IsSubAgent:           isSubAgent,
		DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
		IsYolo:               c.permissions.SkipRequests(),
		Sessions:             c.sessions,
		Messages:             c.messages,
		Tools:                nil,
		Notify:               c.notify,
		StreamIdleTimeout:    streamIdleTimeout,
		ToolMaxDuration:      toolMaxDuration,
		DataDirectory:        c.cfg.Config().Options.DataDirectory,
		CheckpointInterval:   checkpointInterval, // Fork patch: batch 8
		// Fork patch: peak-hours mid-turn re-check. Re-reads the provider
		// config live, and reloads from disk when tracked config files change,
		// so a peak_hours edit made by another process while this turn is
		// running still takes effect.
		PeakHoursCheck: func() error {
			return c.checkLivePeakHours(large.ModelCfg.Provider)
		},
	})

	c.readyWg.Go(func() error {
		// Orchestrator block only ever applies to the top-level coder prompt
		// (isSubAgent false) — a sub-agent renders task.md.tpl via
		// taskPrompt, which doesn't reference WorkerAvailable, but guarding
		// on !isSubAgent here keeps this call's intent explicit rather than
		// relying on the template to ignore the field.
		systemPrompt, err := prompt.Build(ctx, large.Model.Provider(), large.Model.Model(), c.cfg, !isSubAgent && c.workerSubAgentActive(c.cfg.Config()))
		if err != nil {
			return err
		}
		result.SetSystemPrompt(systemPrompt)
		return nil
	})

	c.readyWg.Go(func() error {
		tools, err := c.buildTools(ctx, agent, isSubAgent)
		if err != nil {
			return err
		}
		result.SetTools(tools)
		return nil
	})

	return result, nil
}

// workerToolNames are added on top of the sub-agent's existing (read-only)
// AllowedTools when it is acting as a worker (see workerSubAgentActive):
// enough hands to actually do delegated work, not just look around.
//
// Deliberately excludes "agent": a worker must not spawn sub-workers of its
// own — recursion guard, keeps the delegation tree exactly two levels deep
// (orchestrator -> worker).
//
// Includes "ask_question": runSubAgent now recognizes AwaitingAnswerError
// before its generic error branch and returns a question-shaped SUCCESSFUL
// tool response (see subAgentQuestionText in question_stop.go) instead of
// collapsing it into "Failed to generate response: ...". The orchestrator
// answers via AgentParams.ResumeSessionID, which continues the same
// sub-session rather than starting a fresh one. See
// docs/plans/2026-07-26-orchestrator-worker-e2e.md, phase 3, for the full
// round-trip design and why a synchronous blocking version is impossible.
var workerToolNames = []string{"edit", "multiedit", "write", "bash", "todos", "download", "fetch", tools.AskQuestionToolName}

// buildToolsAgentConfig returns the config.Agent buildTools should use to
// resolve AllowedTools for this build. For a sub-agent acting as a worker
// (see workerSubAgentActive), it returns a copy of agent with the worker
// toolset layered on top of whatever was already allowed (today's read-only
// set, in practice, since this only affects the AgentTask sub-agent). In
// every other case — including the top-level coder, and the sub-agent when
// no Worker is configured or the active role isn't smart — it returns agent
// unchanged, so behavior is byte-identical to before this method existed.
func (c *coordinator) buildToolsAgentConfig(agent config.Agent, isSubAgent bool) config.Agent {
	// buildTools (this method's only caller) does not thread a pinned
	// snapshot through its own many live c.cfg.Config() reads, so this call
	// stays consistent with its caller's existing (out of scope for task
	// #341/P1-1) behavior rather than pinning a snapshot only here.
	if !isSubAgent || !c.workerSubAgentActive(c.cfg.Config()) {
		return agent
	}

	allowed := make([]string, len(agent.AllowedTools), len(agent.AllowedTools)+len(workerToolNames))
	copy(allowed, agent.AllowedTools)
	for _, name := range workerToolNames {
		if !slices.Contains(allowed, name) {
			allowed = append(allowed, name)
		}
	}
	agent.AllowedTools = allowed
	return agent
}

func (c *coordinator) buildTools(ctx context.Context, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	agent = c.buildToolsAgentConfig(agent, isSubAgent)

	// SSRF guard escape hatch (Options.AllowPrivateNetworkFetch, off by
	// default): when enabled, every model-facing HTTP tool below gets an
	// explicit allowPrivate=true client instead of letting its own nil
	// fallback build the guarded default. See ssrf_guard.go.
	allowPrivateNetworkFetch := c.cfg.Config().Options.AllowPrivateNetworkFetch
	fetchClient := func(timeout time.Duration) *http.Client {
		if !allowPrivateNetworkFetch {
			return nil
		}
		return tools.NewSSRFGuardedClient(timeout, true)
	}

	var allTools []fantasy.AgentTool
	if slices.Contains(agent.AllowedTools, AgentToolName) {
		agentTool, err := c.agentTool(ctx)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, agentTool)
	}

	if slices.Contains(agent.AllowedTools, tools.AgenticFetchToolName) {
		agenticFetchTool, err := c.agenticFetchTool(ctx, fetchClient(30*time.Second))
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, agenticFetchTool)
	}

	// Get the model name for the agent
	modelID := ""
	if modelCfg, ok := c.cfg.Config().Models[agent.Model]; ok {
		if model := c.cfg.Config().GetModel(modelCfg.Provider, modelCfg.Model); model != nil {
			modelID = model.ID
		}
	}

	logFile := filepath.Join(c.cfg.Config().Options.DataDirectory, "logs", "crush.log")

	// Build hook runner if PreToolUse hooks are configured.
	var hookRunner *hooks.Runner
	if preToolHooks := c.cfg.Config().Hooks[hooks.EventPreToolUse]; len(preToolHooks) > 0 {
		hookRunner = hooks.NewRunner(preToolHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}

	// Background-job completion notification (web/interactive only).
	// When a bash command auto-backgrounds and later finishes, push a
	// one-message completion notice into the owning session via the
	// existing InjectMessage path. Kill-switch defaults to ON; a session
	// that is BUSY merges it into the running turn, IDLE sessions get a
	// persisted message (no auto-resume). crush run is single-turn and
	// never receives it.
	opts := c.cfg.Config().Options
	notifyDone := opts.NotifyOnBackgroundJobDone == nil || *opts.NotifyOnBackgroundJobDone
	var onBgDone func(string, *shell.BackgroundShell)
	if notifyDone {
		onBgDone = func(sessionID string, sh *shell.BackgroundShell) {
			c.notifyBackgroundJobDone(sessionID, sh)
		}
	}

	allTools = append(
		allTools,
		tools.NewAskQuestionTool(),
		tools.NewBashTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Options.Attribution, modelID, onBgDone),
		tools.NewCrushInfoTool(c.cfg, c.allSkills, c.activeSkills, c.skillTracker),
		tools.NewCrushLogsTool(logFile),
		tools.NewJobOutputTool(),
		tools.NewJobKillTool(),
		tools.NewDownloadTool(c.permissions, c.cfg.WorkingDir(), fetchClient(5*time.Minute)),
		tools.NewEditTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewMultiEditTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewFetchTool(c.permissions, c.cfg.WorkingDir(), fetchClient(30*time.Second)),
		tools.NewGlobTool(c.cfg.WorkingDir()),
		tools.NewGrepTool(c.cfg.WorkingDir(), c.cfg.Config().Tools.Grep),
		tools.NewLsTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Tools.Ls),
		tools.NewReadDelegationTranscriptTool(c.sessions, c.messages),
		tools.NewSourcegraphTool(fetchClient(30*time.Second)),
		tools.NewTodosTool(c.sessions),
		tools.NewViewTool(c.permissions, c.filetracker, c.skillTracker, c.cfg.WorkingDir(), c.cfg.Config().Options.SkillsPaths...),
		tools.NewWriteTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
	)

	if len(c.cfg.Config().MCP) > 0 {
		allTools = append(
			allTools,
			tools.NewListMCPResourcesTool(c.cfg, c.permissions),
			tools.NewReadMCPResourceTool(c.cfg, c.permissions),
		)
	}

	var filteredTools []fantasy.AgentTool
	for _, tool := range allTools {
		if slices.Contains(agent.AllowedTools, tool.Info().Name) {
			filteredTools = append(filteredTools, tool)
		}
	}

	for _, tool := range tools.GetMCPTools(c.permissions, c.cfg, c.cfg.WorkingDir()) {
		if agent.AllowedMCP == nil {
			// No MCP restrictions
			filteredTools = append(filteredTools, tool)
			continue
		}
		if len(agent.AllowedMCP) == 0 {
			// No MCPs allowed
			slog.Debug("No MCPs allowed", "tool", tool.Name(), "agent", agent.Name)
			break
		}

		for mcp, tools := range agent.AllowedMCP {
			if mcp != tool.MCP() {
				continue
			}
			if len(tools) == 0 || slices.Contains(tools, tool.MCPToolName()) {
				filteredTools = append(filteredTools, tool)
				break
			}
			slog.Debug("MCP not allowed", "tool", tool.Name(), "agent", agent.Name)
		}
	}
	slices.SortFunc(filteredTools, func(a, b fantasy.AgentTool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})

	// Wrap tools with hook interception for the top-level agent only.
	// Sub-agents (the `agent` task tool, `agentic_fetch`, etc.) run
	// without hook interception to avoid firing the user's hook N times
	// per delegated turn. The top-level invocation of the sub-agent tool
	// itself is still wrapped from the coder's side.
	filteredTools = wrapToolsWithHooks(filteredTools, hookRunner, isSubAgent)

	return filteredTools, nil
}

// workerSubAgentActive reports whether a sub-agent being built right now is
// acting as a "worker": the parent run is driven by the Large ("smart") slot
// — or the active role is unknown, which for the interactive TUI/web path is
// equivalent to smart — AND a Worker model is actually configured. This is
// the single shared predicate for "the sub-agent should behave like a
// worker", used both to pick the sub-agent's model (buildAgentModels, below)
// and to pick its tool set (buildTools): a sub-agent that gets the Worker
// model but stays read-only, or vice versa, would defeat the point of the
// feature. isSubAgent must be checked by the caller first — this method
// assumes it's already true and only re-checks the role/config gate.
//
// cfg MUST be the same *config.Config the caller already pinned via
// Snapshot() for the rest of its build/resolve operation (task #341, P1-1).
// This method used to take no cfg argument and read c.cfg.Config() live
// instead — a torn read: buildAgentModels captured one generation via
// Snapshot() for the large/small/worker model lookups, then this method
// re-read Models[Worker] from whatever generation happened to be published
// at the moment it ran. A reload landing in between could hand back a
// worker slot from a DIFFERENT generation than the one buildAgentModels
// otherwise built from, up to and including a zero-value model or a
// provider lookup that no longer resolves. Threading the same *config.Config
// through closes that gap: every reader of "is worker active" within one
// resolve/build operation now agrees on exactly one generation.
//
// Mirrors the semantics documented on buildAgentModels below: falls through
// to false (today's behavior) when Worker isn't configured, or when the
// operator explicitly chose a non-large role (fast/worker/reviewer) for the
// whole run — we don't second-guess that choice by force-upgrading/
// downgrading sub-agents. Fork patch (reviewer/worker roles).
func (c *coordinator) workerSubAgentActive(cfg *config.Config) bool {
	c.activeModelRoleMu.Lock()
	activeRole := c.activeModelRole
	c.activeModelRoleMu.Unlock()

	if activeRole != "" && activeRole != config.SelectedModelTypeLarge {
		return false
	}

	workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]
	return ok && workerModelCfg.Model != ""
}

// TODO: when we support multiple agents we need to change this so that we pass in the agent specific model config
func (c *coordinator) buildAgentModels(ctx context.Context, isSubAgent bool) (Model, Model, error) {
	// Single atomic snapshot for every config read below (task #341/P1-3;
	// gap found by independent review — this function used to read
	// large/small/worker each via a separate c.cfg.Config() call, then take
	// a fourth, separate Snapshot() just for buildModelsFromCfg's provider
	// lookups. A reload landing between any of those reads could produce a
	// cross-generation mix, exactly what Snapshot() exists to prevent.
	cfg, _ := c.cfg.Snapshot()

	largeModelCfg, ok := cfg.Models[config.SelectedModelTypeLarge]
	if !ok {
		return Model{}, Model{}, errLargeModelNotSelected
	}
	smallModelCfg, ok := cfg.Models[config.SelectedModelTypeSmall]
	if !ok {
		return Model{}, Model{}, errSmallModelNotSelected
	}

	// Fork patch (reviewer/worker roles): when spawning a sub-agent acting as
	// a worker (see workerSubAgentActive), prefer the cheaper Worker slot for
	// the sub-agent's large-model slot. This never touches the small-model
	// slot, and falls through to today's behavior (Large for everything)
	// otherwise.
	if isSubAgent && c.workerSubAgentActive(cfg) {
		largeModelCfg = cfg.Models[config.SelectedModelTypeWorker]
	}

	return c.buildModelsFromCfg(ctx, cfg, largeModelCfg, smallModelCfg, isSubAgent)
}

// buildModelsFromCfg builds Model objects from explicit SelectedModel configs.
// The cfg parameter must be from a single atomic Snapshot() call to ensure
// consistency across all provider reads (task #341, P1-3).
func (c *coordinator) buildModelsFromCfg(ctx context.Context, cfg *config.Config, largeModelCfg, smallModelCfg config.SelectedModel, isSubAgent bool) (Model, Model, error) {
	largeProviderCfg, ok := cfg.Providers.Get(largeModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errLargeModelProviderNotConfigured
	}

	largeProvider, err := c.buildProvider(largeProviderCfg, largeModelCfg, isSubAgent)
	if err != nil {
		return Model{}, Model{}, err
	}

	smallProviderCfg, ok := cfg.Providers.Get(smallModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errSmallModelProviderNotConfigured
	}

	smallProvider, err := c.buildProvider(smallProviderCfg, smallModelCfg, true)
	if err != nil {
		return Model{}, Model{}, err
	}

	var largeCatwalkModel *catwalk.Model
	var smallCatwalkModel *catwalk.Model

	for _, m := range largeProviderCfg.Models {
		if m.ID == largeModelCfg.Model {
			largeCatwalkModel = &m
		}
	}
	for _, m := range smallProviderCfg.Models {
		if m.ID == smallModelCfg.Model {
			smallCatwalkModel = &m
		}
	}

	if largeCatwalkModel == nil {
		return Model{}, Model{}, errLargeModelNotFound
	}

	if smallCatwalkModel == nil {
		return Model{}, Model{}, errSmallModelNotFound
	}

	largeModelID := largeModelCfg.Model
	smallModelID := smallModelCfg.Model

	if largeModelCfg.Provider == openrouter.Name && isExactoSupported(largeModelID) {
		largeModelID += ":exacto"
	}

	if smallModelCfg.Provider == openrouter.Name && isExactoSupported(smallModelID) {
		smallModelID += ":exacto"
	}

	largeModel, err := largeProvider.LanguageModel(ctx, largeModelID)
	if err != nil {
		return Model{}, Model{}, err
	}
	smallModel, err := smallProvider.LanguageModel(ctx, smallModelID)
	if err != nil {
		return Model{}, Model{}, err
	}

	return Model{
			Model:      largeModel,
			CatwalkCfg: *largeCatwalkModel,
			ModelCfg:   largeModelCfg,
			FlatRate:   largeProviderCfg.FlatRate,
		}, Model{
			Model:      smallModel,
			CatwalkCfg: *smallCatwalkModel,
			ModelCfg:   smallModelCfg,
			FlatRate:   smallProviderCfg.FlatRate,
		}, nil
}

func (c *coordinator) buildAnthropicProvider(baseURL, apiKey string, headers map[string]string, providerID string) (fantasy.Provider, error) {
	var opts []anthropic.Option

	switch {
	case strings.HasPrefix(apiKey, "Bearer "):
		// NOTE: Prevent the SDK from picking up the API key from env.
		os.Setenv("ANTHROPIC_API_KEY", "")
		headers["Authorization"] = apiKey
	case providerID == string(catwalk.InferenceProviderMiniMax) || providerID == string(catwalk.InferenceProviderMiniMaxChina):
		// NOTE: Prevent the SDK from picking up the API key from env.
		os.Setenv("ANTHROPIC_API_KEY", "")
		headers["Authorization"] = "Bearer " + apiKey
	case apiKey != "":
		// X-Api-Key header
		opts = append(opts, anthropic.WithAPIKey(apiKey))
	}

	if len(headers) > 0 {
		opts = append(opts, anthropic.WithHeaders(headers))
	}

	if baseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(baseURL))
	}

	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, anthropic.WithHTTPClient(httpClient))
	}
	return anthropic.New(opts...)
}

func (c *coordinator) buildOpenaiProvider(baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithUseResponsesAPI(),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, openai.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openai.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	return openai.New(opts...)
}

func (c *coordinator) buildOpenrouterProvider(_, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []openrouter.Option{
		openrouter.WithAPIKey(apiKey),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, openrouter.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openrouter.WithHeaders(headers))
	}
	return openrouter.New(opts...)
}

func (c *coordinator) buildVercelProvider(_, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []vercel.Option{
		vercel.WithAPIKey(apiKey),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, vercel.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, vercel.WithHeaders(headers))
	}
	return vercel.New(opts...)
}

func (c *coordinator) buildOpenaiCompatProvider(baseURL, apiKey string, headers map[string]string, extraBody map[string]any, providerID string, isSubAgent bool) (fantasy.Provider, error) {
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		opts = append(
			opts,
			openaicompat.WithUseResponsesAPI(),
			openaicompat.WithResponsesAPIFunc(func(modelID string) bool {
				return copilotResponsesModels[modelID]
			}),
		)
		httpClient = copilot.NewClient(isSubAgent, c.cfg.Config().Options.Debug)
	}
	if httpClient == nil && c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	if httpClient != nil {
		opts = append(opts, openaicompat.WithHTTPClient(httpClient))
	}

	if len(headers) > 0 {
		opts = append(opts, openaicompat.WithHeaders(headers))
	}

	for extraKey, extraValue := range extraBody {
		opts = append(opts, openaicompat.WithSDKOptions(openaisdk.WithJSONSet(extraKey, extraValue)))
	}

	return openaicompat.New(opts...)
}

func (c *coordinator) buildAzureProvider(baseURL, apiKey string, headers map[string]string, options map[string]string) (fantasy.Provider, error) {
	opts := []azure.Option{
		azure.WithBaseURL(baseURL),
		azure.WithAPIKey(apiKey),
		azure.WithUseResponsesAPI(),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, azure.WithHTTPClient(httpClient))
	}
	if options == nil {
		options = make(map[string]string)
	}
	if apiVersion, ok := options["apiVersion"]; ok {
		opts = append(opts, azure.WithAPIVersion(apiVersion))
	}
	if len(headers) > 0 {
		opts = append(opts, azure.WithHeaders(headers))
	}

	return azure.New(opts...)
}

func (c *coordinator) buildBedrockProvider(apiKey string, headers map[string]string) (fantasy.Provider, error) {
	var opts []bedrock.Option
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, bedrock.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, bedrock.WithHeaders(headers))
	}
	switch {
	case apiKey != "":
		opts = append(opts, bedrock.WithAPIKey(apiKey))
	case os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "":
		opts = append(opts, bedrock.WithAPIKey(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")))
	default:
		// Skip, let the SDK do authentication.
	}
	return bedrock.New(opts...)
}

func (c *coordinator) buildGoogleProvider(baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []google.Option{
		google.WithBaseURL(baseURL),
		google.WithGeminiAPIKey(apiKey),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}
	return google.New(opts...)
}

func (c *coordinator) buildGoogleVertexProvider(headers map[string]string, options map[string]string) (fantasy.Provider, error) {
	opts := []google.Option{}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}

	project := options["project"]
	location := options["location"]

	opts = append(opts, google.WithVertex(project, location))

	return google.New(opts...)
}

func (c *coordinator) isAnthropicThinking(model config.SelectedModel) bool {
	if model.Think {
		return true
	}
	opts, err := anthropic.ParseOptions(model.ProviderOptions)
	return err == nil && opts.Thinking != nil
}

func (c *coordinator) buildProvider(providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool) (fantasy.Provider, error) {
	headers := maps.Clone(providerCfg.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	// handle special headers for anthropic
	if providerCfg.Type == anthropic.Name && c.isAnthropicThinking(model) {
		if v, ok := headers["anthropic-beta"]; ok {
			headers["anthropic-beta"] = v + ",interleaved-thinking-2025-05-14"
		} else {
			headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		}
	}

	apiKey, _ := c.cfg.Resolve(providerCfg.APIKey)
	baseURL, _ := c.cfg.Resolve(providerCfg.BaseURL)

	switch providerCfg.ID {
	case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
		if opencodeMessagesModels[model.Model] {
			baseURL = strings.TrimSuffix(baseURL, "/v1")
			return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID)
		}
	}

	switch providerCfg.Type {
	case openai.Name:
		return c.buildOpenaiProvider(baseURL, apiKey, headers)
	case anthropic.Name:
		return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID)
	case openrouter.Name:
		return c.buildOpenrouterProvider(baseURL, apiKey, headers)
	case vercel.Name:
		return c.buildVercelProvider(baseURL, apiKey, headers)
	case azure.Name:
		return c.buildAzureProvider(baseURL, apiKey, headers, providerCfg.ExtraParams)
	case bedrock.Name:
		return c.buildBedrockProvider(apiKey, headers)
	case google.Name:
		return c.buildGoogleProvider(baseURL, apiKey, headers)
	case "google-vertex":
		return c.buildGoogleVertexProvider(headers, providerCfg.ExtraParams)
	case openaicompat.Name, hyper.Name:
		switch providerCfg.ID {
		case hyper.Name:
			baseURL = hyper.BaseURL() + "/v1"
			headers["x-crush-id"] = event.GetID()
		case string(catwalk.InferenceProviderZAI):
			if providerCfg.ExtraBody == nil {
				providerCfg.ExtraBody = map[string]any{}
			}
			providerCfg.ExtraBody["tool_stream"] = true
		}
		return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent)
	case cliprovider.ProviderType:
		return cliprovider.New(c.cfg.WorkingDir(), c.permissions.SkipRequests, c.permissions, c.sessions, &externalMCPProxy{cfg: c.cfg}), nil
	default:
		// Known custom providers (litellm, ollama, omlx, lmstudio) are
		// openai-compat under the hood.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent)
		}
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	}
}

// externalMCPProxy implements cliprovider.ExternalMCPProxy by delegating to
// the internal mcp package for tool listing and execution.
type externalMCPProxy struct {
	cfg *config.ConfigStore
}

func (p *externalMCPProxy) ListTools() []cliprovider.ExternalMCPTool {
	var result []cliprovider.ExternalMCPTool
	for serverName, tools := range mcp.Tools() {
		for _, t := range tools {
			result = append(result, cliprovider.ExternalMCPTool{
				ServerName:  serverName,
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
		}
	}
	return result
}

func (p *externalMCPProxy) CallTool(ctx context.Context, serverName, toolName, inputJSON string) (string, error) {
	result, err := mcp.RunTool(ctx, p.cfg, serverName, toolName, inputJSON)
	if err != nil {
		return "", err
	}
	return result.Content, nil
}

func isExactoSupported(modelID string) bool {
	supportedModels := []string{
		"moonshotai/kimi-k2-0905",
		"deepseek/deepseek-v3.1-terminus",
		"z-ai/glm-4.6",
		"openai/gpt-oss-120b",
		"qwen/qwen3-coder",
	}
	return slices.Contains(supportedModels, modelID)
}

func (c *coordinator) Cancel(sessionID string) {
	c.currentAgent.Cancel(sessionID)
}

func (c *coordinator) CancelAll() (stillBusy bool) {
	return c.currentAgent.CancelAll()
}

func (c *coordinator) ClearQueue(sessionID string) {
	c.currentAgent.ClearQueue(sessionID)
}

// startInterruptTicker launches a goroutine that polls pending_injects for an
// interrupt=true row for sessionID every interruptInjectTick, for as long as
// ctx is live (i.e. the duration of the owning turn). On each interrupt row
// it consumes it, requeues the already-persisted message via
// ConsumeInterruptInjectAndEnqueue, and cancels the running generation.
//
// CORRECTED (task #421/P0-1; the original text here claimed "a replacement
// turn runs under the same coordinator-level Run", which stopped being true
// once handleInterruptTick started marking calls FromDurableQueue=true —
// see mailbox.go's guard on mb.replacement): for a durable-queue-originated
// interrupt, cancelling the current generation does NOT hand this Run() call
// a replacement turn to keep running — sessionAgent.Run's turn loop simply
// ends (hasNext=false), and this ticker's own ctx (the owning Run call's)
// is cancelled right along with it, so the ticker exits too. The durable row
// is the only remaining owner of the interrupted work; it is executed
// SEPARATELY, in the same OS process but outside this Run call, by
// RunNonInteractive's DrainSessionNow call (internal/app/app.go) once this
// Run() returns — not by this ticker continuing to poll for it. The one
// case this ticker DOES keep ticking across is a NON-durable interrupt
// (InterruptAndSend, still sets mb.replacement): that replacement genuinely
// does run under this same Run call, and remains interruptible by a second
// cross-process interrupt via this same ticker, exactly as originally
// documented. The goroutine also exits when ctx is cancelled (turn finished/
// aborted), so it never outlives the turn either way.
//
// Returns a channel that is closed when the ticker goroutine exits. Callers
// should defer a receive from this channel to ensure the goroutine has fully
// joined before returning, avoiding in-flight handleInterruptTick execution
// after context cancellation and DB cleanup.
func (c *coordinator) startInterruptTicker(ctx context.Context, sessionID string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interruptInjectTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// P1-3 fix: give each tick a bounded operation deadline so that
				// even if handleInterruptTick blocks (e.g., on a DB call or
				// downstream dependency that ignores parent ctx cancellation),
				// the tick returns within interruptTickOperationTimeout and we
				// can observe ctx.Done() on the next loop iteration.
				tickCtx, tickCancel := context.WithTimeout(ctx, interruptTickOperationTimeout)
				_, err := c.handleInterruptTick(tickCtx, sessionID)
				tickCancel()

				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) {
						// This is a signal that some operation inside handleInterruptTick
						// blocked without respecting ctx cancellation. Log at warning level
						// so it's visible in production and warrants investigation, but don't
						// stop the ticker — subsequent ticks may succeed.
						slog.Warn("coordinator: interrupt-inject tick timed out",
							"session_id", sessionID, "timeout", interruptTickOperationTimeout)
					} else {
						slog.Warn("coordinator: interrupt-inject tick failed",
							"session_id", sessionID, "err", err)
					}
					continue
				}
				// Continue ticking. Whether a replacement turn "remains
				// interruptible by this same ticker" depends on which path
				// fired above — see startInterruptTicker's own doc for the
				// distinction (task #421/P0-1 correction): a durable-queue
				// interrupt has no replacement turn under THIS Run call at
				// all (the row is executed separately, after Run returns),
				// so this loop iteration is really just clearing the way for
				// the ctx-cancellation exit that follows shortly. A
				// non-durable interrupt's replacement genuinely does run
				// here and stays covered by this same ticker. Either way,
				// subsequent interrupts are handled correctly (the durable
				// queue serves FIFO ordering regardless of which path
				// consumes it).
			}
		}
	}()
	return done
}

// handleInterruptTick performs one poll of the interrupt-inject queue. It
// returns fired=true when it consumed an interrupt row and issued a
// cancel+requeue (the caller then stops ticking). Extracted from the ticker
// goroutine so it can be unit-tested directly with a real session.Service and
// message.Service, without a live provider. It is a no-op returning
// (false, nil) when no interrupt row is pending.
//
// P0-2 fix (atomic): uses ConsumeInterruptInjectAndEnqueue to delete and
// enqueue in a single transaction, eliminating the data loss window where
// a separate delete-then-enqueue sequence could lose the row if enqueue failed.
// This approach requires building the call data BEFORE the atomic transaction.
// Every fallible step (messages.Get, buildCall, marshal) runs BEFORE the
// atomic consume — PeekInterruptInject does not delete, so a failure there
// simply leaves the row in place for the next tick to retry naturally; no
// explicit recreation is needed. Once ConsumeInterruptInjectAndEnqueue
// commits, the call is durably enqueued and nothing after that point
// (Notify, InterruptAndReplace) can lose it.
func (c *coordinator) handleInterruptTick(ctx context.Context, sessionID string) (bool, error) {
	// First, peek at the row to get the message reference (SELECT only, no delete)
	pi, err := c.sessions.PeekInterruptInject(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if pi == nil {
		return false, nil
	}

	// Load the message to build call data
	injMsg, getErr := c.messages.Get(ctx, pi.MessageID)
	if getErr != nil {
		return false, fmt.Errorf("interrupt inject references missing message %q: %w", pi.MessageID, getErr)
	}

	// Resolve the session's model configuration from the DB or config
	// defaults so this cross-process interrupt tick respects a persisted
	// per-session model override instead of falling back to the shared/
	// global model (same class of bug as InterruptAndSend,
	// closed for that call site separately).
	pinned, resolveErr := c.resolveSessionModels(ctx, sessionID)
	if resolveErr != nil {
		return false, fmt.Errorf("failed to resolve session models for interrupt tick: %w", resolveErr)
	}

	// Build the call
	call, buildErr := c.buildCall(ctx, sessionID, injMsg.FullText(), pinned, nil)
	if buildErr != nil {
		return false, buildErr
	}

	// Reference the existing row; the agent must not re-create it.
	call.ExistingMessageID = pi.MessageID
	call.InjectID = pi.ID

	// Mark as originating from the durable queue so InterruptAndReplace skips
	// mb.replacement to avoid double-execution (P0-1 fix). The pump will execute
	// the durable row directly; we only cancel the in-flight generation here.
	call.FromDurableQueue = true

	// Generate idempotency key
	var idempotencyKey string
	if call.LogicalCallID != "" {
		idempotencyKey = fmt.Sprintf("%s-%s", call.SessionID, call.LogicalCallID)
	} else {
		idempotencyKey = fmt.Sprintf("%s-%s", call.SessionID, call.InjectID)
	}

	// Convert to SessionAgentCallData for serialization
	callData := ToSessionAgentCallData(call)
	callDataJSON, marshalErr := json.Marshal(callData)
	if marshalErr != nil {
		return false, fmt.Errorf("failed to serialize call data for interrupt inject: %w", marshalErr)
	}

	// Now atomically consume (delete) and enqueue in one transaction, matching
	// the exact row peeked above so a concurrent deletion of that row (rather
	// than a stale re-select of "the oldest row") can never cause us to
	// consume a different row than the one callData was built from.
	enqueuedPi, enqueueErr := c.sessions.ConsumeInterruptInjectAndEnqueue(ctx, sessionID, pi.ID, idempotencyKey, callDataJSON)
	if enqueueErr != nil {
		// Transaction rolled back, so row still exists for retry
		return false, fmt.Errorf("failed to enqueue interrupt inject: %w", enqueueErr)
	}
	if enqueuedPi == nil {
		// Row vanished between peek and enqueue — handled gracefully
		return false, nil
	}

	// Notify the message so web UI renders it live
	c.messages.Notify(injMsg)

	// InterruptAndReplace atomically records call and cancels only the
	// in-flight generation (design §4). Since we've already enqueued durably,
	// we just need to cancel the in-flight generation if there is one.
	if !c.currentAgent.InterruptAndReplace(sessionID, call) {
		// No owner — session is idle, the durable enqueue already handles it
		slog.Debug("coordinator: interrupt tick enqueued durable call for idle session",
			"session_id", sessionID, "idempotency_key", idempotencyKey)
	}
	return true, nil
}

// InterruptAndSend queues a user message and cancels the running turn.
// agent.Run()'s cancel-handling branch drains the queue and the queued
// message becomes the next Run() — with all assistant content produced so
// far preserved in the DB (the cancel path writes a FinishReasonCanceled
// to the in-flight assistant message before unwinding).
func (c *coordinator) InterruptAndSend(ctx context.Context, sessionID, prompt string, large, small *ModelOverride, attachments ...message.Attachment) error {
	if err := c.readyWg.Wait(); err != nil {
		return err
	}
	var pinned *resolvedOverrides
	if large != nil || small != nil {
		resolved, applyErr := c.applyModelOverrides(ctx, large, small)
		if applyErr != nil {
			return applyErr
		}
		pinned = resolved
	} else {
		// No explicit overrides: resolve from session DB or config defaults.
		// This ensures that an interrupt respects the session's persisted model
		// override (if any) rather than falling back to the shared/global model.
		resolved, resolveErr := c.resolveSessionModels(ctx, sessionID)
		if resolveErr != nil {
			return fmt.Errorf("failed to resolve session models for interrupt: %w", resolveErr)
		}
		pinned = resolved
	}
	call, err := c.buildCall(ctx, sessionID, prompt, pinned, attachments)
	if err != nil {
		return err
	}
	// InterruptAndReplace atomically records call as the replacement the
	// current owner runs next and cancels only the in-flight generation
	// (design §4) — replacing the QueueMessage+Cancel two-step that P0-2
	// made self-defeating (Cancel deterministically wiped what QueueMessage
	// just queued). When the session is idle there is nothing to interrupt,
	// and nobody is running who would ever drain a queued call — so we must
	// start the run ourselves (P0-B).
	if !c.currentAgent.InterruptAndReplace(sessionID, call) {
		if err := c.startDetachedRun(ctx, call); err != nil {
			return fmt.Errorf("failed to enqueue interrupt for idle session %s: %w", sessionID, err)
		}
	}
	return nil
}

// startDetachedRun durably enqueues call for the idle-session paths of
// InterruptAndSend (P0-B). Despite the name (kept
// for git-blame continuity with the pre-#340 version), it no longer runs
// call itself, in a goroutine or otherwise — see the task #340 paragraph
// below for what changed and why.
//
// Those paths used to call QueueMessage(call) when InterruptAndReplace
// reported no owner. That is a runnerless queue: with the session idle
// there is no turn loop left to drain it, so the call sat there until
// some unrelated future Run() happened to come along — while the caller
// (and, through it, the web client) had already been told the message was
// "queued". For a user pressing interrupt on a session that had just
// finished, or landing in the race right after a release, the message
// simply never ran. Durably enqueuing here is what the mailbox's own
// contract says the caller must do when it is handed "no owner" (see
// mailbox.interruptAndReplace's doc).
//
// Task #340, ROUND 3 migration: durably enqueues the call to the
// session_run_queue table (session.EnqueueRunQueueEntry) synchronously, in
// the CALLER's own goroutine and ctx — no longer spawns its own goroutine
// and no longer wraps ctx in context.WithoutCancel. The independent
// RunQueuePump is what actually executes the call later; this function's
// job ends once the durable-enqueue write has committed (or, on enqueue
// failure, once the pending_injects row has been recreated below). This
// eliminates data loss risks from the previous bounded-retry-then-log
// approach and ensures every accepted call gets a guaranteed runner (or an
// explicit terminal failure recorded in the queue), even across process
// restarts.
//
// For the interrupt inject path (InjectID non-empty), we still delete the
// pending_injects row at START to prevent duplicate detached runs if the
// pump picks up the same call before we return. If durable enqueue fails, we
// recreate the row so a future tick can retry (P0-2).
func (c *coordinator) startDetachedRun(ctx context.Context, call SessionAgentCall) error {
	// P0-2 fix: delete the pending_injects row at the START to prevent
	// duplicate detached runs. If durable enqueue fails, we'll recreate it.
	if call.InjectID != "" {
		slog.Debug("coordinator: detached run deleting pending_injects row at start",
			"inject_id", call.InjectID)
		if delErr := c.sessions.DeleteInterruptInject(ctx, call.InjectID); delErr != nil {
			slog.Error("coordinator: detached run failed to delete pending_injects row at start",
				"inject_id", call.InjectID, "err", delErr)
			// Continue anyway — row still exists, so duplicates may occur,
			// but this is better than data loss.
		} else {
			slog.Debug("coordinator: detached run deleted pending_injects row at start",
				"inject_id", call.InjectID)
		}
	}

	// P2-1: Generate idempotency key from LogicalCallID (stable per logical request)
	// instead of timestamp (which changes on every retry). Fallback to timestamp
	// with warning if LogicalCallID is empty (should not happen in normal flow).
	// For interrupt inject path, we can use the InjectID as part of the key.
	var idempotencyKey string
	if call.LogicalCallID != "" {
		idempotencyKey = fmt.Sprintf("%s-%s", call.SessionID, call.LogicalCallID)
	} else if call.InjectID != "" {
		idempotencyKey = fmt.Sprintf("%s-%s", call.SessionID, call.InjectID)
	} else {
		slog.Warn("coordinator: LogicalCallID is empty, falling back to timestamp-based idempotency key (non-idempotent retries)",
			"session_id", call.SessionID)
		idempotencyKey = fmt.Sprintf("%s-%d", call.SessionID, time.Now().UnixNano())
	}

	// Convert to SessionAgentCallData for serialization
	callData := ToSessionAgentCallData(call)
	callDataJSON, err := json.Marshal(callData)
	if err != nil {
		slog.Error("coordinator: failed to serialize call data for durable enqueue",
			"session_id", call.SessionID, "err", err)
		// For interrupt inject path, recreate the row to prevent data loss
		if call.InjectID != "" && call.ExistingMessageID != "" {
			if recreateErr := c.recreatePendingInjectRow(ctx, call); recreateErr != nil {
				slog.Error("coordinator: also failed to recreate pending_injects row during marshal recovery",
					"inject_id", call.InjectID, "recreate_err", recreateErr, "marshal_err", err)
			}
		}
		return fmt.Errorf("failed to serialize call data for durable enqueue: %w", err)
	}

	// Durably enqueue the call BEFORE returning control (P0-2 requirement)
	// This ensures the call will eventually be executed even if this goroutine exits
	if enqueueErr := c.sessions.EnqueueRunQueueEntry(ctx, idempotencyKey, call.SessionID, callDataJSON); enqueueErr != nil {
		slog.Error("coordinator: failed to durably enqueue call for recovery",
			"session_id", call.SessionID, "err", enqueueErr)
		// For interrupt inject path, recreate the row so a future tick can retry
		if call.InjectID != "" && call.ExistingMessageID != "" {
			if recreateErr := c.recreatePendingInjectRow(ctx, call); recreateErr != nil {
				slog.Error("coordinator: also failed to recreate pending_injects row during enqueue recovery",
					"inject_id", call.InjectID, "recreate_err", recreateErr, "enqueue_err", enqueueErr)
			}
		}
		// For non-inject interrupts, there is no fallback — the call is lost.
		// Return error so the caller can handle it (e.g., surface to HTTP response).
		// For inject interrupts, we've recreated the row so a future tick will retry.
		return fmt.Errorf("failed to durably enqueue call for recovery: %w", enqueueErr)
	}

	slog.Debug("coordinator: durably enqueued call for pump recovery",
		"session_id", call.SessionID, "idempotency_key", idempotencyKey, "inject_id", call.InjectID)
	return nil
}

// recreatePendingInjectRow recreates a pending_injects row for future retry
// (helper for startDetachedRun error path, P0-2 fix). Uses a bounded context
// (WithoutCancel + timeout) to ensure recovery writes have a chance even if
// the calling context is being canceled.
//
// Returns the error from recreation (or nil on success) so the caller can surface it.
func (c *coordinator) recreatePendingInjectRow(originalCtx context.Context, call SessionAgentCall) error {
	// Bounded context: disconnect from caller cancellation but enforce a timeout
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(originalCtx), 10*time.Second)
	defer cancel()

	slog.Debug("coordinator: attempting to recreate pending_injects row",
		"inject_id", call.InjectID, "session_id", call.SessionID, "message_id", call.ExistingMessageID)
	// Get the message content to recreate the row.
	msg, getErr := c.messages.Get(recoveryCtx, call.ExistingMessageID)
	if getErr != nil {
		slog.Error("coordinator: failed to recreate pending_injects row (could not get message)",
			"inject_id", call.InjectID, "message_id", call.ExistingMessageID, "err", getErr)
		return fmt.Errorf("could not get message for recreation: %w", getErr)
	}
	inject := session.PendingInject{
		ID:        uuid.New().String(), // Generate new ID to avoid UNIQUE constraint
		SessionID: call.SessionID,
		MessageID: call.ExistingMessageID,
		Content:   msg.FullText(),
		Interrupt: true,
	}
	createErr := c.sessions.CreatePendingInject(recoveryCtx, inject)
	if createErr != nil {
		slog.Error("coordinator: failed to recreate pending_injects row",
			"inject_id", call.InjectID, "err", createErr)
		return fmt.Errorf("failed to recreate pending_injects row: %w", createErr)
	}
	slog.Info("coordinator: successfully recreated pending_injects row for future retry",
		"new_inject_id", inject.ID, "old_inject_id", call.InjectID)
	return nil
}

// RebuildSessionAgentCall reconstructs a full SessionAgentCall from SessionAgentCallData
// for run queue pump execution (task #340, ROUND 3 migration).
//
// It reconstructs the live Model objects (with fantasy.LanguageModel and CatwalkCfg)
// from the serialized ModelCfg using the coordinator's provider configs and catwalk registry.
// ProviderOptions/Temperature/TopP/TopK/FrequencyPenalty/PresencePenalty are NOT
// reconstructed here — they are pure functions of (Model, ProviderConfig) computed
// via mergeCallOptions during normal execution path.
func (c *coordinator) RebuildSessionAgentCall(ctx context.Context, data session.SessionAgentCallData) (SessionAgentCall, error) {
	var largeModel, smallModel Model
	var err error

	// Determine which models to rebuild using a single atomic snapshot.
	cfg, _ := c.cfg.Snapshot()
	var largeCfg, smallCfg config.SelectedModel
	if data.LargeModel != nil {
		largeCfg = fromSessionModelCfg(*data.LargeModel)
	} else {
		// Use default config for large model
		largeCfg = cfg.Models[config.SelectedModelTypeLarge]
	}

	if data.SmallModel != nil {
		smallCfg = fromSessionModelCfg(*data.SmallModel)
	} else {
		// Use default config for small model
		smallCfg = cfg.Models[config.SelectedModelTypeSmall]
	}

	// Build both models (buildModelsFromCfg requires both)
	largeModel, smallModel, err = c.buildModelsFromCfg(ctx, cfg, largeCfg, smallCfg, false)
	if err != nil {
		return SessionAgentCall{}, fmt.Errorf("failed to rebuild models from config: %w", err)
	}

	// sessionAgent.Run reads ProviderOptions/Temperature/TopP/TopK/FrequencyPenalty/
	// PresencePenalty directly off the call (agent.go's fantasy.AgentStreamCall
	// construction) — it does NOT recompute them from LargeModel itself. Every
	// other call-site populates these via mergeCallOptions before the call ever
	// reaches Run, so we must do the same here or every durably-recovered call
	// silently loses its provider options and sampling knobs.
	largeProviderCfg, _ := c.cfg.Config().Providers.Get(largeModel.ModelCfg.Provider)
	providerOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(largeModel, largeProviderCfg)

	return SessionAgentCall{
		SessionID:            data.SessionID,
		LogicalCallID:        data.LogicalCallID, // P2-1 fix: restore stable ID
		Prompt:               data.Prompt,
		Attachments:          data.Attachments,
		ProviderOptions:      providerOptions,
		MaxOutputTokens:      data.MaxOutputTokens,
		Temperature:          temp,
		TopP:                 topP,
		TopK:                 topK,
		FrequencyPenalty:     freqPenalty,
		PresencePenalty:      presPenalty,
		NonInteractive:       data.NonInteractive,
		SystemPromptOverride: data.SystemPromptOverride,
		MaxCost:              data.MaxCost,
		MaxTokens:            data.MaxTokens,
		ExistingMessageID:    data.ExistingMessageID,
		InjectID:             data.InjectID,
		LargeModel:           &largeModel,
		SmallModel:           &smallModel,
		SystemPromptPrefix:   data.SystemPromptPrefix,
		SystemPrompt:         data.SystemPrompt,
		// Mark as originating from the durable queue so mailbox.submit can
		// skip mb.submitted for this call (P0-1: avoid double-execution).
		// See agent.SessionAgentCall.FromDurableQueue documentation.
		FromDurableQueue: true,
	}, nil
}

// RunSessionAgentCall executes a SessionAgentCall directly for run queue pump execution
// (task #340, ROUND 3 migration). This bypasses the normal buildCall path since the
// call is already fully reconstructed with all necessary data.
func (c *coordinator) RunSessionAgentCall(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}

	sessionID := call.SessionID

	// Interrupt-inject ticker: watches pending_injects for interrupt=true rows
	// written by `crush sessions inject --interrupt` in another process, and
	// (on the first hit) cancels the running turn and requeues the referenced
	// message so it picks up immediately. Bound to this turn's lifetime via
	// tickerCtx — stopped by the defer as soon as run() returns, so no
	// idle-process polling. The defer ensures the ticker goroutine has joined
	// before RunSessionAgentCall returns.
	tickerCtx, stopTicker := context.WithCancel(ctx)
	tickerDone := c.startInterruptTicker(tickerCtx, sessionID)
	// defers run LIFO: stopTicker (cancel tickerCtx) must fire before we
	// wait on tickerDone, or the join blocks forever waiting for a
	// goroutine that's still parked on <-ctx.Done().
	defer func() {
		stopTicker()
		<-tickerDone
	}()

	return c.currentAgent.Run(ctx, call)
}

// InjectMessage — see Coordinator interface.
func (c *coordinator) InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error) {
	if err := c.readyWg.Wait(); err != nil {
		return message.Message{}, err
	}

	// Resolve the session's model configuration before building the call.
	pinned, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return message.Message{}, fmt.Errorf("failed to resolve session models for inject: %w", err)
	}

	call, err := c.buildCall(ctx, sessionID, prompt, pinned, attachments)
	if err != nil {
		return message.Message{}, err
	}
	return c.currentAgent.InjectMessage(ctx, call)
}

// filterNonEmpty returns the subset of inputs that are non-empty after
// trimming surrounding whitespace. Used to join stdout/stderr cleanly.
func filterNonEmpty(parts ...string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// backgroundJobSummary formats a finished background command for injection
// into the owning session. Pure and deterministic so it can be unit-tested
// without a running shell.
func backgroundJobSummary(id, command string, stdout, stderr string, exitCode int, elapsed time.Duration) string {
	out := strings.TrimSpace(strings.Join(filterNonEmpty(stdout, stderr), "\n"))
	out = tools.TruncateOutput(out)
	if out == "" {
		out = "(no output)"
	}
	return fmt.Sprintf("Background job %s (`%s`) finished: exit %d, ran %s.\n\n%s",
		id, command, exitCode, elapsed.Round(time.Second), out)
}

// notifyBackgroundJobDone is invoked from a BackgroundShell.OnDone goroutine
// once a backgrounded bash command reaches a terminal state. It builds a
// concise summary and either (Phase 4, when autonomy is eligible) starts a
// fresh turn over it, or (Phase 3 fallback) pushes it into the owning session
// via InjectMessage. Detached: the OnDone goroutine outlives the turn that
// started it, so we never block or cancel the agent. Delivery failures (e.g.
// session closed) are logged at debug level.
func (c *coordinator) notifyBackgroundJobDone(sessionID string, sh *shell.BackgroundShell) {
	stdout, stderr, _, runErr := sh.GetOutput()
	summary := backgroundJobSummary(sh.ID, sh.Command, stdout, stderr, shell.ExitCode(runErr), sh.Elapsed())

	if c.autoResumeEligible(sessionID) {
		// Autonomous idle-resume: start (or, if busy, queue — single-flight via
		// sessionAgent.Run) a fresh turn over the completion summary. The bound
		// is incremented per completion (conservative: a coalesced queued
		// completion still counts toward the cap, which only makes runaway
		// protection stricter). Reset by any human message.
		c.bumpConsecutiveResume(sessionID)
		slog.Info("Phase 4: auto-resuming session on background job completion",
			"session_id", sessionID, "shell_id", sh.ID,
			"consecutive", c.consecutiveResume(sessionID))
		// Detached + cancelable: outlives the OnDone goroutine; the turn's
		// own watchdog/Cancel(sessionID) governs its lifetime, so NO short
		// timeout here (unlike the InjectMessage path — a turn can be long).
		// Tag the context so the persisted user message is marked
		// AutoResumed and rendered with a badge in the web UI. Also tag it
		// as a BackgroundJobNotice so the web shows the notice badge (an
		// auto-resume is also a job-completion notice).
		ctx := context.WithValue(context.Background(), autoResumedCtxKey{}, true)
		ctx = context.WithValue(ctx, backgroundJobNoticeCtxKey{}, true)
		go runAutoResumeRecovered(ctx, sessionID, sh.ID, func(ctx context.Context) (*fantasy.AgentResult, error) {
			return c.Run(ctx, sessionID, summary)
		})
		return
	}

	// Phase 3 behavior (unchanged): persist + (if busy) merge into the running
	// turn; if idle, just persisted + web-visible, no auto-turn. Tag the context
	// so the injected user message is flagged as a BackgroundJobNotice.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	ctx = context.WithValue(ctx, backgroundJobNoticeCtxKey{}, true)
	defer cancel()
	if _, err := c.InjectMessage(ctx, sessionID, summary); err != nil {
		slog.Debug("background job completion not delivered (session likely closed)",
			"session_id", sessionID,
			"shell_id", sh.ID,
			"err", err)
	}
}

// runAutoResumeRecovered runs runFn (normally a closure over c.Run for the
// Phase 4 auto-resume turn) with panic isolation, on the calling goroutine.
// Callers spawn this in its own goroutine (see notifyBackgroundJobDone)
// because it is independent of the BackgroundShell.OnDone goroutine that
// triggers it — OnDone's own recover() does not cover a panic raised in
// here, since by the time this runs it is a sibling goroutine, not a child
// call of OnDone's callback.
//
// runFn re-enters the full synchronous tool-dispatch chain (same call shape
// as app.go's RunNonInteractive goroutine, see runAgentTurnRecovered there),
// so any tool call made during this auto-resumed turn could panic exactly
// like it could during a human-initiated turn. Without this recover, such a
// panic would crash the whole crush process with no log output, at an
// arbitrary time long after the triggering background job completed.
func runAutoResumeRecovered(ctx context.Context, sessionID, shellID string, runFn func(ctx context.Context) (*fantasy.AgentResult, error)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Phase 4 auto-resume run panic",
				"session_id", sessionID, "shell_id", shellID,
				"panic", r, "stack", string(debug.Stack()))
		}
	}()

	if _, err := runFn(ctx); err != nil {
		slog.Debug("Phase 4 auto-resume run failed (session likely closed)",
			"session_id", sessionID, "shell_id", shellID, "err", err)
	}
}

func (c *coordinator) IsBusy() bool {
	return c.currentAgent.IsBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	return c.currentAgent.IsSessionBusy(sessionID)
}

// Model returns the globally configured large model from config.
// This is used for display/status queries and does NOT reflect any per-session
// model overrides. After the per-session model isolation fix, callers that need
// the actual model for a specific session should use resolveSessionModels instead.
func (c *coordinator) Model() Model {
	// Build the default large model from config without caching (this is
	// called infrequently, mostly for status display).
	cfg, _ := c.cfg.Snapshot()
	largeCfg := cfg.Models[config.SelectedModelTypeLarge]
	smallCfg := cfg.Models[config.SelectedModelTypeSmall]

	// Create a temporary context for model building.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	largeModel, _, err := c.buildModelsFromCfg(ctx, cfg, largeCfg, smallCfg, false)
	if err != nil {
		// Return a zero-value model rather than panicking on status queries.
		slog.Error("coordinator.Model: failed to build default large model", "err", err)
		return Model{}
	}
	return largeModel
}

func (c *coordinator) GetSystemPrompt() string {
	return c.currentAgent.SystemPrompt()
}

func (c *coordinator) BuildSystemPrompt(ctx context.Context) (string, error) {
	if c.prompt == nil {
		return "", nil
	}

	// Build the default large model from config for prompt building.
	cfg, _ := c.cfg.Snapshot()
	largeCfg := cfg.Models[config.SelectedModelTypeLarge]
	smallCfg := cfg.Models[config.SelectedModelTypeSmall]

	largeModel, _, err := c.buildModelsFromCfg(ctx, cfg, largeCfg, smallCfg, false)
	if err != nil {
		return "", fmt.Errorf("failed to build default large model: %w", err)
	}

	// Use the same pinned cfg captured above (task #341, P1-1) instead of
	// re-reading c.cfg.Config() live inside workerSubAgentActive.
	return c.prompt.Build(ctx, largeModel.ModelCfg.Provider, largeModel.ModelCfg.Model, c.cfg, c.workerSubAgentActive(cfg))
}

// BuildSystemPromptForSession builds a system prompt for a specific session,
// using the session's model configuration (from DB overrides or config defaults).
// This ensures the system prompt matches the model that will actually run for this session.
func (c *coordinator) BuildSystemPromptForSession(ctx context.Context, sessionID string) (string, error) {
	if c.prompt == nil {
		return "", nil
	}

	// Resolve the session's model configuration (DB overrides → config defaults).
	resolved, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve session models: %w", err)
	}

	// Reuse the system prompt resolveSessionModels already built from its own
	// single pinned config snapshot (task #341, P1-1), rather than rebuilding
	// it here from a second, separately-timed live cfg read
	// (c.workerSubAgentActive() with no argument). A reload landing between
	// the two builds could otherwise make this second build's
	// WorkerAvailable flag disagree with resolved.large, which was pinned
	// from an earlier generation.
	return resolved.systemPrompt, nil
}

func (c *coordinator) UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	return c.sessions.UpdateSystemPrompt(ctx, sessionID, prompt)
}

// SetAgentTimeoutOptions delegates to the current agent's SetTimeoutOptions.
// Fork patch: batch 8.
func (c *coordinator) SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
	c.currentAgent.SetTimeoutOptions(extendsOnProgress, hardCap)
}

func (c *coordinator) UpdateModels(ctx context.Context) error {
	c.clearModelCache()

	// build the models again so we make sure we get the latest config
	large, small, err := c.buildAgentModels(ctx, false)
	if err != nil {
		return err
	}
	c.currentAgent.SetModels(large, small)

	// Update prompt prefix for the new large model provider
	if largeProviderCfg, ok := c.cfg.Config().Providers.Get(large.ModelCfg.Provider); ok {
		c.currentAgent.SetSystemPromptPrefix(largeProviderCfg.SystemPromptPrefix)
	}

	agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return errCoderAgentNotConfigured
	}

	tools, err := c.buildTools(ctx, agentCfg, false)
	if err != nil {
		return err
	}
	c.currentAgent.SetTools(tools)
	return nil
}

// clearModelCache empties the model cache, forcing the next resolveSessionModels
// call to rebuild models from the current config. This is called after credential
// updates (OAuth token refresh, API key re-resolution) to ensure cached models
// with stale clients are not reused (task #341, P1-3).
func (c *coordinator) clearModelCache() {
	if c.modelCache != nil {
		c.modelCache.Reset(make(map[string]cachedModelPair))
	}
}

func (c *coordinator) QueuedPrompts(sessionID string) int {
	return c.currentAgent.QueuedPrompts(sessionID)
}

func (c *coordinator) QueuedPromptsList(sessionID string) []string {
	return c.currentAgent.QueuedPromptsList(sessionID)
}

// Summarize implements Coordinator.
func (c *coordinator) Summarize(ctx context.Context, sessionID string, snapshot *SummarizeSnapshot) error {
	// If the caller didn't provide a snapshot (e.g., tests), resolve one now.
	// This maintains backward compatibility while encouraging the correct pattern.
	if snapshot == nil {
		var err error
		snapshot, err = c.buildSummarizeSnapshot(ctx, sessionID)
		if err != nil {
			return err
		}
	}

	// Refresh OAuth token if needed using the provider from the snapshot.
	// Auth identity is already captured in the snapshot via providerCfg,
	// which is read once during snapshot construction, so no additional
	// pinning is needed here (task #341, P1-1).
	providerCfg, ok := c.cfg.Config().Providers.Get(snapshot.model.ModelCfg.Provider)
	if !ok {
		return errModelProviderNotConfigured
	}
	if err := checkPeakHours(providerCfg); err != nil {
		return err
	}

	if err := c.refreshTokenIfExpired(ctx, providerCfg); err != nil {
		slog.Error("Failed to refresh OAuth2 token before summarize. Proceeding with existing token.", "error", err)
	}

	summarize := func() error {
		return c.currentAgent.Summarize(ctx, sessionID, snapshot)
	}

	// Summarize doesn't need a rebuild callback since it uses a pre-built
	// snapshot that doesn't capture a provider client.
	return c.runWithUnauthorizedRetry(ctx, providerCfg, summarize, nil)
}

// buildSummarizeSnapshot creates an immutable snapshot for a summarize operation,
// resolving the model from the target session's persisted models (or config defaults
// for sessions without overrides). This ensures the entire summarize operation
// uses consistent configuration regardless of concurrent session model changes.
func (c *coordinator) buildSummarizeSnapshot(ctx context.Context, sessionID string) (*SummarizeSnapshot, error) {
	// Resolve the session's model configuration from the DB or config defaults.
	// This always returns a valid snapshot (never nil).
	resolved, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve session models for summarize: %w", err)
	}

	// Get the provider config for this model.
	providerCfg, ok := c.cfg.Config().Providers.Get(resolved.large.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}

	// Build provider options from the resolved model.
	opts := getProviderOptions(resolved.large, providerCfg)

	// Use the prompt prefix from the resolved snapshot (provider config's
	// prefix, already set by resolveSessionModels).
	promptPrefix := resolved.promptPrefix
	if promptPrefix == "" {
		promptPrefix = providerCfg.SystemPromptPrefix
	}

	return &SummarizeSnapshot{
		model:           resolved.large,
		providerOptions: opts,
		promptPrefix:    promptPrefix,
	}, nil
}

// refreshTokenIfExpired proactively refreshes the OAuth token if it has expired.
func (c *coordinator) refreshTokenIfExpired(ctx context.Context, providerCfg config.ProviderConfig) error {
	if providerCfg.OAuthToken == nil || !providerCfg.OAuthToken.IsExpired() {
		return nil
	}
	slog.Debug("Token needs to be refreshed", "provider", providerCfg.ID)
	return c.refreshOAuth2Token(ctx, providerCfg)
}

// checkLivePeakHours returns the current peak-hours decision for providerID.
// It reloads the config first when a tracked config file changed on disk, so
// long-running agents in one process can observe peak_hours edits made by the
// web UI or CLI in another process.
func (c *coordinator) checkLivePeakHours(providerID string) error {
	if c == nil || c.cfg == nil || providerID == "" {
		return nil
	}
	if staleness := c.cfg.ConfigStaleness(); staleness.Dirty {
		reloadCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.cfg.ReloadFromDisk(reloadCtx); err != nil {
			slog.Warn("Failed to reload config before peak-hours check", "provider", providerID, "err", err)
		}
		cancel()
	}
	cfg := c.cfg.Config()
	if cfg == nil || cfg.Providers == nil {
		return nil
	}
	pc, ok := cfg.Providers.Get(providerID)
	if !ok {
		return nil
	}
	return checkPeakHours(pc)
}

// checkPeakHours refuses the request if providerCfg is currently inside its
// configured peak_hours window. Returns nil (allow) when the window is absent
// or not currently active. The returned error wraps errProviderPeakHours so
// callers and classifyProviderError can identify it via errors.Is.
func checkPeakHours(providerCfg config.ProviderConfig) error {
	w := providerCfg.PeakHours
	if w == nil {
		return nil
	}
	now := time.Now()
	if !w.InPeakHours(now) {
		return nil
	}
	end := w.EndTimeToday(now)
	slog.Warn(
		"Refusing request: provider is inside its peak-hours window",
		"provider", providerCfg.ID,
		"window_start", w.Start,
		"window_end", w.End,
		"available_again", end.Format("15:04"),
		"in", time.Until(end).Round(time.Minute).String(),
	)
	return &PeakHoursError{
		ProviderID: providerCfg.ID,
		Start:      w.Start,
		End:        w.End,
		ReopensAt:  end,
	}
}

// runWithUnauthorizedRetry executes fn. If fn returns a 401 error, it
// attempts to refresh credentials and rebuilds the call before retrying.
// Returns the final error: from the retry if a retry was attempted, otherwise
// from the original run. Callers that need to notify the user on persistent
// failure should check isUnauthorized on the returned error.
//
// After credential refresh, rebuildCall is invoked to reconstruct the call
// with fresh credentials, ensuring the retry uses a new provider client
// rather than the stale pinned client from the original attempt (task #341,
// P1-2).
func (c *coordinator) runWithUnauthorizedRetry(ctx context.Context, providerCfg config.ProviderConfig, fn func() error, rebuildCall func() error) error {
	err := fn()
	if err != nil && c.isUnauthorized(err) {
		if retryErr := c.retryAfterUnauthorized(ctx, providerCfg); retryErr == nil {
			// After credential refresh, rebuild the call with fresh models
			// to use the new provider client (task #341, P1-2). rebuildCall
			// is nil for callers that don't pin a call to a specific model
			// snapshot (e.g. summarize, sub-agent delegation) — fn() itself
			// re-resolves what it needs on each invocation for those paths,
			// so there is nothing to rebuild.
			if rebuildCall != nil {
				if rebuildErr := rebuildCall(); rebuildErr != nil {
					return rebuildErr
				}
			}
			return fn()
		}
	}
	return err
}

// retryAfterUnauthorized attempts to refresh credentials after receiving a 401
// and returns nil if retry should be attempted. This calls UpdateModels which
// clears the model cache and rebuilds the shared agent with fresh credentials
// (task #341, P1-3).
func (c *coordinator) retryAfterUnauthorized(ctx context.Context, providerCfg config.ProviderConfig) error {
	switch {
	case providerCfg.OAuthToken != nil:
		slog.Debug("Received 401. Refreshing token and retrying", "provider", providerCfg.ID)
		return c.refreshOAuth2Token(ctx, providerCfg)
	case strings.Contains(providerCfg.APIKeyTemplate, "$"):
		slog.Debug("Received 401. Refreshing API Key template and retrying", "provider", providerCfg.ID)
		return c.refreshApiKeyTemplate(ctx, providerCfg)
	default:
		return nil
	}
}

func (c *coordinator) SummarizeQueued(sessionID string) bool {
	return c.currentAgent.SummarizeQueued(sessionID)
}

func (c *coordinator) TakeSummarizeQueue(sessionID string) (*SummarizeSnapshot, bool) {
	return c.currentAgent.TakeSummarizeQueue(sessionID)
}

func (c *coordinator) CancelQueuedSummarize(sessionID string) {
	c.currentAgent.CancelQueuedSummarize(sessionID)
}

func (c *coordinator) isUnauthorized(err error) bool {
	var providerErr *fantasy.ProviderError
	return errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized
}

func (c *coordinator) refreshOAuth2Token(ctx context.Context, providerCfg config.ProviderConfig) error {
	if err := c.cfg.RefreshOAuthToken(ctx, config.ScopeGlobal, providerCfg.ID); err != nil {
		slog.Error("Failed to refresh OAuth token after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}
	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

func (c *coordinator) refreshApiKeyTemplate(ctx context.Context, providerCfg config.ProviderConfig) error {
	newAPIKey, err := c.cfg.Resolve(providerCfg.APIKeyTemplate)
	if err != nil {
		slog.Error("Failed to re-resolve API key after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}

	providerCfg.APIKey = newAPIKey
	c.cfg.SetProviderRuntimeConfig(providerCfg.ID, providerCfg)

	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

// subAgentParams holds the parameters for running a sub-agent.
type subAgentParams struct {
	Agent          SessionAgent
	SessionID      string
	AgentMessageID string
	ToolCallID     string
	Prompt         string
	SessionTitle   string
	// SessionSetup is an optional callback invoked after session creation
	// but before agent execution, for custom session configuration.
	SessionSetup func(sessionID string)
	// ResumeSessionID, when non-empty, continues an existing sub-agent
	// session instead of creating a new one — see AgentParams.ResumeSessionID
	// for the model-facing contract. Must name a session whose
	// ParentSessionID equals SessionID; runSubAgent verifies this before
	// touching it (see the ownership check in runSubAgent).
	ResumeSessionID string
}

// runSubAgent runs a sub-agent and handles session management and cost accumulation.
// It creates a sub-session (or resumes an existing one, see ResumeSessionID),
// runs the agent with the given prompt, and propagates the cost to the parent
// session.
func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	var session session.Session
	if params.ResumeSessionID != "" {
		// Resume path (Gap 2 / phase 3.2): reuse the existing sub-session so
		// the sub-agent's prior context (files read, work already done) is
		// preserved instead of thrown away. SECURITY: verify the session
		// being resumed is genuinely a child of the CURRENT parent session
		// before touching it — otherwise a model could pass an arbitrary
		// session id (someone else's session, or an unrelated top-level
		// session) and read/append to it. A bad id here is a model mistake,
		// not a crash: return a normal tool error (nil Go error) so the
		// caller can retry rather than aborting the parent's turn.
		existing, err := c.sessions.Get(ctx, params.ResumeSessionID)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf(
				"resume_session_id %q not found: %s", params.ResumeSessionID, err,
			)), nil
		}
		if existing.ParentSessionID != params.SessionID {
			return fantasy.NewTextErrorResponse(fmt.Sprintf(
				"resume_session_id %q is not a child of the current session; refusing to resume a session that does not belong to this agent call",
				params.ResumeSessionID,
			)), nil
		}
		session = existing
	} else {
		// Create sub-session
		agentToolSessionID := c.sessions.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
		created, err := c.sessions.CreateTaskSession(ctx, agentToolSessionID, params.SessionID, params.SessionTitle)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("create session: %w", err)
		}
		session = created
	}

	// Propagate the PARENT session's auto-approve status to this child.
	//
	// A sub-agent runs under its own child session id
	// (parentMessageID$$toolCallID), which is a completely different key
	// from the parent's. A non-interactive `crush run` auto-approves ONLY
	// the root session id it was handed (app.go's
	// Permissions.AutoApproveSession(sess.ID)), so before this call every
	// delegated sub-agent was unapproved: its first non-safe tool call
	// (anything outside tools.safeCommands — e.g. a bare `wc -l`) reached
	// permission.Request's final select, which blocks on a UI response
	// that does not exist in that mode, and hung until the 45-minute tool
	// watchdog cap. Observed in the wild as "the agent spawns a sub-agent
	// and then nothing happens at all" — the parent sat in `delegating`
	// with a healthy heartbeat while the child was stuck on a trivial
	// command.
	//
	// Inheritance (not a blanket auto-approve) is deliberate: an
	// INTERACTIVE parent's sub-agent must still go through the normal
	// prompt path, and the restricted-run allowlist gate still applies on
	// top, since it is consulted inside the auto-approve branch itself.
	// Applied BEFORE SessionSetup so an explicit setup callback can still
	// override (agentic_fetch_tool.go auto-approves unconditionally).
	if c.permissions != nil {
		c.permissions.InheritSessionAutoApprove(params.SessionID, session.ID)
	}

	// Call session setup function if provided
	if params.SessionSetup != nil {
		params.SessionSetup(session.ID)
	}

	// Get model configuration
	model := params.Agent.Model()
	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return fantasy.ToolResponse{}, errModelProviderNotConfigured
	}
	if err := checkPeakHours(providerCfg); err != nil {
		return fantasy.ToolResponse{}, err
	}

	// Run the agent
	run := func() (*fantasy.AgentResult, error) {
		return params.Agent.Run(ctx, SessionAgentCall{
			SessionID:        session.ID,
			Prompt:           params.Prompt,
			MaxOutputTokens:  maxTokens,
			ProviderOptions:  getProviderOptions(model, providerCfg),
			Temperature:      model.ModelCfg.Temperature,
			TopP:             model.ModelCfg.TopP,
			TopK:             model.ModelCfg.TopK,
			FrequencyPenalty: model.ModelCfg.FrequencyPenalty,
			PresencePenalty:  model.ModelCfg.PresencePenalty,
			NonInteractive:   true,
		})
	}
	var result *fantasy.AgentResult
	err := c.runWithUnauthorizedRetry(ctx, providerCfg, func() error {
		var runErr error
		result, runErr = run()
		return runErr
	}, nil)
	// Notify only if still unauthorized after retry.
	if err != nil && c.isUnauthorized(err) && c.notify != nil && model.ModelCfg.Provider == hyper.Name {
		c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: model.ModelCfg.Provider,
		})
	}
	// Charge the parent for whatever the child spent on this run, on EVERY
	// outcome (success, ask_question pause, or genuine error). This replaces
	// the old in-memory baselineCost scheme: TransferChildCostToParent reads
	// the child's persisted parent_cost_accounted ledger inside one
	// transaction and charges only the delta since the last transfer, so the
	// spent amount is never lost. A failed charge only logs a warning — the
	// uncharged delta persists in (child.cost - child.parent_cost_accounted)
	// and is recovered on the next successful call. The previous code skipped
	// the charge on the generic-error path, permanently losing that cost.
	//
	// Detached timeout, same pattern as agent.go's error-path flush (see
	// agent.go's flushCtx uses): ctx may already be cancelled here — by the
	// parent's stream watchdog or a user Ctrl-C — since this runs after the
	// child's own Run() returns. Charging the parent is a single short DB
	// transaction, not tied to the run that just ended, so it must survive
	// that cancellation rather than fail immediately with "begin
	// transaction: context canceled" and silently drop the child's spend
	// (recovery only happens if this same child is ever charged again,
	// which is not guaranteed for a one-shot sub-agent).
	costCtx, costCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	costErr := c.updateParentSessionCost(costCtx, session.ID, params.SessionID)
	costCancel()
	if costErr != nil {
		slog.Warn(
			"Failed to update parent session cost",
			"child_session", session.ID,
			"parent_session", params.SessionID,
			"error", costErr,
		)
	}

	// A sub-agent that called ask_question stops its turn via
	// AwaitingAnswerError (see question_stop.go), not a genuine failure —
	// letting it fall into the generic branch below would tell the parent
	// model "the sub-agent FAILED", and it would likely redo the work itself
	// instead of answering. Recognize this case and return a normal
	// SUCCESSFUL tool response shaped as a question. Cost was already charged
	// above regardless of outcome.
	var awaitingAnswer *AwaitingAnswerError
	if errors.As(err, &awaitingAnswer) {
		return fantasy.NewTextResponse(subAgentQuestionText(session.ID, awaitingAnswer)), nil
	}
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to generate response: %s", err)), nil
	}

	output := subAgentOutput(result)
	if output == "" {
		return fantasy.NewTextErrorResponse("Sub-agent completed but produced no text output."), nil
	}
	return fantasy.NewTextResponse(output), nil
}

func subAgentOutput(result *fantasy.AgentResult) string {
	if result == nil {
		return ""
	}
	return result.Response.Content.Text()
}

// updateParentSessionCost transfers the cost a child session accrued since
// the last transfer to its parent. It is a thin delegate to the
// transactional session.Service.TransferChildCostToParent, which reads the
// child's persisted parent_cost_accounted ledger, charges only the delta
// (cost - accounted, clamped >= 0) to the parent via an atomic additive
// UPDATE, and advances the child's accounted marker — all inside one DB
// transaction.
//
// Because the baseline now lives in the database (parent_cost_accounted),
// there is no in-memory baselineCost parameter: the function is safe across
// process restarts, concurrent resumes of the same child, and failed
// charges (an uncharged delta persists and is recovered on the next call).
// Callers should invoke it on EVERY sub-agent outcome (success,
// ask_question, error) so no spent cost is ever silently dropped.
func (c *coordinator) updateParentSessionCost(ctx context.Context, childSessionID, parentSessionID string) error {
	return c.sessions.TransferChildCostToParent(ctx, childSessionID, parentSessionID)
}

// discoverSkills runs skill discovery for this coordinator at session
// start. Fork note: upstream threads a pre-built skills.Manager through
// from app.New; we rejected that abstraction (see CHANGELOG.fork.md
// Section 2) and keep the simple inline discovery here.
func discoverSkills(cfg *config.ConfigStore) (allSkills, activeSkills []*skills.Skill) {
	builtin, builtinStates := skills.DiscoverBuiltinWithStates()
	discovered := append([]*skills.Skill(nil), builtin...)

	var userStates []*skills.SkillState
	var userPaths []string

	opts := cfg.Config().Options
	if opts != nil && len(opts.SkillsPaths) > 0 {
		userPaths = make([]string, 0, len(opts.SkillsPaths))
		for _, pth := range opts.SkillsPaths {
			expanded := home.Long(pth)
			if strings.HasPrefix(expanded, "$") {
				if resolved, err := cfg.Resolver().ResolveValue(expanded); err == nil {
					expanded = resolved
				}
			}
			userPaths = append(userPaths, expanded)
		}
		var userSkills []*skills.Skill
		userSkills, userStates = skills.DiscoverWithStates(userPaths)
		discovered = append(discovered, userSkills...)
	}

	allSkills = skills.Deduplicate(discovered)
	var disabledSkills []string
	if opts != nil {
		disabledSkills = opts.DisabledSkills
	}
	activeSkills = skills.Filter(allSkills, disabledSkills)

	allStates := append([]*skills.SkillState(nil), builtinStates...)
	allStates = append(allStates, userStates...)

	allStates = skills.DeduplicateStates(allStates)

	slices.SortStableFunc(allStates, func(a, b *skills.SkillState) int {
		return strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path))
	})
	skills.SetLatestStates(allStates)
	skills.PublishStates(allStates)

	logDiscoveryStats(builtin, builtinStates, userStates, userPaths, allSkills, activeSkills, disabledSkills)
	return allSkills, activeSkills
}

// logTurnSkillUsage emits a per-turn diagnostic line showing which skills
// (if any) were loaded during this turn and which looked relevant based on
// a cheap keyword match against the user prompt. The goal is to surface
// "should-have-loaded but didn't" situations for later analysis.
//
// Logged at Info level under component=skills; heavy fields are elided when
// there is nothing interesting to report.
func logTurnSkillUsage(
	sessionID string,
	prompt string,
	activeSkills []*skills.Skill,
	tracker *skills.Tracker,
	before []string,
) {
	if tracker == nil || len(activeSkills) == 0 {
		return
	}

	after := tracker.LoadedNames()

	beforeSet := make(map[string]bool, len(before))
	for _, n := range before {
		beforeSet[n] = true
	}
	var loadedThisTurn []string
	for _, n := range after {
		if !beforeSet[n] {
			loadedThisTurn = append(loadedThisTurn, n)
		}
	}

	slog.Info(
		"Skill turn summary",
		"component", "skills",
		"session_id", sessionID,
		"prompt_len", len(prompt),
		"active_total", len(activeSkills),
		"loaded_total", len(after),
		"loaded_this_turn", loadedThisTurn,
	)
}

// logDiscoveryStats emits a single structured log line summarising skill
// discovery for the current session. It is intentionally low-volume: one
// line per session start.
func logDiscoveryStats(
	builtin []*skills.Skill,
	builtinStates, userStates []*skills.SkillState,
	userPaths []string,
	allSkills, activeSkills []*skills.Skill,
	disabled []string,
) {
	countErrors := func(states []*skills.SkillState) int {
		n := 0
		for _, s := range states {
			if s.State == skills.StateError {
				n++
			}
		}
		return n
	}

	userOK := 0
	for _, s := range userStates {
		if s.State == skills.StateNormal {
			userOK++
		}
	}

	activeNames := make([]string, 0, len(activeSkills))
	for _, s := range activeSkills {
		activeNames = append(activeNames, s.Name)
	}

	xml := skills.ToPromptXML(activeSkills)

	slog.Info(
		"Skill discovery complete",
		"component", "skills",
		"builtin_ok", len(builtin),
		"builtin_errors", countErrors(builtinStates),
		"user_ok", userOK,
		"user_errors", countErrors(userStates),
		"user_paths", len(userPaths),
		"deduped_total", len(allSkills),
		"active", len(activeSkills),
		"disabled", len(disabled),
		"prompt_bytes", len(xml),
		"prompt_tok_est", skills.ApproxTokenCount(xml),
		"active_names", activeNames,
	)
}
