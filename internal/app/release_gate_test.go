package app

// Release Gate Test Suite for Task #343 (Criterion 7)
//
// This file implements the release gate test that proves Shutdown() with a
// non-cooperative agent does NOT close DB under live writer and does NOT
// leave global DB pool mutex occupied forever.
//
// CRITICAL DESIGN RULE:
// This test uses real App initialization and a real blocking Run() to prove
// the end-to-end behavior, not mocks.
//
// Run this test with:
//   go test ./internal/app -run TestReleaseGate_7_ShutdownWithNonCooperativeAgent -v

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// blockingTool is a fantasy.AgentTool whose Run() blocks on a channel that
// the test controls, WITHOUT selecting on ctx.Done(). This deliberately
// simulates the "non-cooperative" class of work.
type blockingTool struct {
	started chan struct{}
	unblock chan struct{}
}

func (b *blockingTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: "blocking_tool"}
}

func (b *blockingTool) Run(ctx context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	close(b.started)
	<-b.unblock // deliberately ignores ctx — the whole point of this test
	return fantasy.NewTextResponse("unblocked"), nil
}

func (b *blockingTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (b *blockingTool) SetProviderOptions(_ fantasy.ProviderOptions) {}

// TestReleaseGate_7_ShutdownWithNonCooperativeAgent proves that Shutdown()
// with a non-cooperative agent does NOT close DB under live writer and does
// NOT leave global DB pool mutex occupied forever.
//
// CRITERION: Shutdown() with non-cooperative agent (real blocked Run()) does NOT
//
//	close DB under live writer and does NOT leave global DB pool mutex busy.
//
// NO EXTERNAL POKE: This test does NOT manually call CancelAll. It relies on
// App.Shutdown() to call CancelAll() which must genuinely detect the blocked Run().
//
// This test uses REAL App.Shutdown() and REAL blocking Run(), but bypasses
// agent.NewCoordinator (which has no seam for custom tools) by using
// agent.NewSessionAgent directly with a thin Coordinator adapter.
//
// REVERT CHECK PROCEDURE:
//  1. In agent.go's Run(), remove the `a.runWg.Add(1)` / `defer a.runWg.Done()`
//     pair at the top of the function.
//     (Reverting CancelAll to the OLD IsBusy()-polling loop instead, as an
//     earlier version of this comment suggested, is NOT a valid revert
//     check for this test: a blocked Run() correctly leaves the mailbox in
//     mbOwned for the whole test, so IsBusy() would ALSO correctly report
//     stillBusy=true — polling and the runWg.Wait() join converge on the
//     same observable result for this specific "genuinely still blocked"
//     scenario. The two differ only in edge cases IsBusy()-polling handled
//     incorrectly — see task #343's own history — not in this test's
//     shape. Only removing the runWg instrumentation itself breaks the
//     join CancelAll relies on.)
//  2. Run: go test -run TestReleaseGate_7_ShutdownWithNonCooperativeAgent -v
//  3. FAIL: CancelAll returns immediately, stillBusy=false, DB gets closed under live writer
//  4. Restore the runWg.Add/Done pair and PASS
func TestReleaseGate_7_ShutdownWithNonCooperativeAgent(t *testing.T) {
	dataDir := t.TempDir()

	// Set up a mock provider that triggers the blocking tool.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"blocking_tool","arguments":"{}"}}]},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	model := agent.Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}

	tool := &blockingTool{
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}

	// Connect to DB.
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)

	// Create SessionAgent directly (bypasses coordinator which can't inject custom tools)
	sa := agent.NewSessionAgent(agent.SessionAgentOptions{
		DataDirectory: dataDir,
		LargeModel:    model,
		SmallModel:    model,
		Sessions:      sessions,
		Messages:      messages,
		Tools:         []fantasy.AgentTool{tool},
		IsYolo:        true,
		SystemPrompt:  "You are a test assistant.",
	})

	// Create a thin Coordinator adapter that delegates to SessionAgent
	coordAdapter := &sessionAgentCoordinatorAdapter{
		sessionAgent: sa,
	}

	// Create a session.
	sess, err := sessions.Create(context.Background(), "release-gate-7")
	require.NoError(t, err)

	// Add a seed message.
	_, err = messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)

	// Start a Run() that will get stuck in the blocking tool.
	runDone := make(chan struct{}, 1)
	go func() {
		defer close(runDone)
		_, _ = coordAdapter.Run(context.Background(), sess.ID, "trigger the blocking tool")
	}()

	// Wait for the tool to actually be invoked.
	select {
	case <-tool.started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking tool never started — test setup is broken, proves nothing")
	}

	// CRITICAL: Call App.Shutdown() - it must detect the blocked Run() via CancelAll().
	// This is NOT a mock - it's the REAL App.Shutdown() path.
	app := &App{
		AgentCoordinator: coordAdapter,
		DB:               func() *sql.DB { return conn },
		dataDir:          dataDir,
		dbReleasesNeeded: 1,
		globalCtx:        context.Background(),
	}

	shutdownStart := time.Now()
	shutdownDone := make(chan struct{})
	go func() {
		app.Shutdown()
		close(shutdownDone)
	}()

	// Verify Shutdown returns within bounded time (not hung forever).
	select {
	case <-shutdownDone:
		shutdownDuration := time.Since(shutdownStart)
		t.Logf("Shutdown completed in %v", shutdownDuration)
	case <-time.After(15 * time.Second):
		t.Fatal("Shutdown did not complete within 15 seconds")
	}

	// CRITICAL VERIFICATION 1: DB was NOT closed because shutdown detected live writer.
	// We verify this by trying to use the connection - it should still work.
	_, err = messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "verify DB still open"}},
	})
	require.NoError(t, err, "DB should still be open after forced shutdown")

	// CRITICAL VERIFICATION 2: DB pool mutex is not occupied.
	// db.Release should complete without deadlock.
	releaseDone := make(chan struct{})
	go func() {
		db.Release(dataDir)
		close(releaseDone)
	}()

	select {
	case <-releaseDone:
		// Success - no deadlock
	case <-time.After(5 * time.Second):
		t.Fatal("db.Release blocked - DB pool mutex still occupied")
	}

	// Unblock the tool so the stuck Run() goroutine can finish and this test can clean up.
	close(tool.unblock)

	// Wait for the Run() goroutine to finish (it should return quickly once unblocked).
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() never returned after unblocking the tool — leaked goroutine")
	}
}

