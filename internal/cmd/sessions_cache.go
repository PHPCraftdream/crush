package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/spf13/cobra"
)

var sessionsCacheCmd = &cobra.Command{
	Use:   "cache [session-id]",
	Short: "Show prompt-cache effectiveness and token breakdown",
	Long: `Show token usage and prompt-cache effectiveness, per model or per day.

With a session id, reports that one session. With no argument, aggregates
across ALL sessions - use --since to bound the period and --by to choose the
grouping.

Token accounting is recorded per assistant message and split into three
DISJOINT classes, so the prompt size is their sum:

  INPUT   fresh tokens billed at the full input rate
  READ    tokens served from the provider's prompt cache (much cheaper)
  WRITE   tokens written INTO the cache (slightly dearer than plain input)

  HIT     read / (input + read + write)

Grouping is by the model that ACTUALLY PRODUCED each message, so a session
that switched models mid-conversation is split correctly across them. This is
what "crush sessions cost" cannot do: it groups by the session's current model
and its TOKENS column sums last-snapshot session counters rather than real
totals.

Two things this command deliberately refuses to do, because a confident wrong
number is worse than an absent one:

  * HIT prints "n/a", never 0%, when the provider does not report caching -
    a fabricated zero is indistinguishable from a genuine cache miss.
  * Every table states its COVERAGE when some messages have no usage
    recorded. Messages written before per-message tracking existed are
    excluded from the sums rather than counted as zero, and a ratio computed
    over a fraction of the data is never presented as the whole period's.

"--by day" prints no HIT column at all: a single day can span providers whose
cache visibility differs, and blending those into one ratio would mean
nothing. Use "--by model" when the hit rate is the question.

Rows whose usage was ESTIMATED (the provider sent none, so counts were derived
from message lengths) are flagged rather than blended in silently.`,
	Example: `
# Cache effectiveness for one session (short hash works)
crush sessions cache a1b2c3d

# Across every session, grouped by the model that produced each message
crush sessions cache --by model

# Last week, day by day
crush sessions cache --since 7d --by day

# Machine-readable; cache_hit_ratio is null when it cannot be stated
crush sessions cache --since 30d --json | jq '.by_model[]'
  `,
	Args: cobra.MaximumNArgs(1),
	RunE: sessionsCacheCmdRun,
}

// timeRange is the [since, until] window (Unix seconds) an aggregate covers.
// A zero-length --since means "everything ever recorded".
type timeRange struct {
	since int64
	until int64
}

func resolveTimeRange(sinceStr string) (timeRange, error) {
	r := timeRange{since: 0, until: math.MaxInt64}
	if sinceStr == "" {
		return r, nil
	}
	d, err := parseSinceDuration(sinceStr)
	if err != nil {
		return timeRange{}, fmt.Errorf("--since: %w", err)
	}
	r.since = time.Now().Add(-d).Unix()
	return r, nil
}

