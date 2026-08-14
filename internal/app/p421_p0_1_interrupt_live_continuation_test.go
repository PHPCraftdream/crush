package app

// P0-1 regression test (task #421). Found and confirmed by two independent
// reviews (docs/reviews/2026-08-13-release-gate.md's AMENDMENT/F1 and
// docs/reviews/2026-08-13-release-readiness-static-audit.md's §P0-1):
//
// A cross-process interrupt (`crush sessions inject --interrupt`) landing on
// a session a `crush run` process is actively generating for cancels the
// in-flight generation and durably enqueues its replacement
// (handleInterruptTick, internal/agent/coordinator.go), but deliberately
// does NOT hand the running sessionAgent.Run a live mb.replacement — see
// mailbox.go's FromDurableQueue guard. That guard correctly closes a
// double-execution hazard, but it also removes the only in-process
// continuation path: sessionAgent.Run's turn loop simply ends
// (hasNext=false), Coordinator.Run returns, and RunNonInteractive's `done`
// channel fires with a cancellation. The durable run_queue row sits pending
// until the background RunQueuePump's next tick (3s in production) happens
// to fire — a race the process routinely loses, since the rest of
// RunNonInteractive's select fires within milliseconds and the process then
// proceeds straight to Shutdown(), whose RunQueuePump.Stop() only waits for
// ALREADY-IN-FLIGHT workers, never drains new pending rows. The user's
// accepted interrupt silently does not run until some unrelated future
// `crush` invocation happens to tick the same session's row.
//
// FIX: RunNonInteractive's `case result := <-done` branch (internal/app/
// app.go), on detecting a cancellation, calls the new
// RunQueuePump.DrainSessionNow (internal/session/run_queue_pump.go) to
// synchronously lease-and-execute any pending durable entry for this
// session — reusing the exact same lease/renew/watchdog/ack machinery the
// background tick uses (executeEntrySync, extracted from executeEntry) —
// before finalizing the envelope. If something was drained, runErr/
// isCanceled are reset so the envelope reflects the continuation's outcome,
// not the stale cancellation.
//
// This test proves the FULL, real path end to end: a real App (via New, the
// production entry point — not a hand-built App{} literal), a real
// RunQueuePump, a real interrupt-inject row consumed by the real
// interrupt-inject ticker (NOT a hand-invoked handleInterruptTick call), and
// a real second HTTP round-trip to the mock provider for the continuation —
// all inside ONE call to RunNonInteractive, ONE process.
//
// REVERT CHECK PROCEDURE:
//  1. In app.go's `case result := <-done:` branch, delete (or comment out)
//     the `if isCanceled && app.RunQueuePump != nil { ... }` block this task
//     added.
//  2. Run: go test ./internal/app -run TestRunNonInteractive_P0_1_LiveContinuation -v
//  3. FAIL: the test times out waiting for RunNonInteractive to return
//     (bounded at 20s) — without the drain, RunNonInteractive returns almost
//     immediately with the stale cancellation and never makes the second
//     provider call, so callCount never reaches 2 and the require.Eventually
//     equivalent below never resolves the way the assertions expect; more
//     directly, runErr will be non-nil (a cancellation) instead of nil.
//  4. Restore the block and confirm the test passes again.
//
// Run this test with:
//   go test ./internal/app -run TestRunNonInteractive_P0_1_LiveContinuation -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestRunNonInteractive_P0_1_LiveContinuation(t *testing.T) {
	// CRITICAL: isolate global config/data resolution before calling
	// app.New() below — see p348_p0_1_pump_coordinator_wiring_test.go's
	// identical block for the full rationale (both GlobalConfig() and
	// GlobalConfigData() resolution paths must be isolated separately).
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("CRUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("CRUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()

	var callCount atomic.Int64
	var mainTurnSeen atomic.Bool
	firstCallStarted := make(chan struct{})
	var promptsMu sync.Mutex
	var promptsSeen []string

	// The mock provider distinguishes calls purely by REQUEST CONTENT, not
	// call order: large and small both point at this same "probe" model, so
	// auto-title generation (agent.go's generateTitle, prompt prefixed
	// "Generate a concise title for the following content:") fires its own
	// request against this same server, and can race ahead of — or land
	// between — the main turn's own calls. Keying "the first call" on call
	// order alone risks the hanging branch below intercepting a title-gen
	// request instead of the main turn's, which would desynchronize the
	// whole test (confirmed empirically: an earlier, call-order-keyed
	// version of this test passed while asserting practically nothing,
	// because "call #2" turned out to be an unrelated retry of the
	// ORIGINAL prompt, not the interrupt-driven continuation — the mock's
	// response didn't depend on which prompt actually arrived).
	const injectedText = "please answer THIS instead"
	const titleGenMarker = "Generate a concise title for"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)

		body, _ := io.ReadAll(r.Body)
		promptsMu.Lock()
		promptsSeen = append(promptsSeen, string(body))
		promptsMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		write := func(chunks []string) {
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
		}

		switch {
		case strings.Contains(string(body), injectedText):
			// This IS the interrupt-driven durable continuation.
			write([]string{
				`{"id":"c2","object":"chat.completion.chunk","created":2,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"continuation delivered"},"finish_reason":null}]}`,
				`{"id":"c2","object":"chat.completion.chunk","created":2,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
			})
			return
		case strings.Contains(string(body), titleGenMarker):
			// Auto-title generation — an unrelated background call this
			// same "probe" model also serves. Respond immediately so it
			// never hangs the run or gets mistaken for the main turn.
			write([]string{
				`{"id":"cT","object":"chat.completion.chunk","created":9,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"Untitled"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			})
			return
		}

		if mainTurnSeen.CompareAndSwap(false, true) {
			// The main turn's OWN first request — the generation that
			// will be interrupted. Hang until the client cancels: the
			// real signature of a busy, in-flight generation.
			// mailbox.interruptAndReplace's cancel() is what should end
			// this, via the interrupt-inject ticker consuming the
			// injected row below.
			close(firstCallStarted)
			fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"working on it"},"finish_reason":null}]}`)
			if fl != nil {
				fl.Flush()
			}
			<-r.Context().Done()
			return
		}

		// A LATER main-turn-shaped request that does NOT carry the
		// injected text — e.g. a retry of the ORIGINAL prompt. If the
		// interrupt mechanism worked, this branch should never be hit;
		// respond with content that cannot be mistaken for the
		// interrupt-driven continuation so the test can tell the
		// difference either way.
		write([]string{
			`{"id":"cX","object":"chat.completion.chunk","created":3,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"unrelated non-continuation reply"},"finish_reason":null}]}`,
			`{"id":"cX","object":"chat.completion.chunk","created":3,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		})
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
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

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
	require.NotNil(t, application.RunQueuePump, "App.New must start a RunQueuePump when dataDir is set")

	sess, err := application.Sessions.Create(context.Background(), "p421-live-continuation")
	require.NoError(t, err)

	var runErr error
	var output bytes.Buffer
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		runErr = application.RunNonInteractive(context.Background(), &output, "start please", RunOverrides{}, true, RunModeJSON, sess.ID, false)
	}()

	select {
	case <-firstCallStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("first generation never reached the mock provider — test setup is broken, proves nothing")
	}

	// Inject a cross-process interrupt exactly the way `crush sessions
	// inject --interrupt` does: create the referenced message row first
	// (so it's immediately visible in the web UI / message history), then
	// the pending_injects signal row the interrupt ticker polls for.
	injMsg, err := application.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: injectedText}},
	})
	require.NoError(t, err)
	require.NoError(t, application.Sessions.CreatePendingInject(context.Background(), session.PendingInject{
		SessionID: sess.ID,
		MessageID: injMsg.ID,
		Content:   injectedText,
		Interrupt: true,
	}))

	// Bounded well past the real 3s interrupt-inject ticker interval
	// (interruptInjectTick, internal/agent/coordinator.go) plus the
	// continuation's own round-trip.
	select {
	case <-runDone:
	case <-time.After(20 * time.Second):
		t.Fatal("RunNonInteractive did not return within 20s — the durable continuation was never drained in this process (P0-1 regression)")
	}

	require.NoError(t, runErr, "RunNonInteractive must report the drained continuation's success, not the stale interrupt-induced cancellation: %v", runErr)
	require.GreaterOrEqual(t, callCount.Load(), int64(2), "the continuation must have made its own, second provider call in this same process")

	var envelope struct {
		FinalText  string `json:"final_text"`
		ExitReason string `json:"exit_reason"`
	}
	require.NoError(t, json.Unmarshal(output.Bytes(), &envelope), "output must be a single JSON envelope")
	require.Equal(t, "end_turn", envelope.ExitReason)
	require.Contains(t, envelope.FinalText, "continuation delivered",
		"the envelope must reflect the DRAINED continuation's output, not the cancelled first generation's")

	promptsMu.Lock()
	defer promptsMu.Unlock()
	require.GreaterOrEqual(t, len(promptsSeen), 2)
	// Positional indexing (promptsSeen[1]) isn't reliable here: an
	// auto-title-generation call, or a retry of the ORIGINAL prompt, can
	// also land as "the second call" and would be a false positive if the
	// mock server's response didn't already key off the request content
	// itself (see the handler above) rather than call order.
	var found bool
	for _, p := range promptsSeen[1:] {
		if strings.Contains(p, injectedText) {
			found = true
			break
		}
	}
	require.True(t, found, "the continuation's own provider request must carry the injected message's content; saw %d calls after the first", len(promptsSeen)-1)
}
