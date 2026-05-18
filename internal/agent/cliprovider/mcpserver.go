package cliprovider

// crushMCPServer starts an in-process MCP HTTP server that exposes crush's
// core tools (bash, view, write, edit, glob, grep) to an external CLI process
// (e.g. the claude CLI). Each server instance generates a random Bearer token
// so only the CLI process spawned by crush can connect.
//
// Usage:
//  1. Create the server with newCrushMCPServer.
//  2. Write mcpConfigFile() to a temp file and pass it to the claude CLI via
//     the --mcp-config flag.
//  3. Call stop() when the CLI process exits to free the port.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpToolEvent is emitted by the MCP server when a tool call starts or ends.
// id is a UUID; name is non-empty for start events, empty for end events.
type mcpToolEvent struct {
	id    string
	name  string // non-empty = start event; empty = end event
	input string // JSON-encoded input (start events only)
}

// crushMCPServer is an in-process MCP HTTP server with token auth.
// The token is accepted via Authorization: Bearer header (Claude CLI)
// or as a ?token= query parameter (Qwen CLI, which cannot set headers).
type crushMCPServer struct {
	addr    string // "127.0.0.1:PORT"
	token   string
	httpSrv *http.Server
	// toolCh receives tool-call notifications from MCP handlers so the
	// Stream scan loop can emit ToolInputStart/Delta/End stream parts.
	toolCh chan mcpToolEvent
}

// stop shuts down the HTTP server.
func (s *crushMCPServer) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.httpSrv.Shutdown(ctx); err != nil {
		slog.Debug("cliprovider: MCP server shutdown error", "err", err)
	}
}

// mcpConfigJSON returns the JSON bytes of the MCP server config suitable for
// writing to a temp file and passing to the claude CLI via --mcp-config.
func (s *crushMCPServer) mcpConfigJSON() ([]byte, error) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"crush": map[string]any{
				"type": "http",
				"url":  "http://" + s.addr + "/mcp",
				"headers": map[string]string{
					"Authorization": "Bearer " + s.token,
				},
			},
		},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// mcpURL returns the URL with the token embedded as a query parameter,
// for clients (e.g. qwen CLI) that cannot set custom HTTP headers.
func (s *crushMCPServer) mcpURL() string {
	return "http://" + s.addr + "/mcp?token=" + s.token
}

// newCrushMCPServer starts a local MCP HTTP server and returns it.
// The server exposes crush's core tools; each tool call goes through
// perms.Request before execution so crush's permission dialog appears.
// The token is accepted via Authorization: Bearer header OR ?token= query param.
// If token is empty a cryptographically random one is generated.
// sessions and sessionID are used by the todos tool to persist task updates.
func newCrushMCPServer(ctx context.Context, perms permission.Service, sessions session.Service, sessionID string, workingDir string, token string, mcpProxy ExternalMCPProxy) (*crushMCPServer, error) {
	if token == "" {
		// 32-byte random token → 64-char hex string.
		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			return nil, fmt.Errorf("cliprovider: generate MCP token: %w", err)
		}
		token = hex.EncodeToString(tokenBytes)
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "crush",
		Title:   "Crush",
		Version: "1.0",
	}, nil)

	toolCh := make(chan mcpToolEvent, 32)
	registerMCPTools(srv, perms, sessions, sessionID, workingDir, toolCh)
	if mcpProxy != nil {
		registerExternalMCPTools(ctx, srv, perms, workingDir, mcpProxy, toolCh)
	}

	rawHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			// Stateless mode: each POST creates a temporary session.
			// Simple and sufficient for single-agent CLI use.
			Stateless: true,
		},
	)

	// Auth middleware: accept token via Authorization header or ?token= query param.
	bearer := "Bearer " + token
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != bearer && r.URL.Query().Get("token") != token {
			slog.Debug("cliprovider: MCP request rejected — bad token",
				"remote", r.RemoteAddr, "path", r.URL.Path)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		slog.Debug("cliprovider: MCP request", "method", r.Method, "path", r.URL.Path)
		rawHandler.ServeHTTP(w, r)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("cliprovider: start MCP listener: %w", err)
	}

	httpSrv := &http.Server{Handler: http.StripPrefix("/mcp", handler)}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Debug("cliprovider: MCP server stopped", "err", err)
		}
	}()

	addr := ln.Addr().String()
	slog.Info("cliprovider: MCP server started", "addr", addr)
	return &crushMCPServer{
		addr:    addr,
		token:   token,
		httpSrv: httpSrv,
		toolCh:  toolCh,
	}, nil
}

