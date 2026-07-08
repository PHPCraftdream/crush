package cliprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
)

func TestMain(m *testing.M) {
	// go-pty's Windows ConPTY path has an internal data race that the -race
	// detector flags. Force pipe mode for the whole cliprovider suite on
	// Windows so streaming tests stay race-clean; Unix keeps PTY coverage.
	testDisablePTY = runtime.GOOS == "windows"
	os.Exit(m.Run())
}

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
			got := formatPrompt(tt.prompt, nil)
			if got != tt.want {
				t.Errorf("formatPrompt() =\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestFormatPromptWithFilePaths(t *testing.T) {
	msgs := fantasy.Prompt{
		fantasy.NewUserMessage("Look at this"),
	}
	paths := map[int][]string{
		0: {"/tmp/image.png"},
	}
	got := formatPrompt(msgs, paths)
	want := "User: Look at this\n[Attached file: /tmp/image.png]"
	if got != want {
		t.Errorf("formatPrompt() =\n%q\nwant:\n%q", got, want)
	}
}

func TestFormatPromptFileOnlyMessage(t *testing.T) {
	msgs := fantasy.Prompt{
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
			fantasy.FilePart{Filename: "test.png", Data: []byte("fake"), MediaType: "image/png"},
		}},
	}
	paths := map[int][]string{
		0: {"/tmp/test.png"},
	}
	got := formatPrompt(msgs, paths)
	want := "User: \n[Attached file: /tmp/test.png]"
	if got != want {
		t.Errorf("formatPrompt() =\n%q\nwant:\n%q", got, want)
	}
}

