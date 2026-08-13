package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	EntrySetupStandard         = "standard"
	EntrySetupPullbackStrength = "pullback_strength"
)

// Signal mirrors a row from the signals table.
type Signal struct {
	ID                 string
	Ticker             string
	Action             string // BUY | SELL
	Strategy           string
	SignalPrice        float64
	StopLoss           float64
	TakeProfit         float64
	Confidence         float64
	SizePct            float64
	Status             string // pending | approved | blocked | executed | expired | stale
	BlockReason        string
	AnalysisIDs        []string
	ExpiresAt          time.Time
	StalePriceMovePct  float64
	EntrySetup         string
	EntrySetupEvidence string
	// ScannerRunID links scanner-originated signals to the exact prospective
	// universe snapshot that sourced the ticker.
	ScannerRunID string
	// PullbackStrength marks signals whose stop and eligibility came from the
	// complete pullback setup. It survives manual approval/restarts so execution
	// can revalidate the low-of-day stop against a fresh mark. For these signals,
	// CreatedAt is the observation boundary captured immediately after the
	// qualification-time daily snapshot returned; exact trade scanning starts
	// there, before gate evaluation and persistence.
	PullbackStrength bool
	// Deferred marks a signal the engine held back for first-tick
	// confirmation (entry gates, migration 043). Only deferred signals are
	// eligible for the deferred-execution sweep — manual-approval pendings
	// are never touched by it.
	Deferred  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// InsertSignal creates a new signal record and returns its UUID.
func InsertSignal(ctx context.Context, s Signal) (string, error) {
	ids := s.AnalysisIDs
	if ids == nil {
		ids = []string{}
	}
	createdAt := s.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	entrySetup := s.EntrySetup
	if entrySetup == "" {
		entrySetup = EntrySetupStandard
	}
	var id string
	err := GetDB().QueryRow(ctx, `
		INSERT INTO signals (
			ticker, action, strategy, signal_price, stop_loss, take_profit,
			confidence, size_pct, status, block_reason,
			analysis_ids, expires_at, stale_threshold_pct, deferred,
			pullback_strength, entry_setup, entry_setup_evidence, created_at,
			scanner_run_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		RETURNING id`,
		s.Ticker, s.Action, s.Strategy, s.SignalPrice,
		nullableFloat(s.StopLoss), nullableFloat(s.TakeProfit),
		s.Confidence, s.SizePct, s.Status, nullableString(s.BlockReason),
		ids, s.ExpiresAt, s.StalePriceMovePct, s.Deferred,
		s.PullbackStrength, entrySetup, nullableString(s.EntrySetupEvidence), createdAt,
		nullableString(s.ScannerRunID),
	).Scan(&id)
	return id, err
}

// UpdateSignalStatus sets the status (and optional block_reason) for a signal.
func UpdateSignalStatus(ctx context.Context, id, status, blockReason string) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE signals SET status=$1, block_reason=$2, updated_at=NOW() WHERE id=$3`,
		status, nullableString(blockReason), id)
	return err
}

// QueueSignalForDeferredExecution makes an approved, pending, or temporarily
// blocked BUY retryable by the normal fresh engine admission path without
// changing its strategy data.
func QueueSignalForDeferredExecution(ctx context.Context, id string) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE signals
		SET status='pending', deferred=TRUE, block_reason=NULL, updated_at=NOW()
		WHERE id=$1::uuid AND action='BUY' AND status IN ('pending','approved','blocked')`, id)
	return err
}

// UpdateSignalPrice updates the signal_price for a signal.
func UpdateSignalPrice(ctx context.Context, id string, price float64) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE signals SET signal_price=$1, updated_at=NOW() WHERE id=$2`,
		price, id)
	return err
}

// GetPendingSignals returns all signals with status = 'pending' or 'approved'.
func GetPendingSignals(ctx context.Context) ([]Signal, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, ticker, action, strategy, signal_price,
		       COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       confidence, COALESCE(size_pct,0), status,
		       COALESCE(block_reason,''), expires_at, stale_threshold_pct,
		       pullback_strength, entry_setup, COALESCE(entry_setup_evidence,''),
		       created_at, updated_at, COALESCE(scanner_run_id::text, '')
		FROM signals WHERE status IN ('pending','approved')
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSignals(rows)
}

// GetDeferredSignals returns pending signals the engine deferred for
// first-tick confirmation. Deliberately excludes manual-approval pendings
// (deferred = FALSE) — see migration 043.
func GetDeferredSignals(ctx context.Context) ([]Signal, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, ticker, action, strategy, signal_price,
		       COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       confidence, COALESCE(size_pct,0), status,
		       COALESCE(block_reason,''), expires_at, stale_threshold_pct,
		       pullback_strength, entry_setup, COALESCE(entry_setup_evidence,''),
		       created_at, updated_at, COALESCE(scanner_run_id::text, '')
		FROM signals WHERE status = 'pending' AND deferred = TRUE
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sigs, err := scanSignals(rows)
	for i := range sigs {
		sigs[i].Deferred = true
	}
	return sigs, err
}

// GetSignalsByTicker returns recent signals for a ticker.
func GetSignalsByTicker(ctx context.Context, ticker string, limit int) ([]Signal, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, ticker, action, strategy, signal_price,
		       COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       confidence, COALESCE(size_pct,0), status,
		       COALESCE(block_reason,''), expires_at, stale_threshold_pct,
		       pullback_strength, entry_setup, COALESCE(entry_setup_evidence,''),
		       created_at, updated_at, COALESCE(scanner_run_id::text, '')
		FROM signals WHERE ticker=$1
		ORDER BY created_at DESC LIMIT $2`, ticker, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSignals(rows)
}