// registerMCPTools adds crush tool implementations to the MCP server.
// Each tool requests permission via perms.Request before executing.
// toolCh, if non-nil, receives start/end notifications for each tool call.
func registerMCPTools(srv *mcp.Server, perms permission.Service, sessions session.Service, sessionID string, workingDir string, toolCh chan mcpToolEvent) {
	registerBashTool(srv, perms, workingDir, toolCh)
	registerViewTool(srv, perms, workingDir, toolCh)
	registerWriteTool(srv, perms, workingDir, toolCh)
	registerGlobTool(srv, perms, workingDir, toolCh)
	registerGrepTool(srv, perms, workingDir, toolCh)
	if sessions != nil && sessionID != "" {
		registerTodosTool(srv, sessions, sessionID)
	}
}

// emitToolStart sends a tool-call start notification to toolCh if non-nil.
func emitToolStart(toolCh chan mcpToolEvent, id, name, inputJSON string) {
	if toolCh == nil {
		return
	}
	select {
	case toolCh <- mcpToolEvent{id: id, name: name, input: inputJSON}:
	default:
		slog.Debug("cliprovider: toolCh full, dropping start event", "tool", name)
	}
}

// emitToolEnd sends a tool-call end notification to toolCh if non-nil.
func emitToolEnd(toolCh chan mcpToolEvent, id string) {
	if toolCh == nil {
		return
	}
	select {
	case toolCh <- mcpToolEvent{id: id}:
	default:
		slog.Debug("cliprovider: toolCh full, dropping end event", "id", id)
	}
}

// registerExternalMCPTools exposes all enabled external MCP tools (from the
// internal mcp package) on the crush MCP HTTP server, so CLI models can call
// them. Tool names are prefixed with the server name to avoid collisions.
// Each tool call goes through perms.Request so the user can approve/deny it
// in the crush UI (or auto-approve in yolo mode).
func registerExternalMCPTools(ctx context.Context, srv *mcp.Server, perms permission.Service, workingDir string, proxy ExternalMCPProxy, toolCh chan mcpToolEvent) {
	for _, ext := range proxy.ListTools() {
		ext := ext // capture
		toolName := ext.ServerName + "__" + ext.Name

		// Build the InputSchema as json.RawMessage from the external tool's schema.
		var rawSchema json.RawMessage
		if ext.InputSchema != nil {
			if b, err := json.Marshal(ext.InputSchema); err == nil {
				rawSchema = b
			}
		}
		if rawSchema == nil {
			rawSchema = json.RawMessage(`{"type":"object"}`)
		}

		srv.AddTool(&mcp.Tool{
			Name:        toolName,
			Description: ext.Description,
			InputSchema: rawSchema,
		}, func(reqCtx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := uuid.New().String()
			inputJSON := string(req.Params.Arguments)

			emitToolStart(toolCh, id, toolName, inputJSON)
			defer emitToolEnd(toolCh, id)

			// Request permission via crush UI (respects yolo mode).
			if perms != nil {
				var params any
				_ = json.Unmarshal(req.Params.Arguments, &params)
				granted, err := perms.Request(reqCtx, permission.CreatePermissionRequest{
					SessionID:   mcpSessionID,
					ToolCallID:  id,
					ToolName:    "mcp_" + toolName,
					Description: fmt.Sprintf("call %s on MCP server %s", ext.Name, ext.ServerName),
					Action:      "execute",
					Params:      params,
					Path:        workingDir,
				})
				if err != nil {
					return &mcp.CallToolResult{
						Content: []mcp.Content{&mcp.TextContent{Text: "permission request failed: " + err.Error()}},
						IsError: true,
					}, nil
				}
				if !granted {
					return &mcp.CallToolResult{
						Content: []mcp.Content{&mcp.TextContent{Text: "tool call denied by user"}},
						IsError: true,
					}, nil
				}
			}

			result, err := proxy.CallTool(reqCtx, ext.ServerName, ext.Name, inputJSON)
			if err != nil {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "error: " + err.Error()}},
					IsError: true,
				}, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: result}},
			}, nil
		})

		slog.Info("cliprovider: registered external MCP tool", "tool", toolName, "server", ext.ServerName)
	}
}