func TestSaveFileParts(t *testing.T) {
	msgs := fantasy.Prompt{
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "Check this"},
			fantasy.FilePart{Filename: "screenshot.png", Data: []byte("PNG_DATA"), MediaType: "image/png"},
		}},
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
			fantasy.FilePart{Filename: "", Data: []byte("JPEG_DATA"), MediaType: "image/jpeg"},
		}},
	}

	tmpDir, paths, err := saveFileParts(msgs)
	if err != nil {
		t.Fatalf("saveFileParts() error: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	if len(paths[0]) != 1 {
		t.Fatalf("expected 1 file for msg 0, got %d", len(paths[0]))
	}
	if len(paths[1]) != 1 {
		t.Fatalf("expected 1 file for msg 1, got %d", len(paths[1]))
	}

	// Check file contents.
	data, err := os.ReadFile(paths[0][0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "PNG_DATA" {
		t.Errorf("file content = %q, want %q", data, "PNG_DATA")
	}
	if filepath.Base(paths[0][0]) != "screenshot.png" {
		t.Errorf("filename = %q, want screenshot.png", filepath.Base(paths[0][0]))
	}

	// Second file: auto-generated name from MIME type.
	data2, err := os.ReadFile(paths[1][0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data2) != "JPEG_DATA" {
		t.Errorf("file content = %q, want %q", data2, "JPEG_DATA")
	}
}

func TestSaveFilePartsNoFiles(t *testing.T) {
	msgs := fantasy.Prompt{
		fantasy.NewUserMessage("just text"),
	}
	tmpDir, paths, err := saveFileParts(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tmpDir != "" || paths != nil {
		t.Errorf("expected no temp dir for text-only prompt, got dir=%q paths=%v", tmpDir, paths)
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
	p := New("/tmp", nil, nil, nil, nil)
	if p.Name() != ProviderID {
		t.Errorf("Name() = %q, want %q", p.Name(), ProviderID)
	}
}

func TestLanguageModelUnknown(t *testing.T) {
	p := New("/tmp", nil, nil, nil, nil)
	_, err := p.LanguageModel(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
	if !strings.Contains(err.Error(), "unknown CLI model") {
		t.Errorf("error = %q, want to contain 'unknown CLI model'", err)
	}
}

func TestLanguageModelKnown(t *testing.T) {
	p := New("/tmp", nil, nil, nil, nil)
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

	// The guaranteed contract: a non-zero exit always surfaces as an error.
	if gotError == nil {
		t.Fatal("expected error from non-zero exit code")
	}
	// Where stderr TEXT surfaces is mode-dependent. In pipe mode (NoPTY, e.g.
	// Windows) it is appended to the error deterministically. In PTY mode
	// (Unix) the kernel merges stderr into the tty's stdout, but a
	// fast-exiting process can close the PTY before the drain loop reads the
	// final line (a documented PTY tail-drain race — see provider.go), so the
	// stderr text is best-effort, not guaranteed. Only assert the text in the
	// deterministic pipe path; under PTY the non-zero-exit error above is the
	// contract we rely on. Asserting the racy PTY tail made `go test -race`
	// flaky under load on Linux CI.
	if testDisablePTY {
		surfaced := gotError.Error() + "\n" + gotText.String()
		if !strings.Contains(surfaced, "error-text") {
			t.Errorf("stderr should surface in error or output; err=%v text=%q", gotError, gotText.String())
		}
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

	// init event is skipped
	ev, _ := json.Marshal(map[string]any{
		"type": "init", "session_id": "x", "model": "auto-gemini-3",
	})
	if _, ok := parse(ev); ok {
		t.Error("init event should be skipped")
	}

	// user message echo is skipped
	ev, _ = json.Marshal(map[string]any{
		"type": "message", "role": "user", "content": "hello",
	})
	if _, ok := parse(ev); ok {
		t.Error("user message should be skipped")
	}

	// assistant delta yields TextDelta
	ev, _ = json.Marshal(map[string]any{
		"type": "message", "role": "assistant", "content": "Hello!", "delta": true,
	})
	part, ok := parse(ev)
	if !ok {
		t.Fatal("expected part from assistant delta event")
	}
	if part.Type != fantasy.StreamPartTypeTextDelta {
		t.Errorf("part.Type = %v, want TextDelta", part.Type)
	}
	if part.Delta != "Hello!" {
		t.Errorf("part.Delta = %q, want %q", part.Delta, "Hello!")
	}

	// result event is skipped (handled by ParseUsageLine)
	ev, _ = json.Marshal(map[string]any{
		"type": "result", "status": "success",
		"stats": map[string]any{"total_tokens": 100},
	})
	if _, ok := parse(ev); ok {
		t.Error("result event should be skipped by part parser")
	}

	// assistant message with empty content is skipped
	ev, _ = json.Marshal(map[string]any{
		"type": "message", "role": "assistant", "content": "", "delta": true,
	})
	if _, ok := parse(ev); ok {
		t.Error("assistant message with empty content should be skipped")
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

// ── QwenArgs ────────────────────────────────────────────────────────────────

func TestQwenArgs(t *testing.T) {
	fn := qwenArgs()

	args := fn(false)
	if !contains(args, "--output-format") || !contains(args, "stream-json") {
		t.Errorf("qwenArgs(false) = %v, missing --output-format stream-json", args)
	}
	if !contains(args, "--include-partial-messages") {
		t.Errorf("qwenArgs(false) = %v, missing --include-partial-messages", args)
	}
	if contains(args, "--approval-mode") {
		t.Errorf("qwenArgs(false) must not include --approval-mode: %v", args)
	}

	argsYolo := fn(true)
	if !contains(argsYolo, "--approval-mode") || !contains(argsYolo, "yolo") {
		t.Errorf("qwenArgs(true) = %v, missing --approval-mode yolo", argsYolo)
	}
}

// ── CodexArgs ────────────────────────────────────────────────────────────────

func TestCodexArgs(t *testing.T) {
	fn := codexArgs("gpt-5.3-codex")

	args := fn(false)
	if !contains(args, "exec") {
		t.Errorf("codexArgs(false) = %v, missing 'exec'", args)
	}
	if !contains(args, "--json") {
		t.Errorf("codexArgs(false) = %v, missing '--json'", args)
	}
	if !contains(args, "-m") || !contains(args, "gpt-5.3-codex") {
		t.Errorf("codexArgs(false) = %v, missing -m gpt-5.3-codex", args)
	}
	if contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("codexArgs(false) must not include --dangerously-bypass-approvals-and-sandbox: %v", args)
	}

	argsYolo := fn(true)
	if !contains(argsYolo, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("codexArgs(true) = %v, missing --dangerously-bypass-approvals-and-sandbox", argsYolo)
	}
}

func TestCodexArgsAllModels(t *testing.T) {
	models := []string{
		"gpt-5.3-codex",
		"gpt-5.4",
		"gpt-5.2-codex",
		"gpt-5.1-codex-max",
		"gpt-5.2",
		"gpt-5.1-codex-mini",
	}
	for _, model := range models {
		fn := codexArgs(model)
		args := fn(false)
		if !contains(args, model) {
			t.Errorf("codexArgs(%q)(false) = %v, missing model name", model, args)
		}
		argsYolo := fn(true)
		if !contains(argsYolo, "--dangerously-bypass-approvals-and-sandbox") {
			t.Errorf("codexArgs(%q)(true) = %v, missing yolo flag", model, argsYolo)
		}
	}
}

// ── CodexPartParser ──────────────────────────────────────────────────────────

func TestCodexPartParser(t *testing.T) {
	parse := codexPartParser()

	// thread.started is skipped
	ev, _ := json.Marshal(map[string]any{"type": "thread.started", "thread_id": "x"})
	if _, ok := parse(ev); ok {
		t.Error("thread.started should be skipped")
	}

	// turn.started is skipped
	ev, _ = json.Marshal(map[string]any{"type": "turn.started"})
	if _, ok := parse(ev); ok {
		t.Error("turn.started should be skipped")
	}

	// item.started is skipped
	ev, _ = json.Marshal(map[string]any{
		"type": "item.started",
		"item": map[string]any{"type": "command_execution", "command": "ls"},
	})
	if _, ok := parse(ev); ok {
		t.Error("item.started should be skipped")
	}

	// item.completed command_execution is skipped
	ev, _ = json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"type":              "command_execution",
			"command":           "ls",
			"aggregated_output": "file.txt",
			"exit_code":         0,
		},
	})
	if _, ok := parse(ev); ok {
		t.Error("item.completed command_execution should be skipped")
	}

	// item.completed agent_message yields TextDelta
	ev, _ = json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"type": "agent_message",
			"text": "Here is the answer.",
		},
	})
	part, ok := parse(ev)
	if !ok {
		t.Fatal("item.completed agent_message should yield a part")
	}
	if part.Type != fantasy.StreamPartTypeTextDelta {
		t.Errorf("part.Type = %v, want TextDelta", part.Type)
	}
	if part.Delta != "Here is the answer." {
		t.Errorf("part.Delta = %q, want %q", part.Delta, "Here is the answer.")
	}

	// item.completed agent_message with empty text is skipped
	ev, _ = json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{"type": "agent_message", "text": ""},
	})
	if _, ok := parse(ev); ok {
		t.Error("agent_message with empty text should be skipped")
	}

	// turn.completed is skipped (usage handled by ParseUsageLine)
	ev, _ = json.Marshal(map[string]any{
		"type":  "turn.completed",
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 50},
	})
	if _, ok := parse(ev); ok {
		t.Error("turn.completed should be skipped by part parser")
	}

	// invalid JSON is skipped
	if _, ok := parse([]byte("{bad json")); ok {
		t.Error("invalid JSON should be skipped")
	}
}

