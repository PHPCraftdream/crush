package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modelFor builds a Model whose every interesting field is derived from name,
// so a snapshot that mixes two sessions' values is impossible to mistake for a
// correct one.
func modelFor(name string) Model {
	return Model{
		CatwalkCfg: catwalk.Model{
			Name:             name + "-catwalk",
			DefaultMaxTokens: 1000,
		},
		ModelCfg: config.SelectedModel{
			Provider:        name + "-provider",
			Model:           name + "-model",
			ReasoningEffort: name + "-effort",
		},
	}
}

// TestResolveTurnConfig_ParallelSessionsGetIndependentSnapshots is the
// regression test for task #265 (P0-1) at the agent layer — scenario 7 of the
// release gate (#277): parallel sessions running different
// model/provider/reasoning/prefix must each get an independent, immutable
// snapshot, and none may observe another's values.
//
// The bug: sessionAgent.largeModel/smallModel/systemPrompt/systemPromptPrefix
// are process-wide csync.Values, and coordinator.applyModelOverrides REWRITES
// them in place for whichever session is starting. runTurn re-read them at
// turn start (and generateTitle re-read smallModel later still), so with two
// sessions live — the entire premise of this fork — session A's override
// became session B's model. Worse, the reads were separate, so one turn could
// straddle an override and run session A's model with session B's system
// prompt.
//
// The test drives resolveTurnConfig from N goroutines, each pinning its own
// values on its own call, while a writer goroutine continuously rewrites all
// four shared fields to a contaminant value. Every snapshot must come back
// exactly as pinned. Run under -race, the writer also proves the reads are
// synchronised, not merely lucky.
func TestResolveTurnConfig_ParallelSessionsGetIndependentSnapshots(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{
		largeModel:         csync.NewValue(modelFor("shared-large")),
		smallModel:         csync.NewValue(modelFor("shared-small")),
		systemPrompt:       csync.NewValue("shared-prompt"),
		systemPromptPrefix: csync.NewValue("shared-prefix"),
	}

	const sessions = 8
	const rounds = 200

	stopWriter := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; ; i++ {
			select {
			case <-stopWriter:
				return
			default:
			}
			// Exactly what applyModelOverrides does to the shared agent.
			a.largeModel.Set(modelFor(fmt.Sprintf("contaminant-large-%d", i)))
			a.smallModel.Set(modelFor(fmt.Sprintf("contaminant-small-%d", i)))
			a.systemPrompt.Set(fmt.Sprintf("contaminant-prompt-%d", i))
			a.systemPromptPrefix.Set(fmt.Sprintf("contaminant-prefix-%d", i))
		}
	}()

	var wg sync.WaitGroup
	for s := range sessions {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()

			name := fmt.Sprintf("session-%d", s)
			large := modelFor(name + "-large")
			small := modelFor(name + "-small")
			prefix := name + "-prefix"
			prompt := name + "-prompt"

			call := SessionAgentCall{
				SessionID:          name,
				LargeModel:         &large,
				SmallModel:         &small,
				SystemPromptPrefix: &prefix,
				SystemPrompt:       &prompt,
			}

			for range rounds {
				cfg := a.resolveTurnConfig(call)

				// The whole point: not "some consistent value", but THIS
				// session's value. A snapshot holding another session's model
				// is the production bug.
				assert.Equal(t, large, cfg.largeModel, "%s must see its own large model, never the shared/contaminated one", name)
				assert.Equal(t, small, cfg.smallModel, "%s must see its own small model", name)
				assert.Equal(t, prefix, cfg.promptPrefix, "%s must see its own prompt prefix", name)
				assert.Equal(t, prompt, cfg.systemPrompt, "%s must see its own system prompt", name)
			}
		}(s)
	}
	wg.Wait()
	close(stopWriter)
	<-writerDone
}

// TestResolveTurnConfig_Precedence pins the resolution rules the snapshot
// replaced, so a later refactor cannot quietly reorder them.
func TestResolveTurnConfig_Precedence(t *testing.T) {
	t.Parallel()

	newAgent := func() *sessionAgent {
		return &sessionAgent{
			largeModel:         csync.NewValue(modelFor("shared-large")),
			smallModel:         csync.NewValue(modelFor("shared-small")),
			systemPrompt:       csync.NewValue("shared-prompt"),
			systemPromptPrefix: csync.NewValue("shared-prefix"),
		}
	}

	t.Run("an unpinned call still reads the shared fields", func(t *testing.T) {
		// Backward compatibility: every caller that predates #265 passes no
		// pins at all and must behave exactly as before.
		cfg := newAgent().resolveTurnConfig(SessionAgentCall{SessionID: "s"})
		assert.Equal(t, modelFor("shared-large"), cfg.largeModel)
		assert.Equal(t, modelFor("shared-small"), cfg.smallModel)
		assert.Equal(t, "shared-prompt", cfg.systemPrompt)
		assert.Equal(t, "shared-prefix", cfg.promptPrefix)
	})

	t.Run("SystemPromptOverride beats a pinned base prompt", func(t *testing.T) {
		// The per-session prompt persisted in the DB is the most specific
		// value there is; the pinned base is only the model-derived default.
		base := "pinned-base"
		cfg := newAgent().resolveTurnConfig(SessionAgentCall{
			SessionID:            "s",
			SystemPrompt:         &base,
			SystemPromptOverride: "per-session",
		})
		assert.Equal(t, "per-session", cfg.systemPrompt)
	})

	t.Run("a pinned base prompt beats the shared one", func(t *testing.T) {
		base := "pinned-base"
		cfg := newAgent().resolveTurnConfig(SessionAgentCall{SessionID: "s", SystemPrompt: &base})
		assert.Equal(t, "pinned-base", cfg.systemPrompt)
	})

	t.Run("pins are independent of each other", func(t *testing.T) {
		// Pinning only the large model must not drag the others off the
		// shared values — that would be a different flavour of the same
		// mixing bug.
		large := modelFor("only-large")
		cfg := newAgent().resolveTurnConfig(SessionAgentCall{SessionID: "s", LargeModel: &large})
		assert.Equal(t, large, cfg.largeModel)
		assert.Equal(t, modelFor("shared-small"), cfg.smallModel)
		assert.Equal(t, "shared-prompt", cfg.systemPrompt)
		assert.Equal(t, "shared-prefix", cfg.promptPrefix)
	})
}

