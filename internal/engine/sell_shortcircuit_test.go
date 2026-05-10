package engine

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/risk"
)

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

		// Casing must be exact — strategy names flow through runAnalysis as-is
		// (only timeframes are lowercased upstream in processStock) and the
		// canonical list in risk.IsSellOnlyStrategy is lowercase, so an
		// uppercase variant must not skip.
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

// TestBuildOwnershipShortCircuitBlock verifies the SELL-only short-circuit
// emits the same telemetry shape the post-LLM blocked path does — a synthetic
// blocked signal row carrying the canonical NoPositionBlockReason text plus a
// matching SignalBlocked event with FailedCheck="position_ownership". Without
// this surface, every SELL-on-no-long cycle would disappear from the Signals
// table and the dashboard once the short-circuit is taken (Codex/Copilot
// review on PR #337).
func TestBuildOwnershipShortCircuitBlock(t *testing.T) {
	cfg := config.RiskConfig{SignalExpiryMin: 30, StalePriceMovePct: 2.0}
	candles := []data.Candle{{Close: 123.45}}
	reason := risk.NoPositionBlockReason("AAPL")
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

	sig, riskEv, blockEv := buildOwnershipShortCircuitBlock(cfg, "AAPL", "sell_only", candles, reason, now)

	if sig.Ticker != "AAPL" || sig.Action != "SELL" || sig.Strategy != "sell_only" {
		t.Errorf("signal identity wrong: %+v", sig)
	}
	if sig.Status != "blocked" {
		t.Errorf("signal status = %q, want %q", sig.Status, "blocked")
	}
	if sig.BlockReason != reason {
		t.Errorf("signal block_reason = %q, want %q", sig.BlockReason, reason)
	}
	wantReasonText := "no open long position in AAPL — cannot sell"
	if sig.BlockReason != wantReasonText {
		t.Errorf("signal block_reason text = %q, want %q", sig.BlockReason, wantReasonText)
	}
	if sig.SignalPrice != 123.45 {
		t.Errorf("signal price = %v, want 123.45 (latest candle close)", sig.SignalPrice)
	}
	wantExpiry := now.Add(30 * time.Minute)
	if !sig.ExpiresAt.Equal(wantExpiry) {
		t.Errorf("signal expires_at = %v, want %v", sig.ExpiresAt, wantExpiry)
	}

	if riskEv.Ticker != "AAPL" || riskEv.Passed {
		t.Errorf("risk event identity/pass wrong: %+v", riskEv)
	}
	if len(riskEv.Checks) != 1 {
		t.Fatalf("risk event checks len = %d, want 1", len(riskEv.Checks))
	}
	if riskEv.Checks[0].Name != "position_ownership" || riskEv.Checks[0].Passed {
		t.Errorf("risk check[0] = %+v, want failed position_ownership", riskEv.Checks[0])
	}
	if riskEv.Checks[0].Reason != reason {
		t.Errorf("risk check[0] reason = %q, want %q", riskEv.Checks[0].Reason, reason)
	}

	if blockEv.Ticker != "AAPL" || blockEv.Action != "SELL" {
		t.Errorf("block event identity wrong: %+v", blockEv)
	}
	if blockEv.FailedCheck != "position_ownership" {
		t.Errorf("block event FailedCheck = %q, want %q", blockEv.FailedCheck, "position_ownership")
	}
	if blockEv.BlockReason != reason {
		t.Errorf("block event BlockReason = %q, want %q", blockEv.BlockReason, reason)
	}
}

// TestBuildOwnershipShortCircuitBlockDefaults verifies the builder applies the
// same expiry/stale defaults the post-LLM generateSignal path applies, so a
// freshly-installed config without explicit risk values still produces a
// well-formed blocked signal row.
func TestBuildOwnershipShortCircuitBlockDefaults(t *testing.T) {
	cfg := config.RiskConfig{} // zero values
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	sig, _, _ := buildOwnershipShortCircuitBlock(cfg, "TSLA", "exit_signal", nil, "x", now)

	if want := now.Add(30 * time.Minute); !sig.ExpiresAt.Equal(want) {
		t.Errorf("default expires_at = %v, want %v", sig.ExpiresAt, want)
	}
	if sig.StalePriceMovePct != 2.0 {
		t.Errorf("default stale_price_move_pct = %v, want 2.0", sig.StalePriceMovePct)
	}
	if sig.SignalPrice != 0 {
		t.Errorf("signal price with no candles = %v, want 0", sig.SignalPrice)
	}
}
