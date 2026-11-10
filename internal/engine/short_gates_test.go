package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// trendBars returns 40 completed daily bars ending yesterday whose closes
// move by step each session from start (negative step = downtrend).
func trendBars(ticker string, now time.Time, start, step float64) []data.Candle {
	et := now.In(nyLoc)
	today := time.Date(et.Year(), et.Month(), et.Day(), 0, 0, 0, 0, nyLoc)
	var bars []data.Candle
	for i := 40; i >= 1; i-- {
		c := start + step*float64(40-i)
		bars = append(bars, data.Candle{Ticker: ticker, Timeframe: data.Timeframe1Day,
			Timestamp: today.AddDate(0, 0, -i), Open: c, High: c + 1, Low: c - 1, Close: c, Volume: 200_000})
	}
	return bars
}

func TestEvaluateShortTrendGate(t *testing.T) {
	now := time.Now()
	down := trendBars("X", now, 140, -1) // 140 → 101
	if r := evaluateShortTrendGate(100, down, now); r != "" {
		t.Fatalf("confirmed downtrend blocked: %s", r)
	}
	up := trendBars("X", now, 60, 1)
	if r := evaluateShortTrendGate(100, up, now); !strings.Contains(r, "not a confirmed downtrend") {
		t.Fatalf("uptrend not blocked: %q", r)
	}
	if r := evaluateShortTrendGate(100, down[:10], now); !strings.Contains(r, "unavailable") {
		t.Fatalf("missing evidence must block a short: %q", r)
	}
}

func TestEvaluateShortRunupGates(t *testing.T) {
	cfg := config.EntryGatesConfig{NewsMinRunupPct: 1, MomentumMaxRunupPct: 5}
	if r := evaluateShortRunupGates("momentum", 94, 100, cfg); !strings.Contains(r, "chase the drop") {
		t.Fatalf("momentum short after a 6%% drop: %q", r)
	}
	if r := evaluateShortRunupGates("momentum", 97, 100, cfg); r != "" {
		t.Fatalf("momentum short after a 3%% drop blocked: %q", r)
	}
	if r := evaluateShortRunupGates("news_sentiment", 99.5, 100, cfg); !strings.Contains(r, "not confirmed the bearish headline") {
		t.Fatalf("news short without tape confirmation: %q", r)
	}
	if r := evaluateShortRunupGates("news_sentiment", 98, 100, cfg); r != "" {
		t.Fatalf("news short with tape confirmation blocked: %q", r)
	}
	if r := evaluateShortRunupGates("momentum", 94, 0, cfg); r != "" {
		t.Fatal("unknown prior close must fail open like the long gate")
	}
}

// End to end at the release boundary: with trend alignment on, a short in
// an uptrend is blocked with the short release gate recorded; a short in a
// confirmed downtrend is placed.
func TestShortEntryReleaseTrendGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		step   float64
		start  float64
		placed int
	}{
		{"uptrend blocks", 1, 60, 0},
		{"downtrend places", -1, 138, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShortFixture(t, "SHTG")
			f.e.cfg.Risk.EntryGates.TrendAlignmentEnabled = true
			f.market.bars = trendBars(f.ticker, time.Now(), tc.start, tc.step)
			id, sig := f.signal()
			f.e.executeShortEntry(f.ctx, id, sig, false)
			if n := len(f.broker.placedOrders()); n != tc.placed {
				status, reason := f.signalStatus(id)
				t.Fatalf("placed=%d want %d (status %s: %s)", n, tc.placed, status, reason)
			}
			got, _ := store.GetSignalByID(f.ctx, id)
			if got == nil {
				t.Fatal("signal missing")
			}
			var decisions []store.EntryGateDecision
			if err := json.Unmarshal(got.EntryGateDecisions, &decisions); err != nil {
				t.Fatalf("decode gate audit: %v (%s)", err, got.EntryGateDecisions)
			}
			wantStatus := "passed"
			if tc.placed == 0 {
				wantStatus = "blocked"
			}
			n := 0
			for _, d := range decisions {
				if d.Name == shortGateTrend+shortGateReleaseName {
					n++
					if d.Status != wantStatus {
						t.Fatalf("trend release decision status = %s, want %s", d.Status, wantStatus)
					}
				}
			}
			if n != 1 {
				t.Fatalf("trend release decision recorded %d times, want once: %s", n, got.EntryGateDecisions)
			}
		})
	}
}