// ── bash ─────────────────────────────────────────────────────────────────────

type mcpBashInput struct {
	Command     string `json:"command"     description:"Shell command to execute"`
	Description string `json:"description" description:"Brief description of what the command does"`
	WorkingDir  string `json:"working_dir,omitempty" description:"Working directory (defaults to project root)"`
}

func registerBashTool(srv *mcp.Server, perms permission.Service, workingDir string, toolCh chan mcpToolEvent) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Bash",
		Description: "Execute a shell command. Requires user approval.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpBashInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Bash called", "command", input.Command, "description", input.Description)

		wd := workingDir
		if input.WorkingDir != "" {
			wd = input.WorkingDir
		}

		id := uuid.New().String()
		inputJSON, _ := json.Marshal(input)
		emitToolStart(toolCh, id, "Bash", string(inputJSON))
		defer emitToolEnd(toolCh, id)

		granted, err := perms.Request(ctx, permission.CreatePermissionRequest{
			SessionID:   mcpSessionID,
			ToolCallID:  uuid.New().String(),
			ToolName:    "bash",
			Description: input.Description,
			Action:      "run",
			Params:      input,
			Path:        wd,
		})
		if err != nil {
			slog.Debug("cliprovider: MCP Bash permission error", "err", err)
			return toolError("permission request failed: " + err.Error()), nil, nil
		}
		if !granted {
			slog.Debug("cliprovider: MCP Bash denied by user")
			return toolError("command denied by user"), nil, nil
		}

		out, runErr := runShell(ctx, input.Command, wd)
		slog.Debug("cliprovider: MCP Bash executed", "command", input.Command, "output_len", len(out), "err", runErr)
		if runErr != nil {
			return toolError(fmt.Sprintf("command failed: %v\n%s", runErr, out)), nil, nil
		}
		return toolText(out), nil, nil
	})
}

// ── view / read ───────────────────────────────────────────────────────────────

type mcpViewInput struct {
	Path      string `json:"path"                description:"File path to read"`
	StartLine int    `json:"start_line,omitempty" description:"First line to read (1-based, 0 = beginning)"`
	EndLine   int    `json:"end_line,omitempty"   description:"Last line to read (0 = end of file)"`
}

func registerViewTool(srv *mcp.Server, perms permission.Service, workingDir string, toolCh chan mcpToolEvent) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Read",
		Description: "Read the contents of a file.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpViewInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Read called", "path", input.Path)

		id := uuid.New().String()
		inputJSON, _ := json.Marshal(input)
		emitToolStart(toolCh, id, "Read", string(inputJSON))
		defer emitToolEnd(toolCh, id)

		path := resolvePath(input.Path, workingDir)
		granted, err := perms.Request(ctx, permission.CreatePermissionRequest{
			SessionID:   mcpSessionID,
			ToolCallID:  uuid.New().String(),
			ToolName:    "view",
			Description: "Read file: " + input.Path,
			Action:      "read",
			Params:      input,
			Path:        path,
		})
		if err != nil || !granted {
			return toolError("read denied"), nil, nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			slog.Debug("cliprovider: MCP Read error", "path", path, "err", err)
			return toolError(err.Error()), nil, nil
		}

		content := string(data)
		// Fork patch: batch 15 — when a sub-agent (called via this MCP)
		// reads CLAUDE.md it gets the OPERATOR-facing delegation guidance
		// inserted by `crush claude-init`. That guidance tells the reader
		// to "delegate work to crush sub-agents" — which causes a
		// sub-agent that just read this to spawn a NEW crush sub-agent,
		// recursing until timeout. Strip the block before returning so
		// the sub-agent never sees the instruction it would loop on.
		// Filesystem file is untouched; only THIS read sees the filtered
		// content. Operator reading via shell or external tools still
		// sees the original.
		if isClaudeMdPath(path) {
			content = stripCrushClaudeInitBlock(content)
		}
		if input.StartLine > 0 || input.EndLine > 0 {
			content = sliceLines(content, input.StartLine, input.EndLine)
		}
		slog.Debug("cliprovider: MCP Read ok", "path", path, "bytes", len(data))
		return toolText(content), nil, nil
	})
}

