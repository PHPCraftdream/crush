package permission

// Fork patch: permission rules are persisted per-session in the SQLite store
// (see DB migration `20260308000002_add_session_permissions.sql` and the
// `enabled` flag from `20260312000002`). The Service interface adds
// ListSessionPermissions / UpdatePermissionEnabled / DeletePermission for the
// web UI's permissions modal. Upstream keeps the rules in memory only.
//
// The PermissionRequest / PermissionNotification structs lost their JSON tags
// on purpose: the web wire format is defined in `internal/server/protocol.go`,
// not on these in-memory types — keeping the tags would cause subtle drift
// between the two layers.
//
// See CHANGELOG.fork.md section 4.C (DB migrations) and section 4.A
// (WebSocket protocol) before resolving a merge conflict in this file.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
)

// hookApprovalKey is the unexported context key used to mark a tool call as
// pre-approved by a PreToolUse hook. The value is the tool call ID so an
// approval can't be reused across calls that happen to share a context.
type hookApprovalKey struct{}

// WithHookApproval returns a context that marks the given tool call ID as
// pre-approved by a hook. When the permission service sees a matching
// request it short-circuits the normal prompt and grants immediately.
func WithHookApproval(ctx context.Context, toolCallID string) context.Context {
	return context.WithValue(ctx, hookApprovalKey{}, toolCallID)
}

// hookApproved reports whether the context carries a hook approval for the
// given tool call ID.
func hookApproved(ctx context.Context, toolCallID string) bool {
	if toolCallID == "" {
		return false
	}
	v, _ := ctx.Value(hookApprovalKey{}).(string)
	return v == toolCallID
}

