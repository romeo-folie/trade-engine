package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type pullbackEvidenceMarket struct {
	quote      *data.Quote
	quotes     []*data.Quote
	quoteCall  int
	quoteErr   error
	trades     []data.MarketTrade
	tradesErr  error
	fetchFrom  time.Time
	fetches    [][2]time.Time
	history    []data.Candle
	historyErr error
}

func (m *pullbackEvidenceMarket) FetchQuote(context.Context, string) (*data.Quote, error) {
	if len(m.quotes) > 0 {
		i := m.quoteCall
		if i >= len(m.quotes) {
			i = len(m.quotes) - 1
		}
		m.quoteCall++
		return m.quotes[i], m.quoteErr
	}
	return m.quote, m.quoteErr
}

func (m *pullbackEvidenceMarket) FetchCandles(
	_ context.Context, _ string, _ data.Timeframe, from, _ time.Time,
) ([]data.Candle, error) {
	m.fetchFrom = from
	return nil, nil
}

func (m *pullbackEvidenceMarket) FetchHistory(
	context.Context, string, data.Timeframe, int,
) ([]data.Candle, error) {
	return m.history, m.historyErr
}

func (m *pullbackEvidenceMarket) FetchTrades(
	_ context.Context, _ string, from, to time.Time,
) ([]data.MarketTrade, error) {
	m.fetchFrom = from
	m.fetches = append(m.fetches, [2]time.Time{from, to})
	return m.trades, m.tradesErr
}

func freshEvidenceQuote(now time.Time, mark float64) *data.Quote {
	return &data.Quote{
		Bid: mark - 0.02, Ask: mark, Last: mark - 0.01,
		Timestamp: now.Add(-time.Second),
	}
}

func TestPullbackExecutionEvidenceIgnoresSignalMinuteFlush(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-10 * time.Second)
	market := &pullbackEvidenceMarket{
		quote: freshEvidenceQuote(now, 101),
		trades: []data.MarketTrade{{
			Timestamp: since.Add(-time.Nanosecond), Price: 95,
		}},
	}
	engine := &Engine{marketData: market}

	got, err := engine.pullbackExecutionEvidence(
		context.Background(), "TEST", since, now,
	)
	if err != nil {
		t.Fatalf("pullbackExecutionEvidence: %v", err)
	}
	if got.Mark != 101 || got.MinimumTraded != 101 {
		t.Fatalf("same-minute evidence = %+v, want quote-only 101", got)
	}
	if !market.fetchFrom.Equal(since) {
		t.Fatalf("exact path started at %s, want %s", market.fetchFrom, since)
	}
}

func TestPullbackExecutionEvidenceTracksOnlyPostSignalTrades(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-2 * time.Minute)
	market := &pullbackEvidenceMarket{
		quote: freshEvidenceQuote(now, 103),
		trades: []data.MarketTrade{
			{Timestamp: since.Add(-time.Nanosecond), Price: 90},
			{Timestamp: since, Price: 97.5},
			{Timestamp: since.Add(time.Minute), Price: 102},
		},
	}
	engine := &Engine{marketData: market}

	got, err := engine.pullbackExecutionEvidence(
		context.Background(), "TEST", since, now,
	)
	if err != nil {
		t.Fatalf("pullbackExecutionEvidence: %v", err)
	}
	if got.Mark != 103 || got.MinimumTraded != 97.5 {
		t.Fatalf("post-signal evidence = %+v, want mark 103/min 97.5", got)
	}
	if !market.fetchFrom.Equal(since) {
		t.Fatalf("path started at %s, want %s", market.fetchFrom, since)
	}
}

