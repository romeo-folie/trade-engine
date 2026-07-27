package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CurrencyStrengthRun is the durable daily audit row for the currency-strength
// strategy. Order fill fields may remain zero when the broker acknowledges a
// market order before reporting its execution.
type CurrencyStrengthRun struct {
	ID                string
	SessionDate       time.Time
	Status            string
	Timezone          string
	StrengthStart     string
	StrengthEnd       string
	ExitTime          string
	RankInstruments   []string
	TradeInstruments  []string
	Units             int
	StrongestCurrency string
	WeakestCurrency   string
	StrengthGap       float64
	SignalSymbol      string
	SignalSide        string
	SkipReason        string
	EntryOrderID      string
	EntryStatus       string
	EntryFillPrice    float64
	EntryFilledQty    float64
	EnteredAt         *time.Time
	ExitOrderID       string
	ExitStatus        string
	ExitFillPrice     float64
	ExitFilledQty     float64
	ExitedAt          *time.Time
	RealizedPnL       *float64
	Error             string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// StartCurrencyStrengthRun creates today's audit row without erasing a decision
// already recorded by an earlier process. This makes same-day restarts
// idempotent while still refreshing the effective schedule and basket.
func StartCurrencyStrengthRun(
	ctx context.Context,
	db *pgxpool.Pool,
	run CurrencyStrengthRun,
) error {
	_, err := db.Exec(ctx, `
		INSERT INTO currency_strength_runs (
			session_date, status, timezone, strength_start, strength_end, exit_time,
			rank_instruments, trade_instruments, units
		) VALUES ($1, 'scheduled', $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (session_date) DO UPDATE SET
			timezone=EXCLUDED.timezone,
			strength_start=EXCLUDED.strength_start,
			strength_end=EXCLUDED.strength_end,
			exit_time=EXCLUDED.exit_time,
			rank_instruments=EXCLUDED.rank_instruments,
			trade_instruments=EXCLUDED.trade_instruments,
			units=EXCLUDED.units,
			updated_at=NOW()`,
		run.SessionDate, run.Timezone, run.StrengthStart, run.StrengthEnd,
		run.ExitTime, run.RankInstruments, run.TradeInstruments, run.Units,
	)
	return err
}

// MarkCurrencyStrengthEvaluating records that the decision window is being
// processed. Final outcomes overwrite this transient state.
func MarkCurrencyStrengthEvaluating(
	ctx context.Context,
	db *pgxpool.Pool,
	sessionDate time.Time,
) error {
	_, err := db.Exec(ctx, `
		UPDATE currency_strength_runs
		SET status='evaluating', error=NULL, updated_at=NOW()
		WHERE session_date=$1 AND status IN ('scheduled', 'evaluating')`,
		sessionDate,
	)
	return err
}

// RecordCurrencyStrengthNoSignal stores a completed decision that placed no
// order. The reason remains visible in the Intraday dashboard.
func RecordCurrencyStrengthNoSignal(
	ctx context.Context,
	db *pgxpool.Pool,
	sessionDate time.Time,
	reason string,
) error {
	_, err := db.Exec(ctx, `
		UPDATE currency_strength_runs
		SET status='no_signal', skip_reason=$2, error=NULL, updated_at=NOW()
		WHERE session_date=$1`,
		sessionDate, reason,
	)
	return err
}

// RecordCurrencyStrengthEntry persists the ranked currencies, chosen pair, and
// immediate broker acknowledgement after a paper entry is submitted.
func RecordCurrencyStrengthEntry(
	ctx context.Context,
	db *pgxpool.Pool,
	run CurrencyStrengthRun,
) error {
	_, err := db.Exec(ctx, `
		UPDATE currency_strength_runs
		SET status='entered',
		    strongest_currency=COALESCE(NULLIF($2,''), strongest_currency),
		    weakest_currency=COALESCE(NULLIF($3,''), weakest_currency),
		    strength_gap=COALESCE(NULLIF($4, 0), strength_gap),
		    signal_symbol=$5, signal_side=$6, skip_reason=NULL,
		    entry_order_id=$7, entry_status=$8,
		    entry_fill_price=NULLIF($9, 0), entry_filled_qty=NULLIF($10, 0),
		    entered_at=COALESCE($11, entered_at), error=NULL, updated_at=NOW()
		WHERE session_date=$1`,
		run.SessionDate, run.StrongestCurrency, run.WeakestCurrency,
		run.StrengthGap, run.SignalSymbol, run.SignalSide,
		run.EntryOrderID, run.EntryStatus, run.EntryFillPrice,
		run.EntryFilledQty, run.EnteredAt,
	)
	return err
}

// RecordCurrencyStrengthExit persists the flatten order acknowledgement. A
// submitted-but-unfilled order remains explicitly "exit_submitted"; it is not
// presented as a confirmed exit.
func RecordCurrencyStrengthExit(
	ctx context.Context,
	db *pgxpool.Pool,
	run CurrencyStrengthRun,
) error {
	_, err := db.Exec(ctx, `
		UPDATE currency_strength_runs
		SET status=CASE WHEN LOWER($3)='filled' THEN 'exited' ELSE 'exit_submitted' END,
		    exit_order_id=$2, exit_status=$3,
		    exit_fill_price=NULLIF($4, 0), exit_filled_qty=NULLIF($5, 0),
		    exited_at=$6, realized_pnl=$7, error=NULL, updated_at=NOW()
		WHERE session_date=$1`,
		run.SessionDate, run.ExitOrderID, run.ExitStatus, run.ExitFillPrice,
		run.ExitFilledQty, run.ExitedAt, run.RealizedPnL,
	)
	return err
}

// RecordCurrencyStrengthFailure keeps strategy failures visible instead of
// leaving an apparently active run behind.
func RecordCurrencyStrengthFailure(
	ctx context.Context,
	db *pgxpool.Pool,
	sessionDate time.Time,
	message string,
) error {
	_, err := db.Exec(ctx, `
		UPDATE currency_strength_runs
		SET status='failed', error=$2, updated_at=NOW()
		WHERE session_date=$1`,
		sessionDate, message,
	)
	return err
}

// ListCurrencyStrengthRunsPage returns newest-first rows for the requested
// zero-based offset. Callers request pageSize+1 to detect an older page.
func ListCurrencyStrengthRunsPage(
	ctx context.Context,
	db *pgxpool.Pool,
	limit, offset int,
) ([]CurrencyStrengthRun, error) {
	if limit < 1 {
		return nil, nil
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := db.Query(ctx, `
		SELECT id, session_date, status, timezone, strength_start, strength_end,
		       exit_time, rank_instruments, trade_instruments, units,
		       COALESCE(strongest_currency,''), COALESCE(weakest_currency,''),
		       COALESCE(strength_gap,0), COALESCE(signal_symbol,''),
		       COALESCE(signal_side,''), COALESCE(skip_reason,''),
		       COALESCE(entry_order_id,''), COALESCE(entry_status,''),
		       COALESCE(entry_fill_price,0), COALESCE(entry_filled_qty,0),
		       entered_at, COALESCE(exit_order_id,''), COALESCE(exit_status,''),
		       COALESCE(exit_fill_price,0), COALESCE(exit_filled_qty,0),
		       exited_at, realized_pnl, COALESCE(error,''),
		       created_at, updated_at
		FROM currency_strength_runs
		ORDER BY session_date DESC, created_at DESC
		LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CurrencyStrengthRun
	for rows.Next() {
		var run CurrencyStrengthRun
		if err := rows.Scan(
			&run.ID, &run.SessionDate, &run.Status, &run.Timezone,
			&run.StrengthStart, &run.StrengthEnd, &run.ExitTime,
			&run.RankInstruments, &run.TradeInstruments, &run.Units,
			&run.StrongestCurrency, &run.WeakestCurrency, &run.StrengthGap,
			&run.SignalSymbol, &run.SignalSide, &run.SkipReason,
			&run.EntryOrderID, &run.EntryStatus, &run.EntryFillPrice,
			&run.EntryFilledQty, &run.EnteredAt, &run.ExitOrderID,
			&run.ExitStatus, &run.ExitFillPrice, &run.ExitFilledQty,
			&run.ExitedAt, &run.RealizedPnL, &run.Error,
			&run.CreatedAt, &run.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}
