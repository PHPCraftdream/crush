package agent

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/x/vcr"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/joho/godotenv/autoload"
)

func TestMain(m *testing.M) {
	slog.SetLogLoggerLevel(slog.LevelError)

	// Isolate every test in this binary from the real host global config
	// (task #456, following up on task #450's test-speed investigation).
	// Several tests in this package reach config.Init with an empty
	// dataDir (coderAgent in common_test.go, used by e.g. TestCoderAgent
	// and friends, plus config.Init call sites in coordinator_test.go,
	// interrupt_test.go, p0_2_fault_injection_test.go, and others) --
	// without this, that falls through to the real
	// GlobalConfigData()/GlobalConfig() resolution paths: the operator's
	// actual ~/.config/crush/crush.json, complete with real provider API
	// keys and any MCP servers it configures. A test that reaches
	// app-level config resolution can then try to open real network
	// connections to those servers -- internal/cmd/models_use_test.go's
	// isolatedModelsEnv documents observing exactly this hang a test run
	// for 9+ minutes until the panic-timeout. This fork's own CLAUDE.md
	// documents the same hazard and the same fix: CRUSH_GLOBAL_DATA and
	// CRUSH_GLOBAL_CONFIG are two SEPARATE resolution paths (not aliases
	// of each other), and BOTH must be pointed at throwaway directories
	// for genuine isolation. Set process-wide here, once, before any test
	// in this binary can run -- t.Setenv isn't usable outside a *testing.T,
	// and this needs to apply uniformly regardless of which test runs
	// first or whether tests run in parallel.
	globalTmp, err := os.MkdirTemp("", "crush-agent-test-global-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: failed to create isolated global config dir: %v\n", err)
		os.Exit(1)
	}
	dataDir := filepath.Join(globalTmp, "data")
	configDir := filepath.Join(globalTmp, "config")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		os.RemoveAll(globalTmp)
		fmt.Fprintf(os.Stderr, "TestMain: failed to create %s: %v\n", dataDir, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		os.RemoveAll(globalTmp)
		fmt.Fprintf(os.Stderr, "TestMain: failed to create %s: %v\n", configDir, err)
		os.Exit(1)
	}
	os.Setenv("CRUSH_GLOBAL_DATA", dataDir)
	os.Setenv("XDG_DATA_HOME", dataDir)
	os.Setenv("CRUSH_GLOBAL_CONFIG", configDir)
	os.Setenv("XDG_CONFIG_HOME", configDir)

	m.Run()
	os.RemoveAll(globalTmp)
}

var modelPairs = []modelPair{
	{"glm-5.1", hyperBuilder("glm-5.1"), hyperBuilder("gpt-oss-120b")},
}

func getModels(t *testing.T, r *vcr.Recorder, pair modelPair) (fantasy.LanguageModel, fantasy.LanguageModel) {
	large, err := pair.largeModel(t, r)
	require.NoError(t, err)
	small, err := pair.smallModel(t, r)
	require.NoError(t, err)
	return large, small
}

func setupAgent(t *testing.T, pair modelPair) (SessionAgent, fakeEnv) {
	r := vcr.NewRecorder(t)
	large, small := getModels(t, r, pair)
	env := testEnv(t)

	createSimpleGoProject(t, env.workingDir)
	agent, err := coderAgent(r, env, large, small)
	require.NoError(t, err)
	return agent, env
}

func TestCoderAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows for now")
	}
	if testing.Short() {
		// VCR-backed integration test against hyper.charm.land. The cassettes
		// drift and re-recording needs a Charm hyper key, so skip in -short
		// (CI). Runs in full `go test` locally for anyone who can re-record.
		t.Skip("skipping network/VCR integration test in -short mode")
	}

	for _, pair := range modelPairs {
		t.Run(pair.name, func(t *testing.T) {
			t.Run("simple test", func(t *testing.T) {
				agent, env := setupAgent(t, pair)

				session, err := env.sessions.Create(t.Context(), "New Session")
				require.NoError(t, err)

				res, err := agent.Run(t.Context(), SessionAgentCall{
					Prompt:          "Hello",
					SessionID:       session.ID,
					MaxOutputTokens: 10000,
				})
				require.NoError(t, err)
				assert.NotNil(t, res)

				msgs, err := env.messages.List(t.Context(), session.ID)
				require.NoError(t, err)
				// Should have the agent and user message
				assert.Equal(t, len(msgs), 2)
			})
			t.Run("read a file", func(t *testing.T) {
				agent, env := setupAgent(t, pair)

				session, err := env.sessions.Create(t.Context(), "New Session")
				require.NoError(t, err)
				res, err := agent.Run(t.Context(), SessionAgentCall{
					Prompt:          "Read the go mod",
					SessionID:       session.ID,
					MaxOutputTokens: 10000,
				})

				require.NoError(t, err)
				assert.NotNil(t, res)

				msgs, err := env.messages.List(t.Context(), session.ID)
				require.NoError(t, err)
				foundFile := false
				var tcID string
			out:
				for _, msg := range msgs {
					if msg.Role == message.Assistant {
						for _, tc := range msg.ToolCalls() {
							if tc.Name == tools.ViewToolName {
								tcID = tc.ID
							}
						}
					}
					if msg.Role == message.Tool {
						for _, tr := range msg.ToolResults() {
							if tr.ToolCallID == tcID {
								if strings.Contains(tr.Content, "module example.com/testproject") {
									foundFile = true
									break out
								}
							}
						}
					}
				}
				require.True(t, foundFile)
			})
			t.Run("update a file", func(t *testing.T) {
				agent, env := setupAgent(t, pair)

				session, err := env.sessions.Create(t.Context(), "New Session")
				require.NoError(t, err)

				res, err := agent.Run(t.Context(), SessionAgentCall{
					Prompt:          "update the main.go file by changing the print to say hello from crush",
					SessionID:       session.ID,
					MaxOutputTokens: 10000,
				})
				require.NoError(t, err)
				assert.NotNil(t, res)

				msgs, err := env.messages.List(t.Context(), session.ID)
				require.NoError(t, err)

				foundRead := false
				foundWrite := false
				var readTCID, writeTCID string

				for _, msg := range msgs {
					if msg.Role == message.Assistant {
						for _, tc := range msg.ToolCalls() {
							if tc.Name == tools.ViewToolName {
								readTCID = tc.ID
							}
							if tc.Name == tools.EditToolName || tc.Name == tools.WriteToolName {
								writeTCID = tc.ID
							}
						}
					}
					if msg.Role == message.Tool {
						for _, tr := range msg.ToolResults() {
							if tr.ToolCallID == readTCID {
								foundRead = true
							}
							if tr.ToolCallID == writeTCID {
								foundWrite = true
							}
						}
					}
				}

				require.True(t, foundRead, "Expected to find a read operation")
				require.True(t, foundWrite, "Expected to find a write operation")

				mainGoPath := filepath.Join(env.workingDir, "main.go")
				content, err := os.ReadFile(mainGoPath)
				require.NoError(t, err)
				require.Contains(t, strings.ToLower(string(content)), "hello from crush")
			})
			t.Run("bash tool", func(t *testing.T) {
				agent, env := setupAgent(t, pair)

				session, err := env.sessions.Create(t.Context(), "New Session")
				require.NoError(t, err)

				res, err := agent.Run(t.Context(), SessionAgentCall{
					Prompt:          "use bash to create a file named test.txt with content 'hello bash'. do not print its timestamp",
					SessionID:       session.ID,
					MaxOutputTokens: 10000,
				})
				require.NoError(t, err)
				assert.NotNil(t, res)

				msgs, err := env.messages.List(t.Context(), session.ID)
				require.NoError(t, err)

				foundBash := false
				var bashTCID string

				for _, msg := range msgs {
					if msg.Role == message.Assistant {
						for _, tc := range msg.ToolCalls() {
							if tc.Name == tools.BashToolName {
								bashTCID = tc.ID
							}
						}
					}
					if msg.Role == message.Tool {
						for _, tr := range msg.ToolResults() {
							if tr.ToolCallID == bashTCID {
								foundBash = true
							}
						}
					}
				}

				require.True(t, foundBash, "Expected to find a bash operation")

				testFilePath := filepath.Join(env.workingDir, "test.txt")
				content, err := os.ReadFile(testFilePath)
				require.NoError(t, err)
				require.Contains(t, string(content), "hello bash")
			})
			t.Run("download tool", func(t *testing.T) {
				agent, env := setupAgent(t, pair)

				session, err := env.sessions.Create(t.Context(), "New Session")
				require.NoError(t, err)

				res, err := agent.Run(t.Context(), SessionAgentCall{
					Prompt:          "download the file from https://example-files.online-convert.com/document/txt/example.txt and save it as example.txt",
					SessionID:       session.ID,
					MaxOutputTokens: 10000,
				})
				require.NoError(t, err)
				assert.NotNil(t, res)

				msgs, err := env.messages.List(t.Context(), session.ID)
				require.NoError(t, err)

				foundDownload := false
				var downloadTCID string

				for _, msg := range msgs {
					if msg.Role == message.Assistant {
						for _, tc := range msg.ToolCalls() {
							if tc.Name == tools.DownloadToolName {
								downloadTCID = tc.ID
							}
						}
					}
					if msg.Role == message.Tool {
						for _, tr := range msg.ToolResults() {
							if tr.ToolCallID == downloadTCID {
								foundDownload = true
							}
						}
					}
				}

				require.True(t, foundDownload, "Expected to find a download operation")

				examplePath := filepath.Join(env.workingDir, "example.txt")
				_, err = os.Stat(examplePath)
				require.NoError(t, err, "Expected example.txt file to exist")
			})
			t.Run("fetch tool", func(t *testing.T) {
				agent, env := setupAgent(t, pair)

				session, err := env.sessions.Create(t.Context(), "New Session")
				require.NoError(t, err)

				res, err := agent.Run(t.Context(), SessionAgentCall{
					Prompt:          "fetch the content from https://example-files.online-convert.com/website/html/example.html and tell me if it contains the word 'John Doe'",
					SessionID:       session.ID,
					MaxOutputTokens: 10000,
				})
				require.NoError(t, err)
				assert.NotNil(t, res)

				msgs, err := env.messages.List(t.Context(), session.ID)
				require.NoError(t, err)

				foundFetch := false
				var fetchTCID string

				for _, msg := range msgs {
					if msg.Role == message.Assistant {
						for _, tc := range msg.ToolCalls() {
							if tc.Name == tools.FetchToolName {
								fetchTCID = tc.ID
							}
						}
					}
					if msg.Role == message.Tool {
						for _, tr := range msg.ToolResults() {
							if tr.ToolCallID == fetchTCID {
								foundFetch = true
							}
						}
					}
				}

				require.True(t, foundFetch, "Expected to find a fetch operation")
			})
			t.Run("glob tool", func(t *testing.T) {
				agent, env := setupAgent(t, pair)

				session, err := env.sessions.Create(t.Context(), "New Session")
				require.NoError(t, err)

				res, err := agent.Run(t.Context(), SessionAgentCall{
					Prompt:          "use glob to find all .go files in the current directory",
					SessionID:       session.ID,
					MaxOutputTokens: 10000,
				})
				require.NoError(t, err)
				assert.NotNil(t, res)

				msgs, err := env.messages.List(t.Context(), session.ID)
				require.NoError(t, err)

				foundGlob := false
				var globTCID string

				for _, msg := range msgs {
					if msg.Role == message.Assistant {
						for _, tc := range msg.ToolCalls() {
							if tc.Name == tools.GlobToolName {
								globTCID = tc.ID
							}
						}
					}
					if msg.Role == message.Tool {
						for _, tr := range msg.ToolResults() {
							if tr.ToolCallID == globTCID {
								foundGlob = true
								require.Contains(t, tr.Content, "main.go", "Expected glob to find main.go")
							}
						}
					}
				}

				require.True(t, foundGlob, "Expected to find a glob operation")
			})
			t.Run("grep tool", func(t *testing.T) {
				agent, env := setupAgent(t, pair)

				session, err := env.sessions.Create(t.Context(), "New Session")
				require.NoError(t, err)

				res, err := agent.Run(t.Context(), SessionAgentCall{
					Prompt:          "use grep to search for the word 'package' in go files",
					SessionID:       session.ID,
					MaxOutputTokens: 10000,
				})
				require.NoError(t, err)
				assert.NotNil(t, res)

				msgs, err := env.messages.List(t.Context(), session.ID)
				require.NoError(t, err)

				foundGrep := false
				var grepTCID string

				for _, msg := range msgs {
					if msg.Role == message.Assistant {
						for _, tc := range msg.ToolCalls() {
							if tc.Name == tools.GrepToolName {
								grepTCID = tc.ID
							}
						}
					}
					if msg.Role == message.Tool {
						for _, tr := range msg.ToolResults() {
							if tr.ToolCallID == grepTCID {
								foundGrep = true
								require.Contains(t, tr.Content, "main.go", "Expected grep to find main.go")
							}
						}
					}
				}

				require.True(t, foundGrep, "Expected to find a grep operation")
			})
			t.Run("ls tool", func(t *testing.T) {
				agent, env := setupAgent(t, pair)

				session, err := env.sessions.Create(t.Context(), "New Session")
				require.NoError(t, err)

				res, err := agent.Run(t.Context(), SessionAgentCall{
					Prompt:          "use ls to list the files in the current directory",
					SessionID:       session.ID,
					MaxOutputTokens: 10000,
				})
				require.NoError(t, err)
				assert.NotNil(t, res)

				msgs, err := env.messages.List(t.Context(), session.ID)
				require.NoError(t, err)

				foundLS := false
				var lsTCID string

				for _, msg := range msgs {
					if msg.Role == message.Assistant {
						for _, tc := range msg.ToolCalls() {
							if tc.Name == tools.LSToolName {
								lsTCID = tc.ID
							}
						}
					}
					if msg.Role == message.Tool {
						for _, tr := range msg.ToolResults() {
							if tr.ToolCallID == lsTCID {
								foundLS = true
								require.Contains(t, tr.Content, "main.go", "Expected ls to list main.go")
								require.Contains(t, tr.Content, "go.mod", "Expected ls to list go.mod")
							}
						}
					}
				}

				require.True(t, foundLS, "Expected to find an ls operation")
			})
			t.Run("multiedit tool", func(t *testing.T) {
				agent, env := setupAgent(t, pair)

				session, err := env.sessions.Create(t.Context(), "New Session")
				require.NoError(t, err)

				res, err := agent.Run(t.Context(), SessionAgentCall{
					Prompt:          "use multiedit to change 'Hello, World!' to 'Hello, Crush!' and add a comment '// Greeting' above the fmt.Println line in main.go",
					SessionID:       session.ID,
					MaxOutputTokens: 10000,
				})
				require.NoError(t, err)
				assert.NotNil(t, res)

				msgs, err := env.messages.List(t.Context(), session.ID)
				require.NoError(t, err)

				foundMultiEdit := false
				var multiEditTCID string

				for _, msg := range msgs {
					if msg.Role == message.Assistant {
						for _, tc := range msg.ToolCalls() {
							if tc.Name == tools.MultiEditToolName {
								multiEditTCID = tc.ID
							}
						}
					}
					if msg.Role == message.Tool {
						for _, tr := range msg.ToolResults() {
							if tr.ToolCallID == multiEditTCID {
								foundMultiEdit = true
							}
						}
					}
				}

				require.True(t, foundMultiEdit, "Expected to find a multiedit operation")

				mainGoPath := filepath.Join(env.workingDir, "main.go")
				content, err := os.ReadFile(mainGoPath)
				require.NoError(t, err)
				require.Contains(t, string(content), "Hello, Crush!", "Expected file to contain 'Hello, Crush!'")
			})
			t.Run("sourcegraph tool", func(t *testing.T) {
				agent, env := setupAgent(t, pair)

				session, err := env.sessions.Create(t.Context(), "New Session")
				require.NoError(t, err)

				res, err := agent.Run(t.Context(), SessionAgentCall{
					Prompt:          "use sourcegraph to search for 'func main' in Go repositories",
					SessionID:       session.ID,
					MaxOutputTokens: 10000,
				})
				require.NoError(t, err)
				assert.NotNil(t, res)

				msgs, err := env.messages.List(t.Context(), session.ID)
				require.NoError(t, err)

				foundSourcegraph := false
				var sourcegraphTCID string

				for _, msg := range msgs {
					if msg.Role == message.Assistant {
						for _, tc := range msg.ToolCalls() {
							if tc.Name == tools.SourcegraphToolName {
								sourcegraphTCID = tc.ID
							}
						}
					}
					if msg.Role == message.Tool {
						for _, tr := range msg.ToolResults() {
							if tr.ToolCallID == sourcegraphTCID {
								foundSourcegraph = true
							}
						}
					}
				}

				require.True(t, foundSourcegraph, "Expected to find a sourcegraph operation")
			})
			t.Run("write tool", func(t *testing.T) {
				agent, env := setupAgent(t, pair)

				session, err := env.sessions.Create(t.Context(), "New Session")
				require.NoError(t, err)

				res, err := agent.Run(t.Context(), SessionAgentCall{
					Prompt:          "use write to create a new file called config.json with content '{\"name\": \"test\", \"version\": \"1.0.0\"}'",
					SessionID:       session.ID,
					MaxOutputTokens: 10000,
				})
				require.NoError(t, err)
				assert.NotNil(t, res)

				msgs, err := env.messages.List(t.Context(), session.ID)
				require.NoError(t, err)

				foundWrite := false
				var writeTCID string

				for _, msg := range msgs {
					if msg.Role == message.Assistant {
						for _, tc := range msg.ToolCalls() {
							if tc.Name == tools.WriteToolName {
								writeTCID = tc.ID
							}
						}
					}
					if msg.Role == message.Tool {
						for _, tr := range msg.ToolResults() {
							if tr.ToolCallID == writeTCID {
								foundWrite = true
							}
						}
					}
				}

				require.True(t, foundWrite, "Expected to find a write operation")

				configPath := filepath.Join(env.workingDir, "config.json")
				content, err := os.ReadFile(configPath)
				require.NoError(t, err)
				require.Contains(t, string(content), "test", "Expected config.json to contain 'test'")
				require.Contains(t, string(content), "1.0.0", "Expected config.json to contain '1.0.0'")
			})
			t.Run("parallel tool calls", func(t *testing.T) {
				agent, env := setupAgent(t, pair)

				session, err := env.sessions.Create(t.Context(), "New Session")
				require.NoError(t, err)

				res, err := agent.Run(t.Context(), SessionAgentCall{
					Prompt:          "use glob to find all .go files and use ls to list the current directory, it is very important that you run both tool calls in parallel",
					SessionID:       session.ID,
					MaxOutputTokens: 10000,
				})
				require.NoError(t, err)
				assert.NotNil(t, res)

				msgs, err := env.messages.List(t.Context(), session.ID)
				require.NoError(t, err)

				var assistantMsg *message.Message
				var toolMsgs []message.Message

				for _, msg := range msgs {
					if msg.Role == message.Assistant && len(msg.ToolCalls()) > 0 {
						assistantMsg = &msg
					}
					if msg.Role == message.Tool {
						toolMsgs = append(toolMsgs, msg)
					}
				}

				require.NotNil(t, assistantMsg, "Expected to find an assistant message with tool calls")
				require.NotNil(t, toolMsgs, "Expected to find a tool message")

				toolCalls := assistantMsg.ToolCalls()
				require.GreaterOrEqual(t, len(toolCalls), 2, "Expected at least 2 tool calls in parallel")

				foundGlob := false
				foundLS := false
				var globTCID, lsTCID string

				for _, tc := range toolCalls {
					if tc.Name == tools.GlobToolName {
						foundGlob = true
						globTCID = tc.ID
					}
					if tc.Name == tools.LSToolName {
						foundLS = true
						lsTCID = tc.ID
					}
				}

				require.True(t, foundGlob, "Expected to find a glob tool call")
				require.True(t, foundLS, "Expected to find an ls tool call")

				require.GreaterOrEqual(t, len(toolMsgs), 2, "Expected at least 2 tool results in the same message")

				foundGlobResult := false
				foundLSResult := false

				for _, msg := range toolMsgs {
					for _, tr := range msg.ToolResults() {
						if tr.ToolCallID == globTCID {
							foundGlobResult = true
							require.Contains(t, tr.Content, "main.go", "Expected glob result to contain main.go")
							require.False(t, tr.IsError, "Expected glob result to not be an error")
						}
						if tr.ToolCallID == lsTCID {
							foundLSResult = true
							require.Contains(t, tr.Content, "main.go", "Expected ls result to contain main.go")
							require.False(t, tr.IsError, "Expected ls result to not be an error")
						}
					}
				}

				require.True(t, foundGlobResult, "Expected to find glob tool result")
				require.True(t, foundLSResult, "Expected to find ls tool result")
			})
		})
	}
}

