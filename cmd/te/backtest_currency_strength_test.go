package main

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

type strengthCandlesProvider struct {
	candles []data.Candle
}

func (p strengthCandlesProvider) FetchCandles(context.Context, string, data.Timeframe, time.Time, time.Time) ([]data.Candle, error) {
	return p.candles, nil
}

func (strengthCandlesProvider) FetchQuote(context.Context, string) (*data.Quote, error) {
	return nil, nil
}

func (strengthCandlesProvider) FetchHistory(context.Context, string, data.Timeframe, int) ([]data.Candle, error) {
	return nil, nil
}

func TestSimulateStrengthDayRequiresBroadStrongAndWeakVotes(t *testing.T) {
	pairs := []strengthPair{
		{Instrument: config.Instrument{Symbol: "EUR/USD", SecType: "CASH"}, Base: "EUR", Quote: "USD"},
		{Instrument: config.Instrument{Symbol: "GBP/USD", SecType: "CASH"}, Base: "GBP", Quote: "USD"},
		{Instrument: config.Instrument{Symbol: "USD/JPY", SecType: "CASH", PointValue: 0.007}, Base: "USD", Quote: "JPY"},
		{Instrument: config.Instrument{Symbol: "EUR/JPY", SecType: "CASH", PointValue: 0.007}, Base: "EUR", Quote: "JPY"},
		{Instrument: config.Instrument{Symbol: "GBP/JPY", SecType: "CASH", PointValue: 0.007}, Base: "GBP", Quote: "JPY"},
	}
	day := map[string]map[string]float64{
		"EUR/USD": {"06:00": 1.10, "09:00": 1.11, "17:00": 1.12},
		"GBP/USD": {"06:00": 1.20, "09:00": 1.21, "17:00": 1.22},
		"USD/JPY": {"06:00": 140.0, "09:00": 139.0, "17:00": 138.0},
		"EUR/JPY": {"06:00": 154.0, "09:00": 155.0, "17:00": 156.0},
		"GBP/JPY": {"06:00": 168.0, "09:00": 169.0, "17:00": 170.0},
	}
	costs := map[string]orbreplay.CostModel{
		"EUR/USD": {Spread: 0.00008, CommissionPerSide: 2},
		"GBP/USD": {Spread: 0.00010, CommissionPerSide: 2},
		"USD/JPY": {Spread: 0.012, CommissionPerSide: 2},
		"EUR/JPY": {Spread: 0.018, CommissionPerSide: 2},
		"GBP/JPY": {Spread: 0.022, CommissionPerSide: 2},
	}
	p := strengthParam{Start: "06:00", RankEnd: "09:00", Exit: "17:00", MinGap: 0.0001, BroadMin: 2, Units: 100000}

	trade := simulateStrengthDay(day, pairs, costs, p, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), nil)
	if trade == nil {
		t.Fatal("expected broad strength trade")
	}
	if trade.Ticker != "EUR/USD" || trade.Side != "buy" {
		t.Fatalf("trade = %s %s, want EUR/USD buy", trade.Ticker, trade.Side)
	}

	p.BroadMin = 3
	if trade := simulateStrengthDay(day, pairs, costs, p, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), nil); trade != nil {
		t.Fatalf("broad-min=3 produced trade %+v, want skip", trade)
	}
}

