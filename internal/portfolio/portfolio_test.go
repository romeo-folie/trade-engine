package portfolio

import (
	"testing"
	"time"
)

func TestTrackerUpdate_BasicState(t *testing.T) {
	tr := NewTracker(time.Hour)
	positions := []ExecPosition{
		{Ticker: "AAPL", CurrentValue: 5000, UnrealizedPL: 200},
		{Ticker: "TSLA", CurrentValue: 3000, UnrealizedPL: -100},
	}
	tr.Update(positions, 2000, 10000, 500, 150) // 2000 cash, 10000 equity, 500 day pnl, 150 realized today

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
	tr.Update([]ExecPosition{{CurrentValue: 8000}}, 2000, 10000, 0, 0)
	if tr.CurrentState().PeakValue != 10000 {
		t.Fatalf("PeakValue = %.2f, want 10000", tr.CurrentState().PeakValue)
	}

	// Lower value: peak stays at 10000
	tr.Update([]ExecPosition{{CurrentValue: 6000}}, 2000, 8000, 0, 0)
	if tr.CurrentState().PeakValue != 10000 {
		t.Fatalf("PeakValue = %.2f, want 10000 (unchanged)", tr.CurrentState().PeakValue)
	}

	// New high: peak = 12000
	tr.Update([]ExecPosition{{CurrentValue: 10000}}, 2000, 12000, 0, 0)
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

func TestConcurrentAccess(t *testing.T) {
	tr := NewTracker(time.Hour)
	done := make(chan struct{})

	// Writer goroutine.
	go func() {
		for i := 0; i < 100; i++ {
			tr.Update([]ExecPosition{{CurrentValue: float64(i * 100)}}, 1000, float64(1000+i*100), 0, 0)
		}
		close(done)
	}()

	// Reader goroutine (concurrent).
	for i := 0; i < 100; i++ {
		_ = tr.CurrentState()
	}
	<-done
}

func TestPositions(t *testing.T) {
	tr := NewTracker(time.Hour)
	positions := []ExecPosition{
		{Ticker: "AAPL", Quantity: 10, AvgCostBasis: 150.0, CurrentValue: 1700, UnrealizedPL: 200},
		{Ticker: "MSFT", Quantity: 5, AvgCostBasis: 300.0, CurrentValue: 1600, UnrealizedPL: 100},
	}
	tr.Update(positions, 5000, 8300, 600, 300)

	got := tr.Positions()
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}

	if got[0].Ticker != "AAPL" || got[0].Quantity != 10 || got[0].AvgCostBasis != 150.0 {
		t.Errorf("incorrect position details for AAPL: %+v", got[0])
	}
	if got[1].Ticker != "MSFT" || got[1].Quantity != 5 || got[1].AvgCostBasis != 300.0 {
		t.Errorf("incorrect position details for MSFT: %+v", got[1])
	}

	// Verify it's a copy.
	got[0].Ticker = "MODIFIED"
	if tr.Positions()[0].Ticker == "MODIFIED" {
		t.Error("Positions() returned a pointer or aliased slice")
	}
}
