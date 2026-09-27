package backtest

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

// ── coversRange ───────────────────────────────────────────────────────────────

func TestCoversRange_Empty(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC)
	if coversRange(nil, from, to, data.Timeframe1Day) {
		t.Error("empty slice should not cover any range")
	}
}

func TestBuildRequestUsesBacktestRiskConfig(t *testing.T) {
	cfg := config.RiskConfig{MaxPositionPct: 10, MaxPerTicker: 2, MinRiskReward: 1.5}
	req, err := buildRequest(RunOptions{Ticker: "AAPL", Timeframe: data.Timeframe1Day},
		[]data.Candle{{Close: 100, High: 101, Low: 99, Volume: 1000}}, cfg)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if got := req.RiskPolicy; !got.Configured || got.MaxTickerExposurePct != 10 || got.MaxPerTicker != 2 || got.MinRiskReward != 1.5 {
		t.Fatalf("backtest request policy = %+v, want configured 10%% / 2 / 1.5", got)
	}
	zero, err := buildRequest(RunOptions{Ticker: "AAPL"}, nil, config.RiskConfig{})
	if err != nil || !zero.RiskPolicy.Configured || zero.RiskPolicy.Normalized().MaxTickerExposurePct != 0 {
		t.Fatalf("explicit-zero backtest policy was not preserved: policy=%+v err=%v", zero.RiskPolicy, err)
	}
}

func TestCoversRange_ExactBoundary(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)
	candles := []data.Candle{
		{Timestamp: from},
		{Timestamp: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
		{Timestamp: to},
	}
	if !coversRange(candles, from, to, data.Timeframe1Day) {
		t.Error("exact boundary candles should cover range")
	}
}

func TestCoversRange_WithinTolerance(t *testing.T) {
	// tolerance for 1d is 2 × 24h = 48h; shift boundaries by 12h — should still cover
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC)
	candles := []data.Candle{
		{Timestamp: from.Add(12 * time.Hour)}, // within 2d tolerance
		{Timestamp: to.Add(-12 * time.Hour)},  // within 2d tolerance
	}
	if !coversRange(candles, from, to, data.Timeframe1Day) {
		t.Error("candles within tolerance should cover range")
	}
}

func TestCoversRange_OutsideTolerance(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC)
	// First candle starts 3 days late — outside 2d daily tolerance
	candles := []data.Candle{
		{Timestamp: from.Add(72*time.Hour + time.Minute)},
		{Timestamp: to},
	}
	if coversRange(candles, from, to, data.Timeframe1Day) {
		t.Error("candles starting outside tolerance should not cover range")
	}
}

func TestCoversRange_Hourly(t *testing.T) {
	from := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)
	to := time.Date(2024, 1, 1, 17, 0, 0, 0, time.UTC)
	candles := []data.Candle{
		{Timestamp: from},
		{Timestamp: to},
	}
	if !coversRange(candles, from, to, data.Timeframe1Hour) {
		t.Error("hourly exact boundary should cover range")
	}
}

// ── candleDuration ────────────────────────────────────────────────────────────

func TestCandleDuration(t *testing.T) {
	cases := []struct {
		tf   data.Timeframe
		want time.Duration
	}{
		{data.Timeframe1Min, time.Minute},
		{data.Timeframe5Min, 5 * time.Minute},
		{data.Timeframe15Min, 15 * time.Minute},
		{data.Timeframe1Hour, time.Hour},
		{data.Timeframe1Day, 24 * time.Hour},
	}
	for _, c := range cases {
		got := candleDuration(c.tf)
		if got != c.want {
			t.Errorf("candleDuration(%s): expected %v, got %v", c.tf, c.want, got)
		}
	}
}

// ── evaluateExit (baseline policy) ────────────────────────────────────────────

func newPos(sl, tp float64) *openPos {
	return &openPos{
		ticker:     "TEST",
		direction:  "buy",
		entryTS:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		entryPrice: 100,
		quantity:   10,
		cost:       1000,
		stopLoss:   sl,
		takeProfit: tp,
	}
}

