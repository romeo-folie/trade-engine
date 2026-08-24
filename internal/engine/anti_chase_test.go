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
	start := now.AddDate(0, 0, -count-2)
	for i := range candles {
		closePrice := 50 + float64(i)*.1
		candles[i] = data.Candle{
			Ticker: "TEST", Timeframe: data.Timeframe1Day,
			Timestamp: start.AddDate(0, 0, i), Open: closePrice - .2,
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
	// The last bar is today's incomplete daily session and must not become a
	// fourth-session-old reference merely because its mark is used live.
	candles[len(candles)-1].Timestamp = now
	mark := 100.0
	snapshot := buildAntiChaseSnapshot("TEST", candles, mark, "test", nil, now)
	if snapshot.EvidenceStatus != "available" || snapshot.CompletedSessions != 252 {
		t.Fatalf("partial-session snapshot = %+v", snapshot)
	}
	want := (mark - candles[249].Close) / candles[249].Close * 100
	if math.Abs(snapshot.Return3SessionPct-want) > 1e-9 {
		t.Fatalf("three-session return = %.12f, want %.12f", snapshot.Return3SessionPct, want)
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