// crushClaudeInitBlockPattern is the same regex `internal/cmd/claude_init.go`
// uses to identify our injected block. Duplicated here to avoid a cmd→
// cliprovider import (cmd already imports a lot from the agent layer).
// If the marker scheme ever changes, update both sites.
var crushClaudeInitBlockPattern = regexp.MustCompile(`(?s)<!-- crush-claude-init:v\d+ -->.*?<!-- /crush-claude-init -->\s*`)

func isClaudeMdPath(path string) bool {
	base := filepath.Base(path)
	// Case-insensitive — Windows users sometimes write "Claude.md".
	return strings.EqualFold(base, "CLAUDE.md")
}

func stripCrushClaudeInitBlock(content string) string {
	return crushClaudeInitBlockPattern.ReplaceAllString(content, "")
}

// ── write ─────────────────────────────────────────────────────────────────────

type mcpWriteInput struct {
	Path    string `json:"path"    description:"File path to write"`
	Content string `json:"content" description:"Content to write to the file"`
}

func registerWriteTool(srv *mcp.Server, perms permission.Service, workingDir string, toolCh chan mcpToolEvent) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Write",
		Description: "Write content to a file, creating or overwriting it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpWriteInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Write called", "path", input.Path, "bytes", len(input.Content))

		// Emit start event with path only (omit large content from stream part).
		id := uuid.New().String()
		inputJSON, _ := json.Marshal(struct {
			Path string `json:"path"`
		}{Path: input.Path})
		emitToolStart(toolCh, id, "Write", string(inputJSON))
		defer emitToolEnd(toolCh, id)

		path := resolvePath(input.Path, workingDir)
		granted, err := perms.Request(ctx, permission.CreatePermissionRequest{
			SessionID:   mcpSessionID,
			ToolCallID:  uuid.New().String(),
			ToolName:    "write",
			Description: "Write file: " + input.Path,
			Action:      "write",
			Params:      input,
			Path:        path,
		})
		if err != nil || !granted {
			return toolError("write denied"), nil, nil
		}

		if err := os.WriteFile(path, []byte(input.Content), 0o644); err != nil {
			slog.Debug("cliprovider: MCP Write error", "path", path, "err", err)
			return toolError(err.Error()), nil, nil
		}
		slog.Debug("cliprovider: MCP Write ok", "path", path)
		return toolText("file written"), nil, nil
	})
}

// ── glob ──────────────────────────────────────────────────────────────────────

type mcpGlobInput struct {
	Pattern string `json:"pattern" description:"Glob pattern (e.g. **/*.go)"`
	Path    string `json:"path,omitempty" description:"Directory to search in"`
}

func registerGlobTool(srv *mcp.Server, perms permission.Service, workingDir string, toolCh chan mcpToolEvent) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Glob",
		Description: "Find files matching a glob pattern.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpGlobInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Glob called", "pattern", input.Pattern, "path", input.Path)

		id := uuid.New().String()
		inputJSON, _ := json.Marshal(input)
		emitToolStart(toolCh, id, "Glob", string(inputJSON))
		defer emitToolEnd(toolCh, id)

		dir := workingDir
		if input.Path != "" {
			dir = resolvePath(input.Path, workingDir)
		}

		granted, err := perms.Request(ctx, permission.CreatePermissionRequest{
			SessionID:   mcpSessionID,
			ToolCallID:  uuid.New().String(),
			ToolName:    "glob",
			Description: "Find files: " + input.Pattern,
			Action:      "read",
			Params:      input,
			Path:        dir,
		})
		if err != nil || !granted {
			return toolError("glob denied"), nil, nil
		}

		// Use doublestar.Glob for safe, shell-free glob matching with ** support.
		fsys := os.DirFS(dir)
		matches, globErr := doublestar.Glob(fsys, input.Pattern)
		if globErr != nil {
			return toolError("glob error: " + globErr.Error()), nil, nil
		}
		const maxGlobResults = 200
		if len(matches) > maxGlobResults {
			matches = matches[:maxGlobResults]
		}
		slog.Debug("cliprovider: MCP Glob ok", "matches", len(matches))
		return toolText(strings.Join(matches, "\n")), nil, nil
	})
}

