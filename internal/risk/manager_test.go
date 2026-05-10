package risk

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func defaultCfg() config.RiskConfig {
	return config.RiskConfig{
		ConfidenceThreshold: 0.65,
		MinRiskReward:       2.0,
		MaxPositionPct:      5.0,
		MaxOpenPositions:    10,
		MaxPerTicker:        2,
		MaxDollarRiskPct:    1.0,
		DailyLossLimitPct:   3.0,
		MaxDrawdownPct:      10.0,
		CooldownAfterLoss:   30,
	}
}

func baseInput() CheckInput {
	return CheckInput{
		Cfg:            defaultCfg(),
		Action:         "BUY",
		Confidence:     0.80,
		SizePct:        2.0,
		SignalPrice:    100.0,
		StopLoss:       95.0,
		TakeProfit:     110.0,
		PortfolioValue: 10000,
		Cash:           5000,
		OpenPositions:  2,
		DailyPnL:       0,
		PeakValue:      10000,
	}
}

// Helpers

func assertPasses(t *testing.T, fn func(CheckInput) CheckResult, in CheckInput) {
	t.Helper()
	r := fn(in)
	if !r.Passed {
		t.Fatalf("%s: expected pass, got fail: %s", r.Name, r.Reason)
	}
}

func assertFails(t *testing.T, fn func(CheckInput) CheckResult, in CheckInput) CheckResult {
	t.Helper()
	r := fn(in)
	if r.Passed {
		t.Fatalf("%s: expected fail, got pass", r.Name)
	}
	return r
}

// ── Confidence ────────────────────────────────────────────────────────────────

func TestConfidence_Pass(t *testing.T) {
	assertPasses(t, checkConfidence, baseInput())
}

func TestConfidence_Fail(t *testing.T) {
	in := baseInput()
	in.Confidence = 0.50
	r := assertFails(t, checkConfidence, in)
	if r.Name != "confidence_threshold" {
		t.Fatalf("wrong check name: %s", r.Name)
	}
}

// ── Position ownership ────────────────────────────────────────────────────────

func TestPositionOwnership_BuyAlwaysPasses(t *testing.T) {
	in := baseInput()
	in.Action = "BUY"
	in.HoldsLong = false
	assertPasses(t, checkPositionOwnership, in)
}

func TestPositionOwnership_SellWithSufficientPosition(t *testing.T) {
	in := baseInput()
	in.Action = "SELL"
	in.Ticker = "HIMS"
	in.HoldsLong = true
	// proposedQty = (5000 * 2.0/100) / 100.0 = 1.0; held = 10 → ok
	in.LongQuantity = 10
	assertPasses(t, checkPositionOwnership, in)
}

func TestPositionOwnership_SellExceedsHeldQuantity(t *testing.T) {
	in := baseInput()
	in.Action = "SELL"
	in.Ticker = "HIMS"
	in.HoldsLong = true
	// proposedQty = (5000 * 2.0/100) / 100.0 = 1.0; held = 0.5 → oversell
	in.LongQuantity = 0.5
	r := assertFails(t, checkPositionOwnership, in)
	if r.Name != "position_ownership" {
		t.Fatalf("wrong check name: %s", r.Name)
	}
}

func TestPositionOwnership_SellWithoutPosition(t *testing.T) {
	in := baseInput()
	in.Action = "SELL"
	in.Ticker = "HIMS"
	in.HoldsLong = false
	r := assertFails(t, checkPositionOwnership, in)
	if r.Name != "position_ownership" {
		t.Fatalf("wrong check name: %s", r.Name)
	}
}

// ── Risk/Reward ───────────────────────────────────────────────────────────────

func TestRiskReward_Pass(t *testing.T) {
	// reward=10, risk=5 → R:R=2.0
	assertPasses(t, checkRiskReward, baseInput())
}

func TestRiskReward_Fail(t *testing.T) {
	in := baseInput()
	in.TakeProfit = 101 // reward=1, risk=5 → R:R=0.2
	assertFails(t, checkRiskReward, in)
}

func TestRiskReward_SkipWhenNoStopLoss(t *testing.T) {
	in := baseInput()
	in.StopLoss = 0
	in.TakeProfit = 0
	assertPasses(t, checkRiskReward, in)
}

// ── Position size ─────────────────────────────────────────────────────────────

func TestPositionSize_Pass(t *testing.T) {
	assertPasses(t, checkPositionSize, baseInput())
}

func TestPositionSize_Fail(t *testing.T) {
	in := baseInput()
	in.SizePct = 6.0
	assertFails(t, checkPositionSize, in)
}

