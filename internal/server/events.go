package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/lsp"
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

	// LSP state changes — broadcast a full snapshot on each event.
	go func() {
		ch := appPkg.SubscribeLSPEvents(ctx)
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
				h.Broadcast(EventLSPState, buildLSPSnapshot(a.Config()))
			case <-ctx.Done():
				return
			}
		}
	}()

	// Send initial LSP snapshot so new clients see configured servers immediately.
	h.Broadcast(EventLSPState, buildLSPSnapshot(a.Config()))

	slog.Debug("ws: event subscriptions started")

	// Unused imports guard — referenced through the generic channels above.
	_ = session.Session{}
	_ = message.Message{}
	_ = permission.PermissionRequest{}
	_ = history.File{}
}

// buildLSPSnapshot returns the current state of all configured LSP servers in wire format.
func buildLSPSnapshot(cfg *config.Config) LSPSnapshot {
	states := appPkg.GetLSPStates()
	servers := make([]LSPServerInfo, 0)

	if cfg == nil {
		return LSPSnapshot{Servers: servers}
	}

	for name, lspCfg := range cfg.LSP {
		info := LSPServerInfo{
			Name:      name,
			Disabled:  lspCfg.Disabled,
			Command:   lspCfg.Command,
			Args:      lspCfg.Args,
			Env:       lspCfg.Env,
			FileTypes: lspCfg.FileTypes,
		}
		if clientInfo, ok := states[name]; ok {
			info.State = serverStateString(clientInfo.State)
			info.DiagnosticCount = clientInfo.DiagnosticCount
		} else if lspCfg.Disabled {
			info.State = "disabled"
		} else {
			info.State = "unstarted"
		}
		servers = append(servers, info)
	}

	return LSPSnapshot{Servers: servers}
}

func serverStateString(state lsp.ServerState) string {
	switch state {
	case lsp.StateUnstarted:
		return "unstarted"
	case lsp.StateStarting:
		return "starting"
	case lsp.StateReady:
		return "ready"
	case lsp.StateError:
		return "error"
	case lsp.StateStopped:
		return "stopped"
	case lsp.StateDisabled:
		return "disabled"
	default:
		return "unknown"
	}
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
				srv.Source = string(mcpCfg.Source)
			}
		}
		servers = append(servers, srv)
	}
	return MCPSnapshot{Servers: servers}
}
