package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

func trendCandles(closes ...float64) []data.Candle {
	out := make([]data.Candle, len(closes))
	for i, closePrice := range closes {
		out[i] = data.Candle{
			Timeframe: data.Timeframe1Day,
			Timestamp: time.Time{},
			High:      closePrice + 1,
			Low:       closePrice - 1,
			Close:     closePrice,
		}
	}
	return out
}

func descendingCloses(count int, start float64) []float64 {
	out := make([]float64, count)
	for i := range out {
		out[i] = start - float64(i)
	}
	return out
}

func TestEvaluateTrendAlignmentGate_BlocksConfirmedDailyDowntrend(t *testing.T) {
	cfg := config.EntryGatesConfig{TrendAlignmentEnabled: true}
	candles := trendCandles(descendingCloses(30, 130)...)

	reason, evaluated := evaluateTrendAlignmentGate(
		"BUY", 90, data.Timeframe1Day, candles, time.Now(), cfg,
	)
	if !evaluated || reason == "" {
		t.Fatalf("confirmed daily downtrend should block: evaluated=%v reason=%q",
			evaluated, reason)
	}
	if !strings.Contains(reason, "declining daily EMA9") {
		t.Fatalf("unexpected reason: %s", reason)
	}
}

func TestStockTimeframesForEntryAddsDailyWithoutMutation(t *testing.T) {
	input := []string{"1H", "4H"}
	got := stockTimeframesForEntry(input, true)
	if strings.Join(got, ",") != "1h,4h,1d" {
		t.Fatalf("timeframes = %v", got)
	}
	if strings.Join(input, ",") != "1H,4H" {
		t.Fatalf("input mutated: %v", input)
	}
	disabled := stockTimeframesForEntry(input, false)
	if strings.Join(disabled, ",") != "1h,4h" {
		t.Fatalf("disabled timeframes = %v", disabled)
	}
	if got := primaryStockTimeframe(disabled); got != data.Timeframe1Hour {
		t.Fatalf("evidence-only daily fetch changed primary timeframe to %s", got)
	}
	if got := primaryStockTimeframe([]string{"1h", "1d"}); got != data.Timeframe1Day {
		t.Fatalf("explicit daily timeframe should remain preferred, got %s", got)
	}
}

func TestEvaluateTrendAlignmentGate_RequiresAlignedEvidence(t *testing.T) {
	cfg := config.EntryGatesConfig{TrendAlignmentEnabled: true}
	declining := trendCandles(descendingCloses(30, 130)...)
	rising := trendCandles(append(descendingCloses(22, 130),
		110, 112, 114, 116, 118, 120, 122, 124)...)

	tests := []struct {
		name       string
		action     string
		price      float64
		timeframe  data.Timeframe
		candles    []data.Candle
		cfg        config.EntryGatesConfig
		evaluated  bool
		wantReason bool
	}{
		{name: "price above EMAs passes", action: "BUY", price: 200, timeframe: data.Timeframe1Day, candles: declining, cfg: cfg, evaluated: true},
		{name: "rising EMAs pass", action: "BUY", price: 90, timeframe: data.Timeframe1Day, candles: rising, cfg: cfg, evaluated: true},
		{name: "sell out of scope", action: "SELL", price: 90, timeframe: data.Timeframe1Day, candles: declining, cfg: cfg},
		{name: "disabled", action: "BUY", price: 90, timeframe: data.Timeframe1Day, candles: declining, cfg: config.EntryGatesConfig{}},
		{name: "non-daily fails open", action: "BUY", price: 90, timeframe: data.Timeframe1Hour, candles: declining, cfg: cfg},
		{name: "thin history fails open", action: "BUY", price: 90, timeframe: data.Timeframe1Day, candles: declining[:21], cfg: cfg},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, evaluated := evaluateTrendAlignmentGate(
				tt.action, tt.price, tt.timeframe, tt.candles, time.Now(), tt.cfg,
			)
			if evaluated != tt.evaluated || (reason != "") != tt.wantReason {
				t.Fatalf("evaluated=%v reason=%q, want evaluated=%v reason=%v",
					evaluated, reason, tt.evaluated, tt.wantReason)
			}
		})
	}
}

func TestEvaluateTrendAlignmentGate_ExcludesCurrentPartialDailyBar(t *testing.T) {
	cfg := config.EntryGatesConfig{TrendAlignmentEnabled: true}
	closes := descendingCloses(30, 130)
	candles := trendCandles(append(closes, 500)...)
	now := time.Date(2026, time.July, 24, 10, 0, 0, 0, nyLoc)
	candles[len(candles)-1].Timestamp =
		time.Date(2026, time.July, 24, 0, 0, 0, 0, nyLoc)

	reason, evaluated := evaluateTrendAlignmentGate(
		"BUY", 90, data.Timeframe1Day, candles, now, cfg,
	)
	if !evaluated || reason == "" {
		t.Fatalf("partial rebound must not reverse completed trend: evaluated=%v reason=%q",
			evaluated, reason)
	}
}

