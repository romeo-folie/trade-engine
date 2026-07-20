package orbreplay

import (
	"math"
	"testing"
	"time"
)

func TestProfitFactor_AllWinnersIsInf(t *testing.T) {
	trades := []SimTrade{
		{NetPnL: 10, GrossPnL: 14, R: 1.8, ExitType: "tp", EntryTime: time.Now(), ExitTime: time.Now()},
		{NetPnL: 6, GrossPnL: 10, R: 1.5, ExitType: "tp", EntryTime: time.Now(), ExitTime: time.Now()},
	}
	m := Summarize("baseline", trades).Overall
	if !math.IsInf(m.ProfitFactor, 1) {
		t.Errorf("ProfitFactor = %v; want +Inf when there are no losses", m.ProfitFactor)
	}
	if m.WinRate != 1.0 {
		t.Errorf("WinRate = %v; want 1.0", m.WinRate)
	}
}

func TestProfitFactor_Net(t *testing.T) {
	// Net PF = Σ net wins / Σ |net losses| = 10 / 5 = 2.0.
	trades := []SimTrade{
		{NetPnL: 10, GrossPnL: 14, R: 2, EntryTime: time.Now(), ExitTime: time.Now()},
		{NetPnL: -5, GrossPnL: -1, R: -1, EntryTime: time.Now(), ExitTime: time.Now()},
	}
	m := Summarize("baseline", trades).Overall
	if m.ProfitFactor != 2.0 {
		t.Errorf("ProfitFactor = %v; want 2.0 (net)", m.ProfitFactor)
	}
	if m.Wins != 1 || m.Losses != 1 {
		t.Errorf("W/L = %d/%d; want 1/1", m.Wins, m.Losses)
	}
}

func TestMetrics_OvernightHold(t *testing.T) {
	d1 := time.Date(2026, 4, 13, 23, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 4, 14, 1, 0, 0, 0, time.UTC) // crosses UTC midnight
	trades := []SimTrade{{NetPnL: -5, R: -1, EntryTime: d1, ExitTime: d2}}
	m := Summarize("baseline", trades).Overall
	if m.OvernightHold != 1 {
		t.Errorf("OvernightHold = %d; want 1", m.OvernightHold)
	}
}

func TestCostModel_PerFillCommissionBpsAndMinimum(t *testing.T) {
	cost := CostModel{CommissionBpsPerSide: 3.0, CommissionMinPerSide: 2.0}
	if got := cost.PerFillCommission(100, 50, 1); got != 2 {
		t.Fatalf("minimum commission = %v, want 2", got)
	}
	if got := cost.PerFillCommission(1600, 58.125, 1); math.Abs(got-27.9) > 0.01 {
		t.Fatalf("bps commission = %v, want about 27.9", got)
	}
}
