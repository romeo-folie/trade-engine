// Command jmia-consolidate collapses the 8 stale JMIA trade rows from
// dev-mode restarts into 1 canonical row, marking the others
// status='consolidated' and pointing them at the canonical row via
// consolidated_into.
//
// SAFETY: this command does NOTHING without --apply. Without --apply it
// prints the rows it WOULD touch and exits 0. With --apply it wraps every
// mutation in a single transaction, so a failed update rolls back the
// entire collapse.
//
// USAGE (run AFTER you've confirmed the broker quantity with
// `broker-verify`):
//
//	go run ./cmd/jmia-consolidate \
//	    --broker-qty=50 \
//	    --broker-avg-entry=2.34 \
//	    --apply
//
// REVERSIBILITY: the operation is reversible with a single SQL statement
// against the canonical id printed at end of the run:
//
//	UPDATE trades
//	SET status='open', consolidated_into=NULL
//	WHERE consolidated_into = '<canonical_id>';
//
// The canonical row's pre-collapse quantity / entry_price are NOT
// preserved by this script — capture them from the dry-run output before
// running with --apply if you want a perfect rollback.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "jmia-consolidate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		ticker         = flag.String("ticker", "JMIA", "ticker symbol to consolidate")
		brokerQty      = flag.Float64("broker-qty", 0, "broker-reported aggregate quantity for the canonical row (required)")
		brokerAvgEntry = flag.Float64("broker-avg-entry", 0, "broker-reported average entry price for the canonical row (required)")
		expectedRows   = flag.Int("expected-rows", 8, "abort if the number of open rows for the ticker doesn't match (safety guard)")
		apply          = flag.Bool("apply", false, "actually perform the migration; without this flag the script only prints what it would do")
	)
	flag.Parse()

	if *brokerQty <= 0 {
		return fmt.Errorf("--broker-qty is required and must be > 0 (run cmd/broker-verify to confirm the live position)")
	}
	if *brokerAvgEntry <= 0 {
		return fmt.Errorf("--broker-avg-entry is required and must be > 0")
	}

	// Auto-load .env so DATABASE_URL is available without explicit export.
	_ = godotenv.Load()

	ctx := context.Background()
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer store.Close()

	db := store.GetDB()

	// 1. Snapshot the open rows.
	rows, err := db.Query(ctx, `
		SELECT id, COALESCE(signal_id::text,''), quantity, entry_price, opened_at
		FROM trades
		WHERE ticker=$1 AND status='open'
		ORDER BY opened_at ASC`, *ticker)
	if err != nil {
		return fmt.Errorf("query open %s rows: %w", *ticker, err)
	}
	type openRow struct {
		ID         string
		SignalID   string
		Quantity   float64
		EntryPrice float64
		OpenedAt   string
	}
	var openRows []openRow
	for rows.Next() {
		var r openRow
		var openedAt any
		if err := rows.Scan(&r.ID, &r.SignalID, &r.Quantity, &r.EntryPrice, &openedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan open row: %w", err)
		}
		r.OpenedAt = fmt.Sprintf("%v", openedAt)
		openRows = append(openRows, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate open rows: %w", err)
	}

	if len(openRows) == 0 {
		return fmt.Errorf("no open %s trades found — nothing to do", *ticker)
	}
	if len(openRows) != *expectedRows {
		return fmt.Errorf("expected %d open %s rows but found %d — re-check the DB before applying (override with --expected-rows=%d if intentional)",
			*expectedRows, *ticker, len(openRows), len(openRows))
	}

	// 2. Pick the canonical row deterministically: the only one that has a
	//    matching row in `orders` (i.e. was actually placed at the broker).
	//    Tiebreaker: oldest opened_at.
	var canonicalID string
	err = db.QueryRow(ctx, `
		SELECT id
		FROM trades
		WHERE ticker=$1
		  AND status='open'
		  AND id IN (SELECT trade_id FROM orders WHERE ticker=$1 AND trade_id IS NOT NULL)
		ORDER BY opened_at ASC
		LIMIT 1`, *ticker).Scan(&canonicalID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("no open %s row has a matching orders row — refusing to guess at the canonical row", *ticker)
		}
		return fmt.Errorf("select canonical row: %w", err)
	}

	// 3. Print plan.
	fmt.Printf("ticker:           %s\n", *ticker)
	fmt.Printf("open rows found:  %d\n", len(openRows))
	fmt.Printf("canonical row id: %s  (only row linked from orders)\n", canonicalID)
	fmt.Printf("broker qty:       %.4f\n", *brokerQty)
	fmt.Printf("broker avg entry: %.6f\n\n", *brokerAvgEntry)
	fmt.Println("PLAN:")
	for _, r := range openRows {
		if r.ID == canonicalID {
			fmt.Printf("  CANONICAL  %s  qty=%g entry=%g  -> qty=%g entry=%g (broker aggregate)\n",
				r.ID, r.Quantity, r.EntryPrice, *brokerQty, *brokerAvgEntry)
		} else {
			fmt.Printf("  collapse   %s  qty=%g entry=%g  -> status=consolidated, consolidated_into=%s\n",
				r.ID, r.Quantity, r.EntryPrice, canonicalID)
		}
	}
	fmt.Println()

	if !*apply {
		fmt.Println("DRY RUN — re-run with --apply to commit.")
		return nil
	}

	// 4. Apply in a single transaction. ALTER TYPE was already committed by
	//    the schema migration; this transaction only touches data rows.
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		// safe to call after Commit (returns ErrTxClosed which we ignore).
		_ = tx.Rollback(ctx)
	}()

	// Update canonical row's quantity / entry_price to the broker aggregate.
	canonicalTag, err := tx.Exec(ctx, `
		UPDATE trades
		SET quantity=$1, entry_price=$2
		WHERE id=$3 AND status='open'`,
		*brokerQty, *brokerAvgEntry, canonicalID)
	if err != nil {
		return fmt.Errorf("update canonical row: %w", err)
	}
	if canonicalTag.RowsAffected() != 1 {
		return fmt.Errorf("update canonical row: expected 1 row affected, got %d", canonicalTag.RowsAffected())
	}

	// Mark the others consolidated.
	consolidateTag, err := tx.Exec(ctx, `
		UPDATE trades
		SET status='consolidated', consolidated_into=$1
		WHERE ticker=$2
		  AND status='open'
		  AND id <> $1`,
		canonicalID, *ticker)
	if err != nil {
		return fmt.Errorf("mark consolidated: %w", err)
	}
	expectedConsolidated := int64(len(openRows) - 1)
	if consolidateTag.RowsAffected() != expectedConsolidated {
		return fmt.Errorf("mark consolidated: expected %d rows affected, got %d", expectedConsolidated, consolidateTag.RowsAffected())
	}

	// Smoke check: exactly one open row remains for the ticker.
	var remainingOpen int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM trades WHERE ticker=$1 AND status='open'`, *ticker,
	).Scan(&remainingOpen); err != nil {
		return fmt.Errorf("smoke count: %w", err)
	}
	if remainingOpen != 1 {
		return fmt.Errorf("smoke check failed: expected 1 open %s row after collapse, got %d — rolling back",
			*ticker, remainingOpen)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}

	fmt.Printf("APPLIED. canonical=%s  consolidated=%d  remaining_open=%d\n",
		canonicalID, consolidateTag.RowsAffected(), remainingOpen)
	fmt.Println()
	fmt.Println("Reversibility:")
	fmt.Printf("  UPDATE trades SET status='open', consolidated_into=NULL WHERE consolidated_into = '%s';\n", canonicalID)
	return nil
}
