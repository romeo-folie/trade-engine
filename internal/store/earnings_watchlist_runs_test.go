package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func TestEarningsWatchlistCohortIsImmutableAcrossRetry(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	target := time.Date(2046, time.June, 4, 0, 0, 0, 0, time.UTC)
	_, _ = GetDB().Exec(ctx, `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target)
	t.Cleanup(func() {
		_, _ = GetDB().Exec(ctx, `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target)
	})
	now := time.Now()
	attempt, claimed, err := ClaimEarningsWatchlistRun(ctx, target, now)
	if err != nil || !claimed {
		t.Fatalf("claim first attempt: id=%q claimed=%t err=%v", attempt, claimed, err)
	}
	if _, found, err := LoadEarningsWatchlistCohort(ctx, target); err != nil || found {
		t.Fatalf("initial cohort found=%t err=%v, want unset", found, err)
	}
	first := []EarningsWatchlistCandidate{
		{Ticker: "COHORT_A", LiquidityRank: 1, MedianDollarVolume: 100},
		{Ticker: "COHORT_B", LiquidityRank: 2, MedianDollarVolume: 50, Context: tradeprofile.DiscoveryContext{Thesis: "original"}},
	}
	if got, err := PersistEarningsWatchlistCohort(ctx, target, attempt, first); err != nil || len(got) != 2 || got[1].Ticker != "COHORT_B" {
		t.Fatalf("persist first cohort=%+v err=%v", got, err)
	}
	if err := FailEarningsWatchlistRun(ctx, target, attempt, ErrResearchQueueCapacity); err != nil {
		t.Fatal(err)
	}
	retryAt := now.Add(31 * time.Minute)
	secondAttempt, claimed, err := ClaimEarningsWatchlistRun(ctx, target, retryAt)
	if err != nil || !claimed || secondAttempt == attempt {
		t.Fatalf("claim retry: id=%q claimed=%t err=%v", secondAttempt, claimed, err)
	}
	got, found, err := LoadEarningsWatchlistCohort(ctx, target)
	if err != nil || !found || len(got) != 2 || got[0].Ticker != "COHORT_A" || got[1].Ticker != "COHORT_B" || got[1].Context.Thesis != "original" {
		t.Fatalf("loaded retry cohort=%+v found=%t err=%v, want original ranked cohort", got, found, err)
	}
	changed := []EarningsWatchlistCandidate{{Ticker: "COHORT_C", LiquidityRank: 1, MedianDollarVolume: 1000}}
	got, err = PersistEarningsWatchlistCohort(ctx, target, secondAttempt, changed)
	if err != nil || len(got) != 2 || got[0].Ticker != "COHORT_A" || got[1].Ticker != "COHORT_B" {
		t.Fatalf("retry replaced immutable cohort with %+v (err=%v)", got, err)
	}
}

func TestEarningsMainWatchlistCapacityDoesNotRecordAllocationSuppression(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	var runsTable *string
	if err := GetDB().QueryRow(ctx, `SELECT to_regclass('public.earnings_watchlist_runs')::text`).Scan(&runsTable); err != nil {
		t.Fatalf("check earnings watchlist schema: %v", err)
	}
	if runsTable == nil {
		t.Skip("earnings watchlist schema is unavailable in this integration database")
	}
	target := time.Date(2045, time.January, 2, 0, 0, 0, 0, time.UTC)
	const ticker = "EARNINGS_MAIN_CAP_AUDIT_RT"
	_, _ = GetDB().Exec(ctx, `DELETE FROM research_queue_decisions WHERE ticker=$1`, ticker)
	_, _ = GetDB().Exec(ctx, `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target)
	t.Cleanup(func() {
		_, _ = GetDB().Exec(ctx, `DELETE FROM research_queue_decisions WHERE ticker=$1`, ticker)
		_, _ = GetDB().Exec(ctx, `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target)
	})
	attemptID, claimed, err := ClaimEarningsWatchlistRun(ctx, target, time.Now())
	if err != nil || !claimed {
		t.Fatalf("claim earnings run: attempt=%q claimed=%t err=%v", attemptID, claimed, err)
	}
	err = recordEarningsCapacityFailure(ctx, target, attemptID, EarningsWatchlistCandidate{
		Ticker: ticker, ResearchQueue: &ResearchQueueAdmission{
			Capacity: 1, Bucket: "earnings_scheduled", Reservation: 1,
		},
	}, ErrWatchlistCapacity)
	if !errors.Is(err, ErrWatchlistCapacity) {
		t.Fatalf("record main-watchlist capacity failure: %v", err)
	}
	var decisions int
	if err := GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM research_queue_decisions WHERE ticker=$1`, ticker).Scan(&decisions); err != nil {
		t.Fatalf("count allocation decisions: %v", err)
	}
	if decisions != 0 {
		t.Fatalf("allocation decisions=%d, want none for main-watchlist capacity", decisions)
	}
}
