package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/cliprovider"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/version"

	"charm.land/catwalk/pkg/catwalk"
)

// handleIncoming dispatches an incoming WS message from a client.
// All long-running operations are launched in goroutines so this
// function never blocks.
func handleIncoming(ctx context.Context, a *appPkg.App, c *Client, raw []byte) {
	var msg WSMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		slog.Debug("ws: malformed message", "err", err)
		c.reply("", EventError, nil, "malformed message")
		return
	}

	switch msg.Type {
	case CmdSendMessage:
		go handleSendMessage(ctx, a, c, msg)
	case CmdInterruptAndSend:
		go handleInterruptAndSend(ctx, a, c, msg)
	case CmdInjectMessage:
		go handleInjectMessage(ctx, a, c, msg)
	case CmdCancelAgent:
		go handleCancelAgent(ctx, a, c, msg)
	case CmdCreateSession:
		go handleCreateSession(ctx, a, c, msg)
	case CmdForkSession:
		go handleForkSession(ctx, a, c, msg)
	case CmdDeleteSession:
		go handleDeleteSession(ctx, a, c, msg)
	case CmdDeleteOtherSessions:
		go handleDeleteOtherSessions(ctx, a, c, msg)
	case CmdListSessions:
		go handleListSessions(ctx, a, c, msg)
	case CmdLoadMessages:
		go handleLoadMessages(ctx, a, c, msg)
	case CmdGrantPermission:
		go handleGrantPermission(a, c, msg, false)
	case CmdGrantPermissionPersistent:
		go handleGrantPermission(a, c, msg, true)
	case CmdDenyPermission:
		go handleDenyPermission(a, c, msg)
	case CmdListSessionPermissions:
		go handleListSessionPermissions(ctx, a, c, msg)
	case CmdUpdatePermissionRule:
		go handleUpdatePermissionRule(a, c, msg)
	case CmdDeletePermissionRule:
		go handleDeletePermissionRule(a, c, msg)
	case CmdGetConfig:
		go handleGetConfig(a, c, msg)
	case CmdGetLogs:
		go handleGetLogs(a, c, msg)
	case CmdSetTheme:
		go handleSetTheme(a, c, msg)
	case CmdSetKeepAlive:
		go handleSetKeepAlive(a, c, msg)
	case CmdRenameSession:
		go handleRenameSession(ctx, a, c, msg)
	case CmdSetSessionModels:
		go handleSetSessionModels(ctx, a, c, msg)
	case CmdSetYolo:
		go handleSetYolo(a, c, msg)
	case CmdRemoveRecentModel:
		go handleRemoveRecentModel(a, c, msg)
	case CmdTrackModelUsage:
		go handleTrackModelUsage(a, c, msg)
	case CmdSetProviderKey:
		go handleSetProviderKey(a, c, msg)
	case CmdRemoveProviderKey:
		go handleRemoveProviderKey(a, c, msg)
	case CmdDeleteMessage:
		go handleDeleteMessage(ctx, a, c, msg)
	case CmdDeleteMessages:
		go handleDeleteMessages(ctx, a, c, msg)
	case CmdUpdateMessageContent:
		go handleUpdateMessageContent(ctx, a, c, msg)
	case CmdUpdateMessageThinking:
		go handleUpdateMessageThinking(ctx, a, c, msg)
	case CmdGetSystemPrompt:
		go handleGetSystemPrompt(ctx, a, c, msg)
	case CmdSetSystemPrompt:
		go handleSetSystemPrompt(ctx, a, c, msg)
	case CmdSummarizeSession:
		go handleSummarizeSession(ctx, a, c, msg)
	case CmdCancelQueuedSummarize:
		go handleCancelQueuedSummarize(a, c, msg)
	case CmdDeleteMessagePart:
		go handleDeleteMessagePart(ctx, a, c, msg)
	case CmdUpdateMessagePart:
		go handleUpdateMessagePart(ctx, a, c, msg)
	case CmdTogglePinMessage:
		go handleTogglePinMessage(ctx, a, c, msg)
	case CmdRerunMessage:
		go handleRerunMessage(ctx, a, c, msg)
	case CmdLogClientEvent:
		go handleLogClientEvent(a, c, msg)
	case CmdLogClientError:
		go handleLogClientError(c, msg)
	case CmdSetMCPDisabled:
		go handleSetMCPDisabled(ctx, a, c, msg)
	case CmdAddMCPServer:
		go handleAddMCPServer(ctx, a, c, msg)
	case CmdRemoveMCPServer:
		go handleRemoveMCPServer(a, c, msg)
	case CmdUpdateMCPServer:
		go handleUpdateMCPServer(ctx, a, c, msg)
	case CmdSetDebug:
		go handleSetDebug(a, c, msg)
	case CmdAddContextPath:
		go handleAddContextPath(a, c, msg)
	case CmdRemoveContextPath:
		go handleRemoveContextPath(a, c, msg)
	case CmdGetSkills:
		go handleGetSkills(a, c, msg)
	case CmdAddSkillsPath:
		go handleAddSkillsPath(a, c, msg)
	case CmdRemoveSkillsPath:
		go handleRemoveSkillsPath(a, c, msg)
	case CmdInitializeProject:
		go handleInitializeProject(ctx, a, c, msg)
	case CmdAddCustomProvider:
		go handleAddCustomProvider(a, c, msg)
	case CmdRemoveCustomProvider:
		go handleRemoveCustomProvider(a, c, msg)
	case CmdUpdateCustomProvider:
		go handleUpdateCustomProvider(a, c, msg)
	case CmdUpdateTodos:
		go handleUpdateTodos(ctx, a, c, msg)
	default:
		slog.Debug("ws: unknown command", "type", msg.Type)
		c.reply(msg.ID, EventError, nil, "unknown command: "+msg.Type)
	}
}

// saveAttachmentToDisk saves an attachment to .crush/attachments/ with a
// timestamped filename and returns the absolute path.
func saveAttachmentToDisk(workingDir, fileName string, data []byte) (string, error) {
	dir := filepath.Join(workingDir, ".crush", "attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create attachments dir: %w", err)
	}
	ts := time.Now().Format("2006-01-02_15-04-05")
	name := ts + "_" + filepath.Base(fileName)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write attachment: %w", err)
	}
	return path, nil
}

func handleSendMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SendMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	slog.Info("ws: handleSendMessage", "sessionID", p.SessionID, "content", p.Content, "attachments", len(p.Attachments))

	// Save attachments to disk and append file paths to the prompt text.
	// This ensures CLI-based agents can access files via their read tools.
	var attachments []message.Attachment
	for _, att := range p.Attachments {
		slog.Info("ws: attachment received", "fileName", att.FileName, "mimeType", att.MimeType, "dataLen", len(att.Data))

		// Save to .crush/attachments/ with timestamped name.
		savedPath, saveErr := saveAttachmentToDisk(a.Store().WorkingDir(), att.FileName, att.Data)
		if saveErr != nil {
			slog.Warn("ws: failed to save attachment to disk", "err", saveErr)
		} else {
			p.Content += "\n[Attached file: " + savedPath + "]"
			slog.Info("ws: attachment saved", "path", savedPath)
		}

		attachments = append(attachments, message.Attachment{
			FileName: att.FileName,
			MimeType: att.MimeType,
			Content:  att.Data,
		})
	}

	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}

	// A human re-entering the loop re-arms Phase 4 autonomy for this session.
	a.AgentCoordinator.ResetAutoResumeCounter(p.SessionID)

	// Priority:
	// 1. Explicit override in message payload (from UI)
	// 2. Models stored in the session record in DB
	// 3. Global defaults from config

	var largeOverride, smallOverride *agent.ModelOverride

	// Check payload first
	if p.LargeModel != nil {
		largeOverride = &agent.ModelOverride{Provider: p.LargeModel.Provider, Model: p.LargeModel.Model}
	}
	if p.SmallModel != nil {
		smallOverride = &agent.ModelOverride{Provider: p.SmallModel.Provider, Model: p.SmallModel.Model}
	}

	// If no payload override, check DB
	if largeOverride == nil || smallOverride == nil {
		sess, err := a.Sessions.Get(ctx, p.SessionID)
		if err == nil {
			if largeOverride == nil && sess.LargeModelID != "" {
				slog.Info("ws: using models from DB", "sessionID", p.SessionID, "large", sess.LargeModelID)
				largeOverride = &agent.ModelOverride{Provider: sess.LargeModelProvider, Model: sess.LargeModelID}
			}
			if smallOverride == nil && sess.SmallModelID != "" {
				smallOverride = &agent.ModelOverride{Provider: sess.SmallModelProvider, Model: sess.SmallModelID}
			}
		}
	}

	if largeOverride != nil {
		slog.Info("ws: final models for run", "sessionID", p.SessionID, "large", largeOverride.Model)
	}

	// Decouple the agent run from the WebSocket connection lifetime.
	// Without this, closing/refreshing the browser tab would cancel the agent.
	// Explicit cancellation is still available via Cancel(sessionID).
	agentCtx := context.WithoutCancel(ctx)

	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: true})
	var err error
	if largeOverride != nil || smallOverride != nil {
		_, err = a.AgentCoordinator.RunWithOverrides(agentCtx, p.SessionID, p.Content, largeOverride, smallOverride, attachments...)
	} else {
		_, err = a.AgentCoordinator.Run(agentCtx, p.SessionID, p.Content, attachments...)
	}
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: false})

	if err != nil {
		slog.Error("ws: agent run error", "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
	}

	// Run any compact (summarise) that was queued while the task was busy.
	if _, queued := a.AgentCoordinator.TakeSummarizeQueue(p.SessionID); queued {
		c.hub.Broadcast(EventSummarizeQueued, SummarizeQueuedPayload{SessionID: p.SessionID, Queued: false})
		c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: true})
		if summarizeErr := a.AgentCoordinator.Summarize(agentCtx, p.SessionID); summarizeErr != nil {
			slog.Error("ws: queued summarize error", "err", summarizeErr)
		}
		c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: false})
	}
}

