package views

import (
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/events"
)

func sampleData() DashboardData {
	return DashboardData{
		Mode:          "paper",
		PaperMode:     true,
		TotalValue:    100000,
		Cash:          60000,
		Invested:      40000,
		TodayPnL:      250.50,
		UnrealizedPnL: -120.30,
		Positions: []PositionRow{
			{Ticker: "AAPL", EntryPrice: 180, CurrentPrice: 185, Quantity: 10, PnL: 50, PnLPct: 2.78, StopLoss: 175, TakeProfit: 195, Strategy: "default"},
			{Ticker: "TSLA", EntryPrice: 250, CurrentPrice: 245, Quantity: 5, PnL: -25, PnLPct: -1.0, StopLoss: 240, TakeProfit: 270, Strategy: "default"},
		},
		PendingSignals: []SignalRow{
			{ID: "sig-12345678-abcd", Ticker: "MSFT", Action: "BUY", Confidence: 0.82, Status: "pending", CreatedAt: time.Now()},
		},
		RecentSignals: []SignalRow{
			{ID: "sig-aaaabbbb-1234", Ticker: "AAPL", Action: "BUY", Confidence: 0.75, Status: "executed", CreatedAt: time.Now()},
			{ID: "sig-ccccdddd-5678", Ticker: "TSLA", Action: "SELL", Confidence: 0.60, Status: "blocked", Reasoning: "High risk environment detected", CreatedAt: time.Now()},
		},
		OpenPositionCnt: 2,
		LargestPosPct:   46.25,
		LastUpdate:      time.Now(),
	}
}

func TestRenderDashboard_ContainsAllSections(t *testing.T) {
	d := sampleData()
	out := RenderDashboard(d, 120, 40)

	sections := []string{
		"PAPER",        // mode header
		"Portfolio",    // portfolio section
		"Open Positions (2)", // positions header
		"AAPL",         // position ticker
		"TSLA",
		"Pending Approvals", // pending section
		"MSFT",
		"Signal Feed",  // signal feed
		"Risk:",        // risk bar
	}

	for _, s := range sections {
		if !strings.Contains(out, s) {
			t.Errorf("expected dashboard to contain %q", s)
		}
	}
}

func TestRenderDashboard_EmptyData(t *testing.T) {
	d := DashboardData{Mode: "manual", LastUpdate: time.Now()}
	out := RenderDashboard(d, 120, 40)

	if !strings.Contains(out, "No open positions") {
		t.Error("expected 'No open positions' for empty data")
	}
	if !strings.Contains(out, "No recent signals") {
		t.Error("expected 'No recent signals' for empty data")
	}
}

func TestRenderDashboard_AutoMode(t *testing.T) {
	d := DashboardData{Mode: "auto", LastUpdate: time.Now()}
	out := RenderDashboard(d, 120, 40)
	if !strings.Contains(out, "AUTO") {
		t.Error("expected AUTO mode in header")
	}
}

func TestApplyEvent_Relevant(t *testing.T) {
	d := &DashboardData{}
	relevant := []events.Type{
		events.TradeExecuted, events.TradeClosed,
		events.SignalGenerated, events.SignalBlocked,
	}
	for _, et := range relevant {
		if !ApplyEvent(d, events.Event{Type: et}) {
			t.Errorf("expected %s to be relevant", et)
		}
	}
}

func TestApplyEvent_Irrelevant(t *testing.T) {
	d := &DashboardData{}
	if ApplyEvent(d, events.Event{Type: events.DataFetched}) {
		t.Error("expected DataFetched to be irrelevant")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("short", 10) != "short" {
		t.Error("short string should not be truncated")
	}
	long := "this is a very long string that should be truncated"
	result := truncate(long, 20)
	if len(result) != 20 {
		t.Errorf("expected length 20, got %d", len(result))
	}
	if !strings.HasSuffix(result, "...") {
		t.Error("expected truncated string to end with ...")
	}
}

func TestColorPnL(t *testing.T) {
	pos := colorPnL(100)
	neg := colorPnL(-50)
	zero := colorPnL(0)

	if !strings.Contains(pos, "+") {
		t.Error("positive P&L should have +")
	}
	if !strings.Contains(neg, "-") {
		t.Error("negative P&L should have -")
	}
	_ = zero // just shouldn't panic
}
