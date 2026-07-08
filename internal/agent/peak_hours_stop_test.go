package agent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPeakHoursStoppedFinishText(t *testing.T) {
	t.Run("generic error falls back gracefully", func(t *testing.T) {
		underlying := errors.New("provider zai is in peak hours (08:00–12:00), refusing until 12:00")
		msg, details := peakHoursStoppedFinishText(underlying)

		if msg == "" {
			t.Fatal("message must be non-empty — empty message looks identical to a voluntary finish")
		}
		if details == "" {
			t.Fatal("details must be non-empty")
		}
		if !strings.Contains(details, underlying.Error()) {
			t.Errorf("details %q must include the underlying checkPeakHours error verbatim (provider/window/reopen time)", details)
		}
		if !strings.Contains(strings.ToLower(details), "resume") {
			t.Errorf("details %q should instruct the orchestrator to schedule a resume", details)
		}
		if !strings.Contains(strings.ToLower(details), "not a crash") {
			t.Errorf("details %q should clarify this is an intentional stop, not a crash", details)
		}
	})

	t.Run("PeakHoursError yields an exact, cron-ready resume timestamp", func(t *testing.T) {
		reopensAt := time.Date(2026, 7, 8, 12, 0, 0, 0, time.Local)
		err := &PeakHoursError{ProviderID: "zai", Start: "08:00", End: "12:00", ReopensAt: reopensAt}
		_, details := peakHoursStoppedFinishText(err)

		if !strings.Contains(details, "RESUME AT") {
			t.Fatalf("details %q must contain an explicit RESUME AT line an orchestrator can act on", details)
		}
		if !strings.Contains(details, reopensAt.Format(time.RFC3339)) {
			t.Errorf("details %q must contain the exact RFC3339 reopen timestamp for unambiguous cron scheduling, want %s", details, reopensAt.Format(time.RFC3339))
		}
	})
}
