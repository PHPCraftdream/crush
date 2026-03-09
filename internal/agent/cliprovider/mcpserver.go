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
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// crushMCPServer is an in-process MCP HTTP server with Bearer-token auth.
type crushMCPServer struct {
	addr    string // "127.0.0.1:PORT"
	token   string
	httpSrv *http.Server
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

// newCrushMCPServer starts a local MCP HTTP server and returns it.
// The server exposes crush's core tools; each tool call goes through
// perms.Request before execution so crush's permission dialog appears.
func newCrushMCPServer(ctx context.Context, perms permission.Service, workingDir string) (*crushMCPServer, error) {
	// 32-byte random token → 64-char hex string.
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("cliprovider: generate MCP token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "crush",
		Title:   "Crush",
		Version: "1.0",
	}, nil)

	registerMCPTools(srv, perms, workingDir)

	rawHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			// Stateless mode: each POST creates a temporary session.
			// Simple and sufficient for single-agent CLI use.
			Stateless: true,
		},
	)

	// Bearer-token auth middleware.
	bearer := "Bearer " + token
	authedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != bearer {
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

	httpSrv := &http.Server{Handler: http.StripPrefix("/mcp", authedHandler)}
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
	}, nil
}

// registerMCPTools adds crush tool implementations to the MCP server.
// Each tool requests permission via perms.Request before executing.
func registerMCPTools(srv *mcp.Server, perms permission.Service, workingDir string) {
	registerBashTool(srv, perms, workingDir)
	registerViewTool(srv, perms, workingDir)
	registerWriteTool(srv, perms, workingDir)
	registerGlobTool(srv, perms, workingDir)
	registerGrepTool(srv, perms, workingDir)
}

// ── bash ─────────────────────────────────────────────────────────────────────

type mcpBashInput struct {
	Command     string `json:"command"     description:"Shell command to execute"`
	Description string `json:"description" description:"Brief description of what the command does"`
	WorkingDir  string `json:"working_dir,omitempty" description:"Working directory (defaults to project root)"`
}

func registerBashTool(srv *mcp.Server, perms permission.Service, workingDir string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Bash",
		Description: "Execute a shell command. Requires user approval.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpBashInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Bash called", "command", input.Command, "description", input.Description)

		wd := workingDir
		if input.WorkingDir != "" {
			wd = input.WorkingDir
		}

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

func registerViewTool(srv *mcp.Server, perms permission.Service, workingDir string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Read",
		Description: "Read the contents of a file.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpViewInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Read called", "path", input.Path)

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
		if input.StartLine > 0 || input.EndLine > 0 {
			content = sliceLines(content, input.StartLine, input.EndLine)
		}
		slog.Debug("cliprovider: MCP Read ok", "path", path, "bytes", len(data))
		return toolText(content), nil, nil
	})
}

// ── write ─────────────────────────────────────────────────────────────────────

type mcpWriteInput struct {
	Path    string `json:"path"    description:"File path to write"`
	Content string `json:"content" description:"Content to write to the file"`
}

func registerWriteTool(srv *mcp.Server, perms permission.Service, workingDir string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Write",
		Description: "Write content to a file, creating or overwriting it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpWriteInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Write called", "path", input.Path, "bytes", len(input.Content))

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

func registerGlobTool(srv *mcp.Server, perms permission.Service, workingDir string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Glob",
		Description: "Find files matching a glob pattern.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpGlobInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Glob called", "pattern", input.Pattern, "path", input.Path)

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

func registerGrepTool(srv *mcp.Server, perms permission.Service, workingDir string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "Grep",
		Description: "Search file contents using a regular expression.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpGrepInput) (*mcp.CallToolResult, any, error) {
		slog.Debug("cliprovider: MCP Grep called", "pattern", input.Pattern, "path", input.Path)

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
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
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
