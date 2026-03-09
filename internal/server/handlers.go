package server

import (
	"context"
	"encoding/json"
	"log/slog"

	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
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
	case CmdGetSystemPrompt:
		go handleGetSystemPrompt(ctx, a, c, msg)
	case CmdSetSystemPrompt:
		go handleSetSystemPrompt(ctx, a, c, msg)
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

	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: true})
	var err error
	if largeOverride != nil || smallOverride != nil {
		_, err = a.AgentCoordinator.RunWithOverrides(ctx, p.SessionID, p.Content, largeOverride, smallOverride, attachments...)
	} else {
		_, err = a.AgentCoordinator.Run(ctx, p.SessionID, p.Content, attachments...)
	}
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: false})

	if err != nil {
		slog.Error("ws: agent run error", "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
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
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleSetYolo(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetYoloPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
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
			pw := ProviderWire{Name: p.Name, Enabled: true, Type: string(p.Type), Models: make([]ModelInfoWire, len(ep.Models))}
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
			pw := ProviderWire{Name: ep.Name, Enabled: true, Type: string(ep.Type), Models: make([]ModelInfoWire, len(ep.Models))}
			for i, m := range ep.Models {
				pw.Models[i] = ModelInfoWire{ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow}
			}
			wire.Providers[ep.ID] = pw
		}
	}

	wire.Yolo = a.Permissions.SkipRequests()

	for _, m := range cfg.RecentModels[config.SelectedModelTypeLarge] {
		wire.RecentLargeModels = append(wire.RecentLargeModels, ModelEntryWire{Provider: m.Provider, Model: m.Model})
	}
	for _, m := range cfg.RecentModels[config.SelectedModelTypeSmall] {
		wire.RecentSmallModels = append(wire.RecentSmallModels, ModelEntryWire{Provider: m.Provider, Model: m.Model})
	}

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
	c.reply(msg.ID, EventResponse, map[string]string{"theme": p.Theme}, "")
}
