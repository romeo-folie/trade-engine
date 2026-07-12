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
	// ExitSource records how the exit was determined: "broker" (an IBKR fill) or
	// "replay" (reconstructed from market-data bars). Empty until the trade closes.
	ExitSource string
	CreatedAt  time.Time
	// FilledAt is the entry-leg fill time. The exit reconciler uses it as the
	// lower bound when matching exit fills. ClosedAt is set only once the trade
	// actually closes (TP/stop/expiry), so it stays nil while a trade is open.
	FilledAt *time.Time
	ClosedAt *time.Time
	// StopMovedAt is the time the protective stop was first moved by adaptive stop
	// management (break-even / trailing); nil means never moved (stop_loss is still the
	// original placement stop). The bar-replay backstop reads it to avoid replaying the
	// stop leg at a moved level over pre-move bars. Populated by GetOpenFilledORBTrades.
	StopMovedAt *time.Time
	// Scale-out (partial take-profit) linkage. ScaleOutRole is "runner" or "partial" for
	// the two legs of a split-bracket trade, "" for an ordinary single-bracket trade. On a
	// "partial" row, ScaleOutParentID is the id of its "runner" row (the primary leg); nil
	// otherwise. Reporting and distinct-ticker counting fold a partial into its parent so
	// the pair counts as one logical trade. See migration 036.
	ScaleOutRole     string
	ScaleOutParentID *string
	// Instrument identity. AssetClass is "equity" (default), "forex", "futures",
	// or "option". The remaining fields are populated only for options (Issue #410,
	// migration 039) and identify the specific contract; they are zero/nil for all
	// other asset classes. Multiplier is the contract multiplier (100 for standard
	// US equity options, 1 otherwise) and makes P&L instrument-correct — see ExitPnLM.
	AssetClass string
	Underlying string
	Expiry     *time.Time
	Strike     float64
	Right      string // "C" | "P"
	OCCSymbol  string
	Multiplier int
	// ExitOrderID is the broker order id of the in-flight sell-to-close, persisted
	// so restart recovery can poll it for the real fill (options only; empty until
	// an exit is submitted). Cleared implicitly when the trade closes.
	ExitOrderID string
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
	var parentID any
	if trade.ScaleOutParentID != nil && *trade.ScaleOutParentID != "" {
		parentID = *trade.ScaleOutParentID
	}
	// Instrument identity: default asset_class to 'equity' and multiplier to 1 so
	// existing callers (equity/forex/futures) that leave these zero behave exactly
	// as before. The option-specific columns are written as NULL unless set.
	assetClass := trade.AssetClass
	if assetClass == "" {
		assetClass = "equity"
	}
	multiplier := trade.Multiplier
	if multiplier == 0 {
		multiplier = 1
	}
	var strike any
	if trade.Strike != 0 {
		strike = trade.Strike
	}
	var id string
	err := db.QueryRow(ctx, `
		INSERT INTO orb_trades (session_id, ticker, side, entry_stop, stop_loss, take_profit, qty, order_id, status, tp_order_id, sl_order_id, scale_out_role, scale_out_parent_id, asset_class, underlying, expiry, strike, "right", occ_symbol, multiplier)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		RETURNING id`,
		trade.SessionID, trade.Ticker, trade.Side,
		trade.EntryStop, trade.StopLoss, trade.TakeProfit,
		trade.Qty, nullableString(trade.OrderID), trade.Status,
		nullableString(trade.TpOrderID), nullableString(trade.SlOrderID),
		nullableString(trade.ScaleOutRole), parentID,
		assetClass, nullableString(trade.Underlying), trade.Expiry,
		strike, nullableString(trade.Right), nullableString(trade.OCCSymbol), multiplier,
	).Scan(&id)
	return id, err
}