func makeCandle(low, high float64) data.Candle {
	ts := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	return data.Candle{Timestamp: ts, Open: 100, High: high, Low: low, Close: 105}
}

func TestCheckSLTP_NoTrigger(t *testing.T) {
	pos := newPos(90, 120)
	candle := makeCandle(95, 115) // neither SL nor TP reached
	_, triggered := evaluateExit(pos, candle, 0, 0, exitPolicy{})
	if triggered {
		t.Error("expected no trigger")
	}
}

func TestCheckSLTP_StopLoss(t *testing.T) {
	pos := newPos(95, 120)
	candle := makeCandle(93, 115) // low touches SL
	trade, triggered := evaluateExit(pos, candle, 0, 0, exitPolicy{})
	if !triggered {
		t.Fatal("expected SL trigger")
	}
	if trade.ExitReason != "stop_loss" {
		t.Errorf("expected exit_reason stop_loss, got %q", trade.ExitReason)
	}
	if trade.ExitPrice != 95 {
		t.Errorf("expected exit price 95 (SL), got %.2f", trade.ExitPrice)
	}
}

func TestCheckSLTP_TakeProfit(t *testing.T) {
	pos := newPos(90, 110)
	candle := makeCandle(95, 115) // high touches TP
	trade, triggered := evaluateExit(pos, candle, 0, 0, exitPolicy{})
	if !triggered {
		t.Fatal("expected TP trigger")
	}
	if trade.ExitReason != "take_profit" {
		t.Errorf("expected exit_reason take_profit, got %q", trade.ExitReason)
	}
	if trade.ExitPrice != 110 {
		t.Errorf("expected exit price 110 (TP), got %.2f", trade.ExitPrice)
	}
}

func TestCheckSLTP_SLWinsOverTP(t *testing.T) {
	// Both SL and TP triggered in same candle — SL must win (conservative).
	pos := newPos(95, 105)
	candle := makeCandle(90, 110)
	trade, triggered := evaluateExit(pos, candle, 0, 0, exitPolicy{})
	if !triggered {
		t.Fatal("expected trigger")
	}
	if trade.ExitReason != "stop_loss" {
		t.Errorf("expected stop_loss to win over take_profit, got %q", trade.ExitReason)
	}
}

func TestPullbackFillBlock(t *testing.T) {
	tests := []struct {
		name       string
		mark       float64
		stop       float64
		target     float64
		maxWidth   float64
		confidence float64
		riskCfg    config.RiskConfig
		wantReason string
	}{
		{name: "valid", mark: 100, stop: 98, target: 104, maxWidth: 4, confidence: 0.8, riskCfg: config.RiskConfig{MinRiskReward: 1.5}},
		{name: "gap through stop", mark: 97, stop: 98, target: 104, maxWidth: 4, confidence: 0.8, riskCfg: config.RiskConfig{MinRiskReward: 1.5}, wantReason: "gap_invalidation"},
		{name: "upward gap widens stop", mark: 110, stop: 100, target: 125, maxWidth: 4, confidence: 0.8, riskCfg: config.RiskConfig{MinRiskReward: 1.5}, wantReason: "fill_stop_width"},
		{name: "gap ruins reward", mark: 103, stop: 100, target: 105, maxWidth: 4, confidence: 0.8, riskCfg: config.RiskConfig{MinRiskReward: 1.5}, wantReason: "fill_risk_reward"},
		{name: "gap above target", mark: 106, stop: 103, target: 105, maxWidth: 4, confidence: 0.8, riskCfg: config.RiskConfig{MinRiskReward: 1.5}, wantReason: "fill_risk_reward"},
		{
			name: "gap moves into stricter confidence tier", mark: 100, stop: 96, target: 106,
			maxWidth: 4, confidence: 0.56,
			riskCfg: config.RiskConfig{
				MinRiskReward: 1.2,
				ConfidenceTiers: []config.ConfidenceTier{
					{RRMin: 2, ConfMin: 0.55},
					{RRMin: 1.4, ConfMin: 0.58},
				},
			},
			wantReason: "fill_confidence_tier",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pullbackFillBlock(
				tt.mark, tt.stop, tt.target, tt.maxWidth, tt.confidence, tt.riskCfg,
			); got != tt.wantReason {
				t.Fatalf("pullbackFillBlock() = %q, want %q", got, tt.wantReason)
			}
		})
	}
}

