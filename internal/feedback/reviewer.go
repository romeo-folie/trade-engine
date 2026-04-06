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

// getArbiterInvokedCountForTrades returns how many unique signals in the slice had their
// analysis NOT fast_pathed (i.e. the arbiter was actually invoked), counting per signal
// rather than per analysis row to avoid double-counting.
func getArbiterInvokedCountForTrades(ctx context.Context, trades []store.Trade) (arbitrated int, total int, err error) {
	// Collect signal IDs from trades that have one.
	var signalIDs []string
	for _, t := range trades {
		if t.SignalID != "" {
			signalIDs = append(signalIDs, t.SignalID)
		}
	}
	if len(signalIDs) == 0 {
		return 0, 0, nil
	}

	// Count unique signals, not individual analysis rows, to avoid inflating counts
	// when a signal has multiple analysis_ids.
	err = store.GetDB().QueryRow(ctx, `
		SELECT COUNT(DISTINCT s.id) FILTER (WHERE a.fast_pathed = false) AS arbitrated,
		       COUNT(DISTINCT s.id) AS total
		FROM signals s
		JOIN trades t ON t.signal_id = s.id
		JOIN LATERAL unnest(s.analysis_ids) AS aid ON TRUE
		JOIN analyses a ON a.id = aid
		WHERE t.status = 'closed' AND s.id = ANY($1)`,
		signalIDs,
	).Scan(&arbitrated, &total)
	if err != nil {
		return 0, 0, err
	}
	return arbitrated, total, nil
}

// windowLabel converts a window integer (days) to a label like "1d", "7d", "30d".
func windowLabel(days int) string {
	return fmt.Sprintf("%dd", days)
}

// upsert persists a feedback insight and returns it for accumulation in allInsights.
func upsert(ctx context.Context, date time.Time, scope, scopeKey, metric, window string, value float64, tradeCount int) (store.FeedbackInsight, error) {
	ins := store.FeedbackInsight{
		Date:       date,
		Scope:      scope,
		ScopeKey:   scopeKey,
		Metric:     metric,
		Value:      value,
		Window:     window,
		TradeCount: tradeCount,
	}
	return ins, store.UpsertFeedbackInsight(ctx, ins)
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
			for _, m := range []struct {
				metric string
				value  float64
			}{
				{"win_rate", s.winRate()},
				{"avg_pnl_pct", s.avgPnlPct()},
				{"trade_count", float64(s.total)},
			} {
				ins, err := upsert(ctx, today, "model", modelKey, m.metric, label, m.value, s.total)
				if err != nil {
					return fmt.Errorf("feedback: upsert model insight %s/%s: %w", modelKey, m.metric, err)
				}
				allInsights = append(allInsights, ins)
			}
		}

		// Upsert instrument metrics.
		for ticker, s := range instrStats {
			for _, m := range []struct {
				metric string
				value  float64
			}{
				{"win_rate", s.winRate()},
				{"avg_pnl_pct", s.avgPnlPct()},
				{"trade_count", float64(s.total)},
			} {
				ins, err := upsert(ctx, today, "instrument", ticker, m.metric, label, m.value, s.total)
				if err != nil {
					return fmt.Errorf("feedback: upsert instrument insight %s/%s: %w", ticker, m.metric, err)
				}
				allInsights = append(allInsights, ins)
			}
		}

		// Arbiter invoked rate: fraction of signals where the arbiter was actually
		// consulted (fast_pathed=false). This is an "invoked" rate, not an "override"
		// rate — it reflects how often the arbiter runs, not how often it disagrees.
		arbitrated, arbTotal, err := getArbiterInvokedCountForTrades(ctx, trades)
		if err != nil {
			return fmt.Errorf("feedback: arbiter count window=%s: %w", label, err)
		}
		var invokedRate float64
		if arbTotal > 0 {
			invokedRate = float64(arbitrated) / float64(arbTotal)
		}
		arbIns, err := upsert(ctx, today, "system", "arbiter", "arbiter_invoked_rate", label, invokedRate, arbTotal)
		if err != nil {
			return fmt.Errorf("feedback: upsert arbiter insight window=%s: %w", label, err)
		}
		allInsights = append(allInsights, arbIns)

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
			// ORBTrade has no PnLPct field — compute it from fill price and quantity.
			var pnlPct float64
			if ot.FillPrice > 0 && ot.EntryStop > 0 {
				notional := ot.FillPrice * float64(ot.Qty)
				if notional > 0 {
					pnlPct = ot.PnL / notional * 100
				}
			}
			orbStats[ot.Ticker].add(ot.PnL, pnlPct)
		}

		for ticker, s := range orbStats {
			for _, m := range []struct {
				metric string
				value  float64
			}{
				{"win_rate", s.winRate()},
				{"avg_pnl_pct", s.avgPnlPct()},
				{"trade_count", float64(s.total)},
			} {
				ins, err := upsert(ctx, today, "orb_session", ticker, m.metric, label, m.value, s.total)
				if err != nil {
					return fmt.Errorf("feedback: upsert orb insight %s/%s: %w", ticker, m.metric, err)
				}
				allInsights = append(allInsights, ins)
			}
		}
	}

	// Check for threshold violations and raise flags.
	return checkFlags(ctx, r.bus, r.cfg.Thresholds, today, allInsights)
}
