package store

import (
	"context"
	"time"
)

const BuyControlStocks = "stocks"

// BuyControl is the persisted, auditable operator switch for new BUY orders.
// It intentionally says nothing about exits: protective and analyst exits must
// remain available while entries are paused.
type BuyControl struct {
	Scope     string
	Enabled   bool
	UpdatedAt time.Time
	UpdatedBy string
}

// StockBuyEntriesEnabled is the shared release gate for every new stock
// position. It deliberately leaves exits untouched and preserves legacy
// non-database modes; broker-backed paths have a durable store and fail closed
// when its control cannot be read.
func StockBuyEntriesEnabled(ctx context.Context) (bool, error) {
	if !IsConnected() {
		return true, nil
	}
	control, err := GetBuyControl(ctx, BuyControlStocks)
	if err != nil {
		return false, err
	}
	return control.Enabled, nil
}

// GetBuyControl returns an enabled default when the operator has never set a
// control for this scope. The default preserves existing installs while still
// making an explicit pause survive every process restart.
func GetBuyControl(ctx context.Context, scope string) (BuyControl, error) {
	control := BuyControl{Scope: scope, Enabled: true}
	err := GetDB().QueryRow(ctx, `
		SELECT enabled, updated_at, updated_by FROM buy_controls WHERE scope = $1`, scope,
	).Scan(&control.Enabled, &control.UpdatedAt, &control.UpdatedBy)
	if IsNoRows(err) {
		return control, nil
	}
	return control, err
}

func SetBuyControl(ctx context.Context, scope string, enabled bool, updatedBy string) error {
	_, err := GetDB().Exec(ctx, `
		INSERT INTO buy_controls (scope, enabled, updated_at, updated_by)
		VALUES ($1, $2, NOW(), $3)
		ON CONFLICT (scope) DO UPDATE
		SET enabled = EXCLUDED.enabled, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
		scope, enabled, updatedBy)
	return err
}