func TestPullbackSignalBlockUsesConfidenceTiers(t *testing.T) {
	riskCfg := config.RiskConfig{
		ConfidenceThreshold: 0.65,
		MinRiskReward:       1.2,
		ConfidenceTiers: []config.ConfidenceTier{
			{RRMin: 2, ConfMin: 0.55},
			{RRMin: 1.4, ConfMin: 0.58},
		},
	}
	if got := pullbackSignalBlock(100, 98, 104, 0.60, riskCfg); got != "" {
		t.Fatalf("high-R:R tier-eligible signal blocked with %q", got)
	}
	if got := pullbackSignalBlock(100, 98, 103, 0.56, riskCfg); got != "signal_confidence_tier" {
		t.Fatalf("stricter tier block = %q, want signal_confidence_tier", got)
	}
}

func TestPullbackStopTakesPrecedenceOverReplayStopOverride(t *testing.T) {
	policy := resolveExitPolicy(RunOptions{StopWidthPct: 10})
	if got := initialEntryStop(100, 98, policy.stopWidth, true); got != 98 {
		t.Fatalf("pullback stop = %.2f, want qualified low-of-day 98", got)
	}
	if got := initialEntryStop(100, 98, policy.stopWidth, false); got != 90 {
		t.Fatalf("generic overridden stop = %.2f, want 90", got)
	}
}

func TestEntryCandleExitUsesConservativeStopFirst(t *testing.T) {
	pos := newPos(98, 105)
	entryCandle := data.Candle{
		Timestamp: pos.entryTS,
		Open:      100,
		High:      106,
		Low:       97,
		Close:     104,
	}
	trade, triggered := evaluateExit(pos, entryCandle, 0, 0, exitPolicy{})
	if !triggered || trade.ExitReason != "stop_loss" || trade.ExitTS != pos.entryTS {
		t.Fatalf("entry-candle exit = %+v triggered=%v, want same-candle stop", trade, triggered)
	}
}

func TestCheckSLTP_ExitSignal(t *testing.T) {
	pos := newPos(90, 120)
	pos.exitSignal = true
	candle := makeCandle(95, 115)
	candle.Open = 102
	trade, triggered := evaluateExit(pos, candle, 0, 0, exitPolicy{})
	if !triggered {
		t.Fatal("expected exit signal trigger")
	}
	if trade.ExitReason != "signal" {
		t.Errorf("expected exit_reason signal, got %q", trade.ExitReason)
	}
	if trade.ExitPrice != 102 { // should exit at candle open
		t.Errorf("expected exit at candle open 102, got %.2f", trade.ExitPrice)
	}
}

func TestCheckSLTP_NoSLNoTP(t *testing.T) {
	pos := newPos(0, 0) // no SL or TP configured
	candle := makeCandle(50, 200)
	_, triggered := evaluateExit(pos, candle, 0, 0, exitPolicy{})
	if triggered {
		t.Error("zero SL/TP should not trigger")
	}
}

