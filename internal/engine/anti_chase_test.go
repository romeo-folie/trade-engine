package engine

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

func antiChaseDailyCandles(count int, now time.Time) []data.Candle {
	candles := make([]data.Candle, count)
	latest := MostRecentCompletedMarketSession(now, 0)
	sessions := make([]USMarketSession, count)
	for i := count - 1; i >= 0; i-- {
		sessions[i] = latest
		for candidate := latest.Date.AddDate(0, 0, -1); ; candidate = candidate.AddDate(0, 0, -1) {
			if previous, ok := MarketSessionForDate(candidate); ok {
				latest = previous
				break
			}
		}
	}
	for i, session := range sessions {
		closePrice := 50 + float64(i)*.1
		candles[i] = data.Candle{
			Ticker: "TEST", Timeframe: data.Timeframe1Day,
			Timestamp: session.Close, Open: closePrice - .2,
			High: closePrice + .5, Low: closePrice - .5, Close: closePrice,
			Volume: 1000,
		}
	}
	return candles
}

func TestBuildAntiChaseSnapshotUsesCompletedDailyHistory(t *testing.T) {
	now := time.Date(2026, time.August, 24, 18, 0, 0, 0, time.UTC)
	candles := antiChaseDailyCandles(253, now)
	mark := candles[len(candles)-1].Close
	snapshot := buildAntiChaseSnapshot("TEST", candles, mark, "test", nil, now)
	if snapshot.EvidenceStatus != "available" {
		t.Fatalf("snapshot evidence = %q (%s)", snapshot.EvidenceStatus, snapshot.EvidenceReason)
	}
	if snapshot.HistoryScope != "true_52w" || snapshot.CompletedSessions != 253 {
		t.Fatalf("history scope=%q sessions=%d, want true_52w/253", snapshot.HistoryScope, snapshot.CompletedSessions)
	}
	if snapshot.PriorHigh52Week <= 0 || snapshot.PriorHighProximity <= 0 {
		t.Fatalf("prior high evidence missing: %+v", snapshot)
	}
}

func TestBuildAntiChaseSnapshotFailsClosedForShortHistory(t *testing.T) {
	now := time.Date(2026, time.August, 24, 18, 0, 0, 0, time.UTC)
	candles := antiChaseDailyCandles(20, now)
	snapshot := buildAntiChaseSnapshot("TEST", candles, 55, "test", nil, now)
	if snapshot.EvidenceStatus != "unavailable" || snapshot.CompletedSessions != 20 {
		t.Fatalf("short history snapshot = %+v", snapshot)
	}
}

func TestBuildAntiChaseSnapshotPartialSessionUsesThreeSessionReference(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc)
	candles := antiChaseDailyCandles(253, now)
	// Today's incomplete bar follows, rather than replaces, the latest
	// completed Friday session.
	candles = append(candles, data.Candle{Ticker: "TEST", Timeframe: data.Timeframe1Day, Timestamp: now, Open: 100, High: 101, Low: 99, Close: 100, Volume: 1000})
	mark := 100.0
	snapshot := buildAntiChaseSnapshot("TEST", candles, mark, "test", nil, now)
	if snapshot.EvidenceStatus != "available" || snapshot.CompletedSessions != 253 {
		t.Fatalf("partial-session snapshot = %+v", snapshot)
	}
	want := (mark - candles[250].Close) / candles[250].Close * 100
	if math.Abs(snapshot.Return3SessionPct-want) > 1e-9 {
		t.Fatalf("three-session return = %.12f, want %.12f", snapshot.Return3SessionPct, want)
	}
}

func TestBuildAntiChaseSnapshotLiveSessionUsesThreeSessionReferenceWithoutPartialBar(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc)
	candles := antiChaseDailyCandles(253, now)
	mark := 100.0
	snapshot := buildAntiChaseSnapshot("TEST", candles, mark, "test", nil, now)
	if snapshot.EvidenceStatus != "available" || snapshot.CompletedSessions != len(candles) {
		t.Fatalf("completed-bar live-session snapshot = %+v", snapshot)
	}
	want := (mark - candles[250].Close) / candles[250].Close * 100
	if math.Abs(snapshot.Return3SessionPct-want) > 1e-9 {
		t.Fatalf("three-session return = %.12f, want %.12f", snapshot.Return3SessionPct, want)
	}
}

func TestBuildAntiChaseSnapshotLiveSessionIncludesYesterdayInPriorHigh(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc)
	candles := antiChaseDailyCandles(253, now)
	// Today's mark is intraday, so yesterday remains a valid prior session even
	// when it set a higher wick than its close.
	candles[len(candles)-1].High = 150
	snapshot := buildAntiChaseSnapshot("TEST", candles, 145, "test", nil, now)
	if snapshot.PriorHigh52Week != 150 {
		t.Fatalf("live-session prior high = %.2f, want yesterday's 150.00", snapshot.PriorHigh52Week)
	}
}

