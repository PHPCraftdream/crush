package agent

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// makeStep creates a StepResult with the given tool calls and results in its Content.
func makeStep(calls []fantasy.ToolCallContent, results []fantasy.ToolResultContent) fantasy.StepResult {
	var content fantasy.ResponseContent
	for _, c := range calls {
		content = append(content, c)
	}
	for _, r := range results {
		content = append(content, r)
	}
	return fantasy.StepResult{
		Response: fantasy.Response{
			Content: content,
		},
	}
}

// makeToolStep creates a step with a single tool call and matching text result.
func makeToolStep(name, input, output string) fantasy.StepResult {
	callID := fmt.Sprintf("call_%s_%s", name, input)
	return makeStep(
		[]fantasy.ToolCallContent{
			{ToolCallID: callID, ToolName: name, Input: input},
		},
		[]fantasy.ToolResultContent{
			{ToolCallID: callID, ToolName: name, Result: fantasy.ToolResultOutputContentText{Text: output}},
		},
	)
}

// makeEmptyStep creates a step with no tool calls (e.g. a text-only response).
func makeEmptyStep() fantasy.StepResult {
	return fantasy.StepResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "thinking..."},
			},
		},
	}
}

func TestHasRepeatedToolCalls(t *testing.T) {
	t.Run("no steps", func(t *testing.T) {
		result, _ := hasRepeatedToolCalls(nil, 10, 5)
		if result {
			t.Error("expected false for empty steps")
		}
	})

	t.Run("fewer steps than window", func(t *testing.T) {
		steps := make([]fantasy.StepResult, 5)
		for i := range steps {
			steps[i] = makeToolStep("read", `{"file":"a.go"}`, "content")
		}
		result, _ := hasRepeatedToolCalls(steps, 10, 5)
		if result {
			t.Error("expected false when fewer steps than window size")
		}
	})

	t.Run("all different signatures", func(t *testing.T) {
		steps := make([]fantasy.StepResult, 10)
		for i := range steps {
			steps[i] = makeToolStep("tool", fmt.Sprintf(`{"i":%d}`, i), fmt.Sprintf("result-%d", i))
		}
		result, _ := hasRepeatedToolCalls(steps, 10, 5)
		if result {
			t.Error("expected false when all signatures are different")
		}
	})

	t.Run("exact repeat at threshold not detected", func(t *testing.T) {
		// maxRepeats=5 means > 5 is needed, so exactly 5 should return false
		steps := make([]fantasy.StepResult, 10)
		for i := range 5 {
			steps[i] = makeToolStep("read", `{"file":"a.go"}`, "content")
		}
		for i := 5; i < 10; i++ {
			steps[i] = makeToolStep("tool", fmt.Sprintf(`{"i":%d}`, i), fmt.Sprintf("result-%d", i))
		}
		result, _ := hasRepeatedToolCalls(steps, 10, 5)
		if result {
			t.Error("expected false when count equals maxRepeats (threshold is >)")
		}
	})

	t.Run("loop detected", func(t *testing.T) {
		// 6 identical steps in a window of 10 with maxRepeats=5 → detected
		steps := make([]fantasy.StepResult, 10)
		for i := range 6 {
			steps[i] = makeToolStep("read", `{"file":"a.go"}`, "content")
		}
		for i := 6; i < 10; i++ {
			steps[i] = makeToolStep("tool", fmt.Sprintf(`{"i":%d}`, i), fmt.Sprintf("result-%d", i))
		}
		result, detail := hasRepeatedToolCalls(steps, 10, 5)
		if !result {
			t.Error("expected true when same signature appears more than maxRepeats times")
		}
		// Regression value: the detail must carry the tool name + count so the
		// Finish part's message can name the offending tool specifically.
		if detail.ToolName != "read" {
			t.Errorf("detail.ToolName = %q, want %q", detail.ToolName, "read")
		}
		if detail.Count != 6 {
			t.Errorf("detail.Count = %d, want 6", detail.Count)
		}
		if detail.Threshold != 5 {
			t.Errorf("detail.Threshold = %d, want 5", detail.Threshold)
		}
	})

	t.Run("steps without tool calls are skipped", func(t *testing.T) {
		// Mix of tool steps and empty steps — empty ones should not affect counts
		steps := make([]fantasy.StepResult, 10)
		for i := range 4 {
			steps[i] = makeToolStep("read", `{"file":"a.go"}`, "content")
		}
		for i := 4; i < 8; i++ {
			steps[i] = makeEmptyStep()
		}
		for i := 8; i < 10; i++ {
			steps[i] = makeToolStep("write", `{"file":"b.go"}`, "ok")
		}
		result, _ := hasRepeatedToolCalls(steps, 10, 5)
		if result {
			t.Error("expected false: only 4 repeated tool calls, empty steps should be skipped")
		}
	})

	t.Run("multiple different patterns alternating", func(t *testing.T) {
		// Two patterns alternating: each appears 5 times — not above threshold
		steps := make([]fantasy.StepResult, 10)
		for i := range steps {
			if i%2 == 0 {
				steps[i] = makeToolStep("read", `{"file":"a.go"}`, "content-a")
			} else {
				steps[i] = makeToolStep("write", `{"file":"b.go"}`, "content-b")
			}
		}
		result, _ := hasRepeatedToolCalls(steps, 10, 5)
		if result {
			t.Error("expected false: two patterns each appearing 5 times (not > 5)")
		}
	})
}

