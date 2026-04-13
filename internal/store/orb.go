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
	CreatedAt  time.Time
	ClosedAt   *time.Time
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

// CreateORBTrade inserts a new ORB trade record.
func CreateORBTrade(ctx context.Context, db *pgxpool.Pool, trade ORBTrade) error {
	_, err := db.Exec(ctx, `
		INSERT INTO orb_trades (session_id, ticker, side, entry_stop, stop_loss, take_profit, qty, order_id, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		trade.SessionID, trade.Ticker, trade.Side,
		trade.EntryStop, trade.StopLoss, trade.TakeProfit,
		trade.Qty, nullableString(trade.OrderID), trade.Status,
	)
	return err
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
func FillORBTradeByOrderID(ctx context.Context, db *pgxpool.Pool, orderID string, fillPrice float64) error {
	_, err := db.Exec(ctx, `
		UPDATE orb_trades
		SET status='filled', fill_price=$1, closed_at=NOW()
		WHERE order_id=$2 AND status='pending'`,
		fillPrice, orderID)
	return err
}

// UpdateORBTrade updates a trade's status, fill price, and PnL.
func UpdateORBTrade(ctx context.Context, db *pgxpool.Pool, id, status string, fillPrice *float64, pnl *float64) error {
	_, err := db.Exec(ctx, `
		UPDATE orb_trades
		SET status=$1, fill_price=$2, pnl=$3,
		    closed_at = CASE WHEN $1 IN ('filled','cancelled','expired') THEN NOW() ELSE closed_at END
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
		       created_at, closed_at
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
			&t.CreatedAt, &t.ClosedAt,
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
		       created_at, closed_at
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
			&t.CreatedAt, &t.ClosedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
