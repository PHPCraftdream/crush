package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
)

const batchInterval = 16 * time.Millisecond

// subscribeAndBroadcast subscribes to all app event sources and forwards them
// to the hub, batching high-frequency message updates the same way the TUI does.
func subscribeAndBroadcast(ctx context.Context, a *appPkg.App, h *Hub) {
	// Sessions
	go func() {
		ch := a.Sessions.Subscribe(ctx)
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				switch ev.Type {
				case pubsub.CreatedEvent:
					h.Broadcast(EventSessionCreated, ev.Payload)
				case pubsub.UpdatedEvent:
					h.Broadcast(EventSessionUpdated, ev.Payload)
				case pubsub.DeletedEvent:
					h.Broadcast(EventSessionDeleted, ev.Payload)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Messages — batch streaming updates to avoid flooding the client.
	go func() {
		ch := a.Messages.Subscribe(ctx)
		ticker := time.NewTicker(batchInterval)
		defer ticker.Stop()

		// pending holds the latest update per message ID within the current batch.
		pending := make(map[string]message.Message)

		flush := func() {
			for _, msg := range pending {
				h.Broadcast(EventMessageUpdated, toMessageWire(msg))
			}
			clear(pending)
		}

		for {
			select {
			case <-ctx.Done():
				flush()
				return
			case ev, ok := <-ch:
				if !ok {
					flush()
					return
				}
				switch ev.Type {
				case pubsub.CreatedEvent:
					h.Broadcast(EventMessageCreated, toMessageWire(ev.Payload))
				case pubsub.UpdatedEvent:
					// Deduplicate: keep only the latest update per message ID.
					pending[ev.Payload.ID] = ev.Payload
				case pubsub.DeletedEvent:
					delete(pending, ev.Payload.ID)
					h.Broadcast(EventMessageDeleted, ev.Payload)
				}
			case <-ticker.C:
				flush()
			}
		}
	}()

	// Permission requests
	go func() {
		ch := a.Permissions.Subscribe(ctx)
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				h.Broadcast(EventPermissionRequest, ev.Payload)
			case <-ctx.Done():
				return
			}
		}
	}()

	// Permission notifications
	go func() {
		ch := a.Permissions.SubscribeNotifications(ctx)
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				h.Broadcast(EventPermissionNotification, ev.Payload)
			case <-ctx.Done():
				return
			}
		}
	}()

	// File history
	go func() {
		ch := a.History.Subscribe(ctx)
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				_ = ev // cast to concrete type if needed
				h.Broadcast(EventFileUpdated, ev.Payload)
			case <-ctx.Done():
				return
			}
		}
	}()

	// MCP state changes — broadcast a full snapshot of all servers on each event.
	go func() {
		ch := mcp.SubscribeEvents(ctx)
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
				h.Broadcast(EventMCPState, buildMCPSnapshot(a.Config()))
			case <-ctx.Done():
				return
			}
		}
	}()

	// LSP state changes
	go func() {
		ch := appPkg.SubscribeLSPEvents(ctx)
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				h.Broadcast(EventLSPState, ev.Payload)
			case <-ctx.Done():
				return
			}
		}
	}()

	slog.Debug("ws: event subscriptions started")

	// Unused imports guard — referenced through the generic channels above.
	_ = session.Session{}
	_ = message.Message{}
	_ = permission.PermissionRequest{}
	_ = history.File{}
}

// buildMCPSnapshot returns the current state of all MCP servers in wire format.
func buildMCPSnapshot(cfg *config.Config) MCPSnapshot {
	all := mcp.GetStates()
	servers := make([]MCPServerInfo, 0, len(all))
	for name, info := range all {
		srv := MCPServerInfo{
			Name:      name,
			Status:    info.State.String(),
			Disabled:  info.State == mcp.StateDisabled,
			ToolCount: info.Counts.Tools,
			Tools:     mcp.GetServerToolNames(name),
		}
		if cfg != nil {
			if mcpCfg, ok := cfg.MCP[name]; ok {
				srv.ServerType = string(mcpCfg.Type)
				srv.Command = mcpCfg.Command
				srv.Args = mcpCfg.Args
				srv.URL = mcpCfg.URL
				srv.Env = mcpCfg.Env
				srv.Headers = mcpCfg.Headers
			}
		}
		servers = append(servers, srv)
	}
	return MCPSnapshot{Servers: servers}
}
