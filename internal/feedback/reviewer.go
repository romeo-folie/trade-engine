package feedback

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// Reviewer runs the nightly feedback computation job.
type Reviewer struct {
	cfg            config.FeedbackConfig
	bus            *events.Bus
	db             *pgxpool.Pool
	now            func() time.Time
	lastReviewDate time.Time // tracks which day was last reviewed
}

// NewReviewer creates a new Reviewer.
func NewReviewer(cfg config.FeedbackConfig, bus *events.Bus, db *pgxpool.Pool) *Reviewer {
	return &Reviewer{
		cfg: cfg,
		bus: bus,
		db:  db,
		now: time.Now,
	}
}

// Run starts the review ticker. It blocks until ctx is cancelled.
func (r *Reviewer) Run(ctx context.Context) error {
	parts := strings.SplitN(r.cfg.ReviewTimeUTC, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("feedback: invalid review_time_utc %q (expected HH:MM)", r.cfg.ReviewTimeUTC)
	}
	reviewHour, err := strconv.Atoi(parts[0])
	if err != nil {
		return fmt.Errorf("feedback: invalid review_time_utc hour: %w", err)
	}
	reviewMin, err := strconv.Atoi(parts[1])
	if err != nil {
		return fmt.Errorf("feedback: invalid review_time_utc minute: %w", err)
	}

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			now := r.now().UTC()
			today := now.Truncate(24 * time.Hour)
			reviewedToday := r.lastReviewDate.Equal(today)
			passedReviewTime := now.Hour() > reviewHour ||
				(now.Hour() == reviewHour && now.Minute() >= reviewMin)
			if passedReviewTime && !reviewedToday {
				if err := r.runReview(ctx); err != nil {
					// Log but don't return — keep running so the next day works.
					fmt.Printf("feedback: nightly review error: %v\n", err)
				}
				r.lastReviewDate = today
			}
		}
	}
}

// tradeStats aggregates win-rate and average PnL% for a group of trades.
type tradeStats struct {
	wins       int
	total      int
	totalPnlPct float64
}

func (s *tradeStats) add(pnl float64, pnlPct float64) {
	s.total++
	s.totalPnlPct += pnlPct
	if pnl > 0 {
		s.wins++
	}
}

func (s *tradeStats) winRate() float64 {
	if s.total == 0 {
		return 0
	}
	return float64(s.wins) / float64(s.total)
}

func (s *tradeStats) avgPnlPct() float64 {
	if s.total == 0 {
		return 0
	}
	return s.totalPnlPct / float64(s.total)
}

