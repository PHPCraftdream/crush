package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deadlineExceededModel is a fantasy.LanguageModel whose Stream call fails
// with a bare context.DeadlineExceeded, simulating what `crush run
// --timeout` produces when its root context deadline fires mid-turn (see
// run.go's timeoutDur handling). Only Stream is exercised by Run().
type deadlineExceededModel struct{}

func (deadlineExceededModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, context.DeadlineExceeded
}

func (deadlineExceededModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, context.DeadlineExceeded
}

func (deadlineExceededModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, context.DeadlineExceeded
}

func (deadlineExceededModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, context.DeadlineExceeded
}

func (deadlineExceededModel) Provider() string { return "test" }
func (deadlineExceededModel) Model() string    { return "deadline-exceeded" }

// TestRun_RunTimeoutDeadlineExceededSurfacesClearFinish reproduces the
// regression found while triaging a crashed session in another repo: a
// context.DeadlineExceeded from the run's own root --timeout wasn't matched
// by isCancelErr (errors.Is(err, context.Canceled) only), so it fell into
// the generic else branch as an unhelpful "Provider Error" indistinguishable
// from a real provider failure. It must now surface as a dedicated "Run
// timeout exceeded" finish.
func TestRun_RunTimeoutDeadlineExceededSurfacesClearFinish(t *testing.T) {
	env := testEnv(t)
	agent := testSessionAgent(env, deadlineExceededModel{}, deadlineExceededModel{}, "test system prompt")

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	_, err = agent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       sess.ID,
		MaxOutputTokens: 100,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	var assistant *message.Message
	for i := range msgs {
		if msgs[i].Role == message.Assistant {
			assistant = &msgs[i]
		}
	}
	require.NotNil(t, assistant, "expected an assistant message")

	finish := assistant.FinishPart()
	require.NotNil(t, finish)
	assert.Equal(t, message.FinishReasonError, finish.Reason)
	assert.Equal(t, "Run timeout exceeded", finish.Message)
}
