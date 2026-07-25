package pullbackfreq

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/strategy/pullback"
)

type stubProvider struct {
	candles []data.Candle
	err     error
}

func (p stubProvider) FetchCandles(
	context.Context, string, data.Timeframe, time.Time, time.Time,
) ([]data.Candle, error) {
	return p.candles, p.err
}

func TestRunRecordsLoadFailures(t *testing.T) {
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	result, err := Run(context.Background(), stubProvider{err: errors.New("feed unavailable")},
		[]Member{{Ticker: "A"}}, Config{From: day, To: day})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.LoadErrors["A"] != "feed unavailable" {
		t.Fatalf("load error = %q, want feed unavailable", result.LoadErrors["A"])
	}

	result, err = Run(context.Background(), stubProvider{},
		[]Member{{Ticker: "A"}}, Config{From: day, To: day})
	if err != nil {
		t.Fatalf("Run() empty result error = %v", err)
	}
	if result.LoadErrors["A"] != "no daily candles returned" {
		t.Fatalf("empty load error = %q, want no daily candles returned", result.LoadErrors["A"])
	}
}

func TestRunHonorsCancelledContext(t *testing.T) {
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, stubProvider{}, []Member{{Ticker: "A"}}, Config{From: day, To: day})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestAnalyzeRanksPointInTimeTopQuartile(t *testing.T) {
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	members := []Member{{Ticker: "A"}, {Ticker: "B"}, {Ticker: "C"}, {Ticker: "D"}}
	series := map[string][]data.Candle{}
	for _, member := range members {
		series[member.Ticker] = []data.Candle{{
			Ticker: member.Ticker, Timestamp: day, Close: 100, Volume: 100,
		}}
	}
	returns := map[string]float64{"A": 80, "B": 60, "C": 50, "D": 40}
	result := analyze(series, members, Config{
		From: day, To: day, MinDollarVolume: 1, TopFraction: 0.25,
	}, func(candles []data.Candle, _ float64, _ pullback.Config) pullback.Result {
		r := returns[candles[len(candles)-1].Ticker]
		return pullback.Result{
			Eligible: true, Return1M: r, Return3M: r, Return6M: r,
		}
	})

	if result.ObservedSessions != 4 || result.MomentumQualified != 4 {
		t.Fatalf("observed/momentum = %d/%d, want 4/4",
			result.ObservedSessions, result.MomentumQualified)
	}
	if result.SelectedDays != 1 || result.EligibleDays != 1 || result.Episodes != 1 {
		t.Fatalf("selected/eligible/episodes = %d/%d/%d, want 1/1/1",
			result.SelectedDays, result.EligibleDays, result.Episodes)
	}
	if result.ByTicker["A"].SelectedDays != 1 {
		t.Fatalf("top-ranked A selected days = %d, want 1", result.ByTicker["A"].SelectedDays)
	}
	if result.Blocks["below_top_quartile"] != 3 {
		t.Fatalf("below-top-quartile blocks = %d, want 3", result.Blocks["below_top_quartile"])
	}
}

func TestAnalyzeUsesOnlyCandlePrefixAndRespectsMembershipDates(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	candles := makeCandles("A", start, 4)
	var windowLengths []int
	result := analyze(map[string][]data.Candle{"A": candles}, []Member{{
		Ticker: "A", EligibleFrom: start.AddDate(0, 0, 1), EligibleTo: start.AddDate(0, 0, 2),
	}}, Config{
		From: start, To: start.AddDate(0, 0, 3), MinDollarVolume: 1, TopFraction: 1,
	}, func(window []data.Candle, _ float64, _ pullback.Config) pullback.Result {
		windowLengths = append(windowLengths, len(window))
		return pullback.Result{
			Eligible: false, FailedCheck: "narrowing_range",
			Return1M: 40, Return3M: 40, Return6M: 40,
		}
	})

	if len(windowLengths) != 2 || windowLengths[0] != 2 || windowLengths[1] != 3 {
		t.Fatalf("window lengths = %v, want [2 3]", windowLengths)
	}
	if result.ObservedSessions != 2 {
		t.Fatalf("observed sessions = %d, want 2", result.ObservedSessions)
	}
}

func TestAnalyzeCollapsesNearbyEligibleDaysIntoEpisodes(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	candles := makeCandles("A", start, 6)
	result := analyze(map[string][]data.Candle{"A": candles}, []Member{{Ticker: "A"}}, Config{
		From: start, To: start.AddDate(0, 0, 5), MinDollarVolume: 1,
		TopFraction: 1, EpisodeGapSessions: 1,
	}, func(window []data.Candle, _ float64, _ pullback.Config) pullback.Result {
		index := len(window) - 1
		eligible := index == 0 || index == 1 || index == 4
		return pullback.Result{
			Eligible: eligible, FailedCheck: "reclaim",
			Return1M: 40, Return3M: 40, Return6M: 40,
		}
	})

	if result.EligibleDays != 3 || result.Episodes != 2 {
		t.Fatalf("eligible days/episodes = %d/%d, want 3/2",
			result.EligibleDays, result.Episodes)
	}
}

func makeCandles(ticker string, start time.Time, count int) []data.Candle {
	candles := make([]data.Candle, count)
	for i := range candles {
		candles[i] = data.Candle{
			Ticker: ticker, Timeframe: data.Timeframe1Day,
			Timestamp: start.AddDate(0, 0, i),
			Open:      100, High: 101, Low: 99, Close: 100, Volume: 100,
		}
	}
	return candles
}
