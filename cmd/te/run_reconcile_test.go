package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/engine"
)

func TestRetryExitReconciliationRunsCompletionAfterTransientFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var attempts atomic.Int32
	if completed := retryExitReconciliation(ctx, time.Millisecond, func(context.Context) bool {
		return attempts.Add(1) == 3
	}); !completed {
		t.Fatal("retry did not complete")
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestRetryExitReconciliationStopsWhenStartupIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if completed := retryExitReconciliation(ctx, time.Millisecond, func(context.Context) bool {
		t.Fatal("reconciliation ran after cancellation")
		return true
	}); completed {
		t.Fatal("cancelled startup reported reconciliation complete")
	}
}

func TestRetryBrokerReconciliationPreservesCompletedDrifts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	want := []engine.BrokerDrift{{Ticker: "TSM", Source: "broker_only"}}
	var attempts atomic.Int32
	completed := make(chan []engine.BrokerDrift, 1)
	go retryBrokerReconciliation(ctx, time.Millisecond, func(context.Context) ([]engine.BrokerDrift, bool) {
		if attempts.Add(1) < 2 {
			return nil, false
		}
		return want, true
	}, func(_ context.Context, got []engine.BrokerDrift) {
		completed <- got
	})
	select {
	case got := <-completed:
		if len(got) != 1 || got[0].Ticker != "TSM" {
			t.Fatalf("completed drifts = %+v, want %+v", got, want)
		}
	case <-ctx.Done():
		t.Fatal("broker retry did not complete")
	}
}
