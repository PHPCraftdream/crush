package session_test

// Shared wait-helper for the durable run queue's pending-wait audit
// (branch fix/pending-wait-audit).
//
// ListPendingRunQueueEntries only scans status='pending'. A row the pump has
// leased and is executing RIGHT NOW — status='leased' — is invisible to it,
// so a wait of the form "poll until pending is empty" returns in two
// indistinguishable situations: (a) the row is terminally gone (Acked /
// terminal-failed), or (b) the run is merely in flight. Anything asserted
// right after such a wait (call counters, concurrency flags, message
// history) is therefore unordered relative to the run's remaining work.
//
// runQueueGoneEverywhere is the status-agnostic replacement: the row counts
// as "not done" while it is pending OR leased. AckRunQueueEntry and
// TerminalFailRunQueueEntry are only ever called AFTER the coordinator's Run
// has returned (run_queue_pump.go executeEntrySync's outcome branches), so
// "gone from both states" is the earliest observation point that is ordered
// after every write the run makes. This mirrors entryQueuedAnywhere in
// internal/agent/p482_run_queue_terminal_probe_test.go (commit c4c2f17c),
// which fixed the same defect for TestReleaseGate_9_DoubleFailureNoDuplicate;
// it lives here because the session package's tests cannot reach the agent
// package's helper.

import (
	"context"
	"math"

	"github.com/charmbracelet/crush/internal/session"
)

// runQueueGoneEverywhere reports whether the run queue holds no rows at all
// for this database — neither pending nor leased. Intended for tests that
// enqueue a known set of entries and must wait until every one of them has
// reached a terminal outcome (Ack/delete or terminal-fail/delete).
func runQueueGoneEverywhere(ctx context.Context, svc session.Service) (bool, error) {
	pending, err := svc.ListPendingRunQueueEntries(ctx)
	if err != nil {
		return false, err
	}
	if len(pending) > 0 {
		return false, nil
	}
	// math.MaxInt64 as the staleness cutoff makes this "every leased
	// entry", not just the stale ones.
	leased, err := svc.ListStaleLeasedRunQueueEntries(ctx, math.MaxInt64)
	if err != nil {
		return false, err
	}
	return len(leased) == 0, nil
}
