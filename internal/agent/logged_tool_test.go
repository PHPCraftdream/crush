package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// A tool failure that reaches nobody is the failure mode this wrapper
// exists for: a run once died 42 seconds into a 75k-character prompt and
// the whole window in crush.log held no ERROR record at all, so the cause
// had to be recovered by reading source. These tests pin that both kinds of
// failure are written, and at levels that keep the log usable.

// stubTool returns whatever it is told to.
type stubTool struct {
	name string
	resp fantasy.ToolResponse
	err  error
}

func (s stubTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: s.name} }
func (s stubTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}

func (s stubTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (s stubTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return s.resp, s.err
}

// captureLogs swaps the default slog handler for one writing JSON into a
// buffer, and returns a reader over the records.
func captureLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err == nil {
				out = append(out, rec)
			}
		}
		return out
	}
}

func runWrapped(t *testing.T, inner fantasy.AgentTool) []map[string]any {
	t.Helper()
	records := captureLogs(t)
	wrapped := wrapToolsWithErrorLogging([]fantasy.AgentTool{inner})
	require.Len(t, wrapped, 1)

	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "sess-42")
	_, _ = wrapped[0].Run(ctx, fantasy.ToolCall{ID: "call-7", Name: inner.Info().Name})
	return records()
}

func TestLoggedTool_FatalErrorIsLoggedAtError(t *testing.T) {
	recs := runWrapped(t, stubTool{
		name: "write",
		err:  errors.New("disk is on fire"),
	})

	require.Len(t, recs, 1, "a returned error must produce exactly one record")
	rec := recs[0]
	require.Equal(t, "ERROR", rec["level"],
		"a returned error ends the whole run; anything quieter than ERROR buries it")
	require.Equal(t, "write", rec["tool"])
	require.Equal(t, "sess-42", rec["session_id"],
		"without the session id the record cannot be tied to the run that died — "+
			"exactly what was missing when this had to be diagnosed from source")
	require.Equal(t, "call-7", rec["tool_call_id"])
	require.Equal(t, "fatal", rec["level_kind"])
	require.Contains(t, rec["err"], "disk is on fire")
}

func TestLoggedTool_RecoverableErrorIsLoggedAtWarn(t *testing.T) {
	recs := runWrapped(t, stubTool{
		name: "view",
		resp: fantasy.NewTextErrorResponse("File not found: nope.txt"),
	})

	require.Len(t, recs, 1)
	rec := recs[0]
	require.Equal(t, "WARN", rec["level"],
		"the model mistyping a path is expected; at ERROR it would drown the failures "+
			"that actually end a session")
	require.Equal(t, "view", rec["tool"])
	require.Equal(t, "sess-42", rec["session_id"])
	require.Equal(t, "recoverable", rec["level_kind"])
	require.Contains(t, rec["content"], "nope.txt")
}

// The control: success must stay silent, or the log becomes a transcript of
// every tool call and stops being readable.
func TestLoggedTool_SuccessLogsNothing(t *testing.T) {
	recs := runWrapped(t, stubTool{
		name: "view",
		resp: fantasy.NewTextResponse("file contents"),
	})
	require.Empty(t, recs, "a successful tool call must not be logged")
}

// The wrapper must be transparent: whatever the inner tool returned has to
// come back unchanged, or logging would have altered behaviour.
func TestLoggedTool_PassesThroughUnchanged(t *testing.T) {
	inner := stubTool{name: "view", resp: fantasy.NewTextResponse("payload"), err: errors.New("boom")}
	wrapped := newLoggedTool(inner)

	resp, err := wrapped.Run(context.Background(), fantasy.ToolCall{ID: "c", Name: "view"})
	require.EqualError(t, err, "boom")
	require.Equal(t, "payload", resp.Content)
	require.Equal(t, "view", wrapped.Info().Name)
}
