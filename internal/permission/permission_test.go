package permission

import (
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestService creates a permission service backed by a real temp-dir
// SQLite DB. Fork patch (concurrency): the in-memory grant cache was
// removed and persistent-grant lookup now goes through the DB on every
// Request, so tests need a non-nil *db.Queries to exercise the
// auto-approve path. The nil-q legacy mode would silently skip the
// match and cause Always-Allow tests to hang forever waiting for a
// permission event that the test then has to drain manually.
func newTestService(t *testing.T, skip bool, allowedTools []string) Service {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return NewPermissionService(t.Context(), "/tmp", skip, allowedTools, db.New(conn))
}

func TestSkipRace(t *testing.T) {
	// Fork merge note (origin/main 6b312bee "fix: potential data race on
	// permissionService"): kept the test, adapted the call site to our
	// extended NewPermissionService signature (ctx + Queries).
	svc := newTestService(t, false, nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		svc.SetSkipRequests(true)
	}()
	go func() {
		defer wg.Done()
		svc.SkipRequests()
	}()
	wg.Wait()
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

// TestDisabledPermissions_NotMatched verifies that a row with enabled=0
// in session_permissions is NOT auto-approved on a subsequent Request.
// Fork patch (concurrency): the in-memory cache was removed and the
// check moved to the SQL WHERE clause (MatchSessionPermission.enabled=1),
// so the test now exercises Request directly rather than the loader.
func TestDisabledPermissions_NotMatched(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	q := db.New(conn)

	// Enabled rule for bash:run on /tmp — should be auto-approved.
	err = q.CreateSessionPermission(t.Context(), db.CreateSessionPermissionParams{
		ID: "perm-enabled", SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp",
	})
	require.NoError(t, err)

	// Disabled rule for write:run on /tmp — should NOT auto-approve.
	err = q.CreateSessionPermission(t.Context(), db.CreateSessionPermissionParams{
		ID: "perm-disabled", SessionID: "s1", ToolName: "write", Action: "run", Path: "/tmp",
	})
	require.NoError(t, err)
	err = q.UpdatePermissionEnabled(t.Context(), db.UpdatePermissionEnabledParams{
		ID: "perm-disabled", Enabled: 0,
	})
	require.NoError(t, err)

	svc := NewPermissionService(t.Context(), "/tmp", false, nil, q)
	events := svc.Subscribe(t.Context())

	// Enabled rule auto-approves without raising an event.
	result1, err := svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp",
	})
	require.NoError(t, err)
	assert.True(t, result1, "enabled rule must auto-approve")

	// Disabled rule must surface as a prompt; Deny it to unblock the test.
	var wg sync.WaitGroup
	wg.Add(1)
	var result2 bool
	go func() {
		defer wg.Done()
		result2, _ = svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "s1", ToolName: "write", Action: "run", Path: "/tmp",
		})
	}()
	// Fork patch (concurrency): bounded receive — if Request silently
	// auto-approves the disabled rule (regression) the event never fires
	// and an unbounded `<-events` would hang the suite until go test's
	// outer timeout.
	select {
	case ev := <-events:
		svc.Deny(ev.Payload)
	case <-time.After(2 * time.Second):
		t.Fatal("disabled rule produced no permission event within 2s")
	}
	wg.Wait()
	assert.False(t, result2, "disabled rule must NOT auto-approve")
}