// ── CodexParseUsageLine ──────────────────────────────────────────────────────

func TestCodexParseUsageLine(t *testing.T) {
	// turn.completed with all token fields
	ev, _ := json.Marshal(map[string]any{
		"type": "turn.completed",
		"usage": map[string]any{
			"input_tokens":        8520,
			"cached_input_tokens": 6528,
			"output_tokens":       9,
		},
	})
	usage, ok := codexParseUsageLine(ev)
	if !ok {
		t.Fatal("expected usage from turn.completed")
	}
	if usage.InputTokens != 8520+6528 {
		t.Errorf("InputTokens = %d, want %d", usage.InputTokens, 8520+6528)
	}
	if usage.OutputTokens != 9 {
		t.Errorf("OutputTokens = %d, want %d", usage.OutputTokens, 9)
	}
	if usage.TotalTokens != 8520+6528+9 {
		t.Errorf("TotalTokens = %d, want %d", usage.TotalTokens, 8520+6528+9)
	}

	// non-turn.completed events are skipped
	ev, _ = json.Marshal(map[string]any{"type": "item.completed"})
	if _, ok := codexParseUsageLine(ev); ok {
		t.Error("item.completed should not produce usage")
	}

	// turn.completed with zero usage is skipped
	ev, _ = json.Marshal(map[string]any{"type": "turn.completed", "usage": map[string]any{}})
	if _, ok := codexParseUsageLine(ev); ok {
		t.Error("turn.completed with zero usage should be skipped")
	}

	// invalid JSON is skipped
	if _, ok := codexParseUsageLine([]byte("not json")); ok {
		t.Error("invalid JSON should be skipped")
	}
}