func TestPullbackExecutionEvidenceExtendsThroughRefreshedQuote(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-2 * time.Minute)
	firstBoundary := now.Add(-5 * time.Second)
	secondBoundary := now.Add(-time.Second)
	market := &pullbackEvidenceMarket{
		quotes: []*data.Quote{
			{Bid: 102.98, Ask: 103, Last: 102.99, Timestamp: firstBoundary},
			{Bid: 101.98, Ask: 102, Last: 101.99, Timestamp: secondBoundary},
		},
		trades: []data.MarketTrade{
			{Timestamp: firstBoundary.Add(time.Second), Price: 97.5},
		},
	}
	engine := &Engine{marketData: market}

	got, err := engine.pullbackExecutionEvidence(
		context.Background(), "TEST", since, now,
	)
	if err != nil {
		t.Fatalf("pullbackExecutionEvidence: %v", err)
	}
	if got.Mark != 102 || got.MinimumTraded != 97.5 {
		t.Fatalf("refreshed evidence = %+v, want mark 102/min 97.5", got)
	}
	if len(market.fetches) != 2 {
		t.Fatalf("trade path fetches = %d, want initial plus extension", len(market.fetches))
	}
	extension := market.fetches[1]
	if !extension[0].Equal(firstBoundary) || !extension[1].Equal(secondBoundary) {
		t.Fatalf("extension = [%s,%s], want [%s,%s]",
			extension[0], extension[1], firstBoundary, secondBoundary)
	}
}

func TestPullbackExecutionEvidenceFailsClosedOnStaleQuote(t *testing.T) {
	now := time.Now().UTC()
	market := &pullbackEvidenceMarket{
		quote: &data.Quote{
			Bid: 100, Ask: 101, Last: 100.5,
			Timestamp: now.Add(-3 * time.Minute),
		},
	}
	engine := &Engine{marketData: market}
	if _, err := engine.pullbackExecutionEvidence(
		context.Background(), "TEST", now.Add(-time.Minute), now,
	); err == nil {
		t.Fatal("stale quote accepted")
	}
}

func TestPullbackExecutionEvidencePropagatesQuoteFailure(t *testing.T) {
	now := time.Now().UTC()
	engine := &Engine{marketData: &pullbackEvidenceMarket{quoteErr: errors.New("down")}}
	if _, err := engine.pullbackExecutionEvidence(
		context.Background(), "TEST", now.Add(-time.Minute), now,
	); err == nil {
		t.Fatal("quote failure accepted")
	}
}

func TestPullbackDailyRefreshReturnsObservationBoundary(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	candleTS := time.Now().UTC().Truncate(24 * time.Hour)
	market := &pullbackEvidenceMarket{
		history: []data.Candle{{
			Ticker: "TEST_PB_OBS", Timeframe: data.Timeframe1Day,
			Timestamp: candleTS, Open: 100, High: 103, Low: 98,
			Close: 102, Volume: 1000,
		}},
	}
	engine := &Engine{marketData: market, bus: events.New()}

	before := time.Now()
	observedAt, err := engine.fetchAndStoreCandlesObserved(
		ctx, "TEST_PB_OBS", data.Timeframe1Day,
	)
	after := time.Now()
	if err != nil {
		t.Fatalf("refresh daily evidence: %v", err)
	}
	if observedAt.Before(before) || observedAt.After(after) {
		t.Fatalf("observation boundary %s outside refresh interval [%s,%s]",
			observedAt, before, after)
	}
	stored, err := store.GetCandles(
		ctx, "TEST_PB_OBS", data.Timeframe1Day,
		candleTS.Add(-time.Hour), candleTS.Add(time.Hour),
	)
	if err != nil || len(stored) != 1 || stored[0].Low != 98 {
		t.Fatalf("refreshed daily snapshot = %+v, err=%v", stored, err)
	}
}

func TestPullbackDailyRefreshRejectsEmptySnapshot(t *testing.T) {
	engine := &Engine{
		marketData: &pullbackEvidenceMarket{},
		bus:        events.New(),
	}
	if _, err := engine.fetchAndStoreCandlesObserved(
		context.Background(), "TEST", data.Timeframe1Day,
	); err == nil {
		t.Fatal("empty daily snapshot accepted")
	}
}
