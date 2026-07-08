package agent

import (
	"errors"
	"fmt"
	"time"
)

// peakHoursGuidance returns the orchestrator-facing instruction paragraph
// for ANY peak-hours refusal — both the immediate "refused at the start of
// a new invocation" case (checkPeakHours' pre-flight call sites) and the
// "force-stopped an already-in-flight turn" case (the OnStepFinish
// mid-turn re-check). Both must carry the same guidance: this is not a
// crash, crush will not retry on its own, and an orchestrating agent
// should schedule a resume for the exact reopen time rather than retry
// immediately.
//
// err is whatever checkPeakHours returned. When it is (or wraps) a
// *PeakHoursError we pull the exact ReopensAt time.Time out of it — via
// errors.As, not string parsing — so the instruction gives an
// orchestrating agent an unambiguous, cron-ready timestamp instead of
// making it parse "refusing until 12:00" out of prose. Falls back to a
// generic RESUME AT line if the type assertion fails (defensive: keeps
// working even if the error ever gets wrapped differently upstream).
func peakHoursGuidance(err error) string {
	resumeLine := "RESUME AT: (see the time above)"
	var pe *PeakHoursError
	if errors.As(err, &pe) {
		resumeLine = fmt.Sprintf(
			"RESUME AT: %s (local time, RFC3339: %s)",
			pe.ReopensAt.Format("2006-01-02 15:04"),
			pe.ReopensAt.Format(time.RFC3339),
		)
	}
	return fmt.Sprintf(
		"%s\n\n"+
			"This is not a crash — crush is intentionally refusing/halting because "+
			"of the provider's peak-hours window. crush is exiting now; it will "+
			"not retry on its own.\n\n"+
			"If an orchestrating agent is driving this session: schedule a resume "+
			"for RESUME AT above (e.g. CronCreate / a scheduled wakeup) and "+
			"re-invoke `crush run --session <this session>` then — do not retry "+
			"immediately, and do not pass --allow-peak-hours unless a human "+
			"operator explicitly asked for that specific run.",
		resumeLine,
	)
}

// wrapPeakHoursError appends peakHoursGuidance to a checkPeakHours error
// while preserving errors.Is/errors.As (via %w) — used at every immediate
// pre-flight refusal call site (buildCall, runInternal, Summarize,
// runSubAgent) so a brand-new invocation refused before any work starts
// carries the same orchestrator instructions as a mid-turn stop, instead of
// the operator/orchestrator seeing a terse one-line error that looks like a
// plain crash. Returns nil unchanged.
func wrapPeakHoursError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w\n\n%s", err, peakHoursGuidance(err))
}

// peakHoursStoppedFinishText builds the (msg, details) pair recorded as the
// Finish part when a session is force-stopped mid-turn because its
// provider's peak_hours window opened while the turn was already in
// flight. checkPeakHours is normally only checked once, at the start of a
// turn — this is the correction that also stops an ALREADY-RUNNING turn as
// soon as the window opens, rather than letting it run to completion.
func peakHoursStoppedFinishText(err error) (msg, details string) {
	msg = "Stopped: provider entered its peak-hours window mid-session"
	details = fmt.Sprintf("%s\n\n%s", err.Error(), peakHoursGuidance(err))
	return msg, details
}
