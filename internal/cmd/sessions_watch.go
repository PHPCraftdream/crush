package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsWatchCmd = &cobra.Command{
	Use:   "watch [session-id]",
	Short: "Pick a session (or take one by id) and live-tail it until it ends",
	Long: `One-stop "open a live view of a session" command.

Without arguments: shows an interactive picker (arrow keys, Enter to
select) and then drops into live-tail of the chosen session. The picker
shows the 15 most recently active sessions and a "(+N not shown)"
footer when there are older ones — use "crush sessions list" to see
every session.

With a <session-id> argument: skips the picker and live-tails that
session directly. Short hashes (the HASH column of "sessions list")
are accepted.

Live-tail prints every existing message in the session, then polls
every --interval (default 1s) for new messages and prints them as they
arrive. The loop exits automatically when the session ends — detected
via any of:
  (a) the session row has an ended_reason
  (b) the lock file disappears (process exited / was killed)
  (c) the latest assistant message has a non-partial Finish part

On exit a summary block is printed: id, title, end reason, duration,
tokens (prompt + completion) and cost (with budget if one was set).

Ctrl+C interrupts and prints "(interrupted — session still running)"
without a summary so you don't mistake "I stopped watching" for
"the session ended".`,
	Example: `
# Pick a session interactively and live-tail it
crush sessions watch

# Live-tail a specific session (full id or short hash)
crush sessions watch abc123

# Faster polling for snappier output
crush sessions watch --interval 500ms
  `,
	Args: cobra.MaximumNArgs(1),
	RunE: sessionsWatchCmdRun,
}

func sessionsWatchCmdRun(cmd *cobra.Command, args []string) error {
	interval, _ := cmd.Flags().GetDuration("interval")

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	// Resolve the data directory from the already-booted app's config
	// (honors --data-dir / a configured data_directory), not a raw
	// cwd-based guess — same fix as `sessions locks` (task #231). See
	// task #233. Threaded through as dataDir (not a pre-joined locksDir)
	// so isSessionFinished can hand it straight to
	// session.InspectSessionLock, which derives its own locks/session-*.lock
	// path — see isSessionFinished's doc comment.
	dataDir := a.Config().Options.DataDirectory
	ctx := cmd.Context()

	var sessionID string
	if len(args) == 1 {
		sess, err := resolveSessionID(ctx, a.Sessions, args[0])
		if err != nil {
			return err
		}
		sessionID = sess.ID
	} else {
		picked, err := pickSessionForWatch(ctx, a)
		if err != nil {
			return err
		}
		if picked == "" {
			return nil
		}
		sessionID = picked
	}

	return liveTailSession(ctx, a, sessionID, dataDir, interval)
}

func init() {
	sessionsWatchCmd.Flags().Duration("interval", time.Second, "Poll interval for new messages (e.g. 1s, 500ms, 2s)")
}