func TestBuildAntiChaseSnapshotIncludesEarlyCloseBarAfterClose(t *testing.T) {
	now := time.Date(2024, time.November, 29, 14, 0, 0, 0, nyLoc) // Black Friday, 13:00 close.
	candles := antiChaseDailyCandles(253, now)
	candles[len(candles)-1].Timestamp = now
	mark := candles[len(candles)-1].Close
	snapshot := buildAntiChaseSnapshot("TEST", candles, mark, "test", nil, now)
	if snapshot.CompletedSessions != len(candles) {
		t.Fatalf("early-close completed sessions = %d, want %d", snapshot.CompletedSessions, len(candles))
	}
	want := (mark - candles[249].Close) / candles[249].Close * 100
	if math.Abs(snapshot.Return3SessionPct-want) > 1e-9 {
		t.Fatalf("early-close three-session return = %.12f, want %.12f", snapshot.Return3SessionPct, want)
	}
}

func TestBuildAntiChaseSnapshotRejectsStaleDailyHistory(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc) // Monday; Friday is latest completed session.
	candles := antiChaseDailyCandles(253, now)
	candles[len(candles)-1].Timestamp = candles[len(candles)-2].Timestamp // Thursday instead of Friday.
	snapshot := buildAntiChaseSnapshot("TEST", candles, 100, "test", nil, now)
	if snapshot.EvidenceStatus != "unavailable" || snapshot.EvidenceReason == "" {
		t.Fatalf("stale history snapshot = %+v", snapshot)
	}
}

func TestBuildAntiChaseSnapshotRejectsUntimestampedDailyHistory(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc)
	candles := antiChaseDailyCandles(253, now)
	candles[len(candles)-1].Timestamp = time.Time{}
	snapshot := buildAntiChaseSnapshot("TEST", candles, 100, "test", nil, now)
	if snapshot.EvidenceStatus != "unavailable" {
		t.Fatalf("untimestamped history snapshot = %+v", snapshot)
	}
}

func TestBuildAnalysisAntiChaseSnapshotFailsClosedOnlyForEnabledCatalystRule(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc)
	candles := antiChaseDailyCandles(253, now)
	failed := buildAnalysisAntiChaseSnapshot("TEST", candles, 100, "test", nil, context.DeadlineExceeded, true, now)
	if failed.EvidenceStatus != "unavailable" || failed.EvidenceReason == "" {
		t.Fatalf("enabled catalyst rule accepted unread news: %+v", failed)
	}
	disabled := buildAnalysisAntiChaseSnapshot("TEST", candles, 100, "test", nil, context.DeadlineExceeded, false, now)
	if disabled.EvidenceStatus != "available" {
		t.Fatalf("disabled catalyst rule should retain daily evidence: %+v", disabled)
	}
	empty := buildAnalysisAntiChaseSnapshot("TEST", candles, 100, "test", nil, nil, true, now)
	if empty.EvidenceStatus != "available" {
		t.Fatalf("empty successful news read should remain valid: %+v", empty)
	}
}

func TestFreshAntiChaseAskRejectsStaleQuote(t *testing.T) {
	now := time.Now().UTC()
	engine := &Engine{marketData: &pullbackEvidenceMarket{
		quote: &data.Quote{Ticker: "TEST", Bid: 99, Ask: 100, Timestamp: now.Add(-time.Minute)},
	}}
	if got := engine.freshAntiChaseAsk(context.Background(), "TEST"); got != 0 {
		t.Fatalf("stale quote ask = %.2f, want rejected", got)
	}
	engine.marketData = &pullbackEvidenceMarket{
		quote: &data.Quote{Ticker: "TEST", Bid: 99, Ask: 100, Timestamp: now},
	}
	if got := engine.freshAntiChaseAsk(context.Background(), "TEST"); got != 100 {
		t.Fatalf("fresh quote ask = %.2f, want 100", got)
	}
}

func TestAntiChaseNewsEvidenceFailsClosedOnlyForEnabledCatalystRule(t *testing.T) {
	if _, reason := antiChaseNewsEvidence(nil, context.DeadlineExceeded, true); reason == "" {
		t.Fatal("missing catalyst evidence passed with stale-catalyst rule enabled")
	}
	if news, reason := antiChaseNewsEvidence(nil, context.DeadlineExceeded, false); reason != "" || len(news) != 0 {
		t.Fatalf("disabled catalyst rule should tolerate missing news: news=%v reason=%q", news, reason)
	}
}