func TestResolveExitPolicy_Defaults(t *testing.T) {
	// Zero-value options reproduce baseline (no trail) with sane defaults.
	p := resolveExitPolicy(RunOptions{})
	if p.trail || p.useEMA {
		t.Errorf("empty options should be baseline, got trail=%v useEMA=%v", p.trail, p.useEMA)
	}
	if p.atrMult != 1.5 || p.emaPeriod != 9 {
		t.Errorf("expected defaults atrMult=1.5 emaPeriod=9, got %.1f/%d", p.atrMult, p.emaPeriod)
	}
	// Explicit values pass through; stopWidth converts pct→fraction.
	p2 := resolveExitPolicy(RunOptions{ExitMode: "trail", TrailRule: "ema", TrailATRMult: 2, TrailEMAPeriod: 21, ExitConfMin: 0.7, StopWidthPct: 2})
	if !p2.trail || !p2.useEMA || p2.atrMult != 2 || p2.emaPeriod != 21 || p2.exitConfMin != 0.7 || p2.stopWidth != 0.02 {
		t.Errorf("unexpected resolved policy: %+v", p2)
	}
}

// ── evaluateExit (trail / uncap / R-multiple) ─────────────────────────────────

// The live ATR rule replaces the broker stop with the ratcheted level, so it
// rests there and fills INTRABAR at its own price. Exiting at the close (the
// pre-fix behavior) both delayed the exit and credited the wrong fill.
func TestEvaluateExit_TrailATR_FillsIntrabarAtTrailPrice(t *testing.T) {
	pos := newPos(90, 0)
	pos.highWater = 130
	pos.trailStop = 120
	// Low pierces the trail intrabar; the close recovers back above it.
	candle := data.Candle{Timestamp: time.Now(), Open: 122, High: 124, Low: 115, Close: 121}
	trade, triggered := evaluateExit(pos, candle, 0 /*no ratchet*/, 0, exitPolicy{trail: true, atrMult: 1.5})
	if !triggered {
		t.Fatal("expected ATR-trail exit when the bar trades through the resting stop")
	}
	if trade.ExitReason != "trail_atr" || trade.ExitPrice != 120 {
		t.Errorf("expected trail_atr @120 (the stop price), got %q @%.2f", trade.ExitReason, trade.ExitPrice)
	}
}

// A bar that gaps open below the resting stop sweeps through it; filling at the
// stop price would credit a price that never traded.
func TestEvaluateExit_TrailATR_GapDownFillsAtOpen(t *testing.T) {
	pos := newPos(90, 0)
	pos.trailStop = 120
	candle := data.Candle{Timestamp: time.Now(), Open: 110, High: 112, Low: 105, Close: 108}
	trade, triggered := evaluateExit(pos, candle, 0, 0, exitPolicy{trail: true, atrMult: 1.5})
	if !triggered {
		t.Fatal("expected exit when the bar gaps below the resting stop")
	}
	if trade.ExitPrice != 110 {
		t.Errorf("gap-down must fill at the open (110), got %.2f", trade.ExitPrice)
	}
}

// Before the trail ratchets above the entry stop the exit is an ordinary
// stop-out, so the exit-reason split stays interpretable.
func TestEvaluateExit_TrailATR_ReasonIsStopLossBeforeRatchet(t *testing.T) {
	pos := newPos(90, 0)
	pos.trailStop = 90 // still at the entry stop
	candle := data.Candle{Timestamp: time.Now(), Open: 95, High: 96, Low: 85, Close: 88}
	trade, triggered := evaluateExit(pos, candle, 0, 0, exitPolicy{trail: true, atrMult: 1.5})
	if !triggered {
		t.Fatal("expected stop-out")
	}
	if trade.ExitReason != "stop_loss" || trade.ExitPrice != 90 {
		t.Errorf("expected stop_loss @90, got %q @%.2f", trade.ExitReason, trade.ExitPrice)
	}
}