// ── grep ──────────────────────────────────────────────────────────────────────

type mcpGrepInput struct {
	Pattern string `json:"pattern" description:"Regular expression to search for"`
	Path    string `json:"path,omitempty" description:"Directory or file to search in"`
	Glob    string `json:"glob,omitempty" description:"File glob filter (e.g. *.go)"`
}

func registerGrepTool(srv *mcp.Server, perms permission.Service, workingDir string, toolCh chan mcpToolEvent) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Grep",
		Description: "Search file contents using a regular expression.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpGrepInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Grep called", "pattern", input.Pattern, "path", input.Path)

		id := uuid.New().String()
		inputJSON, _ := json.Marshal(input)
		emitToolStart(toolCh, id, "Grep", string(inputJSON))
		defer emitToolEnd(toolCh, id)

		dir := workingDir
		if input.Path != "" {
			dir = resolvePath(input.Path, workingDir)
		}

		granted, err := perms.Request(ctx, permission.CreatePermissionRequest{
			SessionID:   mcpSessionID,
			ToolCallID:  uuid.New().String(),
			ToolName:    "grep",
			Description: "Search: " + input.Pattern,
			Action:      "read",
			Params:      input,
			Path:        dir,
		})
		if err != nil || !granted {
			return toolError("grep denied"), nil, nil
		}

		// --max-count limits matches per file; pipe through head to cap total output.
		args := []string{"grep", "-rn", "--color=never", "--max-count=100", input.Pattern, dir}
		if input.Glob != "" {
			args = append(args, "--include="+input.Glob)
		}
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		_ = cmd.Run() // grep exits 1 when no matches — not an error

		// Cap total output size to avoid flooding the model context.
		const maxGrepBytes = 200 * 1024
		out := buf.String()
		if len(out) > maxGrepBytes {
			out = out[:maxGrepBytes] + "\n...(output truncated)"
		}
		slog.Debug("cliprovider: MCP Grep ok", "results_len", len(out))
		return toolText(out), nil, nil
	})
}

// ── todos ─────────────────────────────────────────────────────────────────────

// mergeMCPTodos merges the CLI model's desired todo list with the current DB
// state. The model's list is authoritative; the only protection applied is
// status protection: statuses can only advance (pending→in_progress→completed).
func mergeMCPTodos(dbTodos []session.Todo, modelItems []mcpTodoItem) []session.Todo {
	if len(dbTodos) == 0 {
		todos := make([]session.Todo, len(modelItems))
		for i, item := range modelItems {
			todos[i] = session.Todo{Content: item.Content, Status: session.TodoStatus(item.Status), ActiveForm: item.ActiveForm}
		}
		return todos
	}
	dbByContent := make(map[string]session.Todo, len(dbTodos))
	for _, t := range dbTodos {
		dbByContent[t.Content] = t
	}
	var result []session.Todo
	for _, item := range modelItems {
		wantStatus := session.TodoStatus(item.Status)
		if dbTodo, exists := dbByContent[item.Content]; exists {
			if mcpStatusLevel(dbTodo.Status) > mcpStatusLevel(wantStatus) {
				slog.Info("cliprovider: MCP todos protecting status from regression",
					"content", item.Content, "db_status", dbTodo.Status, "model_status", wantStatus)
				wantStatus = dbTodo.Status
			}
		}
		result = append(result, session.Todo{Content: item.Content, Status: wantStatus, ActiveForm: item.ActiveForm})
	}
	return result
}

func mcpStatusLevel(s session.TodoStatus) int {
	switch s {
	case session.TodoStatusInProgress:
		return 1
	case session.TodoStatusCompleted:
		return 2
	default:
		return 0
	}
}


type mcpTodoItem struct {
	Content    string `json:"content"     description:"What needs to be done (imperative form)"`
	Status     string `json:"status"      description:"Task status: pending, in_progress, or completed"`
	ActiveForm string `json:"active_form" description:"Present continuous form (e.g. 'Running tests')"`
}