// cacheRowJSON is the --json shape. Ratios are emitted as nullable so a
// consumer can tell "not applicable" from zero without re-deriving the rule.
type cacheRowJSON struct {
	Provider            string   `json:"provider"`
	Model               string   `json:"model"`
	Messages            int64    `json:"messages"`
	Estimated           int64    `json:"estimated"`
	InputTokens         int64    `json:"input_tokens"`
	CacheReadTokens     int64    `json:"cache_read_tokens"`
	CacheCreationTokens int64    `json:"cache_creation_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	ReasoningTokens     int64    `json:"reasoning_tokens"`
	PromptTokens        int64    `json:"prompt_tokens"`
	TotalTokens         int64    `json:"total_tokens"`
	CostUSD             float64  `json:"cost_usd"`
	CacheSupport        string   `json:"cache_support"`
	CacheHitRatio       *float64 `json:"cache_hit_ratio"`
}

type cacheReportJSON struct {
	SessionID    string         `json:"session_id"`
	ByModel      []cacheRowJSON `json:"by_model"`
	Total        cacheRowJSON   `json:"total"`
	MissingUsage int64          `json:"messages_missing_usage"`
	Coverage     *float64       `json:"coverage"`
}

func toCacheRow(u message.TokenUsage, messages, estimated int64) cacheRowJSON {
	row := cacheRowJSON{
		Provider:            u.Provider,
		Model:               u.Model,
		Messages:            messages,
		Estimated:           estimated,
		InputTokens:         u.InputTokens,
		CacheReadTokens:     u.CacheReadTokens,
		CacheCreationTokens: u.CacheCreationTokens,
		OutputTokens:        u.OutputTokens,
		ReasoningTokens:     u.ReasoningTokens,
		PromptTokens:        u.PromptTokens(),
		TotalTokens:         u.TotalTokens,
		CostUSD:             u.CostUSD,
		CacheSupport:        string(u.CacheSupport),
	}
	if ratio, ok := u.CacheHitRatio(); ok {
		row.CacheHitRatio = &ratio
	}
	return row
}

func sessionsCacheCmdRun(cmd *cobra.Command, args []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")
	sinceStr, _ := cmd.Flags().GetString("since")
	by, _ := cmd.Flags().GetString("by")

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	ctx := cmd.Context()

	rng, err := resolveTimeRange(sinceStr)
	if err != nil {
		return err
	}

	// With no session argument this reports across ALL sessions for the
	// window rather than opening the interactive picker: "how is the cache
	// doing lately" is a fleet question, and forcing a per-session pick would
	// make the period flags unreachable.
	if len(args) == 0 {
		switch by {
		case "day":
			return cacheByDay(ctx, a, rng, asJSON)
		case "model", "":
			return cacheByModel(ctx, a, rng, asJSON)
		default:
			return fmt.Errorf("--by: unknown grouping %q (want model or day)", by)
		}
	}

	if sinceStr != "" || by != "" {
		return fmt.Errorf("--since/--by aggregate across sessions; drop the session argument to use them")
	}

	sess, err := resolveSessionID(ctx, a.Sessions, args[0])
	if err != nil {
		return err
	}
	sessionID := sess.ID

	report, err := a.Messages.UsageBySession(ctx, sessionID)
	if err != nil {
		return err
	}

	// Largest prompt first: that is the row whose cache behaviour actually
	// moves the bill.
	sort.Slice(report.ByModel, func(i, j int) bool {
		return report.ByModel[i].Usage.PromptTokens() > report.ByModel[j].Usage.PromptTokens()
	})

	if asJSON {
		return renderCacheJSON(sessionID, report)
	}
	return renderCacheText(sessionID, report)
}

func renderCacheJSON(sessionID string, report message.UsageReport) error {
	out := cacheReportJSON{
		SessionID:    sessionID,
		ByModel:      make([]cacheRowJSON, 0, len(report.ByModel)),
		Total:        toCacheRow(report.Total(), report.Messages(), 0),
		MissingUsage: report.MissingUsage,
	}
	for _, m := range report.ByModel {
		out.ByModel = append(out.ByModel, toCacheRow(m.Usage, m.Messages, m.Estimated))
	}
	if cov, ok := report.Coverage(); ok {
		out.Coverage = &cov
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func renderCacheText(sessionID string, report message.UsageReport) error {
	if len(report.ByModel) == 0 {
		// sessionID is empty for the cross-session period views.
		if sessionID == "" {
			fmt.Println("No token usage recorded in this period.")
		} else {
			fmt.Printf("No token usage recorded for session %s.\n", short(sessionID))
		}
		if report.MissingUsage > 0 {
			fmt.Printf("%d assistant message(s) predate per-message usage tracking.\n", report.MissingUsage)
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MODEL\tMSGS\tINPUT\tREAD\tWRITE\tOUTPUT\tHIT\tCOST")

	for _, m := range report.ByModel {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t$%.4f\n",
			modelLabel(m.Usage),
			m.Messages,
			formatInt64(m.Usage.InputTokens),
			formatInt64(m.Usage.CacheReadTokens),
			formatInt64(m.Usage.CacheCreationTokens),
			formatInt64(m.Usage.OutputTokens),
			formatHitRatio(m.Usage),
			m.Usage.CostUSD,
		)
	}

	total := report.Total()
	fmt.Fprintf(w, "TOTAL\t%d\t%s\t%s\t%s\t%s\t%s\t$%.4f\n",
		report.Messages(),
		formatInt64(total.InputTokens),
		formatInt64(total.CacheReadTokens),
		formatInt64(total.CacheCreationTokens),
		formatInt64(total.OutputTokens),
		formatHitRatio(total),
		total.CostUSD,
	)
	if err := w.Flush(); err != nil {
		return err
	}

	// State what the numbers were computed over. A cache ratio derived from a
	// fraction of the session must never be presented as the session's.
	if cov, ok := report.Coverage(); ok && report.MissingUsage > 0 {
		fmt.Printf("\nCoverage: %d of %d assistant messages have usage recorded (%.0f%%);\n",
			report.Messages(), report.Messages()+report.MissingUsage, cov*100)
		fmt.Printf("%d predate per-message usage tracking and are excluded above.\n", report.MissingUsage)
	}

	var estimated int64
	for _, m := range report.ByModel {
		estimated += m.Estimated
	}
	if estimated > 0 {
		fmt.Printf("\n%d message(s) have ESTIMATED usage (the provider sent none;\n", estimated)
		fmt.Printf("counts were derived from message lengths and are approximate).\n")
	}

	return nil
}

// modelLabel renders provider/model, falling back gracefully when a row
// predates provenance being recorded.
//
// Rows are grouped by (provider, model, cache_support), so ONE model can
// legitimately produce two rows: the messages whose cache numbers are real,
// and those recorded without cache visibility (a provider that doesn't report
// it, or estimated usage). Merging them would force the whole model's hit rate
// to "n/a" and hide a genuine measurement, so they stay split — but the split
// needs to be visible, otherwise the same model appearing twice just looks
// like a bug.
func modelLabel(u message.TokenUsage) string {
	var name string
	switch {
	case u.Provider != "" && u.Model != "":
		name = u.Provider + "/" + u.Model
	case u.Model != "":
		name = u.Model
	case u.Provider != "":
		name = u.Provider
	default:
		name = "(unknown)"
	}
	if u.CacheSupport != message.CacheSupportNative {
		name += " (no cache data)"
	}
	return name
}

// formatHitRatio prints the cache-hit share, or "n/a" when the provider does
// not report caching. Never prints 0% for an unanswerable case.
func formatHitRatio(u message.TokenUsage) string {
	ratio, ok := u.CacheHitRatio()
	if !ok {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", ratio*100)
}

// ── Cross-session period views ───────────────────────────────────────────────

func cacheByModel(ctx context.Context, a *app.App, rng timeRange, asJSON bool) error {
	report, err := a.Messages.UsageByModelInRange(ctx, rng.since, rng.until)
	if err != nil {
		return err
	}
	sort.Slice(report.ByModel, func(i, j int) bool {
		return report.ByModel[i].Usage.PromptTokens() > report.ByModel[j].Usage.PromptTokens()
	})
	if asJSON {
		return renderCacheJSON("", report)
	}
	return renderCacheText("", report)
}

// dayRowJSON is the --by day --json shape. CacheHitRatio is intentionally
// absent: a day bucket spans models whose cache visibility differs, so a
// blended ratio would be meaningless (see message.UsageByDayInRange).
type dayRowJSON struct {
	Day                 string  `json:"day"`
	Messages            int64   `json:"messages"`
	Estimated           int64   `json:"estimated"`
	InputTokens         int64   `json:"input_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CostUSD             float64 `json:"cost_usd"`
}