func makeTestTodos(n int) []session.Todo {
	todos := make([]session.Todo, n)
	for i := range n {
		todos[i] = session.Todo{
			Status:  session.TodoStatusPending,
			Content: fmt.Sprintf("Task %d: Implement feature with some description that makes it realistic", i),
		}
	}
	return todos
}

func BenchmarkBuildSummaryPrompt(b *testing.B) {
	cases := []struct {
		name     string
		numTodos int
	}{
		{"0todos", 0},
		{"5todos", 5},
		{"10todos", 10},
		{"50todos", 50},
	}

	for _, tc := range cases {
		todos := makeTestTodos(tc.numTodos)

		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = buildSummaryPrompt(todos)
			}
		})
	}
}

// Fork merge note: upstream's TestPreparePrompt_FiltersImageAttachments was
// removed at merge time — it tests the `supportsImages bool` parameter that
// we don't carry on preparePrompt(). Our equivalent scrub lives in
// workaroundProviderMediaLimitations() and is exercised by the higher-level
// streaming tests.

func TestPreparePrompt_OrphanedToolUse(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "test")
	require.NoError(t, err)

	// Create a user message.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		},
	})
	require.NoError(t, err)

	// Create an assistant message with a tool call but no tool result —
	// this simulates a cancelled/interrupted agent tool call.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "let me check"},
			message.ToolCall{
				ID:       "call_orphaned_1",
				Name:     "agent",
				Input:    `{"prompt":"do something"}`,
				Finished: true,
			},
		},
	})
	require.NoError(t, err)

	// Create the next user message (the one that interrupted the tool call).
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Fix #2"},
		},
	})
	require.NoError(t, err)

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)

	history, _ := agent.preparePrompt(msgs, nil)

	// The history must contain a synthetic tool result for the orphaned call.
	found := false
	for _, msg := range history {
		if msg.Role != fantasy.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if tr.ToolCallID == "call_orphaned_1" {
					found = true
					_, isError := tr.Output.(fantasy.ToolResultOutputContentError)
					require.True(t, isError, "orphaned tool result should be an error")
				}
			}
		}
	}
	require.True(t, found, "expected synthetic tool result for orphaned tool call")
}