// sessionAgentCoordinatorAdapter implements agent.Coordinator by delegating to agent.SessionAgent.
// This is a thin adapter used only for TestReleaseGate_7 to inject custom tools (blockingTool).
// agent.NewCoordinator has no seam for custom tools, so we use NewSessionAgent directly.
type sessionAgentCoordinatorAdapter struct {
	sessionAgent agent.SessionAgent
}

// Run converts Coordinator.Run signature to SessionAgent.Run signature
func (a *sessionAgentCoordinatorAdapter) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return a.sessionAgent.Run(ctx, agent.SessionAgentCall{
		SessionID:   sessionID,
		Prompt:      prompt,
		Attachments: attachments,
	})
}

// RunWithOverrides - minimal implementation for test compatibility (not used by App.Shutdown())
func (a *sessionAgentCoordinatorAdapter) RunWithOverrides(ctx context.Context, sessionID, prompt string, large, small *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	// For this test, just use the base Run without overrides
	return a.Run(ctx, sessionID, prompt, attachments...)
}

// Cancel - delegate to SessionAgent
func (a *sessionAgentCoordinatorAdapter) Cancel(sessionID string) {
	a.sessionAgent.Cancel(sessionID)
}

// CancelAll - delegate to SessionAgent (CRITICAL for this test)
func (a *sessionAgentCoordinatorAdapter) CancelAll() (stillBusy bool) {
	return a.sessionAgent.CancelAll()
}

// IsSessionBusy - delegate to SessionAgent
func (a *sessionAgentCoordinatorAdapter) IsSessionBusy(sessionID string) bool {
	return a.sessionAgent.IsSessionBusy(sessionID)
}

// IsBusy - delegate to SessionAgent
func (a *sessionAgentCoordinatorAdapter) IsBusy() bool {
	return a.sessionAgent.IsBusy()
}

// QueuedPrompts - delegate to SessionAgent
func (a *sessionAgentCoordinatorAdapter) QueuedPrompts(sessionID string) int {
	return a.sessionAgent.QueuedPrompts(sessionID)
}

// QueuedPromptsList - delegate to SessionAgent
func (a *sessionAgentCoordinatorAdapter) QueuedPromptsList(sessionID string) []string {
	return a.sessionAgent.QueuedPromptsList(sessionID)
}

// ClearQueue - delegate to SessionAgent
func (a *sessionAgentCoordinatorAdapter) ClearQueue(sessionID string) {
	a.sessionAgent.ClearQueue(sessionID)
}

// InterruptAndSend - minimal implementation for test compatibility (not used by App.Shutdown())
func (a *sessionAgentCoordinatorAdapter) InterruptAndSend(ctx context.Context, sessionID, prompt string, large, small *agent.ModelOverride, attachments ...message.Attachment) error {
	// For this test, just return error - not used by App.Shutdown()
	return fmt.Errorf("InterruptAndSend not implemented in test adapter")
}

