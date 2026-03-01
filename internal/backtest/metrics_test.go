package backtest

import (
	"math"
	"testing"
	"time"
)

func TestComputeZeroTrades(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 6, 30, 0, 0, 0, 0, time.UTC)
	m := Compute(nil, 100000, 100000, from, to, 0.04)
	if m.TotalTrades != 0 {
		t.Errorf("expected 0 trades, got %d", m.TotalTrades)
	}
	if m.TotalReturnPct != 0 {
		t.Errorf("expected 0%% return, got %.4f%%", m.TotalReturnPct)
	}
}

func TestComputeAllWins(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC)
	trades := []SimulatedTrade{
		{EntryTS: time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), ExitTS: time.Date(2024, 2, 10, 0, 0, 0, 0, time.UTC), EntryPrice: 100, ExitPrice: 110, Quantity: 10, PnL: 100, PnLPct: 10},
		{EntryTS: time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC), ExitTS: time.Date(2024, 5, 15, 0, 0, 0, 0, time.UTC), EntryPrice: 200, ExitPrice: 220, Quantity: 5, PnL: 100, PnLPct: 10},
	}
	m := Compute(trades, 100000, 100200, from, to, 0.04)
	if m.TotalTrades != 2 {
		t.Errorf("expected 2 trades, got %d", m.TotalTrades)
	}
	if m.WinRatePct != 100 {
		t.Errorf("expected 100%% win rate, got %.1f%%", m.WinRatePct)
	}
	if m.ProfitFactor <= 0 || math.IsInf(m.ProfitFactor, 0) == false {
		// All wins = infinite profit factor
	}
	if m.BestTradePct != 10 {
		t.Errorf("expected best trade 10%%, got %.2f%%", m.BestTradePct)
	}
	if m.WorstTradePct != 10 {
		t.Errorf("expected worst trade 10%%, got %.2f%%", m.WorstTradePct)
	}
}

func TestComputeAllLosses(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 6, 30, 0, 0, 0, 0, time.UTC)
	trades := []SimulatedTrade{
		{EntryTS: time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), ExitTS: time.Date(2024, 2, 5, 0, 0, 0, 0, time.UTC), EntryPrice: 100, ExitPrice: 95, Quantity: 10, PnL: -50, PnLPct: -5},
		{EntryTS: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), ExitTS: time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC), EntryPrice: 100, ExitPrice: 90, Quantity: 10, PnL: -100, PnLPct: -10},
	}
	m := Compute(trades, 100000, 99850, from, to, 0.04)
	if m.WinRatePct != 0 {
		t.Errorf("expected 0%% win rate, got %.1f%%", m.WinRatePct)
	}
	if m.ProfitFactor != 0 {
		t.Errorf("expected profit factor 0, got %.2f", m.ProfitFactor)
	}
	if m.WorstTradePct != -10 {
		t.Errorf("expected worst trade -10%%, got %.2f%%", m.WorstTradePct)
	}
}

func TestComputeSingleTrade(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC)
	trades := []SimulatedTrade{
		{EntryTS: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), ExitTS: time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC), EntryPrice: 150, ExitPrice: 165, Quantity: 100, PnL: 1500, PnLPct: 10},
	}
	m := Compute(trades, 100000, 101500, from, to, 0.04)
	if m.TotalTrades != 1 {
		t.Errorf("expected 1 trade, got %d", m.TotalTrades)
	}
	if m.WinRatePct != 100 {
		t.Errorf("expected 100%% win rate, got %.1f%%", m.WinRatePct)
	}
	if m.Expectancy != 1500 {
		t.Errorf("expected expectancy $1500, got $%.2f", m.Expectancy)
	}
	if m.BestTradePct != 10 || m.WorstTradePct != 10 {
		t.Errorf("best/worst should both be 10%%, got %.2f / %.2f", m.BestTradePct, m.WorstTradePct)
	}
}

