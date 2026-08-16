package message

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/charmbracelet/crush/internal/db"
)

// UsageReport is the analytics view over a session's recorded token usage.
type UsageReport struct {
	// ByModel holds one entry per (provider, model) that actually produced
	// messages in this session. Grouped rather than summed into a single row
	// because cache behaviour is a property of the model — averaging a
	// heavily-cached Claude turn with an uncached Gemini one produces a
	// number that describes neither.
	ByModel []ModelUsage

	// MissingUsage counts assistant messages in this session with no usage
	// recorded — rows written before this feature existed, or turns whose
	// usage write failed. Report it alongside any ratio: a cache-hit figure
	// computed over 3 of 400 messages is not a session statistic.
	MissingUsage int64
}

// DayUsage is one calendar day's totals, for the period view.
type DayUsage struct {
	// Day is a local-time "YYYY-MM-DD" string, matching the format
	// `sessions cost --by day` already prints.
	Day       string
	Usage     TokenUsage
	Messages  int64
	Estimated int64
}

// ModelUsage is one (provider, model) group's totals.
type ModelUsage struct {
	Usage TokenUsage
	// Messages is how many messages contributed to Usage.
	Messages int64
	// Estimated is how many of those had synthesized rather than
	// provider-reported numbers (see TokenUsage.Estimated).
	Estimated int64
}

// Total sums every group. CacheSupport degrades and Provider/Model clear when
// groups disagree, so a mixed total cannot present a cache ratio derived from
// partial visibility — see TokenUsage.Add.
func (r UsageReport) Total() TokenUsage {
	var total TokenUsage
	for _, m := range r.ByModel {
		total = total.Add(m.Usage)
	}
	return total
}

// Messages is the number of messages the report covers.
func (r UsageReport) Messages() int64 {
	var n int64
	for _, m := range r.ByModel {
		n += m.Messages
	}
	return n
}

// Coverage is the share of assistant messages that have usage recorded, in
// [0,1]. ok is false when there are no assistant messages at all, so callers
// render "n/a" rather than a meaningless 0% or a divide-by-zero.
func (r UsageReport) Coverage() (ratio float64, ok bool) {
	covered := r.Messages()
	total := covered + r.MissingUsage
	if total == 0 {
		return 0, false
	}
	return float64(covered) / float64(total), true
}