func TestPreparePrompt_OrphanedToolUseMixed(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "test")
	require.NoError(t, err)

	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		},
	})
	require.NoError(t, err)

	// Assistant with 2 tool calls: one has a result, one is orphaned.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{
				ID:       "call_ok",
				Name:     "view",
				Input:    `{"path":"/foo"}`,
				Finished: true,
			},
			message.ToolCall{
				ID:       "call_orphaned",
				Name:     "agent",
				Input:    `{"prompt":"search"}`,
				Finished: true,
			},
		},
	})
	require.NoError(t, err)

	// Only one tool result — for call_ok.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "call_ok",
				Name:       "view",
				Content:    "file contents",
			},
		},
	})
	require.NoError(t, err)

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)

	history, _ := agent.preparePrompt(msgs, nil)

	// Should have a synthetic result only for the orphaned call.
	var syntheticCount int
	for _, msg := range history {
		if msg.Role != fantasy.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if tr.ToolCallID == "call_orphaned" {
					syntheticCount++
				}
			}
		}
	}
	require.Equal(t, 1, syntheticCount, "expected exactly one synthetic result for the orphaned call")
}

func TestWorkaroundProviderMediaLimitations_TextOnlyModel(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	pngBase64 := base64.StdEncoding.EncodeToString([]byte("fake-png-data"))

	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output: fantasy.ToolResultOutputContentMedia{
						Data:      pngBase64,
						MediaType: "image/png",
					},
				},
			},
		},
	}

	// Non-Anthropic provider, no image support — should replace media with
	// a text placeholder and not create a synthetic user message.
	largeModel := Model{
		ModelCfg: config.SelectedModel{Provider: "openai"},
		CatwalkCfg: catwalk.Model{
			SupportsImages: false,
		},
	}

	result := agent.workaroundProviderMediaLimitations(messages, largeModel)

	// Should produce exactly one message: the tool message with a text
	// placeholder. No synthetic user message with FilePart.
	require.Len(t, result, 1)
	require.Equal(t, fantasy.MessageRoleTool, result[0].Role)

	tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](result[0].Content[0])
	require.True(t, ok)
	_, ok = fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output)
	require.True(t, ok)
}