// GetORBTradeByID returns a single ORB trade with full instrument identity,
// including the option-contract columns (Issue #410). It is the canonical
// full-fidelity read for the options-ORB router, exits, and reconciler; the
// list readers select only the fields their callers need and leave the option
// columns zero. Returns (nil, nil) when no row matches.
func GetORBTradeByID(ctx context.Context, db *pgxpool.Pool, id string) (*ORBTrade, error) {
	var t ORBTrade
	err := db.QueryRow(ctx, `
		SELECT id, session_id, ticker, side,
		       COALESCE(entry_stop,0), COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(qty,0), COALESCE(order_id,''), status,
		       COALESCE(fill_price,0), COALESCE(pnl,0),
		       COALESCE(tp_order_id,''), COALESCE(sl_order_id,''),
		       COALESCE(exit_price,0), COALESCE(exit_type,''), COALESCE(exit_source,''),
		       created_at, filled_at, closed_at,
		       COALESCE(scale_out_role,''), scale_out_parent_id,
		       COALESCE(asset_class,'equity'), COALESCE(underlying,''), expiry,
		       COALESCE(strike,0), COALESCE("right",''), COALESCE(occ_symbol,''), COALESCE(multiplier,1)
		FROM orb_trades WHERE id=$1`, id,
	).Scan(
		&t.ID, &t.SessionID, &t.Ticker, &t.Side,
		&t.EntryStop, &t.StopLoss, &t.TakeProfit,
		&t.Qty, &t.OrderID, &t.Status,
		&t.FillPrice, &t.PnL,
		&t.TpOrderID, &t.SlOrderID,
		&t.ExitPrice, &t.ExitType, &t.ExitSource,
		&t.CreatedAt, &t.FilledAt, &t.ClosedAt,
		&t.ScaleOutRole, &t.ScaleOutParentID,
		&t.AssetClass, &t.Underlying, &t.Expiry,
		&t.Strike, &t.Right, &t.OCCSymbol, &t.Multiplier,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// GetOpenFilledOptionTrades returns every filled options-ORB trade (asset_class
// 'option', status 'filled') across all sessions, oldest first, with full contract
// identity. The app-managed exit watcher (#415) uses it to find live option
// positions to manage — the generic list readers omit the option columns, so this
// dedicated reader selects them.
func GetOpenFilledOptionTrades(ctx context.Context, db *pgxpool.Pool) ([]ORBTrade, error) {
	rows, err := db.Query(ctx, `
		SELECT id, session_id, ticker, side,
		       COALESCE(entry_stop,0), COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(qty,0), COALESCE(order_id,''), status,
		       COALESCE(fill_price,0), COALESCE(pnl,0),
		       COALESCE(exit_price,0), COALESCE(exit_type,''), COALESCE(exit_source,''),
		       created_at, filled_at, closed_at,
		       COALESCE(underlying,''), expiry, COALESCE(strike,0),
		       COALESCE("right",''), COALESCE(occ_symbol,''), COALESCE(multiplier,1),
		       COALESCE(exit_order_id,'')
		FROM orb_trades
		WHERE status='filled' AND asset_class='option'
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ORBTrade
	for rows.Next() {
		var t ORBTrade
		t.AssetClass = "option"
		if err := rows.Scan(
			&t.ID, &t.SessionID, &t.Ticker, &t.Side,
			&t.EntryStop, &t.StopLoss, &t.TakeProfit,
			&t.Qty, &t.OrderID, &t.Status,
			&t.FillPrice, &t.PnL,
			&t.ExitPrice, &t.ExitType, &t.ExitSource,
			&t.CreatedAt, &t.FilledAt, &t.ClosedAt,
			&t.Underlying, &t.Expiry, &t.Strike,
			&t.Right, &t.OCCSymbol, &t.Multiplier,
			&t.ExitOrderID,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetORBTradeExitOrderID persists the in-flight exit sell's broker order id on a
// trade row so restart recovery can poll it for the real fill (options #415/#416).
func SetORBTradeExitOrderID(ctx context.Context, db *pgxpool.Pool, tradeID, exitOrderID string) error {
	_, err := db.Exec(ctx, `UPDATE orb_trades SET exit_order_id=$1 WHERE id=$2`, exitOrderID, tradeID)
	return err
}

// GetManagedOptionQuantities returns the SUM of contract quantities for non-terminal
// options-ORB trades (status pending or filled) per OCC symbol. The orphan detector
// (#416) compares this against the broker-held quantity so a broker position of 3
// contracts explained by a managed row of only 1 is flagged as a 2-contract surplus,
// not treated as fully managed by OCC presence alone.
func GetManagedOptionQuantities(ctx context.Context, db *pgxpool.Pool) (map[string]float64, error) {
	rows, err := db.Query(ctx, `
		SELECT occ_symbol, COALESCE(SUM(qty),0) FROM orb_trades
		WHERE asset_class='option' AND status IN ('pending','filled') AND occ_symbol IS NOT NULL
		GROUP BY occ_symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var occ string
		var qty float64
		if err := rows.Scan(&occ, &qty); err != nil {
			return nil, err
		}
		out[occ] = qty
	}
	return out, rows.Err()
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

// FillORBTradePartialByOrderID marks a pending trade filled AND overwrites qty with
// the actually-filled contract count. Used when an option limit entry only partially
// fills: the row must reflect the real quantity, otherwise the exit sweep's
// full-quantity fallback (when the broker positions read is unavailable) could sell
// more contracts than are actually held.
func FillORBTradePartialByOrderID(ctx context.Context, db *pgxpool.Pool, orderID string, fillPrice float64, filledQty int) (bool, error) {
	tag, err := db.Exec(ctx, `
		UPDATE orb_trades
		SET status='filled', fill_price=$1, filled_at=NOW(), qty=$2
		WHERE order_id=$3 AND status='pending'`,
		fillPrice, filledQty, orderID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CloseORBTrade records the outcome of an ORB trade once its exit leg (TP or stop)
// fills. exitType is "tp" or "stop"; exitSource is "broker" (an IBKR fill) or
// "replay" (reconstructed from bars). pnl is in quote-currency units. (Pending trades
// that never entered are marked status='expired' separately via MarkStaleORBSessionsDone,
// not through this function.)
// The WHERE clause guards on status='filled' so concurrent reconcilers cannot
// overwrite an already-closed trade.
func CloseORBTrade(ctx context.Context, db *pgxpool.Pool, id, exitType, exitSource string, exitPrice, pnl float64) error {
	_, err := db.Exec(ctx, `
		UPDATE orb_trades
		SET status='closed', exit_type=$1, exit_source=$2, exit_price=$3, pnl=$4, closed_at=NOW()
		WHERE id=$5 AND status='filled'`,
		exitType, exitSource, exitPrice, pnl, id)
	return err
}

// UpdateORBTradeStopLoss records a moved stop on a still-open trade (adaptive break-even /
// trailing). Guarded on status='filled' so it never touches a closed row. The canonical
// stop_loss must track the live broker stop so the next adaptive sweep ratchets from it.
//
// It also stamps stop_moved_at with the FIRST move time (COALESCE keeps the earliest), the
// boundary after which stop_loss no longer equals the original placement stop. The
// bar-replay backstop reads stop_moved_at to stop fabricating stop-outs at a level the
// stop had not yet reached at the replayed bar's time (see migration 035).
func UpdateORBTradeStopLoss(ctx context.Context, db *pgxpool.Pool, id string, stopLoss float64, movedAt time.Time) error {
	_, err := db.Exec(ctx, `
		UPDATE orb_trades
		   SET stop_loss=$1, stop_moved_at=COALESCE(stop_moved_at, $3)
		 WHERE id=$2 AND status='filled'`,
		stopLoss, id, movedAt)
	return err
}

// CloseORBTradeAt is like CloseORBTrade but stamps closed_at with an explicit time
// rather than NOW(). The bar-replay reconciler uses it so a trade closed from a
// historical price path records the actual bar timestamp at which its TP/stop was
// touched — keeping closed_at accurate for trades resolved after the fact (e.g. on
// restart or backfill) instead of collapsing them all to the reconcile time. The
// status='filled' guard makes it idempotent against the IBKR reconcilers.
func CloseORBTradeAt(ctx context.Context, db *pgxpool.Pool, id, exitType, exitSource string, exitPrice, pnl float64, closedAt time.Time) error {
	_, err := db.Exec(ctx, `
		UPDATE orb_trades
		SET status='closed', exit_type=$1, exit_source=$2, exit_price=$3, pnl=$4, closed_at=$5
		WHERE id=$6 AND status='filled'`,
		exitType, exitSource, exitPrice, pnl, closedAt, id)
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
		       COALESCE(exit_price,0), COALESCE(exit_type,''), COALESCE(exit_source,''),
		       created_at, filled_at, closed_at,
		       COALESCE(scale_out_role,''), scale_out_parent_id
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
			&t.ExitPrice, &t.ExitType, &t.ExitSource,
			&t.CreatedAt, &t.FilledAt, &t.ClosedAt,
			&t.ScaleOutRole, &t.ScaleOutParentID,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetOpenFilledORBTrades returns every ORB trade currently in status='filled'
// (entry leg filled, exit not yet recorded) across all sessions, oldest first.
//
// This exists for the paper-mode exit monitor. A real broker holds the bracket's
// TP/SL legs server-side and fills them itself — possibly long after the session
// is "done" — and the IBKR reconcilers close the DB row from that fill. In paper
// mode there is no server-side bracket, so nothing ever fills the exit legs and
// these trades would otherwise stay 'filled' forever. The monitor drives their
// exits from live quotes, which means it must see filled trades regardless of
// which (possibly already-done) session they belong to.
func GetOpenFilledORBTrades(ctx context.Context, db *pgxpool.Pool) ([]ORBTrade, error) {
	rows, err := db.Query(ctx, `
		SELECT id, session_id, ticker, side,
		       COALESCE(entry_stop,0), COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(qty,0), COALESCE(order_id,''), status,
		       COALESCE(fill_price,0), COALESCE(pnl,0),
		       COALESCE(tp_order_id,''), COALESCE(sl_order_id,''),
		       COALESCE(exit_price,0), COALESCE(exit_type,''), COALESCE(exit_source,''),
		       created_at, filled_at, closed_at, stop_moved_at,
		       COALESCE(scale_out_role,''), scale_out_parent_id
		-- Options-ORB rows are excluded: they have their own reader
		-- (GetOpenFilledOptionTrades) and exit path (#415); this keeps the
		-- equity/forex/futures exit reconcilers from touching option trades.
		FROM orb_trades WHERE status='filled' AND COALESCE(asset_class,'equity') <> 'option'
		ORDER BY COALESCE(filled_at, created_at)`)
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
			&t.ExitPrice, &t.ExitType, &t.ExitSource,
			&t.CreatedAt, &t.FilledAt, &t.ClosedAt, &t.StopMovedAt,
			&t.ScaleOutRole, &t.ScaleOutParentID,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetOpenPendingORBTrades returns every ORB trade still awaiting a fill
// (status='pending'), across all sessions, ordered by creation. Mirrors
// GetOpenFilledORBTrades. The broker-truth fill reconciler uses it to find
// entries the broker has actually filled but GetOrderStatus could not confirm
// (the parent order id aged out of the orders+trades endpoints).
func GetOpenPendingORBTrades(ctx context.Context, db *pgxpool.Pool) ([]ORBTrade, error) {
	rows, err := db.Query(ctx, `
		SELECT id, session_id, ticker, side,
		       COALESCE(entry_stop,0), COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(qty,0), COALESCE(order_id,''), status,
		       COALESCE(fill_price,0), COALESCE(pnl,0),
		       COALESCE(tp_order_id,''), COALESCE(sl_order_id,''),
		       COALESCE(exit_price,0), COALESCE(exit_type,''), COALESCE(exit_source,''),
		       created_at, filled_at, closed_at, stop_moved_at,
		       COALESCE(scale_out_role,''), scale_out_parent_id
		-- Options-ORB rows excluded (see GetOpenFilledORBTrades): the option entry
		-- fill-poll is order-ID driven, and fill_reconcile/split-orphan sweeps are
		-- forex-scoped, so options never need this reader.
		FROM orb_trades WHERE status='pending' AND COALESCE(asset_class,'equity') <> 'option'
		ORDER BY created_at`)
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
			&t.ExitPrice, &t.ExitType, &t.ExitSource,
			&t.CreatedAt, &t.FilledAt, &t.ClosedAt, &t.StopMovedAt,
			&t.ScaleOutRole, &t.ScaleOutParentID,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetRecentlyCancelledORBTrades returns ORB trades cancelled on/after `since`.
// The broker-truth fill reconciler uses it to heal the July/today's trap: an
// entry filled at IBKR but marked 'cancelled' at session end (GetOrderStatus
// couldn't confirm the fill), leaving the position unmanaged and invisible to
// every status='filled' reconciler. When the broker still holds the matching
// position such a row is reopened to 'filled'.
func GetRecentlyCancelledORBTrades(ctx context.Context, db *pgxpool.Pool, since time.Time) ([]ORBTrade, error) {
	rows, err := db.Query(ctx, `
		SELECT id, session_id, ticker, side,
		       COALESCE(entry_stop,0), COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(qty,0), COALESCE(order_id,''), status,
		       COALESCE(fill_price,0), COALESCE(pnl,0),
		       COALESCE(tp_order_id,''), COALESCE(sl_order_id,''),
		       COALESCE(exit_price,0), COALESCE(exit_type,''), COALESCE(exit_source,''),
		       created_at, filled_at, closed_at, stop_moved_at,
		       COALESCE(scale_out_role,''), scale_out_parent_id
		FROM orb_trades WHERE status='cancelled' AND closed_at >= $1
		ORDER BY closed_at`, since.UTC())
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
			&t.ExitPrice, &t.ExitType, &t.ExitSource,
			&t.CreatedAt, &t.FilledAt, &t.ClosedAt, &t.StopMovedAt,
			&t.ScaleOutRole, &t.ScaleOutParentID,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// FillPendingORBTradeAtCreated marks a pending entry 'filled' with filled_at set
// to the row's created_at (placement time) rather than NOW(). The broker-truth
// reconciler uses it for INFERRED fills discovered well after the fact (an
// aged-out GetOrderStatus at session cleanup can be an hour past placement), so
// time-based exits anchored on filled_at don't grant a fresh max-hold window —
// unlike FillORBTradeByOrderID, which stamps NOW() for real-time fills. Returns
// whether a row changed.
func FillPendingORBTradeAtCreated(ctx context.Context, db *pgxpool.Pool, orderID string, fillPrice float64) (bool, error) {
	tag, err := db.Exec(ctx, `
		UPDATE orb_trades SET status='filled', fill_price=$1, filled_at=created_at
		WHERE order_id=$2 AND status='pending'`, fillPrice, orderID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ReopenORBTradeToFilled flips a wrongly-'cancelled' entry back to 'filled' with
// the given fill price (clearing closed_at) when the broker is found to still
// hold the position. Guarded to status='cancelled' so it never disturbs a
// genuinely closed or filled row. filled_at is set to the row's created_at (the
// order-placement time, a safe lower bound for the real fill) rather than NOW(),
// so time-based exits anchored on filled_at don't grant a hours-old position a
// fresh max-hold window. Returns whether a row changed.
func ReopenORBTradeToFilled(ctx context.Context, db *pgxpool.Pool, orderID string, fillPrice float64) (bool, error) {
	tag, err := db.Exec(ctx, `
		UPDATE orb_trades
		SET status='filled', fill_price=$1, filled_at=created_at, closed_at=NULL
		WHERE order_id=$2 AND status='cancelled'`, fillPrice, orderID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ORBStats is a per-ticker performance aggregate over ORB trades. Closed-trade
// metrics drive the `te orb stats` summary; OpenFilled surfaces trades whose entry
// filled but whose exit has not yet been recorded (so the reader can spot a gap).
type ORBStats struct {
	Ticker      string
	Closed      int
	Wins        int
	Losses      int
	Scratches   int
	TPExits     int
	StopExits   int
	TimeExits   int
	ReplayExits int
	BrokerExits int
	TotalPnL    float64
	OpenFilled  int
}

// GetORBStats returns per-ticker aggregates across every ORB trade. Only tickers
// with at least one closed or open-filled trade are returned, ordered by ticker.
// Win/loss/scratch are bucketed by realized pnl sign; exit-type and exit-source
// counters break the closed set down by how the trade exited and how that exit was
// determined (broker fill vs bar replay).
func GetORBStats(ctx context.Context, db *pgxpool.Pool) ([]ORBStats, error) {
	// A split-bracket trade is two rows (a partial + its runner); collapse each pair into one
	// LOGICAL trade so counts aren't inflated and win/loss is judged on the COMBINED PnL.
	// COALESCE(scale_out_parent_id, id) keys every leg to its runner (a partial → its runner's
	// id; a runner or an ordinary trade → its own id). A logical trade is "closed" only when
	// all its legs are, and "open" while any leg is still filled; exit_type/source come from
	// the non-partial (runner / ordinary) leg.
	// A logical trade is DONE when every leg is terminal (closed, cancelled, or expired) — a
	// sibling leg that never filled may be cancelled at cleanup OR marked 'expired' by the
	// stale-session sweep; either must not keep its realized runner out of the stats. It
	// counts as a completed trade when done AND at least one leg actually closed (realized an
	// exit); its PnL/exit come from the closed leg(s). It is still OPEN while any leg is
	// filled (working).
	rows, err := db.Query(ctx, `
		WITH logical AS (
		    SELECT COALESCE(scale_out_parent_id, id)                          AS trade_id,
		           MIN(ticker)                                                AS ticker,
		           bool_and(status IN ('closed','cancelled','expired'))       AS all_done,
		           bool_or(status='closed')                                   AS any_closed,
		           bool_or(status='filled')                                   AS any_filled,
		           bool_or(status IN ('closed','filled'))                     AS active,
		           COALESCE(SUM(pnl) FILTER (WHERE status='closed'), 0)       AS pnl,
		           -- The trade's exit is characterised by the non-partial (runner / ordinary)
		           -- leg's exit. But if that leg never closed — e.g. the runner entry was
		           -- cancelled as an orphan and only the partial realised — fall back to the
		           -- closed leg's exit so the realised trade still lands in a tp/stop bucket.
		           COALESCE(
		               MAX(exit_type) FILTER (WHERE status='closed' AND COALESCE(scale_out_role,'') <> 'partial'),
		               MAX(exit_type) FILTER (WHERE status='closed')
		           ) AS exit_type,
		           COALESCE(
		               MAX(exit_source) FILTER (WHERE status='closed' AND COALESCE(scale_out_role,'') <> 'partial'),
		               MAX(exit_source) FILTER (WHERE status='closed')
		           ) AS exit_source
		    FROM orb_trades
		    GROUP BY COALESCE(scale_out_parent_id, id)
		)
		SELECT ticker,
		       COUNT(*) FILTER (WHERE all_done AND any_closed)                          AS closed,
		       COUNT(*) FILTER (WHERE all_done AND any_closed AND pnl > 0)             AS wins,
		       COUNT(*) FILTER (WHERE all_done AND any_closed AND pnl < 0)             AS losses,
		       COUNT(*) FILTER (WHERE all_done AND any_closed AND pnl = 0)             AS scratches,
		       COUNT(*) FILTER (WHERE all_done AND any_closed AND exit_type='tp')      AS tp_exits,
		       COUNT(*) FILTER (WHERE all_done AND any_closed AND exit_type='stop')    AS stop_exits,
		       COUNT(*) FILTER (WHERE all_done AND any_closed AND exit_type='time_exit') AS time_exits,
		       COUNT(*) FILTER (WHERE all_done AND any_closed AND exit_source='replay') AS replay_exits,
		       COUNT(*) FILTER (WHERE all_done AND any_closed AND exit_source='broker') AS broker_exits,
		       COALESCE(SUM(pnl) FILTER (WHERE all_done AND any_closed), 0)            AS total_pnl,
		       COUNT(*) FILTER (WHERE any_filled AND NOT all_done)                     AS open_filled
		FROM logical
		WHERE active
		GROUP BY ticker
		ORDER BY ticker`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ORBStats
	for rows.Next() {
		var s ORBStats
		if err := rows.Scan(
			&s.Ticker, &s.Closed, &s.Wins, &s.Losses, &s.Scratches,
			&s.TPExits, &s.StopExits, &s.TimeExits,
			&s.ReplayExits, &s.BrokerExits, &s.TotalPnL, &s.OpenFilled,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
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
		       COALESCE(exit_price,0), COALESCE(exit_type,''), COALESCE(exit_source,''),
		       created_at, filled_at, closed_at,
		       COALESCE(scale_out_role,''), scale_out_parent_id,
		       COALESCE(asset_class,'equity'), COALESCE(occ_symbol,''), COALESCE("right",''), COALESCE(strike,0)
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
			&t.ExitPrice, &t.ExitType, &t.ExitSource,
			&t.CreatedAt, &t.FilledAt, &t.ClosedAt,
			&t.ScaleOutRole, &t.ScaleOutParentID,
			&t.AssetClass, &t.OCCSymbol, &t.Right, &t.Strike,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
