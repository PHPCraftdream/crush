package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// Task #466 — set_session_models's worker/reviewer plumbing. Mirrors
// p461_model_slot_test.go's partial-update coverage for large/small: nil
// means "don't touch", and setting one of worker/reviewer must not touch
// the large/small slots (and vice versa).

func TestSetSessionModels_SetsWorkerSlot(t *testing.T) {
	ctx := t.Context()
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())

	sess, err := a.Sessions.Create(ctx, "p466-worker-slot")
	require.NoError(t, err)

	payload, err := json.Marshal(SetSessionModelsPayload{
		SessionID:   sess.ID,
		WorkerModel: &ModelOverrideWire{Provider: "worker-provider", Model: "worker-model"},
	})
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, newTestClient(), WSMessage{ID: "c1", Type: CmdSetSessionModels, Payload: payload})

	updated, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "worker-provider", updated.WorkerModelProvider)
	require.Equal(t, "worker-model", updated.WorkerModelID)
	// Untouched slots must stay unset.
	require.Empty(t, updated.LargeModelID)
	require.Empty(t, updated.SmallModelID)
	require.Empty(t, updated.ReviewerModelID)
}

func TestSetSessionModels_WorkerUpdateDoesNotTouchLargeSmall(t *testing.T) {
	ctx := t.Context()
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())

	sess, err := a.Sessions.Create(ctx, "p466-worker-vs-large")
	require.NoError(t, err)

	largePayload, err := json.Marshal(SetSessionModelsPayload{
		SessionID:  sess.ID,
		LargeModel: &ModelOverrideWire{Provider: "large-provider", Model: "large-model"},
	})
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, newTestClient(), WSMessage{ID: "c1", Type: CmdSetSessionModels, Payload: largePayload})

	workerPayload, err := json.Marshal(SetSessionModelsPayload{
		SessionID:   sess.ID,
		WorkerModel: &ModelOverrideWire{Provider: "worker-provider", Model: "worker-model"},
	})
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, newTestClient(), WSMessage{ID: "c2", Type: CmdSetSessionModels, Payload: workerPayload})

	updated, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "large-provider", updated.LargeModelProvider, "setting worker must not clobber the earlier large override")
	require.Equal(t, "large-model", updated.LargeModelID)
	require.Equal(t, "worker-provider", updated.WorkerModelProvider)
	require.Equal(t, "worker-model", updated.WorkerModelID)
}

func TestSetSessionModels_SetsReviewerSlotIndependently(t *testing.T) {
	ctx := t.Context()
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())

	sess, err := a.Sessions.Create(ctx, "p466-reviewer-slot")
	require.NoError(t, err)

	workerPayload, err := json.Marshal(SetSessionModelsPayload{
		SessionID:   sess.ID,
		WorkerModel: &ModelOverrideWire{Provider: "worker-provider", Model: "worker-model"},
	})
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, newTestClient(), WSMessage{ID: "c1", Type: CmdSetSessionModels, Payload: workerPayload})

	reviewerPayload, err := json.Marshal(SetSessionModelsPayload{
		SessionID:     sess.ID,
		ReviewerModel: &ModelOverrideWire{Provider: "reviewer-provider", Model: "reviewer-model"},
	})
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, newTestClient(), WSMessage{ID: "c2", Type: CmdSetSessionModels, Payload: reviewerPayload})

	updated, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "worker-provider", updated.WorkerModelProvider, "setting reviewer must not clobber the earlier worker override")
	require.Equal(t, "reviewer-provider", updated.ReviewerModelProvider)
	require.Equal(t, "reviewer-model", updated.ReviewerModelID)
}