// Lookahead guard: the stop resting during a bar is the one placed from
// previously observed prices. Ratcheting on this bar's own high before testing
// its low would let the stop clairvoyantly ride the bar it exits on.
func TestEvaluateExit_TrailATR_NoLookaheadOnRatchetBar(t *testing.T) {
	pos := newPos(90, 0)
	pos.highWater = 100
	pos.trailStop = 90
	// High 150 with ATR 10 × 1.5 implies a 135 trail. Low 130 is below that
	// implied level but far above the stop actually resting during the bar (90).
	candle := data.Candle{Timestamp: time.Now(), Open: 140, High: 150, Low: 130, Close: 145}
	_, triggered := evaluateExit(pos, candle, 10, 0, exitPolicy{trail: true, atrMult: 1.5})
	if triggered {
		t.Fatal("must not exit on the same bar whose high raised the trail (lookahead)")
	}
	if pos.trailStop != 135 {
		t.Errorf("trail should ratchet to 135 for subsequent bars, got %.2f", pos.trailStop)
	}
}

// trailStop can never loosen protection below the original hard stop; live
// ComputeNewStop clamps with max(currentStop, ...).
func TestEvaluateExit_TrailATR_NeverLoosensBelowHardStop(t *testing.T) {
	pos := newPos(90, 0)
	pos.trailStop = 88 // below the hard stop — must not widen the exit
	candle := data.Candle{Timestamp: time.Now(), Open: 95, High: 96, Low: 89, Close: 91}
	trade, triggered := evaluateExit(pos, candle, 0, 0, exitPolicy{trail: true, atrMult: 1.5})
	if !triggered {
		t.Fatal("expected the hard stop to still protect at 90")
	}
	if trade.ExitPrice != 90 {
		t.Errorf("expected fill at the hard stop 90, got %.2f", trade.ExitPrice)
	}
}

func TestEvaluateExit_TrailATR_RatchetsUp(t *testing.T) {
	pos := newPos(90, 0)
	pos.highWater = 100
	pos.trailStop = 90
	// New high 150 with ATR 10, mult 1.5 → candidate trail 150-15=135; close 140 stays above → no exit yet.
	candle := data.Candle{Timestamp: time.Now(), Open: 145, High: 150, Low: 138, Close: 140}
	_, triggered := evaluateExit(pos, candle, 10, 0, exitPolicy{trail: true, atrMult: 1.5})
	if triggered {
		t.Fatal("should not exit while close holds above the ratcheted trail")
	}
	if pos.trailStop != 135 {
		t.Errorf("expected trailStop ratcheted to 135, got %.2f", pos.trailStop)
	}
}

func TestEvaluateExit_TrailEMA(t *testing.T) {
	pos := newPos(90, 0)
	candle := data.Candle{Timestamp: time.Now(), Open: 98, High: 99, Low: 92, Close: 95} // close < ema
	trade, triggered := evaluateExit(pos, candle, 0, 97 /*ema*/, exitPolicy{trail: true, useEMA: true, emaPeriod: 9})
	if !triggered {
		t.Fatal("expected EMA-trail exit when close falls below the EMA")
	}
	if trade.ExitReason != "trail_ema" || trade.ExitPrice != 95 {
		t.Errorf("expected trail_ema @95, got %q @%.2f", trade.ExitReason, trade.ExitPrice)
	}
}

// updateOneEMA9 deliberately leaves the broker stop where it started: moving it
// to the EMA would convert a close-confirmed rule into an intraday touch rule.
// The replay must not ratchet it either, or the two arms stop being comparable.
func TestEvaluateExit_TrailEMA_StopNeverRatchets(t *testing.T) {
	pos := newPos(90, 0)
	pos.trailStop = 90
	p := exitPolicy{trail: true, useEMA: true, emaPeriod: 9, atrMult: 1.5}
	// A large up-bar that would move an ATR trail well above the entry stop.
	candle := data.Candle{Timestamp: time.Now(), Open: 150, High: 200, Low: 148, Close: 195}
	if _, triggered := evaluateExit(pos, candle, 10, 100 /*ema below close*/, p); triggered {
		t.Fatal("should hold while the close stays above the EMA")
	}
	if pos.trailStop != 90 {
		t.Errorf("EMA rule must not ratchet the stop; got %.2f, want 90", pos.trailStop)
	}
	if got := pos.activeStop(p); got != 90 {
		t.Errorf("EMA rule must keep the original hard stop resting; got %.2f, want 90", got)
	}
}