func TestWorkaroundProviderMediaLimitations_VisionModel(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	pngBase64 := base64.StdEncoding.EncodeToString([]byte("fake-png-data"))

	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output: fantasy.ToolResultOutputContentMedia{
						Data:      pngBase64,
						MediaType: "image/png",
					},
				},
			},
		},
	}

	// Non-Anthropic provider, image support — should create a synthetic
	// user message with FilePart.
	largeModel := Model{
		ModelCfg: config.SelectedModel{Provider: "openai"},
		CatwalkCfg: catwalk.Model{
			SupportsImages: true,
		},
	}

	result := agent.workaroundProviderMediaLimitations(messages, largeModel)

	// Should produce two messages: tool message with placeholder text,
	// and synthetic user message with FilePart.
	require.Len(t, result, 2)
	require.Equal(t, fantasy.MessageRoleTool, result[0].Role)
	require.Equal(t, fantasy.MessageRoleUser, result[1].Role)

	// The tool message should have text placeholder.
	tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](result[0].Content[0])
	require.True(t, ok)
	textOutput, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output)
	require.True(t, ok)
	require.Contains(t, textOutput.Text, "see attached file")

	// The synthetic user message should contain a TextPart and a FilePart.
	require.Len(t, result[1].Content, 2)
	file, ok := fantasy.AsMessagePart[fantasy.FilePart](result[1].Content[1])
	require.True(t, ok)
	require.Equal(t, "image/png", file.MediaType)
}

func TestWorkaroundProviderMediaLimitations_AnthropicProvider(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	pngBase64 := base64.StdEncoding.EncodeToString([]byte("fake-png-data"))

	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output: fantasy.ToolResultOutputContentMedia{
						Data:      pngBase64,
						MediaType: "image/png",
					},
				},
			},
		},
	}

	// Anthropic provider — should return messages unchanged regardless of
	// SupportsImages, since Anthropic handles media in tool results natively.
	largeModel := Model{
		ModelCfg: config.SelectedModel{Provider: string(catwalk.InferenceProviderAnthropic)},
		CatwalkCfg: catwalk.Model{
			SupportsImages: true,
		},
	}

	result := agent.workaroundProviderMediaLimitations(messages, largeModel)
	require.Len(t, result, 1)
	require.Equal(t, fantasy.MessageRoleTool, result[0].Role)

	// The media should still be in the tool result, untouched.
	tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](result[0].Content[0])
	require.True(t, ok)
	media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](tr.Output)
	require.True(t, ok)
	require.Equal(t, "image/png", media.MediaType)
}

func TestProviderRetryLogFields(t *testing.T) {
	t.Run("nil provider error", func(t *testing.T) {
		fields := providerRetryLogFields(nil, 2*time.Second)
		require.Equal(t, []any{"retry_delay", "2s"}, fields)
	})

	t.Run("provider error with title and message", func(t *testing.T) {
		fields := providerRetryLogFields(&fantasy.ProviderError{
			StatusCode: 429,
			Title:      "rate limit",
			Message:    "too many requests",
		}, 1500*time.Millisecond)
		require.Equal(t, []any{
			"retry_delay", "1.5s",
			"status_code", 429,
			"title", "rate limit",
			"message", "too many requests",
		}, fields)
	})

	t.Run("provider error without optional strings", func(t *testing.T) {
		fields := providerRetryLogFields(&fantasy.ProviderError{
			StatusCode: 503,
		}, time.Second)
		require.Equal(t, []any{
			"retry_delay", "1s",
			"status_code", 503,
		}, fields)
	})
}

// TestSanitizeToolInput pins the JSON validation guard that prevents
// malformed tool call arguments from a provider (e.g. truncated or garbled
// JSON) from getting persisted verbatim and bricking the session on replay.
func TestSanitizeToolInput(t *testing.T) {
	t.Run("valid JSON is returned unchanged", func(t *testing.T) {
		input := `{"path":"foo.go","limit":10}`
		out, sanitized := sanitizeToolInput("view", "call_1", input)
		require.Equal(t, input, out)
		require.False(t, sanitized)
	})

	t.Run("malformed JSON is replaced with empty object", func(t *testing.T) {
		out, sanitized := sanitizeToolInput("view", "call_2", `{"path":"foo.go"`)
		require.Equal(t, "{}", out)
		require.True(t, sanitized)
	})

	t.Run("empty string is sanitized", func(t *testing.T) {
		out, sanitized := sanitizeToolInput("view", "call_3", "")
		require.Equal(t, "{}", out)
		require.True(t, sanitized)
	})
}

// TestSetTimeoutOptions verifies that SetTimeoutOptions populates the internal
// fields on the sessionAgent. Fork patch: batch 8.
func TestSetTimeoutOptions(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	require.False(t, agent.timeoutExtendsOnProgress, "default should be false")
	require.Zero(t, agent.timeoutHardCap, "default should be 0")

	sa.SetTimeoutOptions(true, 30*time.Second)

	assert.True(t, agent.timeoutExtendsOnProgress)
	assert.Equal(t, 30*time.Second, agent.timeoutHardCap)
}

// TestWatchdogFinishMessage_HardCapDoesNotBlameProviderOrTool is the
// regression test for task #223: commit 77c1104a fixed the BOOLEAN passed
// to onFire (a hard-cap fire with a tool in flight no longer wrongly
// claimed toolTimeout=true), but until this change agent.go's onFire
// callback still only had two message branches — "Tool timeout" and
// "Stream stalled" — so a genuine --timeout-hard-cap fire (with or without
// a tool in flight) always fell into the "Stream stalled" branch, which
// falsely blames the PROVIDER for silence and cites idleTimeout, a
// completely different configured duration than the one that actually
// fired. This proves watchdogFinishMessage — the pure mapping the runTurn
// AddFinish call now delegates to — gives causeHardCap its own title and
// body that (a) is NOT "Stream stalled", (b) does NOT mention idleTimeout's
// duration or the word "Provider", and (c) DOES cite the actual configured
// hardCap duration that fired.
func TestWatchdogFinishMessage_HardCapDoesNotBlameProviderOrTool(t *testing.T) {
	const toolMaxDuration = 45 * time.Minute
	const hardCap = 20 * time.Minute
	const idleTimeout = 3 * time.Minute
	const provider = "anthropic"

	title, body := watchdogFinishMessage(causeHardCap, toolMaxDuration, hardCap, idleTimeout, provider)

	assert.Equal(t, "Turn timeout", title, "hard-cap fire must get its own title, not reuse Tool timeout/Stream stalled")
	assert.NotEqual(t, streamStalledFinishTitle, title)
	assert.NotContains(t, body, "Provider", "must not blame the provider for a wall-clock ceiling it had nothing to do with")
	assert.NotContains(t, body, provider, "must not name the provider at all — it did not cause this")
	assert.NotContains(t, body, idleTimeout.String(), "must not cite idleTimeout — that's not the duration that fired")
	assert.NotContains(t, body, toolMaxDuration.String(), "must not cite toolMaxDuration — no specific tool caused this")
	assert.Contains(t, body, hardCap.String(), "must cite the actual --timeout-hard-cap duration that fired")
}

