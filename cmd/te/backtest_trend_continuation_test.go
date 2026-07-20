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

func TestNewSpecBacktestsDoNotOverwriteSharedORBSessionDefault(t *testing.T) {
	if btORBSession != "" {
		t.Fatalf("btORBSession default = %q, want empty so te backtest orb uses config/full-grid defaults", btORBSession)
	}
	if got := orbFadeCmd.Flags().Lookup("session").DefValue; got != "newyork" {
		t.Fatalf("orb-fade --session default = %q, want newyork", got)
	}
	if got := sweepReclaimCmd.Flags().Lookup("session").DefValue; got != "newyork" {
		t.Fatalf("sweep-reclaim --session default = %q, want newyork", got)
	}
	if got := trendContCmd.Flags().Lookup("session").DefValue; got != "newyork" {
		t.Fatalf("trend-continuation --session default = %q, want newyork", got)
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

func TestApplyContMonthlyStopLossScopedPerTicker(t *testing.T) {
	trades := []orbreplay.SimTrade{
		{Ticker: "XAG/USD", EntryTime: time.Date(2026, 1, 1, 13, 30, 0, 0, time.UTC), NetPnL: -600},
		{Ticker: "XAU/USD", EntryTime: time.Date(2026, 1, 1, 13, 31, 0, 0, time.UTC), NetPnL: 300},
		{Ticker: "XAG/USD", EntryTime: time.Date(2026, 1, 2, 13, 30, 0, 0, time.UTC), NetPnL: -400},
		{Ticker: "XAU/USD", EntryTime: time.Date(2026, 1, 2, 13, 31, 0, 0, time.UTC), NetPnL: 300},
		{Ticker: "XAG/USD", EntryTime: time.Date(2026, 1, 3, 13, 30, 0, 0, time.UTC), NetPnL: 900},
		{Ticker: "XAU/USD", EntryTime: time.Date(2026, 1, 3, 13, 31, 0, 0, time.UTC), NetPnL: 300},
	}

	got := applyContMonthlyStopLoss(trades, 1000)
	if len(got) != 5 {
		t.Fatalf("filtered trades = %d, want 5", len(got))
	}
	for _, trade := range got {
		if trade.Ticker == "XAG/USD" && trade.EntryTime.Day() == 3 {
			t.Fatalf("XAG trade after its monthly stop was not filtered: %+v", trade)
		}
	}
	xauKept := 0
	for _, trade := range got {
		if trade.Ticker == "XAU/USD" {
			xauKept++
		}
	}
	if xauKept != 3 {
		t.Fatalf("XAU trades kept = %d, want 3; monthly stop should not be portfolio-wide", xauKept)
	}
}