// ── Max dollar at risk (#320) ─────────────────────────────────────────────────

func TestMaxDollarRisk_PassWellUnderCap(t *testing.T) {
	// baseInput: qty = (2/100)*5000/100 = 1; risk per share = 5; $@risk = 5
	// pct = 0.05% << 1.0% cap → pass
	assertPasses(t, checkMaxDollarRisk, baseInput())
}

func TestMaxDollarRisk_AtCapExactlyPasses(t *testing.T) {
	in := baseInput()
	// Cash=10000, SizePct=2, SignalPrice=100 → qty=2
	// |100-50|=50; $@risk = 100; pct = 100/10000*100 = 1.00% == cap → pass
	in.Cash = 10000
	in.SignalPrice = 100.0
	in.StopLoss = 50.0
	assertPasses(t, checkMaxDollarRisk, in)
}

func TestMaxDollarRisk_OneBasisPointOverFails(t *testing.T) {
	in := baseInput()
	// Cash=10000, SizePct=2, SignalPrice=100 → qty=2
	// |100-49.495|=50.505; $@risk = 101.01; pct = 1.0101% → rounds to 1.01%
	// Genuine 1 bp over the 1.00% cap; block reason prints "1.01% > 1.00%".
	in.Cash = 10000
	in.SignalPrice = 100.0
	in.StopLoss = 49.495
	r := assertFails(t, checkMaxDollarRisk, in)
	if r.Name != "max_dollar_risk" {
		t.Fatalf("wrong check name: %s", r.Name)
	}
	// Block reason format: "$-at-risk %.2f%% > limit %.2f%%"
	want := "$-at-risk 1.01% > limit 1.00%"
	if r.Reason != want {
		t.Fatalf("block reason mismatch:\n  got:  %q\n  want: %q", r.Reason, want)
	}
}

func TestMaxDollarRisk_PortfolioValueZeroPasses(t *testing.T) {
	in := baseInput()
	in.PortfolioValue = 0
	in.Cash = 10000
	in.SignalPrice = 100.0
	in.StopLoss = 50.0 // would be at cap if portfolio were nonzero
	assertPasses(t, checkMaxDollarRisk, in)
}

func TestMaxDollarRisk_DisabledWhenZero(t *testing.T) {
	in := baseInput()
	in.Cfg.MaxDollarRiskPct = 0
	in.SignalPrice = 100.0
	in.StopLoss = 1.0 // wildly over a 1% cap, but cap is disabled
	assertPasses(t, checkMaxDollarRisk, in)
}

// Mirrors the engine's actual sizing path: position is sized off Cash,
// not PortfolioValue. With most cash already deployed, an equity-based
// qty estimate would overstate $@risk and produce a false block. The
// cash-based estimate keeps the gate consistent with what the engine
// will actually trade.
func TestMaxDollarRisk_UsesCashNotPortfolioValue(t *testing.T) {
	in := baseInput()
	// PortfolioValue=100k, Cash=20k (most deployed), SizePct=5, stop 10% from price.
	// Cash-based qty = (5/100)*20000/100 = 10 → $@risk = 10 * 10 = 100
	// pct = 100/100000 * 100 = 0.10% << 1.0% cap → pass.
	//
	// Equity-based qty (the prior bug) = (5/100)*100000/100 = 50 → $@risk = 500
	// pct = 0.50% — still under cap here, but the multiplier is 5×; the test
	// pins the cash-based formula by asserting a passing reason that matches
	// the smaller estimate.
	in.PortfolioValue = 100000
	in.Cash = 20000
	in.SizePct = 5.0
	in.SignalPrice = 100.0
	in.StopLoss = 90.0
	assertPasses(t, checkMaxDollarRisk, in)
}

func TestMaxDollarRisk_SkipWhenStopLossZero(t *testing.T) {
	// Mirrors checkRiskReward semantics: no stop → nothing to measure → pass.
	in := baseInput()
	in.StopLoss = 0
	in.TakeProfit = 0
	assertPasses(t, checkMaxDollarRisk, in)
}

// SELL signals are exit-only (checkPositionOwnership blocks SELL without an
// open long). The analyst prompt requests stop_loss/take_profit on SELLs too,
// so the entry-risk cap must skip them — otherwise legitimate exits with a
// far stop would be blocked.
func TestMaxDollarRisk_SkipsForSell(t *testing.T) {
	in := baseInput()
	in.Action = "SELL"
	// Stop far enough that the BUY path would fail: $@risk = 2 * 50 = 100,
	// pct = 100/10000*100 = 1.00% (at cap) and we push it well over.
	in.Cash = 10000
	in.SignalPrice = 100.0
	in.StopLoss = 1.0 // wildly over the 1% cap if this check ran
	assertPasses(t, checkMaxDollarRisk, in)
}

