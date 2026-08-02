package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// watchdogCause identifies WHY the watchdog fired, so callers can attribute
// the resulting user-facing finish message to the actual cause instead of
// collapsing every non-tool-timeout fire into a single "stream stalled"
// bucket.
//
// Fork patch: task #223 — split out of a single `toolTimeout bool` that
// could only distinguish "a tool ran too long" from "everything else". That
// left a genuine --timeout-hard-cap fire (whether or not a tool happened to
// be in flight) indistinguishable from a real provider idle-stall: both
// passed toolTimeout=false, so agent.go's onFire callback always reported
// "Stream stalled" — blaming the provider for silence even when the
// provider was fine and the turn simply hit its own configured wall-clock
// ceiling. See CHANGELOG.md for the two-step history: 77c1104a fixed the
// boolean misclassification (hard-cap-with-tool-in-flight no longer claimed
// toolTimeout=true), and this type finishes the job by giving the hard-cap
// case its own identity so the finish message can name the real cause.
type watchdogCause int

const (
	// causeIdleStall means the provider stopped sending data for
	// idleTimeout (or the extendsOnProgress-extended deadline derived from
	// it) with no tool in flight. This is the only cause that should ever
	// produce a "Stream stalled" / provider-blaming message.
	causeIdleStall watchdogCause = iota
	// causeToolTimeout means a tool (including a sub-agent delegation) ran
	// past toolMaxDuration (+ toolCleanupGrace) while in flight — the
	// never-freeze backstop. This is the only cause that should ever
	// produce a "Tool timeout" message.
	causeToolTimeout
	// causeHardCap means the turn hit the operator-configured
	// --timeout-hard-cap wall-clock ceiling, independent of provider
	// activity or tool state — the tool-in-flight state at the moment it
	// fired is incidental, not the cause. Must never be reported as either
	// causeIdleStall or causeToolTimeout.
	causeHardCap
)

// streamWatchdog implements the "Codec must surface control" invariant for
// LLM streaming: if a provider stops sending data mid-stream (network
// glitch, rate-limit, HTTP/2 stall, backend hiccup), we MUST detect it and
// force-unblock the agent rather than freeze with it. The 162-promise-all
// stuck session (D:\dev\garnet-team\.crush, see post-mortem in
// CHANGELOG.fork.md section 4.D) is the failure mode this protects
// against — 4 streams froze for 1.5h until the user killed the process.
type streamWatchdog struct {
	// bump records progress: called from every fantasy stream callback
	// (OnTextDelta, OnReasoningDelta, OnToolInputStart, OnToolCall,
	// OnToolResult, OnStepFinish, OnRetry, ...).
	bump func()
	// toolStarted / toolFinished bracket synchronous tool execution. While
	// any tool is in flight the idle timer is PAUSED — a long `cargo build`,
	// `cargo clippy`, test run or compile is not a provider stall, and the
	// provider legitimately sends nothing until the tool returns. Without
	// this, any single bash command longer than idleTimeout was force-
	// cancelled as a false stall (observed: shamir-db f1-rename-index killed
	// at exactly 180s during a workspace clippy).
	//
	// A single toolMaxDuration bounds the pause regardless of which tool is
	// in flight — including a sub-agent delegation via the `agent` tool.
	// There used to be a separate, larger cap just for delegations, but
	// distinguishing "exempt" from "plain" tools this way produced its own
	// false cutoffs: a sub-agent's OWN plain tool call (a slow build/test
	// inside its turn) hit the shorter plain cap just as often as a parent
	// waiting on a delegation hit the old short cap. One generous cap for
	// every tool avoids both failure modes at the cost of a genuinely wedged
	// tool taking longer to get caught — an acceptable tradeoff since the
	// point is to bound the wait, not to bound it tightly.
	toolStarted  func()
	toolFinished func()
	// stalled reports whether THIS watchdog (not user/web cancel) is what
	// fired the cancel. Callers use it to distinguish "user pressed
	// Ctrl-C" from "watchdog timed out" when crafting the finish-part
	// message the user sees.
	stalled *atomic.Bool
	// done is closed when the watchdog goroutine has exited. Callers
	// MUST receive from it before returning to avoid a goroutine leak.
	done <-chan struct{}
}

