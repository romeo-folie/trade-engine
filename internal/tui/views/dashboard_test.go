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
		Watchlist:       []string{"AAPL", "TSLA", "MSFT"},
		LastUpdate:      time.Now(),
	}
}

func TestRenderDashboard_ContainsAllSections(t *testing.T) {
	d := sampleData()
	out := RenderDashboard(d, 120, 40, 0)

	sections := []string{
		"PAPER",              // mode header
		"Portfolio",          // portfolio section
		"Open Positions (2)", // positions header
		"AAPL",               // position ticker
		"TSLA",
		"Watchlist (3)",    // watchlist section
		"Pending Approvals", // pending section
		"MSFT",
		"Signal Feed", // signal feed
		"Risk:",       // risk bar
	}

	for _, s := range sections {
		if !strings.Contains(out, s) {
			t.Errorf("expected dashboard to contain %q", s)
		}
	}
}

func TestRenderDashboard_EmptyData(t *testing.T) {
	d := DashboardData{Mode: "manual", LastUpdate: time.Now()}
	out := RenderDashboard(d, 120, 40, 0)

	if !strings.Contains(out, "No open positions") {
		t.Error("expected 'No open positions' for empty data")
	}
	if !strings.Contains(out, "No recent signals") {
		t.Error("expected 'No recent signals' for empty data")
	}
}

func TestRenderDashboard_AutoMode(t *testing.T) {
	d := DashboardData{Mode: "auto", LastUpdate: time.Now()}
	out := RenderDashboard(d, 120, 40, 0)
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

// ── deriveCashAndTotal tests ──────────────────────────────────────────────────

func TestDeriveCashAndTotal_LongProfit(t *testing.T) {
	// Long position: bought 10 shares @ $100 (cost basis $1000).
	// Price rose to $120 → invested $1200, unrealized PnL $200.
	// initialCapital = $10000 → cash = $10000 - $1000 = $9000, total = $9000 + $1200 = $10200.
	cash, total := deriveCashAndTotal(10000, 1000, 1200)
	if cash != 9000 {
		t.Errorf("expected cash=9000, got %.2f", cash)
	}
	if total != 10200 {
		t.Errorf("expected total=10200, got %.2f", total)
	}
}

func TestDeriveCashAndTotal_LongLoss(t *testing.T) {
	// Long position: bought 10 shares @ $100 (cost basis $1000).
	// Price fell to $80 → invested $800.
	// initialCapital = $10000 → cash = $10000 - $1000 = $9000, total = $9000 + $800 = $9800.
	cash, total := deriveCashAndTotal(10000, 1000, 800)
	if cash != 9000 {
		t.Errorf("expected cash=9000, got %.2f", cash)
	}
	if total != 9800 {
		t.Errorf("expected total=9800, got %.2f", total)
	}
}

func TestDeriveCashAndTotal_CashClamp(t *testing.T) {
	// Cost basis exceeds initial capital (e.g. margin/over-allocation edge case).
	// Cash must be clamped to 0, not go negative.
	cash, total := deriveCashAndTotal(500, 1000, 1100)
	if cash != 0 {
		t.Errorf("expected cash clamped to 0, got %.2f", cash)
	}
	if total != 1100 {
		t.Errorf("expected total=1100 (0+invested), got %.2f", total)
	}
}

func TestDeriveCashAndTotal_ShortPosition(t *testing.T) {
	// Short position: sold 10 shares @ $100 (cost basis $1000).
	// Price rose to $120 → mark-to-market value $1200, unrealized PnL = -$200.
	// Cost basis (entryPrice×qty) = $1000 regardless of direction.
	// cash = $10000 - $1000 = $9000, total = $9000 + $1200 = $10200.
	// (Loss is visible in UnrealizedPnL on the dashboard, not in cash/total distortion.)
	cash, total := deriveCashAndTotal(10000, 1000, 1200)
	if cash != 9000 {
		t.Errorf("expected cash=9000 for short, got %.2f", cash)
	}
	if total != 10200 {
		t.Errorf("expected total=10200 for short, got %.2f", total)
	}
}

func TestDeriveCashAndTotal_NoPositions(t *testing.T) {
	// No open positions: cash == initialCapital, total == initialCapital.
	cash, total := deriveCashAndTotal(100000, 0, 0)
	if cash != 100000 {
		t.Errorf("expected cash=100000, got %.2f", cash)
	}
	if total != 100000 {
		t.Errorf("expected total=100000, got %.2f", total)
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
