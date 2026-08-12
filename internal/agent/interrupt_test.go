package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueueMessage_AppendsToSessionQueue verifies that QueueMessage on
// sessionAgent stores the call without starting a Run and that QueuedPrompts
// counts it correctly. This is the primitive used by InterruptAndSend.
func TestQueueMessage_AppendsToSessionQueue(t *testing.T) {
	env := testEnv(t)
	a := testSessionAgent(env, nil, nil, "sys").(*sessionAgent)

	const sessionID = "session-test"

	require.Equal(t, 0, a.QueuedPrompts(sessionID))

	a.QueueMessage(SessionAgentCall{SessionID: sessionID, Prompt: "first"})
	a.QueueMessage(SessionAgentCall{SessionID: sessionID, Prompt: "second"})

	require.Equal(t, 2, a.QueuedPrompts(sessionID))
	assert.Equal(t, []string{"first", "second"}, a.QueuedPromptsList(sessionID))

	// Different session must have its own queue.
	a.QueueMessage(SessionAgentCall{SessionID: "other", Prompt: "x"})
	assert.Equal(t, 2, a.QueuedPrompts(sessionID))
	assert.Equal(t, 1, a.QueuedPrompts("other"))
}

// TestCoordinator_InterruptAndSend_UsesInterruptAndReplace verifies the
// public coordinator method routes through InterruptAndReplace (design §4),
// NOT the QueueMessage+Cancel two-step that P0-2 made self-defeating. The
// replacement-reaches-next-turn behavior is covered by the real-agent
// integration test TestInterruptAndReplace_ReplacementReachesNextTurn_P0_2.
func TestCoordinator_InterruptAndSend_UsesInterruptAndReplace(t *testing.T) {
	const providerID = "anthropic"
	providerCfg := config.ProviderConfig{ID: providerID}

	env := testEnv(t)
	coord := newTestCoordinator(t, env, providerID, providerCfg)

	// Pre-create the session so resolveSessionSystemPrompt finds it.
	sess, err := env.sessions.Create(t.Context(), "test session")
	require.NoError(t, err)

	mock := &mockSessionAgent{
		model: Model{
			CatwalkCfg: catwalk.Model{DefaultMaxTokens: 4096, ContextWindow: 200000},
			ModelCfg:   config.SelectedModel{Provider: providerID, Model: "claude-test"},
		},
	}
	coord.currentAgent = mock

	att := message.Attachment{FileName: "a.txt", MimeType: "text/plain", Content: []byte("hi")}
	err = coord.InterruptAndSend(t.Context(), sess.ID, "stop, do X instead", nil, nil, att)
	require.NoError(t, err)

	// InterruptAndSend now routes through InterruptAndReplace (design §4),
	// NOT the QueueMessage+Cancel two-step that P0-2 made self-defeating.
	// One call recorded as an interrupt-and-replace, with the user's prompt
	// and attachment carried through.
	require.Len(t, mock.interruptAndReplaced, 1)
	assert.Equal(t, sess.ID, mock.interruptAndReplaced[0].SessionID)
	assert.Equal(t, "stop, do X instead", mock.interruptAndReplaced[0].Prompt)
	require.Len(t, mock.interruptAndReplaced[0].Attachments, 1)
	assert.Equal(t, "a.txt", mock.interruptAndReplaced[0].Attachments[0].FileName)

	// The old QueueMessage+Cancel path must no longer fire.
	assert.Empty(t, mock.queuedCalls, "InterruptAndSend must not use QueueMessage anymore")
	assert.Empty(t, mock.cancelled, "InterruptAndSend must not call Cancel anymore")
}

// TestCoordinator_InterruptAndSend_UnknownProvider_Errors verifies that we
// don't queue / cancel when the model setup fails: that would leave a stuck
// queued message that nothing will ever start.
func TestCoordinator_InterruptAndSend_UnknownProvider_Errors(t *testing.T) {
	env := testEnv(t)
	// Note: not registering the provider config below makes buildCall fail.
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}

	sess, err := env.sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	// Set up the session with an unknown provider override.
	// This will cause resolveSessionModels to fail when building models.
	err = env.sessions.UpdateModels(t.Context(), sess.ID, "ghost-provider", "ghost-model", "", "")
	require.NoError(t, err)

	mock := &mockSessionAgent{
		model: Model{
			ModelCfg: config.SelectedModel{Provider: "ghost-provider"},
		},
	}
	coord.currentAgent = mock

	err = coord.InterruptAndSend(t.Context(), sess.ID, "hello", nil, nil)
	require.Error(t, err, "InterruptAndSend must error when model resolution fails")
	assert.Empty(t, mock.queuedCalls, "queue must not be touched when build fails")
	assert.Empty(t, mock.cancelled, "Cancel must not be called when build fails")
	assert.Empty(t, mock.interruptAndReplaced, "InterruptAndReplace must not be called when build fails")
}
