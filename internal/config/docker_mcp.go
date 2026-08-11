package config

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/platform"
	"golang.org/x/sync/singleflight"
)

var dockerMCPVersionRunner = func(ctx context.Context) error {
	cmd := platform.Command(ctx, "docker", "mcp", "version")
	return cmd.Run()
}

const dockerMCPAvailabilityTTL = 10 * time.Second

var dockerMCPAvailabilityCache struct {
	mu        sync.Mutex
	available bool
	checkedAt time.Time
	known     bool
}

// dockerMCPRefreshGroup single-flights concurrent RefreshDockerMCPAvailability
// calls so that N goroutines racing to refresh (e.g. several callers all
// observing a stale/unknown cache at once) share a single 'docker mcp
// version' subprocess invocation instead of each spawning their own —
// spawning a process is comparatively expensive and there is no benefit to
// running it more than once for callers that overlap in time.
var dockerMCPRefreshGroup singleflight.Group

// DockerMCPName is the name of the Docker MCP configuration.
const DockerMCPName = "docker"

// IsDockerMCPAvailable checks if Docker MCP is available by running
// 'docker mcp version'.
func IsDockerMCPAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := dockerMCPVersionRunner(ctx)
	return err == nil
}

// DockerMCPAvailabilityCached returns the cached Docker MCP availability and
// whether the cached value is still fresh.
func DockerMCPAvailabilityCached() (available bool, known bool) {
	dockerMCPAvailabilityCache.mu.Lock()
	defer dockerMCPAvailabilityCache.mu.Unlock()

	if !dockerMCPAvailabilityCache.known {
		return false, false
	}
	if time.Since(dockerMCPAvailabilityCache.checkedAt) > dockerMCPAvailabilityTTL {
		return dockerMCPAvailabilityCache.available, false
	}
	return dockerMCPAvailabilityCache.available, true
}

// RefreshDockerMCPAvailability refreshes and caches Docker MCP availability.
// Concurrent calls are single-flighted (see dockerMCPRefreshGroup): only one
// 'docker mcp version' subprocess actually runs at a time, and every caller
// that arrived while it was in flight receives its result.
func RefreshDockerMCPAvailability() bool {
	v, _, _ := dockerMCPRefreshGroup.Do("refresh", func() (any, error) {
		available := IsDockerMCPAvailable()
		dockerMCPAvailabilityCache.mu.Lock()
		dockerMCPAvailabilityCache.available = available
		dockerMCPAvailabilityCache.checkedAt = time.Now()
		dockerMCPAvailabilityCache.known = true
		dockerMCPAvailabilityCache.mu.Unlock()
		return available, nil
	})
	available, _ := v.(bool)
	return available
}

// IsDockerMCPEnabled checks if Docker MCP is already configured.
func (c *Config) IsDockerMCPEnabled() bool {
	if c.MCP == nil {
		return false
	}
	_, exists := c.MCP[DockerMCPName]
	return exists
}

// DockerMCPConfig returns the default Docker MCP stdio configuration.
func DockerMCPConfig() MCPConfig {
	return MCPConfig{
		Type:     MCPStdio,
		Command:  "docker",
		Args:     []string{"mcp", "gateway", "run"},
		Disabled: false,
	}
}

// PrepareDockerMCPConfig validates Docker MCP availability and stages the
// Docker MCP configuration in memory.
func (s *ConfigStore) PrepareDockerMCPConfig() (MCPConfig, error) {
	if !IsDockerMCPAvailable() {
		return MCPConfig{}, fmt.Errorf("docker mcp is not available, please ensure docker is installed and 'docker mcp version' succeeds")
	}

	mcpConfig := DockerMCPConfig()
	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.MCP = maps.Clone(cfgCopy.MCP)
		if cfgCopy.MCP == nil {
			cfgCopy.MCP = make(map[string]MCPConfig)
		}
		cfgCopy.MCP[DockerMCPName] = mcpConfig
	})
	return mcpConfig, nil
}

// PersistDockerMCPConfig persists a previously prepared Docker MCP
// configuration to the global config file.
func (s *ConfigStore) PersistDockerMCPConfig(mcpConfig MCPConfig) error {
	if err := s.SetConfigField(ScopeGlobal, "mcp."+DockerMCPName, mcpConfig); err != nil {
		return fmt.Errorf("failed to persist docker mcp configuration: %w", err)
	}
	return nil
}

// EnableDockerMCP adds Docker MCP configuration and persists it.
func (s *ConfigStore) EnableDockerMCP() error {
	mcpConfig, err := s.PrepareDockerMCPConfig()
	if err != nil {
		return err
	}
	if err := s.PersistDockerMCPConfig(mcpConfig); err != nil {
		return err
	}
	return nil
}

// DisableDockerMCP removes Docker MCP configuration and persists the change.
func (s *ConfigStore) DisableDockerMCP() error {
	if s.Config().MCP == nil {
		return nil
	}

	var mcpAfterRemoval MCPs
	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.MCP = maps.Clone(cfgCopy.MCP)
		delete(cfgCopy.MCP, DockerMCPName)
		mcpAfterRemoval = cfgCopy.MCP
	})

	// Persist the updated MCP map to the config file.
	if err := s.SetConfigField(ScopeGlobal, "mcp", mcpAfterRemoval); err != nil {
		return fmt.Errorf("failed to persist docker mcp removal: %w", err)
	}

	return nil
}
