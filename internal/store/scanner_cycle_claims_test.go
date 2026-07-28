package store_test

import (
	"context"
	"sync"
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
	if renewed, err := store.RenewScannerCycle(ctx, key, "owner-a", time.Hour); err != nil || renewed {
		t.Fatalf("completed lease renewal = %v/%v, want false", renewed, err)
	}
	expiredKey := key + "-expired"
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(ctx, "DELETE FROM scanner_cycle_claims WHERE run_key=$1", expiredKey)
	})
	claimed, err := store.ClaimScannerCycle(ctx, expiredKey, "owner-a", time.Millisecond)
	if err != nil || !claimed {
		t.Fatalf("expired-test claim = %v/%v", claimed, err)
	}
	time.Sleep(10 * time.Millisecond)
	if renewed, err := store.RenewScannerCycle(ctx, expiredKey, "owner-a", time.Hour); err != nil || renewed {
		t.Fatalf("expired lease renewal = %v/%v, want false", renewed, err)
	}
	third, err := store.ClaimScannerCycle(ctx, key, "owner-b", time.Hour)
	if err != nil || third {
		t.Fatalf("completed claim = %v/%v, want false", third, err)
	}
}

func TestScannerCycleClaimConcurrentWinners(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	key := "test-cycle-concurrent-" + time.Now().UTC().Format("20060102150405.000000000")
	t.Cleanup(func() { _, _ = store.GetDB().Exec(ctx, "DELETE FROM scanner_cycle_claims WHERE run_key=$1", key) })
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for _, owner := range []string{"owner-a", "owner-b"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			claimed, err := store.ClaimScannerCycle(ctx, key, owner, time.Hour)
			results <- claimed
			errs <- err
		}(owner)
	}
	wg.Wait()
	close(results)
	close(errs)
	winners := 0
	for claimed := range results {
		if claimed {
			winners++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent claim error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent winners = %d, want 1", winners)
	}
}
