package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/stretchr/testify/require"
)

type mockBashPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
}

func (m *mockBashPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return true, nil
}

func (m *mockBashPermissionService) Grant(req permission.PermissionRequest) {}

func (m *mockBashPermissionService) Deny(req permission.PermissionRequest) {}

func (m *mockBashPermissionService) GrantPersistent(req permission.PermissionRequest) {}

func (m *mockBashPermissionService) AutoApproveSession(sessionID string) {}

func (m *mockBashPermissionService) InheritSessionAutoApprove(parentID, childID string) {}

func (m *mockBashPermissionService) SetSkipRequests(skip bool) {}

func (m *mockBashPermissionService) SetRunAllowlist(allowlist permission.RunAllowlist) {}

func (m *mockBashPermissionService) SkipRequests() bool {
	return false
}

func (m *mockBashPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

func (m *mockBashPermissionService) ListSessionPermissions(context.Context, string) ([]db.SessionPermission, error) {
	return nil, nil
}

func (m *mockBashPermissionService) UpdatePermissionEnabled(context.Context, string, bool) error {
	return nil
}

func (m *mockBashPermissionService) DeletePermission(context.Context, string) error {
	return nil
}

func TestBashTool_DefaultAutoBackgroundThreshold(t *testing.T) {
	workingDir := t.TempDir()
	tool := newBashToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "default threshold",
		Command:     "echo done",
	})

	require.False(t, resp.IsError)
	var meta BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.False(t, meta.Background)
	require.Empty(t, meta.ShellID)
	require.Contains(t, meta.Output, "done")
}

func TestBashTool_CustomAutoBackgroundThreshold(t *testing.T) {
	workingDir := t.TempDir()
	tool := newBashToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description:         "custom threshold",
		Command:             "sleep 1.5 && echo done",
		AutoBackgroundAfter: 1,
	})

	require.False(t, resp.IsError)
	var meta BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.True(t, meta.Background)
	require.NotEmpty(t, meta.ShellID)
	require.Contains(t, resp.Content, "moved to background")

	bgManager := shell.GetBackgroundShellManager()
	require.NoError(t, bgManager.Kill(context.Background(), meta.ShellID))
}

// TestBashTool_CtxCancelWaitsForConfirmedProcessKill is the regression test
// for a real bug found via live testing: bash.go's ctx-cancellation branch
// used to call bgManager.Kill(ctx, id) with the ALREADY-cancelled tool-call
// context as Kill's own wait-for-confirmation deadline. Since that ctx was
// already done (that's why the branch fired), Kill's internal
// `select { case <-shell.done: ...; case <-ctx.Done(): return ctx.Err() }`
// would take the already-ready ctx.Done() case almost immediately, so Kill
// returned right after firing shell.cancel() WITHOUT waiting for the
// underlying process tree to actually be confirmed torn down — racing
// ahead of the asynchronous OS-level kill (context.AfterFunc's callback in
// exec_windows.go/exec_unix.go runs in its own goroutine). Observed live
// consequence: the bash.exe/subprocess tree could still be running minutes
// after this tool call had already returned control to the agent, an
// escalating leak across repeated cancelled tool calls.
//
// Fixed by using a fresh, bounded context (killConfirmationTimeout) instead
// of the dead ctx as Kill's wait deadline. This proves: once the tool call
// returns after a ctx cancellation, the underlying BackgroundShell must
// already report IsDone() == true -- not just "asked to stop".
func TestBashTool_CtxCancelWaitsForConfirmedProcessKill(t *testing.T) {
	workingDir := t.TempDir()
	tool := newBashToolForTest(workingDir)
	bgManager := shell.GetBackgroundShellManager()

	before := make(map[string]bool, len(bgManager.List()))
	for _, id := range bgManager.List() {
		before[id] = true
	}

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), SessionIDContextKey, "cancel-confirm-session"))

	input, err := json.Marshal(BashParams{
		Description: "slow command for cancel-confirmation test",
		Command:     "sleep 5 && echo should-not-print",
		// Generous relative to how quickly we cancel below, so the test
		// reliably hits the ctx.Done() branch in bash.go, not the
		// auto-background-move branch.
		AutoBackgroundAfter: 30,
	})
	require.NoError(t, err)
	call := fantasy.ToolCall{ID: "test-call", Name: BashToolName, Input: string(input)}

	callDone := make(chan struct{})
	var runErr error
	go func() {
		defer close(callDone)
		_, runErr = tool.Run(ctx, call)
	}()

	// Wait for the new background shell to actually appear (bgManager.Start
	// has registered it) and grab a stable reference to it -- Kill() removes
	// the manager's own map entry (m.shells.Take), so we must hold this
	// pointer ourselves to still be able to check IsDone() afterward.
	var bgShell *shell.BackgroundShell
	require.Eventually(t, func() bool {
		for _, id := range bgManager.List() {
			if before[id] {
				continue
			}
			if sh, ok := bgManager.Get(id); ok {
				bgShell = sh
				return true
			}
		}
		return false
	}, 2*time.Second, 20*time.Millisecond, "expected a new background shell to appear for the slow command")

	cancel()

	select {
	case <-callDone:
	case <-time.After(killConfirmationTimeout + 3*time.Second):
		t.Fatal("bash tool call did not return within killConfirmationTimeout + slack after ctx cancellation -- the fix should bound this wait, not remove it")
	}

	require.ErrorIs(t, runErr, context.Canceled)
	require.True(t, bgShell.IsDone(),
		"the underlying shell must be CONFIRMED done (process tree torn down) by the moment the cancelled tool call returns -- not just signalled to stop and then abandoned")
}

type recordingPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
	requestCount int
	allow        bool
}

func (m *recordingPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	m.requestCount++
	return m.allow, nil
}

func (m *recordingPermissionService) Grant(req permission.PermissionRequest) {}

func (m *recordingPermissionService) Deny(req permission.PermissionRequest) {}

func (m *recordingPermissionService) GrantPersistent(req permission.PermissionRequest) {}

func (m *recordingPermissionService) AutoApproveSession(sessionID string) {}

func (m *recordingPermissionService) InheritSessionAutoApprove(parentID, childID string) {}

func (m *recordingPermissionService) SetSkipRequests(skip bool) {}

func (m *recordingPermissionService) SetRunAllowlist(allowlist permission.RunAllowlist) {}

func (m *recordingPermissionService) SkipRequests() bool {
	return false
}

func (m *recordingPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

// Fork patch: our permission.Service has three extra DB-backed methods
// (ListSessionPermissions / UpdatePermissionEnabled / DeletePermission)
// for the WebUI's permissions modal. Stub them so the upstream
// recordingPermissionService satisfies our extended interface.
func (m *recordingPermissionService) ListSessionPermissions(ctx context.Context, sessionID string) ([]db.SessionPermission, error) {
	return nil, nil
}

func (m *recordingPermissionService) UpdatePermissionEnabled(ctx context.Context, ruleID string, enabled bool) error {
	return nil
}

func (m *recordingPermissionService) DeletePermission(ctx context.Context, ruleID string) error {
	return nil
}

func newBashToolForTest(workingDir string) fantasy.AgentTool {
	permissions := &mockBashPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	return NewBashTool(permissions, workingDir, attribution, "test-model", nil)
}

func newBashToolWithRecordingPerms(workingDir string, allow bool) (fantasy.AgentTool, *recordingPermissionService) {
	perms := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  allow,
	}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	return NewBashTool(perms, workingDir, attribution, "test-model", nil), perms
}

