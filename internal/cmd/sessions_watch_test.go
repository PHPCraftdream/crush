package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
)

// In the tests below, the last lockAlive argument follows the new
// semantics: it's true ONLY when the lock heartbeat is fresh (process
// is verifiably alive). When the lock is missing OR stale (mtime older
// than the heartbeat window) lockAlive is false. The function now
// treats lockAlive=true as an absolute "do not terminate" signal that
// overrides every DB-derived signal — see isSessionFinishedFromState's
// docstring for why (tool-result rows can carry Finish.Reason="stop"
// that has nothing to do with end of session).

func TestIsSessionFinishedFromState_LockAlive_BlocksEndedReason(t *testing.T) {
	// Even with EndedReason set, an alive lock means the process is
	// still doing post-finish work (cleanup, summary stream, etc).
	// Don't print the summary block until the process actually exits.
	sess := session.Session{ID: "s1", EndedReason: "max_cost"}
	done, reason := isSessionFinishedFromState(sess, nil, nil, nil, true)
	assert.False(t, done, "lock alive must override EndedReason")
	assert.Equal(t, "", reason)
}

func TestIsSessionFinishedFromState_LockAlive_BlocksFinishPart(t *testing.T) {
	// The real-world bug this guards: tool-result rows can have
	// Finish.Reason="stop" mid-session. An alive lock proves the
	// process is still mid-loop, so don't terminate on that signal.
	sess := session.Session{ID: "s1"}
	msg := message.Message{
		ID:   "m1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.Finish{Reason: message.FinishReasonEndTurn, Partial: false},
		},
	}
	done, reason := isSessionFinishedFromState(sess, nil, []message.Message{msg}, nil, true)
	assert.False(t, done, "lock alive must override a terminal-looking Finish part")
	assert.Equal(t, "", reason)
}

func TestIsSessionFinishedFromState_SignalA_EndedReason(t *testing.T) {
	sess := session.Session{ID: "s1", EndedReason: "max_cost"}
	done, reason := isSessionFinishedFromState(sess, nil, nil, nil, false)
	assert.True(t, done, "EndedReason + dead lock must terminate")
	assert.Equal(t, "max_cost", reason)
}

func TestIsSessionFinishedFromState_SignalB_LockGoneWithMessages(t *testing.T) {
	sess := session.Session{ID: "s1"} // no EndedReason
	msgs := []message.Message{{ID: "m1"}}
	done, reason := isSessionFinishedFromState(sess, nil, msgs, nil, false)
	assert.True(t, done)
	assert.Equal(t, "lock_released", reason)
}

func TestIsSessionFinishedFromState_SignalB_LockGoneButNoMessagesYet(t *testing.T) {
	// Race guard: lock missing + zero messages may mean the acquirer
	// has opened but not yet written. Don't terminate.
	sess := session.Session{ID: "s1"}
	done, reason := isSessionFinishedFromState(sess, nil, nil, nil, false)
	assert.False(t, done, "lock gone but no messages yet must not terminate (race guard)")
	assert.Equal(t, "", reason)
}

func TestIsSessionFinishedFromState_SignalC_AssistantEndTurn(t *testing.T) {
	sess := session.Session{ID: "s1"}
	msg := message.Message{
		ID:   "m1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.Finish{Reason: message.FinishReasonEndTurn, Partial: false},
		},
	}
	done, reason := isSessionFinishedFromState(sess, nil, []message.Message{msg}, nil, false)
	assert.True(t, done)
	assert.Equal(t, "end_turn", reason)
}

func TestIsSessionFinishedFromState_SignalC_AssistantMaxTokens(t *testing.T) {
	sess := session.Session{ID: "s1"}
	msg := message.Message{
		ID:   "m1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.Finish{Reason: message.FinishReasonMaxTokens, Partial: false},
		},
	}
	done, reason := isSessionFinishedFromState(sess, nil, []message.Message{msg}, nil, false)
	assert.True(t, done, "max_tokens is a terminal finish reason")
	assert.Equal(t, "max_tokens", reason)
}

func TestIsSessionFinishedFromState_SignalC_AssistantCanceledOrError(t *testing.T) {
	sess := session.Session{ID: "s1"}
	for _, r := range []message.FinishReason{
		message.FinishReasonCanceled,
		message.FinishReasonError,
	} {
		msg := message.Message{
			ID:    "m1",
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.Finish{Reason: r, Partial: false}},
		}
		done, reason := isSessionFinishedFromState(sess, nil, []message.Message{msg}, nil, false)
		assert.True(t, done, "reason %q must terminate", r)
		assert.Equal(t, string(r), reason)
	}
}