func TestGetToolInteractionSignature(t *testing.T) {
	t.Run("empty content returns empty string", func(t *testing.T) {
		sig := getToolInteractionSignature(fantasy.ResponseContent{})
		if sig != "" {
			t.Errorf("expected empty string, got %q", sig)
		}
	})

	t.Run("text only content returns empty string", func(t *testing.T) {
		content := fantasy.ResponseContent{
			fantasy.TextContent{Text: "hello"},
		}
		sig := getToolInteractionSignature(content)
		if sig != "" {
			t.Errorf("expected empty string, got %q", sig)
		}
	})

	t.Run("tool call with result produces signature", func(t *testing.T) {
		content := fantasy.ResponseContent{
			fantasy.ToolCallContent{ToolCallID: "1", ToolName: "read", Input: `{"file":"a.go"}`},
			fantasy.ToolResultContent{ToolCallID: "1", ToolName: "read", Result: fantasy.ToolResultOutputContentText{Text: "content"}},
		}
		sig := getToolInteractionSignature(content)
		if sig == "" {
			t.Error("expected non-empty signature")
		}
	})

	t.Run("same interactions produce same signature", func(t *testing.T) {
		content1 := fantasy.ResponseContent{
			fantasy.ToolCallContent{ToolCallID: "1", ToolName: "read", Input: `{"file":"a.go"}`},
			fantasy.ToolResultContent{ToolCallID: "1", ToolName: "read", Result: fantasy.ToolResultOutputContentText{Text: "content"}},
		}
		content2 := fantasy.ResponseContent{
			fantasy.ToolCallContent{ToolCallID: "2", ToolName: "read", Input: `{"file":"a.go"}`},
			fantasy.ToolResultContent{ToolCallID: "2", ToolName: "read", Result: fantasy.ToolResultOutputContentText{Text: "content"}},
		}
		sig1 := getToolInteractionSignature(content1)
		sig2 := getToolInteractionSignature(content2)
		if sig1 != sig2 {
			t.Errorf("expected same signature for same interactions, got %q and %q", sig1, sig2)
		}
	})

	t.Run("different inputs produce different signatures", func(t *testing.T) {
		content1 := fantasy.ResponseContent{
			fantasy.ToolCallContent{ToolCallID: "1", ToolName: "read", Input: `{"file":"a.go"}`},
			fantasy.ToolResultContent{ToolCallID: "1", ToolName: "read", Result: fantasy.ToolResultOutputContentText{Text: "content"}},
		}
		content2 := fantasy.ResponseContent{
			fantasy.ToolCallContent{ToolCallID: "1", ToolName: "read", Input: `{"file":"b.go"}`},
			fantasy.ToolResultContent{ToolCallID: "1", ToolName: "read", Result: fantasy.ToolResultOutputContentText{Text: "content"}},
		}
		sig1 := getToolInteractionSignature(content1)
		sig2 := getToolInteractionSignature(content2)
		if sig1 == sig2 {
			t.Error("expected different signatures for different inputs")
		}
	})
}

// TestLoopDetectedFinishText is the regression test for the loop-detection
// finish-reason fix. Before the fix, a loop-detected stop recorded an
// assistant message whose Finish part had EMPTY message/details —
// indistinguishable from a model that voluntarily finished (end_turn). An
// operator/orchestrator had no way to tell a legitimate polling pattern had
// been truncated. This test proves loopDetectedFinishText returns non-empty,
// specific strings (naming the offending tool + count) that get persisted on
// the Finish part via agent.go's OnStepFinish else-if branch.
//
// It would fail against the old code path (which passed "", "") because the
// helper did not exist — the only signal was a slog.Debug log line, never
// persisted on the message.
func TestLoopDetectedFinishText(t *testing.T) {
	t.Run("populated detail yields non-empty message and details", func(t *testing.T) {
		detail := loopDetail{ToolName: "job_output", Count: 6, Threshold: 5}
		msg, details := loopDetectedFinishText(detail)

		if msg == "" {
			t.Fatal("message must be non-empty — empty message is the exact bug (loop stop looks identical to voluntary finish)")
		}
		if details == "" {
			t.Fatal("details must be non-empty — empty details is the exact bug")
		}
		// Message must name the offending tool and the count, so it is
		// specific rather than generic.
		if !strings.Contains(msg, "job_output") {
			t.Errorf("message %q should name the tool %q", msg, "job_output")
		}
		if !strings.Contains(msg, "6") {
			t.Errorf("message %q should include the count 6", msg)
		}
		// Details must mention the legitimate-polling caveat (the operator
		// signal that distinguishes this from a clean finish).
		if !strings.Contains(details, "polling") {
			t.Errorf("details %q should mention polling so operators know the task may be unfinished", details)
		}
	})

	t.Run("empty tool name does not produce empty message", func(t *testing.T) {
		// Even when ToolName couldn't be extracted, the message must still be
		// non-empty (placeholder) — a loop stop must always be distinguishable.
		detail := loopDetail{ToolName: "", Count: 6, Threshold: 5}
		msg, details := loopDetectedFinishText(detail)
		if msg == "" || details == "" {
			t.Fatalf("message/details must be non-empty even with empty tool name; got msg=%q details=%q", msg, details)
		}
		if !strings.Contains(msg, "(unknown)") {
			t.Errorf("message %q should contain (unknown) placeholder for empty tool name", msg)
		}
	})
}