// handleInterruptAndSend cancels the running turn and queues a new user
// message in one shot. The in-flight agent.Run() finalises the cancelled
// assistant message with FinishReasonCanceled, then its cancel-handling
// branch drains the queue and immediately re-enters Run() with the new
// message — so the user keeps everything produced so far plus their new
// instruction.
func handleInterruptAndSend(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SendMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	slog.Info("ws: handleInterruptAndSend", "sessionID", p.SessionID, "content", p.Content, "attachments", len(p.Attachments))

	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}

	// A human re-entering the loop re-arms Phase 4 autonomy for this session.
	a.AgentCoordinator.ResetAutoResumeCounter(p.SessionID)

	// Same attachments path as handleSendMessage: save to disk, append paths
	// to the prompt text so CLI tools can read them, and forward attachment
	// metadata so vision-capable providers can ingest images.
	var attachments []message.Attachment
	for _, att := range p.Attachments {
		savedPath, saveErr := saveAttachmentToDisk(a.Store().WorkingDir(), att.FileName, att.Data)
		if saveErr != nil {
			slog.Warn("ws: failed to save attachment to disk", "err", saveErr)
		} else {
			p.Content += "\n[Attached file: " + savedPath + "]"
		}
		attachments = append(attachments, message.Attachment{
			FileName: att.FileName,
			MimeType: att.MimeType,
			Content:  att.Data,
		})
	}

	// Model overrides follow the same priority as handleSendMessage:
	// payload > DB session record > global defaults.
	var largeOverride, smallOverride *agent.ModelOverride
	if p.LargeModel != nil {
		largeOverride = &agent.ModelOverride{Provider: p.LargeModel.Provider, Model: p.LargeModel.Model}
	}
	if p.SmallModel != nil {
		smallOverride = &agent.ModelOverride{Provider: p.SmallModel.Provider, Model: p.SmallModel.Model}
	}
	if largeOverride == nil || smallOverride == nil {
		if sess, err := a.Sessions.Get(ctx, p.SessionID); err == nil {
			if largeOverride == nil && sess.LargeModelID != "" {
				largeOverride = &agent.ModelOverride{Provider: sess.LargeModelProvider, Model: sess.LargeModelID}
			}
			if smallOverride == nil && sess.SmallModelID != "" {
				smallOverride = &agent.ModelOverride{Provider: sess.SmallModelProvider, Model: sess.SmallModelID}
			}
		}
	}

	agentCtx := context.WithoutCancel(ctx)
	if err := a.AgentCoordinator.InterruptAndSend(agentCtx, p.SessionID, p.Content, largeOverride, smallOverride, attachments...); err != nil {
		slog.Error("ws: interrupt-and-send failed", "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	// Don't toggle EventAgentBusy here: the running handleSendMessage
	// goroutine will publish busy=false when its Run() returns, and the
	// queue drain inside Run() will publish busy=true again for the new
	// turn. Touching the flag here would create a flicker.
	c.reply(msg.ID, EventResponse, map[string]string{"status": "queued"}, "")
}

// handleInjectMessage persists a user message to the session DB right now
// (so the UI shows it instantly) and — if the session is busy — schedules
// the same message to be merged into the next provider request without
// cancelling the in-flight turn. See SessionAgent.InjectMessage for the
// drain-at-PrepareStep mechanism.
func handleInjectMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SendMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	slog.Info("ws: handleInjectMessage", "sessionID", p.SessionID, "content", p.Content, "attachments", len(p.Attachments))

	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}

	// A human re-entering the loop re-arms Phase 4 autonomy for this session.
	a.AgentCoordinator.ResetAutoResumeCounter(p.SessionID)

	// Same attachments path as handleSendMessage.
	var attachments []message.Attachment
	for _, att := range p.Attachments {
		savedPath, saveErr := saveAttachmentToDisk(a.Store().WorkingDir(), att.FileName, att.Data)
		if saveErr != nil {
			slog.Warn("ws: failed to save attachment to disk", "err", saveErr)
		} else {
			p.Content += "\n[Attached file: " + savedPath + "]"
		}
		attachments = append(attachments, message.Attachment{
			FileName: att.FileName,
			MimeType: att.MimeType,
			Content:  att.Data,
		})
	}

	agentCtx := context.WithoutCancel(ctx)
	if _, err := a.AgentCoordinator.InjectMessage(agentCtx, p.SessionID, p.Content, attachments...); err != nil {
		slog.Error("ws: inject-message failed", "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "injected"}, "")
}

func handleSetSessionModels(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SetSessionModelsPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	slog.Info("ws: handleSetSessionModels", "sessionID", p.SessionID, "large", p.LargeModel, "small", p.SmallModel)

	var lp, lm, lre, sp, sm, sre string
	if p.LargeModel != nil {
		lp, lm = p.LargeModel.Provider, p.LargeModel.Model
		lre = p.LargeModel.ReasoningEffort
		if lre == "" {
			lre = "medium" // default
		}
	}
	if p.SmallModel != nil {
		sp, sm = p.SmallModel.Provider, p.SmallModel.Model
		sre = p.SmallModel.ReasoningEffort
		if sre == "" {
			sre = "medium" // default
		}
	}

	if err := a.Sessions.UpdateModels(ctx, p.SessionID, lp, lm, sp, sm); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}

	// Update reasoning effort for models that support it
	if lre != "" || sre != "" {
		if err := a.Sessions.UpdateReasoningEffort(ctx, p.SessionID, lre, sre); err != nil {
			slog.Warn("ws: failed to update reasoning effort", "err", err)
		}
	}

	// Record recently used models in the config (persists across restarts)
	store := a.Store()
	if store != nil && lp != "" && lm != "" {
		if err := store.RecordRecentModel(config.ScopeGlobal, config.SelectedModelTypeLarge, config.SelectedModel{Provider: lp, Model: lm}); err != nil {
			slog.Warn("ws: failed to record recent large model", "err", err)
		}
	}
	if store != nil && sp != "" && sm != "" {
		if err := store.RecordRecentModel(config.ScopeGlobal, config.SelectedModelTypeSmall, config.SelectedModel{Provider: sp, Model: sm}); err != nil {
			slog.Warn("ws: failed to record recent small model", "err", err)
		}
	}

	// Broadcast updated session so clients can refresh their model selectors.
	if sess, err := a.Sessions.Get(ctx, p.SessionID); err == nil {
		c.hub.Broadcast(EventSessionUpdated, sess)
	}
	// Broadcast updated config so all clients see the new recent-models list immediately.
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveRecentModel(a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveRecentModelPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		return
	}
	modelType := config.SelectedModelType(p.ModelType)
	if err := store.RemoveRecentModel(config.ScopeGlobal, modelType, config.SelectedModel{Provider: p.Provider, Model: p.Model}); err != nil {
		slog.Warn("ws: failed to remove recent model", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleTrackModelUsage(a *appPkg.App, c *Client, msg WSMessage) {
	var p TrackModelUsagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		return
	}
	modelType := config.SelectedModelType(p.ModelType)
	// Use UpdatePreferredModel which handles both preferred model and recent models tracking
	if err := store.UpdatePreferredModel(config.ScopeGlobal, modelType, config.SelectedModel{Provider: p.Provider, Model: p.Model}); err != nil {
		slog.Warn("ws: failed to track model usage", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleSetYolo(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetYoloPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		slog.Error("handleSetYolo: invalid payload", "err", err, "payload", string(msg.Payload))
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	slog.Info("handleSetYolo: received", "sessionID", p.SessionID, "enabled", p.Enabled)

	// Set session-specific YOLO mode in database.
	if p.SessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.Sessions.SetYolo(ctx, p.SessionID, p.Enabled); err != nil {
			slog.Error("handleSetYolo: failed to set session YOLO", "err", err)
		}
	} else {
		slog.Warn("handleSetYolo: empty sessionID")
	}

	// Also set global skip flag for backwards compatibility.
	a.Permissions.SetSkipRequests(p.Enabled)
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleCancelAgent(_ context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p CancelAgentPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	a.AgentCoordinator.Cancel(p.SessionID)
	// Force-broadcast busy=false immediately so the UI unblocks and the replay
	// buffer records a definitive "not busy" state. The goroutine will also
	// broadcast false when it actually finishes (harmless duplicate).
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: false})
}

func handleCreateSession(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p CreateSessionPayload
	if len(msg.Payload) > 0 && string(msg.Payload) != "null" {
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			c.reply(msg.ID, EventError, nil, "invalid payload")
			return
		}
	}
	title := p.Title
	if title == "" {
		title = "New Session"
	}
	sess, err := a.Sessions.Create(ctx, title)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}

	// Set default models from config for the new session immediately
	cfg := a.Config()
	if cfg != nil {
		var lp, lm, sp, sm string
		if large, ok := cfg.Models[config.SelectedModelTypeLarge]; ok {
			lp, lm = large.Provider, large.Model
		}
		if small, ok := cfg.Models[config.SelectedModelTypeSmall]; ok {
			sp, sm = small.Provider, small.Model
		}
		if lp != "" || sp != "" {
			_ = a.Sessions.UpdateModels(ctx, sess.ID, lp, lm, sp, sm)
			// Re-fetch to get updated state with models
			if updated, err := a.Sessions.Get(ctx, sess.ID); err == nil {
				sess = updated
			}
		}
	}

	// Generate and save the system prompt for the new session.
	if a.AgentCoordinator != nil {
		if sp, err := a.AgentCoordinator.BuildSystemPrompt(ctx); err == nil && sp != "" {
			if err := a.AgentCoordinator.UpdateSessionSystemPrompt(ctx, sess.ID, sp); err == nil {
				if updated, err := a.Sessions.Get(ctx, sess.ID); err == nil {
					sess = updated
				}
			}
		}
	}

	// Broadcast to all clients so every tab sees the new session.
	c.hub.Broadcast(EventSessionCreated, sess)
}

func handleForkSession(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p ForkSessionPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "sessionID required")
		return
	}

	// Load source session
	src, err := a.Sessions.Get(ctx, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}

	title := p.Title
	if title == "" {
		title = src.Title + " fork"
	}

	// Create the new (forked) session
	fork, err := a.Sessions.Create(ctx, title)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}

	// Copy models from source
	if src.LargeModelProvider != "" || src.SmallModelProvider != "" {
		_ = a.Sessions.UpdateModels(
			ctx, fork.ID,
			src.LargeModelProvider, src.LargeModelID,
			src.SmallModelProvider, src.SmallModelID,
		)
	}

	// Copy system prompt from source
	if src.SystemPrompt != "" {
		_ = a.Sessions.UpdateSystemPrompt(ctx, fork.ID, src.SystemPrompt)
	}

	// Copy todos from source
	if len(src.Todos) > 0 {
		if updated, err2 := a.Sessions.Get(ctx, fork.ID); err2 == nil {
			updated.Todos = src.Todos
			fork, _ = a.Sessions.Save(ctx, updated)
		}
	}

	// Re-fetch fork to get fully updated state
	if updated, err2 := a.Sessions.Get(ctx, fork.ID); err2 == nil {
		fork = updated
	}

	// Copy all messages from source session
	msgs, err := a.Messages.List(ctx, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	for _, m := range msgs {
		_, _ = a.Messages.Create(ctx, fork.ID, message.CreateMessageParams{
			Role:             message.MessageRole(m.Role),
			Parts:            m.Parts,
			Model:            string(m.Model),
			Provider:         m.Provider,
			ReasoningEffort:  m.ReasoningEffort,
			IsSummaryMessage: m.IsSummaryMessage,
		})
	}

	// Broadcast so all tabs see the fork and switch to it
	c.hub.Broadcast(EventSessionCreated, fork)
}

