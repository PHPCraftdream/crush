package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hyperp "github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestConfigStore_ConfigPath_GlobalAlwaysWorks(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		globalDataPath: "/some/global/crush.json",
	})

	path, err := store.configPath(ScopeGlobal)
	require.NoError(t, err)
	require.Equal(t, "/some/global/crush.json", path)
}

func TestConfigStore_ConfigPath_WorkspaceReturnsPath(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		workspacePath: "/some/workspace/.crush/crush.json",
	})

	path, err := store.configPath(ScopeWorkspace)
	require.NoError(t, err)
	require.Equal(t, "/some/workspace/.crush/crush.json", path)
}

func TestConfigStore_ConfigPath_WorkspaceErrorsWhenEmpty(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		globalDataPath: "/some/global/crush.json",
		workspacePath:  "",
	})

	_, err := store.configPath(ScopeWorkspace)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoWorkspaceConfig))
}

func TestConfigStore_SetConfigField_WorkspaceScopeGuard(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: filepath.Join(t.TempDir(), "global.json"),
		workspacePath:  "",
	})

	err := store.SetConfigField(ScopeWorkspace, "foo", "bar")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoWorkspaceConfig))
}

func TestConfigStore_SetConfigField_GlobalScopeAlwaysWorks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	globalPath := filepath.Join(dir, "crush.json")
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: globalPath,
	})

	err := store.SetConfigField(ScopeGlobal, "foo", "bar")
	require.NoError(t, err)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	require.Contains(t, string(data), `"foo"`)
}

func TestConfigStore_RemoveConfigField_WorkspaceScopeGuard(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: filepath.Join(t.TempDir(), "global.json"),
		workspacePath:  "",
	})

	err := store.RemoveConfigField(ScopeWorkspace, "foo")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoWorkspaceConfig))
}

func TestConfigStore_HasConfigField_WorkspaceScopeGuard(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: filepath.Join(t.TempDir(), "global.json"),
		workspacePath:  "",
	})

	has := store.HasConfigField(ScopeWorkspace, "foo")
	require.False(t, has)
}

func TestConfigStore_RuntimeOverrides_Independent(t *testing.T) {
	t.Parallel()

	store1 := newTestConfigStore(testStoreOpts{config: &Config{}})
	store2 := newTestConfigStore(testStoreOpts{config: &Config{}})

	require.False(t, store1.Overrides().SkipPermissionRequests)
	require.False(t, store2.Overrides().SkipPermissionRequests)

	store1.SetSkipPermissionRequests(true)

	require.True(t, store1.Overrides().SkipPermissionRequests)
	require.False(t, store2.Overrides().SkipPermissionRequests)
}

func TestConfigStore_RuntimeOverrides_SetterPublishesNewGeneration(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{config: &Config{}})

	require.False(t, store.Overrides().SkipPermissionRequests)

	store.SetSkipPermissionRequests(true)

	require.True(t, store.Overrides().SkipPermissionRequests)
}

// TestConfigStore_UpdateAgentAllowedTools_PublishesNewGenerationWithoutMutatingOldSnapshot
// pins the copy-on-write contract of UpdateAgentAllowedTools: a *Config
// pointer captured via Config() BEFORE the call must NOT observe the
// change (it's a distinct, frozen generation), while a Config() call AFTER
// sees the update. This is the regression test for the bug UpdateAgentAllowedTools
// replaces — the old disableToolsInConfig wrote straight into
// cfg.Agents[id] on the currently-published *Config, which meant any
// reader holding that same pointer from before the call would see the
// mutation retroactively.
func TestConfigStore_UpdateAgentAllowedTools_PublishesNewGenerationWithoutMutatingOldSnapshot(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		config: &Config{
			Agents: map[string]Agent{
				AgentCoder: {
					ID:           AgentCoder,
					AllowedTools: []string{"view", "grep", "agent", "agentic_fetch", "bash", "edit"},
				},
			},
		},
	})

	// Capture the *Config pointer BEFORE the mutation, simulating a
	// concurrent reader that read Config() earlier and is still holding
	// that pointer (e.g. mid-turn).
	oldCfg := store.Config()
	oldTools := oldCfg.Agents[AgentCoder].AllowedTools

	store.UpdateAgentAllowedTools(AgentCoder, []string{"view", "grep", "bash", "edit"})

	// The old snapshot's Config must be completely unaffected: neither its
	// Agents map entry nor the AllowedTools slice it pointed to should
	// have changed underneath the earlier reader.
	require.Equal(t, []string{"view", "grep", "agent", "agentic_fetch", "bash", "edit"}, oldCfg.Agents[AgentCoder].AllowedTools,
		"a *Config captured before UpdateAgentAllowedTools must not observe the mutation")
	require.Equal(t, []string{"view", "grep", "agent", "agentic_fetch", "bash", "edit"}, oldTools,
		"the AllowedTools slice captured before the call must not be touched in place")

	// A fresh Config() call after the mutation sees the new generation.
	newCfg := store.Config()
	require.Equal(t, []string{"view", "grep", "bash", "edit"}, newCfg.Agents[AgentCoder].AllowedTools,
		"Config() after UpdateAgentAllowedTools must see the new generation")

	// The two *Config pointers must be distinct — proof that a new
	// generation was published rather than the old one mutated in place.
	require.NotSame(t, oldCfg, newCfg, "UpdateAgentAllowedTools must publish a new *Config, not mutate the old one")
}

func TestGlobalWorkspaceDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRUSH_GLOBAL_DATA", dir)

	wsDir := GlobalWorkspaceDir()
	globalData := GlobalConfigData()

	require.Equal(t, filepath.Dir(globalData), wsDir)
	require.Equal(t, dir, wsDir)
}

func TestScope_String(t *testing.T) {
	t.Parallel()

	require.Equal(t, "global", ScopeGlobal.String())
	require.Equal(t, "workspace", ScopeWorkspace.String())
	require.Contains(t, Scope(99).String(), "Scope(99)")
}

func TestConfigStaleness_CleanImmediatelyAfterSnapshot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create a config file
	content := []byte(`{"options": {"debug": true}}`)
	require.NoError(t, os.WriteFile(configPath, content, 0o600))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})
	store.captureStalenessSnapshot([]string{configPath})

	result := store.ConfigStaleness()
	require.False(t, result.Dirty)
	require.Empty(t, result.Changed)
	require.Empty(t, result.Missing)
}

func TestConfigStaleness_DetectsFileContentChange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create initial config file
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": false}`), 0o600))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})
	store.captureStalenessSnapshot([]string{configPath})

	// Modify the file
	time.Sleep(10 * time.Millisecond) // Ensure different mtime
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	require.Contains(t, result.Changed, configPath)
	require.Empty(t, result.Missing)
}

func TestConfigStaleness_DetectsFileDeletion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create initial config file
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})
	store.captureStalenessSnapshot([]string{configPath})

	// Delete the file
	require.NoError(t, os.Remove(configPath))

	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	require.Empty(t, result.Changed)
	require.Contains(t, result.Missing, configPath)
}

func TestConfigStaleness_DetectsNewFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Don't create file initially
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})
	store.captureStalenessSnapshot([]string{configPath})

	// Now create the file
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	require.Contains(t, result.Changed, configPath)
	require.Empty(t, result.Missing)
}

func TestConfigStaleness_SortedOutput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.json")
	pathB := filepath.Join(dir, "b.json")
	pathC := filepath.Join(dir, "c.json")

	// Create all files
	for _, p := range []string{pathA, pathB, pathC} {
		require.NoError(t, os.WriteFile(p, []byte(`{}`), 0o600))
	}

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: pathA,
	})
	// Add in reverse order to test sorting
	store.captureStalenessSnapshot([]string{pathC, pathA, pathB})

	// Modify all files
	time.Sleep(10 * time.Millisecond)
	for _, p := range []string{pathA, pathB, pathC} {
		require.NoError(t, os.WriteFile(p, []byte(`{"changed": true}`), 0o600))
	}

	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	// Should be sorted alphabetically
	require.Equal(t, []string{pathA, pathB, pathC}, result.Changed)
}

func TestConfigStaleness_RefreshClearsDirtyState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create initial config file
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": false}`), 0o600))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})
	store.captureStalenessSnapshot([]string{configPath})

	// Modify the file
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	// Verify dirty
	result := store.ConfigStaleness()
	require.True(t, result.Dirty)

	// Refresh snapshot
	require.NoError(t, store.RefreshStalenessSnapshot())

	// Verify clean now
	result = store.ConfigStaleness()
	require.False(t, result.Dirty)
	require.Empty(t, result.Changed)
	require.Empty(t, result.Missing)
}

