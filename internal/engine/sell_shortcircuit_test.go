package engine

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
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

// TestShouldPrefetchOpenTrades verifies the gating predicate that controls
// whether runAnalysis issues an up-front store.GetOpenTrades query (#319,
// PR #337 round-3). The prefetch must run only when both conditions hold:
//
//   - the strategy is canonically SELL-only (dual-direction strategies do not
//     need ownership info up-front; generateSignal fetches on-demand). Without
//     this gate every analysis cycle pays a wasted DB roundtrip — Copilot perf
//     regression flagged on round-2.
//   - the store is connected (a nil pool would panic via GetDB inside
//     GetOpenTrades). Without this gate the SELL-only path crashes whenever
//     the engine runs degraded without a DB — Copilot crash flagged on round-2.
func TestShouldPrefetchOpenTrades(t *testing.T) {
	cases := []struct {
		name         string
		strategy     string
		dbConnected  bool
		wantPrefetch bool
	}{
		// SELL-only strategies prefetch only with a live DB.
		{"sell_only with db prefetches", "sell_only", true, true},
		{"exit_signal with db prefetches", "exit_signal", true, true},
		{"close_position with db prefetches", "close_position", true, true},

		// SELL-only without DB must NOT prefetch — would panic in GetDB.
		// Falls through to LLM; the post-LLM gate handles no-DB the same
		// way it does for any other SELL.
		{"sell_only no db skips", "sell_only", false, false},
		{"exit_signal no db skips", "exit_signal", false, false},

		// Dual-direction strategies must NEVER prefetch even with a live DB —
		// the prefetch is purely there to support the SELL ownership
		// short-circuit, so for trend_following / mean_reversion / momentum /
		// news_sentiment / etc. we leave it to generateSignal's on-demand
		// fetch (perf regression fix).
		{"trend_following with db skips", "trend_following", true, false},
		{"trend_following no db skips", "trend_following", false, false},
		{"mean_reversion with db skips", "mean_reversion", true, false},
		{"momentum with db skips", "momentum", true, false},
		{"news_sentiment with db skips", "news_sentiment", true, false},

		// Empty / unknown strategies are treated as dual-direction.
		{"empty with db skips", "", true, false},
		{"unknown with db skips", "made_up", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldPrefetchOpenTrades(tc.strategy, tc.dbConnected)
			if got != tc.wantPrefetch {
				t.Fatalf("shouldPrefetchOpenTrades(%q, dbConnected=%v) = %v, want %v",
					tc.strategy, tc.dbConnected, got, tc.wantPrefetch)
			}
		})
	}
}

// TestEmitOwnershipShortCircuitBlock_NoDBStillPublishesEvents verifies the
// no-DB safety guard added in PR #337 round-3. emitOwnershipShortCircuitBlock
// previously called store.InsertSignal unconditionally, which panics via
// GetDB when the engine is running degraded without a database (Copilot crash
// review). The fix skips the persist but still publishes RiskCheckCompleted +
// SignalBlocked events so the in-process surfaces (TUI, Telegram, dashboard
// SSE) continue to see the block. Only the persistent signals row is lost.
func TestEmitOwnershipShortCircuitBlock_NoDBStillPublishesEvents(t *testing.T) {
	// store.IsConnected reads the unexported package-level pool, which is nil
	// in the test process unless an explicit Connect was issued. Engine tests
	// in this package run without Connect, so IsConnected == false here.

	bus := events.New()
	riskSub := bus.Subscribe(events.RiskCheckCompleted)
	defer bus.Unsubscribe(riskSub)
	blockSub := bus.Subscribe(events.SignalBlocked)
	defer bus.Unsubscribe(blockSub)

	cfg := config.Defaults()
	cfg.Risk.SignalExpiryMin = 30
	cfg.Risk.StalePriceMovePct = 2.0

	e := &Engine{
		cfg: cfg,
		bus: bus,
	}

	candles := []data.Candle{{Close: 555.55}}
	reason := risk.NoPositionBlockReason("MSFT")

	// Must not panic even though the store has no live pool.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("emitOwnershipShortCircuitBlock panicked with no DB: %v", r)
		}
	}()
	e.emitOwnershipShortCircuitBlock(context.Background(), "MSFT", "sell_only", candles, reason)

	// Both events must have fired despite the missing DB. We use a short
	// deadline rather than blocking forever so a bug that drops the publish
	// surfaces as a clear timeout failure.
	select {
	case ev := <-riskSub.C:
		payload, ok := ev.Payload.(events.RiskCheckCompletedPayload)
		if !ok {
			t.Fatalf("RiskCheckCompleted payload type = %T, want RiskCheckCompletedPayload", ev.Payload)
		}
		if payload.Ticker != "MSFT" || payload.Passed {
			t.Errorf("risk event identity/pass wrong: %+v", payload)
		}
		if len(payload.Checks) != 1 || payload.Checks[0].Name != "position_ownership" {
			t.Errorf("risk event checks wrong: %+v", payload.Checks)
		}
	case <-time.After(time.Second):
		t.Fatal("RiskCheckCompleted not published within 1s")
	}

	select {
	case ev := <-blockSub.C:
		payload, ok := ev.Payload.(events.SignalBlockedPayload)
		if !ok {
			t.Fatalf("SignalBlocked payload type = %T, want SignalBlockedPayload", ev.Payload)
		}
		if payload.Ticker != "MSFT" || payload.Action != "SELL" {
			t.Errorf("block event identity wrong: %+v", payload)
		}
		if payload.FailedCheck != "position_ownership" || payload.BlockReason != reason {
			t.Errorf("block event metadata wrong: %+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("SignalBlocked not published within 1s")
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