func TestBashPermissionsParams_RunAllowlistCommandContract(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	svc := permission.NewPermissionService(t.Context(), "/tmp", false, nil, db.New(conn))
	svc.AutoApproveSession("run-session")
	allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
		Restrict:  true,
		AllowBash: []string{"git diff"},
	})
	require.NoError(t, err)
	svc.SetRunAllowlist(allowlist)

	allowed, err := svc.Request(t.Context(), permission.CreatePermissionRequest{
		SessionID: "run-session",
		ToolName:  BashToolName,
		Action:    "execute",
		Path:      "/tmp",
		Params:    BashPermissionsParams{Command: "git diff HEAD~1"},
	})
	require.NoError(t, err)
	require.True(t, allowed)

	denied, err := svc.Request(t.Context(), permission.CreatePermissionRequest{
		SessionID: "run-session",
		ToolName:  BashToolName,
		Action:    "execute",
		Path:      "/tmp",
		Params:    BashPermissionsParams{Command: "rm -rf /"},
	})
	require.NoError(t, err)
	require.False(t, denied)
}

func TestBashTool_ChainedCommandsRequirePermission(t *testing.T) {
	workingDir := t.TempDir()
	tool, perms := newBashToolWithRecordingPerms(workingDir, true)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	// ls && echo should trigger permission check.
	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "chained ls",
		Command:     "ls && echo done",
	})

	require.False(t, resp.IsError)
	require.Equal(t, 1, perms.requestCount, "chained command should trigger permission request")

	// Plain ls should NOT trigger permission check.
	perms.requestCount = 0
	resp = runBashTool(t, tool, ctx, BashParams{
		Description: "plain ls",
		Command:     "ls -la",
	})

	require.False(t, resp.IsError)
	require.Equal(t, 0, perms.requestCount, "plain ls should not trigger permission request")
}

func TestBashTool_ChainedCommandsDenied(t *testing.T) {
	workingDir := t.TempDir()
	tool, perms := newBashToolWithRecordingPerms(workingDir, false)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "chained ls denied",
		Command:     "ls && rm -rf /",
	})

	require.Equal(t, 1, perms.requestCount)
	require.Contains(t, resp.Content, "User denied permission")
}

func runBashTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params BashParams) fantasy.ToolResponse {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	call := fantasy.ToolCall{
		ID:    "test-call",
		Name:  BashToolName,
		Input: string(input),
	}

	resp, err := tool.Run(ctx, call)
	require.NoError(t, err)
	return resp
}

// TestBashTool_OnBackgroundCompleteFires proves the onBackgroundComplete
// callback (wired through BackgroundShell.OnDone in NewBashTool) is invoked
// once a command that auto-backgrounds reaches a terminal state. We shrink
// AutoBackgroundAfter so the command backgrounds quickly, then assert the
// stub fires within a few seconds.
func TestBashTool_OnBackgroundCompleteFires(t *testing.T) {
	workingDir := t.TempDir()

	type completion struct {
		sessionID string
		sh        *shell.BackgroundShell
	}
	done := make(chan completion, 1)
	permissions := &mockBashPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	onComplete := func(sessionID string, sh *shell.BackgroundShell) {
		select {
		case done <- completion{sessionID: sessionID, sh: sh}:
		default:
		}
	}
	tool := NewBashTool(permissions, workingDir, attribution, "test-model", onComplete)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "bg-complete-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description:         "auto-bg with callback",
		Command:             "sleep 1.5 && echo finished",
		AutoBackgroundAfter: 1,
	})

	require.False(t, resp.IsError)
	var meta BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.True(t, meta.Background, "command should have been auto-backgrounded")
	require.NotEmpty(t, meta.ShellID)

	select {
	case got := <-done:
		require.Equal(t, "bg-complete-session", got.sessionID)
		require.NotNil(t, got.sh)
		require.Equal(t, meta.ShellID, got.sh.ID)
	case <-time.After(10 * time.Second):
		t.Fatal("onBackgroundComplete was not invoked within 10s")
	}

	// Clean up the background shell if it is still tracked.
	_ = shell.GetBackgroundShellManager().Kill(context.Background(), meta.ShellID)
}