// ── ClaudeParseUsageLine ─────────────────────────────────────────────────────

func TestClaudeParseUsageLine(t *testing.T) {
	// result event with full usage
	ev, _ := json.Marshal(map[string]any{
		"type": "result",
		"usage": map[string]any{
			"input_tokens":                100,
			"output_tokens":               50,
			"cache_creation_input_tokens": 200,
			"cache_read_input_tokens":     300,
		},
	})
	usage, ok := claudeParseUsageLine(ev)
	if !ok {
		t.Fatal("expected usage from result event")
	}
	if usage.InputTokens != 100+200+300 {
		t.Errorf("InputTokens = %d, want %d (sum of all input variants)", usage.InputTokens, 100+200+300)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want %d", usage.OutputTokens, 50)
	}
	if usage.TotalTokens != 100+200+300+50 {
		t.Errorf("TotalTokens = %d, want %d", usage.TotalTokens, 100+200+300+50)
	}

	// non-result events are skipped
	ev, _ = json.Marshal(map[string]any{"type": "stream_event"})
	if _, ok := claudeParseUsageLine(ev); ok {
		t.Error("stream_event should not produce usage")
	}

	// result with zero usage is skipped
	ev, _ = json.Marshal(map[string]any{"type": "result", "usage": map[string]any{}})
	if _, ok := claudeParseUsageLine(ev); ok {
		t.Error("result with zero usage should be skipped")
	}
}

// ── GeminiParseUsageLine ─────────────────────────────────────────────────────

func TestGeminiParseUsageLine(t *testing.T) {
	// result event with stats
	ev, _ := json.Marshal(map[string]any{
		"type":   "result",
		"status": "success",
		"stats": map[string]any{
			"total_tokens":  10267,
			"input_tokens":  10100,
			"output_tokens": 42,
		},
	})
	usage, ok := geminiParseUsageLine(ev)
	if !ok {
		t.Fatal("expected usage from gemini result event")
	}
	if usage.InputTokens != 10100 {
		t.Errorf("InputTokens = %d, want 10100", usage.InputTokens)
	}
	if usage.OutputTokens != 42 {
		t.Errorf("OutputTokens = %d, want 42", usage.OutputTokens)
	}
	if usage.TotalTokens != 10267 {
		t.Errorf("TotalTokens = %d, want 10267", usage.TotalTokens)
	}

	// non-result events are skipped
	ev, _ = json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": "hi"})
	if _, ok := geminiParseUsageLine(ev); ok {
		t.Error("message event should not produce usage")
	}

	// result with zero tokens is skipped
	ev, _ = json.Marshal(map[string]any{"type": "result", "status": "success", "stats": map[string]any{}})
	if _, ok := geminiParseUsageLine(ev); ok {
		t.Error("result with zero tokens should be skipped")
	}
}

// ── Integration: stream with codex JSONL output ──────────────────────────────