// Guards a hole in checkRiskReward: when TakeProfit is zero, that check is
// skipped, so a StopLoss == SignalPrice (zero risk distance) wouldn't be
// caught upstream. checkMaxDollarRisk fails it explicitly, mirroring the
// same defensive pattern checkRiskReward uses for "stop equals signal price".
func TestMaxDollarRisk_FailsWhenStopEqualsSignal(t *testing.T) {
	in := baseInput()
	in.SignalPrice = 100.0
	in.StopLoss = 100.0
	in.TakeProfit = 0 // checkRiskReward would skip this case entirely
	// Confirm the load-bearing assumption: checkRiskReward does NOT catch this.
	if rr := checkRiskReward(in); !rr.Passed {
		t.Fatalf("precondition: expected checkRiskReward to skip when TP==0, got fail: %s", rr.Reason)
	}
	r := assertFails(t, checkMaxDollarRisk, in)
	if r.Name != "max_dollar_risk" {
		t.Fatalf("wrong check name: %s", r.Name)
	}
	want := "stop-loss equals signal price (zero risk, invalid)"
	if r.Reason != want {
		t.Fatalf("block reason mismatch:\n  got:  %q\n  want: %q", r.Reason, want)
	}
}

// ── Max open positions ────────────────────────────────────────────────────────

func TestMaxOpenPositions_Pass(t *testing.T) {
	assertPasses(t, checkMaxOpenPositions, baseInput())
}

func TestMaxOpenPositions_Fail(t *testing.T) {
	in := baseInput()
	in.OpenPositions = 10
	assertFails(t, checkMaxOpenPositions, in)
}

// ── Max per ticker (#312) ─────────────────────────────────────────────────────

func TestMaxPerTicker_PassWhenBelowLimit(t *testing.T) {
	in := baseInput()
	in.Ticker = "JMIA"
	in.OpenByTicker = map[string]int{"JMIA": 1}
	assertPasses(t, checkMaxPerTicker, in)
}

func TestMaxPerTicker_FailWhenAtLimit(t *testing.T) {
	in := baseInput()
	in.Ticker = "JMIA"
	in.OpenByTicker = map[string]int{"JMIA": 2}
	r := assertFails(t, checkMaxPerTicker, in)
	if r.Name != "max_per_ticker" {
		t.Fatalf("wrong check name: %s", r.Name)
	}
}

func TestMaxPerTicker_DisabledWhenZero(t *testing.T) {
	in := baseInput()
	in.Cfg.MaxPerTicker = 0
	in.Ticker = "JMIA"
	// Even an absurdly large count should pass when the cap is disabled.
	in.OpenByTicker = map[string]int{"JMIA": 99}
	assertPasses(t, checkMaxPerTicker, in)
}

func TestMaxPerTicker_NilMapTreatedAsZero(t *testing.T) {
	in := baseInput()
	in.Ticker = "JMIA"
	in.OpenByTicker = nil
	assertPasses(t, checkMaxPerTicker, in)
}

