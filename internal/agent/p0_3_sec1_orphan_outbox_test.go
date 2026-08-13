package agent

// P0-3 and SEC-1 test: orphan recovery with durable outbox and prompt redaction.
//
// This test verifies that when durable enqueue fails:
// 1. The call is written to the orphan outbox (durable record exists)
// 2. No raw prompt is leaked in ERROR logs (SEC-1)
//
// REVERT CHECK PROCEDURE (P0-3 durability):
//  1. In agent.go's restartOrphanedWithRetry, comment out the outbox write
//     (a.sessions.WriteToOrphanOutbox(...) line) and restore the old
//     a.startBoundedDetachedRun(call) call.
//  2. Run: go test ./internal/agent -run TestP0_3_OrphanOutboxDurability -v
//  3. FAIL: No durable record exists in orphan_call_outbox after restartOrphanedWithRetry
//
// REVERT CHECK PROCEDURE (SEC-1 prompt redaction):
//  1. In agent.go's restartOrphanedWithRetry, remove the promptLength/promptHash
//     fields and add back raw "prompt" field in log calls.
//  2. Run: go test ./internal/agent -run TestSEC1_PromptRedaction -v
//  3. FAIL: Raw prompt appears in log output

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/cliprovider"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// enqueueFailingSessions wraps a real session.Service and fails ONLY
// EnqueueRunQueueEntry — every other call (message-adjacent session
// bookkeeping, etc.) goes through to the real, still-open DB.
type enqueueFailingSessions struct {
	session.Service
}

var errSimulatedEnqueueFailure = errors.New("p0-3 test: simulated durable enqueue failure")

func (e *enqueueFailingSessions) EnqueueRunQueueEntry(ctx context.Context, idempotencyKey, sessionID string, callData []byte) error {
	return errSimulatedEnqueueFailure
}

// TestP0_3_OrphanOutboxDurability verifies that when durable enqueue fails,
// the call is written to the orphan outbox (durable record exists).
func TestP0_3_OrphanOutboxDurability(t *testing.T) {
	// Disable raw prompt logging for security test
	t.Setenv("CRUSH_CLIPROVIDER_LOG_RAW_PROMPT", "")

	env := testEnv(t)
	model := newFastSSEModel(t, "orphan outbox test")

	// Wrap sessions to fail ONLY EnqueueRunQueueEntry
	failingSessions := &enqueueFailingSessions{Service: env.sessions}

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:   model,
		SmallModel:   model,
		SystemPrompt: "test system prompt",
		Sessions:     failingSessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "p0-3-outbox-durability-test")
	require.NoError(t, err)

	// Create a call with a prompt that contains sensitive data (should NOT appear in logs)
	secretPrompt := "this is a secret prompt with API_KEY=sk-1234567890abcdef"
	call := SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          secretPrompt,
		LogicalCallID:   "p0-3-outbox-logical-id",
		MaxOutputTokens: 100,
	}

	// Attempt to restart orphaned call (enqueue will fail, should write to outbox)
	retryErr := sa.restartOrphanedWithRetry([]SessionAgentCall{call})
	require.Error(t, retryErr, "restartOrphanedWithRetry must return an error when enqueue fails")
	require.ErrorContains(t, retryErr, "failed to durably enqueue call")

	// Verify that a durable record exists in the orphan outbox
	pendingOutbox, err := env.sessions.ListPendingOrphanOutboxEntries(t.Context())
	require.NoError(t, err, "should be able to list pending outbox entries")
	require.Len(t, pendingOutbox, 1, "exactly one outbox entry should exist")

	// Verify the outbox entry contains the correct call data
	outboxEntry := pendingOutbox[0]
	require.Equal(t, sess.ID, outboxEntry.SessionID, "outbox entry should have correct session ID")
	require.Contains(t, outboxEntry.ID, sess.ID, "outbox ID should contain session ID")
	require.Contains(t, outboxEntry.CallData, "p0-3-outbox-logical-id", "call data should contain LogicalCallID")
	require.Contains(t, outboxEntry.CallData, "this is a secret prompt", "call data should contain the prompt")

	// Verify the entry is in pending state (not done or failed)
	require.Equal(t, "pending", outboxEntry.Status, "outbox entry should be in pending state")
	require.Equal(t, int64(0), outboxEntry.Attempts, "outbox entry should have 0 attempts")
	require.Equal(t, int64(5), outboxEntry.MaxAttempts, "outbox entry should have 5 max attempts")
}

