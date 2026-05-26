package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ORBSession mirrors a row from the orb_sessions table.
type ORBSession struct {
	ID          string
	Date        time.Time
	Status      string
	SessionName string
	Candidates  json.RawMessage
	Selected    []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ORBTrade mirrors a row from the orb_trades table.
type ORBTrade struct {
	ID         string
	SessionID  string
	Ticker     string
	Side       string
	EntryStop  float64
	StopLoss   float64
	TakeProfit float64
	Qty        int
	OrderID    string
	Status     string
	FillPrice  float64
	PnL        float64
	// Exit tracking — populated after the TP or stop-loss leg fills.
	TpOrderID string
	SlOrderID string
	ExitPrice float64
	ExitType  string // "tp" | "stop" | "expired"
	CreatedAt time.Time
	// FilledAt is the entry-leg fill time. The exit reconciler uses it as the
	// lower bound when matching exit fills. ClosedAt is set only once the trade
	// actually closes (TP/stop/expiry), so it stays nil while a trade is open.
	FilledAt *time.Time
	ClosedAt *time.Time
}

// CreateORBSession inserts a new ORB session for the given date and session name.
func CreateORBSession(ctx context.Context, db *pgxpool.Pool, date time.Time, sessionName string) (*ORBSession, error) {
	var s ORBSession
	err := db.QueryRow(ctx, `
		INSERT INTO orb_sessions (date, session_name) VALUES ($1, $2)
		RETURNING id, date, status, session_name, candidates, selected, created_at, updated_at`,
		date, sessionName,
	).Scan(&s.ID, &s.Date, &s.Status, &s.SessionName, &s.Candidates, &s.Selected, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// UpdateORBSession updates an ORB session's status and optionally its candidates
// and selected tickers. When candidates or selected are nil, the existing DB values
// are preserved via COALESCE — preventing audit data from being erased on completion.
func UpdateORBSession(ctx context.Context, db *pgxpool.Pool, id, status string, candidates json.RawMessage, selected []string) error {
	_, err := db.Exec(ctx, `
		UPDATE orb_sessions
		SET status     = $1,
		    candidates = COALESCE($2::jsonb, candidates),
		    selected   = COALESCE($3, selected),
		    updated_at = NOW()
		WHERE id = $4`,
		status, candidates, selected, id)
	return err
}

// MarkStaleORBSessionsDone marks any in-progress sessions for the given session name
// whose date is earlier than the cutoff date as "done", and simultaneously expires any
// pending trades in those sessions. Both updates run in a single transaction so the
// session and trade state remain consistent.
func MarkStaleORBSessionsDone(ctx context.Context, db *pgxpool.Pool, cutoffDate time.Time, sessionName string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		WITH swept AS (
			UPDATE orb_sessions
			SET status = 'done', updated_at = NOW()
			WHERE status IN ('executing', 'range_watch', 'candidate_ready', 'scanning')
			  AND date < $1::date
			  AND session_name = $2
			RETURNING id
		)
		UPDATE orb_trades
		SET status     = 'expired',
		    exit_type  = 'expired',
		    closed_at  = COALESCE(closed_at, NOW())
		WHERE session_id IN (SELECT id FROM swept)
		  AND status = 'pending'`,
		cutoffDate.Format("2006-01-02"), sessionName)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetORBSessionByDate retrieves the ORB session for a given date and session name.
// Returns nil, nil when no session exists for that date/name combination.
func GetORBSessionByDate(ctx context.Context, db *pgxpool.Pool, date time.Time, sessionName string) (*ORBSession, error) {
	var s ORBSession
	err := db.QueryRow(ctx, `
		SELECT id, date, status, session_name, candidates, selected, created_at, updated_at
		FROM orb_sessions WHERE date=$1 AND session_name=$2`, date, sessionName,
	).Scan(&s.ID, &s.Date, &s.Status, &s.SessionName, &s.Candidates, &s.Selected, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// CreateORBTrade inserts a new ORB trade record and returns the generated row ID.
func CreateORBTrade(ctx context.Context, db *pgxpool.Pool, trade ORBTrade) (string, error) {
	var id string
	err := db.QueryRow(ctx, `
		INSERT INTO orb_trades (session_id, ticker, side, entry_stop, stop_loss, take_profit, qty, order_id, status, tp_order_id, sl_order_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id`,
		trade.SessionID, trade.Ticker, trade.Side,
		trade.EntryStop, trade.StopLoss, trade.TakeProfit,
		trade.Qty, nullableString(trade.OrderID), trade.Status,
		nullableString(trade.TpOrderID), nullableString(trade.SlOrderID),
	).Scan(&id)
	return id, err
}

// CancelORBTradeByOrderID marks a pending trade as cancelled given its broker order ID.
// Used by tickDone to update the DB after best-effort cancel calls.
func CancelORBTradeByOrderID(ctx context.Context, db *pgxpool.Pool, orderID string) error {
	_, err := db.Exec(ctx, `
		UPDATE orb_trades
		SET status='cancelled', closed_at=NOW()
		WHERE order_id=$1 AND status='pending'`,
		orderID)
	return err
}

// FillORBTradeByOrderID marks a pending trade as filled given its broker order ID.
// Returns true if a row was actually updated (i.e. the trade was pending).
// filled_at records the entry-leg fill time; closed_at stays NULL until the trade
// actually exits via CloseORBTrade.
func FillORBTradeByOrderID(ctx context.Context, db *pgxpool.Pool, orderID string, fillPrice float64) (bool, error) {
	tag, err := db.Exec(ctx, `
		UPDATE orb_trades
		SET status='filled', fill_price=$1, filled_at=NOW()
		WHERE order_id=$2 AND status='pending'`,
		fillPrice, orderID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CloseORBTrade records the outcome of an ORB trade once its exit leg (TP or stop)
// fills. exitType is "tp", "stop", or "expired". pnl is in quote-currency units.
// The WHERE clause guards on status='filled' so concurrent reconcilers cannot
// overwrite an already-closed trade.
func CloseORBTrade(ctx context.Context, db *pgxpool.Pool, id, exitType string, exitPrice, pnl float64) error {
	_, err := db.Exec(ctx, `
		UPDATE orb_trades
		SET status='closed', exit_type=$1, exit_price=$2, pnl=$3, closed_at=NOW()
		WHERE id=$4 AND status='filled'`,
		exitType, exitPrice, pnl, id)
	return err
}

// UpdateORBTrade updates a trade's status, fill price, and PnL. closed_at is set
// only for terminal-without-fill transitions ('cancelled','expired'); 'filled' is
// excluded so it never stamps closed_at on a still-open trade (that would break the
// reconciler's lower-bound logic and the closed_at invariant). Entry fills go
// through FillORBTradeByOrderID, which sets filled_at instead.
func UpdateORBTrade(ctx context.Context, db *pgxpool.Pool, id, status string, fillPrice *float64, pnl *float64) error {
	_, err := db.Exec(ctx, `
		UPDATE orb_trades
		SET status=$1, fill_price=$2, pnl=$3,
		    closed_at = CASE WHEN $1 IN ('cancelled','expired') THEN NOW() ELSE closed_at END
		WHERE id=$4`,
		status, fillPrice, pnl, id)
	return err
}

// GetRecentORBSessions returns up to limit ORB sessions ordered by date descending.
func GetRecentORBSessions(ctx context.Context, db *pgxpool.Pool, limit int) ([]ORBSession, error) {
	rows, err := db.Query(ctx, `
		SELECT id, date, status, session_name, candidates, selected, created_at, updated_at
		FROM orb_sessions ORDER BY date DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ORBSession
	for rows.Next() {
		var s ORBSession
		if err := rows.Scan(
			&s.ID, &s.Date, &s.Status, &s.SessionName, &s.Candidates, &s.Selected,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetRecentORBTrades returns up to limit ORB trades ordered by created_at descending.
func GetRecentORBTrades(ctx context.Context, db *pgxpool.Pool, limit int) ([]ORBTrade, error) {
	rows, err := db.Query(ctx, `
		SELECT id, session_id, ticker, side,
		       COALESCE(entry_stop,0), COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(qty,0), COALESCE(order_id,''), status,
		       COALESCE(fill_price,0), COALESCE(pnl,0),
		       COALESCE(tp_order_id,''), COALESCE(sl_order_id,''),
		       COALESCE(exit_price,0), COALESCE(exit_type,''),
		       created_at, filled_at, closed_at
		FROM orb_trades ORDER BY created_at DESC LIMIT $1`, limit)
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
			&t.TpOrderID, &t.SlOrderID,
			&t.ExitPrice, &t.ExitType,
			&t.CreatedAt, &t.FilledAt, &t.ClosedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetORBTrades returns all trades for a given session.
func GetORBTrades(ctx context.Context, db *pgxpool.Pool, sessionID string) ([]ORBTrade, error) {
	rows, err := db.Query(ctx, `
		SELECT id, session_id, ticker, side,
		       COALESCE(entry_stop,0), COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(qty,0), COALESCE(order_id,''), status,
		       COALESCE(fill_price,0), COALESCE(pnl,0),
		       COALESCE(tp_order_id,''), COALESCE(sl_order_id,''),
		       COALESCE(exit_price,0), COALESCE(exit_type,''),
		       created_at, filled_at, closed_at
		FROM orb_trades WHERE session_id=$1
		ORDER BY created_at`, sessionID)
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
			&t.TpOrderID, &t.SlOrderID,
			&t.ExitPrice, &t.ExitType,
			&t.CreatedAt, &t.FilledAt, &t.ClosedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
