package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestScannerCycleClaimIsIdempotent(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	key := "test-cycle-claim-" + time.Now().UTC().Format("20060102150405.000000000")
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(ctx, "DELETE FROM scanner_cycle_claims WHERE run_key=$1", key)
	})
	first, err := store.ClaimScannerCycle(ctx, key, "owner-a", time.Hour)
	if err != nil || !first {
		t.Fatalf("first claim = %v/%v", first, err)
	}
	second, err := store.ClaimScannerCycle(ctx, key, "owner-b", time.Hour)
	if err != nil || second {
		t.Fatalf("concurrent claim = %v/%v, want false", second, err)
	}
	if err := store.CompleteScannerCycle(ctx, key, "owner-a", false); err != nil {
		t.Fatalf("complete claim: %v", err)
	}
	third, err := store.ClaimScannerCycle(ctx, key, "owner-b", time.Hour)
	if err != nil || third {
		t.Fatalf("completed claim = %v/%v, want false", third, err)
	}
}
