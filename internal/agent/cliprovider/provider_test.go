package cliprovider

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestFormatPrompt(t *testing.T) {
	tests := []struct {
		name   string
		prompt fantasy.Prompt
		want   string
	}{
		{
			name:   "empty",
			prompt: nil,
			want:   "",
		},
		{
			name: "system message",
			prompt: fantasy.Prompt{
				fantasy.NewSystemMessage("You are helpful."),
			},
			want: "<system>\nYou are helpful.\n</system>",
		},
		{
			name: "user message",
			prompt: fantasy.Prompt{
				fantasy.NewUserMessage("Hello"),
			},
			want: "User: Hello",
		},
		{
			name: "full conversation",
			prompt: fantasy.Prompt{
				fantasy.NewSystemMessage("Be helpful."),
				fantasy.NewUserMessage("Hi"),
				{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Hello!"}}},
				fantasy.NewUserMessage("How are you?"),
			},
			want: "<system>\nBe helpful.\n</system>\n\nUser: Hi\n\nAssistant: Hello!\n\nUser: How are you?",
		},
		{
			name: "tool role included",
			prompt: fantasy.Prompt{
				{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "result: ok"}}},
			},
			want: "Tool: result: ok",
		},
		{
			name: "non-text parts skipped",
			prompt: fantasy.Prompt{
				{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
					fantasy.TextPart{Text: "Look at this"},
					fantasy.FilePart{Filename: "test.png", Data: []byte("fake"), MediaType: "image/png"},
				}},
			},
			want: "User: Look at this",
		},
		{
			name: "message with only non-text parts skipped entirely",
			prompt: fantasy.Prompt{
				{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
					fantasy.FilePart{Filename: "test.png", Data: []byte("fake"), MediaType: "image/png"},
				}},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatPrompt(tt.prompt)
			if got != tt.want {
				t.Errorf("formatPrompt() =\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestExtractText(t *testing.T) {
	msg := fantasy.Message{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "hello "},
			fantasy.TextPart{Text: "world"},
		},
	}
	got := extractText(msg)
	if got != "hello world" {
		t.Errorf("extractText() = %q, want %q", got, "hello world")
	}
}

func TestExtractTextNonText(t *testing.T) {
	msg := fantasy.Message{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "text"},
			fantasy.ToolCallPart{ToolCallID: "1", ToolName: "bash", Input: `{}`},
		},
	}
	got := extractText(msg)
	if got != "text" {
		t.Errorf("extractText() = %q, want %q", got, "text")
	}
}

func TestNewProvider(t *testing.T) {
	p := New("/tmp", nil, nil)
	if p.Name() != ProviderID {
		t.Errorf("Name() = %q, want %q", p.Name(), ProviderID)
	}
}

func TestLanguageModelUnknown(t *testing.T) {
	p := New("/tmp", nil, nil)
	_, err := p.LanguageModel(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
	if !strings.Contains(err.Error(), "unknown CLI model") {
		t.Errorf("error = %q, want to contain 'unknown CLI model'", err)
	}
}

func TestLanguageModelKnown(t *testing.T) {
	p := New("/tmp", nil, nil)
	lm, err := p.LanguageModel(context.Background(), "cli-claude-sonnet")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lm.Provider() != ProviderID {
		t.Errorf("Provider() = %q, want %q", lm.Provider(), ProviderID)
	}
	if lm.Model() != "cli-claude-sonnet" {
		t.Errorf("Model() = %q, want %q", lm.Model(), "cli-claude-sonnet")
	}
}

func TestStreamBinaryNotFound(t *testing.T) {
	spec := CLISpec{
		ModelID:    "test-missing",
		ModelName:  "Test Missing",
		Binary:     "this-binary-should-not-exist-anywhere-on-path",
		PromptFlag: "-p",
		BuildArgs:  func(bool) []string { return nil },
	}
	m := &cliModel{spec: spec, workingDir: t.TempDir()}
	_, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")},
	})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestStreamExitError(t *testing.T) {
	shell, flag := "bash", "-c"

	spec := CLISpec{
		ModelID:    "test-fail",
		ModelName:  "Test Fail",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs: func(bool) []string {
			return []string{flag, "echo output-text; echo error-text >&2; exit 1"}
		},
	}
	m := &cliModel{spec: spec, workingDir: t.TempDir()}
	stream, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	var gotError error
	var gotText strings.Builder
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			gotText.WriteString(part.Delta)
		case fantasy.StreamPartTypeError:
			gotError = part.Error
		}
	}

	if gotError == nil {
		t.Fatal("expected error from non-zero exit code")
	}
	if !strings.Contains(gotError.Error(), "error-text") {
		t.Errorf("error should contain stderr, got: %v", gotError)
	}
}

