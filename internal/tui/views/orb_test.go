package views

import (
	"strings"
	"testing"
	"time"
)

func TestRenderORBPanelEmpty(t *testing.T) {
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "pending",
	}
	out := RenderORBPanel(data, 80)
	if !strings.Contains(out, "ORB Session") {
		t.Error("expected 'ORB Session' title")
	}
	if !strings.Contains(out, "2026-03-20") {
		t.Error("expected date in output")
	}
}

func TestRenderORBPanelWithCandidates(t *testing.T) {
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "candidate_ready",
		Candidates: []ORBCandidateRow{
			{Ticker: "AAPL", GapPct: 5.2, Conviction: 0.85, Action: "trade", Reasoning: "earnings beat"},
			{Ticker: "TSLA", GapPct: -4.1, Conviction: 0.60, Action: "skip", Reasoning: "no clear catalyst"},
		},
		Selected: []string{"AAPL"},
	}
	out := RenderORBPanel(data, 80)
	if !strings.Contains(out, "AAPL") {
		t.Error("expected AAPL in output")
	}
	if !strings.Contains(out, "candidate ready") {
		t.Error("expected 'candidate ready' status")
	}
}

func TestRenderORBPanelWithTrades(t *testing.T) {
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "done",
		Trades: []ORBTradeRow{
			{Ticker: "AAPL", Side: "buy", EntryStop: 150.15, StopLoss: 139.86, TakeProfit: 170.73, Qty: 9, Status: "closed", FillPrice: 150.20, ExitPrice: 170.73, ExitType: "tp", PnL: 125.50},
		},
	}
	out := RenderORBPanel(data, 80)
	if !strings.Contains(out, "TP hit") {
		t.Error("expected 'TP hit' exit label in trade row")
	}
	if !strings.Contains(out, "PnL") {
		t.Error("expected PnL in output")
	}
}

func TestRenderORBPanelCostBasis(t *testing.T) {
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "done",
		Trades: []ORBTradeRow{
			{Ticker: "AAPL", Side: "buy", EntryStop: 150.00, StopLoss: 148.00, TakeProfit: 154.00, Qty: 10, Status: "filled", FillPrice: 150.20},
		},
	}
	out := RenderORBPanel(data, 120)
	// cost = 150.20 * 10 = 1502.00; per-trade row no longer shows cost, only summary footer does
	if !strings.Contains(out, "notional $1502.00") {
		t.Errorf("expected 'notional $1502.00' in footer, got:\n%s", out)
	}
	// no closed trades → no W/L in footer
	if strings.Contains(out, "W:") {
		t.Errorf("expected no W/L count for unfinalised trade, got:\n%s", out)
	}
}

func TestRenderORBPanelSessionSummary(t *testing.T) {
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "done",
		Trades: []ORBTradeRow{
			{Ticker: "AAPL", Side: "buy", EntryStop: 150.00, StopLoss: 148.00, TakeProfit: 154.00, Qty: 10, Status: "closed", FillPrice: 150.20, ExitPrice: 154.00, ExitType: "tp", PnL: 38.00},
			{Ticker: "NVDA", Side: "sell", EntryStop: 820.00, StopLoss: 825.00, TakeProfit: 808.00, Qty: 5, Status: "closed", FillPrice: 819.80, ExitPrice: 825.00, ExitType: "stop", PnL: -26.00},
		},
	}
	out := RenderORBPanel(data, 160)
	if !strings.Contains(out, "W:1 L:1") {
		t.Errorf("expected 'W:1 L:1' in footer, got:\n%s", out)
	}
	if !strings.Contains(out, "net PnL") {
		t.Errorf("expected 'net PnL' in footer, got:\n%s", out)
	}
	if !strings.Contains(out, "notional") {
		t.Errorf("expected 'notional' in footer, got:\n%s", out)
	}
}

func TestRenderORBPanelBreakevenTrade(t *testing.T) {
	// A closed trade with exactly PnL == 0 should still show the footer.
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "done",
		Trades: []ORBTradeRow{
			{Ticker: "AAPL", Side: "buy", EntryStop: 150.00, StopLoss: 148.00, TakeProfit: 154.00, Qty: 10, Status: "closed", FillPrice: 150.00, ExitPrice: 150.00, ExitType: "exit", PnL: 0},
		},
	}
	out := RenderORBPanel(data, 120)
	if !strings.Contains(out, "net PnL") {
		t.Errorf("expected 'net PnL' for breakeven closed trade, got:\n%s", out)
	}
	// breakeven counts as neither win nor loss
	if !strings.Contains(out, "W:0 L:0") {
		t.Errorf("expected 'W:0 L:0' for breakeven, got:\n%s", out)
	}
}

func TestRenderORBPanelPendingNoFooter(t *testing.T) {
	// Pending trades with no fill should produce no footer at all.
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "executing",
		Trades: []ORBTradeRow{
			{Ticker: "AAPL", Side: "buy", EntryStop: 150.00, StopLoss: 148.00, TakeProfit: 154.00, Qty: 10, Status: "pending"},
		},
	}
	out := RenderORBPanel(data, 120)
	if strings.Contains(out, "notional") {
		t.Errorf("expected no footer for all-pending trades, got:\n%s", out)
	}
	if strings.Contains(out, "net PnL") {
		t.Errorf("expected no footer for all-pending trades, got:\n%s", out)
	}
}