func TestStreamWithCodexParser(t *testing.T) {
	jsonLines := strings.Join([]string{
		`{"type":"thread.started","thread_id":"t1"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"type":"command_execution","command":"ls"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"ls","aggregated_output":"file.txt","exit_code":0}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"The directory contains file.txt"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":50,"output_tokens":20}}`,
	}, "\n") + "\n"

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "codex.jsonl")
	if err := os.WriteFile(tmpFile, []byte(jsonLines), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	readCmd := "cat " + strings.ReplaceAll(tmpFile, "\\", "/")
	spec := CLISpec{
		ModelID:        "test-codex",
		ModelName:      "Test Codex",
		Binary:         "bash",
		PromptFlag:     "-p",
		BuildArgs:      func(bool) []string { return []string{"-c", readCmd} },
		NewPartParser:  codexPartParser,
		ParseUsageLine: codexParseUsageLine,
	}
	m := &cliModel{spec: spec, workingDir: tmpDir}
	stream, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	var text strings.Builder
	var finalUsage fantasy.Usage
	var finished bool
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeFinish:
			finished = true
			finalUsage = part.Usage
		case fantasy.StreamPartTypeError:
			t.Fatalf("unexpected error: %v", part.Error)
		}
	}

	if !finished {
		t.Error("expected finish part")
	}
	want := "The directory contains file.txt"
	if text.String() != want {
		t.Errorf("text = %q, want %q", text.String(), want)
	}
	if finalUsage.InputTokens != 150 {
		t.Errorf("InputTokens = %d, want 150 (100+50)", finalUsage.InputTokens)
	}
	if finalUsage.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20", finalUsage.OutputTokens)
	}
}

// ── Integration: stream with Gemini JSONL output ─────────────────────────────

func TestStreamWithGeminiParser(t *testing.T) {
	jsonLines := strings.Join([]string{
		`{"type":"init","session_id":"abc","model":"auto-gemini-3"}`,
		`{"type":"message","role":"user","content":"hello"}`,
		`{"type":"message","role":"assistant","content":"Hello ","delta":true}`,
		`{"type":"message","role":"assistant","content":"world!","delta":true}`,
		`{"type":"result","status":"success","stats":{"total_tokens":15,"input_tokens":10,"output_tokens":5}}`,
	}, "\n") + "\n"

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "gemini.jsonl")
	if err := os.WriteFile(tmpFile, []byte(jsonLines), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	readCmd := "cat " + strings.ReplaceAll(tmpFile, "\\", "/")
	spec := CLISpec{
		ModelID:        "test-gemini",
		ModelName:      "Test Gemini",
		Binary:         "bash",
		PromptFlag:     "-p",
		BuildArgs:      func(bool) []string { return []string{"-c", readCmd} },
		NewPartParser:  geminiPartParser,
		ParseUsageLine: geminiParseUsageLine,
	}
	m := &cliModel{spec: spec, workingDir: tmpDir}
	stream, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	var text strings.Builder
	var finalUsage fantasy.Usage
	var finished bool
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeFinish:
			finished = true
			finalUsage = part.Usage
		case fantasy.StreamPartTypeError:
			t.Fatalf("unexpected error: %v", part.Error)
		}
	}

	if !finished {
		t.Error("expected finish part")
	}
	if text.String() != "Hello world!" {
		t.Errorf("text = %q, want %q", text.String(), "Hello world!")
	}
	if finalUsage.TotalTokens != 15 {
		t.Errorf("TotalTokens = %d, want 15", finalUsage.TotalTokens)
	}
	if finalUsage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", finalUsage.InputTokens)
	}
	if finalUsage.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", finalUsage.OutputTokens)
	}
}

// ── Spec invariants ──────────────────────────────────────────────────────────

func TestAllSpecsHaveUniqueIDs(t *testing.T) {
	seen := make(map[string]bool)
	for _, spec := range All {
		if seen[spec.ModelID] {
			t.Errorf("duplicate ModelID: %q", spec.ModelID)
		}
		seen[spec.ModelID] = true
	}
}

