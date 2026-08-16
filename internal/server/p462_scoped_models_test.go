package server

import (
	"encoding/json"
	"testing"

	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// Task #462 — the scoped-models WS API (get_scoped_models / set_scoped_model
// / clear_scoped_model) that the System/Folder/Session modal (task #463)
// reads and writes. Distinct from set_session_models: these commands only
// ever touch config.ScopeGlobal ("system") and config.ScopeWorkspace
// ("folder"), never the session DB.
//
// Every test seeds a REAL custom provider (via handleAddCustomProvider)
// before writing a scoped model that references it. config's own self-heal
// (internal/config/load.go's configureSelectedModels, invoked on reload)
// silently replaces any models.<slot> selection that names a provider/model
// pair config.GetModel can't resolve — an early version of this file used a
// bare fictitious "acme"/"some-model" pair and every assertion about WHICH
// value survived was actually observing the self-heal fallback, not the
// scoped-write logic under test.
func addTestProvider(t *testing.T, a *appPkg.App) {
	t.Helper()
	payload, err := json.Marshal(AddCustomProviderPayload{
		ID:      "acme",
		BaseURL: "https://acme.example.invalid/v1",
		Type:    "openai-compat",
		Models: []CustomModelPayload{
			{ID: "acme-large-v1", Name: "Acme Large v1", ContextWindow: 128000},
			{ID: "small-global", Name: "Small Global", ContextWindow: 32000},
			{ID: "small-workspace", Name: "Small Workspace", ContextWindow: 32000},
			{ID: "large-global", Name: "Large Global", ContextWindow: 128000},
			{ID: "large-workspace", Name: "Large Workspace", ContextWindow: 128000},
		},
	})
	require.NoError(t, err)
	client := newTestClient()
	handleAddCustomProvider(a, client, WSMessage{ID: "seed", Type: CmdAddCustomProvider, Payload: payload})
}

func TestSetScopedModel_WritesGlobalAndIsReflectedInGet(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	store := a.Store()
	addTestProvider(t, a)

	setPayload, err := json.Marshal(SetScopedModelPayload{
		Scope:    "global",
		Slot:     "large",
		Provider: "acme",
		Model:    "acme-large-v1",
	})
	require.NoError(t, err)
	handleSetScopedModel(a, newTestClient(), WSMessage{ID: "c1", Type: CmdSetScopedModel, Payload: setPayload})

	// On-disk, at the scope actually written.
	onDisk, err := store.ReadAllModelsAtScope(config.ScopeGlobal)
	require.NoError(t, err)
	require.NotNil(t, onDisk[config.SelectedModelTypeLarge])
	require.Equal(t, "acme", onDisk[config.SelectedModelTypeLarge].Provider)
	require.Equal(t, "acme-large-v1", onDisk[config.SelectedModelTypeLarge].Model)

	// The read API reports it as both the global value AND the effective
	// value (nothing at workspace to shadow it).
	wire, err := buildScopedModelsWire(a)
	require.NoError(t, err)
	require.NotNil(t, wire.Large.Global)
	require.Equal(t, "acme", wire.Large.Global.Provider)
	require.Equal(t, "acme-large-v1", wire.Large.Global.Model)
	require.Nil(t, wire.Large.Workspace)
	require.NotNil(t, wire.Large.Effective)
	require.Equal(t, "acme-large-v1", wire.Large.Effective.Model)
	require.Equal(t, "global", wire.Large.EffectiveScope)
}

func TestSetScopedModel_WorkspaceShadowsGlobal(t *testing.T) {
	dataDir := t.TempDir()
	workingDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	store := a.Store()
	require.True(t, store.HasWorkspaceConfig(), "test app must resolve a workspace config path")
	addTestProvider(t, a)

	globalPayload, err := json.Marshal(SetScopedModelPayload{
		Scope: "global", Slot: "small", Provider: "acme", Model: "small-global",
	})
	require.NoError(t, err)
	handleSetScopedModel(a, newTestClient(), WSMessage{ID: "c1", Type: CmdSetScopedModel, Payload: globalPayload})

	workspacePayload, err := json.Marshal(SetScopedModelPayload{
		Scope: "workspace", Slot: "small", Provider: "acme", Model: "small-workspace",
	})
	require.NoError(t, err)
	handleSetScopedModel(a, newTestClient(), WSMessage{ID: "c2", Type: CmdSetScopedModel, Payload: workspacePayload})

	wire, err := buildScopedModelsWire(a)
	require.NoError(t, err)
	require.NotNil(t, wire.Small.Global)
	require.Equal(t, "small-global", wire.Small.Global.Model)
	require.NotNil(t, wire.Small.Workspace)
	require.Equal(t, "small-workspace", wire.Small.Workspace.Model)
	require.NotNil(t, wire.Small.Effective)
	require.Equal(t, "small-workspace", wire.Small.Effective.Model, "workspace must win over global")
	require.Equal(t, "workspace", wire.Small.EffectiveScope)
}

func TestClearScopedModel_FallsBackToOtherScope(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	addTestProvider(t, a)

	globalPayload, err := json.Marshal(SetScopedModelPayload{
		Scope: "global", Slot: "large", Provider: "acme", Model: "large-global",
	})
	require.NoError(t, err)
	handleSetScopedModel(a, newTestClient(), WSMessage{ID: "c1", Type: CmdSetScopedModel, Payload: globalPayload})

	workspacePayload, err := json.Marshal(SetScopedModelPayload{
		Scope: "workspace", Slot: "large", Provider: "acme", Model: "large-workspace",
	})
	require.NoError(t, err)
	handleSetScopedModel(a, newTestClient(), WSMessage{ID: "c2", Type: CmdSetScopedModel, Payload: workspacePayload})

	before, err := buildScopedModelsWire(a)
	require.NoError(t, err)
	require.Equal(t, "large-workspace", before.Large.Effective.Model)

	clearPayload, err := json.Marshal(ClearScopedModelPayload{Scope: "workspace", Slot: "large"})
	require.NoError(t, err)
	handleClearScopedModel(a, newTestClient(), WSMessage{ID: "c3", Type: CmdClearScopedModel, Payload: clearPayload})

	after, err := buildScopedModelsWire(a)
	require.NoError(t, err)
	require.Nil(t, after.Large.Workspace, "workspace override must be gone")
	require.NotNil(t, after.Large.Global, "global value must be untouched")
	require.Equal(t, "large-global", after.Large.Global.Model)
	require.NotNil(t, after.Large.Effective)
	require.Equal(t, "large-global", after.Large.Effective.Model, "must fall back to global once workspace is cleared")
	require.Equal(t, "global", after.Large.EffectiveScope)
}

func TestClearScopedModel_MissingKeyIsNoOp(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())

	clearPayload, err := json.Marshal(ClearScopedModelPayload{Scope: "workspace", Slot: "reviewer"})
	require.NoError(t, err)

	client := newTestClient()
	handleClearScopedModel(a, client, WSMessage{ID: "c1", Type: CmdClearScopedModel, Payload: clearPayload})

	select {
	case raw := <-client.send:
		var resp WSMessage
		require.NoError(t, json.Unmarshal(raw, &resp))
		require.Empty(t, resp.Error, "clearing an already-unset slot must succeed, not error")
	default:
		t.Fatal("expected a reply on the client's send channel")
	}
}

func TestSetScopedModel_RejectsUnknownScope(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())

	payload, err := json.Marshal(SetScopedModelPayload{
		Scope: "not-a-real-scope", Slot: "large", Provider: "acme", Model: "m",
	})
	require.NoError(t, err)

	client := newTestClient()
	handleSetScopedModel(a, client, WSMessage{ID: "c1", Type: CmdSetScopedModel, Payload: payload})

	raw := <-client.send
	var resp WSMessage
	require.NoError(t, json.Unmarshal(raw, &resp))
	require.Equal(t, EventError, resp.Type)
	require.NotEmpty(t, resp.Error)
}
