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
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	mcp "github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
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
	CancelAll()
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
	Summarize(context.Context, string) error
	SummarizeQueued(sessionID string) bool
	TakeSummarizeQueue(sessionID string) (fantasy.ProviderOptions, bool)
	CancelQueuedSummarize(sessionID string)
	Model() Model
	UpdateModels(ctx context.Context) error
	GetSystemPrompt() string
	BuildSystemPrompt(ctx context.Context) (string, error)
	UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error
	// SetAgentTimeoutOptions configures the stream watchdog's deadline
	// extension on the current agent. Called from RunNonInteractive when
	// --timeout-extends-on-progress is set. Fork patch: batch 8.
	SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration)
	// SetRunLimits sets cost and token caps for the next Run call.
	// Fork patch: batch 30.
	SetRunLimits(maxCost float64, maxTokens int64)
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

	// Phase 4 autonomous idle-resume guardrails.
	persistentMode         bool           // true only for the long-lived web server; false for crush run.
	autoResumeMu           sync.Mutex     // guards consecutiveAutoResumes.
	consecutiveAutoResumes map[string]int // sessionID -> consecutive auto-resumes since last human message.
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
	}

	agentCfg, ok := cfg.Config().Agents[config.AgentCoder]
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

	// Check if this session has specific models assigned in the DB.
	// If so, apply overrides before running (without going through RunWithOverrides
	// to avoid mutual recursion).
	sess, err := c.sessions.Get(ctx, sessionID)
	if err == nil && (sess.LargeModelID != "" || sess.SmallModelID != "") {
		var large, small *ModelOverride
		if sess.LargeModelID != "" {
			large = &ModelOverride{Provider: sess.LargeModelProvider, Model: sess.LargeModelID, ReasoningEffort: sess.LargeModelReasoningEffort}
		}
		if sess.SmallModelID != "" {
			small = &ModelOverride{Provider: sess.SmallModelProvider, Model: sess.SmallModelID, ReasoningEffort: sess.SmallModelReasoningEffort}
		}
		if applyErr := c.applyModelOverrides(ctx, large, small); applyErr != nil {
			slog.Error("Coordinator.Run: failed to apply DB model overrides, using current models", "err", applyErr)
		}
	}

	return c.runInternal(ctx, sessionID, prompt, attachments...)
}

// SetRunLimits stores cost and token caps for the next Run call.
// Fork patch: batch 30.
func (c *coordinator) SetRunLimits(maxCost float64, maxTokens int64) {
	c.runLimitsMu.Lock()
	c.maxCost = maxCost
	c.maxTokens = maxTokens
	c.runLimitsMu.Unlock()
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
	c.persistentMode = persistent
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
		c.persistentMode &&
		c.consecutiveResume(sessionID) < maxConsecutiveAutoResumes
}