// TestSEC1_PromptRedaction verifies that raw prompt is NOT logged in ERROR logs
// when CRUSH_CLIPROVIDER_LOG_RAW_PROMPT is not set.
func TestSEC1_PromptRedaction(t *testing.T) {
	// Ensure raw prompt logging is DISABLED
	t.Setenv("CRUSH_CLIPROVIDER_LOG_RAW_PROMPT", "")
	require.False(t, cliprovider.LogRawPromptEnabled(), "raw prompt logging must be disabled for this test")

	// Capture slog output to verify prompt redaction
	var logBuf bytes.Buffer
	handler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	env := testEnv(t)
	model := newFastSSEModel(t, "prompt redaction test")

	failingSessions := &enqueueFailingSessions{Service: env.sessions}

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:   model,
		SmallModel:   model,
		SystemPrompt: "test system prompt",
		Sessions:     failingSessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "sec1-prompt-redaction-test")
	require.NoError(t, err)

	// Create a call with a unique, sensitive marker that we can check for in logs
	secretMarker := "SECRET_API_KEY_sk_abcdefghijklmnopqrstuvwxyz1234567890"
	secretPrompt := "execute this command with " + secretMarker
	call := SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          secretPrompt,
		LogicalCallID:   "sec1-test-logical-id",
		MaxOutputTokens: 100,
	}

	// Attempt to restart orphaned call (this will log ERROR messages)
	retryErr := sa.restartOrphanedWithRetry([]SessionAgentCall{call})
	require.Error(t, retryErr)

	// SEC-1 verification: raw prompt must NOT appear in logs when diagnostic mode is off
	require.NotContains(t, logBuf.String(), secretMarker,
		"SECURITY LEAK: raw prompt found in log output when diagnostic mode is off")

	// Verify the outbox entry contains the full prompt (prompt is safely stored, not logged)
	pendingOutbox, err := env.sessions.ListPendingOrphanOutboxEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, pendingOutbox, 1, "outbox entry should exist")
	require.Contains(t, pendingOutbox[0].CallData, secretMarker, "outbox call data should contain the full prompt")
}

// TestSEC1_PromptRedaction_WithDiagnosticFlag verifies that raw prompt IS logged
// when CRUSH_CLIPROVIDER_LOG_RAW_PROMPT=1 is set.
func TestSEC1_PromptRedaction_WithDiagnosticFlag(t *testing.T) {
	// Enable raw prompt logging
	t.Setenv("CRUSH_CLIPROVIDER_LOG_RAW_PROMPT", "1")
	require.True(t, cliprovider.LogRawPromptEnabled(), "raw prompt logging must be enabled for this test")
	defer t.Setenv("CRUSH_CLIPROVIDER_LOG_RAW_PROMPT", "")

	// Capture slog output to verify prompt IS logged in diagnostic mode
	var logBuf bytes.Buffer
	handler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	env := testEnv(t)
	model := newFastSSEModel(t, "prompt redaction with flag test")

	failingSessions := &enqueueFailingSessions{Service: env.sessions}

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:   model,
		SmallModel:   model,
		SystemPrompt: "test system prompt",
		Sessions:     failingSessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "sec1-diag-flag-test")
	require.NoError(t, err)

	// Create a call with a unique marker to verify it appears in logs
	secretMarker := "DIAG_MODE_SECRET_MARKER_xyz789"
	secretPrompt := "this is a secret prompt for diagnostic mode with " + secretMarker
	call := SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          secretPrompt,
		LogicalCallID:   "sec1-diag-logical-id",
		MaxOutputTokens: 100,
	}

	// Attempt to restart orphaned call
	retryErr := sa.restartOrphanedWithRetry([]SessionAgentCall{call})
	require.Error(t, retryErr)

	// In diagnostic mode, raw prompt SHOULD appear in logs
	require.Contains(t, logBuf.String(), secretMarker,
		"In diagnostic mode, raw prompt should appear in log output")

	// The outbox entry should still exist
	pendingOutbox, err := env.sessions.ListPendingOrphanOutboxEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, pendingOutbox, 1, "outbox entry should exist")
}

