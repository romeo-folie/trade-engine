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
	completed := make(chan struct{})
	go retryExitReconciliation(ctx, time.Millisecond, func(context.Context) bool {
		return attempts.Add(1) == 3
	}, func(context.Context) {
		close(completed)
	})
	select {
	case <-completed:
		if got := attempts.Load(); got != 3 {
			t.Fatalf("attempts = %d, want 3", got)
		}
	case <-ctx.Done():
		t.Fatal("retry did not complete")
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