func TestIsSessionFinishedFromState_SignalC_ToolUseDoesNotTerminate(t *testing.T) {
	// The agent ran a tool and is about to consume the result — that's
	// mid-loop, not end of session. The watch must keep polling.
	// BUT: with the lock dead and at least one message, signal (b) kicks
	// in and ends the watch with "lock_released" — that's correct, the
	// process exited before completing the loop.
	// Here we test the BEFORE-signal-(b) check: tool_use alone, with
	// lock alive (so signal (b) is blocked), must not terminate.
	sess := session.Session{ID: "s1"}
	msg := message.Message{
		ID:   "m1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.Finish{Reason: message.FinishReasonToolUse, Partial: false},
		},
	}
	done, reason := isSessionFinishedFromState(sess, nil, []message.Message{msg}, nil, true)
	assert.False(t, done, "tool_use is not a terminal finish reason")
	assert.Equal(t, "", reason)
}

func TestIsSessionFinishedFromState_SignalC_UnknownReasonDoesNotTerminate(t *testing.T) {
	// Unknown / unrecognised FinishReason strings (e.g. "stop" coming
	// from a tool-result row, or future provider-specific values) must
	// NOT trigger end of session — conservative default. Tested with
	// lock alive to isolate from signal (b).
	sess := session.Session{ID: "s1"}
	for _, r := range []message.FinishReason{
		message.FinishReason("stop"), // <-- the actual real-world bug
		message.FinishReason(""),
		message.FinishReasonUnknown,
		message.FinishReason("some_future_reason"),
	} {
		msg := message.Message{
			ID:    "m1",
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.Finish{Reason: r, Partial: false}},
		}
		done, _ := isSessionFinishedFromState(sess, nil, []message.Message{msg}, nil, true)
		assert.False(t, done, "non-terminal reason %q must not end the watch", r)
	}
}

func TestIsSessionFinishedFromState_SignalC_PartialFinishIsNotEnd(t *testing.T) {
	// Streaming agents emit Partial=true Finish parts mid-stream.
	sess := session.Session{ID: "s1"}
	msg := message.Message{
		ID:   "m1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.Finish{Reason: message.FinishReasonEndTurn, Partial: true},
		},
	}
	done, reason := isSessionFinishedFromState(sess, nil, []message.Message{msg}, nil, true)
	assert.False(t, done, "Partial=true Finish parts must not terminate")
	assert.Equal(t, "", reason)
}

func TestIsSessionFinishedFromState_SignalC_ToolMessageFinishIsIgnored(t *testing.T) {
	// THE bug this whole patch was written for: a tool-result row
	// (Role=Tool) carries a Finish part with Reason="stop" because the
	// tool subprocess exited. The watch must NOT mistake that for end of
	// session. Tested with lock alive so signal (b) is out of the way.
	sess := session.Session{ID: "s1"}
	msgs := []message.Message{
		{
			ID:    "m1",
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.Finish{Reason: message.FinishReasonToolUse, Partial: false}},
		},
		{
			ID:   "m2",
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "tc1", Name: "bash", Content: "done"},
				message.Finish{Reason: message.FinishReason("stop"), Partial: false},
			},
		},
	}
	done, reason := isSessionFinishedFromState(sess, nil, msgs, nil, true)
	assert.False(t, done, "tool-result Finish (even with reason=stop) must be ignored")
	assert.Equal(t, "", reason)
}

func TestIsSessionFinishedFromState_SignalC_ScansBackPastToolMessages(t *testing.T) {
	// If the latest message is a Tool but the latest ASSISTANT before
	// it has a terminal Finish, that still counts as end of session
	// (with lock dead). The walk-backwards logic must find it.
	sess := session.Session{ID: "s1"}
	msgs := []message.Message{
		{
			ID:    "m1",
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.Finish{Reason: message.FinishReasonEndTurn, Partial: false}},
		},
		{
			ID:   "m2",
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "tc1", Name: "bash", Content: "ok"},
				message.Finish{Reason: message.FinishReason("stop"), Partial: false},
			},
		},
	}
	done, reason := isSessionFinishedFromState(sess, nil, msgs, nil, false)
	assert.True(t, done, "must walk back past trailing Tool message to the Assistant end_turn")
	assert.Equal(t, "end_turn", reason)
}

func TestIsSessionFinishedFromState_NoSignals(t *testing.T) {
	// Live session: row has no EndedReason, lock alive, no Finish part
	// yet. The loop must keep polling.
	sess := session.Session{ID: "s1"}
	msg := message.Message{ID: "m1", Role: message.Assistant}
	done, reason := isSessionFinishedFromState(sess, nil, []message.Message{msg}, nil, true)
	assert.False(t, done)
	assert.Equal(t, "", reason)
}

func TestIsSessionFinishedFromState_TransientErrorsWithLiveLockDoNotTerminate(t *testing.T) {
	// A DB hiccup on either Sessions.Get or Messages.List must NOT end
	// the watch loop while the process is alive — it should keep
	// polling and try again next tick.
	sess := session.Session{}
	done, reason := isSessionFinishedFromState(sess, errors.New("db down"), nil, errors.New("db down"), true)
	assert.False(t, done, "transient DB errors with live lock must not terminate")
	assert.Equal(t, "", reason)
}