// End to end at signal generation: a Jev SHORT in an uptrend is stored as a
// blocked signal naming the short trend gate, and never executes.
func TestGenerateSignalShortTrendGateBlocks(t *testing.T) {
	f := newShortFixture(t, "SHGG")
	f.e.cfg.Risk.EntryGates.TrendAlignmentEnabled = true
	f.market.bars = trendBars(f.ticker, time.Now(), 60, 1)
	resp := analysis.AnalysisResponse{
		Action: analysis.ActionShort, Confidence: 0.8, SizePct: 1.5,
		StopLoss: 101, TakeProfit: 92, SelectedStrategy: "momentum",
	}
	candles := []data.Candle{{Ticker: f.ticker, Timeframe: data.Timeframe1Day, Timestamp: time.Now(), Close: 98}}
	f.e.generateSignal(context.Background(), f.ticker, "1d", "momentum", []string{"momentum"}, candles, resp, nil, "", nil, "", "", false)
	var status, reason, gates string
	if err := store.GetDB().QueryRow(f.ctx, `SELECT status::text, COALESCE(block_reason,''), COALESCE(entry_gate_decisions::text,'') FROM signals WHERE ticker=$1`, f.ticker).Scan(&status, &reason, &gates); err != nil {
		t.Fatal(err)
	}
	if status != "blocked" || !strings.Contains(reason, "trend alignment (short)") || !strings.Contains(gates, shortGateTrend) {
		t.Fatalf("status=%s reason=%q gates=%s", status, reason, gates)
	}
	if len(f.broker.placedOrders()) != 0 {
		t.Fatal("a gated SHORT placed an order")
	}
}

// sessionBars builds n completed daily bars on consecutive real market
// sessions ending at the most recent completed session, using close(i)
// (i = 0 oldest) and volume(i).
func sessionBars(ticker string, now time.Time, n int, closeAt func(int) float64, volumeAt func(int) int64) []data.Candle {
	sessions := make([]time.Time, 0, n)
	s := MostRecentCompletedMarketSession(now, 0)
	for len(sessions) < n {
		sessions = append(sessions, s.Date)
		for d := s.Date.AddDate(0, 0, -1); ; d = d.AddDate(0, 0, -1) {
			if prev, ok := MarketSessionForDate(d); ok {
				s = prev
				break
			}
		}
	}
	bars := make([]data.Candle, n)
	for i := 0; i < n; i++ {
		c := closeAt(i)
		bars[i] = data.Candle{Ticker: ticker, Timeframe: data.Timeframe1Day, Timestamp: sessions[n-1-i],
			Open: c, High: c * 1.01, Low: c * 0.99, Close: c, Volume: volumeAt(i)}
	}
	return bars
}