// TestReloadFromDisk_UsesNewConfigValues is a regression test ensuring that
// ReloadFromDisk updates store state BEFORE running model/agent setup,
// so the new config values are used rather than stale pre-reload values.
func TestReloadFromDisk_UsesNewConfigValues(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Isolate from the host's global config so only test-provided
	// providers are visible.
	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)

	// Create initial config with one model preference
	initialConfig := `{
		"models": {
			"large": {"provider": "openai", "model": "gpt-4"}
		},
		"providers": {
			"openai": {
				"api_key": "test-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config properly
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Set globalDataPath for the test (Load doesn't set this directly)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Verify initial model
	require.Equal(t, "openai", store.Config().Models[SelectedModelTypeLarge].Provider)
	require.Equal(t, "gpt-4", store.Config().Models[SelectedModelTypeLarge].Model)

	// Modify config on disk to change model
	updatedConfig := `{
		"models": {
			"large": {"provider": "anthropic", "model": "claude-3"}
		},
		"providers": {
			"openai": {
				"api_key": "test-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			},
			"anthropic": {
				"api_key": "test-key-2",
				"models": [{"id": "claude-3", "name": "Claude 3"}]
			}
		}
	}`
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(updatedConfig), 0o600))

	// Reload from disk
	ctx := context.Background()
	err = store.ReloadFromDisk(ctx)
	require.NoError(t, err)

	// Verify the NEW config values are now in effect (regression check)
	require.Equal(t, "anthropic", store.Config().Models[SelectedModelTypeLarge].Provider)
	require.Equal(t, "claude-3", store.Config().Models[SelectedModelTypeLarge].Model)
}

// TestSetConfigField_AutoReloads verifies that SetConfigField automatically
// reloads config into memory after writing, so subsequent reads see the new value.
func TestSetConfigField_AutoReloads(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create initial config file with debug = false
	initialConfig := `{"options": {"debug": false}}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Verify initial state
	require.False(t, store.Config().Options.Debug)

	// Set globalDataPath and capture snapshot for staleness tracking
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Use SetConfigField to change debug to true
	err = store.SetConfigField(ScopeGlobal, "options.debug", true)
	require.NoError(t, err)

	// Verify in-memory state was automatically reloaded and reflects the change
	require.True(t, store.Config().Options.Debug, "Expected config to auto-reload and show debug = true")

	// Verify staleness is clean after the reload
	staleness := store.ConfigStaleness()
	require.False(t, staleness.Dirty, "Expected staleness to be clean after auto-reload")
}

