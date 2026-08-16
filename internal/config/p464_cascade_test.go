package config

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

// Task #464 — locks the system -> folder half of the model cascade
// (system -> folder -> session; the session half is covered in
// internal/agent's p464_cascade_test.go, since session resolution lives in
// resolveSessionModels there).
//
// Both scopes' writers are exercised through the real ConfigStore API
// (UpdatePreferredModel), not hand-crafted JSON files, so this test also
// guards the write path, not just the merge/read path.

func cascadeTestProvider(id, model string) catwalk.Provider {
	return catwalk.Provider{
		ID:   catwalk.InferenceProvider(id),
		Name: id,
		Type: catwalk.TypeOpenAI,
		Models: []catwalk.Model{
			{ID: model, Name: model, DefaultMaxTokens: 4096},
		},
	}
}

// seedCascadeStore builds a real *ConfigStore isolated from the host's
// global config, with a resolvable workspace scope (both prerequisites for
// ScopeWorkspace writes to succeed rather than returning ErrNoWorkspaceConfig).
func seedCascadeStore(t *testing.T) *ConfigStore {
	t.Helper()
	isolateAllGlobalConfigPaths(t)
	workingDir := t.TempDir()
	store, err := Init(workingDir, "", false)
	require.NoError(t, err)
	require.True(t, store.HasWorkspaceConfig(), "test store must resolve a workspace scope")
	return store
}

func TestCascade_GlobalOnly_ResolvesToGlobal(t *testing.T) {
	store := seedCascadeStore(t)

	globalProvider := cascadeTestProvider("cascade-global", "cascade-global-model")
	store.Config().Providers.Set(string(globalProvider.ID), asProviderConfig(globalProvider))
	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, SelectedModelTypeLarge,
		SelectedModel{Provider: "cascade-global", Model: "cascade-global-model"}))

	effective, ok := store.Config().Models[SelectedModelTypeLarge]
	require.True(t, ok)
	require.Equal(t, "cascade-global", effective.Provider)
	require.Equal(t, "cascade-global-model", effective.Model)
}

func TestCascade_WorkspaceWinsOverGlobal(t *testing.T) {
	store := seedCascadeStore(t)

	globalProvider := cascadeTestProvider("cascade-global2", "cascade-global-model2")
	workspaceProvider := cascadeTestProvider("cascade-workspace", "cascade-workspace-model")
	store.Config().Providers.Set(string(globalProvider.ID), asProviderConfig(globalProvider))
	store.Config().Providers.Set(string(workspaceProvider.ID), asProviderConfig(workspaceProvider))

	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, SelectedModelTypeLarge,
		SelectedModel{Provider: "cascade-global2", Model: "cascade-global-model2"}))
	require.NoError(t, store.UpdatePreferredModel(ScopeWorkspace, SelectedModelTypeLarge,
		SelectedModel{Provider: "cascade-workspace", Model: "cascade-workspace-model"}))

	effective, ok := store.Config().Models[SelectedModelTypeLarge]
	require.True(t, ok)
	require.Equal(t, "cascade-workspace", effective.Provider, "folder scope must win over system scope")
	require.Equal(t, "cascade-workspace-model", effective.Model)

	// Both scopes' explicit values must still be independently readable —
	// the modal (task #463) needs to show "folder overrides system", not
	// just the merged winner.
	globalAll, err := store.ReadAllModelsAtScope(ScopeGlobal)
	require.NoError(t, err)
	require.NotNil(t, globalAll[SelectedModelTypeLarge])
	require.Equal(t, "cascade-global2", globalAll[SelectedModelTypeLarge].Provider)

	workspaceAll, err := store.ReadAllModelsAtScope(ScopeWorkspace)
	require.NoError(t, err)
	require.NotNil(t, workspaceAll[SelectedModelTypeLarge])
	require.Equal(t, "cascade-workspace", workspaceAll[SelectedModelTypeLarge].Provider)
}

func TestCascade_ClearingWorkspaceFallsBackToGlobal(t *testing.T) {
	store := seedCascadeStore(t)

	globalProvider := cascadeTestProvider("cascade-global3", "cascade-global-model3")
	workspaceProvider := cascadeTestProvider("cascade-workspace3", "cascade-workspace-model3")
	store.Config().Providers.Set(string(globalProvider.ID), asProviderConfig(globalProvider))
	store.Config().Providers.Set(string(workspaceProvider.ID), asProviderConfig(workspaceProvider))

	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, SelectedModelTypeSmall,
		SelectedModel{Provider: "cascade-global3", Model: "cascade-global-model3"}))
	require.NoError(t, store.UpdatePreferredModel(ScopeWorkspace, SelectedModelTypeSmall,
		SelectedModel{Provider: "cascade-workspace3", Model: "cascade-workspace-model3"}))

	before, ok := store.Config().Models[SelectedModelTypeSmall]
	require.True(t, ok)
	require.Equal(t, "cascade-workspace3", before.Provider)

	// Clear the workspace override — mirrors `crush models unset small
	// --local` / the modal's per-slot clear (task #463).
	require.NoError(t, store.RemoveConfigField(ScopeWorkspace, "models.small"))

	after, ok := store.Config().Models[SelectedModelTypeSmall]
	require.True(t, ok)
	require.Equal(t, "cascade-global3", after.Provider, "clearing the folder override must fall back to the system default")
	require.Equal(t, "cascade-global-model3", after.Model)

	workspaceAll, err := store.ReadAllModelsAtScope(ScopeWorkspace)
	require.NoError(t, err)
	require.Nil(t, workspaceAll[SelectedModelTypeSmall], "the workspace override must be gone, not merely shadowed")
}

// asProviderConfig converts a catwalk.Provider into the ProviderConfig shape
// UpdatePreferredModel's GetModel lookup needs to resolve the model
// (self-heal in configureSelectedModels replaces an unresolvable
// provider/model pair with the default — see internal/server's
// p462_scoped_models_test.go for the same self-heal trap hit and fixed
// there).
func asProviderConfig(p catwalk.Provider) ProviderConfig {
	return ProviderConfig{
		ID:     string(p.ID),
		Name:   p.Name,
		Type:   p.Type,
		Models: p.Models,
	}
}