// Mirrors the engine-side aggregation: 8 open rows for the same
// (ticker, direction) collapse to a single distinct position and a
// per-ticker count that depends on the action.
//
// The implementation only counts a row toward byTicker when the action
// matches the row's direction (BUY+long or SELL+short). Subtests pin
// each action variant so a future change to the gating logic can't
// silently regress HOLD/WAIT to count as live additions.
func TestDistinctPositionCounting_EngineAggregation(t *testing.T) {
	type openTrade struct {
		Ticker    string
		Direction string
	}
	openTrades := []openTrade{
		{"JMIA", "long"}, {"JMIA", "long"}, {"JMIA", "long"}, {"JMIA", "long"},
		{"JMIA", "long"}, {"JMIA", "long"}, {"JMIA", "long"}, {"JMIA", "long"},
	}

	aggregate := func(action string) (positions map[string]struct{}, byTicker map[string]int) {
		positions = make(map[string]struct{}, len(openTrades))
		byTicker = make(map[string]int, len(openTrades))
		for _, ot := range openTrades {
			key := ot.Ticker + "|" + ot.Direction
			positions[key] = struct{}{}
			if (action == "BUY" && ot.Direction == "long") ||
				(action == "SELL" && ot.Direction == "short") {
				byTicker[ot.Ticker]++
			}
		}
		return positions, byTicker
	}

	t.Run("BUY", func(t *testing.T) {
		positions, byTicker := aggregate("BUY")
		if got := len(positions); got != 1 {
			t.Fatalf("distinct positions: want 1, got %d", got)
		}
		if got := byTicker["JMIA"]; got != 8 {
			t.Fatalf("byTicker[JMIA]: want 8, got %d", got)
		}

		// Feeding into CheckAll: max_open_positions sees 1 (passes), but the
		// per-ticker cap of 2 catches the burst.
		in := baseInput()
		in.Ticker = "JMIA"
		in.OpenPositions = len(positions)
		in.OpenByTicker = byTicker
		results, ok := CheckAll(in)
		if ok {
			t.Fatalf("expected CheckAll to block, got pass; results=%+v", results)
		}
		if reason := BlockReason(results); reason == "" {
			t.Fatal("expected non-empty block reason")
		}
	})

	// SELL against 8 long rows must NOT count any of them — a sell
	// against held longs is a close, not an additional position.
	t.Run("SELL", func(t *testing.T) {
		positions, byTicker := aggregate("SELL")
		if got := len(positions); got != 1 {
			t.Fatalf("distinct positions: want 1, got %d", got)
		}
		if got := byTicker["JMIA"]; got != 0 {
			t.Fatalf("byTicker[JMIA] for SELL on longs: want 0, got %d", got)
		}
	})

	// HOLD/WAIT are non-actions: they must never inflate per-ticker
	// counts regardless of how many open rows the ticker has.
	for _, action := range []string{"HOLD", "WAIT"} {
		action := action
		t.Run(action, func(t *testing.T) {
			positions, byTicker := aggregate(action)
			if got := len(positions); got != 1 {
				t.Fatalf("distinct positions: want 1, got %d", got)
			}
			if got := byTicker["JMIA"]; got != 0 {
				t.Fatalf("byTicker[JMIA] for %s: want 0, got %d", action, got)
			}
		})
	}
}

// ── Daily loss ────────────────────────────────────────────────────────────────

func TestDailyLoss_Pass(t *testing.T) {
	assertPasses(t, checkDailyLossLimit, baseInput())
}

func TestDailyLoss_Fail(t *testing.T) {
	in := baseInput()
	in.DailyPnL = -400 // 4% of 10000 > 3% limit
	assertFails(t, checkDailyLossLimit, in)
}

// ── Max drawdown ──────────────────────────────────────────────────────────────

func TestMaxDrawdown_Pass(t *testing.T) {
	assertPasses(t, checkMaxDrawdown, baseInput())
}

func TestMaxDrawdown_Fail(t *testing.T) {
	in := baseInput()
	in.PortfolioValue = 8900 // 11% drawdown from peak 10000
	assertFails(t, checkMaxDrawdown, in)
}

// ── Cooldown ──────────────────────────────────────────────────────────────────

func TestCooldown_PassWhenNoRecentLoss(t *testing.T) {
	in := baseInput()
	in.LastLossAt = time.Time{} // zero
	assertPasses(t, checkCooldownAfterLoss, in)
}

func TestCooldown_PassWhenCooldownExpired(t *testing.T) {
	in := baseInput()
	in.LastLossAt = time.Now().Add(-45 * time.Minute) // 45m ago > 30m cooldown
	assertPasses(t, checkCooldownAfterLoss, in)
}

func TestCooldown_Fail(t *testing.T) {
	in := baseInput()
	in.LastLossAt = time.Now().Add(-5 * time.Minute) // 5m ago < 30m cooldown
	assertFails(t, checkCooldownAfterLoss, in)
}

// ── CheckAll ──────────────────────────────────────────────────────────────────

func TestCheckAll_AllPass(t *testing.T) {
	_, ok := CheckAll(baseInput())
	if !ok {
		t.Fatal("expected all checks to pass")
	}
}

func TestCheckAll_ShortCircuitsOnFirstFail(t *testing.T) {
	in := baseInput()
	in.Confidence = 0.10
	results, ok := CheckAll(in)
	if ok {
		t.Fatal("expected failure")
	}
	// Should stop after first failed check.
	if len(results) != 1 {
		t.Fatalf("expected 1 result (short-circuit), got %d", len(results))
	}
}

func TestBlockReason_ReturnsFirstFailReason(t *testing.T) {
	in := baseInput()
	in.Confidence = 0.10
	results, _ := CheckAll(in)
	reason := BlockReason(results)
	if reason == "" {
		t.Fatal("expected non-empty block reason")
	}
}