// InjectMessage - delegate to SessionAgent
func (a *sessionAgentCoordinatorAdapter) InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error) {
	return a.sessionAgent.InjectMessage(ctx, agent.SessionAgentCall{
		SessionID:   sessionID,
		Prompt:      prompt,
		Attachments: attachments,
	})
}

// Summarize - minimal implementation for test compatibility (not used by App.Shutdown())
func (a *sessionAgentCoordinatorAdapter) Summarize(context.Context, string, *agent.SummarizeSnapshot) error {
	// For this test, just return error - not used by App.Shutdown()
	return fmt.Errorf("Summarize not implemented in test adapter")
}

// SummarizeQueued - delegate to SessionAgent
func (a *sessionAgentCoordinatorAdapter) SummarizeQueued(sessionID string) bool {
	return a.sessionAgent.SummarizeQueued(sessionID)
}

// TakeSummarizeQueue - delegate to SessionAgent
func (a *sessionAgentCoordinatorAdapter) TakeSummarizeQueue(sessionID string) (*agent.SummarizeSnapshot, bool) {
	return a.sessionAgent.TakeSummarizeQueue(sessionID)
}

// CancelQueuedSummarize - delegate to SessionAgent
func (a *sessionAgentCoordinatorAdapter) CancelQueuedSummarize(sessionID string) {
	a.sessionAgent.CancelQueuedSummarize(sessionID)
}

// Model - delegate to SessionAgent
func (a *sessionAgentCoordinatorAdapter) Model() agent.Model {
	return a.sessionAgent.Model()
}

// UpdateModels - minimal implementation for test compatibility (not used by App.Shutdown())
func (a *sessionAgentCoordinatorAdapter) UpdateModels(ctx context.Context) error {
	// For this test, just return nil - not used by App.Shutdown()
	return nil
}

// GetSystemPrompt - delegate to SessionAgent
func (a *sessionAgentCoordinatorAdapter) GetSystemPrompt() string {
	return a.sessionAgent.SystemPrompt()
}

// BuildSystemPrompt - minimal implementation for test compatibility (not used by App.Shutdown())
func (a *sessionAgentCoordinatorAdapter) BuildSystemPrompt(ctx context.Context) (string, error) {
	return a.sessionAgent.SystemPrompt(), nil
}

// UpdateSessionSystemPrompt - minimal implementation for test compatibility (not used by App.Shutdown())
func (a *sessionAgentCoordinatorAdapter) UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	// For this test, just return nil - not used by App.Shutdown()
	return nil
}

// SetAgentTimeoutOptions - delegate to SessionAgent
func (a *sessionAgentCoordinatorAdapter) SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
	a.sessionAgent.SetTimeoutOptions(extendsOnProgress, hardCap)
}

// SetRunLimits - minimal implementation for test compatibility (not used by App.Shutdown())
func (a *sessionAgentCoordinatorAdapter) SetRunLimits(maxCost float64, maxTokens int64) {
	// No-op for this test
}

// SetActiveModelRole - minimal implementation for test compatibility (not used by App.Shutdown())
func (a *sessionAgentCoordinatorAdapter) SetActiveModelRole(role config.SelectedModelType) {
	// No-op for this test
}

// SetAllowPeakHours - minimal implementation for test compatibility (not used by App.Shutdown())
func (a *sessionAgentCoordinatorAdapter) SetAllowPeakHours(allow bool) {
	// No-op for this test
}

// SetPersistentMode - minimal implementation for test compatibility (not used by App.Shutdown())
func (a *sessionAgentCoordinatorAdapter) SetPersistentMode(persistent bool) {
	// No-op for this test
}

// ResetAutoResumeCounter - minimal implementation for test compatibility (not used by App.Shutdown())
func (a *sessionAgentCoordinatorAdapter) ResetAutoResumeCounter(sessionID string) {
	// No-op for this test
}

// RebuildSessionAgentCall - minimal implementation for test compatibility (not used by App.Shutdown())
func (a *sessionAgentCoordinatorAdapter) RebuildSessionAgentCall(ctx context.Context, data session.SessionAgentCallData) (agent.SessionAgentCall, error) {
	// For this test, just return an empty call - not used by App.Shutdown()
	return agent.SessionAgentCall{}, fmt.Errorf("RebuildSessionAgentCall not implemented in test adapter")
}

// RunSessionAgentCall - minimal implementation for test compatibility (not used by App.Shutdown())
func (a *sessionAgentCoordinatorAdapter) RunSessionAgentCall(ctx context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	// For this test, delegate to sessionAgent.Run - not used by App.Shutdown()
	return a.sessionAgent.Run(ctx, call)
}