// The hard stop is a resting broker order under the EMA rule too, so a gap
// through it fills at the open rather than at the stop price.
func TestEvaluateExit_TrailEMA_HardStopGapFillsAtOpen(t *testing.T) {
	pos := newPos(90, 0)
	candle := data.Candle{Timestamp: time.Now(), Open: 85, High: 88, Low: 83, Close: 87}
	trade, triggered := evaluateExit(pos, candle, 0, 95, exitPolicy{trail: true, useEMA: true, emaPeriod: 9})
	if !triggered {
		t.Fatal("expected the hard stop to fire on the gap")
	}
	if trade.ExitReason != "stop_loss" || trade.ExitPrice != 85 {
		t.Errorf("expected stop_loss @85 (the open), got %q @%.2f", trade.ExitReason, trade.ExitPrice)
	}
}

func TestEvaluateExit_TrailUncapsTakeProfit(t *testing.T) {
	pos := newPos(90, 110) // has a take-profit at 110
	pos.trailStop = 88     // far below price → won't trigger
	// High 130 blows through the TP, but in trail mode the fixed TP is ignored.
	candle := data.Candle{Timestamp: time.Now(), Open: 118, High: 130, Low: 116, Close: 128}
	_, triggered := evaluateExit(pos, candle, 0, 0, exitPolicy{trail: true, atrMult: 1.5})
	if triggered {
		t.Error("trail mode must ignore the fixed take-profit (winners uncapped)")
	}
}

func TestClosePosition_RMultiple(t *testing.T) {
	pos := &openPos{ticker: "T", direction: "buy", entryTS: time.Now(), entryPrice: 100, quantity: 10, cost: 1000, riskPerShare: 5}
	trade := closePosition(pos, time.Now(), 130, "trail_atr")
	if trade.RiskPerShare != 5 {
		t.Errorf("expected RiskPerShare 5, got %.2f", trade.RiskPerShare)
	}
	if trade.RMultiple != 6 { // (130-100)/5
		t.Errorf("expected 6R, got %.2fR", trade.RMultiple)
	}
}

// ── closePosition ─────────────────────────────────────────────────────────────

func TestClosePosition_PnLCalculation(t *testing.T) {
	pos := newPos(90, 120)
	ts := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)
	trade := closePosition(pos, ts, 110, "take_profit")

	expectedPnL := 10*110 - 1000.0 // proceeds - cost
	expectedPct := expectedPnL / 1000 * 100

	if trade.PnL != expectedPnL {
		t.Errorf("PnL: expected %.2f, got %.2f", expectedPnL, trade.PnL)
	}
	if trade.PnLPct != expectedPct {
		t.Errorf("PnLPct: expected %.2f, got %.2f", expectedPct, trade.PnLPct)
	}
	if trade.ExitReason != "take_profit" {
		t.Errorf("exit reason: expected take_profit, got %q", trade.ExitReason)
	}
	if trade.ExitTS != ts {
		t.Errorf("exit TS mismatch")
	}
}

func TestClosePosition_ZeroCostNoPanic(t *testing.T) {
	pos := &openPos{
		ticker:    "TEST",
		direction: "long",
		quantity:  10,
		cost:      0, // zero cost — should not divide by zero
	}
	trade := closePosition(pos, time.Now(), 100, "end_of_data")
	if trade.PnLPct != 0 {
		t.Errorf("expected 0 PnLPct for zero cost, got %.2f", trade.PnLPct)
	}
}

// ── PromptCache (pure methods) ────────────────────────────────────────────────

func TestPromptCache_InitialStats(t *testing.T) {
	c := NewPromptCache()
	hits, misses := c.Stats()
	if hits != 0 || misses != 0 {
		t.Errorf("expected 0/0 stats, got %d/%d", hits, misses)
	}
	if c.HitRate() != 0 {
		t.Errorf("expected 0 hit rate, got %.2f", c.HitRate())
	}
}

