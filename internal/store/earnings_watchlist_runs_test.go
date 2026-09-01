package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOrderedEarningsWatchlistCandidatesLocksTickersCanonically(t *testing.T) {
	candidates := []EarningsWatchlistCandidate{
		{Ticker: " zzz "}, {Ticker: "aaa"}, {Ticker: "BBB"},
	}
	ordered := orderedEarningsWatchlistCandidates(candidates)
	got := []string{ordered[0].Ticker, ordered[1].Ticker, ordered[2].Ticker}
	want := []string{"aaa", "BBB", " zzz "}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ordered tickers = %v, want %v", got, want)
		}
	}
	if candidates[0].Ticker != " zzz " {
		t.Fatalf("ordering mutated caller candidates: %v", candidates)
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
