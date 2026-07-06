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

// TestLoopDetection_OnStepFinishOrdering is the regression test for the
// ordering bug in commit 44827270's fix.
//
// The bug: fantasy's internal Run loop calls OnStepFinish for a step BEFORE it
// evaluates StopWhen on that same step (verified against vendored
// charm.land/fantasy@v0.25.2 agent.go:936-947). The original fix set
// loopDetected/loopDetail inside the StopWhen closure and read them inside
// OnStepFinish's AddFinish chain. For the one step that actually trips the
// detector, OnStepFinish ran first and read a stale (still-false) flag → it
// took the plain else branch with empty message/details. StopWhen then set the
// flag and broke the loop, with NO later OnStepFinish to apply it.
//
// The fix: OnStepFinish maintains its OWN stepHistory accumulator, appends the
// current step at the top of the callback, and recomputes
// hasRepeatedToolCalls from that history BEFORE the AddFinish chain — so it
// knows loopDetected for the current step in time.
//
// This test simulates that exact sequence (append-then-recompute, mirroring
// OnStepFinish) and proves the detector fires on the SAME step whose
// insertion trips the threshold, with no extra step required. Against the
// broken code (relying on a StopWhen-set flag), the equivalent per-step loop
// would still see loopDetected=false on the tripping step — this test encodes
// the invariant the fix must uphold.
func TestLoopDetection_OnStepFinishOrdering(t *testing.T) {
	const (
		windowSize = 10
		maxRepeats = 5
	)

	// Build a sequence of steps: 4 filler steps first, then 6 identical
	// tool-call steps. Once the 6th identical step is appended (sequence
	// index 9), the trailing window of 10 contains all 6 identical steps
	// (4 fillers + 6 repeats = 10) → count 6 > maxRepeats(5) → fires.
	// This makes the tripping step deterministic: index 9.
	mkRepeat := func() fantasy.StepResult {
		return makeToolStep("job_output", `{"id":"j1"}`, "running")
	}
	mkFiller := func(i int) fantasy.StepResult {
		return makeToolStep("other", fmt.Sprintf(`{"i":%d}`, i), fmt.Sprintf("r%d", i))
	}

	var sequence []fantasy.StepResult
	for i := 0; i < 4; i++ {
		sequence = append(sequence, mkFiller(i))
	}
	for i := 0; i < 6; i++ {
		sequence = append(sequence, mkRepeat())
	}

	// Simulate OnStepFinish's per-step append+recompute (the fixed logic).
	var stepHistory []fantasy.StepResult
	var firedOnStep int = -1 // which step index first trips the detector
	var firedDetail loopDetail
	for i, step := range sequence {
		// Mirror agent.go OnStepFinish: append THIS step, THEN recompute.
		stepHistory = append(stepHistory, step)
		detected, detail := hasRepeatedToolCalls(stepHistory, windowSize, maxRepeats)
		if detected {
			firedOnStep = i
			firedDetail = detail
			break
		}
	}

	if firedOnStep < 0 {
		t.Fatal("loop detection never fired — test sequence is malformed; expected it to trip")
	}

	// The detector must fire on the step whose insertion pushes the repeat
	// count above the threshold — NOT one step later (which would be the
	// StopWhen-set-flag bug). With 4 fillers then 6 repeats, the 6th repeat
	// is at sequence index 9, and the window (last 10) is exactly full and
	// contains all 6 repeats. Assert exactly that.
	if firedOnStep != 9 {
		t.Fatalf("expected detection on step index 9 (the 6th identical repeat, when the window first holds 6 of them), got %d — this is the ordering bug: detection is happening on the wrong step", firedOnStep)
	}

	// The detail computed AT THAT STEP must be populated — OnStepFinish uses
	// it immediately to build the finish text, with no later callback to
	// fill it in.
	if firedDetail.ToolName != "job_output" {
		t.Errorf("detail.ToolName = %q, want %q", firedDetail.ToolName, "job_output")
	}
	if firedDetail.Count != 6 {
		t.Errorf("detail.Count = %d, want 6", firedDetail.Count)
	}

	// And the finish text built from that same-step detail must be non-empty
	// (this is what gets persisted on the assistant message's Finish part).
	msg, details := loopDetectedFinishText(firedDetail)
	if msg == "" || details == "" {
		t.Fatalf("finish text must be non-empty on the detection step; got msg=%q details=%q", msg, details)
	}
}