// liveTailSession prints every existing message in a session and then
// polls for new messages until the session ends. See the command Long
// description for the end-detection signals. On exit it prints a final
// summary block (duration, cost, tokens, ended_reason).
func liveTailSession(ctx context.Context, a *app.App, sessionID, dataDir string, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}

	sess, err := a.Sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session %s: %w", sessionID, err)
	}

	fmt.Fprintf(os.Stderr, "watching session %s (%s)\n", truncate(sess.ID, 12), truncate(sess.Title, 60))
	fmt.Fprintln(os.Stderr, "(Ctrl+C to exit early)")
	fmt.Fprintln(os.Stderr)

	messages, err := a.Messages.List(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	// Origin context lets ToolResult renderings show the file_path /
	// url / pattern the call was about. Rebuilt every tick because new
	// ToolCalls arrive over the polling loop.
	callCtx := buildToolCallContext(messages)

	now := time.Now()
	lastPrinted := ""
	for i, msg := range messages {
		printMessageWithTime(os.Stdout, msg, "text", now, callCtx, i < len(messages)-1)
		lastPrinted = msg.ID
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Per-session "last message printed" cursor for every in-flight
	// sub-agent delegation this watch has followed so far, keyed by child
	// session id. Lets printNewMessagesSince (below) resume each child
	// stream independently across ticks, the same way lastPrinted does for
	// the top-level session.
	subAgentLastPrinted := map[string]string{}

	// Resume-race guard state — see decideWatchExit. watchStart anchors the
	// grace window; sawLiveLock records whether this watch has ever seen the
	// session's lock heartbeating (after which end signals are trusted with
	// no delay); warnedWaiting keeps the "holding on" notice to one line.
	watchStart := time.Now()
	sawLiveLock := false
	warnedWaiting := false

	for {
		// Ctrl+C wins over everything. fang wraps the root command's
		// context with signal.NotifyContext(os.Interrupt), so a single
		// Ctrl+C cancels ctx. Check it at the very top of every iteration
		// — BEFORE the isSessionFinished I/O below — so an interrupt that
		// lands mid-tick (or while the loop is about to do a DB read on a
		// now-cancelled ctx) always exits promptly with the interrupted
		// message, never a spurious "database error: context canceled" or
		// a false end-of-session summary.
		if watchInterrupted(ctx) {
			return nil
		}

		// Check for end first so we print a summary even when there are
		// no new messages to emit on this tick.
		st, reason := isSessionFinished(ctx, a, sessionID, dataDir)
		if st.lockAlive {
			sawLiveLock = true
		}
		tickAt := time.Now()
		lastActivityAge := time.Duration(0)
		if st.lastActivity > 0 {
			lastActivityAge = tickAt.Sub(time.Unix(st.lastActivity, 0))
		}
		switch decideWatchExit(st, sawLiveLock, tickAt.Sub(watchStart), lastActivityAge) {
		case watchExit:
			printWatchSummary(os.Stderr, ctx, a, sessionID, reason)
			return nil
		case watchWaitForStart:
			if !warnedWaiting {
				fmt.Fprintf(os.Stderr,
					"(session looks finished but no live lock yet — waiting up to %s in case a run is starting up)\n",
					formatDurationShort(watchStartGrace))
				warnedWaiting = true
			}
		case watchKeepWatching:
		}

		select {
		case <-ctx.Done():
			printWatchInterrupted(os.Stderr)
			return nil
		case <-ticker.C:
		}

		msgs, err := a.Messages.List(ctx, sessionID)
		if err != nil {
			// A cancelled context surfaces here as context.Canceled when
			// the interrupt raced the ticker branch of the select above
			// (both channels ready → Go picks pseudo-randomly). Treat it
			// as the interrupt it really is, not a database failure.
			if ctx.Err() != nil {
				printWatchInterrupted(os.Stderr)
				return nil
			}
			return fmt.Errorf("database error: %w", err)
		}
		callCtx = buildToolCallContext(msgs)
		tickNow := time.Now()
		var anchor *message.Message
		if lastPrinted != "" {
			anchor = findByID(msgs, lastPrinted)
		}
		for i := range msgs {
			if msgs[i].ID == lastPrinted {
				continue
			}
			if lastPrinted == "" || isAfter(&msgs[i], anchor) {
				printMessageWithTime(os.Stdout, msgs[i], "text", tickNow, callCtx, i < len(msgs)-1)
				lastPrinted = msgs[i].ID
			}
		}

		// Live-tail whichever descendant session is currently the freshest
		// edge of work — i.e. an in-flight `agent` delegation — the SAME way
		// the top-level session's own messages are shown above: real tool
		// calls/results as they land, not a synthetic timer-driven pulse.
		//
		// This replaces the old "sub-agent active: activity Ns ago" line,
		// which ticked (and printed) on every poll interval regardless of
		// whether the sub-agent had actually done anything — the embedded
		// age string changed every second, so the "only when the note
		// changed" throttle never actually throttled anything. Printing
		// real messages instead is naturally event-driven: nothing new in
		// the child session means nothing is printed, tool-call-quiet
		// periods produce no output at all, matching how the top-level
		// stream already behaves.
		if act := computeCallTreeActivity(ctx, a, sessionID); act.SubAgentActive && act.DeepestSessionID != "" {
			printNewMessagesSince(subAgentWriter(os.Stdout, act.DeepestSessionID), ctx, a, act.DeepestSessionID, subAgentLastPrinted, tickNow)
		}
	}
}

// printNewMessagesSince prints every message in sessionID newer than
// lastPrinted[sessionID] to w, using the same rendering
// (printMessageWithTime) the top-level live-tail stream uses. When the
// session has never been seen before (no entry in lastPrinted yet), its
// ENTIRE history so far is printed — mirroring the backfill
// liveTailSession does for the root session before entering its poll loop
// — so a delegation already in progress when watch starts (or first
// notices it) is shown from the beginning, not just from this tick
// forward. Updates lastPrinted in place and reports whether anything was
// printed.
func printNewMessagesSince(w io.Writer, ctx context.Context, a *app.App, sessionID string, lastPrinted map[string]string, now time.Time) bool {
	msgs, err := a.Messages.List(ctx, sessionID)
	if err != nil || len(msgs) == 0 {
		return false
	}
	callCtx := buildToolCallContext(msgs)
	last := lastPrinted[sessionID]
	var anchor *message.Message
	if last != "" {
		anchor = findByID(msgs, last)
	}
	printed := false
	for i := range msgs {
		if msgs[i].ID == last {
			continue
		}
		if last == "" || isAfter(&msgs[i], anchor) {
			printMessageWithTime(w, msgs[i], "text", now, callCtx, i < len(msgs)-1)
			lastPrinted[sessionID] = msgs[i].ID
			printed = true
		}
	}
	return printed
}

// subAgentWriter wraps w so every line written to it is prefixed with a
// short tag naming the delegated child session — otherwise a sub-agent's
// "[tool: bash] …" output would be visually indistinguishable from the
// top-level session's own tool calls in the same stream.
func subAgentWriter(w io.Writer, childSessionID string) io.Writer {
	return &prefixWriter{w: w, prefix: "  [sub-agent " + short(session.HashID(childSessionID)) + "] "}
}

// prefixWriter prepends prefix to every line written through it. Used to
// visually nest a sub-agent delegation's live-tail output under the
// top-level session's own stream.
type prefixWriter struct {
	w      io.Writer
	prefix string
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	s := string(b)
	// strings.Split on a trailing "\n" produces a final empty element —
	// skip it so we don't emit a bare prefix-only line after the last
	// real one.
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		if _, err := fmt.Fprintf(p.w, "%s%s\n", p.prefix, line); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// newestMessageUnix returns the newest activity timestamp among a session's
// own messages (max of created_at / updated_at). Used by the watch loop as
// the baseline for the sub-agent pulse: a descendant session's activity only
// counts as "the fresher edge" when it is newer than every top-level row.
func newestMessageUnix(msgs []message.Message) int64 {
	var newest int64
	for i := range msgs {
		if ts := latestMessageUnix(msgs[i]); ts > newest {
			newest = ts
		}
	}
	return newest
}

// watchInterrupted reports whether the watch's context has been cancelled
// (a single Ctrl+C, via fang's signal.NotifyContext(os.Interrupt)). When it
// has, it prints the distinguishing interrupted message and returns true so
// the caller can exit immediately. Kept as a tiny, app-free seam so the
// interrupt-exit path is unit-testable without spinning up a real app / DB.
func watchInterrupted(ctx context.Context) bool {
	if ctx.Err() == nil {
		return false
	}
	printWatchInterrupted(os.Stderr)
	return true
}

// printWatchInterrupted emits the "stopped watching, session not ended"
// notice. Deliberately distinct from the end-of-session summary block so
// "I stopped watching" is never misread as "the session ended".
func printWatchInterrupted(w io.Writer) {
	fmt.Fprintln(w, "\n(interrupted — session still running)")
}

// liveLockMaxAge is the threshold for considering a lock file "alive" by
// mtime alone. The session heartbeat touches the lock every 10s; we add a
// 10s grace window so a missed tick during a slow GC pause / disk sync does
// not look like a dead process. Matches session.lockStaleDuration in spirit.
//
// Task #222: this threshold alone is no longer sufficient proof of death.
// The heartbeat's mtime touch is now gated on real RecordActivity() calls
// (task #214/#222), so a session blocked on a single long-running tool call
// can go up to toolExecutionMaxDefault (45 minutes in production) with the
// lock mtime never refreshing, while streamWatchdogTick (30s, see
// internal/agent/agent.go) means even the per-tick recordActivity fix does
// not guarantee a touch inside every 20s window — there's a transient
// window right after each tick where up to 30s can pass untouched. See
// combinedLockLiveness for the PID-liveness fallback that closes this gap.
const liveLockMaxAge = 20 * time.Second

// combinedLockLiveness reports whether a lock should be treated as "alive"
// for watch's end-detection purposes, combining two independent signals so
// neither one's blind spot alone can produce a false "session ended":
//
//   - mtimeFresh: the heartbeat touched the file within liveLockMaxAge.
//     Fast and cheap, but — per task #222 — can go stale on a perfectly
//     healthy session that's blocked in a single long tool call, since the
//     heartbeat's touch is now gated on recorded activity rather than an
//     unconditional timer.
//   - pidAlive: the PID recorded in the lock file is still a live OS
//     process (session.IsProcessAlive). Immune to the activity-gating
//     blind spot above, but doesn't by itself prove THIS session is what
//     the PID is doing (a crashed holder whose PID got reused would give a
//     false positive) — which is why mtimeFresh remains the primary,
//     faster-to-go-stale-and-thus-safer-in-the-common-case signal, with
//     pidAlive as the fallback that only matters when mtime already looks
//     stale.
//
// Kept as its own tiny pure function (no app/filesystem/clock — the caller
// resolves both booleans first) so combining them is independently
// unit-testable, same spirit as isSessionFinishedFromState.
func combinedLockLiveness(mtimeFresh, pidAlive bool) bool {
	return mtimeFresh || pidAlive
}

// isSessionFinished reports whether a live-tail loop should exit. Returns
// the end reason as a short human label so the summary block can show
// it next to "reason:". I/O-doing wrapper; the pure decision lives in
// isSessionFinishedFromState so it is unit-testable without an app /
// filesystem.
//
// Lock liveness is delegated entirely to session.InspectSessionLock (task
// #241) rather than re-implementing the "mtime fresh, else fall back to a
// bounded PID-liveness probe" check locally. This file used to hand-roll
// that exact check (mtimeFresh/pidAlive/combinedLockLiveness below) without
// ever bounding the PID fallback, so a lock abandoned by a killed/crashed
// `crush run` whose recorded PID the OS later recycled for an unrelated
// process would report lockAlive: true forever — isSessionFinishedFromState
// would then never see a false lockAlive, and `sessions watch` would hang
// on a session that in fact ended hours earlier. InspectSessionLock already
// carries the fix for that (maxPidFallbackAge, task #235); calling it here
// closes the same gap without a second, independently-maintained copy of
// the bound. combinedLockLiveness itself is left in place (still exercised
// by its own unit tests) as it remains an accurate description of the OR
// semantics InspectSessionLock's Live field encodes internally.
func isSessionFinished(ctx context.Context, a *app.App, sessionID, dataDir string) (watchState, string) {
	sess, sessErr := a.Sessions.Get(ctx, sessionID)
	msgs, msgsErr := a.Messages.List(ctx, sessionID)

	lockAlive := session.InspectSessionLock(dataDir, sessionID, liveLockMaxAge).Live

	done, reason := isSessionFinishedFromState(sess, sessErr, msgs, msgsErr, lockAlive)

	// Freshest activity anywhere we can see it, used to tell "this session
	// wrapped up long ago" from "something is happening right now".
	lastActivity := sess.UpdatedAt
	if newest := newestMessageUnix(msgs); newest > lastActivity {
		lastActivity = newest
	}
	return watchState{
		done:         done,
		lockAlive:    lockAlive,
		lastActivity: lastActivity,
	}, reason
}

// watchState is the raw per-tick observation the live-tail loop feeds into
// [decideWatchExit]. Split out from the exit decision so the (surprisingly
// subtle) "is this really the end?" rule can be unit-tested without an app,
// a filesystem or a clock.
type watchState struct {
	// done is the classic end verdict from isSessionFinishedFromState.
	done bool
	// lockAlive reports whether the session lock was heartbeating this tick.
	lockAlive bool
	// lastActivity is the newest unix timestamp seen on the session row or
	// any of its messages. 0 when unknown.
	lastActivity int64
}

// watchStartGrace is how long a freshly started watch keeps waiting when
// the session ALREADY looked finished before the watch ever observed a
// live lock.
//
// Why this exists: `crush run --session <id>` on an existing session
// RESUMES it, and clears the previous run's ended_reason only once the app
// has booted (app.go's SetEndedReason(ctx, id, "")). Booting takes seconds
// — config load, DB open, provider init. An orchestrator that launches the
// run and immediately starts `sessions watch` therefore lands in a window
// where the lock of the new run does not exist yet while the DB still
// carries the PREVIOUS run's terminal state (ended_reason set, last
// assistant message finished with end_turn). Watch used to trust that
// instantly and print a full "--- session ended ---" summary for a session
// that was in fact just starting — observed in the wild on session
// r24-8-dealloc-batch-internals, where `sessions why` reported the session
// alive with a 5s-old heartbeat moments after watch had "completed".
//
// 15s comfortably covers a cold start while staying far below the
// patience of anyone watching a live session.
const watchStartGrace = 15 * time.Second

// watchIdleForSure is how quiet a session must be for a missing lock to be
// unambiguous. Past this, nothing is starting up — the session genuinely
// ended earlier — so `sessions watch <old-session>` still prints its
// summary immediately instead of sitting through watchStartGrace.
const watchIdleForSure = 60 * time.Second

// watchVerdict is what the live-tail loop should do about a tick.
type watchVerdict int

const (
	// watchKeepWatching: the session is running (or nothing says otherwise).
	watchKeepWatching watchVerdict = iota
	// watchExit: the session really has ended — print the summary and stop.
	watchExit
	// watchWaitForStart: it LOOKS ended, but a run may be booting into this
	// session right now. Hold off until watchStartGrace expires.
	watchWaitForStart
)

// decideWatchExit turns a raw observation into an action.
//
// The only genuinely ambiguous case is "the end signals fire, but this
// watch has never once seen the session's lock alive". That is both what a
// long-finished session looks like AND what a session being resumed looks
// like during the new run's boot window. Two escape hatches keep the grace
// from costing anything in practice:
//
//   - sawLiveLock: once we have observed the session actually running, a
//     later end signal is trusted immediately. This is the normal
//     "watch a live session until it finishes" path — unchanged, no delay.
//   - lastActivityAge > watchIdleForSure: a session nothing has touched for
//     a minute is not mid-boot. Trusted immediately.
func decideWatchExit(st watchState, sawLiveLock bool, sinceWatchStart, lastActivityAge time.Duration) watchVerdict {
	if !st.done {
		return watchKeepWatching
	}
	if sawLiveLock || lastActivityAge > watchIdleForSure || sinceWatchStart >= watchStartGrace {
		return watchExit
	}
	return watchWaitForStart
}

// isSessionFinishedFromState is the pure decision used by isSessionFinished.
//
// The lock heartbeat is the AUTHORITATIVE signal: while the holding
// process is alive (lock mtime < liveLockMaxAge) we never terminate the
// watch, regardless of what the DB rows say. This guards against the
// real-world failure mode where a tool-result message carries a Finish
// part with reason="stop" (the tool finished — not the session), or
// where an assistant message has Finish reason="tool_use" (it ran a
// tool and is about to consume the result, not done).
//
// Only when the lock is no longer alive do we trust the DB-derived
// signals:
//
//	(a) session row has a non-empty EndedReason
//	(b) lock disappeared / went stale AND the session has at least one
//	    message (the "at least one message" guard avoids racing the
//	    acquirer that has opened the file but not yet touched / written
//	    the lock)
//	(c) the latest ASSISTANT message has a non-partial Finish whose
//	    Reason is a terminal FinishReason (end_turn / max_tokens /
//	    canceled / error). tool_use, unknown, and any unrecognised
//	    string are treated as "not yet done" — the agent is mid-loop.
//
// Errors on the session lookup are treated as "no signal (a)", and
// errors on the message lookup as "no signal (b)/(c)" — neither is
// treated as termination, so a transient DB hiccup does not end the tail.
func isSessionFinishedFromState(
	sess session.Session,
	sessErr error,
	msgs []message.Message,
	msgsErr error,
	lockAlive bool,
) (bool, string) {
	if lockAlive {
		return false, ""
	}
	if sessErr == nil && sess.EndedReason != "" {
		return true, sess.EndedReason
	}
	if msgsErr == nil && len(msgs) > 0 {
		// Walk back to the latest assistant message — tool result rows
		// carry their own Finish parts (e.g. reason="stop" when the
		// tool subprocess exits) that have nothing to do with end of
		// session. Only the assistant author's own finish counts.
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			if m.Role != message.Assistant {
				continue
			}
			f := m.FinishPart()
			if f == nil || f.Partial {
				break
			}
			if isTerminalFinishReason(f.Reason) {
				return true, string(f.Reason)
			}
			// Latest assistant has Finish but it's tool_use / unknown /
			// some unrecognised reason — the loop is mid-step, not done.
			break
		}
		// Lock is not alive AND we have at least one message — the
		// holder process is gone but the session never wrote an
		// EndedReason or a terminal assistant Finish. Treat as ended.
		return true, "lock_released"
	}
	return false, ""
}

// isTerminalFinishReason reports whether a FinishReason indicates the
// agent has finished its work for this turn AND has nothing queued
// (i.e. it is safe to consider the session done). tool_use means the
// agent ran a tool and will continue after the result; unknown means
// we cannot tell; everything else recognised is a real end.
func isTerminalFinishReason(r message.FinishReason) bool {
	switch r {
	case message.FinishReasonEndTurn,
		message.FinishReasonMaxTokens,
		message.FinishReasonCanceled,
		message.FinishReasonError:
		return true
	}
	return false
}

// printWatchSummary emits the final block shown when a watched session
// finishes. Pulls fresh totals from the session row so any in-flight
// IncrementCost from the agent's last step is reflected. Thin wrapper;
// the formatting lives in formatWatchSummary so it can be unit-tested
// without a live app.
func printWatchSummary(w io.Writer, ctx context.Context, a *app.App, sessionID, reason string) {
	sess, err := a.Sessions.Get(ctx, sessionID)
	if err != nil {
		fmt.Fprintf(w, "\n--- session ended (could not load summary: %v) ---\n", err)
		return
	}
	fmt.Fprint(w, formatWatchSummary(sess, reason, time.Now()))
}

// formatWatchSummary renders the human-readable end-of-watch block.
// "now" is taken as an argument so tests can pin duration to a known
// value without sleeping. Layout (one blank line above for separation
// from the live message stream):
//
//	--- session ended ---
//	id:       <session-id>
//	title:    <title>           (omitted when empty)
//	reason:   <reason>
//	duration: <X>h<Y>m / <Y>m<Z>s / <Z>s  (compact form)
//	tokens:   <total> (prompt <p> + completion <c>)
//	cost:     $0.0000 [ / $X.XXXX budget ]
func formatWatchSummary(sess session.Session, reason string, now time.Time) string {
	duration := time.Duration(0)
	if sess.CreatedAt > 0 {
		duration = now.Sub(time.Unix(sess.CreatedAt, 0))
	}
	tokens := sess.PromptTokens + sess.CompletionTokens
	var b strings.Builder
	b.WriteString("\n--- session ended ---\n")
	fmt.Fprintf(&b, "id:       %s\n", sess.ID)
	if sess.Title != "" {
		fmt.Fprintf(&b, "title:    %s\n", sess.Title)
	}
	fmt.Fprintf(&b, "reason:   %s\n", reason)
	fmt.Fprintf(&b, "duration: %s\n", formatDurationShort(duration))
	fmt.Fprintf(&b, "tokens:   %s (prompt %s + completion %s)\n",
		formatWatchInt(tokens), formatWatchInt(sess.PromptTokens), formatWatchInt(sess.CompletionTokens))
	fmt.Fprintf(&b, "cost:     $%.4f", sess.Cost)
	if sess.BudgetMaxCost > 0 {
		fmt.Fprintf(&b, " / $%.4f budget", sess.BudgetMaxCost)
	}
	b.WriteString("\n")
	return b.String()
}

// pickSessionForWatch runs the interactive picker used by both
// "sessions pick" and "sessions watch". Returns "" when the user quits
// without selecting.
func pickSessionForWatch(ctx context.Context, a *app.App) (string, error) {
	sessions, err := a.Sessions.List(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to list sessions: %w", err)
	}
	// Filter out internal child sessions — same convention as sessions pick.
	visible := sessions[:0]
	for _, s := range sessions {
		if s.ParentSessionID != "" {
			continue
		}
		visible = append(visible, s)
	}
	if len(visible) == 0 {
		fmt.Fprintln(os.Stderr, "(no sessions)")
		return "", nil
	}

	items := make([]sessionItem, len(visible))
	now := time.Now()
	for i, s := range visible {
		items[i] = sessionItem{
			id:      s.ID,
			hash:    short(session.HashID(s.ID)),
			title:   truncate(s.Title, 40),
			updated: time.Unix(s.UpdatedAt, 0).Format("2006-01-02 15:04"),
			cost:    s.Cost,
			ago:     formatAge(now.Sub(time.Unix(s.UpdatedAt, 0))),
		}
	}
	items, hidden := trimSessionItems(items, pickerMaxItems)

	m := pickerModel{
		items:  items,
		hidden: hidden,
		cursor: 0,
		binary: os.Args[0],
	}
	p := tea.NewProgram(&m)
	if _, err := p.Run(); err != nil {
		return "", fmt.Errorf("failed to run picker: %w", err)
	}
	if m.quit || m.selected == "" {
		return "", nil
	}
	return m.selected, nil
}

// formatWatchInt thousands-separates a token count for the summary line.
// (Renamed from the old dashboard helper so it doesn't read like the
// removed feature was still around.)
func formatWatchInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

// formatAge formats a duration for the picker's "ago" column. Used by
// both sessions_pick.go and sessions_watch.go.
func formatAge(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh%dm", h, m)
}
