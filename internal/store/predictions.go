package store

import (
	"context"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

// ── Types ─────────────────────────────────────────────────────────────────────

// PredictionSnapshot mirrors a row from the prediction_snapshots table.
type PredictionSnapshot struct {
	ID                 string
	Platform           string
	ContractID         string
	ContractTitle      string
	Price              float64
	YesPrice           float64
	NoPrice            float64
	Volume24h          float64
	OpenInterest       float64
	Bid                float64
	Ask                float64
	ResolutionDeadline time.Time
	Resolved           bool
	ResolutionValue    *bool
	Categories         []string
	CreatedAt          time.Time
}

// ── Insert ────────────────────────────────────────────────────────────────────

// InsertPredictionSnapshot stores a contract snapshot.
// The UNIQUE constraint on (platform, contract_id, created_at) makes this
// idempotent when called multiple times within the same second.
func InsertPredictionSnapshot(ctx context.Context, s data.ContractSnapshot) error {
	_, err := GetDB().Exec(ctx, `
		INSERT INTO prediction_snapshots (
			platform, contract_id, contract_title,
			price, yes_price, no_price,
			volume_24h, open_interest, bid, ask,
			resolution_deadline, resolved, resolution_value,
			categories
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (platform, contract_id, created_at) DO NOTHING`,
		s.Platform, s.ContractID, s.ContractTitle,
		s.Price, nullableFloat(s.YesPrice), nullableFloat(s.NoPrice),
		nullableFloat(s.Volume24h), nullableFloat(s.OpenInterest),
		nullableFloat(s.Bid), nullableFloat(s.Ask),
		nullableTime(s.ResolutionDeadline), s.Resolved, s.ResolutionValue,
		s.Categories,
	)
	return err
}

// ── Query ─────────────────────────────────────────────────────────────────────

// GetRecentSnapshots returns the most recent N snapshots for a contract.
func GetRecentSnapshots(ctx context.Context, platform, contractID string, limit int) ([]PredictionSnapshot, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, platform, contract_id, contract_title,
		       price, COALESCE(yes_price,0), COALESCE(no_price,0),
		       COALESCE(volume_24h,0), COALESCE(open_interest,0),
		       COALESCE(bid,0), COALESCE(ask,0),
		       COALESCE(resolution_deadline, '2099-01-01 00:00:00+00'::timestamptz), resolved,
		       resolution_value, COALESCE(categories, '{}'), created_at
		FROM prediction_snapshots
		WHERE platform = $1 AND contract_id = $2
		ORDER BY created_at DESC LIMIT $3`,
		platform, contractID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snaps []PredictionSnapshot
	for rows.Next() {
		var s PredictionSnapshot
		if err := rows.Scan(
			&s.ID, &s.Platform, &s.ContractID, &s.ContractTitle,
			&s.Price, &s.YesPrice, &s.NoPrice,
			&s.Volume24h, &s.OpenInterest,
			&s.Bid, &s.Ask,
			&s.ResolutionDeadline, &s.Resolved,
			&s.ResolutionValue, &s.Categories, &s.CreatedAt,
		); err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
	}
	return snaps, rows.Err()
}

// GetSnapshotsSince returns snapshots for a contract created after 'since'.
func GetSnapshotsSince(ctx context.Context, platform, contractID string, since time.Time) ([]PredictionSnapshot, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, platform, contract_id, contract_title,
		       price, COALESCE(yes_price,0), COALESCE(no_price,0),
		       COALESCE(volume_24h,0), COALESCE(open_interest,0),
		       COALESCE(bid,0), COALESCE(ask,0),
		       COALESCE(resolution_deadline, '2099-01-01 00:00:00+00'::timestamptz), resolved,
		       resolution_value, COALESCE(categories, '{}'), created_at
		FROM prediction_snapshots
		WHERE platform = $1 AND contract_id = $2 AND created_at > $3
		ORDER BY created_at ASC`,
		platform, contractID, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snaps []PredictionSnapshot
	for rows.Next() {
		var s PredictionSnapshot
		if err := rows.Scan(
			&s.ID, &s.Platform, &s.ContractID, &s.ContractTitle,
			&s.Price, &s.YesPrice, &s.NoPrice,
			&s.Volume24h, &s.OpenInterest,
			&s.Bid, &s.Ask,
			&s.ResolutionDeadline, &s.Resolved,
			&s.ResolutionValue, &s.Categories, &s.CreatedAt,
		); err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
	}
	return snaps, rows.Err()
}

// GetLatestSnapshot returns the most recent snapshot for a contract, or nil.
func GetLatestSnapshot(ctx context.Context, platform, contractID string) (*PredictionSnapshot, error) {
	snaps, err := GetRecentSnapshots(ctx, platform, contractID, 1)
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, nil
	}
	return &snaps[0], nil
}

// MarkContractResolved sets resolved=true and resolution_value on all snapshots
// for a contract.  Called when a platform signals the contract has settled.
func MarkContractResolved(ctx context.Context, platform, contractID string, value bool) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE prediction_snapshots
		SET resolved = true, resolution_value = $1
		WHERE platform = $2 AND contract_id = $3 AND resolved = false`,
		value, platform, contractID)
	return err
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
