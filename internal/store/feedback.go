package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FeedbackInsight mirrors a row from the feedback_insights table.
type FeedbackInsight struct {
	ID         string
	Date       time.Time
	Scope      string // "model" | "instrument" | "orb_session" | "system"
	ScopeKey   string // e.g. "anthropic/claude-sonnet", "EUR/USD:london"
	Metric     string // "win_rate", "avg_pnl_pct", "trade_count", "arbiter_override_rate"
	Value      float64
	Window     string // "1d" | "7d" | "30d"
	TradeCount int
	CreatedAt  time.Time
}

// FeedbackFlag mirrors a row from the feedback_flags table.
type FeedbackFlag struct {
	ID          string
	Date        time.Time
	Scope       string
	ScopeKey    string
	Severity    string // "info" | "warning" | "critical"
	Message     string
	Metric      string
	MetricValue float64
	Threshold   float64
	Resolved    bool
	ResolvedAt  *time.Time
	CreatedAt   time.Time
}

// UpsertFeedbackInsight inserts or updates a feedback insight row keyed on
// (date, scope, scope_key, metric, window).
func UpsertFeedbackInsight(ctx context.Context, ins FeedbackInsight) error {
	_, err := GetDB().Exec(ctx, `
		INSERT INTO feedback_insights (date, scope, scope_key, metric, value, time_window, trade_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (date, scope, scope_key, metric, time_window)
		DO UPDATE SET value = EXCLUDED.value, trade_count = EXCLUDED.trade_count, created_at = NOW()`,
		ins.Date, ins.Scope, ins.ScopeKey, ins.Metric, ins.Value, ins.Window, ins.TradeCount,
	)
	return err
}

// InsertFeedbackFlag creates a new feedback flag and returns its UUID.
func InsertFeedbackFlag(ctx context.Context, flag FeedbackFlag) (string, error) {
	var id string
	err := GetDB().QueryRow(ctx, `
		INSERT INTO feedback_flags (date, scope, scope_key, severity, message, metric, metric_value, threshold)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		flag.Date, flag.Scope, flag.ScopeKey, flag.Severity,
		flag.Message, flag.Metric, flag.MetricValue, flag.Threshold,
	).Scan(&id)
	return id, err
}

// GetFeedbackInsights returns up to limit insights for the given scope, scopeKey, and window,
// ordered by date descending.
func GetFeedbackInsights(ctx context.Context, scope, scopeKey, window string, limit int) ([]FeedbackInsight, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, date, scope, scope_key, metric, value, time_window, trade_count, created_at
		FROM feedback_insights
		WHERE scope = $1 AND scope_key = $2 AND time_window = $3
		ORDER BY date DESC
		LIMIT $4`,
		scope, scopeKey, window, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFeedbackInsights(rows)
}

// GetFreshORBRiskMultiplier returns the most recent auto-throttle risk multiplier the nightly
// reviewer stored for an ORB instrument over the given window — but ONLY if it was written on
// or after notBefore — plus whether one was found. The freshness bound matters because the
// reviewer skips writing below its min-trades gate, so an old stored value would otherwise
// keep sizing live trades after the rolling window has too few trades. A missing (or stale)
// value means the throttle has no current opinion, so the caller falls back to a neutral 1.0.
func GetFreshORBRiskMultiplier(ctx context.Context, ticker, window string, notBefore time.Time) (float64, bool, error) {
	var v float64
	// The `date` column is a DATE; compare it to notBefore's UTC CALENDAR day so a caller
	// passing a wall-clock timestamp (with a time-of-day) doesn't drop a row written on the
	// boundary day. Anchoring to UTC keeps it independent of the DB session timezone.
	err := GetDB().QueryRow(ctx, `
		SELECT value FROM feedback_insights
		WHERE scope='orb_session' AND scope_key=$1 AND metric='risk_multiplier' AND time_window=$2
		  AND date >= ($3 AT TIME ZONE 'UTC')::date
		ORDER BY date DESC
		LIMIT 1`,
		ticker, window, notBefore,
	).Scan(&v)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return v, true, nil
}

// GetLatestFeedbackInsights returns all insights for a scope+window on a specific date.
// Used by the nightly reviewer to check computed results.
func GetLatestFeedbackInsights(ctx context.Context, scope, window string, date time.Time) ([]FeedbackInsight, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, date, scope, scope_key, metric, value, time_window, trade_count, created_at
		FROM feedback_insights
		WHERE scope = $1 AND time_window = $2 AND date = $3`,
		scope, window, date,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFeedbackInsights(rows)
}

// GetActiveFlags returns up to limit unresolved feedback flags ordered by creation time descending.
func GetActiveFlags(ctx context.Context, limit int) ([]FeedbackFlag, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, date, scope, scope_key, severity, message, metric, metric_value, threshold,
		       resolved, resolved_at, created_at
		FROM feedback_flags
		WHERE resolved = false
		ORDER BY created_at DESC
		LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFeedbackFlags(rows)
}

// ResolveFlag marks a feedback flag as resolved.
func ResolveFlag(ctx context.Context, id string) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE feedback_flags SET resolved = true, resolved_at = NOW() WHERE id = $1`,
		id,
	)
	return err
}

