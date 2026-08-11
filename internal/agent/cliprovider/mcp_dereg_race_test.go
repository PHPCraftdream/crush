package cliprovider

// BUG-2 regression test (found by a full-project @crush --role reviewer
// audit, 2026-08-11): registerQwenMCP/registerGeminiMCP write under a
// STABLE per-workingDir server name (see qwenMCPID/geminiMCPID), so two
// concurrent crush sessions in the same project integrating with the same
// CLI provider share one mcpServers[serverName] entry — register
// unconditionally overwrites it with whichever session called last.
// deregisterQwenMCP/deregisterGeminiMCP used to delete that entry
// unconditionally too, regardless of which session currently owned it: if
// session A finished after session B had already overwritten A's entry
// with B's own (different port), A's deregister deleted B's still-active
// registration, breaking B's ability to reconnect to its own MCP server.
//
// This test isolates os.UserHomeDir() (not the repo's own cached
// internal/home.Dir() — qwenSettingsPath/geminiSettingsPath call the
// stdlib directly, which re-reads HOME/USERPROFILE on every call, so
// t.Setenv works here without the package-init-time-caching pitfall found
// elsewhere in this codebase).
//
// REVERT CHECK PROCEDURE:
//  1. In mcpserver.go, revert deregisterQwenMCP/deregisterGeminiMCP to
//     unconditionally delete (drop the expectedURL/expectedAddr comparison
//     and parameter).
//  2. Run: go test ./internal/agent/cliprovider -run TestDeregisterQwenMCP_DoesNotClobberNewerSession -v
//  3. FAIL: session B's entry is gone after session A's (stale) deregister
//     call.
//  4. Restore the comparison and PASS.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func isolateHomeDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)        // POSIX
	t.Setenv("USERPROFILE", home) // Windows
	t.Setenv("HOMEDRIVE", "")     // avoid HOMEDRIVE+HOMEPATH overriding USERPROFILE on some Windows Go versions
	t.Setenv("HOMEPATH", "")
	return home
}

func readQwenMCPServerURL(t *testing.T, home, serverName string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".qwen", "settings.json"))
	if os.IsNotExist(err) {
		return "", false
	}
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		return "", false
	}
	entry, ok := mcpServers[serverName].(map[string]any)
	if !ok {
		return "", false
	}
	url, _ := entry["httpUrl"].(string)
	return url, true
}

func TestDeregisterQwenMCP_DoesNotClobberNewerSession(t *testing.T) {
	home := isolateHomeDir(t)
	const serverName = "crush-test-shared"

	// Session A registers first.
	require.NoError(t, registerQwenMCP(serverName, "127.0.0.1:5001", "token-a"))

	// Session B (concurrent, same project+provider) overwrites with its
	// own URL — this is the existing, accepted "last write wins" behavior
	// for register; the bug this test targets is specifically about
	// deregister.
	require.NoError(t, registerQwenMCP(serverName, "127.0.0.1:5002", "token-b"))

	url, ok := readQwenMCPServerURL(t, home, serverName)
	require.True(t, ok)
	require.Equal(t, "http://127.0.0.1:5002/mcp", url, "session B's registration must be the current one on disk")

	// Session A finishes and deregisters using ITS OWN (now-stale) addr —
	// must be a no-op, must NOT delete session B's still-active entry.
	deregisterQwenMCP(serverName, "127.0.0.1:5001")

	url, ok = readQwenMCPServerURL(t, home, serverName)
	require.True(t, ok, "session B's entry must still exist — session A's stale deregister must not have deleted it")
	require.Equal(t, "http://127.0.0.1:5002/mcp", url, "session B's entry must be untouched")

	// Session B finishes and deregisters using its OWN (current) addr —
	// this one must actually remove the entry.
	deregisterQwenMCP(serverName, "127.0.0.1:5002")

	_, ok = readQwenMCPServerURL(t, home, serverName)
	require.False(t, ok, "session B's own deregister, with the matching addr, must remove the entry")
}

func readGeminiMCPServerURL(t *testing.T, home, serverName string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".gemini", "settings.json"))
	if os.IsNotExist(err) {
		return "", false
	}
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		return "", false
	}
	entry, ok := mcpServers[serverName].(map[string]any)
	if !ok {
		return "", false
	}
	url, _ := entry["url"].(string)
	return url, true
}

func TestDeregisterGeminiMCP_DoesNotClobberNewerSession(t *testing.T) {
	home := isolateHomeDir(t)
	const serverName = "crush-test-shared"

	require.NoError(t, registerGeminiMCP(serverName, "127.0.0.1:5001", "token-a"))
	require.NoError(t, registerGeminiMCP(serverName, "127.0.0.1:5002", "token-b"))

	url, ok := readGeminiMCPServerURL(t, home, serverName)
	require.True(t, ok)
	require.Equal(t, "http://127.0.0.1:5002/mcp", url)

	// Session A's stale deregister must not clobber session B's entry.
	deregisterGeminiMCP(serverName, "127.0.0.1:5001")

	url, ok = readGeminiMCPServerURL(t, home, serverName)
	require.True(t, ok, "session B's entry must still exist")
	require.Equal(t, "http://127.0.0.1:5002/mcp", url)

	// Session B's own deregister, with the matching addr, must remove it.
	deregisterGeminiMCP(serverName, "127.0.0.1:5002")

	_, ok = readGeminiMCPServerURL(t, home, serverName)
	require.False(t, ok)
}