// TestWatchdogFinishMessage_AllThreeCausesDistinct proves the three causes
// produce three genuinely different titles, so the resulting finish message
// always names the actual mechanism (tool / turn wall-clock / provider
// idle) that triggered the watchdog, never collapsing two causes into one
// message.
func TestWatchdogFinishMessage_AllThreeCausesDistinct(t *testing.T) {
	const toolMaxDuration = 45 * time.Minute
	const hardCap = 20 * time.Minute
	const idleTimeout = 3 * time.Minute
	const provider = "anthropic"

	toolTitle, toolBody := watchdogFinishMessage(causeToolTimeout, toolMaxDuration, hardCap, idleTimeout, provider)
	hardCapTitle, _ := watchdogFinishMessage(causeHardCap, toolMaxDuration, hardCap, idleTimeout, provider)
	idleTitle, idleBody := watchdogFinishMessage(causeIdleStall, toolMaxDuration, hardCap, idleTimeout, provider)

	assert.Equal(t, "Tool timeout", toolTitle)
	assert.Equal(t, "Turn timeout", hardCapTitle)
	assert.Equal(t, streamStalledFinishTitle, idleTitle)
	assert.NotEqual(t, toolTitle, hardCapTitle)
	assert.NotEqual(t, hardCapTitle, idleTitle)
	assert.NotEqual(t, toolTitle, idleTitle)

	assert.Contains(t, toolBody, toolMaxDuration.String(), "tool-timeout body must cite toolMaxDuration")
	assert.Contains(t, idleBody, idleTimeout.String(), "idle-stall body must cite idleTimeout")
	assert.Contains(t, idleBody, provider, "idle-stall body must name the provider — it genuinely is the cause here")
}

// TestWatchdogFinishMessage_IdleStallTitleMatchesRetryConstant is the
// regression test for task #236: "Stream stalled" used to be hardcoded
// independently in watchdogFinishMessage's causeIdleStall branch AND in
// coordinator.go's streamStalledFinishTitle constant (the value
// shouldRetryTurn's finish-part matching compares against to decide
// whether a stall qualifies for transparent streamStallRetriesDefault
// retry). Two independently-maintained copies of the same string meant a
// future reword of one — e.g. for clarity or i18n — could silently break
// the retry match with no test catching it, since every existing test
// compared watchdogFinishMessage's output against ITS OWN separate
// hardcoded literal rather than against the actual constant coordinator.go
// reads. This test cross-references the two directly: if
// watchdogFinishMessage's idle-stall title and streamStalledFinishTitle
// ever diverge again, this assertion — not a hardcoded string — is what
// catches it.
func TestWatchdogFinishMessage_IdleStallTitleMatchesRetryConstant(t *testing.T) {
	const toolMaxDuration = 45 * time.Minute
	const hardCap = 20 * time.Minute
	const idleTimeout = 3 * time.Minute
	const provider = "anthropic"

	title, _ := watchdogFinishMessage(causeIdleStall, toolMaxDuration, hardCap, idleTimeout, provider)

	assert.Equal(t, streamStalledFinishTitle, title, "watchdogFinishMessage's idle-stall title must match coordinator.go's streamStalledFinishTitle exactly, or transparent stall-retry silently stops matching")
}

// TestWatchdogToolResultMessage_ReflectsRealCause is the regression test for
// task #227 (same root-cause family as task #223's watchdogFinishMessage
// split): the synthetic tool-result content built for the model when a tool
// call is still unfinished at watchdog-fire time used to unconditionally
// read "the provider stream stalled for >idleTimeout" regardless of the
// real cause — so even after 7b48f75a fixed the HUMAN-facing finish
// message, the MODEL was still told the wrong story on a genuine hard-cap
// or tool-timeout fire. This proves watchdogToolResultMessage — what
// agent.go's runTurn now delegates to instead of a hardcoded string — gives
// each cause distinct, correctly-attributed wording:
//   - causeHardCap must NOT mention "provider" or cite idleTimeout; it must
//     cite the actual hardCap duration.
//   - causeToolTimeout must NOT mention "provider" or cite idleTimeout; it
//     must cite the actual toolMaxDuration.
//   - causeIdleStall is the only cause allowed to blame the provider and
//     cite idleTimeout.
func TestWatchdogToolResultMessage_ReflectsRealCause(t *testing.T) {
	const toolMaxDuration = 45 * time.Minute
	const hardCap = 20 * time.Minute
	const idleTimeout = 3 * time.Minute
	const provider = "anthropic"

	t.Run("hard cap does not blame the provider or cite idleTimeout", func(t *testing.T) {
		msg := watchdogToolResultMessage(causeHardCap, toolMaxDuration, hardCap, idleTimeout, provider)
		assert.NotContains(t, msg, provider, "must not name the provider at all — it did not cause this")
		assert.NotContains(t, msg, idleTimeout.String(), "must not cite idleTimeout — that's not the duration that fired")
		assert.Contains(t, msg, hardCap.String(), "must cite the actual --timeout-hard-cap duration that fired")
	})

	t.Run("tool timeout does not blame the provider or cite idleTimeout", func(t *testing.T) {
		msg := watchdogToolResultMessage(causeToolTimeout, toolMaxDuration, hardCap, idleTimeout, provider)
		assert.NotContains(t, msg, provider, "must not name the provider at all — it did not cause this")
		assert.NotContains(t, msg, idleTimeout.String(), "must not cite idleTimeout — that's not the duration that fired")
		assert.Contains(t, msg, toolMaxDuration.String(), "must cite the actual toolMaxDuration that fired")
	})

	t.Run("idle stall is the only cause that blames the provider", func(t *testing.T) {
		msg := watchdogToolResultMessage(causeIdleStall, toolMaxDuration, hardCap, idleTimeout, provider)
		assert.Contains(t, msg, idleTimeout.String(), "idle-stall must cite idleTimeout — the provider genuinely is the cause here")
	})

	t.Run("all three causes produce distinct messages", func(t *testing.T) {
		hardCapMsg := watchdogToolResultMessage(causeHardCap, toolMaxDuration, hardCap, idleTimeout, provider)
		toolMsg := watchdogToolResultMessage(causeToolTimeout, toolMaxDuration, hardCap, idleTimeout, provider)
		idleMsg := watchdogToolResultMessage(causeIdleStall, toolMaxDuration, hardCap, idleTimeout, provider)
		assert.NotEqual(t, hardCapMsg, toolMsg)
		assert.NotEqual(t, toolMsg, idleMsg)
		assert.NotEqual(t, hardCapMsg, idleMsg)
	})
}