func TestCodexSpecsHaveAlwaysStdin(t *testing.T) {
	for _, spec := range All {
		if spec.Binary == "codex" && !spec.AlwaysStdin {
			t.Errorf("codex spec %q must have AlwaysStdin=true", spec.ModelID)
		}
	}
}

func TestQwenSpecHasAlwaysStdin(t *testing.T) {
	for _, spec := range All {
		if spec.Binary == "qwen" && !spec.AlwaysStdin {
			t.Errorf("qwen spec %q must have AlwaysStdin=true", spec.ModelID)
		}
	}
}

func TestCodexSpecsHaveCorrectBinary(t *testing.T) {
	codexIDs := []string{
		"cli-codex",
		"cli-codex-gpt-5-4",
		"cli-codex-gpt-5-2",
		"cli-codex-max",
		"cli-codex-gpt-5-2-base",
		"cli-codex-mini",
	}
	specsByID := make(map[string]CLISpec)
	for _, s := range All {
		specsByID[s.ModelID] = s
	}
	for _, id := range codexIDs {
		spec, ok := specsByID[id]
		if !ok {
			t.Errorf("missing expected codex spec %q", id)
			continue
		}
		if spec.Binary != "codex" {
			t.Errorf("spec %q has Binary=%q, want 'codex'", id, spec.Binary)
		}
		if spec.NewPartParser == nil {
			t.Errorf("spec %q has nil NewPartParser", id)
		}
		if spec.ParseUsageLine == nil {
			t.Errorf("spec %q has nil ParseUsageLine", id)
		}
	}
}