func TestSimulateStrengthDayAppliesRoutedPairConfirmation(t *testing.T) {
	pairs := []strengthPair{
		{Instrument: config.Instrument{Symbol: "EUR/USD", SecType: "CASH"}, Base: "EUR", Quote: "USD"},
		{Instrument: config.Instrument{Symbol: "GBP/USD", SecType: "CASH"}, Base: "GBP", Quote: "USD"},
		{Instrument: config.Instrument{Symbol: "USD/JPY", SecType: "CASH", PointValue: 0.007}, Base: "USD", Quote: "JPY"},
		{Instrument: config.Instrument{Symbol: "EUR/JPY", SecType: "CASH", PointValue: 0.007}, Base: "EUR", Quote: "JPY"},
		{Instrument: config.Instrument{Symbol: "GBP/JPY", SecType: "CASH", PointValue: 0.007}, Base: "GBP", Quote: "JPY"},
	}
	day := map[string]map[string]float64{
		"EUR/USD": {"06:00": 1.10, "09:00": 1.102, "17:00": 1.12},
		"GBP/USD": {"06:00": 1.20, "09:00": 1.21, "17:00": 1.22},
		"USD/JPY": {"06:00": 140.0, "09:00": 139.0, "17:00": 138.0},
		"EUR/JPY": {"06:00": 154.0, "09:00": 155.0, "17:00": 156.0},
		"GBP/JPY": {"06:00": 168.0, "09:00": 169.0, "17:00": 170.0},
	}
	costs := map[string]orbreplay.CostModel{
		"EUR/USD": {Spread: 0.00008, CommissionPerSide: 2},
		"GBP/USD": {Spread: 0.00010, CommissionPerSide: 2},
		"USD/JPY": {Spread: 0.012, CommissionPerSide: 2},
		"EUR/JPY": {Spread: 0.018, CommissionPerSide: 2},
		"GBP/JPY": {Spread: 0.022, CommissionPerSide: 2},
	}
	p := strengthParam{Start: "06:00", RankEnd: "09:00", Exit: "17:00", MinGap: 0.0001, BroadMin: 2, Confirm: 0.01, Units: 100000}

	if trade := simulateStrengthDay(day, pairs, costs, p, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), nil); trade != nil {
		t.Fatalf("confirm threshold produced trade %+v, want skip", trade)
	}

	p.Confirm = 0.005
	if trade := simulateStrengthDay(day, pairs, costs, p, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), nil); trade == nil {
		t.Fatal("lower confirm threshold skipped trade, want execution")
	}
}

func TestSimulateStrengthDayAllowsConnectedPartialBasket(t *testing.T) {
	pairs := []strengthPair{
		{Instrument: config.Instrument{Symbol: "EUR/USD", SecType: "CASH"}, Base: "EUR", Quote: "USD"},
		{Instrument: config.Instrument{Symbol: "GBP/USD", SecType: "CASH"}, Base: "GBP", Quote: "USD"},
		{Instrument: config.Instrument{Symbol: "USD/JPY", SecType: "CASH"}, Base: "USD", Quote: "JPY"},
		{Instrument: config.Instrument{Symbol: "EUR/JPY", SecType: "CASH"}, Base: "EUR", Quote: "JPY"},
		{Instrument: config.Instrument{Symbol: "GBP/JPY", SecType: "CASH"}, Base: "GBP", Quote: "JPY"},
	}
	day := map[string]map[string]float64{
		"EUR/USD": {"06:00": 1.10, "09:00": 1.12, "17:00": 1.13},
		"GBP/USD": {"06:00": 1.20, "09:00": 1.21, "17:00": 1.20},
		"USD/JPY": {"06:00": 140, "09:00": 139, "17:00": 138},
		"EUR/JPY": {"06:00": 154, "09:00": 155, "17:00": 156},
		// GBP/JPY is deliberately absent; the other four edges still connect all currencies.
	}
	costs := map[string]orbreplay.CostModel{
		"EUR/USD": {Spread: 0.00008},
		"GBP/USD": {Spread: 0.00010},
		"USD/JPY": {Spread: 0.012},
		"EUR/JPY": {Spread: 0.018},
		"GBP/JPY": {Spread: 0.022},
	}
	p := strengthParam{
		Start: "06:00", RankEnd: "09:00", Exit: "17:00", MinGap: 0,
		BroadMin: 1, Units: 100000, CurrencyModel: "least_squares", AllowPartial: true,
	}
	if trade := simulateStrengthDay(day, pairs, costs, p, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), nil); trade == nil {
		t.Fatal("connected partial basket skipped, want a routed trade")
	}
}