// TestEffectiveToolMaxDuration_Default proves the unified cap: with no
// explicit operator override, every run — sub-agent or top-level,
// orchestrating a delegation or not — gets exactly toolExecutionMaxDefault.
// There used to be a separate, larger cap reserved for a top-level run
// orchestrating a worker delegation; that split was removed (see
// toolExecutionMaxDefault's doc) because it produced its own false cutoffs
// — a sub-agent's OWN plain tool call (a slow build/test inside its turn)
// still only got the short cap. One value, no special-casing.
func TestEffectiveToolMaxDuration_Default(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	require.Zero(t, agent.toolMaxDuration, "no explicit operator override by default")

	got := agent.effectiveToolMaxDuration()
	assert.Equal(t, toolExecutionMaxDefault, got,
		"every run must get the same never-freeze backstop by default, regardless of orchestrator/sub-agent status")
}

// TestEffectiveToolMaxDuration_OperatorOverrideWins verifies
// Options.StreamToolTimeoutSeconds (a.toolMaxDuration) always wins over the
// built-in default, in both directions.
func TestEffectiveToolMaxDuration_OperatorOverrideWins(t *testing.T) {
	t.Run("operator override shorter than the default wins", func(t *testing.T) {
		env := testEnv(t)
		sa := testSessionAgent(env, nil, nil, "test prompt")
		agent := sa.(*sessionAgent)
		agent.toolMaxDuration = 5 * time.Minute // shorter than the default

		got := agent.effectiveToolMaxDuration()
		assert.Equal(t, 5*time.Minute, got,
			"an explicit operator override shorter than the default must still win")
	})

	t.Run("operator override longer than the default wins", func(t *testing.T) {
		env := testEnv(t)
		sa := testSessionAgent(env, nil, nil, "test prompt")
		agent := sa.(*sessionAgent)
		agent.toolMaxDuration = 90 * time.Minute // longer than toolExecutionMaxDefault

		got := agent.effectiveToolMaxDuration()
		assert.Equal(t, 90*time.Minute, got,
			"an explicit operator override longer than the default must still win")
	})
}

// TestEffectiveToolCleanupGrace_TopLevelGetsDefault proves a top-level
// (non-sub-agent) session — the one whose watchdog may be waiting on an
// `agent`-tool delegation and needs to give the child's own watchdog a
// chance to fire first — gets toolCleanupGraceDefault with no explicit
// override configured.
func TestEffectiveToolCleanupGrace_TopLevelGetsDefault(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)
	agent.isSubAgent = false

	require.Zero(t, agent.toolCleanupGrace, "no explicit operator override by default")

	got := agent.effectiveToolCleanupGrace()
	assert.Equal(t, toolCleanupGraceDefault, got,
		"a top-level session must get the default grace so a nested child watchdog gets a chance to fire first")
}

// TestEffectiveToolCleanupGrace_SubAgentGetsNoGrace is the task #205
// regression test: it proves the asymmetry that actually closes the
// parent/child cancellation race. Task #200's original fix applied
// toolCleanupGraceDefault symmetrically to BOTH the parent and the child
// sub-agent's own watchdog, which cancels out of the "child fires before
// parent" inequality algebraically and leaves the parent's unconditional
// head start (it starts timing at OnToolCall, before the child's own turn
// has even begun executing) as the sole deciding factor — so the parent
// still always won. A sub-agent can never itself be waiting on a further
// nested `agent`-tool delegation (excluded from workerToolNames for
// sub-agents), so it is always the deepest watchdog in the chain and must
// get NO grace: it should fire at bare toolMaxDuration, strictly before the
// parent's toolMaxDuration+90s.
func TestEffectiveToolCleanupGrace_SubAgentGetsNoGrace(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)
	agent.isSubAgent = true

	require.Zero(t, agent.toolCleanupGrace, "no explicit operator override by default")

	got := agent.effectiveToolCleanupGrace()
	assert.Zero(t, got,
		"a sub-agent's own watchdog must get no grace — it is always the deepest watchdog "+
			"in the chain and must fire at bare toolMaxDuration, strictly before the parent's "+
			"toolMaxDuration+90s, to win the parent/child cancellation race")
}

// TestEffectiveToolCleanupGrace_ExplicitOverrideWinsForSubAgent verifies the
// explicit operator/test override (SessionAgentOptions.ToolCleanupGrace)
// still wins even for a sub-agent that opts back into a non-zero grace —
// the isSubAgent-based default only applies when no override is set.
func TestEffectiveToolCleanupGrace_ExplicitOverrideWinsForSubAgent(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)
	agent.isSubAgent = true
	agent.toolCleanupGrace = 5 * time.Second

	got := agent.effectiveToolCleanupGrace()
	assert.Equal(t, 5*time.Second, got,
		"an explicit override must win even for a sub-agent session")
}

// TestEffectiveToolCleanupGrace_ExplicitOverrideWinsForTopLevel verifies the
// explicit operator/test override also wins for a top-level session,
// consistent with effectiveToolMaxDuration's override precedence.
func TestEffectiveToolCleanupGrace_ExplicitOverrideWinsForTopLevel(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)
	agent.isSubAgent = false
	agent.toolCleanupGrace = 5 * time.Second

	got := agent.effectiveToolCleanupGrace()
	assert.Equal(t, 5*time.Second, got,
		"an explicit override must win even for a top-level session")
}