// startStreamWatchdog launches a goroutine that calls cancel() if bump() is
// not invoked for idleTimeout. The goroutine exits when ctx is cancelled
// (either by the caller or by the watchdog itself) and closes done.
//
// onFire, if non-nil, is invoked AFTER stalled is set to true and BEFORE
// cancel() — typically used to emit a slog.Warn with diagnostic context the
// watchdog itself does not have (session ID, model, provider, etc.). The
// watchdogCause identifies which of the three fire conditions triggered:
// causeToolTimeout when a tool exceeded toolMaxDuration (the never-freeze
// backstop), causeHardCap when the turn hit --timeout-hard-cap (regardless
// of whether a tool happened to be in flight at the time), or causeIdleStall
// when the provider genuinely stopped sending data for idleTimeout with no
// tool in flight.
//
// Fork patch: batch 8 — extendsOnProgress + hardCap parameters.
// When extendsOnProgress is true, every bump() also extends the effective
// deadline to max(absoluteDeadline, now+idleTimeout), capped at hardCap
// from the start time. This prevents killing healthy long compositions
// while still bounding the worst case.
//
// Fork patch: never-freeze backstop — toolMaxDuration bounds the tool
// pause. Past it the watchdog fires with cause=causeToolTimeout so the turn
// ends instead of hanging forever on a stuck tool. One cap applies to
// every tool in flight, sub-agent delegations included — see toolStarted's
// doc on the streamWatchdog struct for why a single generous cap replaced
// the old plain-vs-exempt split.
//
// Fork patch: parent/child cancellation race — toolCleanupGrace adds a
// fixed, tool-agnostic buffer ON TOP of toolMaxDuration before this
// watchdog is allowed to fire on the tool-pause path. It exists because a
// tool call that is itself an `agent`-tool delegation runs a full nested
// Run()/runTurn() with its OWN stream watchdog, started only once the
// delegation actually begins executing (after init, DB preamble, etc.) —
// strictly LATER than this (the parent's) watchdog started counting from
// OnToolCall. Both watchdogs are configured with the same toolMaxDuration
// (see toolExecutionMaxDefault's doc for why a two-tier, tool-name-keyed
// cap was rejected), so without a grace period the parent's clock, having
// a head start, always reaches the cap first and cancels genCtx — which
// cascades into the child's ctx — before the child's OWN watchdog ever
// gets to fire on ITS cap and let the child unwind cleanly (write its
// finish part, transfer cost to the parent session per task #197, dump
// goroutines for diagnostics). toolCleanupGrace is added uniformly to
// EVERY tool-in-flight, not selected by tool name/kind — it is a
// structural allowance for "this watchdog is the OUTER one waiting on a
// tool that may itself be watchdog-protected and need to unwind," not a
// special case for the `agent` tool specifically. A plain (non-delegating)
// tool that runs past toolMaxDuration+toolCleanupGrace is still caught,
// just toolCleanupGrace later than before.
//
// Fork patch: task #222 — recordActivity, if non-nil, is invoked on every
// bump() (harmless duplication with the caller's own notify, see bump's
// doc) AND, critically, once per tick while a tool is genuinely in flight
// and still under its cap (see the toolsInFlight branch below). Without
// the latter, a session blocked on a single long-running tool call (e.g.
// a 45-minute toolExecutionMaxDefault-bounded sub-agent delegation)
// recorded ZERO activity for the tool's entire duration — bumpActivity
// only fires from fantasy stream callbacks, none of which fire while a
// tool is synchronously executing. That starved SessionLock's
// activity-gated heartbeat (RecordActivity/task #214) for up to 45
// minutes on a perfectly healthy session, which several consumers
// (`sessions locks` auto-delete, `sessions watch` liveness) mistake for a
// dead holder. recordActivity is typically wired to the same
// notifyActivity(genCtx) callback runTurn already composes via
// withActivityNotify, so this requires no new plumbing beyond passing it
// through.
func startStreamWatchdog(
	ctx context.Context,
	cancel context.CancelFunc,
	idleTimeout, tick time.Duration,
	onFire func(elapsed time.Duration, cause watchdogCause),
	extendsOnProgress bool,
	hardCap time.Duration,
	toolMaxDuration time.Duration,
	toolCleanupGrace time.Duration,
	recordActivity func(),
) streamWatchdog {
	var last atomic.Int64
	startTime := time.Now()
	last.Store(startTime.UnixNano())
	var stalled atomic.Bool
	// toolsInFlight counts tool executions currently running. While > 0 the
	// idle timer is paused (see toolStarted/toolFinished in the struct doc).
	var toolsInFlight atomic.Int64
	// toolStartedAt records the wall-clock time (UnixNano) at which the
	// first tool in the current in-flight batch started. Used with
	// toolMaxDuration to bound the tool pause. Reset to 0 when all tools
	// finish.
	var toolStartedAt atomic.Int64
	// absoluteDeadline is the original deadline from process start.
	absoluteDeadline := startTime.Add(idleTimeout)
	// hardDeadline is the hard cap (e.g. 4x idleTimeout).
	hardDeadline := absoluteDeadline
	if hardCap > 0 {
		hardDeadline = startTime.Add(hardCap)
	}
	done := make(chan struct{})

	// Rate-limited logging for deadline extensions.
	var lastLogNanos atomic.Int64
	const logInterval = 30 * time.Second

	bump := func() {
		now := time.Now()
		last.Store(now.UnixNano())
		if recordActivity != nil {
			recordActivity()
		}
		if extendsOnProgress {
			// Extend the absolute deadline, capped at hardDeadline.
			newDeadline := now.Add(idleTimeout)
			if newDeadline.After(hardDeadline) {
				newDeadline = hardDeadline
			}
			// Rate-limited INFO log for extensions.
			if now.UnixNano()-lastLogNanos.Load() > int64(logInterval) {
				lastLogNanos.Store(now.UnixNano())
				slog.Info(
					"stream-watchdog deadline extended",
					"new_deadline", newDeadline.Format(time.RFC3339),
					"reason", "progress",
				)
			}
		}
	}

	toolStarted := func() {
		if toolsInFlight.Add(1) == 1 {
			toolStartedAt.Store(time.Now().UnixNano())
		}
	}
	toolFinished := func() {
		if remaining := toolsInFlight.Add(-1); remaining <= 0 {
			// Defensive: a missing OnToolCall (or a double result) must not
			// leave the counter negative and silently disable the watchdog.
			toolsInFlight.Store(0)
			toolStartedAt.Store(0)
		} else {
			// Other tools from this batch are still in flight. fantasy fires
			// every OnToolCall for a step BEFORE executing any of them, so a
			// "batch" here is often several tool calls the model issued
			// together but that the executor actually runs one at a time —
			// not true parallel execution. Without this reset,
			// toolStartedAt stays pinned to when the FIRST tool in the
			// batch started, so toolMaxDuration bounds the batch's
			// CUMULATIVE wall time instead of any single tool's runtime —
			// several individually-fast sequential tool calls (e.g. four
			// 8s bash commands under a 12s cap) could sum past the cap and
			// get force-cancelled even though none of them was ever
			// actually stuck (observed live: a sub-agent running four
			// short, safely-bounded bash steps got killed by this exact
			// accumulation). This finish is forward progress on the batch
			// — restart the clock from now, so the cap instead bounds the
			// gap since the LAST tool finished (or the batch started,
			// whichever is more recent). A genuinely stuck remaining tool
			// is still caught: if nothing finishes for a full
			// toolMaxDuration after this reset, the next tick fires
			// exactly as before.
			toolStartedAt.Store(time.Now().UnixNano())
		}
		// Restart the idle clock fresh: the provider is about to resume, so
		// don't count the tool's runtime against the next stall window.
		last.Store(time.Now().UnixNano())
	}

	go func() {
		defer close(done)
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				// Pause while a tool is executing — provider silence during a
				// long compile/test is expected, not a stall. Keep `last`
				// fresh so the idle window starts clean once the tool returns.
				// The pause is bounded: past the applicable cap the watchdog
				// fires with cause=causeToolTimeout (never-freeze backstop).
				if toolsInFlight.Load() > 0 {
					// One cap for the whole batch, regardless of which kind of
					// tool is in it — including a sub-agent delegation via the
					// `agent` tool. Two earlier revisions got this wrong in
					// opposite directions: applying the short primitive-tool cap
					// to a delegation force-cancelled healthy sub-agent work
					// that legitimately runs long; skipping the cap entirely
					// for delegations (and letting the `continue` below bypass
					// every other deadline check too) left the watchdog
					// structurally unable to fire AT ALL while a delegation was
					// in flight — an unbounded parent wait, which is how a
					// wedged child silently froze the whole process (observed:
					// four concurrent `agent` calls in flight, then 15+ minutes
					// of total silence with the process still alive, no error,
					// no finish part, no diagnostics). A single generous cap
					// applied uniformly avoids both: nothing gets a shorter
					// leash than anything else, and nothing gets no leash.
					if toolMaxDuration > 0 {
						if startedAt := toolStartedAt.Load(); startedAt > 0 {
							// toolCleanupGrace gives a nested (child) watchdog —
							// e.g. inside an `agent`-tool delegation — a window to
							// fire on its OWN toolMaxDuration and unwind cleanly
							// before this (outer) watchdog force-cancels the whole
							// batch. See the doc comment on toolCleanupGrace in
							// startStreamWatchdog's signature for why this is a
							// uniform, tool-agnostic buffer rather than a special
							// case keyed on tool name.
							effectiveCap := toolMaxDuration + toolCleanupGrace
							if elapsed := now.Sub(time.Unix(0, startedAt)); elapsed >= effectiveCap {
								stalled.Store(true)
								if onFire != nil {
									onFire(elapsed, causeToolTimeout)
								}
								cancel()
								return
							}
						}
					}
					// An EXPLICITLY configured hard cap must actually be hard:
					// it bounds the whole turn, tool execution included.
					// absoluteDeadline is deliberately NOT applied here — that
					// one is the idle deadline, and a long but healthy tool run
					// must not trip it.
					if hardCap > 0 && now.After(hardDeadline) {
						stalled.Store(true)
						if onFire != nil {
							onFire(now.Sub(startTime), causeHardCap)
						}
						cancel()
						return
					}
					last.Store(now.UnixNano())
					// A tool is in flight AND still under its cap — that is
					// legitimate ongoing activity, not silence. Record it every
					// tick, not just at toolStarted/toolFinished transitions, so
					// SessionLock's activity-gated heartbeat (see recordActivity's
					// doc above) keeps ticking for the tool's whole duration
					// instead of going dark for up to toolMaxDuration.
					if recordActivity != nil {
						recordActivity()
					}
					continue
				}
				lastActivity := time.Unix(0, last.Load())
				idle := now.Sub(lastActivity)

				// An EXPLICITLY configured hard cap must actually be hard: it
				// bounds the whole turn regardless of extendsOnProgress. This
				// check is UNCONDITIONAL — hoisted above the
				// extendsOnProgress branch — because a provider that keeps
				// the stream alive with regular chunks (bump() called often
				// enough that idle never reaches idleTimeout) would otherwise
				// let the turn run forever when extendsOnProgress is false
				// (the default): the idle-only check below never fires, and
				// the old hardCap check lived exclusively inside the
				// extendsOnProgress branch, so --timeout-hard-cap was
				// silently ignored on the non-extending path. cause is
				// causeHardCap here — this is a wall-clock turn limit, not a
				// stuck tool and not provider idle. The hardCap check inside
				// the toolsInFlight branch above reports causeHardCap for the
				// same reason: it's the same wall-clock-hard-cap concept,
				// just checked while a tool happens to be in flight. Only the
				// never-freeze tool-pause backstop (also in the toolsInFlight
				// branch) legitimately reports causeToolTimeout, and only the
				// two idle checks below (extendsOnProgress and plain) report
				// causeIdleStall.
				if hardCap > 0 && now.After(hardDeadline) {
					stalled.Store(true)
					if onFire != nil {
						onFire(idle, causeHardCap)
					}
					cancel()
					return
				}

				if extendsOnProgress {
					// Effective deadline: max(absoluteDeadline,
					// lastActivity+idleTimeout), capped at hardDeadline.
					// hardDeadline is not re-checked here — the unconditional
					// check above already covers it — but effectiveDeadline
					// is still capped at hardDeadline so extension never
					// reports a deadline past the hard cap.
					effectiveDeadline := absoluteDeadline
					extended := lastActivity.Add(idleTimeout)
					if extended.After(effectiveDeadline) {
						effectiveDeadline = extended
					}
					if effectiveDeadline.After(hardDeadline) {
						effectiveDeadline = hardDeadline
					}
					if now.After(effectiveDeadline) {
						stalled.Store(true)
						if onFire != nil {
							onFire(idle, causeIdleStall)
						}
						cancel()
						return
					}
				} else {
					// Original behavior: fire if idle since last
					// activity exceeds idleTimeout.
					if idle >= idleTimeout {
						stalled.Store(true)
						if onFire != nil {
							onFire(idle, causeIdleStall)
						}
						cancel()
						return
					}
				}
			}
		}
	}()
	return streamWatchdog{
		bump:         bump,
		toolStarted:  toolStarted,
		toolFinished: toolFinished,
		stalled:      &stalled,
		done:         done,
	}
}

