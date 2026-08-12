package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCoordinator_InterruptAndSend_IdleSession_ActuallyRuns is the
// end-to-end regression test for release-blocker P0-B (task #293, fixed by
// commit 1410b398).
//
// The bug: InterruptAndSend fell back to QueueMessage(call) whenever
// InterruptAndReplace reported no owner (i.e. the session was idle). That
// is a runnerless queue — with no turn loop alive for the session, nothing
// ever drains it — so the call sat in mailbox's legacy queue forever while
// the caller (and, through it, a web client) had already been told the
// message was accepted. A user pressing "interrupt" on a session that had
// just finished, or landing in the race right after a release, never got
// their message run at all.
//
// This test proves the request is genuinely EXECUTED, not merely accepted:
// it drives the REAL coordinator.InterruptAndSend against a REAL
// sessionAgent (not the mockSessionAgent used by
// TestCoordinator_InterruptAndSend_UsesInterruptAndReplace, which always
// reports hadOwner=true and therefore never reaches the idle branch this
// bug lives in) on a session that has never run anything — genuinely idle,
// so InterruptAndReplace must return false and the fix's startDetachedRun
// path is the only way the call could ever reach the provider. A real
// httptest SSE server stands in for the provider: if the call is only
// queued (pre-fix behavior), no HTTP request ever arrives and the test
// times out waiting for it; with the fix, the request arrives because
// startDetachedRun genuinely calls sessionAgent.Run.
//
// Verified by revert: reverting 1410b398's startDetachedRun call (restoring
// QueueMessage(call)) makes this test time out waiting for the request,
// and the prompt never appears in session history — see the task report
// for the captured failure output.
//
// Since task #340 ROUND 3, startDetachedRun durably enqueues the call rather
// than running it inline, so this test also starts a session.RunQueuePump
// (fast TestTick) — without one, the enqueued call would never be drained
// and the test would time out for an unrelated reason (no pump, not P0-B).
func TestCoordinator_InterruptAndSend_IdleSession_ActuallyRuns(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "P0-B idle interrupt test")
	require.NoError(t, err)

	var calls atomic.Int64
	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	srv := requestStartedSSEServer(&calls, requestStarted, proceed)
	t.Cleanup(srv.Close)

	const providerID = "p0b-probe"
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	titleSrv := singleTurnSSEServer(nil)
	t.Cleanup(titleSrv.Close)
	titleProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(titleSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	agentIface := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{
			Model:      lm,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
			ModelCfg:   config.SelectedModel{Provider: providerID, Model: "probe"},
		},
		SmallModel:           Model{Model: titleLM, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
		DataDirectory:        dataDir,
	})
	sa := agentIface.(*sessionAgent)

	// Precondition: the session has never run anything — genuinely idle, so
	// the mailbox's InterruptAndReplace must report "no owner" (hadOwner ==
	// false), which is the exact branch that used to call the runnerless
	// QueueMessage.
	require.False(t, sa.IsSessionBusy(sess.ID), "precondition: session must be idle for this to exercise the P0-B branch")

	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	// InterruptAndSend's no-explicit-overrides branch resolves session
	// models via resolveSessionModels before enqueuing (P1-4 fix), so cfg
	// needs a genuinely resolvable large+small model — not just a bare
	// provider ID. Point both slots at the same fake SSE server the
	// SessionAgent itself already uses (srv), so resolution succeeds
	// without depending on whatever provider config.Init's own CLI-provider
	// auto-discovery might otherwise pick up from the host machine.
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:      providerID,
		Type:    "openai",
		BaseURL: srv.URL,
		Models: []catwalk.Model{
			{ID: "probe", Name: "Probe", DefaultMaxTokens: 1000},
		},
	})
	cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
		Provider: providerID,
		Model:    "probe",
	}
	cfg.Config().Models[config.SelectedModelTypeSmall] = config.SelectedModel{
		Provider: providerID,
		Model:    "probe",
	}

	coord := &coordinator{
		cfg:          cfg,
		sessions:     env.sessions,
		messages:     env.messages,
		currentAgent: sa,
		modelCache:   csync.NewMap[string, cachedModelPair](),
	}

	// Since task #340 ROUND 3, startDetachedRun durably enqueues the call
	// instead of executing it inline — a session.RunQueuePump is the only
	// thing that drains that queue (mirroring what App.New() wires in
	// production). Without one running here the enqueued call would sit
	// pending forever and the request would never reach the provider.
	pumpCoord := &pumpTestCoordinator{sessionAgent: sa, dataDir: dataDir}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       env.sessions,
		Coordinator:    pumpCoord,
		PumpInstanceID: "test-pump-p0b-idle",
		TestTick:       func() time.Duration { return 100 * time.Millisecond },
	})
	pump.Start()
	t.Cleanup(func() {
		// Pump.Stop() now returns a bool indicating forced shutdown.
		// In test cleanup, we don't need to check it.
		_ = pump.Stop()
	})

	const interruptPrompt = "interrupt on an idle session must actually run, P0-B"
	err = coord.InterruptAndSend(t.Context(), sess.ID, interruptPrompt, nil, nil)
	require.NoError(t, err)

	// THE core assertion: the request must actually reach the provider.
	// Before the fix, InterruptAndSend's idle branch called
	// QueueMessage(call) — a runnerless queue — so no HTTP request would
	// ever be made and this select would time out.
	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatalf("the interrupt-on-idle-session prompt never reached the provider within 10s (calls=%d) — "+
			"P0-B defect: InterruptAndSend queued the call into a runnerless queue instead of starting a run",
			calls.Load())
	}
	close(proceed)

	// Wait for the detached run to actually finish and persist, rather than
	// asserting immediately after the request starts (which would race the
	// detached goroutine's own DB writes).
	require.Eventually(t, func() bool {
		return !sa.IsSessionBusy(sess.ID)
	}, 10*time.Second, 20*time.Millisecond, "the detached run must complete and release the session")

	assert.Equal(t, int64(1), calls.Load(), "expected exactly one provider call from the detached run")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var found bool
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == interruptPrompt {
			found = true
			break
		}
	}
	assert.True(t, found, "the interrupt prompt must have been persisted and actually run via the detached run — "+
		"P0-B: before the fix it was silently queued into a runnerless queue and never executed")
}