func TestStreamSuccess(t *testing.T) {
	shell, flag := "bash", "-c"

	spec := CLISpec{
		ModelID:    "test-ok",
		ModelName:  "Test OK",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs: func(bool) []string {
			return []string{flag, "echo hello world"}
		},
	}
	m := &cliModel{spec: spec, workingDir: t.TempDir()}
	stream, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	var text strings.Builder
	var finished bool
	var errPart error
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeFinish:
			finished = true
		case fantasy.StreamPartTypeError:
			errPart = part.Error
		}
	}

	if errPart != nil {
		t.Fatalf("unexpected error: %v", errPart)
	}
	if !finished {
		t.Error("expected finish part")
	}
	if !strings.Contains(text.String(), "hello world") {
		t.Errorf("output = %q, want to contain 'hello world'", text.String())
	}
}

func TestStreamContextCancel(t *testing.T) {
	shell := "bash"
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("shell %q not found", shell)
	}

	spec := CLISpec{
		ModelID:    "test-cancel",
		ModelName:  "Test Cancel",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs: func(bool) []string {
			return []string{"-c", "sleep 60"}
		},
	}
	m := &cliModel{spec: spec, workingDir: t.TempDir()}

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := m.Stream(ctx, fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	cancel()

	var gotError bool
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			gotError = true
		}
	}
	// The process should be killed either by exec.CommandContext or our ctx check.
	// We just verify the stream terminates without hanging.
	_ = gotError
}

func TestAvailable(t *testing.T) {
	available := Available()
	for _, spec := range available {
		if _, err := exec.LookPath(spec.Binary); err != nil {
			t.Errorf("Available() returned spec with missing binary %q", spec.Binary)
		}
	}
}

func TestMaxPromptArgLen(t *testing.T) {
	if maxPromptArgLen != 30*1024 {
		t.Errorf("maxPromptArgLen = %d, want %d", maxPromptArgLen, 30*1024)
	}
}

func TestBuildArgsPromptFlag(t *testing.T) {
	for _, spec := range All {
		args := spec.BuildArgs(false)
		for _, arg := range args {
			if arg == spec.PromptFlag {
				t.Errorf("BuildArgs for %s should not contain prompt flag %q", spec.ModelID, spec.PromptFlag)
			}
		}
	}
}

func TestClaudeArgs(t *testing.T) {
	fn := claudeArgs("sonnet", "--effort", "high")
	args := fn(false)
	if !contains(args, "--model") || !contains(args, "sonnet") || !contains(args, "--effort") {
		t.Errorf("claudeArgs(false) = %v, missing expected flags", args)
	}
	if contains(args, "--dangerously-skip-permissions") {
		t.Error("claudeArgs(false) should not include --dangerously-skip-permissions")
	}
	if !contains(args, "--output-format") || !contains(args, "stream-json") {
		t.Error("claudeArgs should include --output-format stream-json")
	}
	if !contains(args, "--include-partial-messages") {
		t.Error("claudeArgs should include --include-partial-messages")
	}

	argsYolo := fn(true)
	if !contains(argsYolo, "--dangerously-skip-permissions") {
		t.Error("claudeArgs(true) should include --dangerously-skip-permissions")
	}
}

func TestGeminiArgs(t *testing.T) {
	fn := geminiArgs("gemini-3-flash")
	args := fn(false)
	if !contains(args, "-m") || !contains(args, "gemini-3-flash") {
		t.Errorf("geminiArgs(false) = %v, missing expected flags", args)
	}
	if contains(args, "-y") {
		t.Error("geminiArgs(false) should not include -y")
	}
	if !contains(args, "--output-format") || !contains(args, "stream-json") {
		t.Error("geminiArgs should include --output-format stream-json")
	}

	argsYolo := fn(true)
	if !contains(argsYolo, "-y") {
		t.Error("geminiArgs(true) should include -y")
	}
}

