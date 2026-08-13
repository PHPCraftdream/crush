package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// ackObservingSessionService wraps session.Service to capture the exact
// moment AckRunQueueEntry is called, via a buffered channel. This is the
// only race-free way to observe "the durable row was Acked": polling
// ListPendingRunQueueEntries is NOT equivalent — a leased (in-flight) row is
// already absent from the "pending" list, so polling for "queue empty" only
// proves the row was leased, not that it was Acked/deleted. Only a direct
// hook on the Ack call itself proves the ordering this test needs: Ack must
// happen strictly after the continuation's own provider call completes.
type ackObservingSessionService struct {
	session.Service
	acked chan string
}

func (a *ackObservingSessionService) AckRunQueueEntry(ctx context.Context, id, leasedBy string) (string, error) {
	deletedID, err := a.Service.AckRunQueueEntry(ctx, id, leasedBy)
	if err == nil {
		a.acked <- deletedID
	}
	return deletedID, err
}

// TestP0_2_CompactionContinuation_DurableAckAfterContinuationExecutes is the
// end-to-end regression test for P0-2 (docs/reviews/2026-08-13-release-
// readiness-static-audit.md): "post-compaction continuation of a durable
// call is silently lost, and the durable row is Acked as if the whole
// request succeeded."
//
// Bug chain (see agent.go runTurn's shouldSummarize branch, ~line 3241, and
// mailbox.go's submit, ~line 371):
//  1. A durable run-queue row is leased by RunQueuePump, so the call reaching
//     sessionAgent.Run has FromDurableQueue=true.
//  2. Mid-turn, the model's usage crosses the auto-summarize threshold WHILE
//     the assistant message still has pending tool calls (a tool call part
//     recorded on the assistant message — see hasPendingToolCalls in
//     runTurn, which checks len(currentAssistant.ToolCalls()), not whether
//     the call was resolved).
//  3. runTurn runs inline compaction (runSummarizeBody) and then must
//     continue the interrupted work as a new turn.
//  4. Before the fix, that continuation was submitted via mb.submit(call,
//     nil) and the (bool, int) ownership result was discarded. mailbox.submit
//     honors the P0-1 double-execution guard for calls with
//     FromDurableQueue=true by deliberately NOT queuing them in
//     mb.submitted (that guard exists to stop the durable retry path from
//     re-executing a call the live owner is already handling) — but here
//     the "call" being resubmitted IS the live owner's own continuation, so
//     the guard silently swallows it. runTurn then falls through to
//     drainOrReleaseMerged, finds mb.submitted empty, and returns
//     hasNext=false. Run() returns nil (success), and RunQueuePump.
//     executeEntry sees err==nil and Acks (deletes) the durable row. The
//     continuation work is gone: no provider call, no error, no retry path.
//  5. The fix returns the continuation directly as (next, hasNext=true) from
//     runTurn, bypassing mb.submit entirely for this internal handoff, so
//     the SAME Run() call's own turn loop executes it before the loop can
//     exit and before the pump's Ack can fire.
//
// This test drives the real components: a durable session_run_queue row,
// session.NewRunQueuePump leasing and executing it through the production
// Coordinator-shaped adapter (mirrors coordinator.RebuildSessionAgentCall's
// FromDurableQueue=true wiring, same as TestP0_2_RetryExhaustion_QueuesCall
// in p0_2_regression_test.go), and a real sessionAgent.Run/runTurn/
// runSummarizeBody call chain — not a hand-rolled reimplementation of the
// bug. It proves, via a provider-call counter AND a direct Ack-observing
// hook, that the continuation turn actually reaches the provider exactly
// once BEFORE the durable row is Acked.
//
// REVERT CHECK: temporarily restore the pre-fix code in agent.go's
// shouldSummarize branch (discard mb.submit's return value instead of
// returning the continuation directly, i.e. call `mb.submit(call, nil)`
// and fall through to drainOrReleaseMerged instead of `return nil,
// continuationCall, true, nil`), then run:
//
//	go test ./internal/agent -run TestP0_2_CompactionContinuation_DurableAckAfterContinuationExecutes -v
//
// It must FAIL on the "continuation must reach the provider" assertion
// (continuationCalls stays 0 at the moment of Ack). Restore the fix and it
// passes again.
func TestP0_2_CompactionContinuation_DurableAckAfterContinuationExecutes(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	var mainCalls, summarizeCalls, continuationCalls atomic.Int64

	// Main turn: emits a pending tool call AND usage that crosses the
	// auto-summarize threshold in its finishing chunk. This is what makes
	// runTurn take the shouldSummarize branch with hasPendingToolCalls=true
	// — the exact precondition for the bug.
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mainCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"probe_tool","arguments":"{}"}}]},"finish_reason":null}]}`,
			// ContextWindow=1000, smallContextWindowRatio=0.2 -> threshold=200.
			// total=800 -> remaining=200 <= 200 fires shouldSummarize.
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":600,"completion_tokens":200,"total_tokens":800}}`,
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
	t.Cleanup(mainSrv.Close)

	// Summarize (compaction) turn: plain text completion, low usage so it
	// doesn't itself re-trigger shouldSummarize.
	summarizeSrv := summarizeSSEServer(5, 2, &summarizeCalls)
	t.Cleanup(summarizeSrv.Close)

	// Continuation turn: plain text completion that finishes cleanly (no
	// further tool calls), so the turn loop terminates after it and the
	// pump's Ack becomes observable.
	continuationSrv := summarizeSSEServer(5, 2, &continuationCalls)
	t.Cleanup(continuationSrv.Close)

	dispatchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case mainCalls.Load() == 0:
			mainSrv.Config.Handler.ServeHTTP(w, r)
		case summarizeCalls.Load() == 0:
			summarizeSrv.Config.Handler.ServeHTTP(w, r)
		default:
			continuationSrv.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(dispatchSrv.Close)
	lm := languageModelFromServer(t, dispatchSrv)

	// Separate, uncounted server for the small model / title generation, so
	// the background needsTitle call can't steal a slot from the
	// mainCalls/summarizeCalls/continuationCalls routing above (same
	// isolation TestRunTurn_ShouldSummarize_QueuedMessageContinuesWithoutLockDeadlock
	// and TestP312_InlineCompactionDoesNotDeadlock rely on).
	titleSrv := summarizeSSEServer(1, 1, nil)
	t.Cleanup(titleSrv.Close)
	titleLM := languageModelFromServer(t, titleSrv)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 1000, DefaultMaxTokens: 1000},
	}
	smallModel := Model{
		Model:      titleLM,
		CatwalkCfg: catwalk.Model{ContextWindow: 1000, DefaultMaxTokens: 1000},
	}

	ackObserver := &ackObservingSessionService{Service: env.sessions, acked: make(chan string, 4)}

	sa := NewSessionAgent(SessionAgentOptions{
		DataDirectory: dataDir,
		LargeModel:    model,
		SmallModel:    smallModel,
		SystemPrompt:  "you are a probe",
		IsYolo:        true,
		Sessions:      ackObserver,
		Messages:      env.messages,
		Tools:         []fantasy.AgentTool{},
		// DisableAutoSummarize intentionally left false: the whole point of
		// this test is to exercise the real shouldSummarize gate.
	})
	sessionAgent := sa.(*sessionAgent)

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "p0-2-compaction-continuation-test")
	require.NoError(t, err)
	sessionID := sess.ID

	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "do the thing"}},
	})
	require.NoError(t, err)

	// Enqueue a durable run-queue row directly, mirroring what
	// restartOrphanedWithRetry / handleInterruptTick do in production: this
	// is the row the pump will lease and execute with FromDurableQueue=true.
	durableCall := SessionAgentCall{
		SessionID:       sessionID,
		Prompt:          "do the thing",
		MaxOutputTokens: 1000,
	}
	callDataJSON, err := json.Marshal(ToSessionAgentCallData(durableCall))
	require.NoError(t, err)
	err = env.sessions.EnqueueRunQueueEntry(ctx, "p0-2-idem-"+sessionID, sessionID, callDataJSON)
	require.NoError(t, err)

	pending, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "durable row must be queued before the pump starts")

	// mockCoord adapts session.SessionAgentCallData to agent.SessionAgentCall
	// exactly like pumpTestCoordinator in p0_2_regression_test.go: it sets
	// FromDurableQueue=true, mirroring coordinator.RebuildSessionAgentCall in
	// production, so the leased call takes the exact code path the P0-1
	// guard (mailbox.submit) special-cases.
	mockCoord := &pumpTestCoordinator{
		sessionAgent: sessionAgent,
		dataDir:      dataDir,
	}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       ackObserver,
		Coordinator:    mockCoord,
		PumpInstanceID: "test-pump-p0-2-compaction",
		TestTick:       func() time.Duration { return 100 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	// Wait for the precise Ack event (not "queue looks empty" — a leased,
	// still-executing row is already absent from ListPendingRunQueueEntries,
	// so polling that would only prove the row was leased, not Acked).
	var ackedID string
	select {
	case ackedID = <-ackObserver.acked:
	case <-time.After(10 * time.Second):
		t.Fatal("durable row was never Acked within 10s")
	}
	require.NotEmpty(t, ackedID, "AckRunQueueEntry must report the deleted row's ID")

	// Core proof 1: at the exact moment of Ack, the continuation turn had
	// ALREADY reached the provider. Before the fix, continuationCalls is
	// still 0 here — the row is Acked with the continuation work silently
	// dropped, and no later event will ever bring it back to 1.
	require.Equal(t, int64(1), continuationCalls.Load(),
		"post-compaction continuation must execute exactly once, strictly before the durable row is Acked")

	// Core proof 2: no double execution — main turn and summarize each ran
	// exactly once too (guards against a fix that accidentally resubmits
	// through both the direct-return path AND mb.submit).
	require.Equal(t, int64(1), mainCalls.Load(), "main turn must execute exactly once")
	require.Equal(t, int64(1), summarizeCalls.Load(), "compaction must execute exactly once")

	// Core proof 3: compaction actually happened (summary message wired up).
	finalSess, err := env.sessions.Get(ctx, sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, finalSess.SummaryMessageID, "expected inline compaction to have created a summary")

	// Core proof 4: the continuation's own turn produced real, persisted
	// output (not just a provider hit that was then discarded) — the
	// continuation prompt is the interrupted-session marker text produced by
	// runTurn's shouldSummarize branch.
	msgs, err := env.messages.List(ctx, sessionID)
	require.NoError(t, err)
	var foundContinuationUserMsg bool
	for _, m := range msgs {
		if m.Role == message.User {
			for _, part := range m.Parts {
				if tc, ok := part.(message.TextContent); ok &&
					strings.Contains(tc.Text, "previous session was interrupted") &&
					strings.Contains(tc.Text, "do the thing") {
					foundContinuationUserMsg = true
				}
			}
		}
	}
	require.True(t, foundContinuationUserMsg,
		"the continuation's synthesized prompt must be persisted as a real follow-on turn, proving it executed rather than being silently dropped")

	// Core proof 5: only ONE durable row's worth of work ever existed —
	// nothing new got enqueued as a side effect of the continuation (would
	// indicate it went through the external mb.submit/durable-enqueue path
	// instead of being handed back directly, reopening a double-execution
	// hazard against the P0-1 guard), and no second Ack ever fires.
	finalPending, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, finalPending, "no residual/duplicate durable rows should remain")
	select {
	case unexpected := <-ackObserver.acked:
		t.Fatalf("unexpected second Ack for row %q — indicates double execution", unexpected)
	case <-time.After(300 * time.Millisecond):
	}
}
