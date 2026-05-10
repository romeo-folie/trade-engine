package engine

import "testing"

// shouldSkipSellStrategy backs the pre-LLM SELL ownership short-circuit (#319).
// It must skip exactly when (a) the strategy is canonically SELL-only and
// (b) the portfolio holds no open long. Any other combination must fall
// through so the analyst is consulted.
func TestShouldSkipSellStrategy(t *testing.T) {
	cases := []struct {
		name      string
		strategy  string
		holdsLong bool
		wantSkip  bool
	}{
		// SELL-only strategies short-circuit only when no long is held.
		{"sell_only no position skips", "sell_only", false, true},
		{"exit_signal no position skips", "exit_signal", false, true},
		{"close_position no position skips", "close_position", false, true},

		// SELL-only with an existing long must still hit the LLM — the analyst
		// decides whether to actually exit.
		{"sell_only with long calls llm", "sell_only", true, false},
		{"exit_signal with long calls llm", "exit_signal", true, false},

		// Dual-direction strategies must always reach the LLM regardless of
		// position state — the model picks BUY/SELL/HOLD/WAIT from context.
		{"trend_following no position calls llm", "trend_following", false, false},
		{"trend_following with long calls llm", "trend_following", true, false},
		{"mean_reversion no position calls llm", "mean_reversion", false, false},
		{"momentum with long calls llm", "momentum", true, false},
		{"news_sentiment no position calls llm", "news_sentiment", false, false},

		// Empty / unknown strategies are treated as dual-direction (safe default).
		{"empty strategy calls llm", "", false, false},
		{"unknown strategy calls llm", "made_up", false, false},

		// Casing must be exact — runAnalysis lowercases-by-convention and the
		// canonical list is lowercase. An uppercase variant must not skip.
		{"upper-cased sell_only does not skip", "SELL_ONLY", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldSkipSellStrategy(tc.strategy, tc.holdsLong)
			if got != tc.wantSkip {
				t.Fatalf("shouldSkipSellStrategy(%q, holdsLong=%v) = %v, want %v",
					tc.strategy, tc.holdsLong, got, tc.wantSkip)
			}
		})
	}
}