// HasUnresolvedFlag reports whether an unresolved flag already exists for the
// given date, scope, scopeKey, and metric combination.
func HasUnresolvedFlag(ctx context.Context, date time.Time, scope, scopeKey, metric string) (bool, error) {
	var exists bool
	err := GetDB().QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM feedback_flags
			WHERE date = $1 AND scope = $2 AND scope_key = $3 AND metric = $4 AND resolved = false
		)`,
		date, scope, scopeKey, metric,
	).Scan(&exists)
	return exists, err
}

// GetClosedORBTradesByDateRange returns ORB trades that closed (TP/stop/expiry,
// status 'closed') with their exit leg in [from, to), as keyed by closed_at.
// These are the trades with a realized PnL, used by the feedback reviewer for
// win-rate and avg-PnL stats. Filled-but-open trades are deliberately excluded —
// they have no outcome yet and their closed_at is NULL. Takes an explicit pool
// param following the ORB store convention.
func GetClosedORBTradesByDateRange(ctx context.Context, db *pgxpool.Pool, from, to time.Time) ([]ORBTrade, error) {
	rows, err := db.Query(ctx, `
		SELECT id, session_id, ticker, side,
		       COALESCE(entry_stop,0), COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(qty,0), COALESCE(order_id,''), status,
		       COALESCE(fill_price,0), COALESCE(pnl,0),
		       created_at, closed_at,
		       COALESCE(scale_out_role,''), scale_out_parent_id
		FROM orb_trades
		WHERE status = 'closed' AND closed_at >= $1 AND closed_at < $2
		ORDER BY closed_at DESC`,
		from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ORBTrade
	for rows.Next() {
		var t ORBTrade
		if err := rows.Scan(
			&t.ID, &t.SessionID, &t.Ticker, &t.Side,
			&t.EntryStop, &t.StopLoss, &t.TakeProfit,
			&t.Qty, &t.OrderID, &t.Status,
			&t.FillPrice, &t.PnL,
			&t.CreatedAt, &t.ClosedAt,
			&t.ScaleOutRole, &t.ScaleOutParentID,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanFeedbackInsights(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]FeedbackInsight, error) {
	var out []FeedbackInsight
	for rows.Next() {
		var ins FeedbackInsight
		if err := rows.Scan(
			&ins.ID, &ins.Date, &ins.Scope, &ins.ScopeKey,
			&ins.Metric, &ins.Value, &ins.Window, &ins.TradeCount,
			&ins.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, ins)
	}
	return out, rows.Err()
}

func scanFeedbackFlags(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]FeedbackFlag, error) {
	var out []FeedbackFlag
	for rows.Next() {
		var f FeedbackFlag
		if err := rows.Scan(
			&f.ID, &f.Date, &f.Scope, &f.ScopeKey,
			&f.Severity, &f.Message, &f.Metric, &f.MetricValue, &f.Threshold,
			&f.Resolved, &f.ResolvedAt, &f.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
