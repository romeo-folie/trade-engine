package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrScannerCycleLeaseLost = errors.New("scanner cycle lease is no longer current")

// ScannerCycleLease identifies the durable lease that must fence a promotion.
// A promotion transaction renews this lease and writes the watchlist row under
// the same database transaction, preventing a paused stale scanner from
// overwriting a newer owner's result.
type ScannerCycleLease struct {
	RunKey     string
	OwnerToken string
	Duration   time.Duration
}

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
	if strings.HasPrefix(runKey, "regular:") {
		// The ticker is anchored to process startup, so a restart near a
		// bucket boundary can otherwise create an adjacent key for the same
		// cadence window. Serialize regular claims and reject a still-running
		// or very recently completed neighboring slot.
		if _, err := tx.Exec(ctx, `
			SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			"scanner-regular-cycle"); err != nil {
			return false, err
		}
		var status string
		var leaseUntil, updatedAt time.Time
		err := tx.QueryRow(ctx, `
			SELECT status, lease_until, updated_at
			FROM scanner_cycle_claims
			WHERE run_key LIKE 'regular:%'
			ORDER BY updated_at DESC
			LIMIT 1`,
		).Scan(&status, &leaseUntil, &updatedAt)
		if err == nil {
			now := time.Now().UTC()
			cooldown := lease / 2
			if cooldown <= 0 {
				cooldown = time.Hour
			}
			if (status == "running" && leaseUntil.After(now)) ||
				(status == "completed" && updatedAt.After(now.Add(-cooldown))) {
				return false, tx.Commit(ctx)
			}
		} else if !isNoRows(err) {
			return false, err
		}
	}
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

// RenewScannerCycle extends an active lease only for its current owner. A
// false result means the lease was reclaimed or completed by another process.
func RenewScannerCycle(ctx context.Context, runKey, ownerToken string, lease time.Duration) (bool, error) {
	if strings.TrimSpace(runKey) == "" || strings.TrimSpace(ownerToken) == "" {
		return false, fmt.Errorf("scanner cycle key and owner are required")
	}
	if lease <= 0 {
		lease = 2 * time.Hour
	}
	result, err := GetDB().Exec(ctx, `
		UPDATE scanner_cycle_claims
		SET lease_until=$3, updated_at=now()
		WHERE run_key=$1 AND owner_token=$2 AND status='running'
		  AND lease_until > clock_timestamp()`,
		runKey, ownerToken, time.Now().UTC().Add(lease))
	if err != nil {
		return false, err
	}
	return result.RowsAffected() == 1, nil
}

func renewScannerCycleTx(ctx context.Context, tx pgx.Tx, lease ScannerCycleLease) error {
	if strings.TrimSpace(lease.RunKey) == "" || strings.TrimSpace(lease.OwnerToken) == "" {
		return fmt.Errorf("scanner cycle key and owner are required")
	}
	if lease.Duration <= 0 {
		lease.Duration = 2 * time.Hour
	}
	result, err := tx.Exec(ctx, `
		UPDATE scanner_cycle_claims
		SET lease_until=$3, updated_at=now()
		WHERE run_key=$1 AND owner_token=$2 AND status='running'
		  AND lease_until > clock_timestamp()`,
		lease.RunKey, lease.OwnerToken, time.Now().UTC().Add(lease.Duration))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrScannerCycleLeaseLost
	}
	return nil
}
