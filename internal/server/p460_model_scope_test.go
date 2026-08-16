package server

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// Task #460 — regression tests for the "switching a model inside one session
// changes the model system-wide" bug.
//
// Model defaults cascade system -> folder -> session. The system level lives
// in the global crush.json (config.ScopeGlobal), the folder level in the
// workspace crush.json (config.ScopeWorkspace), and the session level in the
// sessions DB row. Only three writers may touch the system/folder levels:
// `crush models use`, the explicit scoped WS commands, and config's own
// first-run bootstrap/self-heal in load.go. Nothing driven by ordinary chat
// activity may.
//
// The bug: handleTrackModelUsage called
// store.UpdatePreferredModel(config.ScopeGlobal, ...), which writes
// models.<type> into the global crush.json. Two paths reached it —
// ModelSelector.onSelect (an explicit per-session pick) and, far worse,
// web/src/useWS.ts's message_created subscriber, which fires on EVERY
// assistant message. So the system-wide default silently drifted to whichever
// model the most recently active session happened to run.
//
// REVERT CHECK: swap RecordRecentModel back to UpdatePreferredModel in
// handleTrackModelUsage and TestTrackModelUsage_DoesNotChangeGlobalDefault
// fails on its "global default unchanged" assertion.

// globalLargeState captures the system-level large-model default from both
// places it can live: the published in-memory snapshot every reader sees, and
// the global crush.json on disk that survives a restart. A leak into either
// one is a bug, so both are compared.
type globalLargeState struct {
	live   config.SelectedModel
	onDisk *config.SelectedModel
}

func captureGlobalLarge(t *testing.T, store *config.ConfigStore) globalLargeState {
	t.Helper()
	onDisk, err := store.ReadAllModelsAtScope(config.ScopeGlobal)
	require.NoError(t, err)
	return globalLargeState{
		live:   store.Config().Models[config.SelectedModelTypeLarge],
		onDisk: onDisk[config.SelectedModelTypeLarge],
	}
}

// requireGlobalLargeUnchanged asserts the system-level default did not move.
//
// Deliberately a before/after delta rather than a comparison against a seeded
// literal: config.Load self-heals a selected model that names a provider the
// machine cannot actually serve (internal/config/load.go:919-934 rewrites
// models.large via updatePreferredModelLocked), so seeding a synthetic
// "seed-provider" and expecting it to survive asserts the wrong thing and
// fails for a reason unrelated to this bug. Comparing the state before and
// after the handler runs tests exactly the property we care about — the
// handler didn't touch it — whatever the machine's real default happens to be.
func requireGlobalLargeUnchanged(t *testing.T, store *config.ConfigStore, before globalLargeState) {
	t.Helper()
	after := captureGlobalLarge(t, store)

	require.Equal(t, before.live, after.live,
		"the effective system-wide large model changed — a session-scoped action leaked into global config")

	switch before.onDisk {
	case nil:
		require.Nil(t, after.onDisk,
			"models.large was written into the global crush.json by an action that must not touch it")
	default:
		require.NotNil(t, after.onDisk, "models.large vanished from the global crush.json")
		require.Equal(t, *before.onDisk, *after.onDisk,
			"the global crush.json's models.large was rewritten")
	}
}

// TestTrackModelUsage_DoesNotChangeGlobalDefault pins the direct fix: the
// track_model_usage command records recency and nothing else.
func TestTrackModelUsage_DoesNotChangeGlobalDefault(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	store := a.Store()
	before := captureGlobalLarge(t, store)

	tracked := config.SelectedModel{Provider: "other-provider", Model: "other-large-model"}
	// Non-vacuous: if the model we track were already the default, the
	// assertion below would hold even with the bug present.
	require.NotEqual(t, before.live.Provider+"/"+before.live.Model, tracked.Provider+"/"+tracked.Model)

	payload, err := json.Marshal(TrackModelUsagePayload{
		ModelType: string(config.SelectedModelTypeLarge),
		Provider:  tracked.Provider,
		Model:     tracked.Model,
	})
	require.NoError(t, err)

	handleTrackModelUsage(a, newTestClient(), WSMessage{ID: "corr-1", Type: CmdTrackModelUsage, Payload: payload})

	requireGlobalLargeUnchanged(t, store, before)

	// ...but the recency list, which is what this command is actually for,
	// must have picked the model up. Without this the "fix" could trivially
	// be a no-op handler and still pass the assertion above.
	require.Contains(t, store.Config().RecentModels[config.SelectedModelTypeLarge], tracked,
		"track_model_usage no longer records recency")
}

// TestSetSessionModels_DoesNotChangeGlobalDefault covers the user-visible
// path end to end: picking a model for one session must move that session's
// own row and leave the system level alone.
func TestSetSessionModels_DoesNotChangeGlobalDefault(t *testing.T) {
	ctx := t.Context()
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	store := a.Store()
	before := captureGlobalLarge(t, store)

	sess, err := a.Sessions.Create(ctx, "p460-session-scope")
	require.NoError(t, err)
	require.Empty(t, sess.LargeModelID, "a fresh session must start with no override, so it inherits")

	payload, err := json.Marshal(SetSessionModelsPayload{
		SessionID:  sess.ID,
		LargeModel: &ModelOverrideWire{Provider: "session-provider", Model: "session-large-model"},
	})
	require.NoError(t, err)

	handleSetSessionModels(ctx, a, newTestClient(), WSMessage{ID: "corr-2", Type: CmdSetSessionModels, Payload: payload})

	// The session override landed...
	updated, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "session-provider", updated.LargeModelProvider)
	require.Equal(t, "session-large-model", updated.LargeModelID)

	// ...and the system level did not move.
	requireGlobalLargeUnchanged(t, store, before)
}

// newTestClient builds a Client detached from any real connection. Both
// c.reply and hub.Broadcast are non-blocking (select/default), so no hub
// event loop needs to be running for a handler to complete.
func newTestClient() *Client {
	c := newClient(newHub(), nil)
	c.send = make(chan []byte, 8)
	return c
}