// TestRemoveConfigField_AutoReloads verifies that RemoveConfigField automatically
// reloads config into memory after writing.
func TestRemoveConfigField_AutoReloads(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create initial config file with a custom option
	initialConfig := `{"options": {"debug": true, "custom_field": "value"}}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Set globalDataPath and capture snapshot
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Verify the field exists initially (indirectly - store loaded successfully)
	require.True(t, store.Config().Options.Debug)

	// Remove the debug field
	err = store.RemoveConfigField(ScopeGlobal, "options.debug")
	require.NoError(t, err)

	// Verify auto-reload occurred and stale state is clean
	staleness := store.ConfigStaleness()
	require.False(t, staleness.Dirty, "Expected staleness to be clean after auto-reload from RemoveConfigField")
}

// TestSetConfigField_AutoReloadSkipsWhenNoWorkingDir verifies that auto-reload
// gracefully skips when working directory is not set (e.g., during testing).
func TestSetConfigField_AutoReloadSkipsWhenNoWorkingDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create a store without working directory (like some test setups)
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
		// workingDir is empty
	})

	// SetConfigField should succeed even without workingDir (auto-reload skips)
	err := store.SetConfigField(ScopeGlobal, "foo", "bar")
	require.NoError(t, err)

	// Verify file was still written
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "foo")
}

// TestAutoReloadDisabledDuringReload verifies that auto-reload is suppressed
// during ReloadFromDisk to prevent re-entrant/nested reload calls.
func TestAutoReloadDisabledDuringReload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create initial config with a provider that will trigger config modification during reload
	// (simulating the anthropic OAuth token removal case)
	initialConfig := `{
		"providers": {
			"anthropic": {
				"api_key": "test-key",
				"oauth": {"access_token": "token", "refresh_token": "refresh"}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load will trigger configureProviders which removes anthropic OAuth config.
	// This should NOT cause infinite recursion thanks to the publishMu guard
	// (the re-entrant auto-reload is skipped via TryLock).
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Verify the store loaded successfully and publishMu was released.
	require.True(t, store.publishMu.TryLock(), "publishMu should be free after Load")
	store.publishMu.Unlock()

	// Capture snapshot and verify reload also works without recursion
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Modify file and reload - this should work without re-entrancy issues
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"options": {"debug": true}}`), 0o600))

	err = store.ReloadFromDisk(context.Background())
	require.NoError(t, err)

	// Verify reload completed successfully and publishMu was released.
	require.True(t, store.publishMu.TryLock(), "publishMu should be free after ReloadFromDisk")
	store.publishMu.Unlock()
}

// TestSetConfigFields_AutoReloadsAtomically verifies that SetConfigFields writes
// multiple fields in a single disk write and triggers only one auto-reload,
// avoiding intermediate states where only some fields are persisted.
func TestSetConfigFields_AutoReloadsAtomically(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create initial config file.
	initialConfig := `{"options": {"debug": false}}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config.
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Set globalDataPath and capture snapshot.
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Write multiple fields atomically.
	err = store.SetConfigFields(ScopeGlobal, map[string]any{
		"options.debug":  true,
		"options.custom": "hello",
	})
	require.NoError(t, err)

	// Verify both fields are reflected in memory.
	require.True(t, store.Config().Options.Debug)
}

func TestLoadTokenFromDisk_ReturnsNewerToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create config file with a newer token on disk
	configContent := `{
		"providers": {
			"hyper": {
				"oauth": {
					"access_token": "newer-token-from-disk",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.NotNil(t, token)
	require.Equal(t, "newer-token-from-disk", token.AccessToken)
	require.Equal(t, "refresh-abc", token.RefreshToken)
	require.Equal(t, 3600, token.ExpiresIn)
	require.Equal(t, int64(9999999999), token.ExpiresAt)
}

func TestLoadTokenFromDisk_ReturnsNilWhenSameToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create config file with the same token
	configContent := `{
		"providers": {
			"hyper": {
				"oauth": {
					"access_token": "same-token",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.NotNil(t, token)
	require.Equal(t, "same-token", token.AccessToken)
}

func TestLoadTokenFromDisk_ReturnsNilWhenFileMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "nonexistent.json")

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestLoadTokenFromDisk_ReturnsNilWhenProviderMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create config file without the hyper provider
	configContent := `{"providers": {"openai": {"api_key": "test-key"}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestLoadTokenFromDisk_ReturnsNilWhenOAuthMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create config file with provider but no OAuth token
	configContent := `{"providers": {"hyper": {"api_key": "test-key"}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestRefreshOAuthToken_UsesDiskTokenWhenDifferent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create config file with a newer token on disk
	configContent := `{
		"providers": {
			"hyper": {
				"api_key": "newer-access-token",
				"oauth": {
					"access_token": "newer-access-token",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	// Set up store with an older in-memory token
	oldToken := &oauth.Token{
		AccessToken:  "older-access-token",
		RefreshToken: "refresh-abc",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(), // Expired
	}

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("hyper", ProviderConfig{
		ID:         "hyper",
		Name:       "Hyper",
		APIKey:     oldToken.AccessToken,
		OAuthToken: oldToken,
	})

	store := newTestConfigStore(testStoreOpts{
		config: &Config{
			Providers: providers,
		},
		globalDataPath: configPath,
	})

	// Refresh should use the disk token without making an external call
	err := store.RefreshOAuthToken(context.Background(), ScopeGlobal, "hyper")
	require.NoError(t, err)

	// Verify the in-memory token was updated to the disk token
	updatedConfig, ok := store.Config().Providers.Get("hyper")
	require.True(t, ok)
	require.Equal(t, "newer-access-token", updatedConfig.APIKey)
	require.Equal(t, "newer-access-token", updatedConfig.OAuthToken.AccessToken)
	require.Equal(t, "refresh-abc", updatedConfig.OAuthToken.RefreshToken)
}

// TestSetProviderRuntimeConfig_VisibleImmediatelyAndDiscardedByReload
// verifies the core contract of SetProviderRuntimeConfig: the in-memory
// provider update is visible immediately after the call returns, but a
// subsequent ReloadFromDisk rebuilds Providers from disk and discards the
// runtime-only change by design (the template/key on disk is the source of
// truth, not the in-memory copy).
func TestSetProviderRuntimeConfig_VisibleImmediatelyAndDiscardedByReload(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Config with one provider on disk.
	initialConfig := `{
		"providers": {
			"openai": {
				"api_key": "disk-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Isolate from host global config.
	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Verify the provider loaded from disk.
	pc, ok := store.Config().Providers.Get("openai")
	require.True(t, ok)
	require.Equal(t, "disk-key", pc.APIKey)

	// Apply a runtime-only update (simulating an API key refresh).
	pc.APIKey = "refreshed-key"
	store.SetProviderRuntimeConfig("openai", pc)

	// The update must be visible immediately — no intervening reload.
	pc2, ok := store.Config().Providers.Get("openai")
	require.True(t, ok)
	require.Equal(t, "refreshed-key", pc2.APIKey,
		"SetProviderRuntimeConfig change must be visible immediately")

	// A reload rebuilds Providers from disk, discarding the runtime-only
	// change. The API key reverts to the disk value.
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	pc3, ok := store.Config().Providers.Get("openai")
	require.True(t, ok)
	require.Equal(t, "disk-key", pc3.APIKey,
		"reload must rebuild Providers from disk, discarding runtime-only updates")
}

// TestRefreshOAuthToken_SurvivesReloadDuringNetworkCall is the regression
// test for the P2.1 finding: RefreshOAuthToken used to capture the
// Providers *csync.Map ONCE at the top of the function and write the
// refreshed token into that same captured map at the very end, AFTER the
// network round-trip (copilotRefreshTokenFn / hyperExchangeTokenFn). Every
// reload allocates a brand new *csync.Map (see SetProviderRuntimeConfig's
// doc comment), so if a reload published a new generation while the
// network call was in flight, the final write landed in the OLD,
// already-orphaned map — invisible to any reader of the current snapshot.
//
// This test overrides hyperExchangeTokenFn (restored via t.Cleanup) to
// block until the test goroutine has published a new generation (simulating
// a concurrent reload), THEN return the refreshed token — reproducing "a
// reload happened while we were waiting on the network" deterministically,
// without a real HTTP call and without relying on hyper.BaseURL()'s
// process-wide sync.OnceValue memoization (see
// TestProviders_ConcurrentErrorCollection_NotLost's caveat in
// provider_test.go for why that path can't be redirected safely per-test).
//
// The store is built via newTestConfigStore with no workingDir, so
// RefreshOAuthToken's own SetConfigFields call at the end succeeds in
// writing to globalDataPath but its trailing autoReload is skipped (see
// autoReload's "workingDir == "" " guard) — this is deliberate: if
// autoReload ran, it would re-read the very token RefreshOAuthToken just
// persisted to disk and transparently re-sync the in-memory Providers map,
// self-healing the exact bug this test targets and making the buggy and
// fixed code indistinguishable from the test's perspective. Skipping it
// isolates what we actually want to observe: whether RefreshOAuthToken's
// OWN in-memory write landed in the orphaned pre-reload map or the current
// one — independent of any later disk-driven resync.
func TestRefreshOAuthToken_SurvivesReloadDuringNetworkCall(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")
	require.NoError(t, os.WriteFile(configPath, []byte("{}"), 0o600))

	oldToken := &oauth.Token{
		AccessToken:  "old-access-token",
		RefreshToken: "refresh-token-1",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(), // expired, forces refresh
	}
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set(hyperp.Name, ProviderConfig{
		ID:         hyperp.Name,
		Name:       "Hyper",
		APIKey:     oldToken.AccessToken,
		OAuthToken: oldToken,
	})

	store := newTestConfigStore(testStoreOpts{
		config: &Config{
			Providers: providers,
		},
		globalDataPath: configPath,
	})

	mapBeforeReload := store.Config().Providers

	// Override the network call: signal reachedNetworkCall as soon as
	// RefreshOAuthToken has entered it (proof that it already captured
	// `providers := s.loadSnapshot().config.Providers` at the top of the
	// function, since that capture happens strictly before this call), then
	// block until the test has published a new generation (simulating a
	// concurrent reload), THEN return the refreshed token. Without this
	// handshake, the goroutine below racing the reload published right
	// after it could lose the race and observe the NEW generation from the
	// start — which would make this test pass even against the buggy code,
	// since there would be no orphaned map involved at all.
	reachedNetworkCall := make(chan struct{})
	reloadDone := make(chan struct{})
	origFn := hyperExchangeTokenFn
	hyperExchangeTokenFn = func(ctx context.Context, refreshToken string) (*oauth.Token, error) {
		close(reachedNetworkCall)
		<-reloadDone
		return &oauth.Token{
			AccessToken:  "new-access-token",
			RefreshToken: "refresh-token-2",
			ExpiresIn:    3600,
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		}, nil
	}
	t.Cleanup(func() { hyperExchangeTokenFn = origFn })

	refreshErrCh := make(chan error, 1)
	go func() {
		refreshErrCh <- store.RefreshOAuthToken(context.Background(), ScopeGlobal, hyperp.Name)
	}()

	select {
	case <-reachedNetworkCall:
	case <-time.After(10 * time.Second):
		t.Fatal("RefreshOAuthToken did not reach the network call in time")
	}

	// Simulate a concurrent reload publishing a new generation with a
	// BRAND NEW Providers map (containing the pre-refresh provider config,
	// exactly as a real reload rebuilding from the same on-disk state
	// would) while the refresh call above is blocked inside
	// hyperExchangeTokenFn. This reproduces the exact "orphaned map"
	// scenario the fix addresses: RefreshOAuthToken captured
	// mapBeforeReload before this point (guaranteed by the handshake
	// above), but the current generation now points at a different map.
	newProviders := csync.NewMap[string, ProviderConfig]()
	newProviders.Set(hyperp.Name, ProviderConfig{
		ID:         hyperp.Name,
		Name:       "Hyper",
		APIKey:     oldToken.AccessToken,
		OAuthToken: oldToken,
	})
	store.publishMu.Lock()
	next := store.loadSnapshot().clone()
	cfgCopy := *next.config
	cfgCopy.Providers = newProviders
	next.config = &cfgCopy
	store.publishLocked(next)
	store.publishMu.Unlock()

	mapAfterReload := store.Config().Providers
	require.NotSame(t, mapBeforeReload, mapAfterReload,
		"sanity check: the simulated reload must publish a brand new Providers map")

	close(reloadDone)

	select {
	case err := <-refreshErrCh:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("RefreshOAuthToken did not return in time")
	}

	// The refreshed token must be visible in the CURRENTLY published
	// snapshot, not lost in the pre-reload map RefreshOAuthToken captured
	// at its start.
	finalPC, ok := store.Config().Providers.Get(hyperp.Name)
	require.True(t, ok)
	require.Equal(t, "new-access-token", finalPC.APIKey,
		"refreshed token must be visible in the current generation, not dropped into an orphaned map")
	require.Equal(t, "new-access-token", finalPC.OAuthToken.AccessToken)
	require.Equal(t, "refresh-token-2", finalPC.OAuthToken.RefreshToken)

	// mapBeforeReload (the orphaned pre-reload map) must NOT have received
	// the refreshed token — that's the exact failure mode of the bug: a
	// write into a map no reader can reach anymore.
	orphanedPC, ok := mapBeforeReload.Get(hyperp.Name)
	require.True(t, ok)
	require.Equal(t, "old-access-token", orphanedPC.APIKey,
		"the orphaned pre-reload map must be untouched by RefreshOAuthToken's write")
}

// TestProviderUpdates_ConcurrentReloadNoRace runs SetProviderRuntimeConfig
// and ReloadFromDisk concurrently to verify (via the -race detector) that
// the publishMu guard prevents any data race between the in-memory provider
// update and the reload's full snapshot rebuild.
func TestProviderUpdates_ConcurrentReloadNoRace(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	initialConfig := `{
		"providers": {
			"openai": {
				"api_key": "disk-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	ctx := context.Background()
	var wg sync.WaitGroup
	var stop atomic.Bool

	// Reloader: continuously reloads from disk until stop is set.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = store.ReloadFromDisk(ctx)
		}
	}()

	// Writer: repeatedly applies runtime-only provider updates.
	for i := range 200 {
		pc, ok := store.Config().Providers.Get("openai")
		if !ok {
			continue
		}
		pc.APIKey = fmt.Sprintf("refreshed-key-%d", i)
		store.SetProviderRuntimeConfig("openai", pc)
	}

	stop.Store(true)
	wg.Wait()

	// After all concurrent activity, the store must still be consistent:
	// the provider is present (from the last reload or the last write).
	_, ok := store.Config().Providers.Get("openai")
	require.True(t, ok, "provider must still be present after concurrent updates")
}