type mcpTodosInput struct {
	Todos []mcpTodoItem `json:"todos" description:"The updated todo list"`
}

func registerTodosTool(srv *mcp.Server, sessions session.Service, sessionID string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "todos",
		Description: "Update the task list for the current session. Use this to create, update or complete tasks so the user can track progress.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpTodosInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP todos called", "session", sessionID, "count", len(input.Todos))

		sess, err := sessions.Get(ctx, sessionID)
		if err != nil {
			return toolError("failed to get session: " + err.Error()), nil, nil
		}

		// Validate statuses.
		for _, item := range input.Todos {
			switch item.Status {
			case "pending", "in_progress", "completed":
			default:
				return toolError(fmt.Sprintf("invalid status %q for todo %q", item.Status, item.Content)), nil, nil
			}
		}

		// Merge with current DB todos: protect status from regression and keep user-added tasks.
		todos := mergeMCPTodos(sess.Todos, input.Todos)

		slog.Info("cliprovider: MCP todos tool updating todos",
			"session", sessionID,
			"prev", sess.Todos,
			"merged", todos,
		)
		sess.Todos = todos
		if _, err := sessions.Save(ctx, sess); err != nil {
			return toolError("failed to save todos: " + err.Error()), nil, nil
		}

		completedCount, pendingCount, inProgressCount := 0, 0, 0
		for _, t := range todos {
			switch t.Status {
			case session.TodoStatusPending:
				pendingCount++
			case session.TodoStatusInProgress:
				inProgressCount++
			case session.TodoStatusCompleted:
				completedCount++
			}
		}
				slog.Debug("cliprovider: MCP todos saved", "pending", pendingCount, "in_progress", inProgressCount, "completed", completedCount)
		return toolText(fmt.Sprintf("Todo list updated. Status: %d pending, %d in progress, %d completed", pendingCount, inProgressCount, completedCount)), nil, nil
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

// mcpSessionID is used as the session ID for permission requests made by the
// MCP server. It is a fixed string because the MCP server is not tied to a
// specific crush session.
const mcpSessionID = "cli-mcp"

func toolText(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// resolvePath resolves a path relative to workingDir, guarding against
// path traversal for relative paths. Absolute paths are cleaned but not
// restricted (the caller may legitimately reference files outside workingDir).
func resolvePath(path, workingDir string) string {
	if strings.HasPrefix(path, "/") || (len(path) > 1 && path[1] == ':') {
		return filepath.Clean(path) // absolute — just normalise
	}
	resolved := filepath.Clean(filepath.Join(workingDir, path))
	// Block relative paths that escape workingDir (e.g. "../../etc/passwd").
	cleanWD := filepath.Clean(workingDir)
	if resolved != cleanWD && !strings.HasPrefix(resolved, cleanWD+string(filepath.Separator)) {
		slog.Warn("cliprovider: path traversal blocked", "path", path, "resolved", resolved)
		return workingDir
	}
	return resolved
}

func runShell(ctx context.Context, command, dir string) (string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd.exe", "/c", command)
	} else {
		cmd = exec.CommandContext(ctx, "bash", "-c", command)
	}
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// mcpConfigLockTimeout caps how long an MCP id/settings flock will
// wait before failing. Fork patch (concurrency): chosen so a wedged
// sibling crush process (debugger, suspended shell, frozen NFS mount)
// cannot freeze the entire parallel-run fleet on a shared id/settings
// file — see CHANGELOG.fork.md (Section 4.I).
const mcpConfigLockTimeout = 30 * time.Second

// acquireMCPConfigLock is a thin wrapper around
// session.AcquireFileLockContext that enforces mcpConfigLockTimeout.
// All MCP id/settings critical sections in this file use it.
func acquireMCPConfigLock(lockPath string) (*session.FileLock, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mcpConfigLockTimeout)
	defer cancel()
	return session.AcquireFileLockContext(ctx, lockPath)
}

// ── qwen MCP registration ─────────────────────────────────────────────────────

