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
	SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification]
	ListSessionPermissions(sessionID string) ([]db.SessionPermission, error)
	UpdatePermissionEnabled(ruleID string, enabled bool) error
	DeletePermission(ruleID string) error
}

type permissionService struct {
	*pubsub.Broker[PermissionRequest]

	notificationBroker    *pubsub.Broker[PermissionNotification]
	workingDir            string
	sessionPermissions    []PermissionRequest
	sessionPermissionsMu  sync.RWMutex
	pendingRequests       *csync.Map[string, chan bool]
	autoApproveSessions   map[string]bool
	autoApproveSessionsMu sync.RWMutex
	skip                  bool
	allowedTools          []string
	q                     *db.Queries

	// used to make sure we only process one request at a time
	requestMu       sync.Mutex
	activeRequest   *PermissionRequest
	activeRequestMu sync.Mutex
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

	// Persistent permissions are now session-specific.
	s.sessionPermissionsMu.Lock()
	s.sessionPermissions = append(s.sessionPermissions, permission)
	s.sessionPermissionsMu.Unlock()

	// Persist to DB so it survives restarts.
	// Guard: only persist if we have a valid permission (activeRequest matched).
	if s.q != nil && permission.ToolName != "" && permission.Action != "" {
		if err := s.q.CreateSessionPermission(context.Background(), db.CreateSessionPermissionParams{
			ID:        uuid.New().String(),
			SessionID: permission.SessionID,
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
	if s.skip {
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

	// tell the UI that a permission was requested
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: opts.ToolCallID,
	})
	s.requestMu.Lock()
	defer s.requestMu.Unlock()

	s.autoApproveSessionsMu.RLock()
	autoApprove := s.autoApproveSessions[opts.SessionID]
	s.autoApproveSessionsMu.RUnlock()

	if autoApprove {
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	// Check session-specific YOLO mode in database.
	if s.q != nil {
		session, err := s.q.GetSessionByID(ctx, opts.SessionID)
		if err == nil && session.YoloEnabled != 0 {
			s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
				ToolCallID: opts.ToolCallID,
				Granted:    true,
			})
			return true, nil
		}
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

	s.sessionPermissionsMu.RLock()
	slog.Debug("permission: checking persistent grants",
		"count", len(s.sessionPermissions),
		"req_tool", permission.ToolName,
		"req_action", permission.Action,
		"req_session", permission.SessionID,
		"req_path", permission.Path,
	)
	for _, p := range s.sessionPermissions {
		// Skip empty/corrupt entries loaded from DB.
		if p.ToolName == "" || p.Action == "" {
			continue
		}
		sessionMatch := p.SessionID == "" || p.SessionID == permission.SessionID
		toolMatch := p.ToolName == permission.ToolName
		actionMatch := p.Action == permission.Action
		pathMatch := p.Path == permission.Path
		slog.Debug("permission: comparing grant",
			"grant_tool", p.ToolName, "tool_match", toolMatch,
			"grant_action", p.Action, "action_match", actionMatch,
			"grant_session", p.SessionID, "session_match", sessionMatch,
			"grant_path", p.Path, "path_match", pathMatch,
		)
		if toolMatch && actionMatch && sessionMatch && pathMatch {
			s.sessionPermissionsMu.RUnlock()
			s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
				ToolCallID: opts.ToolCallID,
				Granted:    true,
			})
			return true, nil
		}
	}
	s.sessionPermissionsMu.RUnlock()

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
	s.skip = skip
}

func (s *permissionService) SkipRequests() bool {
	return s.skip
}

func NewPermissionService(ctx context.Context, workingDir string, skip bool, allowedTools []string, q *db.Queries) Service {
	svc := &permissionService{
		Broker:              pubsub.NewBroker[PermissionRequest](),
		notificationBroker:  pubsub.NewBroker[PermissionNotification](),
		workingDir:          workingDir,
		sessionPermissions:  make([]PermissionRequest, 0),
		autoApproveSessions: make(map[string]bool),
		skip:                skip,
		allowedTools:        allowedTools,
		pendingRequests:     csync.NewMap[string, chan bool](),
		q:                   q,
	}

	// Load previously persisted "always allow" permissions from DB.
	if q != nil {
		if rows, err := q.ListAllSessionPermissions(ctx); err == nil {
			for _, r := range rows {
				if r.Enabled == 0 {
					continue
				}
				svc.sessionPermissions = append(svc.sessionPermissions, PermissionRequest{
					ID:       r.ID,
					// SessionID is intentionally empty: persistent permissions
					// match requests from any session.
					ToolName: r.ToolName,
					Action:   r.Action,
					Path:     r.Path,
				})
			}
		} else {
			slog.Warn("permission: failed to load persisted permissions", "err", err)
		}
	}

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
