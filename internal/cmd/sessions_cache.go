package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/spf13/cobra"
)

var sessionsCacheCmd = &cobra.Command{
	Use:   "cache [session-id]",
	Short: "Show prompt-cache effectiveness and token breakdown",
	Long: `Show how well the prompt cache is working, per model.

Token accounting is recorded per assistant message and split into three
DISJOINT classes, so the prompt size is their sum:

  input   fresh tokens billed at the full input rate
  read    tokens served from the provider's prompt cache (much cheaper)
  write   tokens written INTO the cache (slightly more expensive than input)

"hit" is read / (input + read + write).

Coverage is reported alongside every figure. Messages written before this
feature existed carry no usage, and a ratio computed over a handful of a
session's messages is not the session's ratio - the command says so rather
than implying full coverage.

Providers that do not report caching show "n/a" instead of 0%, because a
fabricated zero is indistinguishable from a real cache miss.`,
	Example: `
# Cache effectiveness for one session (short hash works)
crush sessions cache a1b2c3d

# Pick a session interactively
crush sessions cache

# Machine-readable
crush sessions cache a1b2c3d --json
  `,
	Args: cobra.MaximumNArgs(1),
	RunE: sessionsCacheCmdRun,
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

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	ctx := cmd.Context()

	var sessionID string
	if len(args) == 1 {
		sess, err := resolveSessionID(ctx, a.Sessions, args[0])
		if err != nil {
			return err
		}
		sessionID = sess.ID
	} else {
		// Same interactive picker `sessions watch` uses, so both commands
		// present an identical session list.
		picked, err := pickSessionForWatch(ctx, a)
		if err != nil {
			return err
		}
		sessionID = picked
	}

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
		fmt.Printf("No token usage recorded for session %s.\n", short(sessionID))
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
func modelLabel(u message.TokenUsage) string {
	switch {
	case u.Provider != "" && u.Model != "":
		return u.Provider + "/" + u.Model
	case u.Model != "":
		return u.Model
	case u.Provider != "":
		return u.Provider
	default:
		return "(unknown)"
	}
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