// qwenMCPID returns a stable MCP server name for the given workingDir.
// If <workingDir>/.crush/ already exists, the ID is stored there in qwen-mcp-id.
// Otherwise a temp file keyed by workingDir is used so we never create .crush/
// in directories that don't already have a crush project.
//
// Fork patch (concurrency): wrap the read-then-write of the id file with
// a flock (session.AcquireFileLock) so two parallel `crush run` processes
// in the same workingDir cannot both miss the file, both generate a UUID,
// and end up with a split-brain MCP server name. See CHANGELOG.fork.md.
func qwenMCPID(workingDir string) (string, error) {
	var idFile string
	crushDir := filepath.Join(workingDir, ".crush")
	if info, err := os.Stat(crushDir); err == nil && info.IsDir() {
		// .crush/ exists — this is a crush project directory, store ID there.
		idFile = filepath.Join(crushDir, "qwen-mcp-id")
	} else {
		// No .crush/ here — use a temp file keyed by a hash of the path so
		// the ID remains stable across crush restarts without polluting the dir.
		h := fmt.Sprintf("%x", []byte(workingDir))
		if len(h) > 16 {
			h = h[:16]
		}
		idFile = filepath.Join(os.TempDir(), "crush-qwen-mcp-"+h)
	}
	// Fork patch: serialise the read-modify-write below across processes.
	lock, err := acquireMCPConfigLock(idFile + ".lock")
	if err != nil {
		return "", fmt.Errorf("cliprovider: lock qwen-mcp-id: %w", err)
	}
	defer lock.Release()
	if data, err := os.ReadFile(idFile); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	}
	// Generate a short stable ID for this project.
	id := "crush-" + uuid.New().String()[:8]
	if err := fsext.AtomicWriteFile(idFile, []byte(id), 0o644); err != nil {
		return "", fmt.Errorf("cliprovider: write qwen-mcp-id: %w", err)
	}
	slog.Info("cliprovider: created qwen MCP ID", "id", id, "file", idFile)
	return id, nil
}

// qwenSettingsPath returns the path to ~/.qwen/settings.json.
func qwenSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".qwen", "settings.json"), nil
}

// registerQwenMCP adds the crush MCP server to ~/.qwen/settings.json.
// It removes any stale entry with the same name first, then writes the new URL.
// The token is embedded in the URL as a query parameter (?token=...) since
// qwen's settings format does not support custom HTTP headers.
//
// Fork patch (concurrency): the read-modify-write of settings.json is
// guarded by a sibling .lock file so parallel `crush run` processes (or
// concurrent crush + qwen invocations) cannot stomp each other's
// entries, and the write itself is atomic so a kill mid-write cannot
// leave a half-truncated settings.json. See CHANGELOG.fork.md.
func registerQwenMCP(serverName, url string) error {
	path, err := qwenSettingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("cliprovider: mkdir qwen settings dir: %w", err)
	}
	lock, err := acquireMCPConfigLock(path + ".lock")
	if err != nil {
		return fmt.Errorf("cliprovider: lock qwen settings: %w", err)
	}
	defer lock.Release()
	var settings map[string]any
	if data, rerr := os.ReadFile(path); rerr == nil {
		_ = json.Unmarshal(data, &settings)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = map[string]any{}
	}
	mcpServers[serverName] = map[string]any{
		"httpUrl": url,
	}
	settings["mcpServers"] = mcpServers
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	slog.Info("cliprovider: registered qwen MCP server", "name", serverName, "url", url)
	return fsext.AtomicWriteFile(path, data, 0o644)
}

// deregisterQwenMCP removes the crush MCP entry from ~/.qwen/settings.json.
//
// Fork patch (concurrency): same flock + atomic-write as registerQwenMCP.
func deregisterQwenMCP(serverName string) {
	path, err := qwenSettingsPath()
	if err != nil {
		return
	}
	lock, err := acquireMCPConfigLock(path + ".lock")
	if err != nil {
		return
	}
	defer lock.Release()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var settings map[string]any
	if json.Unmarshal(data, &settings) != nil {
		return
	}
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		return
	}
	delete(mcpServers, serverName)
	if len(mcpServers) == 0 {
		delete(settings, "mcpServers")
	} else {
		settings["mcpServers"] = mcpServers
	}
	if data, err = json.MarshalIndent(settings, "", "  "); err != nil {
		return
	}
	_ = fsext.AtomicWriteFile(path, data, 0o644)
	slog.Info("cliprovider: deregistered qwen MCP server", "name", serverName)
}