func TestStrengthDayClosesRejectsStaleSnapshots(t *testing.T) {
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	provider := strengthCandlesProvider{candles: []data.Candle{
		{Timestamp: day.Add(5*time.Hour + 55*time.Minute), Close: 1.10},
		{Timestamp: day.Add(8*time.Hour + 59*time.Minute), Close: 1.20},
		// M1 timestamps identify bar opens; the 09:00 bar is not complete at 09:00.
		{Timestamp: day.Add(9 * time.Hour), Close: 9.99},
	}}
	got, err := strengthDayCloses(context.Background(), provider, "EUR/USD", day, []string{"06:00", "09:00"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["06:00"]; ok {
		t.Fatalf("five-minute-old close accepted: %+v", got)
	}
	if got["09:00"] != 1.20 {
		t.Fatalf("fresh close = %v, want 1.20", got["09:00"])
	}
}

type recordingStrengthCandlesProvider struct {
	strengthCandlesProvider
	from time.Time
}

func (p *recordingStrengthCandlesProvider) FetchCandles(ctx context.Context, symbol string, timeframe data.Timeframe, from, to time.Time) ([]data.Candle, error) {
	p.from = from
	return p.strengthCandlesProvider.FetchCandles(ctx, symbol, timeframe, from, to)
}

func TestStrengthDayClosesFetchesCompletedPriorDayBarForMidnight(t *testing.T) {
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	provider := &recordingStrengthCandlesProvider{strengthCandlesProvider: strengthCandlesProvider{candles: []data.Candle{
		{Timestamp: day.Add(-time.Minute), Close: 1.20},
		{Timestamp: day, Close: 9.99},
	}}}
	got, err := strengthDayCloses(context.Background(), provider, "EUR/USD", day, []string{"00:00"})
	if err != nil {
		t.Fatal(err)
	}
	if got["00:00"] != 1.20 {
		t.Fatalf("midnight close=%v, want prior completed bar", got["00:00"])
	}
	if want := day.Add(-strengthSnapshotMaxLag); !provider.from.Equal(want) {
		t.Fatalf("fetch start=%s, want %s", provider.from, want)
	}
}

func TestSimulateStrengthDayAppliesTradeInstrumentFilter(t *testing.T) {
	pairs := []strengthPair{
		{Instrument: config.Instrument{Symbol: "EUR/USD", SecType: "CASH"}, Base: "EUR", Quote: "USD"},
		{Instrument: config.Instrument{Symbol: "GBP/USD", SecType: "CASH"}, Base: "GBP", Quote: "USD"},
		{Instrument: config.Instrument{Symbol: "USD/JPY", SecType: "CASH", PointValue: 0.007}, Base: "USD", Quote: "JPY"},
		{Instrument: config.Instrument{Symbol: "EUR/JPY", SecType: "CASH", PointValue: 0.007}, Base: "EUR", Quote: "JPY"},
		{Instrument: config.Instrument{Symbol: "GBP/JPY", SecType: "CASH", PointValue: 0.007}, Base: "GBP", Quote: "JPY"},
	}
	day := map[string]map[string]float64{
		"EUR/USD": {"06:00": 1.10, "09:00": 1.11, "17:00": 1.12},
		"GBP/USD": {"06:00": 1.20, "09:00": 1.21, "17:00": 1.22},
		"USD/JPY": {"06:00": 140.0, "09:00": 139.0, "17:00": 138.0},
		"EUR/JPY": {"06:00": 154.0, "09:00": 155.0, "17:00": 156.0},
		"GBP/JPY": {"06:00": 168.0, "09:00": 169.0, "17:00": 170.0},
	}
	costs := map[string]orbreplay.CostModel{
		"EUR/USD": {Spread: 0.00008, CommissionPerSide: 2},
		"GBP/USD": {Spread: 0.00010, CommissionPerSide: 2},
		"USD/JPY": {Spread: 0.012, CommissionPerSide: 2},
		"EUR/JPY": {Spread: 0.018, CommissionPerSide: 2},
		"GBP/JPY": {Spread: 0.022, CommissionPerSide: 2},
	}
	p := strengthParam{Start: "06:00", RankEnd: "09:00", Exit: "17:00", MinGap: 0.0001, BroadMin: 2, Units: 100000}

	if trade := simulateStrengthDay(day, pairs, costs, p, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), map[string]bool{"USD/JPY": true}); trade != nil {
		t.Fatalf("trade filter produced trade %+v, want skip", trade)
	}

	if trade := simulateStrengthDay(day, pairs, costs, p, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), map[string]bool{"EUR/USD": true}); trade == nil {
		t.Fatal("matching trade filter skipped trade, want execution")
	}
}

func TestMaybeStrengthScoreWindowsDefersUnusedOverallWindows(t *testing.T) {
	oldTrainFrom, oldTrainTo := btStrengthTrainFrom, btStrengthTrainTo
	oldValFrom, oldValTo := btStrengthValFrom, btStrengthValTo
	oldFwdFrom, oldFwdTo := btStrengthFwdFrom, btStrengthFwdTo
	oldTo := btORBTo
	t.Cleanup(func() {
		btStrengthTrainFrom, btStrengthTrainTo = oldTrainFrom, oldTrainTo
		btStrengthValFrom, btStrengthValTo = oldValFrom, oldValTo
		btStrengthFwdFrom, btStrengthFwdTo = oldFwdFrom, oldFwdTo
		btORBTo = oldTo
	})
	btStrengthTrainFrom, btStrengthTrainTo = "2022-01-01", "2024-12-31"
	btStrengthValFrom, btStrengthValTo = "2025-01-01", "2025-12-31"
	btStrengthFwdFrom, btStrengthFwdTo = "2026-01-01", ""
	btORBTo = "2024-12-31"

	windows, err := maybeStrengthScoreWindows("overall", false)
	if err != nil {
		t.Fatalf("overall mode parsed unused windows: %v", err)
	}
	if windows != nil {
		t.Fatalf("overall mode windows = %+v, want nil", windows)
	}

	if _, err := maybeStrengthScoreWindows("walk-forward", false); err == nil {
		t.Fatal("walk-forward mode accepted invalid forward window, want error")
	}
}

