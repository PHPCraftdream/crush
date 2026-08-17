package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// A todos item with an unusable status must NOT end the run.
//
// It used to. Three things lined up:
//
//  1. TodoItem.Status was a bare `string` with only a `description` tag, so
//     the JSON schema handed to the provider carried no enum and nothing
//     stopped a model from misspelling the value or omitting the field
//     (Go unmarshals a missing field to "").
//  2. The tool rejected such a value by RETURNING an error
//     (`return fantasy.ToolResponse{}, fmt.Errorf("invalid status ...")`)
//     rather than by returning a tool-error RESPONSE.
//  3. fantasy distinguishes the two. In executeSingleTool a non-nil error
//     from the tool sets the second return value — isCriticalError at the
//     call site — and executeTools then does `return nil, errorResult.Error`,
//     aborting the agent loop. A response carrying IsError returns false
//     there and is fed back to the model as an ordinary tool result.
//
// So one mistyped enum value in one item killed a run that may have been
// going for many minutes, and the model never learned why. Observed in the
// wild: session t54-e-cards, 2026-08-17, ~42s into a run on a 75k prompt.
//
// The boundary that decides all of this is whether Run's error is nil, so
// that is what these tests assert on — not the message text alone, which
// would pass just as well with the fatal version.
func TestTodosTool_InvalidStatusIsRecoverableNotFatal(t *testing.T) {
	t.Parallel()

	sessions, _ := newTranscriptTestDB(t)
	sess, err := sessions.Create(context.Background(), "todos-status")
	require.NoError(t, err)

	run := func(t *testing.T, status string) (fantasy.ToolResponse, error) {
		t.Helper()
		input, mErr := json.Marshal(TodosParams{Todos: []TodoItem{{
			Content:    "do the thing",
			Status:     status,
			ActiveForm: "Doing the thing",
		}}})
		require.NoError(t, mErr)
		ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)
		return NewTodosTool(sessions).Run(ctx, fantasy.ToolCall{
			ID: "c1", Name: TodosToolName, Input: string(input),
		})
	}

	assertRecoverable := func(t *testing.T, status string) {
		t.Helper()
		resp, runErr := run(t, status)
		require.NoError(t, runErr,
			"a returned error is what fantasy escalates to isCriticalError — it would "+
				"abort the whole agent loop instead of letting the model fix its input")
		require.True(t, resp.IsError, "the model still has to be told it was wrong")
		require.Contains(t, resp.Content, "invalid status")
		require.Contains(t, resp.Content, "in_progress",
			"the message must spell out the legal values, since the likeliest cause "+
				"is the model not knowing them")
	}

	// The likeliest shape in practice: the field omitted entirely.
	t.Run("empty status", func(t *testing.T) { assertRecoverable(t, "") })
	// A near-miss spelling of a real value.
	t.Run("misspelled status", func(t *testing.T) { assertRecoverable(t, "in-progress") })

	// Nothing may be persisted from a rejected call — a partially applied
	// list would be worse than a rejected one, because the model would have
	// no way to tell which items landed.
	t.Run("rejected call saves nothing", func(t *testing.T) {
		fresh, getErr := sessions.Get(context.Background(), sess.ID)
		require.NoError(t, getErr)
		require.Empty(t, fresh.Todos)
	})

	// Control: a valid status still works, so the assertions above pin the
	// status check specifically rather than some unrelated setup failure.
	t.Run("valid status succeeds", func(t *testing.T) {
		resp, runErr := run(t, "pending")
		require.NoError(t, runErr)
		require.False(t, resp.IsError)
	})
}

// TestTodosTool_InfrastructureErrorsStayFatal is the other half of the rule.
// Making EVERY failure recoverable would be the opposite mistake: a missing
// session ID is a wiring bug the model cannot retry its way out of, and
// swallowing it into a tool-error response would leave the agent looping
// against a broken setup.
func TestTodosTool_InfrastructureErrorsStayFatal(t *testing.T) {
	t.Parallel()

	sessions, _ := newTranscriptTestDB(t)
	input, err := json.Marshal(TodosParams{Todos: []TodoItem{{
		Content: "x", Status: "pending", ActiveForm: "X",
	}}})
	require.NoError(t, err)

	// No session ID in the context at all.
	_, runErr := NewTodosTool(sessions).Run(context.Background(), fantasy.ToolCall{
		ID: "c1", Name: TodosToolName, Input: string(input),
	})
	require.Error(t, runErr, "a missing session ID must still be fatal")
	require.Contains(t, runErr.Error(), "session ID is required")
}

// TestTodosTool_SchemaConstrainsStatus covers the defence-in-depth half: the
// provider is now told which values are legal, so a well-behaved one can
// reject a bad call before it reaches us. This does NOT make the runtime
// check above redundant — schema enforcement varies by provider, and the
// wild failure this all came from was on a provider that let it through.
func TestTodosTool_SchemaConstrainsStatus(t *testing.T) {
	t.Parallel()

	sessions, _ := newTranscriptTestDB(t)
	raw, err := json.Marshal(NewTodosTool(sessions).Info().Parameters)
	require.NoError(t, err)

	var schema struct {
		Todos struct {
			Items struct {
				Properties struct {
					Status struct {
						Enum []string `json:"enum"`
					} `json:"status"`
				} `json:"properties"`
			} `json:"items"`
		} `json:"todos"`
	}
	require.NoError(t, json.Unmarshal(raw, &schema))
	require.Equal(t,
		[]string{"pending", "in_progress", "completed"},
		schema.Todos.Items.Properties.Status.Enum,
		"the enum must reach the provider-facing schema, at the right nesting depth — "+
			"a substring check for \"enum\" would pass even if it landed on the wrong field")
}
