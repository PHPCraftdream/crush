package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildRunResult_HappyPath(t *testing.T) {
	r := buildRunResult(
		"s1", "all done", "", "stop", nil, false,
		map[string]int{"bash": 3, "edit": 1},
		1234, 0.0021,
		2*time.Second+500*time.Millisecond,
		"", "",
		0, "", "",
	)
	assert.Equal(t, "s1", r.SessionID)
	assert.Equal(t, "stop", r.ExitReason)
	assert.Equal(t, "all done", r.FinalText)
	assert.Empty(t, r.Error)
	require.Len(t, r.ToolCalls, 2)
	// Sorted alphabetically — bash before edit.
	assert.Equal(t, "bash", r.ToolCalls[0].Name)
	assert.Equal(t, 3, r.ToolCalls[0].Count)
	assert.Equal(t, "edit", r.ToolCalls[1].Name)
	assert.Equal(t, int64(1234), r.Usage.DeltaTokens)
	assert.InDelta(t, 0.0021, r.Usage.DeltaCostUSD, 1e-9)
	assert.Equal(t, int64(2500), r.DurationMs)
}

func TestBuildRunResult_FallbackExitReason(t *testing.T) {
	cases := []struct {
		name       string
		finishStr  string
		err        error
		canceled   bool
		wantReason string
		wantErrSet bool
	}{
		{"empty + canceled → canceled", "", context.Canceled, true, "canceled", false},
		{"empty + error → error", "", errors.New("boom"), false, "error", true},
		{"empty + no err → unknown", "", nil, false, "unknown", false},
		{"explicit reason wins over fallback", "max_tokens", nil, false, "max_tokens", false},
		{"explicit reason wins over canceled fallback", "stop", context.Canceled, true, "stop", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := buildRunResult("s", "", "", tc.finishStr, tc.err, tc.canceled, nil, 0, 0, 0, "", "", 0, "", "")
			assert.Equal(t, tc.wantReason, r.ExitReason)
			if tc.wantErrSet {
				assert.NotEmpty(t, r.Error, "Error must surface non-cancel errors")
			} else {
				assert.Empty(t, r.Error, "Error must stay empty for cancel / success")
			}
		})
	}
}

func TestBuildRunResult_StableToolCallOrder(t *testing.T) {
	r := buildRunResult("s", "", "", "stop", nil, false,
		map[string]int{"zeta": 1, "alpha": 1, "mu": 2}, 0, 0, 0, "", "", 0, "", "")
	names := make([]string, len(r.ToolCalls))
	for i, t := range r.ToolCalls {
		names[i] = t.Name
	}
	assert.Equal(t, []string{"alpha", "mu", "zeta"}, names)
}

func TestBuildRunResult_NoToolCalls(t *testing.T) {
	r := buildRunResult("s", "done", "", "stop", nil, false, nil, 0, 0, 0, "", "", 0, "", "")
	require.NotNil(t, r.ToolCalls)
	assert.Empty(t, r.ToolCalls)
}

func TestBuildRunResult_AgentErrRequestCancelledIsHandledByCaller(t *testing.T) {
	// Sanity: we don't unwrap ErrRequestCancelled inside buildRunResult —
	// the call site classifies it as canceled and passes canceled=true.
	// Verify that with canceled=true the Error stays empty even though
	// the err itself is non-nil.
	r := buildRunResult("s", "", "", "", agent.ErrRequestCancelled, true, nil, 0, 0, 0, "", "", 0, "", "")
	assert.Equal(t, "canceled", r.ExitReason)
	assert.Empty(t, r.Error)
}