func TestResolveStrengthTradeAppliesSpreadCommissionAndQuoteConversion(t *testing.T) {
	inst := config.Instrument{Symbol: "USD/JPY"}
	cost := orbreplay.CostModel{Spread: 0.012, CommissionPerSide: 2}
	entry := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	exit := time.Date(2026, 1, 2, 17, 0, 0, 0, time.UTC)

	trade := resolveStrengthTrade(inst, "sell", 140.000, 139.000, 1.0/140.0, 1.0/139.0, entry, exit, cost, 100000)
	if trade == nil {
		t.Fatal("nil trade")
	}
	want := (139.994 - 139.006) * 100000 / 139.0
	want -= 4
	if abs := trade.NetPnL - want; abs < -0.000001 || abs > 0.000001 {
		t.Fatalf("net PnL = %.6f, want %.6f", trade.NetPnL, want)
	}
}

func TestWalkForwardSelectionDoesNotUseForwardTrades(t *testing.T) {
	windows := []strengthEvalWindow{
		{Label: "train", From: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2024, 12, 31, 23, 59, 59, 0, time.UTC)},
		{Label: "validate", From: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC)},
		{Label: "forward", From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)},
	}
	trade := func(at time.Time, pnl float64) orbreplay.SimTrade {
		return orbreplay.SimTrade{EntryTime: at, GrossPnL: pnl, NetPnL: pnl}
	}
	base := []orbreplay.SimTrade{
		trade(time.Date(2023, 6, 1, 9, 0, 0, 0, time.UTC), 100),
		trade(time.Date(2023, 7, 1, 9, 0, 0, 0, time.UTC), -20),
		trade(time.Date(2025, 6, 1, 9, 0, 0, 0, time.UTC), 100),
		trade(time.Date(2025, 7, 1, 9, 0, 0, 0, time.UTC), -20),
	}
	withForwardLoss := append(append([]orbreplay.SimTrade(nil), base...), orbreplay.SimTrade{
		EntryTime: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC), GrossPnL: -100000, NetPnL: -100000,
	})
	baseResult := orbreplay.Summarize("base", base)
	forwardResult := orbreplay.Summarize("forward-loss", withForwardLoss)
	if got, want := strengthScoreResult(forwardResult, 4, "walk-forward", windows), strengthScoreResult(baseResult, 4, "walk-forward", windows); got != want {
		t.Fatalf("forward loss changed selection score: got %v want %v", got, want)
	}
	if strengthGateResult(forwardResult, 4, "walk-forward", windows) != strengthGateResult(baseResult, 4, "walk-forward", windows) {
		t.Fatal("forward loss changed selection gate")
	}
	if got, want := strengthSelectionTradeCount(forwardResult, "walk-forward", windows), strengthSelectionTradeCount(baseResult, "walk-forward", windows); got != want {
		t.Fatalf("forward trade changed selection eligibility count: got %d want %d", got, want)
	}
}

func TestSortStrengthResultsUsesSelectionOnlyTieBreak(t *testing.T) {
	a := strengthParam{Start: "06:00", RankEnd: "09:00", Exit: "18:00", MinGap: .001}
	b := strengthParam{Start: "06:00", RankEnd: "09:00", Exit: "18:00", MinGap: .002}
	results := []strengthResult{
		{Param: a, Score: 1, SelectionNetPnL: 100, Result: orbreplay.Result{Overall: orbreplay.Metrics{NetPnL: -10000}}},
		{Param: b, Score: 1, SelectionNetPnL: 50, Result: orbreplay.Result{Overall: orbreplay.Metrics{NetPnL: 10000}}},
	}
	sortStrengthResults(results)
	if results[0].Param.label() != a.label() {
		t.Fatalf("forward-inclusive P&L won tie: first=%s want=%s", results[0].Param.label(), a.label())
	}
}

