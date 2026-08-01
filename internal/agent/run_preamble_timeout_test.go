package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingForeverSessionService.Get never returns on its own — it only
// unblocks when its ctx is cancelled. This stands in for a wedged single-
// writer DB connection (internal/db/connect.go's SetMaxOpenConns(1)):
// a.sessions.Get is the very first DB call in Run()'s preamble, before the
// stream watchdog exists.
type blockingForeverSessionService struct {
	session.Service
	entered chan struct{}
}

func (b *blockingForeverSessionService) Get(ctx context.Context, id string) (session.Session, error) {
	close(b.entered)
	<-ctx.Done()
	return session.Session{}, ctx.Err()
}

// TestRun_PreambleWedgedDBCall_BoundedNotInfinite is the regression test for
// task #193 (docs/reviews/2026-07-31-concurrent-subagent-hang.md §6/§193):
// before this fix, Run()'s DB preamble (sessions.Get / getSessionMessages /
// createUserMessage) ran against the caller's raw ctx with no timeout of its
// own, and the stream watchdog — the only mechanism that could otherwise
// bound a stuck turn — is not started until after the preamble completes.
// A wedged single-writer DB connection at that point therefore hung Run()
// forever with zero diagnostics: no cancellation, no log line, nothing.
//
// This test proves Run() now returns (rather than hanging) once
// sessionPreambleMaxDuration elapses, driven by a Get() that only unblocks
// on ctx cancellation — i.e. it can NEVER pass by the fake happening to
// return on its own, only by the preamble's own bounded context firing.
func TestRun_PreambleWedgedDBCall_BoundedNotInfinite(t *testing.T) {
	t.Parallel()

	prevMax := sessionPreambleMaxDuration
	sessionPreambleMaxDuration = 200 * time.Millisecond
	t.Cleanup(func() { sessionPreambleMaxDuration = prevMax })

	dataDir := t.TempDir()
	a := newLockTestSessionAgent(dataDir, false /* isSubAgent */)
	fakeSessions := &blockingForeverSessionService{entered: make(chan struct{})}
	a.sessions = fakeSessions

	runErrCh := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), SessionAgentCall{
			SessionID: "preamble-wedge-test",
			Prompt:    "test prompt",
		})
		runErrCh <- err
	}()

	select {
	case <-fakeSessions.entered:
		// Good: Run reached the wedged Get call.
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Run to reach a.sessions.Get")
	}

	select {
	case runErr := <-runErrCh:
		require.Error(t, runErr, "Run must surface an error once the preamble deadline fires")
		assert.True(t, errors.Is(runErr, context.DeadlineExceeded),
			"expected the preamble timeout to surface as context.DeadlineExceeded, got %v", runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of the DB preamble wedging — sessionPreambleMaxDuration (200ms in this test) failed to bound it, reproducing the unbounded hang task #193 fixes")
	}
}
