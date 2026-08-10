package app

// P0-1 regression test (found in the final @oh review of tasks #337-349,
// 2026-08-10): App.New started the RunQueuePump BEFORE InitCoderAgent
// assigned app.AgentCoordinator, and the pump's coordinatorAdapterImpl
// captured app.AgentCoordinator BY VALUE at construction time — so every
// production `crush run`/web-server pump was permanently wired to a nil
// coordinator. The pump silently ran in scan-only mode forever
// (run_queue_pump.go's `if p.cfg.Coordinator == nil { ...; return }`
// branch), meaning ANY call that ever reached the durable run queue
// (detached runs, cross-process interrupt recovery, retry-exhausted
// orphaned calls) was accepted, persisted, and then NEVER executed.
//
// No test anywhere in the codebase called app.New (the real production
// entry point) before this — every other test constructs a bare App{}
// literal or calls agent.NewCoordinator directly, both of which bypass
// this exact wiring bug.
//
// PRIMARY fix: App.New now constructs and starts the RunQueuePump only
// AFTER InitCoderAgent has assigned app.AgentCoordinator (moved from
// before the DB-release cleanupFuncs registration to right before New's
// final return), instead of before it. This also closes a second issue
// found in the same review round: starting the pump earlier raced a
// concurrent pump goroutine reading app.AgentCoordinator (an
// unsynchronized plain struct field) against InitCoderAgent's assignment
// the moment a restart-recovered durable queue already had pending work —
// a genuine data race, not just "eventually gets a working value".
//
// SECONDARY fix (defense-in-depth, not sufficient alone — see below):
// coordinatorAdapterImpl now holds *App and reads app.AgentCoordinator
// lazily on every Run() call instead of capturing it once at construction
// time.
//
// REVERT CHECK PROCEDURE (for the SECONDARY fix — the lazy adapter — which
// is what THIS test actually depends on):
//  1. In app.go's coordinatorAdapterImpl.Run, change `coord :=
//     a.app.AgentCoordinator` to `var coord agent.Coordinator` (hardcode
//     nil, simulating a permanently-captured-nil adapter).
//  2. Run: go test -run TestAppNew_RunQueuePump_ExecutesRealEnqueuedCall -v
//  3. FAIL: require.Eventually times out — every Run() call returns "app:
//     coordinator not ready yet" forever.
//  4. Restore the lazy read and PASS.
//
// IMPORTANT — this test does NOT exercise the PRIMARY (ordering) fix.
// Verified directly: reverting ONLY the ordering (moving the pump-start
// block back to before InitCoderAgent, keeping the lazy adapter) still
// PASSES this test cleanly and repeatedly (3/3 runs) — because this test
// enqueues its work AFTER app.New() has already returned, by which point
// InitCoderAgent has always already run regardless of pump-start
// ordering, and the lazy read masks the reordering entirely for this
// specific scenario. The ordering fix's actual value — avoiding a data
// race against app.AgentCoordinator when a RESTART-RECOVERED queue
// already has pending work at App.New time — needs work seeded BEFORE
// app.New() is called, which this test does not do. See
// TestAppNew_RunQueuePump_OrderingRace in
// p348_p0_1_ordering_race_test.go for that scenario specifically.
//
// Run this test with:
//   go test ./internal/app -run TestAppNew_RunQueuePump_ExecutesRealEnqueuedCall -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestAppNew_RunQueuePump_ExecutesRealEnqueuedCall proves that a call
// durably enqueued into session_run_queue (exactly how restartOrphaned /
// restartOrphanedWithRetry / startDetachedRun in internal/agent/agent.go
// and coordinator.go enqueue real orphaned/detached/cross-process-recovered
// calls) actually gets picked up and EXECUTED by the RunQueuePump that
// App.New starts — end to end, through the real production entry point,
// not through a hand-built App{} literal or a directly-constructed
// coordinator that bypasses New's wiring order entirely.
//
// NO EXTERNAL POKE: this test does not call RunQueuePump.tick() or
// coordinator.Run() manually. It enqueues via session.Service (the same
// call production code makes) and waits for the REAL pump, on its REAL
// production 3-second tick interval (New doesn't expose a TestTick knob —
// deliberately: this test is about proving the wiring uses the actual
// production configuration, not a sped-up test seam), to autonomously
// drain the queue.
func TestAppNew_RunQueuePump_ExecutesRealEnqueuedCall(t *testing.T) {
	// CRITICAL: isolate global config/data resolution before calling
	// app.New() below. Without this, App.New's app.checkForUpdates and
	// mcp.Initialize read the REAL host ~/.config/crush/crush.json and, if
	// it configures MCP servers, try to open real network connections from
	// inside this test — this exact path previously hung a stress run for
	// 9+ minutes elsewhere in this repo (see
	// internal/cmd/providers_test.go's runProvidersCmdInIsolatedApp, whose
	// pattern this mirrors). GlobalConfig() (CRUSH_GLOBAL_CONFIG/
	// XDG_CONFIG_HOME) and GlobalConfigData() (CRUSH_GLOBAL_DATA/
	// XDG_DATA_HOME) are two SEPARATE resolution paths — both must be
	// isolated, in two DIFFERENT subdirectories (not the same tmp dir),
	// per CLAUDE.md's "two real config paths" caveat, or lookupConfigs
	// would load the same file twice under two different env vars.
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("CRUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("CRUSH_GLOBAL_CONFIG", isolationConfigDir)
	// Cache-only so provider discovery makes no network calls.
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()

	var providerCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"hello from the real pump"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
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

	// Real config store, the same construction path `crush run`/the web
	// server use — NOT a bare &config.ConfigStore{} literal.
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID:      "openaicompat",
		Type:    openaicompat.Name,
		BaseURL: srv.URL,
		APIKey:  "probe",
		Models: []catwalk.Model{
			{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeLarge, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmall, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	// THE call under test: the real, unmodified production entry point.
	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(func() {
		if application.RunQueuePump != nil {
			application.RunQueuePump.Stop()
		}
		// New() opens an additional read-only pool on top of the caller's
		// own db.Connect above (see New's own doc comment on
		// dbReleasesNeeded) — one db.Release call per Connect/ConnectRead,
		// or a stale pool-refcount entry poisons t.TempDir()'s own RemoveAll
		// cleanup (observed: "process cannot access the file" on Windows).
		for range application.dbReleasesNeeded {
			require.NoError(t, db.Release(dataDir))
		}
	})

	require.NotNil(t, application.RunQueuePump, "App.New must start a RunQueuePump when dataDir is set")
	require.NotNil(t, application.AgentCoordinator, "InitCoderAgent must have run and assigned a coordinator")

	sess, err := application.Sessions.Create(context.Background(), "p0-1-wiring-probe")
	require.NoError(t, err)

	_, err = application.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)

	// Enqueue exactly the way real orphaned/detached/cross-process-recovery
	// code paths do (agent.go's restartOrphaned/restartOrphanedWithRetry,
	// coordinator.go's startDetachedRun): build a SessionAgentCall, convert
	// via agent.ToSessionAgentCallData, marshal, and call
	// session.Service.EnqueueRunQueueEntry directly — no manual Run()/tick()
	// call anywhere in this test.
	call := agent.SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "prove the real production pump executes this",
	}
	callData := agent.ToSessionAgentCallData(call)
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	idempotencyKey := fmt.Sprintf("%s-p0-1-wiring-probe", sess.ID)
	err = application.Sessions.EnqueueRunQueueEntry(context.Background(), idempotencyKey, sess.ID, callDataJSON)
	require.NoError(t, err)

	pending, err := application.Sessions.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1, "call should be enqueued in the durable run queue")

	// Production tick is 3s (session.RunQueuePumpInterval) — this is
	// intentionally NOT sped up (see doc comment above), so give it
	// several real ticks' worth of headroom.
	require.Eventually(t, func() bool {
		pending, checkErr := application.Sessions.ListPendingRunQueueEntries(context.Background())
		if checkErr != nil {
			return false
		}
		return len(pending) == 0 && providerCalls.Load() >= 1
	}, 15*time.Second, 250*time.Millisecond,
		"the real production RunQueuePump must autonomously execute the enqueued call "+
			"via a real AgentCoordinator — if this times out, the pump's coordinator is nil "+
			"(or otherwise non-functional) and every enqueued call is silently never executed")

	require.GreaterOrEqual(t, providerCalls.Load(), int64(1),
		"provider should have been called by the pump-driven execution")

	msgs, err := application.Messages.List(context.Background(), sess.ID)
	require.NoError(t, err)
	var foundPromptContent bool
	for _, m := range msgs {
		for _, part := range m.Parts {
			if tc, ok := part.(message.TextContent); ok && tc.Text == "prove the real production pump executes this" {
				foundPromptContent = true
			}
		}
	}
	require.True(t, foundPromptContent, "the enqueued call's prompt must appear in persisted message history")
}
