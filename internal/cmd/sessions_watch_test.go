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

func TestIsSessionFinishedFromState_SignalA_EndedReason(t *testing.T) {
	sess := session.Session{ID: "s1", EndedReason: "max_cost"}
	done, reason := isSessionFinishedFromState(sess, nil, nil, nil, true)
	assert.True(t, done, "EndedReason set must terminate")
	assert.Equal(t, "max_cost", reason, "reason must equal EndedReason")
}

func TestIsSessionFinishedFromState_SignalB_LockGoneWithMessages(t *testing.T) {
	sess := session.Session{ID: "s1"} // no EndedReason
	msgs := []message.Message{{ID: "m1"}}
	done, reason := isSessionFinishedFromState(sess, nil, msgs, nil, false)
	assert.True(t, done)
	assert.Equal(t, "lock_released", reason)
}

func TestIsSessionFinishedFromState_SignalB_LockGoneButNoMessagesYet(t *testing.T) {
	// Guard against racing the acquirer that has not yet written its first
	// message: lock missing + zero messages must NOT terminate.
	sess := session.Session{ID: "s1"}
	done, reason := isSessionFinishedFromState(sess, nil, nil, nil, false)
	assert.False(t, done, "lock gone but no messages yet must not terminate (race guard)")
	assert.Equal(t, "", reason)
}

func TestIsSessionFinishedFromState_SignalC_FinishPart(t *testing.T) {
	sess := session.Session{ID: "s1"}
	msg := message.Message{
		ID:   "m1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.Finish{Reason: message.FinishReasonEndTurn, Partial: false},
		},
	}
	done, reason := isSessionFinishedFromState(sess, nil, []message.Message{msg}, nil, true)
	assert.True(t, done)
	assert.Equal(t, "end_turn", reason)
}

func TestIsSessionFinishedFromState_SignalC_PartialFinishIsNotEnd(t *testing.T) {
	// Streaming agents emit Partial=true Finish parts mid-stream; those
	// must not be treated as termination.
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

func TestIsSessionFinishedFromState_NoSignals(t *testing.T) {
	// Live session: row has no EndedReason, lock exists, no Finish part
	// yet. The loop must keep polling.
	sess := session.Session{ID: "s1"}
	msg := message.Message{ID: "m1", Role: message.Assistant}
	done, reason := isSessionFinishedFromState(sess, nil, []message.Message{msg}, nil, true)
	assert.False(t, done)
	assert.Equal(t, "", reason)
}

func TestIsSessionFinishedFromState_TransientErrorsDoNotTerminate(t *testing.T) {
	// A DB hiccup on either Sessions.Get or Messages.List must NOT end
	// the watch loop — it should keep polling and try again next tick.
	sess := session.Session{}
	done, reason := isSessionFinishedFromState(sess, errors.New("db down"), nil, errors.New("db down"), true)
	assert.False(t, done, "transient DB errors must not terminate")
	assert.Equal(t, "", reason)
}

func TestIsSessionFinishedFromState_SignalAWinsOverOthers(t *testing.T) {
	// If both EndedReason and FinishPart are set, EndedReason wins —
	// it's the authoritative end label written by the agent.
	sess := session.Session{ID: "s1", EndedReason: "cancelled"}
	msg := message.Message{
		ID:   "m1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.Finish{Reason: message.FinishReasonEndTurn, Partial: false},
		},
	}
	done, reason := isSessionFinishedFromState(sess, nil, []message.Message{msg}, nil, true)
	assert.True(t, done)
	assert.Equal(t, "cancelled", reason)
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
