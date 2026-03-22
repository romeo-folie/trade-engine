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
			{Ticker: "AAPL", GapPct: 5.2, Conviction: 0.85, Reasoning: "earnings beat"},
			{Ticker: "TSLA", GapPct: -4.1, Conviction: 0.60, Reasoning: "no clear catalyst"},
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
			{Ticker: "AAPL", Side: "buy", EntryStop: 150.15, StopLoss: 139.86, TakeProfit: 170.73, Qty: 9, Status: "filled", FillPrice: 150.20, PnL: 125.50},
		},
	}
	out := RenderORBPanel(data, 80)
	if !strings.Contains(out, "filled") {
		t.Error("expected 'filled' status in trade")
	}
	if !strings.Contains(out, "PnL") {
		t.Error("expected PnL in output")
	}
}
