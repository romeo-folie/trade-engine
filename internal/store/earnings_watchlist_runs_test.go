package store

import "testing"

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