func cacheByDay(ctx context.Context, a *app.App, rng timeRange, asJSON bool) error {
	days, err := a.Messages.UsageByDayInRange(ctx, rng.since, rng.until)
	if err != nil {
		return err
	}

	if asJSON {
		rows := make([]dayRowJSON, 0, len(days))
		for _, d := range days {
			rows = append(rows, dayRowJSON{
				Day: d.Day, Messages: d.Messages, Estimated: d.Estimated,
				InputTokens:         d.Usage.InputTokens,
				CacheReadTokens:     d.Usage.CacheReadTokens,
				CacheCreationTokens: d.Usage.CacheCreationTokens,
				OutputTokens:        d.Usage.OutputTokens,
				CostUSD:             d.Usage.CostUSD,
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	if len(days) == 0 {
		fmt.Println("(no token usage recorded in this period)")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DAY\tMSGS\tINPUT\tREAD\tWRITE\tOUTPUT\tCOST")
	var total message.TokenUsage
	var msgs int64
	for _, d := range days {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t$%.4f\n",
			d.Day, d.Messages,
			formatInt64(d.Usage.InputTokens),
			formatInt64(d.Usage.CacheReadTokens),
			formatInt64(d.Usage.CacheCreationTokens),
			formatInt64(d.Usage.OutputTokens),
			d.Usage.CostUSD)
		total = total.Add(d.Usage)
		msgs += d.Messages
	}
	fmt.Fprintf(w, "TOTAL\t%d\t%s\t%s\t%s\t%s\t$%.4f\n",
		msgs,
		formatInt64(total.InputTokens),
		formatInt64(total.CacheReadTokens),
		formatInt64(total.CacheCreationTokens),
		formatInt64(total.OutputTokens),
		total.CostUSD)
	if err := w.Flush(); err != nil {
		return err
	}

	// No HIT column here on purpose. Stating it per day would require
	// blending providers with different cache visibility into one ratio.
	fmt.Println("\n(no hit% per day: a day can span providers whose cache reporting differs;")
	fmt.Println("use --by model for cache-hit rates)")
	return nil
}