// applyModelOverrides sets up the agent with the given model overrides (modifies currentAgent in place).
func (c *coordinator) applyModelOverrides(ctx context.Context, large, small *ModelOverride) error {
	largeCfg := c.cfg.Config().Models[config.SelectedModelTypeLarge]
	smallCfg := c.cfg.Config().Models[config.SelectedModelTypeSmall]

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

	largeModel, smallModel, err := c.buildModelsFromCfg(ctx, largeCfg, smallCfg, false)
	if err != nil {
		return fmt.Errorf("failed to build override models: %w", err)
	}

	c.currentAgent.SetModels(largeModel, smallModel)

	if largeProviderCfg, ok := c.cfg.Config().Providers.Get(largeModel.ModelCfg.Provider); ok {
		c.currentAgent.SetSystemPromptPrefix(largeProviderCfg.SystemPromptPrefix)
	}
	if c.prompt != nil {
		newSystemPrompt, err := c.prompt.Build(ctx, largeModel.ModelCfg.Provider, largeModel.ModelCfg.Model, c.cfg)
		if err != nil {
			slog.Error("applyModelOverrides: failed to rebuild system prompt", "err", err)
		} else {
			c.currentAgent.SetSystemPrompt(newSystemPrompt)
		}
	}
	return nil
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
	built, buildErr := c.prompt.Build(ctx, c.currentAgent.Model().ModelCfg.Provider, c.currentAgent.Model().ModelCfg.Model, c.cfg)
	if buildErr != nil || built == "" {
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
func (c *coordinator) buildCall(ctx context.Context, sessionID, prompt string, attachments []message.Attachment) (SessionAgentCall, error) {
	model := c.currentAgent.Model()

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

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return SessionAgentCall{}, errModelProviderNotConfigured
	}
	if err := checkPeakHours(providerCfg); err != nil {
		return SessionAgentCall{}, err
	}

	mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(model, providerCfg)
	sessionSystemPrompt := c.resolveSessionSystemPrompt(ctx, sessionID)

	return SessionAgentCall{
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
	}, nil
}

// runInternal executes the agent with whatever models are currently set, handling 401 retries.
func (c *coordinator) runInternal(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	model := c.currentAgent.Model()
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

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
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

	run := func() (*fantasy.AgentResult, error) {
		return c.currentAgent.Run(ctx, SessionAgentCall{
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
		})
	}
	// Interrupt-inject ticker: watches pending_injects for interrupt=true rows
	// written by `crush sessions inject --interrupt` in another process, and
	// (on the first hit) cancels the running turn and requeues the referenced
	// message so it picks up immediately. Bound to this turn's lifetime via
	// tickerCtx — stopped by the defer as soon as run() returns, so no
	// idle-process polling. Runs for BOTH the initial turn and every retry
	// re-run below (each run() sees a fresh ticker via this closure).
	tickerCtx, stopTicker := context.WithCancel(ctx)
	defer stopTicker()
	c.startInterruptTicker(tickerCtx, sessionID)

	beforeLoaded := c.skillTracker.LoadedNames()
	var result *fantasy.AgentResult
	originalErr := c.runWithUnauthorizedRetry(ctx, providerCfg, func() error {
		var err error
		result, err = run()
		return err
	})
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

	if err := c.applyModelOverrides(ctx, large, small); err != nil {
		return nil, err
	}

	return c.runInternal(ctx, sessionID, prompt, attachments...)
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
		case string(catwalk.InferenceProviderZAI), string(catwalk.InferenceProviderDeepSeek):
			if model.ModelCfg.Think || model.ModelCfg.ReasoningEffort != "" {
				extraBody["thinking"] = map[string]any{
					"type": "enabled",
				}
			} else {
				extraBody["thinking"] = map[string]any{
					"type": "disabled",
				}
			}
			// GLM-5.x exposes two thinking-effort levels (high / max).
			// We forward via `extra_body.reasoning_effort` — the
			// OpenAI-compat field z.ai already accepts on its Anthropic-
			// compatible coding endpoint. Mapping mirrors z.ai's own
			// "Claude Code selected effort → GLM-5.2 actual mapped effort"
			// table from docs.z.ai/devpack/latest-model:
			//   low, medium, high (default) → high
			//   xhigh, max, ultracode       → max
			// Older GLM-4.x ignore the field harmlessly.
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
		systemPrompt, err := prompt.Build(ctx, large.Model.Provider(), large.Model.Model(), c.cfg)
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

func (c *coordinator) buildTools(ctx context.Context, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	var allTools []fantasy.AgentTool
	if slices.Contains(agent.AllowedTools, AgentToolName) {
		agentTool, err := c.agentTool(ctx)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, agentTool)
	}

	if slices.Contains(agent.AllowedTools, tools.AgenticFetchToolName) {
		agenticFetchTool, err := c.agenticFetchTool(ctx, nil)
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
		tools.NewBashTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Options.Attribution, modelID, onBgDone),
		tools.NewCrushInfoTool(c.cfg, c.allSkills, c.activeSkills, c.skillTracker),
		tools.NewCrushLogsTool(logFile),
		tools.NewJobOutputTool(),
		tools.NewJobKillTool(),
		tools.NewDownloadTool(c.permissions, c.cfg.WorkingDir(), nil),
		tools.NewEditTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewMultiEditTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewFetchTool(c.permissions, c.cfg.WorkingDir(), nil),
		tools.NewGlobTool(c.cfg.WorkingDir()),
		tools.NewGrepTool(c.cfg.WorkingDir(), c.cfg.Config().Tools.Grep),
		tools.NewLsTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Tools.Ls),
		tools.NewSourcegraphTool(nil),
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

// TODO: when we support multiple agents we need to change this so that we pass in the agent specific model config
func (c *coordinator) buildAgentModels(ctx context.Context, isSubAgent bool) (Model, Model, error) {
	largeModelCfg, ok := c.cfg.Config().Models[config.SelectedModelTypeLarge]
	if !ok {
		return Model{}, Model{}, errLargeModelNotSelected
	}
	smallModelCfg, ok := c.cfg.Config().Models[config.SelectedModelTypeSmall]
	if !ok {
		return Model{}, Model{}, errSmallModelNotSelected
	}
	return c.buildModelsFromCfg(ctx, largeModelCfg, smallModelCfg, isSubAgent)
}

// buildModelsFromCfg builds Model objects from explicit SelectedModel configs.
func (c *coordinator) buildModelsFromCfg(ctx context.Context, largeModelCfg, smallModelCfg config.SelectedModel, isSubAgent bool) (Model, Model, error) {
	largeProviderCfg, ok := c.cfg.Config().Providers.Get(largeModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errLargeModelProviderNotConfigured
	}

	largeProvider, err := c.buildProvider(largeProviderCfg, largeModelCfg, isSubAgent)
	if err != nil {
		return Model{}, Model{}, err
	}

	smallProviderCfg, ok := c.cfg.Config().Providers.Get(smallModelCfg.Provider)
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

func (c *coordinator) CancelAll() {
	c.currentAgent.CancelAll()
}

func (c *coordinator) ClearQueue(sessionID string) {
	c.currentAgent.ClearQueue(sessionID)
}

// startInterruptTicker launches a goroutine that polls pending_injects for an
// interrupt=true row for sessionID every interruptInjectTick, for as long as
// ctx is live (i.e. the duration of the owning turn). On the first interrupt
// row it consumes it, requeues the already-persisted message via
// requeueInterruptMessage, and returns — one interrupt event maps to exactly
// one cancel+requeue; the turn restarts with that message and, if a new
// interrupt arrives, the fresh turn's ticker handles it. The goroutine also
// exits when ctx is cancelled (turn finished/aborted), so it never outlives
// the turn.
func (c *coordinator) startInterruptTicker(ctx context.Context, sessionID string) {
	go func() {
		ticker := time.NewTicker(interruptInjectTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fired, err := c.handleInterruptTick(ctx, sessionID)
				if err != nil {
					slog.Warn("coordinator: interrupt-inject tick failed",
						"session_id", sessionID, "err", err)
					continue
				}
				if fired {
					return
				}
			}
		}
	}()
}

// handleInterruptTick performs one poll of the interrupt-inject queue. It
// returns fired=true when it consumed an interrupt row and issued a
// cancel+requeue (the caller then stops ticking). Extracted from the ticker
// goroutine so it can be unit-tested directly with a real session.Service and
// message.Service, without a live provider. It is a no-op returning
// (false, nil) when no interrupt row is pending.
func (c *coordinator) handleInterruptTick(ctx context.Context, sessionID string) (bool, error) {
	pi, err := c.sessions.ConsumeInterruptInject(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if pi == nil {
		return false, nil
	}
	if err := c.requeueInterruptMessage(ctx, sessionID, pi.MessageID); err != nil {
		return false, err
	}
	return true, nil
}

// requeueInterruptMessage loads the already-persisted user message referenced
// by messageID, queues a call that points at it (ExistingMessageID set, so the
// agent splices it in WITHOUT creating a duplicate row), and cancels the
// running turn — mirroring InterruptAndSend's cancel+requeue but for a message
// the CLI already created. Notify is called so a web UI attached to THIS
// process renders the foreign-created message live rather than on reload.
func (c *coordinator) requeueInterruptMessage(ctx context.Context, sessionID, messageID string) error {
	injMsg, err := c.messages.Get(ctx, messageID)
	if err != nil {
		return fmt.Errorf("interrupt inject references missing message %q: %w", messageID, err)
	}

	call, err := c.buildCall(ctx, sessionID, injMsg.FullText(), nil)
	if err != nil {
		return err
	}
	// Reference the existing row; the agent must not re-create it.
	call.ExistingMessageID = messageID
	c.currentAgent.QueueMessage(call)
	c.currentAgent.Cancel(sessionID)

	// The row was created by a foreign process (`crush sessions inject`), so
	// its Create() never published through this process's message broker.
	// Notify pushes the already-persisted message so an attached web UI
	// renders it live. Idempotent — a redundant Notify does not harm the UI.
	c.messages.Notify(injMsg)
	return nil
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
	if large != nil || small != nil {
		if applyErr := c.applyModelOverrides(ctx, large, small); applyErr != nil {
			return applyErr
		}
	}
	call, err := c.buildCall(ctx, sessionID, prompt, attachments)
	if err != nil {
		return err
	}
	c.currentAgent.QueueMessage(call)
	c.currentAgent.Cancel(sessionID)
	return nil
}

// InjectMessage — see Coordinator interface.
func (c *coordinator) InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error) {
	if err := c.readyWg.Wait(); err != nil {
		return message.Message{}, err
	}
	call, err := c.buildCall(ctx, sessionID, prompt, attachments)
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
		go func() {
			// Detached + cancelable: outlives the OnDone goroutine; the turn's
			// own watchdog/Cancel(sessionID) governs its lifetime, so NO short
			// timeout here (unlike the InjectMessage path — a turn can be long).
			// Tag the context so the persisted user message is marked
			// AutoResumed and rendered with a badge in the web UI. Also tag it
			// as a BackgroundJobNotice so the web shows the notice badge (an
			// auto-resume is also a job-completion notice).
			ctx := context.WithValue(context.Background(), autoResumedCtxKey{}, true)
			ctx = context.WithValue(ctx, backgroundJobNoticeCtxKey{}, true)
			if _, err := c.Run(ctx, sessionID, summary); err != nil {
				slog.Debug("Phase 4 auto-resume run failed (session likely closed)",
					"session_id", sessionID, "shell_id", sh.ID, "err", err)
			}
		}()
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

func (c *coordinator) IsBusy() bool {
	return c.currentAgent.IsBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	return c.currentAgent.IsSessionBusy(sessionID)
}

func (c *coordinator) Model() Model {
	return c.currentAgent.Model()
}

func (c *coordinator) GetSystemPrompt() string {
	return c.currentAgent.SystemPrompt()
}

func (c *coordinator) BuildSystemPrompt(ctx context.Context) (string, error) {
	if c.prompt == nil {
		return "", nil
	}
	model := c.currentAgent.Model()
	return c.prompt.Build(ctx, model.ModelCfg.Provider, model.ModelCfg.Model, c.cfg)
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

func (c *coordinator) QueuedPrompts(sessionID string) int {
	return c.currentAgent.QueuedPrompts(sessionID)
}

func (c *coordinator) QueuedPromptsList(sessionID string) []string {
	return c.currentAgent.QueuedPromptsList(sessionID)
}

func (c *coordinator) Summarize(ctx context.Context, sessionID string) error {
	providerCfg, ok := c.cfg.Config().Providers.Get(c.currentAgent.Model().ModelCfg.Provider)
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
		return c.currentAgent.Summarize(ctx, sessionID, getProviderOptions(c.currentAgent.Model(), providerCfg))
	}

	return c.runWithUnauthorizedRetry(ctx, providerCfg, summarize)
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
// attempts to refresh credentials and re-runs fn once. Returns the
// final error: from the retry if a retry was attempted, otherwise from
// the original run. Callers that need to notify the user on persistent
// failure should check isUnauthorized on the returned error.
func (c *coordinator) runWithUnauthorizedRetry(ctx context.Context, providerCfg config.ProviderConfig, fn func() error) error {
	err := fn()
	if err != nil && c.isUnauthorized(err) {
		if retryErr := c.retryAfterUnauthorized(ctx, providerCfg); retryErr == nil {
			return fn()
		}
	}
	return err
}

// retryAfterUnauthorized attempts to refresh credentials after receiving a 401
// and returns nil if retry should be attempted.
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

func (c *coordinator) TakeSummarizeQueue(sessionID string) (fantasy.ProviderOptions, bool) {
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
	c.cfg.Config().Providers.Set(providerCfg.ID, providerCfg)

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
}

// runSubAgent runs a sub-agent and handles session management and cost accumulation.
// It creates a sub-session, runs the agent with the given prompt, and propagates
// the cost to the parent session.
func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	// Create sub-session
	agentToolSessionID := c.sessions.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
	session, err := c.sessions.CreateTaskSession(ctx, agentToolSessionID, params.SessionID, params.SessionTitle)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("create session: %w", err)
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
	err = c.runWithUnauthorizedRetry(ctx, providerCfg, func() error {
		var runErr error
		result, runErr = run()
		return runErr
	})
	// Notify only if still unauthorized after retry.
	if err != nil && c.isUnauthorized(err) && c.notify != nil && model.ModelCfg.Provider == hyper.Name {
		c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: model.ModelCfg.Provider,
		})
	}
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to generate response: %s", err)), nil
	}

	// Update parent session cost on a best-effort basis. A failure here must
	// not discard the sub-agent output that was already produced.
	if err := c.updateParentSessionCost(ctx, session.ID, params.SessionID); err != nil {
		slog.Warn(
			"Failed to update parent session cost",
			"child_session", session.ID,
			"parent_session", params.SessionID,
			"error", err,
		)
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

// updateParentSessionCost accumulates the cost from a child session into
// its parent session. Uses the atomic additive UPDATE (IncrementCost) so
// concurrent sub-agent fan-out (multiple children finishing in different
// goroutines and each charging the same parent) cannot lose cost via
// read-modify-write the way the old Get+modify+Save pattern would.
//
// Fork patch (concurrency): rewritten from Get-parent → modify Cost →
// Save back. See CHANGELOG.fork.md (Section 4.I).
func (c *coordinator) updateParentSessionCost(ctx context.Context, childSessionID, parentSessionID string) error {
	childSession, err := c.sessions.Get(ctx, childSessionID)
	if err != nil {
		return fmt.Errorf("get child session: %w", err)
	}

	// IncrementCost handles the zero-delta case by routing to Get, which
	// surfaces a not-found error if the parent session was deleted between
	// the child finishing and this call — preserves the previous error
	// semantics.
	if _, err := c.sessions.IncrementCost(ctx, parentSessionID, childSession.Cost); err != nil {
		return fmt.Errorf("increment parent session cost: %w", err)
	}

	return nil
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
