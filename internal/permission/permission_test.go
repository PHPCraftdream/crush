package permission

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/pubsub"
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

// TestGrant_ConcurrentDuplicates_NoBlock verifies that concurrent
// duplicate Grant calls for the same permission ID all return promptly.
// Before the atomic Take fix, Grant used Get (no removal): with a buffered
// response channel of capacity 1 and a single Request that receives
// exactly once, a third concurrent Grant permanently blocked on the send
// (the lone receive unblocks only one of the blocked senders). Three
// grants are used because two only transiently block — the single receive
// drains one slot and unblocks the second sender — whereas the third has
// no remaining receiver and hangs forever on the unfixed code.
func TestGrant_ConcurrentDuplicates_NoBlock(t *testing.T) {
	svc := newTestService(t, false, nil)
	events := svc.Subscribe(t.Context())

	var wg sync.WaitGroup
	var result bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		result, _ = svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp",
		})
	}()

	ev := <-events

	// Fire three Grants concurrently for the same ID.
	const numGrants = 3
	done := make(chan struct{}, numGrants)
	for range numGrants {
		go func() {
			defer func() { done <- struct{}{} }()
			svc.Grant(ev.Payload)
		}()
	}

	for range numGrants {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("duplicate Grant blocked: did not return within 2s")
		}
	}
	wg.Wait()
	assert.True(t, result, "Request must be granted by one of the Grants")
}

// TestGrant_ThenDeny_SameID_NoBlock confirms a Grant that resolves a
// request followed by a Deny for the same ID does not block: after the
// Grant atomically consumes the pending entry via Take, the late Deny
// finds nothing (ok=false) and returns. This is a regression guard for
// the racing-responder scenario (web client + auto-approve, double-click).
func TestGrant_ThenDeny_SameID_NoBlock(t *testing.T) {
	svc := newTestService(t, false, nil)
	events := svc.Subscribe(t.Context())

	var wg sync.WaitGroup
	var result bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		result, _ = svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp",
		})
	}()

	ev := <-events

	// Grant resolves the request (non-blocking send into the buffered channel).
	svc.Grant(ev.Payload)
	wg.Wait()
	assert.True(t, result, "Request must be granted")

	// A late Deny for the already-resolved ID must return promptly, not block.
	done := make(chan struct{}, 1)
	go func() {
		defer func() { done <- struct{}{} }()
		svc.Deny(ev.Payload)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("late Deny for already-resolved ID blocked: did not return within 2s")
	}
}

// TestGrantThenDenyRace_SingleTerminalOutcome is a regression guard for
// P2.3: Grant/Deny/GrantPersistent used to publish their notification
// BEFORE calling the atomic pendingRequests.Take, so a losing concurrent
// call could still broadcast its outcome to subscribers even though it
// lost the race to resolve respCh. Firing Grant and Deny concurrently for
// the same ID must produce exactly one terminal (Granted or Denied)
// notification — never both, never neither.
func TestGrantThenDenyRace_SingleTerminalOutcome(t *testing.T) {
	svc := newTestService(t, false, nil)
	events := svc.Subscribe(t.Context())
	notifications := svc.SubscribeNotifications(t.Context())

	var wg sync.WaitGroup
	var result bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		result, _ = svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp",
		})
	}()

	ev := <-events
	// Drain the "requested" (unresolved) notification published by Request
	// itself, so it isn't mistaken for a terminal outcome below.
	initial := <-notifications
	require.False(t, initial.Payload.Granted || initial.Payload.Denied, "unexpected terminal notification before Grant/Deny raced")

	var raceWG sync.WaitGroup
	raceWG.Add(2)
	go func() {
		defer raceWG.Done()
		svc.Grant(ev.Payload)
	}()
	go func() {
		defer raceWG.Done()
		svc.Deny(ev.Payload)
	}()
	raceWG.Wait()
	wg.Wait()

	// Exactly one terminal notification must have been published for this
	// tool call ID. Collect notifications with a short bounded wait since
	// the loser must publish nothing at all.
	terminalCount := 0
	var lastGranted bool
	for {
		select {
		case n := <-notifications:
			if n.Payload.ToolCallID != ev.Payload.ToolCallID {
				continue
			}
			if n.Payload.Granted || n.Payload.Denied {
				terminalCount++
				lastGranted = n.Payload.Granted
			}
		case <-time.After(300 * time.Millisecond):
			goto done
		}
	}