// ── gemini MCP registration ───────────────────────────────────────────────────

// geminiMCPID returns a stable MCP server name for the given workingDir.
// Mirrors the logic of qwenMCPID but uses a separate ID file (gemini-mcp-id).
//
// Fork patch (concurrency): same flock + atomic-write treatment as
// qwenMCPID — see that function's note.
func geminiMCPID(workingDir string) (string, error) {
	var idFile string
	crushDir := filepath.Join(workingDir, ".crush")
	if info, err := os.Stat(crushDir); err == nil && info.IsDir() {
		idFile = filepath.Join(crushDir, "gemini-mcp-id")
	} else {
		h := fmt.Sprintf("%x", []byte(workingDir))
		if len(h) > 16 {
			h = h[:16]
		}
		idFile = filepath.Join(os.TempDir(), "crush-gemini-mcp-"+h)
	}
	lock, err := acquireMCPConfigLock(idFile + ".lock")
	if err != nil {
		return "", fmt.Errorf("cliprovider: lock gemini-mcp-id: %w", err)
	}
	defer lock.Release()
	if data, err := os.ReadFile(idFile); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	}
	id := "crush-" + uuid.New().String()[:8]
	if err := fsext.AtomicWriteFile(idFile, []byte(id), 0o644); err != nil {
		return "", fmt.Errorf("cliprovider: write gemini-mcp-id: %w", err)
	}
	slog.Info("cliprovider: created gemini MCP ID", "id", id, "file", idFile)
	return id, nil
}

// geminiSettingsPath returns the path to ~/.gemini/settings.json.
func geminiSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "settings.json"), nil
}

// registerGeminiMCP adds the crush MCP server to ~/.gemini/settings.json.
// The Authorization: Bearer header is stored in the settings so Gemini sends
// it with each MCP request. trust:true bypasses Gemini's own confirmation
// prompts so tool calls flow directly to crush's permission dialog.
// Fork patch (concurrency): flock + atomic-write — see registerQwenMCP.
func registerGeminiMCP(serverName, addr, token string) error {
	path, err := geminiSettingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lock, err := acquireMCPConfigLock(path + ".lock")
	if err != nil {
		return fmt.Errorf("cliprovider: lock gemini settings: %w", err)
	}
	defer lock.Release()
	var settings map[string]any
	if data, rerr := os.ReadFile(path); rerr == nil {
		_ = json.Unmarshal(data, &settings)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = map[string]any{}
	}
	mcpServers[serverName] = map[string]any{
		"url":  "http://" + addr + "/mcp",
		"type": "http",
		"headers": map[string]string{
			"Authorization": "Bearer " + token,
		},
		"trust": true,
	}
	settings["mcpServers"] = mcpServers
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	slog.Info("cliprovider: registered gemini MCP server", "name", serverName, "addr", addr)
	return fsext.AtomicWriteFile(path, data, 0o644)
}

// deregisterGeminiMCP removes the crush MCP entry from ~/.gemini/settings.json.
//
// Fork patch (concurrency): flock + atomic-write — see registerQwenMCP.
func deregisterGeminiMCP(serverName string) {
	path, err := geminiSettingsPath()
	if err != nil {
		return
	}
	lock, err := acquireMCPConfigLock(path + ".lock")
	if err != nil {
		return
	}
	defer lock.Release()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var settings map[string]any
	if json.Unmarshal(data, &settings) != nil {
		return
	}
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		return
	}
	delete(mcpServers, serverName)
	if len(mcpServers) == 0 {
		delete(settings, "mcpServers")
	} else {
		settings["mcpServers"] = mcpServers
	}
	if data, err = json.MarshalIndent(settings, "", "  "); err != nil {
		return
	}
	_ = fsext.AtomicWriteFile(path, data, 0o644)
	slog.Info("cliprovider: deregistered gemini MCP server", "name", serverName)
}

func sliceLines(content string, start, end int) string {
	lines := strings.Split(content, "\n")
	if start > 0 {
		start-- // convert to 0-based
		if start >= len(lines) {
			return ""
		}
		lines = lines[start:]
	}
	if end > 0 && end <= len(lines) {
		lines = lines[:end]
	}
	return strings.Join(lines, "\n")
}
