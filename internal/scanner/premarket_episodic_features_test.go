package scanner

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

func TestPremarketEpisodicQualifiesLiquidFixtureAndRejectsLookalike(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	checkpoint := time.Date(2026, 7, 27, 8, 0, 0, 0, loc)
	cfg := config.Defaults().Scanner.PremarketEpisodic
	cfg.HistorySessions = 5
	bars := make([]data.PremarketBar, 0, 240)
	for i := 0; i < 240; i++ {
		price := 10.4 + float64(i)/10_000
		bars = append(bars, data.PremarketBar{
			Candle: data.Candle{
				Ticker: "GOOD", Timeframe: data.Timeframe1Min,
				Timestamp: checkpoint.Add(time.Duration(i-240) * time.Minute),
				Open:      price, High: price + 0.02, Low: price - 0.02,
				Close: price, Volume: 1_000,
			},
			TradeCount: 10,
		})
	}
	daily := make([]data.Candle, 15)
	for i := range daily {
		daily[i] = data.Candle{Open: 9.8, High: 10.2, Low: 9.7, Close: 10}
	}
	evidence := data.PremarketEvidence{
		Symbol: "GOOD", PreviousClose: 10,
		Quote: data.Quote{Bid: 10.39, Ask: 10.41, Timestamp: checkpoint.Add(-time.Minute)},
		Bars:  bars, HistoricalVolumes: []int64{
			50_000, 55_000, 60_000, 52_000, 58_000, 51_000,
		},
		DailyBars: daily, Feed: "sip", ObservedAt: checkpoint,
	}
	got := EvaluatePremarketEpisodic(
		"GOOD", evidence, 500, checkpoint,
		PremarketCatalyst{Status: "confirmed", Headline: "Public fixture catalyst"},
		cfg,
	)
	if !got.Qualified || got.Outcome != "qualified" {
		t.Fatalf("qualifying fixture rejected: %+v", got)
	}
	if got.Features.ADR14Pct == 0 || got.Features.NATR14Pct == 0 {
		t.Fatalf("setup-specific volatility evidence missing: %+v", got.Features)
	}

	illiquid := evidence
	illiquid.Symbol = "BAD"
	illiquid.Quote = data.Quote{Bid: 9.90, Ask: 10.50, Timestamp: checkpoint}
	got = EvaluatePremarketEpisodic(
		"BAD", illiquid, 500, checkpoint,
		PremarketCatalyst{Status: "confirmed"}, cfg,
	)
	if got.Qualified || got.RemovalStage != "spread" {
		t.Fatalf("high-spread lookalike was not rejected: %+v", got)
	}
}

func TestPremarketEpisodicNewsFreeIsDistinctAndObservable(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	checkpoint := time.Date(2026, 7, 27, 4, 10, 0, 0, loc)
	cfg := config.Defaults().Scanner.PremarketEpisodic
	cfg.HistorySessions = 5
	cfg.MinBarCoverage = 0.5
	cfg.MinPremarketVolume = 1
	cfg.MinDollarVolume = 1
	cfg.MinTradeCount = 1
	cfg.MinTimeAdjustedRVOL = 0.1
	evidence := data.PremarketEvidence{
		PreviousClose:     10,
		Quote:             data.Quote{Bid: 10.49, Ask: 10.51, Timestamp: checkpoint},
		HistoricalVolumes: []int64{100, 100, 100, 100, 100},
	}
	for i := 5; i >= 1; i-- {
		evidence.Bars = append(evidence.Bars, data.PremarketBar{
			Candle: data.Candle{
				Timestamp: checkpoint.Add(-time.Duration(i) * time.Minute),
				High:      10.6, Low: 10.4, Close: 10.5, Volume: 200,
			},
			TradeCount: 2,
		})
	}
	got := EvaluatePremarketEpisodic(
		"NONEWS", evidence, 500, checkpoint,
		PremarketCatalyst{Status: "confirmed_absent"}, cfg,
	)
	if !got.Qualified || got.Catalyst.Status != "confirmed_absent" {
		t.Fatalf("news-free outcome lost: %+v", got)
	}
	confirmed := EvaluatePremarketEpisodic(
		"NEWS", evidence, 500, checkpoint,
		PremarketCatalyst{Status: "confirmed"}, cfg,
	)
	unavailable := EvaluatePremarketEpisodic(
		"UNKNOWN", evidence, 500, checkpoint,
		PremarketCatalyst{Status: "provider_unavailable"}, cfg,
	)
	if !(confirmed.Score > got.Score && got.Score > unavailable.Score) {
		t.Fatalf("catalyst scores are not distinct: confirmed=%f absent=%f unavailable=%f",
			confirmed.Score, got.Score, unavailable.Score)
	}
}

func TestPremarketEpisodicFailsClosedOnStaleQuoteAndIncompleteBars(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	checkpoint := time.Date(2026, 7, 27, 8, 0, 0, 0, loc)
	cfg := config.Defaults().Scanner.PremarketEpisodic
	base := data.PremarketEvidence{
		PreviousClose: 10,
		Quote:         data.Quote{Bid: 10.4, Ask: 10.5, Timestamp: checkpoint.Add(-3 * time.Minute)},
		Bars: []data.PremarketBar{{
			Candle: data.Candle{Timestamp: checkpoint.Add(-time.Minute), High: 10.6, Low: 10.4, Close: 10.5},
		}},
		HistoricalVolumes: []int64{1, 1, 1, 1, 1},
	}
	if got := EvaluatePremarketEpisodic(
		"STALE", base, 500, checkpoint, PremarketCatalyst{}, cfg,
	); got.RemovalStage != "stale_quote" {
		t.Fatalf("stale quote stage = %q", got.RemovalStage)
	}
	base.Quote.Timestamp = checkpoint.Add(time.Second)
	if got := EvaluatePremarketEpisodic(
		"FUTURE", base, 500, checkpoint, PremarketCatalyst{}, cfg,
	); got.RemovalStage != "quote" {
		t.Fatalf("future quote stage = %q", got.RemovalStage)
	}
	base.Quote.Timestamp = checkpoint
	if got := EvaluatePremarketEpisodic(
		"HOLES", base, 500, checkpoint, PremarketCatalyst{}, cfg,
	); got.RemovalStage != "bar_coverage" {
		t.Fatalf("incomplete bars stage = %q", got.RemovalStage)
	}
}