done:
	assert.Equal(t, 1, terminalCount, "exactly one terminal notification (granted xor denied) must be published for a raced Grant/Deny on the same ID")
	assert.Equal(t, lastGranted, result, "the published terminal notification must match the outcome the Request actually observed")
}

// TestGrantPersistentAfterDenyWins_DoesNotPersist is a regression guard for
// P2.3: GrantPersistent used to publish its notification and write the
// persistent grant to the DB even when it lost the pendingRequests.Take
// race to a concurrent Deny. This verifies that once Deny has already won
// (consumed the pending entry), a subsequent GrantPersistent call for the
// same ID is a pure no-op: no notification, no DB row.
func TestGrantPersistentAfterDenyWins_DoesNotPersist(t *testing.T) {
	svc := newTestService(t, false, nil)
	events := svc.Subscribe(t.Context())

	var wg sync.WaitGroup
	var result bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		result, _ = svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp",
		})
	}()

	ev := <-events

	// Deny wins the race first (synchronously, before GrantPersistent runs).
	svc.Deny(ev.Payload)
	wg.Wait()
	assert.False(t, result, "Request must observe Deny's outcome")

	// GrantPersistent arrives late for the same ID — Take must report
	// ok=false, so it must not publish a granted notification nor persist
	// a grant row to the DB.
	notifications := svc.SubscribeNotifications(t.Context())
	svc.GrantPersistent(PermissionRequest{ID: ev.Payload.ID})

	select {
	case n := <-notifications:
		t.Fatalf("late GrantPersistent must not publish any notification after losing the race, got %+v", n.Payload)
	case <-time.After(300 * time.Millisecond):
		// Expected: no notification.
	}

	// The persistent grant must not have been written: a fresh request for
	// the exact same tool/action/path must still prompt (not auto-approve).
	var result2 bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		result2, _ = svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "s1", ToolName: "bash", Action: "run", Path: "/tmp",
		})
	}()
	select {
	case ev2 := <-events:
		svc.Deny(ev2.Payload)
	case <-time.After(2 * time.Second):
		t.Fatal("expected a fresh permission prompt (no persistent grant should have been written), but Request auto-approved or hung")
	}
	wg.Wait()
	assert.False(t, result2, "GrantPersistent that lost the Take race must not have persisted a grant")
}