// getClosedTradesInRange queries the trades table for closed trades in [from, to).
func getClosedTradesInRange(ctx context.Context, from, to time.Time) ([]store.Trade, error) {
	rows, err := store.GetDB().Query(ctx, `
		SELECT id, COALESCE(signal_id::text,''), ticker, direction, strategy,
		       entry_price, COALESCE(exit_price,0),
		       quantity, COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(pnl,0), COALESCE(pnl_pct,0), status,
		       COALESCE(model_provider,''), COALESCE(model_name,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at
		FROM trades
		WHERE status = 'closed' AND closed_at >= $1 AND closed_at < $2
		ORDER BY closed_at DESC`,
		from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.Trade
	for rows.Next() {
		var t store.Trade
		if err := rows.Scan(
			&t.ID, &t.SignalID, &t.Ticker, &t.Direction, &t.Strategy,
			&t.EntryPrice, &t.ExitPrice,
			&t.Quantity, &t.StopLoss, &t.TakeProfit,
			&t.PnL, &t.PnLPct, &t.Status,
			&t.ModelProvider, &t.ModelName,
			&t.OpenedAt, &t.ClosedAt, &t.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// getFastPathedCountForTrades returns how many trades in the slice had their
// analysis NOT fast_pathed (i.e. the arbiter was actually invoked).
func getArbiterInvokedCountForTrades(ctx context.Context, trades []store.Trade) (arbitrated int, total int, err error) {
	// Collect signal IDs from trades that have one.
	var signalIDs []string
	for _, t := range trades {
		if t.SignalID != "" {
			signalIDs = append(signalIDs, t.SignalID)
		}
	}
	total = len(signalIDs)
	if total == 0 {
		return 0, 0, nil
	}

	rows, err := store.GetDB().Query(ctx, `
		SELECT COALESCE(a.fast_pathed, false)
		FROM signals s
		JOIN analyses a ON a.id = ANY(s.analysis_ids)
		WHERE s.id = ANY($1)`,
		signalIDs,
	)
	if err != nil {
		return 0, total, err
	}
	defer rows.Close()

	for rows.Next() {
		var fp bool
		if err := rows.Scan(&fp); err != nil {
			return 0, total, err
		}
		if !fp {
			arbitrated++
		}
	}
	return arbitrated, total, rows.Err()
}

// windowLabel converts a window integer (days) to a label like "1d", "7d", "30d".
func windowLabel(days int) string {
	return fmt.Sprintf("%dd", days)
}

// upsert is a small helper to reduce repetition in runReview.
func upsert(ctx context.Context, date time.Time, scope, scopeKey, metric, window string, value float64, tradeCount int) error {
	return store.UpsertFeedbackInsight(ctx, store.FeedbackInsight{
		Date:       date,
		Scope:      scope,
		ScopeKey:   scopeKey,
		Metric:     metric,
		Value:      value,
		Window:     window,
		TradeCount: tradeCount,
	})
}

// runReview executes the full nightly feedback computation for all configured windows.
func (r *Reviewer) runReview(ctx context.Context) error {
	now := r.now().UTC()
	today := now.Truncate(24 * time.Hour)

	var allInsights []store.FeedbackInsight

	for _, windowDays := range r.cfg.Windows {
		label := windowLabel(windowDays)
		from := today.AddDate(0, 0, -windowDays)
		to := now

		// ── Standard trades ──────────────────────────────────────────────────
		trades, err := getClosedTradesInRange(ctx, from, to)
		if err != nil {
			return fmt.Errorf("feedback: getClosedTradesInRange window=%s: %w", label, err)
		}

		// Per-model stats keyed by "provider/model".
		modelStats := make(map[string]*tradeStats)
		// Per-instrument stats keyed by ticker.
		instrStats := make(map[string]*tradeStats)

		for _, t := range trades {
			modelKey := t.ModelProvider + "/" + t.ModelName
			if modelStats[modelKey] == nil {
				modelStats[modelKey] = &tradeStats{}
			}
			modelStats[modelKey].add(t.PnL, t.PnLPct)

			if instrStats[t.Ticker] == nil {
				instrStats[t.Ticker] = &tradeStats{}
			}
			instrStats[t.Ticker].add(t.PnL, t.PnLPct)
		}

		// Upsert model metrics.
		for modelKey, s := range modelStats {
			ins := []struct {
				metric string
				value  float64
			}{
				{"win_rate", s.winRate()},
				{"avg_pnl_pct", s.avgPnlPct()},
				{"trade_count", float64(s.total)},
			}
			for _, m := range ins {
				insight := store.FeedbackInsight{
					Date:       today,
					Scope:      "model",
					ScopeKey:   modelKey,
					Metric:     m.metric,
					Value:      m.value,
					Window:     label,
					TradeCount: s.total,
				}
				if err := store.UpsertFeedbackInsight(ctx, insight); err != nil {
					return fmt.Errorf("feedback: upsert model insight %s/%s: %w", modelKey, m.metric, err)
				}
				allInsights = append(allInsights, insight)
			}
		}

		// Upsert instrument metrics.
		for ticker, s := range instrStats {
			ins := []struct {
				metric string
				value  float64
			}{
				{"win_rate", s.winRate()},
				{"avg_pnl_pct", s.avgPnlPct()},
				{"trade_count", float64(s.total)},
			}
			for _, m := range ins {
				insight := store.FeedbackInsight{
					Date:       today,
					Scope:      "instrument",
					ScopeKey:   ticker,
					Metric:     m.metric,
					Value:      m.value,
					Window:     label,
					TradeCount: s.total,
				}
				if err := store.UpsertFeedbackInsight(ctx, insight); err != nil {
					return fmt.Errorf("feedback: upsert instrument insight %s/%s: %w", ticker, m.metric, err)
				}
				allInsights = append(allInsights, insight)
			}
		}

		// Arbiter override rate.
		arbitrated, arbTotal, err := getArbiterInvokedCountForTrades(ctx, trades)
		if err != nil {
			return fmt.Errorf("feedback: arbiter count window=%s: %w", label, err)
		}
		var overrideRate float64
		if arbTotal > 0 {
			overrideRate = float64(arbitrated) / float64(arbTotal)
		}
		arbInsight := store.FeedbackInsight{
			Date:       today,
			Scope:      "system",
			ScopeKey:   "arbiter",
			Metric:     "arbiter_override_rate",
			Value:      overrideRate,
			Window:     label,
			TradeCount: arbTotal,
		}
		if err := store.UpsertFeedbackInsight(ctx, arbInsight); err != nil {
			return fmt.Errorf("feedback: upsert arbiter insight window=%s: %w", label, err)
		}
		allInsights = append(allInsights, arbInsight)

		// ── ORB trades ───────────────────────────────────────────────────────
		orbTrades, err := store.GetClosedORBTradesByDateRange(ctx, r.db, from, to)
		if err != nil {
			return fmt.Errorf("feedback: GetClosedORBTradesByDateRange window=%s: %w", label, err)
		}

		// Group ORB trades by ticker.
		orbStats := make(map[string]*tradeStats)
		for _, ot := range orbTrades {
			if orbStats[ot.Ticker] == nil {
				orbStats[ot.Ticker] = &tradeStats{}
			}
			orbStats[ot.Ticker].add(ot.PnL, 0) // ORBTrade has no PnLPct field
		}

		for ticker, s := range orbStats {
			ins := []struct {
				metric string
				value  float64
			}{
				{"win_rate", s.winRate()},
				{"avg_pnl_pct", s.avgPnlPct()},
				{"trade_count", float64(s.total)},
			}
			for _, m := range ins {
				insight := store.FeedbackInsight{
					Date:       today,
					Scope:      "orb_session",
					ScopeKey:   ticker,
					Metric:     m.metric,
					Value:      m.value,
					Window:     label,
					TradeCount: s.total,
				}
				if err := store.UpsertFeedbackInsight(ctx, insight); err != nil {
					return fmt.Errorf("feedback: upsert orb insight %s/%s: %w", ticker, m.metric, err)
				}
				allInsights = append(allInsights, insight)
			}
		}
	}

	// Check for threshold violations and raise flags.
	return checkFlags(ctx, r.bus, r.cfg.Thresholds, today, allInsights)
}

// suppress unused import warning — upsert helper is used inline
var _ = upsert