// The short anti-chase gate blocks a short into a weak-volume capitulation
// (sharp multi-session drop, oversold), and the snapshot is a short one: side,
// lower band, and the newest bearish catalyst.
func TestShortAntiChaseBlocksCapitulationAndUsesBearishCatalyst(t *testing.T) {
	now := time.Now()
	n := 260
	bars := sessionBars("CAPX", now, n, func(i int) float64 {
		if i >= n-4 {
			return 100 - float64(i-(n-4)+1)*4 // a 16% four-session collapse
		}
		return 100
	}, func(i int) int64 {
		if i == n-1 {
			return 50_000 // weak volume on the final session
		}
		return 200_000
	})
	mark := bars[n-1].Close
	bearishAt := now.Add(-2 * time.Hour)
	bullishAt := now.Add(-1 * time.Hour)
	ev := shortGateEvidence{Daily: bars, News: []analysis.NewsSnippet{
		{Headline: "guidance cut", Sentiment: "bearish", PublishedAt: bearishAt, Source: "wire"},
		{Headline: "upgrade", Sentiment: "bullish", PublishedAt: bullishAt, Source: "wire"},
	}}
	cfg := config.EntryGatesConfig{AntiChase: config.AntiChaseConfig{
		Enabled: true, MomentumExhaustionEnabled: true,
		WeakVolumeRatioMax: .75, RSIMin: 70, StochKMin: 80, ExhaustionReturn3Pct: 5,
	}}
	reason, failed, decisions := evaluateShortGates(cfg, "CAPX", "momentum", mark, "test", ev, now, true)
	if !strings.Contains(reason, "momentum_capitulation") || failed != shortGateAntiChase+shortGateReleaseName {
		t.Fatalf("reason=%q failed=%q decisions=%+v", reason, failed, decisions)
	}
	snap := buildShortAntiChaseSnapshot("CAPX", bars, mark, "test", ev.News, now)
	if snap.Side != "short" || snap.BBLower <= 0 || snap.CatalystAt == nil || !snap.CatalystAt.Equal(bearishAt.UTC()) {
		t.Fatalf("short snapshot = %+v; want side short, a lower band, and the bearish catalyst", snap)
	}
	long := buildAntiChaseSnapshot("CAPX", bars, mark, "test", ev.News, now)
	if long.Side != "" || long.BBLower != 0 || long.CatalystAt == nil || !long.CatalystAt.Equal(bullishAt.UTC()) {
		t.Fatalf("long snapshot changed: %+v", long)
	}
}

// Reviewer finding: after 16:00 ET today's bar is completed, but the run-up
// gates' "prior close" must still be the previous session's close.
func TestPriorSessionCloseIgnoresTodaysBar(t *testing.T) {
	et := time.Now().In(nyLoc)
	today := time.Date(et.Year(), et.Month(), et.Day(), 0, 0, 0, 0, nyLoc)
	afterClose := today.Add(17 * time.Hour)
	bars := []data.Candle{
		{Timestamp: today.AddDate(0, 0, -2), Close: 90},
		{Timestamp: today.AddDate(0, 0, -1), Close: 100},
		{Timestamp: today, Close: 94},
	}
	if got := priorSessionClose(bars, afterClose); got != 100 {
		t.Fatalf("prior close after the bell = %v, want 100 (not today's 94)", got)
	}
	cfg := config.EntryGatesConfig{MomentumMaxRunupPct: 5}
	if r := evaluateShortRunupGates("momentum", 94, priorSessionClose(bars, afterClose), cfg); !strings.Contains(r, "chase the drop") {
		t.Fatalf("after-close momentum chase not caught: %q", r)
	}
}

// A neutral short passes anti-chase; unavailable evidence blocks it.
func TestShortAntiChasePassAndUnavailable(t *testing.T) {
	now := time.Now()
	n := 260
	bars := sessionBars("NEUT", now, n, func(i int) float64 { return 100 - float64(i)*0.05 },
		func(int) int64 { return 200_000 })
	cfg := config.EntryGatesConfig{AntiChase: config.AntiChaseConfig{
		Enabled: true, StaleCatalystRunupEnabled: true, PriorHighExtensionEnabled: true, MomentumExhaustionEnabled: true,
		StaleAfterHours: 24, MinReturn3SessionPct: 8, WeakVolumeRatioMax: .75, StaleEMAExtensionATR: 1.75,
		NearPriorHighPct: 5, HighEMAExtensionATR: 2, RSIMin: 70, StochKMin: 80, ExhaustionReturn3Pct: 5,
	}}
	if reason, _, d := evaluateShortGates(cfg, "NEUT", "momentum", bars[n-1].Close, "test", shortGateEvidence{Daily: bars}, now, false); reason != "" {
		t.Fatalf("neutral short blocked: %q %+v", reason, d)
	}
	reason, failed, _ := evaluateShortGates(cfg, "NEUT", "momentum", 95, "test", shortGateEvidence{Reason: "anti-chase: unable to refresh daily evidence"}, now, false)
	if reason == "" || failed != shortGateAntiChase {
		t.Fatalf("unavailable evidence must block a short: reason=%q failed=%q", reason, failed)
	}
}