// TestRequest_CrossSession_DoesNotSerialize is a regression guard for H-4:
// permissionService.Request used to hold a single process-wide requestMu for
// its entire body, including the blocking wait for a human response. That
// meant an unanswered permission prompt in one session blocked every other
// session's Request from even being published, defeating the fork's N
// concurrent sessions / web-UI design (see CLAUDE.md).
//
// This starts a Request for session A and leaves it unanswered, then starts
// a second Request for session B and asserts the second one's event is
// published promptly — i.e. it never waited on the first session's
// still-pending response.
func TestRequest_CrossSession_DoesNotSerialize(t *testing.T) {
	svc := newTestService(t, false, nil)
	events := svc.Subscribe(t.Context())

	// Session A's request is started but deliberately left unanswered for
	// the duration of the test.
	ctxA, cancelA := context.WithCancel(t.Context())
	defer cancelA()
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		svc.Request(ctxA, CreatePermissionRequest{ //nolint:errcheck
			SessionID: "session-A", ToolName: "bash", Action: "run", Path: "/tmp",
		})
	}()

	evA := <-events
	require.Equal(t, "session-A", evA.Payload.SessionID)

	// Session B's request must be publishable without waiting for A's
	// response. Before the fix, this second Request blocked on requestMu
	// (held by A's in-flight call) until the test timeout.
	doneB := make(chan struct{})
	go func() {
		defer close(doneB)
		svc.Request(t.Context(), CreatePermissionRequest{ //nolint:errcheck
			SessionID: "session-B", ToolName: "write", Action: "run", Path: "/tmp",
		})
	}()

	select {
	case evB := <-events:
		require.Equal(t, "session-B", evB.Payload.SessionID)
		svc.Deny(evB.Payload)
	case <-time.After(2 * time.Second):
		t.Fatal("session B's Request never published its event: appears to be serialized behind session A's unanswered request")
	}

	select {
	case <-doneB:
	case <-time.After(2 * time.Second):
		t.Fatal("session B's Request did not return after Deny")
	}

	// Unblock session A's still-pending request so its goroutine can exit.
	svc.Deny(evA.Payload)
	select {
	case <-doneA:
	case <-time.After(2 * time.Second):
		t.Fatal("session A's Request did not return after Deny")
	}
}

// TestGrantPersistent_ConcurrentDifferentSessions_NoCrossContamination
// guards the activeRequests keying introduced for H-4: activeRequest used to
// be a single shared *PermissionRequest slot, filled in by whichever Request
// was currently inside the (now-removed) requestMu critical section.
// activeRequests is now keyed by permission ID so two concurrent requests
// from different sessions each get GrantPersistent'd with their own
// tool/action/path — not whichever request happened to write the shared
// slot last.
func TestGrantPersistent_ConcurrentDifferentSessions_NoCrossContamination(t *testing.T) {
	svc := newTestService(t, false, nil)
	events := svc.Subscribe(t.Context())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		svc.Request(t.Context(), CreatePermissionRequest{ //nolint:errcheck
			SessionID: "session-A", ToolName: "bash", Action: "run", Path: "/tmp/a",
		})
	}()
	go func() {
		defer wg.Done()
		svc.Request(t.Context(), CreatePermissionRequest{ //nolint:errcheck
			SessionID: "session-B", ToolName: "write", Action: "run", Path: "/tmp/b",
		})
	}()

	var evA, evB pubsub.Event[PermissionRequest]
	for range 2 {
		ev := <-events
		switch ev.Payload.SessionID {
		case "session-A":
			evA = ev
		case "session-B":
			evB = ev
		default:
			t.Fatalf("unexpected session id %q", ev.Payload.SessionID)
		}
	}
	require.NotEmpty(t, evA.Payload.ID)
	require.NotEmpty(t, evB.Payload.ID)

	// GrantPersistent for A, sending only the ID (the real production
	// path) — must reconstruct A's own tool/action/path, not B's.
	svc.GrantPersistent(PermissionRequest{ID: evA.Payload.ID})
	svc.Deny(evB.Payload)
	wg.Wait()

	// A's grant must match A's own path only.
	resultA, err := svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "session-A", ToolName: "bash", Action: "run", Path: "/tmp/a",
	})
	require.NoError(t, err)
	assert.True(t, resultA, "session-A's own tool/path must be auto-approved")

	// B's tool/path must NOT have been persisted (it was Denied, not
	// GrantPersistent'd) — if activeRequests cross-contaminated, A's
	// GrantPersistent could have persisted B's path instead.
	var resultB bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		resultB, _ = svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "session-B", ToolName: "write", Action: "run", Path: "/tmp/b",
		})
	}()
	select {
	case ev := <-events:
		svc.Deny(ev.Payload)
	case <-time.After(2 * time.Second):
		t.Fatal("session-B request unexpectedly auto-approved (cross-contamination) or hung")
	}
	wg.Wait()
	assert.False(t, resultB, "session-B's path must not have been persisted by session-A's GrantPersistent")
}
