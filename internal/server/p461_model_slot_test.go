package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// Task #461 — regression tests for two related "session model selection
// clobbers state it wasn't asked to touch" bugs found while fixing #460.
//
// Bug A (fixed in internal/server/handlers.go's handleSetSessionModels +
// internal/session/session.go's UpdateModels signature): the web UI's
// ModelSelector always sent BOTH slots on every switch — filling the
// untouched one from the current session/default value — so changing only
// the large model re-wrote the small model's override too, freezing it
// against later folder/system changes. UpdateModels used to take four plain
// strings with no way to say "leave this slot alone"; it now takes
// *session.ModelSlotUpdate per slot, nil meaning "don't touch".
//
// Bug B (fixed in handleCreateSession/handleInitializeProject): both handlers
// used to seed a brand-new session's large/small columns from the resolved
// config default AT CREATION TIME. That permanently pinned every session to
// whatever was effective the moment it was created — an untouched session
// would never again follow a later folder/system default change, defeating
// the system -> folder -> session cascade for the overwhelming majority of
// sessions (ones nobody ever manually re-pointed).
//
// REVERT CHECK for bug A: restore handleSetSessionModels to build lp/lm/sp/sm
// from `if p.LargeModel != nil {...}` and call
// a.Sessions.UpdateModels(ctx, p.SessionID, lp, lm, sp, sm) with plain
// strings (session.UpdateModels's old signature) — this test's assertion
// that the small slot survived a large-only update starts failing because
// sp/sm default to "" and clear it.
//
// REVERT CHECK for bug B: restore the "Set default models from config"
// block in handleCreateSession — TestCreateSession_DoesNotFreezeModelAtBirth
// fails because the fresh session's LargeModelID is no longer empty.

func TestSetSessionModels_LargeOnlyUpdateLeavesSmallUntouched(t *testing.T) {
	ctx := t.Context()
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())

	sess, err := a.Sessions.Create(ctx, "p461-partial-update")
	require.NoError(t, err)

	// Establish an initial explicit small-model override, the way a real
	// prior selector click would.
	initial, err := json.Marshal(SetSessionModelsPayload{
		SessionID:  sess.ID,
		SmallModel: &ModelOverrideWire{Provider: "small-provider", Model: "small-model-v1"},
	})
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, newTestClient(), WSMessage{ID: "corr-1", Type: CmdSetSessionModels, Payload: initial})

	afterFirst, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "small-provider", afterFirst.SmallModelProvider)
	require.Equal(t, "small-model-v1", afterFirst.SmallModelID)
	require.Empty(t, afterFirst.LargeModelID, "large must still be unset — nothing has touched it yet")

	// Now switch ONLY the large model, exactly as ModelSelector.onSelect does
	// after the #461 frontend fix — the small slot is omitted from the
	// payload entirely (JSON null), not re-sent with its current value.
	largeOnly, err := json.Marshal(SetSessionModelsPayload{
		SessionID:  sess.ID,
		LargeModel: &ModelOverrideWire{Provider: "large-provider", Model: "large-model-v1"},
	})
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, newTestClient(), WSMessage{ID: "corr-2", Type: CmdSetSessionModels, Payload: largeOnly})

	afterSecond, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "large-provider", afterSecond.LargeModelProvider)
	require.Equal(t, "large-model-v1", afterSecond.LargeModelID)
	// The small override from the FIRST call must survive untouched.
	require.Equal(t, "small-provider", afterSecond.SmallModelProvider,
		"switching only the large model must not touch the small model's override")
	require.Equal(t, "small-model-v1", afterSecond.SmallModelID,
		"switching only the large model must not touch the small model's override")
}

func TestCreateSession_DoesNotFreezeModelAtBirth(t *testing.T) {
	ctx := t.Context()
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())

	payload, err := json.Marshal(CreateSessionPayload{Title: "p461-no-freeze"})
	require.NoError(t, err)

	client := newTestClient()
	handleCreateSession(ctx, a, client, WSMessage{ID: "corr-3", Type: CmdCreateSession, Payload: payload})

	sessions, err := a.Sessions.List(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, sessions)

	var found bool
	for _, s := range sessions {
		if s.Title == "p461-no-freeze" {
			found = true
			require.Empty(t, s.LargeModelID,
				"a freshly created session must start with NO large-model override, so it keeps inheriting the folder/system default")
			require.Empty(t, s.SmallModelID,
				"a freshly created session must start with NO small-model override, so it keeps inheriting the folder/system default")
		}
	}
	require.True(t, found, "the session created via handleCreateSession was not found")
}
