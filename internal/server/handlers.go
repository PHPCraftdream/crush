package server

import (
	"context"
	"encoding/json"
	"log/slog"

	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/agent"
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

	var largeOverride, smallOverride *agent.ModelOverride
	if p.LargeModel != nil {
		largeOverride = &agent.ModelOverride{Provider: p.LargeModel.Provider, Model: p.LargeModel.Model}
	}
	if p.SmallModel != nil {
		smallOverride = &agent.ModelOverride{Provider: p.SmallModel.Provider, Model: p.SmallModel.Model}
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

func handleGetConfig(a *appPkg.App, c *Client, msg WSMessage) {
	cfg := a.Config()
	if cfg == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	// Build wire format with PascalCase keys matching the frontend's ConfigPayload type.
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
	for _, p := range cfg.EnabledProviders() {
		pw := ProviderWire{Models: make([]ModelInfoWire, len(p.Models))}
		for i, m := range p.Models {
			pw.Models[i] = ModelInfoWire{ID: m.ID, Name: m.Name}
		}
		wire.Providers[p.ID] = pw
	}
	c.reply(msg.ID, EventConfig, wire, "")
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