func TestClaudePartParser(t *testing.T) {
	parse := claudePartParser()

	// Non-stream_event events are skipped
	initEvent, _ := json.Marshal(map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": "abc",
	})
	if _, ok := parse(initEvent); ok {
		t.Error("system event should be skipped")
	}

	// text_delta yields a TextDelta part
	ev1, _ := json.Marshal(map[string]any{
		"type": "stream_event",
		"event": map[string]any{
			"type": "content_block_delta",
			"delta": map[string]any{
				"type": "text_delta",
				"text": "Hello",
			},
		},
	})
	part, ok := parse(ev1)
	if !ok {
		t.Fatal("expected part from text_delta event")
	}
	if part.Type != fantasy.StreamPartTypeTextDelta {
		t.Errorf("part.Type = %v, want TextDelta", part.Type)
	}
	if part.Delta != "Hello" {
		t.Errorf("part.Delta = %q, want %q", part.Delta, "Hello")
	}

	// thinking_delta yields a ReasoningDelta part
	ev2, _ := json.Marshal(map[string]any{
		"type": "stream_event",
		"event": map[string]any{
			"type": "content_block_delta",
			"delta": map[string]any{
				"type":     "thinking_delta",
				"thinking": "I think...",
			},
		},
	})
	part, ok = parse(ev2)
	if !ok {
		t.Fatal("expected part from thinking_delta event")
	}
	if part.Type != fantasy.StreamPartTypeReasoningDelta {
		t.Errorf("part.Type = %v, want ReasoningDelta", part.Type)
	}
	if part.Delta != "I think..." {
		t.Errorf("part.Delta = %q, want %q", part.Delta, "I think...")
	}

	// content_block_start with thinking type yields ReasoningStart
	ev3, _ := json.Marshal(map[string]any{
		"type": "stream_event",
		"event": map[string]any{
			"type":          "content_block_start",
			"content_block": map[string]any{"type": "thinking"},
		},
	})
	part, ok = parse(ev3)
	if !ok {
		t.Fatal("expected ReasoningStart from content_block_start thinking")
	}
	if part.Type != fantasy.StreamPartTypeReasoningStart {
		t.Errorf("part.Type = %v, want ReasoningStart", part.Type)
	}

	// content_block_stop after thinking yields ReasoningEnd
	ev4, _ := json.Marshal(map[string]any{
		"type": "stream_event",
		"event": map[string]any{
			"type": "content_block_stop",
		},
	})
	part, ok = parse(ev4)
	if !ok {
		t.Fatal("expected ReasoningEnd from content_block_stop")
	}
	if part.Type != fantasy.StreamPartTypeReasoningEnd {
		t.Errorf("part.Type = %v, want ReasoningEnd", part.Type)
	}

	// result event is skipped
	resultEvent, _ := json.Marshal(map[string]any{
		"type":    "result",
		"subtype": "success",
	})
	if _, ok := parse(resultEvent); ok {
		t.Error("result event should be skipped")
	}

	// invalid JSON is skipped
	if _, ok := parse([]byte("not json")); ok {
		t.Error("invalid JSON should be skipped")
	}
}

func TestGeminiPartParser(t *testing.T) {
	parse := geminiPartParser()

	// Candidates-style event
	ev1, _ := json.Marshal(map[string]any{
		"candidates": []map[string]any{
			{
				"content": map[string]any{
					"parts": []map[string]any{
						{"text": "Hello"},
					},
					"role": "model",
				},
			},
		},
	})
	part, ok := parse(ev1)
	if !ok {
		t.Fatal("expected part from candidates event")
	}
	if part.Type != fantasy.StreamPartTypeTextDelta {
		t.Errorf("part.Type = %v, want TextDelta", part.Type)
	}
	if part.Delta != "Hello" {
		t.Errorf("part.Delta = %q, want %q", part.Delta, "Hello")
	}

	// Direct text field
	ev2, _ := json.Marshal(map[string]any{
		"text": "world",
	})
	part, ok = parse(ev2)
	if !ok {
		t.Fatal("expected part from text event")
	}
	if part.Delta != "world" {
		t.Errorf("part.Delta = %q, want %q", part.Delta, "world")
	}

	// Empty event
	ev3, _ := json.Marshal(map[string]any{
		"type": "metadata",
	})
	if _, ok := parse(ev3); ok {
		t.Error("empty event should be skipped")
	}

	// Invalid JSON
	if _, ok := parse([]byte("{bad")); ok {
		t.Error("invalid JSON should be skipped")
	}
}

func TestStreamWithPartParser(t *testing.T) {
	// Use stream_event/content_block_delta format (claude CLI --verbose output).
	jsonLines := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"He"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"llo"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":", world!"}}}`,
		`{"type":"result","result":"Hello, world!"}`,
	}, "\n") + "\n"

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "stream.jsonl")
	if err := os.WriteFile(tmpFile, []byte(jsonLines), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	shell, flag := "bash", "-c"
	readCmd := "cat " + strings.ReplaceAll(tmpFile, "\\", "/")

	spec := CLISpec{
		ModelID:    "test-stream-json",
		ModelName:  "Test Stream JSON",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs: func(bool) []string {
			return []string{flag, readCmd}
		},
		NewPartParser: claudePartParser,
	}
	m := &cliModel{spec: spec, workingDir: tmpDir}
	stream, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	var text strings.Builder
	var finished bool
	var errPart error
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeFinish:
			finished = true
		case fantasy.StreamPartTypeError:
			errPart = part.Error
		}
	}

	if errPart != nil {
		t.Fatalf("unexpected error: %v", errPart)
	}
	if !finished {
		t.Error("expected finish part")
	}
	got := text.String()
	if got != "Hello, world!" {
		t.Errorf("accumulated text = %q, want %q", got, "Hello, world!")
	}
}

func TestAllSpecsHavePartParser(t *testing.T) {
	for _, spec := range All {
		if spec.NewPartParser == nil {
			t.Errorf("spec %q has nil NewPartParser", spec.ModelID)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
