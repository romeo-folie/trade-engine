package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestAnalysisCacheRoundtrip verifies insert → get → touch → delete-expired.
func TestAnalysisCacheRoundtrip(t *testing.T) {
	ctx := context.Background()

	fp := "test-fp-" + time.Now().Format("150405.000000000")
	entry := store.CachedAnalysis{
		Fingerprint:  fp,
		Ticker:       "AAPL",
		Timeframe:    "1d",
		Strategy:     "trend_following",
		ModelProfile: "balanced",
		AnalysisJSON: json.RawMessage(`{"action":"BUY"}`),
		Decision:     "BUY",
		Confidence:   0.82,
		ReasonCode:   "STRONG_TREND",
		ExpiresAt:    time.Now().Add(2 * time.Hour),
	}

	// Insert.
	if err := store.UpsertCachedAnalysis(ctx, entry); err != nil {
		t.Fatalf("UpsertCachedAnalysis: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort cleanup — delete by expiring immediately.
		_ = store.UpsertCachedAnalysis(ctx, store.CachedAnalysis{
			Fingerprint:  fp,
			Ticker:       entry.Ticker,
			Timeframe:    entry.Timeframe,
			Strategy:     entry.Strategy,
			ModelProfile: entry.ModelProfile,
			AnalysisJSON: entry.AnalysisJSON,
			Decision:     entry.Decision,
			Confidence:   entry.Confidence,
			ExpiresAt:    time.Now().Add(-1 * time.Second),
		})
		_, _ = store.DeleteExpiredCachedAnalyses(ctx, time.Now())
	})

	// Get — should return the entry.
	got, err := store.GetCachedAnalysis(ctx, fp)
	if err != nil {
		t.Fatalf("GetCachedAnalysis: %v", err)
	}
	if got == nil {
		t.Fatal("expected cached entry, got nil")
	}
	if got.Ticker != "AAPL" || got.Decision != "BUY" {
		t.Errorf("unexpected entry: %+v", got)
	}
	if got.UseCount != 0 {
		t.Errorf("expected use_count=0 after insert, got %d", got.UseCount)
	}
	if got.ReasonCode != "STRONG_TREND" {
		t.Errorf("expected reason_code=STRONG_TREND, got %q", got.ReasonCode)
	}

	// Touch — increments use_count.
	if err := store.TouchCachedAnalysis(ctx, fp); err != nil {
		t.Fatalf("TouchCachedAnalysis: %v", err)
	}
	got2, err := store.GetCachedAnalysis(ctx, fp)
	if err != nil || got2 == nil {
		t.Fatalf("GetCachedAnalysis after touch: %v %v", got2, err)
	}
	if got2.UseCount != 1 {
		t.Errorf("expected use_count=1 after touch, got %d", got2.UseCount)
	}

	// Upsert again — use_count increments on conflict.
	if err := store.UpsertCachedAnalysis(ctx, entry); err != nil {
		t.Fatalf("second UpsertCachedAnalysis: %v", err)
	}
	got3, _ := store.GetCachedAnalysis(ctx, fp)
	if got3.UseCount != 2 {
		t.Errorf("expected use_count=2 after conflict upsert, got %d", got3.UseCount)
	}
}

// TestAnalysisCacheExpiry verifies that expired entries are not returned by Get
// and are deleted by DeleteExpiredCachedAnalyses.
func TestAnalysisCacheExpiry(t *testing.T) {
	ctx := context.Background()

	fp := "test-expired-fp-" + time.Now().Format("150405.000000000")
	entry := store.CachedAnalysis{
		Fingerprint:  fp,
		Ticker:       "TSLA",
		Timeframe:    "1d",
		Strategy:     "momentum",
		ModelProfile: "pro",
		AnalysisJSON: json.RawMessage(`{"action":"SELL"}`),
		Decision:     "SELL",
		Confidence:   0.70,
		// Expired well in the past: the expiry check compares against the DB
		// server's clock (NOW()), so a sub-second margin flakes on any skew
		// between the test process and the Postgres server.
		ExpiresAt: time.Now().Add(-1 * time.Minute),
	}

	if err := store.UpsertCachedAnalysis(ctx, entry); err != nil {
		t.Fatalf("UpsertCachedAnalysis (expired): %v", err)
	}

	// Get should return nil for expired entry.
	got, err := store.GetCachedAnalysis(ctx, fp)
	if err != nil {
		t.Fatalf("GetCachedAnalysis expired: %v", err)
	}
	if got != nil {
		t.Error("expected nil for expired entry, got non-nil")
	}

	// DeleteExpired should remove the row.
	n, err := store.DeleteExpiredCachedAnalyses(ctx, time.Now())
	if err != nil {
		t.Fatalf("DeleteExpiredCachedAnalyses: %v", err)
	}
	if n < 1 {
		t.Errorf("expected at least 1 deletion, got %d", n)
	}

	// Confirm gone.
	got2, _ := store.GetCachedAnalysis(ctx, fp)
	if got2 != nil {
		t.Error("entry should be deleted after expiry sweep")
	}
}

// TestAnalysisCacheMiss verifies that a missing fingerprint returns nil, nil.
func TestAnalysisCacheMiss(t *testing.T) {
	ctx := context.Background()
	got, err := store.GetCachedAnalysis(ctx, "nonexistent-fingerprint-xyz")
	if err != nil {
		t.Fatalf("unexpected error on miss: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil on miss, got %+v", got)
	}
}

// TestAnalysisCacheNullReasonCode verifies that empty reason_code round-trips
// as an empty string (not a scan error).
func TestAnalysisCacheNullReasonCode(t *testing.T) {
	ctx := context.Background()

	fp := "test-null-reason-" + time.Now().Format("150405.000000000")
	entry := store.CachedAnalysis{
		Fingerprint:  fp,
		Ticker:       "GOOG",
		Timeframe:    "1d",
		Strategy:     "mean_reversion",
		ModelProfile: "budget",
		AnalysisJSON: json.RawMessage(`{"action":"HOLD"}`),
		Decision:     "HOLD",
		Confidence:   0.60,
		ReasonCode:   "", // intentionally empty
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	}
	if err := store.UpsertCachedAnalysis(ctx, entry); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	t.Cleanup(func() {
		_ = store.UpsertCachedAnalysis(ctx, store.CachedAnalysis{
			Fingerprint: fp, Ticker: entry.Ticker, Timeframe: entry.Timeframe,
			Strategy: entry.Strategy, ModelProfile: entry.ModelProfile,
			AnalysisJSON: entry.AnalysisJSON, Decision: entry.Decision,
			Confidence: entry.Confidence, ExpiresAt: time.Now().Add(-1 * time.Second),
		})
		_, _ = store.DeleteExpiredCachedAnalyses(ctx, time.Now())
	})

	got, err := store.GetCachedAnalysis(ctx, fp)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.ReasonCode != "" {
		t.Errorf("expected empty reason_code, got %q", got.ReasonCode)
	}
}
