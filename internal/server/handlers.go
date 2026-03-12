package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/cliprovider"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
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
	case CmdCancelAgent:
		go handleCancelAgent(ctx, a, c, msg)
	case CmdCreateSession:
		go handleCreateSession(ctx, a, c, msg)
	case CmdForkSession:
		go handleForkSession(ctx, a, c, msg)
	case CmdDeleteSession:
		go handleDeleteSession(ctx, a, c, msg)
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
	case CmdSetTheme:
		go handleSetTheme(a, c, msg)
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
	case CmdSetLSPDisabled:
		go handleSetLSPDisabled(ctx, a, c, msg)
	case CmdAddLSPServer:
		go handleAddLSPServer(a, c, msg)
	case CmdRemoveLSPServer:
		go handleRemoveLSPServer(ctx, a, c, msg)
	case CmdUpdateLSPServer:
		go handleUpdateLSPServer(ctx, a, c, msg)
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

func handleSendMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SendMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	slog.Info("ws: handleSendMessage", "sessionID", p.SessionID, "content", p.Content)

	var attachments []message.Attachment
	for _, att := range p.Attachments {
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

func handleSetSessionModels(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SetSessionModelsPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	slog.Info("ws: handleSetSessionModels", "sessionID", p.SessionID, "large", p.LargeModel, "small", p.SmallModel)

	var lp, lm, sp, sm string
	if p.LargeModel != nil {
		lp, lm = p.LargeModel.Provider, p.LargeModel.Model
	}
	if p.SmallModel != nil {
		sp, sm = p.SmallModel.Provider, p.SmallModel.Model
	}

	if err := a.Sessions.UpdateModels(ctx, p.SessionID, lp, lm, sp, sm); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}

	// Record recently used models in the config (persists across restarts)
	cfg := a.Config()
	if cfg != nil && lp != "" && lm != "" {
		if err := cfg.RecordRecentModel(config.SelectedModelTypeLarge, config.SelectedModel{Provider: lp, Model: lm}); err != nil {
			slog.Warn("ws: failed to record recent large model", "err", err)
		}
	}
	if cfg != nil && sp != "" && sm != "" {
		if err := cfg.RecordRecentModel(config.SelectedModelTypeSmall, config.SelectedModel{Provider: sp, Model: sm}); err != nil {
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
	cfg := a.Config()
	if cfg == nil {
		return
	}
	modelType := config.SelectedModelType(p.ModelType)
	if err := cfg.RemoveRecentModel(modelType, config.SelectedModel{Provider: p.Provider, Model: p.Model}); err != nil {
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
	cfg := a.Config()
	if cfg == nil {
		return
	}
	modelType := config.SelectedModelType(p.ModelType)
	// Use UpdatePreferredModel which handles both preferred model and recent models tracking
	if err := cfg.UpdatePreferredModel(modelType, config.SelectedModel{Provider: p.Provider, Model: p.Model}); err != nil {
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
		_ = a.Sessions.UpdateModels(ctx, fork.ID,
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

func handleListSessions(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	sessions, err := a.Sessions.List(ctx)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if sessions == nil {
		sessions = []session.Session{}
	}
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
	c.reply(msg.ID, EventMessagesList, toMessagesWire(msgs), "")
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
	cfg := a.Config()
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

	for _, p := range cfg.KnownProviders() {
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
		wire.DebugLSP = cfg.Options.DebugLSP
		wire.ContextPaths = cfg.Options.ContextPaths
		wire.SkillsPaths = cfg.Options.SkillsPaths
		wire.InitializeAs = cfg.Options.InitializeAs
	}
	if cfg.Options != nil && cfg.Options.TUI != nil {
		wire.Theme = cfg.Options.TUI.Theme
	}

	for _, m := range cfg.RecentModels[config.SelectedModelTypeLarge] {
		wire.RecentLargeModels = append(wire.RecentLargeModels, ModelEntryWire{Provider: m.Provider, Model: m.Model})
	}
	for _, m := range cfg.RecentModels[config.SelectedModelTypeSmall] {
		wire.RecentSmallModels = append(wire.RecentSmallModels, ModelEntryWire{Provider: m.Provider, Model: m.Model})
	}

	wire.Version = version.FullVersion()

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

func handleSetProviderKey(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetProviderKeyPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if err := cfg.SetProviderAPIKey(p.ProviderID, p.APIKey); err != nil {
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
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if err := cfg.RemoveProviderAPIKey(p.ProviderID); err != nil {
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
	if err := a.Config().SetTheme(p.Theme); err != nil {
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
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	var err error
	if p.Disabled {
		err = mcp.DisableServer(ctx, cfg, p.Name)
	} else {
		err = mcp.EnableServer(ctx, cfg, p.Name)
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
	cfg := a.Config()
	if cfg == nil {
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
	if err := mcp.AddServer(ctx, cfg, p.Name, mcpCfg); err != nil {
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
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if err := mcp.RemoveServer(cfg, p.Name); err != nil {
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
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	// Remove old entry
	if err := mcp.RemoveServer(cfg, p.OldName); err != nil {
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
	if err := mcp.AddServer(ctx, cfg, p.Name, mcpCfg); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleSetLSPDisabled(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SetLSPDisabledPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	lspCfg, ok := cfg.LSP[p.Name]
	if !ok {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("LSP server %q not found", p.Name))
		return
	}
	lspCfg.Disabled = p.Disabled
	cfg.LSP[p.Name] = lspCfg
	if err := cfg.SetConfigField(fmt.Sprintf("lsp.%s.disabled", p.Name), p.Disabled); err != nil {
		slog.Warn("ws: failed to persist LSP disabled state", "name", p.Name, "err", err)
	}
	if p.Disabled {
		a.LSPManager.UnregisterServer(ctx, p.Name)
	} else {
		a.LSPManager.RegisterServer(p.Name, lspCfg)
	}
	c.hub.Broadcast(EventLSPState, buildLSPSnapshot(cfg))
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleAddLSPServer(a *appPkg.App, c *Client, msg WSMessage) {
	var p AddLSPServerPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.Name == "" || p.Command == "" {
		c.reply(msg.ID, EventError, nil, "name and command are required")
		return
	}
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.LSP == nil {
		cfg.LSP = make(config.LSPs)
	}
	if _, exists := cfg.LSP[p.Name]; exists {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("LSP server %q already exists", p.Name))
		return
	}
	lspCfg := config.LSPConfig{
		Command:   p.Command,
		Args:      p.Args,
		Env:       p.Env,
		FileTypes: p.FileTypes,
		Timeout:   p.Timeout,
	}
	cfg.LSP[p.Name] = lspCfg
	if err := cfg.SetConfigField(fmt.Sprintf("lsp.%s", p.Name), lspCfg); err != nil {
		slog.Warn("ws: failed to persist new LSP server", "name", p.Name, "err", err)
	}
	a.LSPManager.RegisterServer(p.Name, lspCfg)
	c.hub.Broadcast(EventLSPState, buildLSPSnapshot(cfg))
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// ── Debug settings ────────────────────────────────────────────────────────────

func handleSetDebug(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetDebugPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.Options == nil {
		cfg.Options = &config.Options{}
	}
	cfg.Options.Debug = p.Debug
	cfg.Options.DebugLSP = p.DebugLSP
	if err := cfg.SetConfigField("options.debug", p.Debug); err != nil {
		slog.Warn("ws: failed to persist debug setting", "err", err)
	}
	if err := cfg.SetConfigField("options.debug_lsp", p.DebugLSP); err != nil {
		slog.Warn("ws: failed to persist debug_lsp setting", "err", err)
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
	cfg := a.Config()
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
	if err := cfg.SetConfigField("options.context_paths", cfg.Options.ContextPaths); err != nil {
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
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.Options == nil {
		c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	cfg.Options.ContextPaths = slices.DeleteFunc(cfg.Options.ContextPaths, func(s string) bool { return s == p.Path })
	if err := cfg.SetConfigField("options.context_paths", cfg.Options.ContextPaths); err != nil {
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
	cfg := a.Config()
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
	if err := cfg.SetConfigField("options.skills_paths", cfg.Options.SkillsPaths); err != nil {
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
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if cfg.Options == nil {
		c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	cfg.Options.SkillsPaths = slices.DeleteFunc(cfg.Options.SkillsPaths, func(s string) bool { return s == p.Path })
	if err := cfg.SetConfigField("options.skills_paths", cfg.Options.SkillsPaths); err != nil {
		slog.Warn("ws: failed to persist skills paths", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// ── Project initialization ────────────────────────────────────────────────────

func handleInitializeProject(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}

	initPrompt, err := agent.InitializePrompt(*cfg)
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
	_ = config.MarkProjectInitialized(cfg)
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
	cfg := a.Config()
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
	if err := cfg.SetConfigField(fmt.Sprintf("providers.%s", p.ID), providerCfg); err != nil {
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
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	cfg.Providers.Del(p.ID)
	if err := cfg.RemoveConfigField(fmt.Sprintf("providers.%s", p.ID)); err != nil {
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
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	// Remove the old entry.
	cfg.Providers.Del(p.OldID)
	if p.OldID != p.ID {
		if err := cfg.RemoveConfigField(fmt.Sprintf("providers.%s", p.OldID)); err != nil {
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
	if err := cfg.SetConfigField(fmt.Sprintf("providers.%s", p.ID), providerCfg); err != nil {
		slog.Warn("ws: failed to persist updated custom provider", "id", p.ID, "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}
func handleRemoveLSPServer(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveLSPServerPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if _, exists := cfg.LSP[p.Name]; !exists {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("LSP server %q not found", p.Name))
		return
	}
	a.LSPManager.UnregisterServer(ctx, p.Name)
	delete(cfg.LSP, p.Name)
	if err := cfg.RemoveConfigField(fmt.Sprintf("lsp.%s", p.Name)); err != nil {
		slog.Warn("ws: failed to remove LSP from config", "name", p.Name, "err", err)
	}
	c.hub.Broadcast(EventLSPState, buildLSPSnapshot(cfg))
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateLSPServer(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateLSPServerPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.OldName == "" || p.Name == "" || p.Command == "" {
		c.reply(msg.ID, EventError, nil, "oldName, name and command are required")
		return
	}
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	// Remove old entry
	a.LSPManager.UnregisterServer(ctx, p.OldName)
	delete(cfg.LSP, p.OldName)
	if err := cfg.RemoveConfigField(fmt.Sprintf("lsp.%s", p.OldName)); err != nil {
		slog.Warn("ws: failed to remove old LSP from config", "name", p.OldName, "err", err)
	}
	// Add new entry
	lspCfg := config.LSPConfig{
		Command:   p.Command,
		Args:      p.Args,
		Env:       p.Env,
		FileTypes: p.FileTypes,
		Timeout:   p.Timeout,
	}
	if cfg.LSP == nil {
		cfg.LSP = make(config.LSPs)
	}
	cfg.LSP[p.Name] = lspCfg
	if err := cfg.SetConfigField(fmt.Sprintf("lsp.%s", p.Name), lspCfg); err != nil {
		slog.Warn("ws: failed to persist updated LSP server", "name", p.Name, "err", err)
	}
	a.LSPManager.RegisterServer(p.Name, lspCfg)
	c.hub.Broadcast(EventLSPState, buildLSPSnapshot(cfg))
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
	slog.Info("ws: user updated todos",
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
