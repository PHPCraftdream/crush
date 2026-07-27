package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/session"
)

// printSubAgentTranscripts renders the messages of every direct sub-agent
// child session of parentID, each as its own clearly-demarcated, indented
// block. It is the shared implementation behind the `--with-subagents` opt-in
// flag on `sessions show` / `last` / `tail`.
//
// This is deliberately OPT-IN. By default those commands print only the
// top-level session's own message stream (plus the one-line #89 sub-agent
// pulse note) — a full child transcript is never dumped inline unless the
// operator explicitly asks for it. When asked, each delegation's own messages
// are rendered under a bracketed header so they can never be confused with the
// parent's own stream:
//
//	┌── sub-agent delegation (session abc12345) ──
//	│ [assistant]
//	│ …child messages, indented…
//	└── end sub-agent delegation (session abc12345) ──
//
// Rendering reuses the same printMessage machinery as the parent stream (so
// tool-call/result previews look identical), writing to an indenting writer so
// every child line is visually offset. Cost: one ListSubSessions on the parent
// plus one Messages.List per child — the same shape as computeCallTreeActivity.
func printSubAgentTranscripts(ctx context.Context, w io.Writer, a *app.App, parentID, format string, now time.Time) {
	if a == nil || parentID == "" {
		return
	}
	children, err := a.Sessions.ListSubSessions(ctx, parentID)
	if err != nil || len(children) == 0 {
		return
	}
	for _, child := range children {
		printOneSubAgentTranscript(ctx, w, a, child, format, now)
	}
}

// printOneSubAgentTranscript renders a single child session's messages as a
// demarcated block. Text format wraps the block in a bracketed header/footer
// and indents every message line; ndjson format tags each row with the child
// session id via a "sub_agent_session" prefix line so machine consumers can
// still segment the stream. Nested sub-agents (a child of a child) are rendered
// recursively so a two-level delegation tree is fully visible.
func printOneSubAgentTranscript(ctx context.Context, w io.Writer, a *app.App, child session.Session, format string, now time.Time) {
	msgs, err := a.Messages.List(ctx, child.ID)
	if err != nil {
		return
	}
	hash := short(session.HashID(child.ID))

	if format == "ndjson" {
		// Structured segmentation marker so ndjson consumers can attribute the
		// following rows to the child session without guessing.
		fmt.Fprintf(w, "{\"sub_agent_session\":%q}\n", child.ID)
		callCtx := buildToolCallContext(msgs)
		for i, msg := range msgs {
			printMessage(w, msg, format, callCtx, i < len(msgs)-1)
		}
		// Recurse into deeper delegations.
		printSubAgentTranscripts(ctx, w, a, child.ID, format, now)
		return
	}

	title := child.Title
	if title == "" {
		title = "sub-agent"
	}
	fmt.Fprintf(w, "┌── sub-agent delegation: %s (session %s) ──\n", title, hash)
	iw := &indentWriter{w: w, prefix: []byte("│ ")}
	callCtx := buildToolCallContext(msgs)
	if len(msgs) == 0 {
		fmt.Fprintf(iw, "(no messages)\n")
	}
	for i, msg := range msgs {
		printMessageWithTime(iw, msg, format, now, callCtx, i < len(msgs)-1)
	}
	// Recurse into deeper delegations, still inside this block's indent.
	printSubAgentTranscripts(ctx, iw, a, child.ID, format, now)
	fmt.Fprintf(w, "└── end sub-agent delegation (session %s) ──\n\n", hash)
}

// indentWriter prefixes every line it writes with a fixed prefix, so a nested
// block of output is visually offset from the parent stream. It tracks whether
// the previous byte was a newline so the prefix is emitted exactly once at the
// start of each line (including the first) and never doubled on a trailing
// newline.
type indentWriter struct {
	w        io.Writer
	prefix   []byte
	midLine  bool // true once we've written the prefix for the current line
	writeErr error
}

func (iw *indentWriter) Write(p []byte) (int, error) {
	if iw.writeErr != nil {
		return 0, iw.writeErr
	}
	total := len(p)
	for len(p) > 0 {
		if !iw.midLine {
			if _, err := iw.w.Write(iw.prefix); err != nil {
				iw.writeErr = err
				return total - len(p), err
			}
			iw.midLine = true
		}
		// Find the next newline.
		nl := -1
		for i, b := range p {
			if b == '\n' {
				nl = i
				break
			}
		}
		if nl < 0 {
			if _, err := iw.w.Write(p); err != nil {
				iw.writeErr = err
				return total - len(p), err
			}
			p = nil
			break
		}
		// Write through the newline, then reset for the next line.
		if _, err := iw.w.Write(p[:nl+1]); err != nil {
			iw.writeErr = err
			return total - len(p), err
		}
		iw.midLine = false
		p = p[nl+1:]
	}
	return total, nil
}
