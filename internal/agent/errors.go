package agent

import "errors"

var (
	ErrRequestCancelled = errors.New("request canceled by user")
	ErrSessionBusy      = errors.New("session is currently processing another request")
	ErrEmptyPrompt      = errors.New("prompt is empty")
	ErrSessionMissing   = errors.New("session id is missing")
	// ErrAgentShuttingDown is returned by Run when CancelAll has already
	// hard-stopped the agent (process shutdown). Returning an explicit
	// error — rather than silently queueing the call, or returning
	// (nil, nil) the way a plain "someone else owns this" would — is
	// deliberate: during shutdown no future turn will ever drain a queue,
	// so a silent accept is exactly the "accepted but never executed"
	// shape the P0-A/P0-B fixes exist to remove. The caller learns
	// immediately and can surface it.
	ErrAgentShuttingDown = errors.New("agent is shutting down")
)
