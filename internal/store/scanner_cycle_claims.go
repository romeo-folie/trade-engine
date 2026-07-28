package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ClaimScannerCycle acquires a durable regular-scanner lease. A completed or
// actively leased key cannot be claimed by a concurrent/restarted engine.
func ClaimScannerCycle(ctx context.Context, runKey, ownerToken string, lease time.Duration) (bool, error) {
	if strings.TrimSpace(runKey) == "" || strings.TrimSpace(ownerToken) == "" {
		return false, fmt.Errorf("scanner cycle key and owner are required")
	}
	if lease <= 0 {
		lease = 2 * time.Hour
	}
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	inserted, err := tx.Exec(ctx, `
		INSERT INTO scanner_cycle_claims (run_key,status,owner_token,lease_until)
		VALUES ($1,'running',$2,$3)
		ON CONFLICT (run_key) DO NOTHING`, runKey, ownerToken, time.Now().UTC().Add(lease))
	if err != nil {
		return false, err
	}
	if inserted.RowsAffected() == 1 {
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	var status string
	var leaseUntil time.Time
	err = tx.QueryRow(ctx, `
		SELECT status, lease_until FROM scanner_cycle_claims
		WHERE run_key=$1 FOR UPDATE`, runKey).Scan(&status, &leaseUntil)
	if err != nil {
		return false, err
	}
	if status == "completed" || (status == "running" && leaseUntil.After(time.Now().UTC())) {
		return false, tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, `
		UPDATE scanner_cycle_claims SET status='running', owner_token=$2,
		lease_until=$3, updated_at=now() WHERE run_key=$1`,
		runKey, ownerToken, time.Now().UTC().Add(lease))
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func CompleteScannerCycle(ctx context.Context, runKey, ownerToken string, failed bool) error {
	status := "completed"
	if failed {
		status = "failed"
	}
	_, err := GetDB().Exec(ctx, `
		UPDATE scanner_cycle_claims SET status=$3, updated_at=now()
		WHERE run_key=$1 AND owner_token=$2`, runKey, ownerToken, status)
	return err
}