// watchdogFinishMessage maps a watchdogCause to the (title, body) pair
// recorded via AddFinish on the assistant message the user sees. Extracted
// as a pure function (task #223) so the cause-to-message mapping is
// directly unit-testable without needing to drive a full runTurn.
//
// Each cause gets its OWN title and cites its OWN configured duration —
// never another cause's duration — so the message never misattributes the
// fire to the wrong subsystem:
//   - causeToolTimeout → "Tool timeout", cites toolMaxDuration. A specific
//     tool call (or sub-agent delegation) ran too long.
//   - causeHardCap → "Turn timeout", cites hardCap (a.timeoutHardCap). The
//     turn's own operator-configured wall-clock ceiling fired — independent
//     of whether a tool happened to be in flight at that moment. Must NOT
//     be phrased as blaming the provider (that's causeIdleStall's job) or a
//     tool (that's causeToolTimeout's job).
//   - anything else (causeIdleStall) → "Stream stalled", cites idleTimeout
//     and names the provider. The provider genuinely stopped sending data.
func watchdogFinishMessage(cause watchdogCause, toolMaxDuration, hardCap, idleTimeout time.Duration, provider string) (title, body string) {
	switch cause {
	case causeToolTimeout:
		return "Tool timeout", fmt.Sprintf(
			"A tool ran for over %s without returning and was auto-cancelled by the watchdog to keep the agent responsive. Re-run the step; if it's a long job, poll its status instead of blocking.",
			toolMaxDuration,
		)
	case causeHardCap:
		return "Turn timeout", fmt.Sprintf(
			"The turn exceeded its configured --timeout-hard-cap of %s and was auto-cancelled by the stream watchdog. This is the overall wall-clock limit for a single turn, not a provider stall or a stuck tool. Re-run with a larger --timeout-hard-cap if the turn genuinely needs more time.",
			hardCap,
		)
	default: // causeIdleStall
		return "Stream stalled", fmt.Sprintf(
			"Provider %q stopped sending streaming data for over %s — the request was auto-cancelled by the stream watchdog. Retry the prompt; if it keeps happening, try a different model or provider.",
			provider, idleTimeout,
		)
	}
}

// watchdogToolResultMessage maps a watchdogCause to the tool-result content
// recorded for any tool call still unfinished when the watchdog fired. This
// is the MODEL-facing counterpart of watchdogFinishMessage: the human sees
// the AddFinish (title, body) pair, but the model only sees whatever text we
// put in this synthetic tool result, so it needs its own cause-aware wording
// rather than reusing watchdogFinishMessage's longer human-oriented body.
//
// Before this (task #227, same root-cause family as task #223's
// watchdogFinishMessage split), this call site hardcoded "the provider
// stream stalled for >idleTimeout" unconditionally — so a hard-cap or
// tool-timeout fire told the MODEL the wrong story even after 7b48f75a
// fixed the human-facing message. Reuses watchdogFinishMessage's (title,
// body) pair as the source of truth for cause-specific wording/duration so
// the two call sites can never drift apart on what each cause means.
func watchdogToolResultMessage(cause watchdogCause, toolMaxDuration, hardCap, idleTimeout time.Duration, provider string) string {
	title, body := watchdogFinishMessage(cause, toolMaxDuration, hardCap, idleTimeout, provider)
	return fmt.Sprintf("Tool call was cancelled: %s — %s", title, body)
}