func TestRequestHash_Deterministic(t *testing.T) {
	req := sampleRequest()
	h1, err1 := RequestHash(req)
	h2, err2 := RequestHash(req)
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected errors: %v / %v", err1, err2)
	}
	if h1 != h2 {
		t.Errorf("hash not deterministic: %q vs %q", h1, h2)
	}
}

func TestRequestHash_ChangeSensitive(t *testing.T) {
	req1 := sampleRequest()
	req2 := sampleRequest()
	req2.Ticker = "TSLA"

	h1, _ := RequestHash(req1)
	h2, _ := RequestHash(req2)
	if h1 == h2 {
		t.Error("different requests should produce different hashes")
	}
}

func TestRequestHash_NonEmpty(t *testing.T) {
	req := sampleRequest()
	h, err := RequestHash(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(h) != 64 { // SHA-256 hex = 64 chars
		t.Errorf("expected 64-char hex hash, got len %d", len(h))
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func sampleRequest() analysis.AnalysisRequest {
	return analysis.AnalysisRequest{
		Ticker:    "AAPL",
		Strategy:  "momentum",
		Timeframe: "1d",
	}
}

// The backtest must gate the trail on the same rule as the live manager, or the
// two disagree about which trades the trail even owns — the class of divergence
// PR #518 was written to close.
func TestRatchetRespectsActivationGate(t *testing.T) {
	pos := &openPos{entryPrice: 100, stopLoss: 95, trailStop: 95, highWater: 100}
	gated := exitPolicy{trail: true, atrMult: 1.5, activateGainPct: 5}

	// +1.5% high: below the gate, so the trail must not ratchet off it.
	pos.ratchet(data.Candle{High: 101.5, Low: 99, Close: 99}, 1.0, gated)
	if pos.trailStop != 95 {
		t.Errorf("trailStop = %v, want 95 — a +1.5%% high has not armed the trail", pos.trailStop)
	}
	if pos.activeStop(gated) != 95 {
		t.Errorf("activeStop = %v, want the original stop while disarmed", pos.activeStop(gated))
	}

	// +10% high: armed, and the trail ratchets from the high-water mark.
	pos.ratchet(data.Candle{High: 110, Low: 105, Close: 108}, 1.0, gated)
	if want := 110 - 1.5*1.0; pos.trailStop != want {
		t.Errorf("trailStop = %v, want %v once armed", pos.trailStop, want)
	}

	// Arming latches: a later low high must not disarm or lower the stop.
	pos.ratchet(data.Candle{High: 101, Low: 100, Close: 100}, 1.0, gated)
	if want := 110 - 1.5*1.0; pos.trailStop != want {
		t.Errorf("trailStop = %v, want %v — arming must latch and the stop must never widen", pos.trailStop, want)
	}

	// Discriminating bar: High 112 but Close 100. Keying arming on the close
	// instead of the high-water mark would disarm here and leave the stop at
	// 108.5 — and because ratchet only ever RAISES trailStop, the assertions
	// above cannot tell the two implementations apart. This one can.
	pos.ratchet(data.Candle{High: 112, Low: 99, Close: 100}, 1.0, gated)
	if want := 112 - 1.5*1.0; pos.trailStop != want {
		t.Errorf("trailStop = %v, want %v — arming must key on the high-water mark, not the close", pos.trailStop, want)
	}
}

func TestRatchetUngatedMatchesLegacyBehaviour(t *testing.T) {
	pos := &openPos{entryPrice: 100, stopLoss: 95, trailStop: 95, highWater: 100}
	ungated := exitPolicy{trail: true, atrMult: 1.5, activateGainPct: 0}
	pos.ratchet(data.Candle{High: 101.5, Low: 99, Close: 99}, 1.0, ungated)
	if want := 101.5 - 1.5*1.0; pos.trailStop != want {
		t.Errorf("trailStop = %v, want %v — a zero gate must trail from the first bar", pos.trailStop, want)
	}
}