func (s *service) SetUsage(ctx context.Context, id string, usage TokenUsage) error {
	// Refuse to write an all-zero row: it would be stored as a measured zero
	// and become indistinguishable from a real reading, quietly inflating
	// coverage while contributing nothing.
	if usage.IsZero() {
		return nil
	}

	estimated := int64(0)
	if usage.Estimated {
		estimated = 1
	}

	err := s.q.UpdateMessageUsage(ctx, db.UpdateMessageUsageParams{
		ID:                  id,
		InputTokens:         nullInt64(usage.InputTokens),
		OutputTokens:        nullInt64(usage.OutputTokens),
		ReasoningTokens:     nullInt64(usage.ReasoningTokens),
		CacheCreationTokens: nullInt64(usage.CacheCreationTokens),
		CacheReadTokens:     nullInt64(usage.CacheReadTokens),
		// total_tokens is the column the analytics queries use to tell
		// "recorded" from "never written", so it must always be non-NULL on a
		// row we did record — even if the provider reported no total and the
		// derived value happens to be 0.
		TotalTokens:    sql.NullInt64{Int64: usage.TotalTokens, Valid: true},
		CostUsd:        sql.NullFloat64{Float64: usage.CostUSD, Valid: true},
		UsageProvider:  nullString(usage.Provider),
		UsageModel:     nullString(usage.Model),
		CacheSupport:   nullString(string(usage.CacheSupport)),
		UsageEstimated: sql.NullInt64{Int64: estimated, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("record usage for message %s: %w", id, err)
	}
	return nil
}

// UsageByModelInRange aggregates recorded usage across ALL sessions whose
// messages fall in [since, until], grouped by the model that produced them.
//
// since/until are Unix seconds; pass 0 and math.MaxInt64 for "everything".
func (s *service) UsageByModelInRange(ctx context.Context, since, until int64) (UsageReport, error) {
	rows, err := s.qRead.SumMessageUsageByModelInRange(ctx, db.SumMessageUsageByModelInRangeParams{
		CreatedAt:   since,
		CreatedAt_2: until,
	})
	if err != nil {
		return UsageReport{}, fmt.Errorf("aggregate usage by model: %w", err)
	}

	report := UsageReport{ByModel: make([]ModelUsage, 0, len(rows))}
	for _, r := range rows {
		report.ByModel = append(report.ByModel, ModelUsage{
			Messages:  r.Recorded,
			Estimated: r.Estimated,
			Usage: TokenUsage{
				InputTokens:         r.InputTokens,
				OutputTokens:        r.OutputTokens,
				ReasoningTokens:     r.ReasoningTokens,
				CacheCreationTokens: r.CacheCreationTokens,
				CacheReadTokens:     r.CacheReadTokens,
				TotalTokens:         r.TotalTokens,
				CostUSD:             r.CostUsd,
				Provider:            r.Provider,
				Model:               r.Model,
				CacheSupport:        CacheSupport(r.CacheSupport),
				Estimated:           r.Estimated > 0,
			},
		})
	}

	missing, err := s.qRead.CountMessagesMissingUsageInRange(ctx, db.CountMessagesMissingUsageInRangeParams{
		CreatedAt:   since,
		CreatedAt_2: until,
	})
	if err != nil {
		return UsageReport{}, fmt.Errorf("count messages missing usage: %w", err)
	}
	report.MissingUsage = missing

	return report, nil
}

// UsageByDayInRange is UsageByModelInRange bucketed by local calendar day.
//
// The per-day rows carry no Provider/Model: a day can span several models, and
// labelling the bucket with one of them would be a lie. CacheSupport is left
// unset for the same reason, which makes CacheHitRatio decline to answer —
// deliberately, since a day's blended cache ratio across providers with
// different cache visibility is not a meaningful number. Use the by-model
// view when the ratio is what matters.
func (s *service) UsageByDayInRange(ctx context.Context, since, until int64) ([]DayUsage, error) {
	rows, err := s.qRead.SumMessageUsageByDayInRange(ctx, db.SumMessageUsageByDayInRangeParams{
		CreatedAt:   since,
		CreatedAt_2: until,
	})
	if err != nil {
		return nil, fmt.Errorf("aggregate usage by day: %w", err)
	}

	out := make([]DayUsage, 0, len(rows))
	for _, r := range rows {
		out = append(out, DayUsage{
			Day:       r.Day,
			Messages:  r.Recorded,
			Estimated: r.Estimated,
			Usage: TokenUsage{
				InputTokens:         r.InputTokens,
				OutputTokens:        r.OutputTokens,
				ReasoningTokens:     r.ReasoningTokens,
				CacheCreationTokens: r.CacheCreationTokens,
				CacheReadTokens:     r.CacheReadTokens,
				TotalTokens:         r.TotalTokens,
				CostUSD:             r.CostUsd,
				Estimated:           r.Estimated > 0,
			},
		})
	}
	return out, nil
}

func (s *service) UsageBySession(ctx context.Context, sessionID string) (UsageReport, error) {
	rows, err := s.qRead.SumMessageUsageBySession(ctx, sessionID)
	if err != nil {
		return UsageReport{}, fmt.Errorf("aggregate usage for session %s: %w", sessionID, err)
	}

	report := UsageReport{ByModel: make([]ModelUsage, 0, len(rows))}
	for _, r := range rows {
		report.ByModel = append(report.ByModel, ModelUsage{
			Messages:  r.Recorded,
			Estimated: r.Estimated,
			Usage: TokenUsage{
				InputTokens:         r.InputTokens,
				OutputTokens:        r.OutputTokens,
				ReasoningTokens:     r.ReasoningTokens,
				CacheCreationTokens: r.CacheCreationTokens,
				CacheReadTokens:     r.CacheReadTokens,
				TotalTokens:         r.TotalTokens,
				CostUSD:             r.CostUsd,
				Provider:            r.Provider,
				Model:               r.Model,
				CacheSupport:        CacheSupport(r.CacheSupport),
				Estimated:           r.Estimated > 0,
			},
		})
	}

	missing, err := s.qRead.CountMessagesMissingUsage(ctx, sessionID)
	if err != nil {
		return UsageReport{}, fmt.Errorf("count messages missing usage for session %s: %w", sessionID, err)
	}
	report.MissingUsage = missing

	return report, nil
}

// nullInt64 stores 0 as a real 0 rather than NULL: within a row we did record,
// zero is a measurement ("no cache hits this turn"), and collapsing it to NULL
// would make it read as "never recorded".
func nullInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: true}
}

func nullString(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}
