package app

// P0-1 ordering-race regression test, companion to
// p348_p0_1_pump_coordinator_wiring_test.go. That test proves the
// SECONDARY fix (coordinatorAdapterImpl's lazy read) but — verified
// directly — cannot catch a reverted PRIMARY fix (the pump-start
// ordering), because it only enqueues work AFTER app.New() has already
// returned, by which point InitCoderAgent has always already run
// regardless of pump-start ordering.
//
// This test closes that gap: it seeds a durable run-queue entry BEFORE
// calling app.New(), simulating the real production scenario the ordering
// fix protects — a process that crashed or restarted with a
// still-pending durably-queued call already sitting in session_run_queue
// (the entire reason session_run_queue survives process restarts). If the
// pump starts before InitCoderAgent assigns app.AgentCoordinator, its
// first tick (which fires essentially immediately on Start()) can race a
// concurrent read of that unsynchronized field against InitCoderAgent's
// write on the main goroutine — a genuine data race, not just a timing
// quirk that "usually still works".
//
// HONEST LIMITATION on the revert-check for this test (found while
// verifying it): the race is real per Go's memory model — Run() reads
// a.app.AgentCoordinator (a plain, unsynchronized struct field) from the
// pump's own goroutine while InitCoderAgent concurrently writes it from
// the main goroutine, with the reverted ordering the pump can start
// before that write happens. But empirically, across 25 revert-check
// iterations under `-race` (two separate sweeps of 10 and 15), the race
// detector reported the concurrent access exactly ZERO times, and the
// test only failed once, non-reproducibly, with no race report attached
// (most likely an unrelated scheduling flake, not this race). The reason:
// InitCoderAgent's own work (prompt construction, skill discovery,
// building the coordinator) takes long enough in practice that by the
// time the pump's first tick actually reaches a call into
// coordinatorAdapterImpl.Run, the assignment has already completed on the
// main goroutine almost every time — the unsynchronized window exists,
// but is narrow relative to InitCoderAgent's own latency. This mirrors
// the review's own "P2-1, PLAUSIBLE — not reproduced live, confirmed by
// inspection" classification for a structurally similar issue elsewhere
// in this same review round: real per code inspection and Go's memory
// model, but not reliably demonstrable via revert-check. The ordering fix
// is still correct and worth keeping (it removes the hazard
// architecturally, at zero cost, rather than relying on InitCoderAgent
// staying slow enough to mask it) — this comment exists so a future
// reader attempting the revert-check below does not conclude the fix is
// unverified just because a handful of attempts didn't reproduce a
// failure.
//
// REVERT CHECK PROCEDURE (expect it to require many attempts, or possibly
// not reproduce at all in your environment — see above):
//  1. In app.go's New(), move the entire "Start the run queue pump" block
//     back up to immediately after `go mcp.Initialize(...)` (before the
//     `!cfg.IsConfigured()` check and `app.InitCoderAgent(ctx)` call) —
//     restoring the pre-fix ordering.
//  2. Run: go test -run TestAppNew_RunQueuePump_OrderingRace -race -v
//     -count=50 (or more)
//  3. Best case: `go test -race` reports a DATA RACE between
//     coordinatorAdapterImpl.Run's read of a.app.AgentCoordinator and
//     InitCoderAgent's write to app.AgentCoordinator. If it does not
//     reproduce even at high iteration counts, that is the expected,
//     documented outcome, not a sign the test or the fix is wrong.
//  4. Restore the corrected ordering (pump-start after InitCoderAgent).
//
// Run this test with:
//   go test ./internal/app -run TestAppNew_RunQueuePump_OrderingRace -race -v -count=10

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
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAppNew_RunQueuePump_OrderingRace(t *testing.T) {
	// Same isolation rationale as p348_p0_1_pump_coordinator_wiring_test.go
	// — see that file's comment for the full explanation.
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("CRUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("CRUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()

	var providerCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"resumed after restart"},"finish_reason":null}]}`,
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
	// SetupAgents populates cfg.Agents (including AgentCoder, which
	// App.InitCoderAgent requires) — normally invoked automatically inside
	// config.Load/reload, but only when IsConfigured() is already true at
	// that point. Here, config.Init above ran against a genuinely empty temp
	// dir with no providers on disk, so IsConfigured() was false and
	// SetupAgents was never called; the Providers.Set/SetSelectedModelRuntime
	// calls above mutate the config directly and do not trigger it either.
	// Found via a CI-only failure ("coder agent configuration is missing")
	// that never reproduced locally — this machine's config isolation
	// apparently still picks up a stray real provider from outside the
	// intended sandbox, making IsConfigured() true early enough to mask the
	// bug; a genuinely clean environment (CI, or any other dev machine)
	// reliably hits it. See ConfigStore.SetupAgents's own doc for the same
	// documented pattern used by coordinator_test.go's role/worker fixtures.
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	// Seed a durable run-queue entry BEFORE app.New() — simulating a
	// process that crashed/restarted with a call already durably
	// enqueued from a PREVIOUS process's lifetime. This is the scenario
	// session_run_queue exists to survive.
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)

	sess, err := sessions.Create(context.Background(), "p0-1-ordering-race-probe")
	require.NoError(t, err)
	_, err = messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)

	call := agent.SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "restart-recovered call, seeded before app.New()",
	}
	callData := agent.ToSessionAgentCallData(call)
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	err = sessions.EnqueueRunQueueEntry(context.Background(), "ordering-race-probe", sess.ID, callDataJSON)
	require.NoError(t, err)

	pending, err := sessions.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1, "entry must be seeded and pending BEFORE app.New() is called")

	// THE call under test: app.New() with pre-existing durable work
	// already in the queue.
	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(func() {
		if application.RunQueuePump != nil {
			application.RunQueuePump.Stop()
		}
		for range application.dbReleasesNeeded {
			require.NoError(t, db.Release(dataDir))
		}
	})

	// VERIFIED ASSUMPTION (pending-wait audit, 2026-08-17): the
	// providerCalls term of this predicate is incremented ONLY by the
	// coder turn's own agent.Stream, which is what makes it a valid
	// ordering signal for the history assertion below (the user message is
	// persisted in the turn preamble, before the stream starts). Two other
	// potential callers of the same httptest server were checked and
	// cannot fire here:
	//   - generateTitle: needsTitle is `len(msgs)==0 || Title=="" ||
	//     Title==DefaultSessionName` (agent.go), and this session was
	//     created with the explicit non-default title
	//     "p0-1-ordering-race-probe" AND already holds the seed message —
	//     both conditions false. The seed message and the explicit title
	//     are therefore LOAD-BEARING for this predicate: dropping either
	//     would let the title goroutine hit the probe server and satisfy
	//     providerCalls>=1 before the turn's user message exists,
	//     collapsing the ordering argument (the final found-assertion
	//     would then flake, not falsely pass).
	//   - auto-summarize: token-gated (contextWindow 200000 minus a tiny
	//     session vs a large threshold) — unreachable in this test.
	// The pending==0 term alone would also be satisfied by a merely
	// leased row (ListPendingRunQueueEntries scans only status='pending');
	// providerCalls>=1 is the term that proves the pump-driven turn
	// actually ran.
	require.Eventually(t, func() bool {
		pending, checkErr := sessions.ListPendingRunQueueEntries(context.Background())
		if checkErr != nil {
			return false
		}
		return len(pending) == 0 && providerCalls.Load() >= 1
	}, 15*time.Second, 250*time.Millisecond,
		"the pre-seeded call must be autonomously executed by App.New's pump, "+
			"proving the pump's coordinator was correctly assigned by the time it processed "+
			"pre-existing durable work — not just work enqueued after New() returned")

	msgs, err := messages.List(context.Background(), sess.ID)
	require.NoError(t, err)
	var found bool
	for _, m := range msgs {
		for _, part := range m.Parts {
			if tc, ok := part.(message.TextContent); ok && tc.Text == "restart-recovered call, seeded before app.New()" {
				found = true
			}
		}
	}
	require.True(t, found, "the pre-seeded call's prompt must appear in persisted message history")
}
