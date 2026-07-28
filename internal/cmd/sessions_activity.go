package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// subTreeWalkMaxSessions caps how many sessions the call-tree walk will
// visit, as a defensive guard against pathological fan-out or an
// accidental parent/child cycle in the data. In practice a run has a
// handful of sub-agent sessions; this ceiling only exists so a single
// `sessions why`/`locks` invocation can never turn into an unbounded
// DB sweep.
const subTreeWalkMaxSessions = 512

// callTreeActivity is the ONE underlying signal this fork plumbs through
// every session-monitoring surface: "the most recent real activity
// anywhere in this session's call tree, including inside an in-flight
// sub-agent delegation".
//
// Why it exists: the top-level session's lock-file heartbeat is a fixed
// 10s timer that only proves the orchestrator PROCESS is alive. When that
// orchestrator delegates work through the `agent` tool it blocks waiting
// on the sub-agent call, so the heartbeat keeps ticking whether the
// sub-agent is working hard or has silently hung — the two are
// indistinguishable from the lock alone.
//
// But every sub-agent runs in its OWN child session (parent_session_id =
// this session), and the agent loop persists a message row on that child
// session on every tool-input-start / tool-call / tool-result
// (internal/agent/agent.go). So the DB already holds a fresh,
// sub-agent-aware activity timestamp mid-delegation — it just lives on the
// child rows, which none of the monitoring commands looked at. This helper
// closes that gap in one place: it walks the session's whole descendant
// tree and returns the newest message activity found anywhere in it.
type callTreeActivity struct {
	// LatestUnix is the newest message activity timestamp (max of
	// created_at / updated_at) across the session and every descendant
	// sub-agent session. 0 when the tree has no messages yet.
	LatestUnix int64
	// DeepestSessionID is the descendant session that produced LatestUnix.
	// Equal to the root session ID when the freshest activity is the
	// session's own (no in-flight sub-agent is newer), or names the
	// in-flight sub-agent session when a delegation is currently the most
	// recent activity. "" when the tree has no messages.
	DeepestSessionID string
	// SubAgentActive reports whether the freshest activity came from a
	// DESCENDANT session rather than the root — i.e. a sub-agent
	// delegation is (or very recently was) the live edge of work.
	SubAgentActive bool
	// LatestRole is the role of the message that produced LatestUnix
	// ("assistant" / "tool" / "user"), for a terse "what kind of activity"
	// hint. "" when unknown.
	LatestRole string
}

// Age returns how long ago the freshest activity in the tree happened,
// relative to now. Returns (0, false) when the tree has no activity at all
// (LatestUnix == 0) so callers can distinguish "no messages yet" from
// "active this instant".
func (c callTreeActivity) Age(now time.Time) (time.Duration, bool) {
	if c.LatestUnix == 0 {
		return 0, false
	}
	return now.Sub(time.Unix(c.LatestUnix, 0)), true
}

// computeCallTreeActivity returns the freshest message activity found
// anywhere in rootID's call tree (rootID itself plus every descendant
// sub-agent session reachable via parent_session_id). This is the single
// source of truth consumed by `sessions why` / `locks` / `list` / `show` /
// `watch` / `last` so none of them has to duplicate freshness logic.
//
// Cost: ONE SQL query (a recursive CTE joined against messages), replacing
// the earlier BFS implementation that issued one Messages.List (full
// message history + Parts decode) plus one ListSubSessions per node in the
// tree. The freshest-timestamp aggregation and the "prefer a descendant on
// a tied timestamp" rule (see session.Service.GetCallTreeActivity) now both
// happen inside SQLite via MAX()/ORDER BY, so no message content ever
// crosses into Go just to answer "what's the newest activity". The
// recursion depth is capped at 511 inside the query itself (mirrors
// subTreeWalkMaxSessions) as a defensive guard against a pathological
// fan-out or an accidental parent/child cycle in the data.
//
// Error handling: a query failure is NOT fatal and returns the zero-value
// callTreeActivity — this is a best-effort diagnostic signal consumed by six
// different display surfaces (`sessions why`/`locks`/`list`/`show`/`watch`/
// `last`), all of which already treat a zero-value callTreeActivity as "no
// activity found". Unlike the old per-node BFS, a single aggregate query
// cannot partially fail on just one tree node — it either returns the whole
// tree's answer or it doesn't return at all — so the failure is logged once
// at Debug level rather than per-node.
func computeCallTreeActivity(ctx context.Context, a *app.App, rootID string) callTreeActivity {
	out := callTreeActivity{}
	if a == nil || rootID == "" {
		return out
	}

	act, ok, err := a.Sessions.GetCallTreeActivity(ctx, rootID)
	if err != nil {
		slog.Debug("computeCallTreeActivity: query failed, tree activity is not reflected in the result", "root_id", rootID, "error", err)
		return out
	}
	if !ok {
		return out
	}

	out.LatestUnix = act.LatestUnix
	out.DeepestSessionID = act.SessionID
	out.SubAgentActive = act.SessionID != rootID
	out.LatestRole = act.Role
	return out
}

