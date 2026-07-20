package main

import (
	"math"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
)

func TestIBKRLondonSilverCommission(t *testing.T) {
	tests := []struct {
		name     string
		notional float64
		want     float64
	}{
		{name: "minimum", notional: 1000, want: 2},
		{name: "tier1", notional: 100000, want: 30},
		{name: "tier2", notional: 300000, want: 85},
		{name: "tier3", notional: 600000, want: 140},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ibkrLondonSilverCommission(tt.notional)
			if math.Abs(got-tt.want) > 0.000001 {
				t.Fatalf("commission(%v) = %v, want %v", tt.notional, got, tt.want)
			}
		})
	}
}

func TestApplyContMonthlyStopLoss(t *testing.T) {
	trades := []orbreplay.SimTrade{
		{EntryTime: time.Date(2026, 1, 3, 13, 30, 0, 0, time.UTC), NetPnL: 900},
		{EntryTime: time.Date(2026, 1, 1, 13, 30, 0, 0, time.UTC), NetPnL: -600},
		{EntryTime: time.Date(2026, 1, 2, 13, 30, 0, 0, time.UTC), NetPnL: -400},
		{EntryTime: time.Date(2026, 2, 1, 13, 30, 0, 0, time.UTC), NetPnL: 50},
	}

	got := applyContMonthlyStopLoss(trades, 1000)
	if len(got) != 3 {
		t.Fatalf("filtered trades = %d, want 3", len(got))
	}
	if got[0].NetPnL != -600 || got[1].NetPnL != -400 || got[2].NetPnL != 50 {
		t.Fatalf("filtered sequence = %+v", got)
	}

	disabled := applyContMonthlyStopLoss(trades, 0)
	if len(disabled) != len(trades) {
		t.Fatalf("disabled stop kept %d trades, want %d", len(disabled), len(trades))
	}
}
