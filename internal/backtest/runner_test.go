package backtest

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
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
		{Timestamp: from.Add(12 * time.Hour)},   // within 2d tolerance
		{Timestamp: to.Add(-12 * time.Hour)},    // within 2d tolerance
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
		{Timestamp: from.Add(72 * time.Hour + time.Minute)},
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

func TestEvaluateExit_TrailATR_ClosesBelowTrail(t *testing.T) {
	pos := newPos(90, 0)
	pos.highWater = 130
	pos.trailStop = 120
	candle := data.Candle{Timestamp: time.Now(), Open: 122, High: 124, Low: 115, Close: 118} // close < trailStop
	trade, triggered := evaluateExit(pos, candle, 0 /*no ratchet*/, 0, exitPolicy{trail: true, atrMult: 1.5})
	if !triggered {
		t.Fatal("expected ATR-trail exit when close falls below the trailing stop")
	}
	if trade.ExitReason != "trail_atr" || trade.ExitPrice != 118 {
		t.Errorf("expected trail_atr @118, got %q @%.2f", trade.ExitReason, trade.ExitPrice)
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

	expectedPnL := 10*110 - 1000.0   // proceeds - cost
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