// TestP0_3_OutboxWriteFailure verifies that when BOTH enqueue and outbox write
// fail, we return a clear error (not silently lose the call).
func TestP0_3_OutboxWriteFailure(t *testing.T) {
	t.Setenv("CRUSH_CLIPROVIDER_LOG_RAW_PROMPT", "")

	env := testEnv(t)
	model := newFastSSEModel(t, "outbox write failure test")

	// Wrap sessions to fail BOTH EnqueueRunQueueEntry AND WriteToOrphanOutbox
	failingSessions := &enqueueAndOutboxFailingSessions{Service: env.sessions}

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:   model,
		SmallModel:   model,
		SystemPrompt: "test system prompt",
		Sessions:     failingSessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "p0-3-outbox-failure-test")
	require.NoError(t, err)

	call := SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "test prompt",
		LogicalCallID:   "p0-3-failure-logical-id",
		MaxOutputTokens: 100,
	}

	// Attempt to restart orphaned call
	retryErr := sa.restartOrphanedWithRetry([]SessionAgentCall{call})
	require.Error(t, retryErr)
	require.ErrorContains(t, retryErr, "failed to durably enqueue call and outbox write also failed", "error should mention both failures")

	// Verify NO outbox entry exists (since outbox write also failed)
	pendingOutbox, err := env.sessions.ListPendingOrphanOutboxEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, pendingOutbox, 0, "no outbox entry should exist when outbox write fails")
}

// enqueueAndOutboxFailingSessions wraps a real session.Service and fails BOTH
// EnqueueRunQueueEntry AND WriteToOrphanOutbox.
type enqueueAndOutboxFailingSessions struct {
	session.Service
}

var errSimulatedOutboxFailure = errors.New("p0-3 test: simulated outbox write failure")

func (e *enqueueAndOutboxFailingSessions) EnqueueRunQueueEntry(ctx context.Context, idempotencyKey, sessionID string, callData []byte) error {
	return errSimulatedEnqueueFailure
}

func (e *enqueueAndOutboxFailingSessions) WriteToOrphanOutbox(ctx context.Context, id, sessionID string, callData []byte) error {
	return errSimulatedOutboxFailure
}

// TestP0_3_OutboxClaimAndRecovery and TestP0_3_OutboxMarkFailed (which
// exercised ClaimOrphanOutboxEntry/MarkOrphanOutboxEntryDone/
// MarkOrphanOutboxEntryFailed directly) were removed along with that dead
// code -- see internal/db/sql/orphan_outbox.sql's header comment and task
// #440's follow-up decision. The atomic DrainOrphanOutboxEntry model these
// once predated is covered by internal/session/p0_3_orphan_outbox_drain_test.go.

// Helper test: verify promptHash generates consistent hashes
func TestPromptHash(t *testing.T) {
	prompt := "test prompt with secrets"

	hash1 := promptHash(prompt)
	hash2 := promptHash(prompt)

	require.Equal(t, hash1, hash2, "hash should be consistent for same input")
	require.Len(t, hash1, 16, "hash should be 16 characters (first 16 of hex SHA256)")
	require.NotContains(t, hash1, prompt, "hash should not contain raw prompt")

	// Different prompts should have different hashes
	otherPrompt := "different prompt"
	otherHash := promptHash(otherPrompt)
	require.NotEqual(t, hash1, otherHash, "different prompts should have different hashes")

	// Empty prompt should return empty hash
	emptyHash := promptHash("")
	require.Equal(t, "", emptyHash, "empty prompt should return empty hash")
}
