package main

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestRouteStrengthPairUsesDirectOrInversePair(t *testing.T) {
	pairs := []strengthPair{
		{Instrument: config.Instrument{Symbol: "EUR/USD"}, Base: "EUR", Quote: "USD"},
		{Instrument: config.Instrument{Symbol: "USD/JPY"}, Base: "USD", Quote: "JPY"},
	}

	pair, side, ok := routeStrengthPair(pairs, "EUR", "USD")
	if !ok || pair.Instrument.Symbol != "EUR/USD" || side != "buy" {
		t.Fatalf("direct route = %s/%s/%v, want EUR/USD buy true", pair.Instrument.Symbol, side, ok)
	}

	pair, side, ok = routeStrengthPair(pairs, "JPY", "USD")
	if !ok || pair.Instrument.Symbol != "USD/JPY" || side != "sell" {
		t.Fatalf("inverse route = %s/%s/%v, want USD/JPY sell true", pair.Instrument.Symbol, side, ok)
	}
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

	trade := simulateStrengthDay(day, pairs, costs, p, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if trade == nil {
		t.Fatal("expected broad strength trade")
	}
	if trade.Ticker != "EUR/USD" || trade.Side != "buy" {
		t.Fatalf("trade = %s %s, want EUR/USD buy", trade.Ticker, trade.Side)
	}

	p.BroadMin = 3
	if trade := simulateStrengthDay(day, pairs, costs, p, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)); trade != nil {
		t.Fatalf("broad-min=3 produced trade %+v, want skip", trade)
	}
}

func TestResolveStrengthTradeAppliesSpreadCommissionAndJPYPointValue(t *testing.T) {
	inst := config.Instrument{Symbol: "USD/JPY", PointValue: 0.007}
	cost := orbreplay.CostModel{Spread: 0.012, CommissionPerSide: 2}
	entry := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	exit := time.Date(2026, 1, 2, 17, 0, 0, 0, time.UTC)

	trade := resolveStrengthTrade(inst, "sell", 140.000, 139.000, entry, exit, cost, 100000)
	if trade == nil {
		t.Fatal("nil trade")
	}
	want := (139.994 - 139.006) * 100000 * 0.007
	want -= 4
	if abs := trade.NetPnL - want; abs < -0.000001 || abs > 0.000001 {
		t.Fatalf("net PnL = %.6f, want %.6f", trade.NetPnL, want)
	}
}
