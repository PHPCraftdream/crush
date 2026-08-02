package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests task #214: the heartbeat must tick on ANY activity of the
// agent OR its sub-agent(s) — see withActivityNotify/notifyActivity in
// agent.go, and SessionLock.RecordActivity/the gated heartbeat goroutine in
// internal/session/lock.go (task #213). heartbeatMtimeAdvances below waits
// slightly longer than one real heartbeat tick (session.lockHeartbeatInterval
// is 10s, unexported) to observe the mtime side effect, matching the
// existing convention in internal/session/lock_test.go
// (TestHeartbeat_RecordActivity_TouchesMtimeOnNextTick et al.) since
// SessionLock exposes no other test seam for "was activity recorded".

// heartbeatMtimeAdvances reports whether lk's lock file mtime advances past
// `before` within one heartbeat interval plus slack. Blocks for slightly
// over session.lockHeartbeatInterval (10s) — same real-time cost the
// existing session package tests already pay.
//
// Returns a plain (bool, error) instead of asserting internally: testify's
// require.* calls t.FailNow(), which does runtime.Goexit() — safe only on
// the test's own goroutine. TestWithActivityNotify_ChainPropagatesToAllAncestors
// calls this from three spawned goroutines; if os.Stat failed there, the
// goroutine would die without ever sending on its results channel, and the
// test would hang until the package-level timeout instead of failing
// cleanly with a clear message (task #226). Callers on any goroutine must
// assert on the returned error themselves, on the goroutine that actually
// owns *testing.T assertions (the main test goroutine).
func heartbeatMtimeAdvances(lk *session.SessionLock, before time.Time) (bool, error) {
	time.Sleep(12 * time.Second) // lockHeartbeatInterval (10s, unexported) + slack
	info, err := os.Stat(lk.Path)
	if err != nil {
		return false, err
	}
	return info.ModTime().After(before), nil
}

func mustAcquireLock(t *testing.T, id string) *session.SessionLock {
	t.Helper()
	lk, err := session.TryAcquireSessionLock(t.TempDir(), id)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lk.Release() })
	return lk
}

// --- Pure unit tests of withActivityNotify / notifyActivity -----------------

// TestWithActivityNotify_SingleLevel proves the base case: calling
// notifyActivity on a ctx produced by withActivityNotify(ctx, lk) results in
// lk's next heartbeat tick touching its mtime.
func TestWithActivityNotify_SingleLevel(t *testing.T) {
	t.Parallel()
	lk := mustAcquireLock(t, "single-level")

	info, err := os.Stat(lk.Path)
	require.NoError(t, err)
	before := info.ModTime()

	ctx := withActivityNotify(context.Background(), lk)
	notifyActivity(ctx)

	advanced, err := heartbeatMtimeAdvances(lk, before)
	require.NoError(t, err)
	assert.True(t, advanced,
		"lk's heartbeat must touch mtime after notifyActivity recorded activity on it")
}

// TestWithActivityNotify_ChainPropagatesToAllAncestors is the core proof of
// the "propagates up the whole delegation chain" requirement: a 3-level
// chain (grandparent -> parent -> child), each level composing its own
// notify on top of the ancestor's via withActivityNotify. Calling
// notifyActivity ONCE on the innermost (grandchild) ctx must record activity
// on ALL THREE locks — this is what lets a grandchild sub-agent's progress
// keep a grandparent's heartbeat alive while every level in between is
// blocked inside a delegating tool call producing no activity of its own.
func TestWithActivityNotify_ChainPropagatesToAllAncestors(t *testing.T) {
	t.Parallel()
	lkGrandparent := mustAcquireLock(t, "grandparent")
	lkParent := mustAcquireLock(t, "parent")
	lkChild := mustAcquireLock(t, "child")

	infoGP, err := os.Stat(lkGrandparent.Path)
	require.NoError(t, err)
	beforeGP := infoGP.ModTime()
	infoP, err := os.Stat(lkParent.Path)
	require.NoError(t, err)
	beforeP := infoP.ModTime()
	infoC, err := os.Stat(lkChild.Path)
	require.NoError(t, err)
	beforeC := infoC.ModTime()

	grandparentCtx := withActivityNotify(context.Background(), lkGrandparent)
	parentCtx := withActivityNotify(grandparentCtx, lkParent)
	childCtx := withActivityNotify(parentCtx, lkChild)

	// Single call at the innermost level.
	notifyActivity(childCtx)

	// All three ancestor locks must observe activity from this one call —
	// checked concurrently so the three real-time waits overlap instead of
	// serializing into a 36s test. Each goroutine only computes
	// (ok, err) and sends it back; every testify assertion happens below
	// on the main test goroutine (task #226 — require.*/assert.* must
	// never run on a goroutine other than the test's own).
	results := make(chan struct {
		name string
		ok   bool
		err  error
	}, 3)
	go func() {
		ok, err := heartbeatMtimeAdvances(lkGrandparent, beforeGP)
		results <- struct {
			name string
			ok   bool
			err  error
		}{"grandparent", ok, err}
	}()
	go func() {
		ok, err := heartbeatMtimeAdvances(lkParent, beforeP)
		results <- struct {
			name string
			ok   bool
			err  error
		}{"parent", ok, err}
	}()
	go func() {
		ok, err := heartbeatMtimeAdvances(lkChild, beforeC)
		results <- struct {
			name string
			ok   bool
			err  error
		}{"child", ok, err}
	}()
	for range 3 {
		r := <-results
		require.NoError(t, r.err, "%s: os.Stat failed", r.name)
		assert.True(t, r.ok, "%s lock's heartbeat must have recorded activity from the single grandchild-level notifyActivity call", r.name)
	}
}