// TestRunInternal_PinsResolvedOverridesOntoTheCall is the coordinator half of
// #265. resolveTurnConfig can only honour a pin that someone actually set, and
// the caller that knows the right values is the coordinator: it resolves the
// session's overrides, then hands control to the agent.
//
// Before the fix, applyModelOverrides wrote the resolved values into the
// SHARED agent and returned nothing, so runInternal read them back out of that
// same shared agent (c.currentAgent.Model()) and the turn read them a third
// time. Each gap is a window for a concurrent session's override to land.
//
// This drives runInternal with a pinned snapshot that DIFFERS from the shared
// agent's model in every field, standing in for "another session overwrote the
// shared state between applyModelOverrides and here", and asserts the call the
// agent receives is built entirely from the pinned values.
func TestRunInternal_PinsResolvedOverridesOntoTheCall(t *testing.T) {
	const sharedProvider = "shared-provider"
	const pinnedProvider = "pinned-provider"

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(sharedProvider, config.ProviderConfig{ID: sharedProvider})
	cfg.Config().Providers.Set(pinnedProvider, config.ProviderConfig{ID: pinnedProvider})

	coord := &coordinator{cfg: cfg, sessions: env.sessions, messages: env.messages}

	var gotCall SessionAgentCall
	// The shared agent is on a completely different model, with a different
	// token budget — if anything downstream re-reads it, the assertions below
	// name exactly which field leaked.
	agent := newMockAgent(sharedProvider, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		gotCall = call
		return agentResultWithText("ok"), nil
	})
	coord.currentAgent = agent

	sess, err := env.sessions.Create(t.Context(), "pinned-overrides")
	require.NoError(t, err)

	pinnedLarge := modelFor("pinned")
	pinnedLarge.ModelCfg.Provider = pinnedProvider
	pinnedLarge.CatwalkCfg.DefaultMaxTokens = 777
	pinnedSmall := modelFor("pinned-small")

	pinned := &resolvedOverrides{
		large:        pinnedLarge,
		small:        pinnedSmall,
		promptPrefix: "pinned-prefix",
		systemPrompt: "pinned-system-prompt",
	}

	_, err = coord.runInternal(t.Context(), sess.ID, "prompt", pinned)
	require.NoError(t, err)

	require.NotNil(t, gotCall.LargeModel, "the call must carry the model rather than leaving the turn to re-read shared state")
	assert.Equal(t, pinnedLarge, *gotCall.LargeModel, "the turn must run the model the overrides resolved, not the shared agent's")
	require.NotNil(t, gotCall.SmallModel)
	assert.Equal(t, pinnedSmall, *gotCall.SmallModel, "the small model drives title generation and must be pinned too")
	require.NotNil(t, gotCall.SystemPromptPrefix)
	assert.Equal(t, "pinned-prefix", *gotCall.SystemPromptPrefix)
	require.NotNil(t, gotCall.SystemPrompt)
	assert.Equal(t, "pinned-system-prompt", *gotCall.SystemPrompt)

	// Not just the model field: everything runInternal DERIVES from the model
	// must come from the pinned one as well. A call carrying the pinned model
	// alongside the shared model's token budget is the "internally
	// inconsistent turn" half of this bug.
	assert.Equal(t, int64(777), gotCall.MaxOutputTokens,
		"max output tokens must be derived from the pinned model (777), not the shared agent's (4096)")
}

// TestBuildCall_PinsModelEvenWithoutOverrides covers the path that carries the
// longest gap between "options resolved" and "turn runs": buildCall's result
// is QUEUED as a replacement, so it may sit for an unbounded time while other
// sessions rewrite the shared agent. Even with no overrides to apply, the
// model read here must travel with the call.
func TestBuildCall_PinsModelEvenWithoutOverrides(t *testing.T) {
	const providerID = "test-provider"

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{ID: providerID})

	coord := &coordinator{cfg: cfg, sessions: env.sessions, messages: env.messages}
	coord.currentAgent = newMockAgent(providerID, 4096, nil)

	sess, err := env.sessions.Create(t.Context(), "buildcall-pin")
	require.NoError(t, err)

	call, err := coord.buildCall(t.Context(), sess.ID, "prompt", nil, nil)
	require.NoError(t, err)

	require.NotNil(t, call.LargeModel,
		"a queued call must pin the model it computed its options from — by the time it runs, the shared agent may be on another session's model")
	assert.Equal(t, providerID, call.LargeModel.ModelCfg.Provider)
	assert.Equal(t, int64(4096), call.MaxOutputTokens)
}