// GetSignalByID retrieves a single signal by UUID.
func GetSignalByID(ctx context.Context, id string) (*Signal, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, ticker, action, strategy, signal_price,
		       COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       confidence, COALESCE(size_pct,0), status,
		       COALESCE(block_reason,''), expires_at, stale_threshold_pct,
		       pullback_strength, entry_setup, COALESCE(entry_setup_evidence,''),
		       created_at, updated_at, COALESCE(scanner_run_id::text, '')
		FROM signals WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results, err := scanSignals(rows)
	if err != nil || len(results) == 0 {
		return nil, err
	}
	return &results[0], nil
}

func scanSignals(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Signal, error) {
	var out []Signal
	for rows.Next() {
		var s Signal
		if err := rows.Scan(
			&s.ID, &s.Ticker, &s.Action, &s.Strategy, &s.SignalPrice,
			&s.StopLoss, &s.TakeProfit,
			&s.Confidence, &s.SizePct, &s.Status,
			&s.BlockReason, &s.ExpiresAt, &s.StalePriceMovePct,
			&s.PullbackStrength, &s.EntrySetup, &s.EntrySetupEvidence,
			&s.CreatedAt, &s.UpdatedAt, &s.ScannerRunID,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetRecentSignals returns the most recent signals regardless of ticker or status.
func GetRecentSignals(ctx context.Context, limit int) ([]Signal, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, ticker, action, strategy, signal_price,
		       COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       confidence, COALESCE(size_pct,0), status, COALESCE(block_reason,''),
		       COALESCE(analysis_ids, '{}'), expires_at, stale_threshold_pct,
		       pullback_strength, entry_setup, COALESCE(entry_setup_evidence,''),
		       created_at, updated_at, COALESCE(scanner_run_id::text, '')
		FROM signals
		ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Signal
	for rows.Next() {
		var s Signal
		if err := rows.Scan(
			&s.ID, &s.Ticker, &s.Action, &s.Strategy, &s.SignalPrice,
			&s.StopLoss, &s.TakeProfit,
			&s.Confidence, &s.SizePct, &s.Status,
			&s.BlockReason, &s.AnalysisIDs, &s.ExpiresAt, &s.StalePriceMovePct,
			&s.PullbackStrength, &s.EntrySetup, &s.EntrySetupEvidence,
			&s.CreatedAt, &s.UpdatedAt, &s.ScannerRunID,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListSignalsOpts controls filtering and cursor pagination for ListSignals.
type ListSignalsOpts struct {
	Limit      int       // 1..200
	Ticker     string    // optional ILIKE substring
	Status     string    // optional exact match
	BeforeTime time.Time // optional cursor; zero = no cursor
	BeforeID   string    // tiebreaker for equal created_at values
}

// ListSignals returns signals with optional ticker/status filtering and keyset
// pagination. The cursor (BeforeTime, BeforeID) is the (created_at, id) of the
// last row from the previous page. Pass a zero BeforeTime for the first page.
func ListSignals(ctx context.Context, opts ListSignalsOpts) ([]Signal, error) {
	var args []any
	var conds []string

	if opts.Ticker != "" {
		args = append(args, opts.Ticker)
		conds = append(conds, fmt.Sprintf("ticker ILIKE '%%' || $%d || '%%'", len(args)))
	}
	if opts.Status != "" {
		args = append(args, opts.Status)
		conds = append(conds, fmt.Sprintf("status = $%d", len(args)))
	}
	if !opts.BeforeTime.IsZero() {
		args = append(args, opts.BeforeTime, opts.BeforeID)
		conds = append(conds, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	args = append(args, opts.Limit)
	q := fmt.Sprintf(`
		SELECT id, ticker, action, strategy, signal_price,
		       COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       confidence, COALESCE(size_pct,0), status, COALESCE(block_reason,''),
		       COALESCE(analysis_ids, '{}'), expires_at, stale_threshold_pct,
		       pullback_strength, entry_setup, COALESCE(entry_setup_evidence,''),
		       created_at, updated_at, COALESCE(scanner_run_id::text, '')
		FROM signals
		%s
		ORDER BY created_at DESC, id DESC
		LIMIT $%d`, where, len(args))

	rows, err := GetDB().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Signal
	for rows.Next() {
		var s Signal
		if err := rows.Scan(
			&s.ID, &s.Ticker, &s.Action, &s.Strategy, &s.SignalPrice,
			&s.StopLoss, &s.TakeProfit,
			&s.Confidence, &s.SizePct, &s.Status,
			&s.BlockReason, &s.AnalysisIDs, &s.ExpiresAt, &s.StalePriceMovePct,
			&s.PullbackStrength, &s.EntrySetup, &s.EntrySetupEvidence,
			&s.CreatedAt, &s.UpdatedAt, &s.ScannerRunID,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