// TestWithActivityNotify_NilLockAndBareContextDoNotPanic covers the two
// degenerate inputs this must tolerate: a nil lk (simulating
// a.dataDir == "", i.e. no OS lock configured at all) and calling
// notifyActivity on a ctx that was never passed through withActivityNotify
// (e.g. in tests, or any code path that doesn't run through runTurn).
func TestWithActivityNotify_NilLockAndBareContextDoNotPanic(t *testing.T) {
	t.Parallel()

	assert.NotPanics(t, func() {
		ctx := withActivityNotify(context.Background(), nil)
		notifyActivity(ctx)
	}, "withActivityNotify/notifyActivity must tolerate a nil lk")

	assert.NotPanics(t, func() {
		notifyActivity(context.Background())
	}, "notifyActivity on a ctx with no activityNotifyContextKey value must be a no-op, not a panic")
}

// --- Integration test: sub-agent activity keeps the PARENT's heartbeat alive ---

// activityCountingSSEServer streams a configurable number of single-token
// text-delta chunks (one fantasy OnTextDelta callback each, so bumpActivity
// fires once per chunk) with a real-time delay between each, then finishes
// the turn. Used to make one child turn's stream last comfortably longer
// than one heartbeat interval, producing multiple bumpActivity calls spread
// out in time rather than one instantaneous burst — closer to what a real,
// slowly-progressing sub-agent looks like.
func activityCountingSSEServer(chunkCount int, chunkDelay time.Duration, callCount *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if callCount != nil {
			callCount.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < chunkCount; i++ {
			if i > 0 {
				time.Sleep(chunkDelay)
			}
			w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"x\"},\"finish_reason\":null}]}\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n"))
		if fl != nil {
			fl.Flush()
		}
		w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
}

// newActivityTestChildAgent builds a real *sessionAgent (via NewSessionAgent,
// same constructor production code uses) backed by a fake SSE server, so its
// Run() -> runTurn() genuinely reaches the real bumpActivity call sites (via
// fantasy's OnTextDelta/OnStepFinish callbacks) instead of a hand-rolled
// stand-in. childDataDir is intentionally a SEPARATE temp dir from the
// parent's: a real child sub-agent session id
// (session.CreateAgentToolSessionID's "parent$$toolcall" shape) lives under
// the SAME dataDir as the parent in production, but this test only needs
// the child to reach its own runTurn and fire real bumpActivity calls — it
// does not assert anything about the child's own lock file, only the
// parent's (via ctx propagation), so an isolated dataDir keeps the two
// locks from being confusable in the test's own assertions.
func newActivityTestChildAgent(t *testing.T, env fakeEnv, providerID string, chunkCount int, chunkDelay time.Duration) (SessionAgent, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := activityCountingSSEServer(chunkCount, chunkDelay, &calls)
	t.Cleanup(srv.Close)

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	titleSrv := activityCountingSSEServer(1, 0, nil)
	t.Cleanup(titleSrv.Close)
	titleProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(titleSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		ModelCfg:   config.SelectedModel{Provider: providerID},
	}
	smallModel := Model{
		Model:      titleLM,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		ModelCfg:   config.SelectedModel{Provider: providerID},
	}
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           smallModel,
		SystemPrompt:         "you are a probe sub-agent",
		IsYolo:               true,
		IsSubAgent:           true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
		DataDirectory:        t.TempDir(),
	})
	return a, &calls
}