func TestStrengthFXRateFindsDirectInverseAndCrossRates(t *testing.T) {
	pairs := []strengthPair{
		{Instrument: config.Instrument{Symbol: "EUR/GBP"}, Base: "EUR", Quote: "GBP"},
		{Instrument: config.Instrument{Symbol: "EUR/USD"}, Base: "EUR", Quote: "USD"},
		{Instrument: config.Instrument{Symbol: "USD/JPY"}, Base: "USD", Quote: "JPY"},
	}
	day := map[string]map[string]float64{
		"EUR/GBP": {"17:00": 0.80},
		"EUR/USD": {"17:00": 1.20},
		"USD/JPY": {"17:00": 150},
	}
	for _, tc := range []struct {
		from string
		want float64
	}{
		{from: "EUR", want: 1.20},
		{from: "JPY", want: 1.0 / 150.0},
		{from: "GBP", want: 1.20 / 0.80},
	} {
		got, ok := strengthFXRate(day, pairs, "17:00", tc.from, "USD")
		if !ok || math.Abs(got-tc.want) > 1e-12 {
			t.Fatalf("%s/USD rate = %.12f, %v; want %.12f, true", tc.from, got, ok, tc.want)
		}
	}
}

func TestStrengthFXRateCrossPathIsPairOrderIndependent(t *testing.T) {
	pairs := []strengthPair{
		{Instrument: config.Instrument{Symbol: "GBP/JPY"}, Base: "GBP", Quote: "JPY"},
		{Instrument: config.Instrument{Symbol: "USD/JPY"}, Base: "USD", Quote: "JPY"},
		{Instrument: config.Instrument{Symbol: "GBP/EUR"}, Base: "GBP", Quote: "EUR"},
		{Instrument: config.Instrument{Symbol: "EUR/USD"}, Base: "EUR", Quote: "USD"},
	}
	day := map[string]map[string]float64{
		"GBP/JPY": {"17:00": 200}, "USD/JPY": {"17:00": 100},
		"GBP/EUR": {"17:00": 1.25}, "EUR/USD": {"17:00": 1.20},
	}
	forward, ok := strengthFXRate(day, pairs, "17:00", "GBP", "USD")
	if !ok || math.Abs(forward-1.5) > 1e-12 {
		t.Fatalf("lexicographic shortest path rate=%v ok=%v, want 1.5", forward, ok)
	}
	for i, j := 0, len(pairs)-1; i < j; i, j = i+1, j-1 {
		pairs[i], pairs[j] = pairs[j], pairs[i]
	}
	reversed, ok := strengthFXRate(day, pairs, "17:00", "GBP", "USD")
	if !ok || reversed != forward {
		t.Fatalf("reversed rate=%v ok=%v, want %v", reversed, ok, forward)
	}
}

func TestCurrencyStrengthResearchUniverseIsCompleteAndHonorsOverrides(t *testing.T) {
	got := currencyStrengthResearchInstruments([]config.Instrument{
		{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 12345},
		{Symbol: "NZD/USD", SecType: "CASH"},
	})
	bySymbol := make(map[string]config.Instrument, len(got))
	for _, inst := range got {
		bySymbol[inst.Symbol] = inst
	}
	for _, symbol := range []string{"AUD/CHF", "AUD/JPY", "AUD/USD", "EUR/AUD", "EUR/CHF", "EUR/GBP", "EUR/JPY", "EUR/USD", "GBP/AUD", "GBP/CHF", "GBP/JPY", "GBP/USD", "CHF/JPY", "USD/CHF", "USD/JPY"} {
		if _, ok := bySymbol[symbol]; !ok {
			t.Fatalf("complete FX research pair %s missing from universe", symbol)
		}
	}
	if bySymbol["EUR/USD"].MinLotSize != 12345 {
		t.Fatalf("configured override lost: %+v", bySymbol["EUR/USD"])
	}
	if len(got) != 15 || bySymbol["NZD/USD"].Symbol != "" {
		t.Fatalf("research universe expanded unexpectedly: count=%d NZD=%+v", len(got), bySymbol["NZD/USD"])
	}
	for _, inst := range got {
		if inst.CommissionBpsPerSide <= 0 || inst.CommissionMinPerSide <= 0 {
			t.Fatalf("default spot FX commission missing for %+v", inst)
		}
	}
}
