package agent

import (
	"fmt"
)

// ErrCallAlreadyAttempted wraps an error that occurred AFTER the call
// started executing (i.e., after createUserMessage successfully persisted
// the user message to the database).
//
// This type is used by the retry tails (restartOrphanedWithRetry and
// startDetachedRun) to distinguish between two failure modes:
//
//  1. Error BEFORE user message creation (e.g., OS lock contention,
//     permission denied during lock acquisition): the call never executed,
//     requeuing is safe and necessary to prevent data loss (task #340).
//
//  2. Error AFTER user message creation (e.g., provider 5xx error mid-stream,
//     DB write failure after user message, summary error): the call has
//     already left a persistent trace, requeuing would cause duplicates.
//
// The wrapping point is the earliest moment after which we can confidently
// say the call "started executing": immediately after createUserMessage
// succeeds (agent.go:1676). Errors from that point forward in runTurn are
// wrapped so the retry tails can check via errors.As and skip requeue.
//
// This is the typed signal designed to fix task #339 (P0, regression from
// commit 37f1820d) while preserving task #340's protection (data loss on
// retry exhaustion).
type ErrCallAlreadyAttempted struct {
	Err error
}

func (e *ErrCallAlreadyAttempted) Error() string {
	return fmt.Sprintf("call already attempted: %v", e.Err)
}

func (e *ErrCallAlreadyAttempted) Unwrap() error {
	return e.Err
}
