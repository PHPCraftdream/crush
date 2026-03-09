package permission

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestService creates a permission service with no DB (in-memory only).
func newTestService(t *testing.T, skip bool, allowedTools []string) Service {
	t.Helper()
	return NewPermissionService(t.Context(), "/tmp", skip, allowedTools, nil)
}

func TestPermissionService_SkipMode(t *testing.T) {
	svc := newTestService(t, true, nil)
	result, err := svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp",
	})
	require.NoError(t, err)
	assert.True(t, result)
}

func TestPermissionService_AllowedTools(t *testing.T) {
	tests := []struct {
		allowedTools []string
		toolName     string
		action       string
		want         bool
	}{
		{[]string{"bash"}, "bash", "run", true},
		{[]string{"bash:run"}, "bash", "run", true},
		{[]string{"bash:read"}, "bash", "run", false},
		{[]string{"view"}, "bash", "run", false},
		{nil, "bash", "run", false},
	}
	for _, tt := range tests {
		svc := newTestService(t, false, tt.allowedTools)
		ps := svc.(*permissionService)
		key := tt.toolName + ":" + tt.action
		got := false
		for _, a := range ps.allowedTools {
			if a == key || a == tt.toolName {
				got = true
				break
			}
		}
		assert.Equal(t, tt.want, got, "tool=%s action=%s allowed=%v", tt.toolName, tt.action, tt.allowedTools)
	}
}

func TestPermissionService_Grant_OnceOnly(t *testing.T) {
	svc := newTestService(t, false, nil)
	events := svc.Subscribe(t.Context())

	var wg sync.WaitGroup
	var result1 bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		result1, _ = svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp",
		})
	}()

	ev := <-events
	svc.Grant(ev.Payload)
	wg.Wait()
	assert.True(t, result1)

	// Next identical request must ask again (no persistence).
	var result2 bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		result2, _ = svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp",
		})
	}()
	ev = <-events
	svc.Deny(ev.Payload)
	wg.Wait()
	assert.False(t, result2, "temporary grant must not persist")
}

// TestAlwaysAllow_SameSession verifies that GrantPersistent auto-approves
// subsequent requests within the same session.
func TestAlwaysAllow_SameSession(t *testing.T) {
	svc := newTestService(t, false, nil)
	events := svc.Subscribe(t.Context())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		svc.Request(t.Context(), CreatePermissionRequest{ //nolint:errcheck
			SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp",
		})
	}()

	ev := <-events
	// Simulate handler: only sends ID (the real production path).
	svc.GrantPersistent(PermissionRequest{ID: ev.Payload.ID})
	wg.Wait()

	// Same session — must be auto-approved without blocking.
	result, err := svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp",
	})
	require.NoError(t, err)
	assert.True(t, result, "same-session subsequent request must be auto-approved")
}

// TestAlwaysAllow_CrossSession verifies that GrantPersistent auto-approves
// requests from a different session (the core "always allow" contract).
func TestAlwaysAllow_CrossSession(t *testing.T) {
	svc := newTestService(t, false, nil)
	events := svc.Subscribe(t.Context())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		svc.Request(t.Context(), CreatePermissionRequest{ //nolint:errcheck
			SessionID: "session-A", ToolName: "bash", Action: "run", Path: "/tmp",
		})
	}()

	ev := <-events
	// Handler sends only ID — simulates production WebSocket handler.
	svc.GrantPersistent(PermissionRequest{ID: ev.Payload.ID})
	wg.Wait()

	// Different session — must still be auto-approved.
	result, err := svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "session-B", ToolName: "bash", Action: "run", Path: "/tmp",
	})
	require.NoError(t, err)
	assert.True(t, result, "cross-session request must be auto-approved after GrantPersistent")
}

// TestAlwaysAllow_DifferentTool verifies that persistent grants are scoped
// to the specific tool+action+path combination.
func TestAlwaysAllow_DifferentTool(t *testing.T) {
	svc := newTestService(t, false, nil)
	events := svc.Subscribe(t.Context())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		svc.Request(t.Context(), CreatePermissionRequest{ //nolint:errcheck
			SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp",
		})
	}()

	ev := <-events
	svc.GrantPersistent(PermissionRequest{ID: ev.Payload.ID})
	wg.Wait()

	// Different tool — must NOT be auto-approved.
	var result2 bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		result2, _ = svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "s1", ToolName: "write", Action: "run", Path: "/tmp",
		})
	}()
	ev = <-events
	svc.Deny(ev.Payload)
	wg.Wait()
	assert.False(t, result2, "different tool must not be auto-approved")
}

// TestAlwaysAllow_DifferentPath verifies path scoping.
func TestAlwaysAllow_DifferentPath(t *testing.T) {
	svc := newTestService(t, false, nil)
	events := svc.Subscribe(t.Context())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		svc.Request(t.Context(), CreatePermissionRequest{ //nolint:errcheck
			SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp/project",
		})
	}()

	ev := <-events
	svc.GrantPersistent(PermissionRequest{ID: ev.Payload.ID})
	wg.Wait()

	// Different path — must NOT be auto-approved.
	var result2 bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		result2, _ = svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "s1", ToolName: "bash", Action: "run", Path: "/other",
		})
	}()
	ev = <-events
	svc.Deny(ev.Payload)
	wg.Wait()
	assert.False(t, result2, "different path must not be auto-approved")
}