// TestParentHeartbeat_StaysAliveFromSubAgentActivity is the single most
// important test in task #214: it proves the literal scenario the user
// asked to fix (verbatim directive: "пульс должен идти при любой активности
// агента или под-агента... если их активности нет, то пульс не должен
// идти") — a PARENT session's heartbeat keeps ticking purely because of a
// SUB-AGENT's real progress, during a window where the parent's own stream
// produces no callbacks of its own (the parent is modeled as blocked inside
// a delegating tool call, e.g. the "agent" tool's Execute closure, waiting
// on runSubAgent -> child.Run()).
//
// Faithfulness note: a fully faithful end-to-end test would drive a real
// top-level Run() whose stream's tool-call step invokes the actual
// coordinator "agent" tool (fantasy.NewParallelAgentTool), which in turn
// calls coordinator.runSubAgent. That requires the full buildAgent/provider
// config plumbing coordinator_test.go's TestRunSubAgent tests already use
// runSubAgent directly (via subAgentParams) rather than through a live
// fantasy tool call — this test follows that same, already-established
// pattern instead of inventing a new one: it (a) builds a parent ctx via
// withActivityNotify(ctx, parentLk), exactly as runTurn does at agent.go's
// genCtx construction site, (b) passes that ctx into coordinator.runSubAgent
// with a REAL child *sessionAgent (built via NewSessionAgent, the same
// constructor production code uses) so the child's Run() genuinely reaches
// runTurn and fires bumpActivity from real fantasy stream callbacks
// (OnTextDelta et al.), not a hand-rolled stand-in, and (c) asserts
// parentLk's heartbeat mtime advances as a result. The one simplification
// relative to production is entering via runSubAgent directly instead of
// through a live "agent" tool call from a running parent stream — the ctx
// plumbing from the tool's Execute closure into runSubAgent is a single,
// already-audited passthrough (agent_tool.go line ~66: `return
// c.runSubAgent(ctx, subAgentParams{...})`, no ctx rebuild in between), so
// this test still exercises the real propagation mechanism
// (withActivityNotify/notifyActivity/genCtx) end to end from parent ctx to
// child bumpActivity call.
func TestParentHeartbeat_StaysAliveFromSubAgentActivity(t *testing.T) {
	env := testEnv(t)
	parentDataDir := t.TempDir()

	parentSession, err := env.sessions.Create(t.Context(), "parent session")
	require.NoError(t, err)

	parentLk, err := session.TryAcquireSessionLock(parentDataDir, parentSession.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = parentLk.Release() })

	info, err := os.Stat(parentLk.Path)
	require.NoError(t, err)
	before := info.ModTime()

	// Parent ctx: exactly what runTurn builds at the genCtx construction
	// site (ctx = withActivityNotify(ctx, lk)) before deriving genCtx and
	// handing it to fantasy.Stream — i.e. what a live "agent" tool
	// Execute(ctx) closure would actually receive.
	parentCtx := withActivityNotify(t.Context(), parentLk)

	// Child: streams several text-delta chunks spread out over more than
	// one heartbeat interval (chunkDelay * chunkCount > 10s), so activity
	// is spread across the whole window rather than one instant burst —
	// the parent's heartbeat must still be alive at the end purely from
	// this trickle, with the parent's OWN stream producing nothing at all
	// (there isn't one — this goroutine's the only thing "blocked" is the
	// runSubAgent call below).
	const providerID = "test-provider"
	childAgent, callCount := newActivityTestChildAgent(t, env, providerID, 4, 3*time.Second)

	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})

	resp, err := coord.runSubAgent(parentCtx, subAgentParams{
		Agent:          childAgent,
		SessionID:      parentSession.ID,
		AgentMessageID: "parent-msg-1",
		ToolCallID:     "call-1",
		Prompt:         "do the sub-task",
		SessionTitle:   "Sub-agent session",
	})
	require.NoError(t, err)
	assert.False(t, resp.IsError, "sub-agent run must succeed: %v", resp.Content)
	assert.Equal(t, int64(1), callCount.Load(), "expected exactly one main-turn request to the child's stream")

	advanced, err := heartbeatMtimeAdvances(parentLk, before)
	require.NoError(t, err)
	assert.True(t, advanced,
		"parent lock's heartbeat mtime must advance purely from the sub-agent's activity, propagated via withActivityNotify/notifyActivity through the ctx runSubAgent forwards unmodified into the child's Run()/runTurn()")
}