type CreatePermissionRequest struct {
	SessionID   string `json:"session_id"`
	ToolCallID  string `json:"tool_call_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	Action      string `json:"action"`
	Params      any    `json:"params"`
	Path        string `json:"path"`
}

type PermissionNotification struct {
	ToolCallID string
	Granted    bool
	Denied     bool
}

type PermissionRequest struct {
	ID          string
	SessionID   string
	ToolCallID  string
	ToolName    string
	Description string
	Action      string
	Params      any
	Path        string
}

type Service interface {
	pubsub.Subscriber[PermissionRequest]
	GrantPersistent(permission PermissionRequest)
	Grant(permission PermissionRequest)
	Deny(permission PermissionRequest)
	Request(ctx context.Context, opts CreatePermissionRequest) (bool, error)
	AutoApproveSession(sessionID string)
	SetSkipRequests(skip bool)
	SkipRequests() bool
	// SetRunAllowlist arms the restricted-run allowlist used by
	// `crush run`. Pass the zero value (or call with IsRestricted ==
	// false) to restore the legacy auto-approve-everything behaviour.
	// The allowlist only governs the non-interactive auto-approve path;
	// it never affects interactive (TUI / web) permission flows.
	SetRunAllowlist(allowlist RunAllowlist)
	SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification]
	ListSessionPermissions(sessionID string) ([]db.SessionPermission, error)
	UpdatePermissionEnabled(ruleID string, enabled bool) error
	DeletePermission(ruleID string) error
}

type permissionService struct {
	*pubsub.Broker[PermissionRequest]

	notificationBroker *pubsub.Broker[PermissionNotification]
	workingDir         string
	// Fork patch (concurrency): the upstream in-memory grant cache was
	// removed. Request now consults the DB on every call via
	// MatchSessionPermission so a grant created in another crush process
	// (parallel `crush run`) is immediately visible without restart. See
	// CHANGELOG.fork.md and the original fork note about why this used
	// to be a []PermissionRequest slice.
	pendingRequests       *csync.Map[string, chan bool]
	autoApproveSessions   map[string]bool
	autoApproveSessionsMu sync.RWMutex
	skip                  atomic.Bool
	allowedTools          []string
	q                     *db.Queries

	// used to make sure we only process one request at a time
	requestMu       sync.Mutex
	activeRequest   *PermissionRequest
	activeRequestMu sync.Mutex

	// runAllowlistGate gates the non-interactive auto-approve path. When
	// its compiled allowlist IsRestricted, AutoApproveSession'd sessions
	// no longer get blanket approval — each request must clear the
	// allowlist instead, or it is denied cleanly without waiting for a
	// UI that isn't there. See runallowlist.go.
	runAllowlistGate runAllowlistGate
}

func (s *permissionService) GrantPersistent(permission PermissionRequest) {
	// The handler may send only the ID; fill in the rest from activeRequest.
	s.activeRequestMu.Lock()
	if s.activeRequest != nil && s.activeRequest.ID == permission.ID {
		permission = *s.activeRequest
	}
	s.activeRequestMu.Unlock()

	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: permission.ToolCallID,
		Granted:    true,
	})
	respCh, ok := s.pendingRequests.Get(permission.ID)
	if ok {
		respCh <- true
	}

	// Fork patch (concurrency): the in-memory append was dropped — the DB
	// is the single source of truth so other processes see this grant on
	// their next Request without a restart. See CHANGELOG.fork.md.
	//
	// session_id is intentionally stored as "" so the grant matches
	// requests from ANY session. This preserves the upstream loader's
	// behaviour (which used to overwrite session_id with "" when reading
	// rows into the in-memory cache) and the cross-session contract
	// exercised by TestAlwaysAllow_CrossSession. MatchSessionPermission's
	// WHERE clause (session_id = '' OR session_id = ?) handles the read.
	// Guard: only persist if we have a valid permission (activeRequest matched).
	if s.q != nil && permission.ToolName != "" && permission.Action != "" {
		if err := s.q.CreateSessionPermission(context.Background(), db.CreateSessionPermissionParams{
			ID:        uuid.New().String(),
			SessionID: "",
			ToolName:  permission.ToolName,
			Action:    permission.Action,
			Path:      permission.Path,
		}); err != nil {
			slog.Warn("permission: failed to persist grant", "err", err)
		}
	}

	s.activeRequestMu.Lock()
	if s.activeRequest != nil && s.activeRequest.ID == permission.ID {
		s.activeRequest = nil
	}
	s.activeRequestMu.Unlock()
}

func (s *permissionService) Grant(permission PermissionRequest) {
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: permission.ToolCallID,
		Granted:    true,
	})
	respCh, ok := s.pendingRequests.Get(permission.ID)
	if ok {
		respCh <- true
	}

	s.activeRequestMu.Lock()
	if s.activeRequest != nil && s.activeRequest.ID == permission.ID {
		s.activeRequest = nil
	}
	s.activeRequestMu.Unlock()
}

func (s *permissionService) Deny(permission PermissionRequest) {
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: permission.ToolCallID,
		Granted:    false,
		Denied:     true,
	})
	respCh, ok := s.pendingRequests.Get(permission.ID)
	if ok {
		respCh <- false
	}

	s.activeRequestMu.Lock()
	if s.activeRequest != nil && s.activeRequest.ID == permission.ID {
		s.activeRequest = nil
	}
	s.activeRequestMu.Unlock()
}

func (s *permissionService) Request(ctx context.Context, opts CreatePermissionRequest) (bool, error) {
	if s.skip.Load() {
		return true, nil
	}

	// Check if the tool/action combination is in the allowlist
	commandKey := opts.ToolName + ":" + opts.Action
	if slices.Contains(s.allowedTools, commandKey) || slices.Contains(s.allowedTools, opts.ToolName) {
		return true, nil
	}

	// A PreToolUse hook that returned decision=allow stamps the context
	// with the tool call ID. Treat that as a pre-approval and skip the
	// prompt entirely. We still publish a granted notification so the UI
	// and audit subscribers see the outcome.
	if hookApproved(ctx, opts.ToolCallID) {
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	s.requestMu.Lock()
	defer s.requestMu.Unlock()

	// tell the UI that a permission was requested
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: opts.ToolCallID,
	})

	s.autoApproveSessionsMu.RLock()
	autoApprove := s.autoApproveSessions[opts.SessionID]
	s.autoApproveSessionsMu.RUnlock()

	if autoApprove {
		// Restricted-run gate. In a non-interactive `crush run` the
		// session is auto-approve, but if the operator armed a
		// restricted allowlist (--restrict-run / permissions.run.restrict)
		// we must not blanket-grant. Consult the allowlist; unmatched
		// requests are denied cleanly here so the agent sees a fast
		// "no" instead of hanging on a UI that doesn't exist.
		gate := s.runAllowlistGate.load()
		if gate.IsRestricted() && !gate.allowsRequest(opts) {
			s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
				ToolCallID: opts.ToolCallID,
				Granted:    false,
				Denied:     true,
			})
			return false, nil
		}
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	fileInfo, err := os.Stat(opts.Path)
	dir := opts.Path
	if err == nil {
		if fileInfo.IsDir() {
			dir = opts.Path
		} else {
			dir = filepath.Dir(opts.Path)
		}
	}

	if dir == "." {
		dir = s.workingDir
	}
	permission := PermissionRequest{
		ID:          uuid.New().String(),
		Path:        dir,
		SessionID:   opts.SessionID,
		ToolCallID:  opts.ToolCallID,
		ToolName:    opts.ToolName,
		Description: opts.Description,
		Action:      opts.Action,
		Params:      opts.Params,
	}

	// Fork patch (concurrency): query the persistent-grant table directly
	// on every Request instead of consulting an in-memory cache that was
	// populated only at startup. Under parallel `crush run` processes,
	// the old cache made an "always allow" granted in process A invisible
	// to process B until B restarted, causing B to re-prompt (or block in
	// non-interactive mode). Query cost is one indexed SELECT; the cache
	// scan it replaces was O(N) anyway. See CHANGELOG.fork.md.
	if s.q != nil {
		if _, err := s.q.MatchSessionPermission(ctx, db.MatchSessionPermissionParams{
			ToolName:  permission.ToolName,
			Action:    permission.Action,
			Path:      permission.Path,
			SessionID: permission.SessionID,
		}); err == nil {
			s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
				ToolCallID: opts.ToolCallID,
				Granted:    true,
			})
			return true, nil
		}
	}

	s.activeRequestMu.Lock()
	s.activeRequest = &permission
	s.activeRequestMu.Unlock()

	respCh := make(chan bool, 1)
	s.pendingRequests.Set(permission.ID, respCh)
	defer s.pendingRequests.Del(permission.ID)

	// Publish the request
	s.Publish(pubsub.CreatedEvent, permission)

	select {
	case <-ctx.Done():
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			ToolCallID: permission.ToolCallID,
			Denied:     true,
		})
		s.activeRequestMu.Lock()
		if s.activeRequest != nil && s.activeRequest.ID == permission.ID {
			s.activeRequest = nil
		}
		s.activeRequestMu.Unlock()
		return false, ctx.Err()
	case granted := <-respCh:
		return granted, nil
	}
}

func (s *permissionService) AutoApproveSession(sessionID string) {
	s.autoApproveSessionsMu.Lock()
	s.autoApproveSessions[sessionID] = true
	s.autoApproveSessionsMu.Unlock()
}

func (s *permissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification] {
	return s.notificationBroker.Subscribe(ctx)
}

func (s *permissionService) SetSkipRequests(skip bool) {
	s.skip.Store(skip)
}

func (s *permissionService) SkipRequests() bool {
	return s.skip.Load()
}

// SetRunAllowlist arms or clears the restricted-run allowlist. The
// allowlist is consulted on the auto-approve path only (see Request),
// so interactive sessions are unaffected.
func (s *permissionService) SetRunAllowlist(allowlist RunAllowlist) {
	s.runAllowlistGate.store(allowlist)
}

func NewPermissionService(ctx context.Context, workingDir string, skip bool, allowedTools []string, q *db.Queries) Service {
	svc := &permissionService{
		Broker:              pubsub.NewBroker[PermissionRequest](),
		notificationBroker:  pubsub.NewBroker[PermissionNotification](),
		workingDir:          workingDir,
		autoApproveSessions: make(map[string]bool),
		allowedTools:        allowedTools,
		pendingRequests:     csync.NewMap[string, chan bool](),
		q:                   q,
	}
	// Fork merge note (origin/main 6b312bee "fix: potential data race on
	// permissionService"): upstream made skip atomic.Bool and initialises it
	// after struct construction. Their pattern preserved.
	svc.skip.Store(skip)

	// Fork patch (concurrency): startup pre-load into an in-memory cache
	// was removed. Request now queries the DB directly on every call so
	// grants from other processes are immediately visible. See
	// CHANGELOG.fork.md.

	return svc
}

func (s *permissionService) ListSessionPermissions(sessionID string) ([]db.SessionPermission, error) {
	return s.q.ListSessionPermissions(context.Background(), sessionID)
}

func (s *permissionService) UpdatePermissionEnabled(ruleID string, enabled bool) error {
	var enabledInt int64
	if enabled {
		enabledInt = 1
	}
	return s.q.UpdatePermissionEnabled(context.Background(), db.UpdatePermissionEnabledParams{
		Enabled: enabledInt,
		ID:      ruleID,
	})
}

func (s *permissionService) DeletePermission(ruleID string) error {
	return s.q.DeletePermission(context.Background(), ruleID)
}
