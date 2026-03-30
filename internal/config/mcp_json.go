package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/charmbracelet/crush/internal/home"
)

// mcpJSONFile represents the .mcp.json file format (Claude Code compatible).
type mcpJSONFile struct {
	MCPServers map[string]mcpJSONEntry `json:"mcpServers"`
}

// mcpJSONEntry is a single server entry in the .mcp.json format.
type mcpJSONEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// loadMCPJSON reads a .mcp.json file and converts its entries to MCPConfig.
func loadMCPJSON(path string) (map[string]MCPConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var file mcpJSONFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}

	result := make(map[string]MCPConfig, len(file.MCPServers))
	for name, entry := range file.MCPServers {
		mcpType := MCPStdio
		switch MCPType(entry.Type) {
		case MCPSSE:
			mcpType = MCPSSE
		case MCPHttp:
			mcpType = MCPHttp
		}

		result[name] = MCPConfig{
			Type:    mcpType,
			Command: entry.Command,
			Args:    entry.Args,
			Env:     entry.Env,
			URL:     entry.URL,
			Headers: entry.Headers,
			Source:  MCPSourceExternal,
		}
	}
	return result, nil
}

// discoverMCPJSONFiles returns paths to .mcp.json files in priority order
// (lowest priority first): global (~/.claude/.mcp.json), then project root.
func discoverMCPJSONFiles(workingDir string) []string {
	var paths []string

	// Global: ~/.claude/.mcp.json
	if homeDir := home.Dir(); homeDir != "" {
		global := filepath.Join(homeDir, ".claude", ".mcp.json")
		if _, err := os.Stat(global); err == nil {
			paths = append(paths, global)
		}
	}

	// Project root: <workingDir>/.mcp.json
	if workingDir != "" {
		project := filepath.Join(workingDir, ".mcp.json")
		if _, err := os.Stat(project); err == nil {
			paths = append(paths, project)
		}
	}

	return paths
}

// loadExternalMCPServers discovers and loads all .mcp.json files, returning
// a merged map of server configs. Later files override earlier ones.
func loadExternalMCPServers(workingDir string) map[string]MCPConfig {
	result := make(map[string]MCPConfig)
	for _, path := range discoverMCPJSONFiles(workingDir) {
		servers, err := loadMCPJSON(path)
		if err != nil {
			slog.Warn("Failed to load .mcp.json", "path", path, "err", err)
			continue
		}
		slog.Info("Loaded MCP servers from .mcp.json", "path", path, "count", len(servers))
		for name, cfg := range servers {
			result[name] = cfg
		}
	}
	return result
}

// mergeExternalMCPServers injects .mcp.json servers into the config's MCP map.
// Servers already defined in crush.json take full precedence. For external
// servers, the disabled state is read from the crush config store.
func mergeExternalMCPServers(cfg *Config, store *ConfigStore, external map[string]MCPConfig) {
	if cfg.MCP == nil {
		cfg.MCP = make(MCPs)
	}
	for name, extCfg := range external {
		if _, exists := cfg.MCP[name]; exists {
			// Crush's own config defines this server — it takes precedence.
			continue
		}
		// Check if the user has toggled this server off via the UI.
		if store.HasConfigField(ScopeWorkspace, "mcp."+name+".disabled") {
			data, _ := os.ReadFile(store.workspacePath)
			if len(data) > 0 {
				var ws struct {
					MCP map[string]struct {
						Disabled bool `json:"disabled"`
					} `json:"mcp"`
				}
				if json.Unmarshal(data, &ws) == nil {
					if srv, ok := ws.MCP[name]; ok {
						extCfg.Disabled = srv.Disabled
					}
				}
			}
		} else if store.HasConfigField(ScopeGlobal, "mcp."+name+".disabled") {
			data, _ := os.ReadFile(store.globalDataPath)
			if len(data) > 0 {
				var gs struct {
					MCP map[string]struct {
						Disabled bool `json:"disabled"`
					} `json:"mcp"`
				}
				if json.Unmarshal(data, &gs) == nil {
					if srv, ok := gs.MCP[name]; ok {
						extCfg.Disabled = srv.Disabled
					}
				}
			}
		}
		cfg.MCP[name] = extCfg
	}
}
