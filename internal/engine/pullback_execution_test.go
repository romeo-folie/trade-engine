package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

type pullbackEvidenceMarket struct {
	quote     *data.Quote
	quoteErr  error
	trades    []data.MarketTrade
	tradesErr error
	fetchFrom time.Time
}

func (m *pullbackEvidenceMarket) FetchQuote(context.Context, string) (*data.Quote, error) {
	return m.quote, m.quoteErr
}

func (m *pullbackEvidenceMarket) FetchCandles(
	_ context.Context, _ string, _ data.Timeframe, from, _ time.Time,
) ([]data.Candle, error) {
	m.fetchFrom = from
	return nil, nil
}

func (*pullbackEvidenceMarket) FetchHistory(
	context.Context, string, data.Timeframe, int,
) ([]data.Candle, error) {
	return nil, nil
}

func (m *pullbackEvidenceMarket) FetchTrades(
	_ context.Context, _ string, from, _ time.Time,
) ([]data.MarketTrade, error) {
	m.fetchFrom = from
	return m.trades, m.tradesErr
}

func freshEvidenceQuote(now time.Time, mark float64) *data.Quote {
	return &data.Quote{
		Bid: mark - 0.02, Ask: mark, Last: mark - 0.01,
		Timestamp: now.Add(-time.Second),
	}
}

func TestPullbackExecutionEvidenceIgnoresSignalMinuteFlush(t *testing.T) {
	now := time.Date(2026, 7, 25, 14, 0, 40, 0, time.UTC)
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
	since := time.Date(2026, 7, 25, 14, 0, 30, 0, time.UTC)
	now := time.Date(2026, 7, 25, 14, 2, 30, 0, time.UTC)
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