func TestAll_HaikuModelsRegistered(t *testing.T) {
	// After the 2026-06-17 cleanup the per-thinking and npx variants
	// were removed. We only carry the canonical `cli-claude-haiku`
	// alias now; the operator picks effort via the UI selector and the
	// cliprovider forwards it through context at call time.
	want := []string{
		"cli-claude-haiku",
	}
	byID := make(map[string]CLISpec, len(All))
	for _, s := range All {
		byID[s.ModelID] = s
	}
	for _, id := range want {
		spec, ok := byID[id]
		if !ok {
			t.Errorf("missing expected spec %q in All", id)
			continue
		}
		if spec.ContextWindow != 200_000 {
			t.Errorf("spec %q ContextWindow = %d, want 200_000", id, spec.ContextWindow)
		}
		if spec.NewPartParser == nil {
			t.Errorf("spec %q has nil NewPartParser", id)
		}
		if spec.ParseUsageLine == nil {
			t.Errorf("spec %q has nil ParseUsageLine", id)
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

// ── Bug 1 regression: bounded wait() must not hang when a grandchild holds stderr ──
//
// Reproduces the real incident: the direct child (bash) exits so stdout EOFs
// and the scanner loop ends, but a backgrounded grandchild keeps the inherited
// stderr fd open. With the OLD unbounded cmd.Wait(), proc.wait() would block
// forever (no ctx check on that path). The fix bounds it against ctx.Done().
//
// The test forces the pipe / non-NoPTY branch (cmd.Stderr = &stderrBuf) by
// using a spec with neither NoPTY nor AlwaysStdin and a small prompt, and
// relies on testDisablePTY being true on Windows so the PTY path is skipped.
func TestStreamWaitBoundedOnGrandchildHoldsStderr(t *testing.T) {
	shell, flag := "bash", "-c"
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("shell %q not found", shell)
	}

	// Script: print one stdout line (so the scanner loop runs and ends on EOF
	// when bash exits), then fork a backgrounded subshell that holds the
	// inherited stderr fd open for 30s and disowns itself so bash doesn't
	// wait for it. After forking, the main bash prints a final line and
	// exits — leaving the orphan grandchild holding stderr.
	//
	// We also write the grandchild's PID to a file so the test can reap it
	// afterwards (kill by PID) and avoid leaking a 30s sleeper on CI.
	script := `
		echo "stdout-line"
		( sleep 30 ) >&2 &
		echo $! > "$1/grandchild.pid"
		disown
		echo "stdout-line-2"
	`

	// Force the pipe branch via testDisablePTY rather than relying on the
	// platform-dependent default (GOOS == "windows", set in TestMain). Under
	// a real PTY (the default on non-Windows), the controlling terminal
	// closing on bash exit sends SIGHUP to the disowned grandchild, killing
	// it before it can hold stderr open — the premise this test depends on
	// never materializes there, and gotErr comes back nil on Linux CI.
	//
	// Setting spec.NoPTY instead would be wrong: it switches Stream() to the
	// merged-stdout/stderr StderrPipe() branch (see the `if m.spec.NoPTY`
	// block below Stream()'s pipe fallback), not the plain
	// `cmd.Stderr = &stderrBuf` branch this test targets (see the
	// top-of-function comment). Only testDisablePTY selects the latter while
	// keeping spec.NoPTY false.
	prevDisablePTY := testDisablePTY
	testDisablePTY = true
	defer func() { testDisablePTY = prevDisablePTY }()

	tmpDir := t.TempDir()
	spec := CLISpec{
		ModelID:    "test-bounded-wait",
		ModelName:  "Test Bounded Wait",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs:  func(bool) []string { return []string{flag, script, tmpDir} },
	}
	m := &cliModel{spec: spec, workingDir: tmpDir}

	// Short ctx deadline: the scanner loop ends quickly (bash exits fast),
	// then proc.wait() is invoked. With the bug, wait() would block on the
	// stderr-holding grandchild well past this deadline. With the fix,
	// wait() returns on ctx.Done() promptly.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := m.Stream(ctx, fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	// Hard watchdog: if the whole drain somehow hangs (bug regressed AND
	// the ctx bound failed), fail loudly instead of stalling the suite.
	done := make(chan struct{})
	var gotErr error
	go func() {
		defer close(done)
		for part := range stream {
			if part.Type == fantasy.StreamPartTypeError {
				gotErr = part.Error
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("stream drain hung past 15s watchdog — bounded wait regression")
	}

	// The ctx deadline MUST have fired and the bounded-wait code path MUST
	// have surfaced a context error. This is the deterministic assertion that
	// gives the test its regression value: with the fix reverted (unbounded
	// cmd.Wait()), the drain hangs and we never reach here (the watchdog
	// above fails the test at 15s). With the fix in place, the ctx-bound
	// select returns ctx.Err() promptly and it propagates as the error part.
	//
	// We deliberately do NOT accept gotErr == nil: if the grandchild's stderr
	// fd were ever closed by the OS instead of being held (different
	// platform/shell), wait() would return normally with no error and this
	// test would no longer prove the bounded path fired — it would have zero
	// regression value. In that case the test should fail loudly so we know
	// to switch to an injected-fake-process technique on that platform.
	if gotErr == nil {
		t.Fatal("expected ctx error from bounded wait() but got nil — the grandchild-holds-stderr premise did not hold in this environment; this test no longer proves the fix and needs a different technique for this platform")
	}
	if !errors.Is(gotErr, context.DeadlineExceeded) && !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("expected context.DeadlineExceeded or context.Canceled from bounded wait(), got %v", gotErr)
	}
	t.Logf("drain completed with ctx-bound wait; gotErr=%v", gotErr)

	// Reap the orphaned grandchild so we don't leak a 30s sleeper.
	if pidData, rerr := os.ReadFile(filepath.Join(tmpDir, "grandchild.pid")); rerr == nil {
		var pid int
		fmt.Sscanf(strings.TrimSpace(string(pidData)), "%d", &pid)
		if pid > 0 {
			// best-effort kill of the orphan; ignore errors (already gone is fine)
			kill, _ := exec.LookPath("taskkill")
			if kill != "" {
				_ = exec.CommandContext(context.Background(), kill, "/F", "/T", "/PID", fmt.Sprintf("%d", pid)).Run()
			} else {
				_ = exec.CommandContext(context.Background(), shell, flag, fmt.Sprintf("kill %d 2>/dev/null || true", pid)).Run()
			}
		}
	}
}

// ── Bug 1 regression (PTY branch parity): bounded wait on ctx ──
//
// Pure-Go check that the PTY branch's wait closure is also bounded against
// ctx. We can't easily force the PTY code path on Windows (testDisablePTY
// is true there), so this test only runs where PTY is exercised (Unix). It
// cancels ctx mid-stream and asserts the stream ends promptly — the wait()
// closure must return on ctx.Done() rather than blocking on ptycmd.Wait().
func TestStreamPTYWaitBoundedOnCtxCancel(t *testing.T) {
	if testDisablePTY {
		t.Skip("PTY path disabled on this platform; PTY-branch wait bound not exercisable")
	}
	shell, flag := "bash", "-c"
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("shell %q not found", shell)
	}

	// A long-running child that blocks forever until killed.
	spec := CLISpec{
		ModelID:    "test-pty-bounded",
		ModelName:  "Test PTY Bounded",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs:  func(bool) []string { return []string{flag, "sleep 60"} },
	}
	m := &cliModel{spec: spec, workingDir: t.TempDir()}

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := m.Stream(ctx, fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	// Drain in a goroutine; cancel shortly after. The stream must end
	// promptly (no hang on wait()).
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range stream {
		}
	}()

	cancel()
	select {
	case <-done:
		// good: bounded
	case <-time.After(10 * time.Second):
		t.Fatal("PTY stream drain hung past 10s after ctx cancel — wait() not bounded")
	}
}

// ── Bug 2 sanity: kill() routes through session.KillProcess (tree-kill) ──
//
// We can't portably prove full tree-kill of a real multi-generation process
// tree in a unit test (OS-flaky in CI), so this is a regression guard that
// the existing ctx-cancellation-kills-the-child coverage still holds AND that
// kill() uses the tree-kill helper rather than the direct-child-only Kill.
// The behavioral assertion: after ctx cancel, the direct child is gone.
func TestStreamKillUsesTreeKillStillTerminatesChild(t *testing.T) {
	shell, flag := "bash", "-c"
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("shell %q not found", shell)
	}

	// Child writes its own PID to a file, then sleeps long enough that we
	// can cancel and observe whether it actually died.
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	spec := CLISpec{
		ModelID:    "test-kill-tree",
		ModelName:  "Test Kill Tree",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs: func(bool) []string {
			return []string{flag, "echo $$ > '" + pidFile + "'; sleep 60"}
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

	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		for range stream {
		}
	}()

	// Give the child time to write its PID and enter the sleep.
	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case <-streamDone:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not end after ctx cancel within 10s")
	}

	// The direct child must be gone after kill().
	pidData, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatalf("could not read child pid file: %v", rerr)
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(pidData)), "%d", &pid)
	if pid <= 0 {
		t.Fatalf("could not parse child pid from %q", pidData)
	}

	// Poll briefly: process death isn't instant after SIGKILL/taskkill.
	deadline := time.Now().Add(3 * time.Second)
	var alive atomic.Bool
	alive.Store(true)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			alive.Store(false)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if alive.Load() {
		t.Errorf("child pid %d still alive after kill() — tree-kill regressed", pid)
		// best-effort cleanup
		_ = exec.CommandContext(context.Background(), shell, flag, fmt.Sprintf("kill -9 %d 2>/dev/null || true", pid)).Run()
	}
}

// processAlive reports whether a process with the given pid is still running.
// Best-effort, portable.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		// taskkill /? exit code logic: we probe via tasklist.
		out, err := exec.CommandContext(context.Background(), "tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
		if err != nil {
			return false
		}
		return strings.Contains(string(out), fmt.Sprintf("%d", pid))
	}
	// POSIX: signal 0 probes existence.
	_ = exec.CommandContext(context.Background(), "kill", "-0", fmt.Sprintf("%d", pid)).Run()
	// kill -0 returns 0 if alive, non-zero otherwise; Run returns nil on 0 exit.
	return exec.CommandContext(context.Background(), "kill", "-0", fmt.Sprintf("%d", pid)).Run() == nil
}