func TestIsSessionFinishedFromState_SignalAWinsOverOthers(t *testing.T) {
	// EndedReason is the authoritative end label. When both are set
	// (lock dead) it wins over a parallel terminal Finish part.
	sess := session.Session{ID: "s1", EndedReason: "cancelled"}
	msg := message.Message{
		ID:   "m1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.Finish{Reason: message.FinishReasonEndTurn, Partial: false},
		},
	}
	done, reason := isSessionFinishedFromState(sess, nil, []message.Message{msg}, nil, false)
	assert.True(t, done)
	assert.Equal(t, "cancelled", reason)
}

func TestIsTerminalFinishReason(t *testing.T) {
	terminal := []message.FinishReason{
		message.FinishReasonEndTurn,
		message.FinishReasonMaxTokens,
		message.FinishReasonCanceled,
		message.FinishReasonError,
	}
	for _, r := range terminal {
		assert.True(t, isTerminalFinishReason(r), "%q must be terminal", r)
	}
	nonTerminal := []message.FinishReason{
		message.FinishReasonToolUse,
		message.FinishReasonUnknown,
		message.FinishReason(""),
		message.FinishReason("stop"),
		message.FinishReason("future_reason_we_dont_know_yet"),
	}
	for _, r := range nonTerminal {
		assert.False(t, isTerminalFinishReason(r), "%q must NOT be terminal", r)
	}
}

func TestFormatWatchSummary_Full(t *testing.T) {
	created := time.Date(2026, 5, 26, 14, 0, 0, 0, time.UTC)
	now := created.Add(3*time.Minute + 45*time.Second)
	sess := session.Session{
		ID:               "abc-123",
		Title:            "fix windows kill",
		PromptTokens:     12345,
		CompletionTokens: 678,
		Cost:             0.1234,
		CreatedAt:        created.Unix(),
	}
	out := formatWatchSummary(sess, "stop", now)
	assert.Contains(t, out, "--- session ended ---")
	assert.Contains(t, out, "id:       abc-123")
	assert.Contains(t, out, "title:    fix windows kill")
	assert.Contains(t, out, "reason:   stop")
	assert.Contains(t, out, "duration: 3m45s")
	assert.Contains(t, out, "tokens:   13,023 (prompt 12,345 + completion 678)")
	assert.Contains(t, out, "cost:     $0.1234")
	assert.NotContains(t, out, "budget", "no budget set → no budget line segment")
	// Sanity: starts with a blank-line separator so it reads cleanly after
	// the live message stream.
	assert.True(t, strings.HasPrefix(out, "\n"), "must lead with blank line")
}

func TestFormatWatchSummary_WithBudget(t *testing.T) {
	sess := session.Session{
		ID:            "s1",
		Cost:          0.05,
		BudgetMaxCost: 1.0,
	}
	out := formatWatchSummary(sess, "max_cost", time.Now())
	assert.Contains(t, out, "cost:     $0.0500 / $1.0000 budget")
}

func TestFormatWatchSummary_NoTitle(t *testing.T) {
	sess := session.Session{ID: "s1"}
	out := formatWatchSummary(sess, "stop", time.Now())
	assert.NotContains(t, out, "title:", "empty title must be omitted entirely")
	assert.Contains(t, out, "id:       s1")
}

func TestFormatWatchSummary_NoCreatedAt(t *testing.T) {
	// Session with CreatedAt == 0 (e.g. a synthetic / unreal session)
	// should not panic on time.Unix and should print a 0s duration.
	sess := session.Session{ID: "s1", CreatedAt: 0}
	out := formatWatchSummary(sess, "stop", time.Now())
	assert.Contains(t, out, "duration: 0s")
}

func TestFormatWatchInt(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
		{1000000000, "1,000,000,000"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, formatWatchInt(c.in), "input=%d", c.in)
	}
}

func TestFormatAge(t *testing.T) {
	// formatAge lives in sessions_watch.go and is shared with sessions_pick;
	// a regression here would break both the picker's "ago" column and any
	// future caller. Boundary cases at 60s and 3600s.
	assert.Equal(t, "0s", formatAge(0))
	assert.Equal(t, "30s", formatAge(30*time.Second))
	assert.Equal(t, "59s", formatAge(59*time.Second))
	assert.Equal(t, "1m0s", formatAge(60*time.Second))
	assert.Equal(t, "5m30s", formatAge(5*time.Minute+30*time.Second))
	assert.Equal(t, "1h0m", formatAge(time.Hour))
	assert.Equal(t, "2h15m", formatAge(2*time.Hour+15*time.Minute))
	assert.Equal(t, "48h0m", formatAge(48*time.Hour))
}