// computeCallTreeActivityBatch is the batch form of computeCallTreeActivity:
// it computes the freshest call-tree activity for EVERY id in rootIDs in
// ONE SQL query, instead of one query per root. Used by `sessions list`,
// which otherwise walked the whole descendant tree of every running session
// individually. Roots with no activity anywhere in their tree are simply
// absent from the returned map (mirroring the zero-value callTreeActivity a
// per-root call would have produced).
func computeCallTreeActivityBatch(ctx context.Context, a *app.App, rootIDs []string) map[string]callTreeActivity {
	out := make(map[string]callTreeActivity, len(rootIDs))
	if a == nil || len(rootIDs) == 0 {
		return out
	}

	results, err := a.Sessions.GetCallTreeActivityBatch(ctx, rootIDs)
	if err != nil {
		slog.Debug("computeCallTreeActivityBatch: query failed, tree activity is not reflected in the result", "root_count", len(rootIDs), "error", err)
		return out
	}

	for rootID, act := range results {
		out[rootID] = callTreeActivity{
			LatestUnix:       act.LatestUnix,
			DeepestSessionID: act.SessionID,
			SubAgentActive:   act.SessionID != rootID,
			LatestRole:       act.Role,
		}
	}
	return out
}

// latestMessageUnix returns the more recent of a message's created_at and
// updated_at. updated_at is bumped by the update_messages_updated_at DB
// trigger on every Update (which the agent loop calls as tool calls stream
// in), so it is the timestamp that actually moves during an in-flight turn;
// created_at covers freshly inserted tool-result rows.
func latestMessageUnix(m message.Message) int64 {
	ts := m.CreatedAt
	if m.UpdatedAt > ts {
		ts = m.UpdatedAt
	}
	return ts
}

// callTreeActivityFresherThan returns the call-tree activity for rootID only
// when it is strictly newer than the given baseline unix timestamp (e.g. the
// lock file's heartbeat mtime, or the root session's own updated_at). It is
// the convenience the display surfaces use: they already have a coarse
// freshness signal (lock mtime / session.UpdatedAt) and only want to enrich
// it when a sub-agent delegation is producing activity the coarse signal
// can't see. Returns (activity, true) when the tree's freshest activity is
// newer than baselineUnix, else (zero, false).
func callTreeActivityFresherThan(ctx context.Context, a *app.App, rootID string, baselineUnix int64) (callTreeActivity, bool) {
	act := computeCallTreeActivity(ctx, a, rootID)
	if act.LatestUnix > baselineUnix {
		return act, true
	}
	return act, false
}

// subAgentActivityNote renders the one-line "…and here's the sub-agent
// pulse" suffix shared by every surface, or "" when there is no in-flight
// sub-agent whose activity is fresher than the baseline. Keeping the
// wording in ONE place is deliberate: the user asked for a single signal
// reaching every command, not per-command freshness prose.
//
// baselineUnix is the coarse freshness the surface already shows (lock
// mtime or session.UpdatedAt). When a descendant sub-agent session has
// produced newer activity, this returns e.g.
//
//	"sub-agent active: assistant activity 3s ago (session abc12345)"
//
// so an operator watching a long delegation can tell "still working" from
// "stuck": a small, moving age means progress; an age that keeps growing
// past the sub-agent's own stream-idle window means it has likely hung.
func subAgentActivityNote(ctx context.Context, a *app.App, rootID string, baselineUnix int64, now time.Time) string {
	act, fresher := callTreeActivityFresherThan(ctx, a, rootID, baselineUnix)
	if !fresher || !act.SubAgentActive {
		return ""
	}
	age, ok := act.Age(now)
	if !ok {
		return ""
	}
	role := act.LatestRole
	if role == "" {
		role = "sub-agent"
	}
	return fmt.Sprintf(
		"sub-agent active: %s activity %s (session %s)",
		role, formatDurationShort(age), short(session.HashID(act.DeepestSessionID)),
	)
}