func handleDeleteSession(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteSessionPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if err := a.Sessions.Delete(ctx, p.SessionID); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// handleDeleteOtherSessions deletes every top-level session except the one
// identified by KeepID. Sub-sessions are not deleted directly — they are
// cleaned up by a.Sessions.Delete when their parent is removed, mirroring
// handleDeleteSession. Each deletion publishes a DeletedEvent that the
// events.go pubsub bridge broadcasts as session_deleted, so every connected
// client updates. A no-op ack is returned when KeepID is empty or there is
// nothing else to delete.
func handleDeleteOtherSessions(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteOtherSessionsPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.KeepID == "" {
		c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	sessions, err := a.Sessions.List(ctx)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	for _, s := range sessions {
		// Skip the kept session and any sub-session (those go when their
		// parent is deleted, matching handleDeleteSession's behaviour).
		if s.ID == p.KeepID || s.ParentSessionID != "" {
			continue
		}
		if err := a.Sessions.Delete(ctx, s.ID); err != nil {
			slog.Warn("delete_other_sessions: failed to delete session", "id", s.ID, "err", err)
		}
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// externalOwnerLiveThreshold mirrors the heartbeat expiry used by the lock
// acquisition path: a lock whose mtime is fresher than this is treated as a
// live external owner. The lock-renewer touches the file every ~10s, so 20s
// gives one missed tick of slack without flipping foreign-owned sessions
// in and out of read-only mode.
const externalOwnerLiveThreshold = 20 * time.Second

// annotateExternalOwnership fills OwnedExternal/OwnedByPID for every session
// in the slice. Only flags sessions whose live lock holder is a DIFFERENT
// process — sessions held by us are owned-but-not-external (the UI keeps
// full controls). Sessions with no lock or only a stale lock are left clean.
func annotateExternalOwnership(a *appPkg.App, sessions []session.Session) {
	dataDir := externalOwnershipDataDir(a)
	if dataDir == "" {
		return
	}
	self := os.Getpid()
	for i := range sessions {
		st := session.InspectSessionLock(dataDir, sessions[i].ID, externalOwnerLiveThreshold)
		if !st.Live || st.PID == 0 || st.PID == self {
			continue
		}
		sessions[i].OwnedExternal = true
		sessions[i].OwnedByPID = st.PID
	}
}

// AnnotateSessionExternalOwnership is the single-session variant used by the
// session pubsub bridge in events.go and by every handler that broadcasts a
// fresh Session payload over WS. Exported so events.go can reach it without
// duplicating the lock-inspection logic.
func AnnotateSessionExternalOwnership(a *appPkg.App, s *session.Session) {
	if s == nil {
		return
	}
	dataDir := externalOwnershipDataDir(a)
	if dataDir == "" {
		return
	}
	self := os.Getpid()
	st := session.InspectSessionLock(dataDir, s.ID, externalOwnerLiveThreshold)
	if !st.Live || st.PID == 0 || st.PID == self {
		s.OwnedExternal = false
		s.OwnedByPID = 0
		return
	}
	s.OwnedExternal = true
	s.OwnedByPID = st.PID
}

func externalOwnershipDataDir(a *appPkg.App) string {
	cfg := a.Config()
	if cfg == nil || cfg.Options == nil {
		return ""
	}
	return cfg.Options.DataDirectory
}

func handleListSessions(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	sessions, err := a.Sessions.List(ctx)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if sessions == nil {
		sessions = []session.Session{}
	}
	annotateExternalOwnership(a, sessions)
	c.reply(msg.ID, EventSessionsList, sessions, "")

	// Correct any stale agent_busy state in the replay buffer by sending the
	// server's authoritative busy state for every session to this client only
	// (not broadcast — other clients already have accurate live state).
	if a.AgentCoordinator != nil {
		for _, s := range sessions {
			busy := a.AgentCoordinator.IsSessionBusy(s.ID)
			c.reply("", EventAgentBusy, AgentBusyPayload{SessionID: s.ID, Busy: busy}, "")
		}
	}
}

func handleLoadMessages(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p LoadMessagesPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	msgs, err := a.Messages.List(ctx, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if msgs == nil {
		msgs = []message.Message{}
	}
	// Wrap with the source sessionID so the frontend can route even an
	// EMPTY list to the right store (sub-agent vs main session). Without
	// this, a lazy load_messages for a sub-agent session that's still empty
	// returns [], and the frontend can't tell whether to clear the active
	// chat or the sub-agent buffer — it would blindly clear the active.
	c.reply(msg.ID, EventMessagesList, map[string]any{
		"SessionID": p.SessionID,
		"Messages":  toMessagesWire(msgs),
	}, "")
}

func handleGrantPermission(a *appPkg.App, c *Client, msg WSMessage, persistent bool) {
	var p PermissionResponsePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	req := permission.PermissionRequest{ID: p.PermissionID}
	if persistent {
		a.Permissions.GrantPersistent(req)
	} else {
		a.Permissions.Grant(req)
	}
}

func handleDenyPermission(a *appPkg.App, c *Client, msg WSMessage) {
	var p PermissionResponsePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	a.Permissions.Deny(permission.PermissionRequest{ID: p.PermissionID})
}

func handleListSessionPermissions(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p ListSessionPermissionsPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	permissions, err := a.Permissions.ListSessionPermissions(p.SessionID)
	if err != nil {
		slog.Warn("ws: failed to query session permissions", "err", err)
		c.reply(msg.ID, EventError, nil, "failed to query permissions")
		return
	}

	c.reply(msg.ID, "session_permissions", permissions, "")
}

func handleUpdatePermissionRule(a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdatePermissionRulePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	if err := a.Permissions.UpdatePermissionEnabled(p.RuleID, p.Enabled); err != nil {
		slog.Warn("ws: failed to update permission rule", "err", err)
		c.reply(msg.ID, EventError, nil, "failed to update rule")
		return
	}

	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleDeletePermissionRule(a *appPkg.App, c *Client, msg WSMessage) {
	var p DeletePermissionRulePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	if err := a.Permissions.DeletePermission(p.RuleID); err != nil {
		slog.Warn("ws: failed to delete permission rule", "err", err)
		c.reply(msg.ID, EventError, nil, "failed to delete rule")
		return
	}

	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func buildConfigWire(a *appPkg.App) (ConfigWire, bool) {
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		return ConfigWire{}, false
	}
	wire := ConfigWire{
		Models:    make(map[string]ModelEntryWire, len(cfg.Models)),
		Providers: make(map[string]ProviderWire),
	}
	for k, v := range cfg.Models {
		wire.Models[string(k)] = ModelEntryWire{
			Provider: v.Provider,
			Model:    v.Model,
		}
	}

	enabledIDs := make(map[string]config.ProviderConfig)
	for _, ep := range cfg.EnabledProviders() {
		enabledIDs[ep.ID] = ep
	}

	for _, p := range store.KnownProviders() {
		id := string(p.ID)
		if ep, ok := enabledIDs[id]; ok {
			pw := ProviderWire{Name: p.Name, Enabled: true, Type: string(p.Type), APIKeySet: ep.APIKey != "", Models: make([]ModelInfoWire, len(ep.Models))}
			for i, m := range ep.Models {
				pw.Models[i] = ModelInfoWire{ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow}
			}
			wire.Providers[id] = pw
		} else {
			pw := ProviderWire{Name: p.Name, Enabled: false, Type: string(p.Type), Models: make([]ModelInfoWire, len(p.Models))}
			for i, m := range p.Models {
				pw.Models[i] = ModelInfoWire{ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow}
			}
			wire.Providers[id] = pw
		}
	}

	for _, ep := range cfg.EnabledProviders() {
		if _, exists := wire.Providers[ep.ID]; !exists {
			// Custom provider not in the known catalog.
			// Built-in auto-detected providers (e.g. local-cli) are not user-added
			// custom providers and must not appear in the Custom Providers modal.
			isCustom := ep.Type != cliprovider.ProviderType
			pw := ProviderWire{
				Name:      ep.Name,
				Enabled:   true,
				Type:      string(ep.Type),
				BaseURL:   ep.BaseURL,
				IsCustom:  isCustom,
				APIKeySet: ep.APIKey != "",
				Models:    make([]ModelInfoWire, len(ep.Models)),
			}
			for i, m := range ep.Models {
				pw.Models[i] = ModelInfoWire{ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow}
			}
			wire.Providers[ep.ID] = pw
		}
	}

	wire.Yolo = a.Permissions.SkipRequests()
	wire.Debug = cfg.Options.Debug
	if cfg.Options != nil {
		wire.ContextPaths = cfg.Options.ContextPaths
		wire.SkillsPaths = cfg.Options.SkillsPaths
		wire.InitializeAs = cfg.Options.InitializeAs
	}
	if cfg.Options != nil && cfg.Options.TUI != nil {
		wire.Theme = cfg.Options.TUI.Theme
	}
	// KeepAliveEnabled: default ON. nil → true; explicit value passes through.
	wire.KeepAliveEnabled = true
	if cfg.Options != nil && cfg.Options.KeepAliveEnabled != nil {
		wire.KeepAliveEnabled = *cfg.Options.KeepAliveEnabled
	}

	for _, m := range cfg.RecentModels[config.SelectedModelTypeLarge] {
		wire.RecentLargeModels = append(wire.RecentLargeModels, ModelEntryWire{Provider: m.Provider, Model: m.Model})
	}
	for _, m := range cfg.RecentModels[config.SelectedModelTypeSmall] {
		wire.RecentSmallModels = append(wire.RecentSmallModels, ModelEntryWire{Provider: m.Provider, Model: m.Model})
	}

	wire.Version = version.FullVersion()
	wire.CWD = store.WorkingDir()

	return wire, true
}

func handleGetConfig(a *appPkg.App, c *Client, msg WSMessage) {
	wire, ok := buildConfigWire(a)
	if !ok {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	c.reply(msg.ID, EventConfig, wire, "")
}

func handleGetLogs(a *appPkg.App, c *Client, msg WSMessage) {
	var p GetLogsPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	// Get log file path
	logPath := a.Store().LogPath()
	if logPath == "" {
		c.reply(msg.ID, EventError, nil, "log path not configured")
		return
	}

	// Read last N lines from log file
	logs, err := readLastNLines(logPath, p.Lines)
	if err != nil {
		slog.Error("Failed to read log file", "path", logPath, "error", err)
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("failed to read logs: %v", err))
		return
	}

	c.reply(msg.ID, EventLogs, logs, "")
}

// readLastNLines reads the last N lines from a file (0 = all lines)
func readLastNLines(path string, n int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	if n <= 0 {
		return string(data), nil
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n"), nil
	}

	return strings.Join(lines[len(lines)-n:], "\n"), nil
}

func handleSetProviderKey(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetProviderKeyPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if err := store.SetProviderAPIKey(config.ScopeGlobal, p.ProviderID, p.APIKey); err != nil {
		slog.Warn("ws: failed to set provider API key", "provider", p.ProviderID, "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	// Broadcast updated config to all clients
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveProviderKey(a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveProviderKeyPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if err := store.RemoveProviderAPIKey(config.ScopeGlobal, p.ProviderID); err != nil {
		slog.Warn("ws: failed to remove provider API key", "provider", p.ProviderID, "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRenameSession(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p RenameSessionPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	sess, err := a.Sessions.Get(ctx, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	sess.Title = p.Title
	sess, err = a.Sessions.Save(ctx, sess)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.hub.Broadcast(EventSessionUpdated, sess)
}

func handleDeleteMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	// Delete publishes DeletedEvent internally; events.go broadcasts EventMessageDeleted.
	if err := a.Messages.Delete(ctx, p.MessageID); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleDeleteMessages(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteMessagesPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	for _, id := range p.MessageIDs {
		if err := a.Messages.Delete(ctx, id); err != nil {
			slog.Warn("ws: failed to delete message", "id", id, "err", err)
		}
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateMessageContent(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateMessageContentPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	m, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	// Replace text parts with the new content, keep all other parts intact
	newParts := make([]message.ContentPart, 0, len(m.Parts))
	replaced := false
	for _, part := range m.Parts {
		if _, ok := part.(message.TextContent); ok && !replaced {
			newParts = append(newParts, message.TextContent{Text: p.Content})
			replaced = true
		} else if _, ok := part.(message.TextContent); ok {
			// skip additional text parts — merged into first
		} else {
			newParts = append(newParts, part)
		}
	}
	if !replaced {
		newParts = append([]message.ContentPart{message.TextContent{Text: p.Content}}, newParts...)
	}
	m.Parts = newParts
	if err := a.Messages.Update(ctx, m); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateMessageThinking(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateMessageThinkingPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	m, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	found := false
	for i, part := range m.Parts {
		if rc, ok := part.(message.ReasoningContent); ok {
			m.Parts[i] = message.ReasoningContent{
				Thinking:         p.Thinking,
				Signature:        rc.Signature,
				ThoughtSignature: rc.ThoughtSignature,
				ToolID:           rc.ToolID,
				ResponsesData:    rc.ResponsesData,
				StartedAt:        rc.StartedAt,
				FinishedAt:       rc.FinishedAt,
			}
			found = true
			break
		}
	}
	if !found {
		c.reply(msg.ID, EventError, nil, "message has no thinking part")
		return
	}
	if err := a.Messages.Update(ctx, m); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleGetSystemPrompt(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p GetSystemPromptPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload: sessionID required")
		return
	}
	sess, err := a.Sessions.Get(ctx, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventSystemPrompt, map[string]string{"sessionID": p.SessionID, "content": sess.SystemPrompt}, "")
}

func handleSetSystemPrompt(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SetSystemPromptPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}
	if err := a.AgentCoordinator.UpdateSessionSystemPrompt(ctx, p.SessionID, p.Content); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleDeleteMessagePart(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteMessagePartPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.MessageID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	m, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if p.PartIndex < 0 || p.PartIndex >= len(m.Parts) {
		c.reply(msg.ID, EventError, nil, "part index out of range")
		return
	}
	m.Parts = append(m.Parts[:p.PartIndex], m.Parts[p.PartIndex+1:]...)
	if err := a.Messages.Update(ctx, m); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateMessagePart(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateMessagePartPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.MessageID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	m, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if p.PartIndex < 0 || p.PartIndex >= len(m.Parts) {
		c.reply(msg.ID, EventError, nil, "part index out of range")
		return
	}
	switch part := m.Parts[p.PartIndex].(type) {
	case message.TextContent:
		m.Parts[p.PartIndex] = message.TextContent{Text: p.Content}
	case message.ReasoningContent:
		m.Parts[p.PartIndex] = message.ReasoningContent{
			Thinking:         p.Content,
			Signature:        part.Signature,
			ThoughtSignature: part.ThoughtSignature,
			ToolID:           part.ToolID,
			ResponsesData:    part.ResponsesData,
			StartedAt:        part.StartedAt,
			FinishedAt:       part.FinishedAt,
		}
	case message.ToolCall:
		m.Parts[p.PartIndex] = message.ToolCall{
			ID:       part.ID,
			Name:     part.Name,
			Input:    p.Content,
			Finished: part.Finished,
		}
	case message.ToolResult:
		m.Parts[p.PartIndex] = message.ToolResult{
			ToolCallID: part.ToolCallID,
			Name:       part.Name,
			Content:    p.Content,
			IsError:    part.IsError,
		}
	default:
		c.reply(msg.ID, EventError, nil, "part type not editable")
		return
	}
	if err := a.Messages.Update(ctx, m); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleTogglePinMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p TogglePinMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.MessageID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if err := a.Messages.SetPinned(ctx, p.MessageID, p.Pinned); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleSummarizeSession(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SummarizeSessionPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}
	agentCtx := context.WithoutCancel(ctx)
	// Summarize will queue the request and return ErrSummarizeQueued if busy.
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: true})
	err := a.AgentCoordinator.Summarize(agentCtx, p.SessionID)
	if errors.Is(err, agent.ErrSummarizeQueued) {
		// Undo the busy broadcast — the session isn't busy with summarise yet.
		c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: false})
		c.hub.Broadcast(EventSummarizeQueued, SummarizeQueuedPayload{SessionID: p.SessionID, Queued: true})
		c.reply(msg.ID, EventResponse, map[string]string{"status": "queued"}, "")
		return
	}
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: false})
	if err != nil {
		slog.Error("ws: summarize error", "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleCancelQueuedSummarize(a *appPkg.App, c *Client, msg WSMessage) {
	var p CancelQueuedSummarizePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}
	a.AgentCoordinator.CancelQueuedSummarize(p.SessionID)
	c.hub.Broadcast(EventSummarizeQueued, SummarizeQueuedPayload{SessionID: p.SessionID, Queued: false})
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleLogClientEvent(a *appPkg.App, c *Client, msg WSMessage) {
	cfg := a.Config()
	if cfg == nil || !cfg.Options.Debug {
		return
	}
	var p LogClientEventPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return
	}
	slog.Debug("client event", "event", p.Event, "details", p.Details)
}

func handleLogClientError(c *Client, msg WSMessage) {
	var p LogClientErrorPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return
	}
	slog.Error("client error", "message", p.Message, "source", p.Source, "stack", p.Stack)
}

func handleSetTheme(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetThemePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if err := a.Store().SetTheme(config.ScopeGlobal, p.Theme); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
}

func handleSetKeepAlive(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetKeepAlivePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if err := a.Store().SetKeepAliveEnabled(config.ScopeGlobal, p.Enabled); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
}

func handleSetMCPDisabled(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SetMCPDisabledPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	var err error
	if p.Disabled {
		err = mcp.DisableServer(ctx, store, p.Name)
	} else {
		err = mcp.EnableServer(ctx, store, p.Name)
	}
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleAddMCPServer(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p AddMCPServerPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.Name == "" {
		c.reply(msg.ID, EventError, nil, "name is required")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	mcpCfg := config.MCPConfig{
		Type:    config.MCPType(p.Type),
		Command: p.Command,
		Args:    p.Args,
		URL:     p.URL,
		Env:     p.Env,
		Headers: p.Headers,
		Timeout: p.Timeout,
	}
	if err := mcp.AddServer(ctx, store, p.Name, mcpCfg); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveMCPServer(a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveMCPServerPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if err := mcp.RemoveServer(store, p.Name); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateMCPServer(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateMCPServerPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.OldName == "" || p.Name == "" {
		c.reply(msg.ID, EventError, nil, "oldName and name are required")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	// Remove old entry
	if err := mcp.RemoveServer(store, p.OldName); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	// Add with new config
	mcpCfg := config.MCPConfig{
		Type:    config.MCPType(p.Type),
		Command: p.Command,
		Args:    p.Args,
		URL:     p.URL,
		Env:     p.Env,
		Headers: p.Headers,
		Timeout: p.Timeout,
	}
	if err := mcp.AddServer(ctx, store, p.Name, mcpCfg); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// ── Debug settings ────────────────────────────────────────────────────────────

func handleSetDebug(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetDebugPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.Options == nil {
		cfg.Options = &config.Options{}
	}
	cfg.Options.Debug = p.Debug
	if err := store.SetConfigField(config.ScopeGlobal, "options.debug", p.Debug); err != nil {
		slog.Warn("ws: failed to persist debug setting", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// ── Context paths ─────────────────────────────────────────────────────────────

func handleAddContextPath(a *appPkg.App, c *Client, msg WSMessage) {
	var p AddContextPathPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.Path == "" {
		c.reply(msg.ID, EventError, nil, "path is required")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.Options == nil {
		cfg.Options = &config.Options{}
	}
	if slices.Contains(cfg.Options.ContextPaths, p.Path) {
		c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	cfg.Options.ContextPaths = append(cfg.Options.ContextPaths, p.Path)
	if err := store.SetConfigField(config.ScopeGlobal, "options.context_paths", cfg.Options.ContextPaths); err != nil {
		slog.Warn("ws: failed to persist context paths", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveContextPath(a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveContextPathPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.Options == nil {
		c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	cfg.Options.ContextPaths = slices.DeleteFunc(cfg.Options.ContextPaths, func(s string) bool { return s == p.Path })
	if err := store.SetConfigField(config.ScopeGlobal, "options.context_paths", cfg.Options.ContextPaths); err != nil {
		slog.Warn("ws: failed to persist context paths", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// ── Skills paths ──────────────────────────────────────────────────────────────

func handleGetSkills(a *appPkg.App, c *Client, msg WSMessage) {
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	paths := []string{}
	if cfg.Options != nil {
		paths = cfg.Options.SkillsPaths
	}
	discovered := skills.Discover(paths)
	commands := skills.DiscoverCommands(skills.DefaultCommandDirs())
	all := append(discovered, commands...)
	infos := make([]SkillInfo, 0, len(all))
	for _, s := range all {
		infos = append(infos, SkillInfo{
			Name:         s.Name,
			Description:  s.Description,
			Path:         s.Path,
			Source:       s.Source,
			Instructions: s.Instructions,
		})
	}
	c.reply(msg.ID, EventSkills, SkillsSnapshot{Skills: infos, Paths: paths}, "")
}

func handleAddSkillsPath(a *appPkg.App, c *Client, msg WSMessage) {
	var p AddSkillsPathPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.Path == "" {
		c.reply(msg.ID, EventError, nil, "path is required")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.Options == nil {
		cfg.Options = &config.Options{}
	}
	if slices.Contains(cfg.Options.SkillsPaths, p.Path) {
		c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	cfg.Options.SkillsPaths = append(cfg.Options.SkillsPaths, p.Path)
	if err := store.SetConfigField(config.ScopeGlobal, "options.skills_paths", cfg.Options.SkillsPaths); err != nil {
		slog.Warn("ws: failed to persist skills paths", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveSkillsPath(a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveSkillsPathPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.Options == nil {
		c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	cfg.Options.SkillsPaths = slices.DeleteFunc(cfg.Options.SkillsPaths, func(s string) bool { return s == p.Path })
	if err := store.SetConfigField(config.ScopeGlobal, "options.skills_paths", cfg.Options.SkillsPaths); err != nil {
		slog.Warn("ws: failed to persist skills paths", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// ── Project initialization ────────────────────────────────────────────────────

func handleInitializeProject(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}

	initPrompt, err := agent.InitializePrompt(store)
	if err != nil {
		c.reply(msg.ID, EventError, nil, "failed to build initialization prompt: "+err.Error())
		return
	}

	// Create a dedicated initialization session.
	sess, err := a.Sessions.Create(ctx, "Project Initialization")
	if err != nil {
		c.reply(msg.ID, EventError, nil, "failed to create session: "+err.Error())
		return
	}

	// Set default models from config.
	cfg := store.Config()
	if large, ok := cfg.Models[config.SelectedModelTypeLarge]; ok {
		_ = a.Sessions.UpdateModels(ctx, sess.ID, large.Provider, large.Model, "", "")
	}

	// Build and save the system prompt.
	if sp, buildErr := a.AgentCoordinator.BuildSystemPrompt(ctx); buildErr == nil && sp != "" {
		_ = a.AgentCoordinator.UpdateSessionSystemPrompt(ctx, sess.ID, sp)
	}

	// Broadcast the new session before replying so the client can navigate.
	if updated, fetchErr := a.Sessions.Get(ctx, sess.ID); fetchErr == nil {
		c.hub.Broadcast(EventSessionCreated, updated)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok", "sessionID": sess.ID}, "")

	// Run the agent in a background context so closing the tab won't cancel it.
	agentCtx := context.WithoutCancel(ctx)
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: sess.ID, Busy: true})
	_, runErr := a.AgentCoordinator.Run(agentCtx, sess.ID, initPrompt)
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: sess.ID, Busy: false})
	if runErr != nil {
		slog.Error("ws: initialization run error", "err", runErr)
	}
	_ = config.MarkProjectInitialized(a.Store())
}

// ── Custom providers ──────────────────────────────────────────────────────────

func handleAddCustomProvider(a *appPkg.App, c *Client, msg WSMessage) {
	var p AddCustomProviderPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.ID == "" || p.BaseURL == "" {
		c.reply(msg.ID, EventError, nil, "id and baseUrl are required")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if _, exists := cfg.Providers.Get(p.ID); exists {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("provider %q already exists", p.ID))
		return
	}
	models := make([]catwalk.Model, len(p.Models))
	for i, m := range p.Models {
		models[i] = catwalk.Model{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.ContextWindow,
			CostPer1MIn:   m.CostPer1MIn,
			CostPer1MOut:  m.CostPer1MOut,
		}
	}
	providerCfg := config.ProviderConfig{
		ID:      p.ID,
		Name:    cmp.Or(p.Name, p.ID),
		Type:    catwalk.Type(cmp.Or(p.Type, "openai-compat")),
		BaseURL: p.BaseURL,
		APIKey:  p.APIKey,
		Models:  models,
	}
	cfg.Providers.Set(p.ID, providerCfg)
	if err := store.SetConfigField(config.ScopeGlobal, fmt.Sprintf("providers.%s", p.ID), providerCfg); err != nil {
		slog.Warn("ws: failed to persist custom provider", "id", p.ID, "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveCustomProvider(a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveCustomProviderPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.ID == "" {
		c.reply(msg.ID, EventError, nil, "id is required")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	cfg.Providers.Del(p.ID)
	if err := store.RemoveConfigField(config.ScopeGlobal, fmt.Sprintf("providers.%s", p.ID)); err != nil {
		slog.Warn("ws: failed to remove custom provider", "id", p.ID, "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateCustomProvider(a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateCustomProviderPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.OldID == "" || p.ID == "" || p.BaseURL == "" {
		c.reply(msg.ID, EventError, nil, "oldId, id and baseUrl are required")
		return
	}
	store := a.Store()
	cfg := store.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	// Remove the old entry.
	cfg.Providers.Del(p.OldID)
	if p.OldID != p.ID {
		if err := store.RemoveConfigField(config.ScopeGlobal, fmt.Sprintf("providers.%s", p.OldID)); err != nil {
			slog.Warn("ws: failed to remove old custom provider", "id", p.OldID, "err", err)
		}
	}
	// Build updated models.
	models := make([]catwalk.Model, len(p.Models))
	for i, m := range p.Models {
		models[i] = catwalk.Model{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.ContextWindow,
			CostPer1MIn:   m.CostPer1MIn,
			CostPer1MOut:  m.CostPer1MOut,
		}
	}
	providerCfg := config.ProviderConfig{
		ID:      p.ID,
		Name:    cmp.Or(p.Name, p.ID),
		Type:    catwalk.Type(cmp.Or(p.Type, "openai-compat")),
		BaseURL: p.BaseURL,
		APIKey:  p.APIKey,
		Models:  models,
	}
	cfg.Providers.Set(p.ID, providerCfg)
	if err := store.SetConfigField(config.ScopeGlobal, fmt.Sprintf("providers.%s", p.ID), providerCfg); err != nil {
		slog.Warn("ws: failed to persist updated custom provider", "id", p.ID, "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// ── Todos ─────────────────────────────────────────────────────────────────────

func handleUpdateTodos(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateTodosPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	sess, err := a.Sessions.Get(ctx, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, "session not found")
		return
	}
	todos := make([]session.Todo, len(p.Todos))
	for i, t := range p.Todos {
		todos[i] = session.Todo{
			Content:    t.Content,
			Status:     session.TodoStatus(t.Status),
			ActiveForm: t.ActiveForm,
		}
	}
	prev := sess.Todos
	sess.Todos = todos
	slog.Info(
		"ws: user updated todos",
		"session", p.SessionID,
		"prev_count", len(prev),
		"new_count", len(todos),
	)
	if _, err := a.Sessions.Save(ctx, sess); err != nil {
		c.reply(msg.ID, EventError, nil, "failed to save todos")
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// handleRerunMessage is an atomic "retry from this user message": it cancels
// any in-flight agent run, waits for idle, deletes every message created AFTER
// the target user message, then deletes the target itself and re-runs the agent
// with the same prompt. Run() creates a fresh user message so the history reads
// naturally. All steps happen in one goroutine — no client-side race.
func handleRerunMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p RerunMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	targetMsg, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, "message not found")
		return
	}
	if targetMsg.Role != message.User {
		c.reply(msg.ID, EventError, nil, "can only rerun user messages")
		return
	}

	text := targetMsg.Content().Text
	if text == "" {
		c.reply(msg.ID, EventError, nil, "empty message")
		return
	}

	sessionID := targetMsg.SessionID
	slog.Info("ws: handleRerunMessage", "sessionID", sessionID, "messageID", p.MessageID,
		"contentPreview", text[:min(len(text), 80)])

	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}

	// 1. Cancel + clear queue if busy, then poll until idle (up to 10s).
	a.AgentCoordinator.Cancel(sessionID)
	a.AgentCoordinator.ClearQueue(sessionID)
	for i := 0; i < 100; i++ {
		if !a.AgentCoordinator.IsSessionBusy(sessionID) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 2. Delete every message AFTER the target (by CreatedAt), keep the target.
	allMsgs, listErr := a.Messages.List(ctx, sessionID)
	if listErr != nil {
		c.reply(msg.ID, EventError, nil, "failed to list messages")
		return
	}
	for _, m := range allMsgs {
		if m.CreatedAt > targetMsg.CreatedAt ||
			(m.CreatedAt == targetMsg.CreatedAt && m.ID != targetMsg.ID) {
			if delErr := a.Messages.Delete(ctx, m.ID); delErr != nil {
				slog.Warn("ws: rerun: failed to delete tail message", "id", m.ID, "err", delErr)
			}
		}
	}

	// 3. Delete the original user message — Run() will recreate it.
	if delErr := a.Messages.Delete(ctx, targetMsg.ID); delErr != nil {
		slog.Warn("ws: rerun: failed to delete original user message", "id", targetMsg.ID, "err", delErr)
	}

	// 4. Re-arm Phase 4 autonomy.
	a.AgentCoordinator.ResetAutoResumeCounter(sessionID)

	// 5. Resolve model overrides (same priority as handleSendMessage).
	var largeOverride, smallOverride *agent.ModelOverride
	if sess, sessErr := a.Sessions.Get(ctx, sessionID); sessErr == nil {
		if sess.LargeModelID != "" {
			largeOverride = &agent.ModelOverride{Provider: sess.LargeModelProvider, Model: sess.LargeModelID}
		}
		if sess.SmallModelID != "" {
			smallOverride = &agent.ModelOverride{Provider: sess.SmallModelProvider, Model: sess.SmallModelID}
		}
	}

	// 6. Run the agent with the same prompt.
	agentCtx := context.WithoutCancel(ctx)
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: sessionID, Busy: true})
	if largeOverride != nil || smallOverride != nil {
		_, err = a.AgentCoordinator.RunWithOverrides(agentCtx, sessionID, text, largeOverride, smallOverride)
	} else {
		_, err = a.AgentCoordinator.Run(agentCtx, sessionID, text)
	}
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: sessionID, Busy: false})

	if err != nil {
		slog.Error("ws: rerun agent error", "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}