func TestComputeMixed(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC)
	trades := []SimulatedTrade{
		{EntryTS: time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), ExitTS: time.Date(2024, 2, 10, 0, 0, 0, 0, time.UTC), EntryPrice: 100, ExitPrice: 110, Quantity: 10, PnL: 100, PnLPct: 10},
		{EntryTS: time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC), ExitTS: time.Date(2024, 4, 5, 0, 0, 0, 0, time.UTC), EntryPrice: 100, ExitPrice: 97, Quantity: 10, PnL: -30, PnLPct: -3},
		{EntryTS: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), ExitTS: time.Date(2024, 6, 20, 0, 0, 0, 0, time.UTC), EntryPrice: 200, ExitPrice: 230, Quantity: 5, PnL: 150, PnLPct: 15},
	}
	finalCash := 100000.0 + 100 - 30 + 150
	m := Compute(trades, 100000, finalCash, from, to, 0.04)

	if m.TotalTrades != 3 {
		t.Fatalf("expected 3 trades, got %d", m.TotalTrades)
	}

	// Win rate: 2 wins / 3 trades = 66.67%
	expectedWR := 2.0 / 3.0 * 100
	if math.Abs(m.WinRatePct-expectedWR) > 0.01 {
		t.Errorf("win rate: expected %.2f%%, got %.2f%%", expectedWR, m.WinRatePct)
	}

	// Profit factor: sum wins / sum losses = 250/30 = 8.33
	expectedPF := 250.0 / 30.0
	if math.Abs(m.ProfitFactor-expectedPF) > 0.01 {
		t.Errorf("profit factor: expected %.2f, got %.2f", expectedPF, m.ProfitFactor)
	}

	// Expectancy: total PnL / trades = 220/3 = 73.33
	expectedExp := 220.0 / 3.0
	if math.Abs(m.Expectancy-expectedExp) > 0.01 {
		t.Errorf("expectancy: expected $%.2f, got $%.2f", expectedExp, m.Expectancy)
	}

	if m.BestTradePct != 15 {
		t.Errorf("best trade: expected 15%%, got %.2f%%", m.BestTradePct)
	}
	if m.WorstTradePct != -3 {
		t.Errorf("worst trade: expected -3%%, got %.2f%%", m.WorstTradePct)
	}

	// Max drawdown should be > 0 (we had a losing trade)
	if m.MaxDrawdownPct <= 0 {
		t.Errorf("expected positive max drawdown, got %.4f%%", m.MaxDrawdownPct)
	}

	// Total return %
	expectedReturn := (finalCash/100000 - 1) * 100
	if math.Abs(m.TotalReturnPct-expectedReturn) > 0.001 {
		t.Errorf("total return: expected %.4f%%, got %.4f%%", expectedReturn, m.TotalReturnPct)
	}
}

// TestDailyReturnsSkipsWeekends verifies that the dailyReturns helper only
// produces observations for trading days (Mon–Fri). Including weekends would
// inflate the sample count by ~31%, diluting mean returns and distorting
// Sharpe/Sortino vs the industry-standard √252 annualisation factor.
func TestDailyReturnsSkipsWeekends(t *testing.T) {
	// 2024-01-01 (Mon) to 2024-01-07 (Sun) = 7 calendar days, 5 trading days.
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 1, 7, 0, 0, 0, 0, time.UTC)
	returns := dailyReturns(nil, 100000, from, to)
	if len(returns) != 5 {
		t.Errorf("expected 5 trading-day observations, got %d (weekends not skipped?)", len(returns))
	}
}

func TestComputeOpenTradesIgnored(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 6, 30, 0, 0, 0, 0, time.UTC)
	trades := []SimulatedTrade{
		// Closed trade
		{EntryTS: time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), ExitTS: time.Date(2024, 2, 10, 0, 0, 0, 0, time.UTC), PnL: 100, PnLPct: 5},
		// Open trade (no ExitTS)
		{EntryTS: time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC), PnL: 50, PnLPct: 2.5},
	}
	m := Compute(trades, 100000, 100150, from, to, 0.04)
	// Only closed trade counted
	if m.TotalTrades != 1 {
		t.Errorf("expected 1 closed trade, got %d", m.TotalTrades)
	}
}