// TestEffectiveToolCleanupGrace_ChildWinsCancellationRace_EarlyWedge proves
// the race is resolved for the case toolCleanupGraceDefault actually
// guarantees: the child's tool call wedges EARLY — within
// toolCleanupGraceDefault of the parent delegating — e.g. the child hangs
// during its own init/DB preamble, or on close to the first tool it runs.
// `delta` here models pure parent/child watchdog START skew (init + DB
// preamble before the child's watchdog even begins counting), which really
// is sub-second to low seconds in practice — NOT how long the child worked
// before hitting its stuck tool. See the companion test
// _LateWedgeParentStillWinsKnownLimitation for the case this grace does
// NOT cover, and toolCleanupGraceDefault's doc comment in agent.go for the
// full, honest scope (found overstated by an @oh review of this fix; the
// doc and this test were both corrected together rather than leaving a
// passing-but-misleading test behind).
func TestEffectiveToolCleanupGrace_ChildWinsCancellationRace_EarlyWedge(t *testing.T) {
	env := testEnv(t)

	parentSA := testSessionAgent(env, nil, nil, "parent prompt")
	parent := parentSA.(*sessionAgent)
	parent.isSubAgent = false

	childSA := testSessionAgent(env, nil, nil, "child prompt")
	child := childSA.(*sessionAgent)
	child.isSubAgent = true

	const toolMaxDuration = 45 * time.Minute
	parent.toolMaxDuration = toolMaxDuration
	child.toolMaxDuration = toolMaxDuration

	// Realistic parent/child WATCHDOG-START skew: the child's own watchdog
	// starts only once its turn actually begins executing (after init and
	// the DB preamble) — strictly later than the parent's OnToolCall. This
	// models a child whose stuck tool is (at latest) the first one it runs,
	// not one it hits after working productively for a while.
	for _, delta := range []time.Duration{0, time.Second, 10 * time.Second, 60 * time.Second} {
		delegationStart := time.Now()
		childToolStart := delegationStart.Add(delta)

		parentFireAt := delegationStart.Add(parent.effectiveToolMaxDuration() + parent.effectiveToolCleanupGrace())
		childFireAt := childToolStart.Add(child.effectiveToolMaxDuration() + child.effectiveToolCleanupGrace())

		assert.Truef(t, childFireAt.Before(parentFireAt),
			"delta=%s: child must fire (%s) strictly before parent (%s) so the child's own "+
				"watchdog can unwind cleanly before the parent cancels genCtx out from under it",
			delta, childFireAt, parentFireAt)
	}
}

// TestEffectiveToolCleanupGrace_LateWedgeParentStillWinsKnownLimitation
// documents, as a passing (not failing) test, the case
// toolCleanupGraceDefault does NOT cover: a child that works productively
// for a while — its own watchdog resetting on every tool-call boundary,
// same as the parent's — and only wedges LATE, deep into its turn, well
// past toolCleanupGraceDefault after the parent delegated. In that case the
// parent's watchdog (which counts from the original OnToolCall, not from
// the child's last progress) still fires and force-cancels the delegation
// FIRST, exactly like before task #205's fix. This is accepted, not a bug:
// the parent still terminates correctly and task #197 already made the
// cost-transfer step cancel-immune, so the only loss on this path is
// diagnostic quality (the child's own finish part / goroutine dump), not
// correctness. A structural fix would require the child to push progress
// signals that reset the PARENT's watchdog too — not implemented. This test
// exists so a future reader sees an explicit, intentional boundary instead
// of silently discovering it in production.
func TestEffectiveToolCleanupGrace_LateWedgeParentStillWinsKnownLimitation(t *testing.T) {
	env := testEnv(t)

	parentSA := testSessionAgent(env, nil, nil, "parent prompt")
	parent := parentSA.(*sessionAgent)
	parent.isSubAgent = false

	childSA := testSessionAgent(env, nil, nil, "child prompt")
	child := childSA.(*sessionAgent)
	child.isSubAgent = true

	const toolMaxDuration = 45 * time.Minute
	parent.toolMaxDuration = toolMaxDuration
	child.toolMaxDuration = toolMaxDuration

	// The child worked productively for well over toolCleanupGraceDefault
	// (90s) before its own watchdog even started timing the tool call that
	// eventually wedges — e.g. several minutes of real edits/tests before a
	// hung bash command. skew (watchdog-start delay) is realistic and
	// small; workDuration (prior productive work) is what pushes the
	// child's effective fire time past the parent's.
	const skew = 2 * time.Second
	const workDuration = 5 * time.Minute
	delegationStart := time.Now()
	childToolStart := delegationStart.Add(skew).Add(workDuration)

	parentFireAt := delegationStart.Add(parent.effectiveToolMaxDuration() + parent.effectiveToolCleanupGrace())
	childFireAt := childToolStart.Add(child.effectiveToolMaxDuration() + child.effectiveToolCleanupGrace())

	assert.Truef(t, parentFireAt.Before(childFireAt),
		"known limitation: for a late wedge (skew+workDuration=%s, past toolCleanupGraceDefault=%s), "+
			"the parent (%s) still fires before the child (%s) — this is the case toolCleanupGraceDefault "+
			"does not cover, see its doc comment in agent.go",
		skew+workDuration, toolCleanupGraceDefault, parentFireAt, childFireAt)
}

// TestCleanTitle pins the normalisation that decides whether a model's title
// response is usable: a length-truncated but non-empty response yields a
// title; a think-only or whitespace-only response yields "" (a real miss).
func TestCleanTitle(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain", "Tor-socks5 proxy setup", "Tor-socks5 proxy setup"},
		{"newlines collapsed", "Tor-socks5\nproxy\nsetup", "Tor-socks5 proxy setup"},
		{"trims surrounding whitespace", "   Hello world  ", "Hello world"},
		{"strips think block keeps title", "<think>pondering the ask</think>Real Title", "Real Title"},
		{"truncated title still usable", "Implement webtunnel pluggable transp", "Implement webtunnel pluggable transp"},
		{"empty stays empty", "", ""},
		{"think-only is empty", "<think>just thinking, no answer</think>", ""},
		{"orphan think tag stripped", "Title here</think>", "Title here"},
		{"whitespace only is empty", "   \n  \t", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cleanTitle(tc.raw))
		})
	}
}
