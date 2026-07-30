package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunAgentTurnRecovered_Panic proves that a panic raised anywhere inside
// the function passed as runFn (standing in for app.AgentCoordinator.Run,
// which — per coordinator.go's runSubAgent and the fantasy library's fully
// synchronous tool-dispatch loop — can panic on behalf of ANY tool call
// executed during the turn, including a sub-agent delegation) does NOT crash
// the test process and instead surfaces as a single, clear agentTurnResponse
// on done, exactly once. Without runAgentTurnRecovered's recover(), this
// panic would propagate out of the goroutine and terminate the whole
// process via Go's default handler — this is precisely the "crush run died
// with zero log lines" failure mode being closed here.
func TestRunAgentTurnRecovered_Panic(t *testing.T) {
	done := make(chan agentTurnResponse, 1)

	panicking := func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
		panic("boom: simulated tool-call panic deep inside AgentCoordinator.Run")
	}

	// Run on its own goroutine, same as production, so an unrecovered panic
	// would take down the test binary rather than just this function.
	go runAgentTurnRecovered(t.Context(), "sess-1", "do the thing", panicking, done)

	select {
	case resp := <-done:
		require.Error(t, resp.err, "a recovered panic must still produce a non-nil error result")
		assert.Nil(t, resp.result)
		assert.Contains(t, resp.err.Error(), "panicked")
		assert.Contains(t, resp.err.Error(), "boom: simulated tool-call panic")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for done — panic was not recovered/reported, goroutine likely died silently")
	}
}

// TestRunAgentTurnRecovered_NormalError verifies that runAgentTurnRecovered
// leaves EXISTING, expected error handling completely untouched: a normal Go
// error returned by runFn (not a panic) must be wrapped and reported exactly
// as before, with no interference from the new recover().
func TestRunAgentTurnRecovered_NormalError(t *testing.T) {
	done := make(chan agentTurnResponse, 1)

	sentinel := assert.AnError
	erroring := func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
		return nil, sentinel
	}

	runAgentTurnRecovered(t.Context(), "sess-2", "prompt", erroring, done)

	resp := <-done
	require.Error(t, resp.err)
	assert.ErrorIs(t, resp.err, sentinel)
	assert.False(t, strings.Contains(resp.err.Error(), "panicked"),
		"a normal error must not be mislabeled as a panic")
}

// TestRunAgentTurnRecovered_NilResultNoError pins the existing
// ErrSessionBusy-mapping behaviour for a nil result with a nil error.
func TestRunAgentTurnRecovered_NilResultNoError(t *testing.T) {
	done := make(chan agentTurnResponse, 1)

	nilResult := func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
		return nil, nil
	}

	runAgentTurnRecovered(t.Context(), "sess-3", "prompt", nilResult, done)

	resp := <-done
	require.Error(t, resp.err)
	assert.Contains(t, resp.err.Error(), "failed to start agent processing stream")
}

// TestRunAgentTurnRecovered_Success verifies the plain happy path still
// reports the result with no error.
func TestRunAgentTurnRecovered_Success(t *testing.T) {
	done := make(chan agentTurnResponse, 1)

	want := &fantasy.AgentResult{}
	success := func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
		return want, nil
	}

	runAgentTurnRecovered(t.Context(), "sess-4", "prompt", success, done)

	resp := <-done
	require.NoError(t, resp.err)
	assert.Same(t, want, resp.result)
}
