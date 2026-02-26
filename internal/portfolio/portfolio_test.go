package portfolio

import (
	"testing"
	"time"
)

func TestTrackerUpdate_BasicState(t *testing.T) {
	tr := NewTracker(time.Hour)
	positions := []position{
		{Ticker: "AAPL", CurrentValue: 5000, UnrealizedPL: 200},
		{Ticker: "TSLA", CurrentValue: 3000, UnrealizedPL: -100},
	}
	tr.Update(positions, 2000, 150)

	s := tr.CurrentState()
	if s.TotalValue != 10000 { // 2000 cash + 5000 + 3000
		t.Fatalf("TotalValue = %.2f, want 10000", s.TotalValue)
	}
	if s.Cash != 2000 {
		t.Fatalf("Cash = %.2f, want 2000", s.Cash)
	}
	if s.Invested != 8000 {
		t.Fatalf("Invested = %.2f, want 8000", s.Invested)
	}
	if s.UnrealizedPnL != 100 {
		t.Fatalf("UnrealizedPnL = %.2f, want 100", s.UnrealizedPnL)
	}
	if s.OpenPositions != 2 {
		t.Fatalf("OpenPositions = %d, want 2", s.OpenPositions)
	}
}

func TestTrackerPeakValue(t *testing.T) {
	tr := NewTracker(time.Hour)

	// First update: peak = 10000
	tr.Update([]position{{CurrentValue: 8000}}, 2000, 0)
	if tr.CurrentState().PeakValue != 10000 {
		t.Fatalf("PeakValue = %.2f, want 10000", tr.CurrentState().PeakValue)
	}

	// Lower value: peak stays at 10000
	tr.Update([]position{{CurrentValue: 6000}}, 2000, 0)
	if tr.CurrentState().PeakValue != 10000 {
		t.Fatalf("PeakValue = %.2f, want 10000 (unchanged)", tr.CurrentState().PeakValue)
	}

	// New high: peak = 12000
	tr.Update([]position{{CurrentValue: 10000}}, 2000, 0)
	if tr.CurrentState().PeakValue != 12000 {
		t.Fatalf("PeakValue = %.2f, want 12000", tr.CurrentState().PeakValue)
	}
}

func TestRecordLoss(t *testing.T) {
	tr := NewTracker(time.Hour)
	before := time.Now()
	tr.RecordLoss()
	s := tr.CurrentState()
	if s.LastLossAt.Before(before) {
		t.Fatal("LastLossAt not updated")
	}
}

func TestToPositions(t *testing.T) {
	eps := []ExecPosition{
		{Ticker: "AAPL", CurrentValue: 5000, UnrealizedPL: 200},
	}
	ps := ToPositions(eps)
	if len(ps) != 1 || ps[0].Ticker != "AAPL" {
		t.Fatal("ToPositions conversion failed")
	}
}