func TestEvaluateRunupGates_NewsTapeConfirmation(t *testing.T) {
	cfg := config.EntryGatesConfig{NewsMinRunupPct: 2.0}

	// Price flat vs prior close — the 16%-win cohort — blocked.
	if reason := evaluateRunupGates("news_sentiment", "BUY", 100.5, 100, cfg); reason == "" {
		t.Fatal("flat tape should block a news BUY")
	} else if !strings.Contains(reason, "tape confirmation") {
		t.Fatalf("unexpected reason: %s", reason)
	}
	// Price down vs prior close — blocked.
	if reason := evaluateRunupGates("news_sentiment", "BUY", 97, 100, cfg); reason == "" {
		t.Fatal("negative reaction should block a news BUY")
	}
	// Tape confirmed (+3%) — passes.
	if reason := evaluateRunupGates("news_sentiment", "BUY", 103, 100, cfg); reason != "" {
		t.Fatalf("confirmed tape should pass, got: %s", reason)
	}
	// Exactly at the threshold — passes (gate blocks strictly below).
	if reason := evaluateRunupGates("news_sentiment", "BUY", 102, 100, cfg); reason != "" {
		t.Fatalf("at-threshold should pass, got: %s", reason)
	}
}

func TestEvaluateRunupGates_MomentumAntiChase(t *testing.T) {
	cfg := config.EntryGatesConfig{MomentumMaxRunupPct: 2.0}

	// Chasing a +5% run-up — blocked.
	if reason := evaluateRunupGates("momentum", "BUY", 105, 100, cfg); reason == "" {
		t.Fatal("chasing momentum BUY should be blocked")
	} else if !strings.Contains(reason, "anti-chase") {
		t.Fatalf("unexpected reason: %s", reason)
	}
	// Not chasing (+1%) — passes.
	if reason := evaluateRunupGates("momentum", "BUY", 101, 100, cfg); reason != "" {
		t.Fatalf("un-chased momentum BUY should pass, got: %s", reason)
	}
	// Exactly at the cap — passes (gate blocks strictly above).
	if reason := evaluateRunupGates("momentum", "BUY", 102, 100, cfg); reason != "" {
		t.Fatalf("at-cap should pass, got: %s", reason)
	}
}

func TestEvaluateRunupGates_ScopeAndFailOpen(t *testing.T) {
	cfg := config.EntryGatesConfig{NewsMinRunupPct: 2.0, MomentumMaxRunupPct: 2.0}

	// SELL signals are never gated.
	if reason := evaluateRunupGates("news_sentiment", "SELL", 100, 100, cfg); reason != "" {
		t.Fatalf("SELL must not be gated, got: %s", reason)
	}
	// Other strategies are never gated.
	if reason := evaluateRunupGates("trend_following", "BUY", 100, 100, cfg); reason != "" {
		t.Fatalf("ungated strategy blocked: %s", reason)
	}
	// Unknown prior close fails open.
	if reason := evaluateRunupGates("news_sentiment", "BUY", 100, 0, cfg); reason != "" {
		t.Fatalf("missing prior close must fail open, got: %s", reason)
	}
	// Zero-value config disables everything.
	if reason := evaluateRunupGates("news_sentiment", "BUY", 100, 100, config.EntryGatesConfig{}); reason != "" {
		t.Fatalf("disabled gates blocked: %s", reason)
	}
	// Momentum gate does not apply the news minimum and vice versa.
	if reason := evaluateRunupGates("momentum", "BUY", 100.5, 100, cfg); reason != "" {
		t.Fatalf("momentum flat tape should pass (min-runup is news-only), got: %s", reason)
	}
}

func TestDeferredPriceHeld(t *testing.T) {
	// Held: within the 1% default tolerance.
	if !deferredPriceHeld(100, 99.5, 0) {
		t.Fatal("price within default tolerance should hold")
	}
	// Dropped past tolerance.
	if deferredPriceHeld(100, 98.5, 1.0) {
		t.Fatal("price below tolerance should not hold")
	}
	// Price above signal always holds.
	if !deferredPriceHeld(100, 102, 1.0) {
		t.Fatal("price above signal should hold")
	}
	// Wider explicit tolerance.
	if !deferredPriceHeld(100, 98.5, 2.0) {
		t.Fatal("price within explicit 2% tolerance should hold")
	}
	// Garbage inputs never hold.
	if deferredPriceHeld(0, 100, 1.0) || deferredPriceHeld(100, 0, 1.0) {
		t.Fatal("non-positive prices must not confirm")
	}
}

func TestSameMarketDay(t *testing.T) {
	et, _ := time.LoadLocation("America/New_York")
	// 23:30 ET vs 00:30 ET next day — different market days even though
	// they may share a UTC day.
	a := time.Date(2026, 6, 1, 23, 30, 0, 0, et)
	b := time.Date(2026, 6, 2, 0, 30, 0, 0, et)
	if sameMarketDay(a, b, et) {
		t.Fatal("ET midnight boundary should split market days")
	}
	// Same ET day expressed in UTC (evening UTC = same ET afternoon).
	c := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC) // 10:00 ET
	d := time.Date(2026, 6, 1, 20, 0, 0, 0, time.UTC) // 16:00 ET
	if !sameMarketDay(c, d, et) {
		t.Fatal("same ET session should be the same market day")
	}
	// 01:00 UTC next day is still the prior ET evening.
	f := time.Date(2026, 6, 2, 1, 0, 0, 0, time.UTC) // 21:00 ET Jun 1
	if !sameMarketDay(c, f, et) {
		t.Fatal("UTC rollover before ET midnight is the same market day")
	}
}
