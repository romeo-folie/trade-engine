package views

import (
	"strings"
	"testing"
	"time"
)

func TestRenderTradeHistory_Empty(t *testing.T) {
	out := RenderTradeHistory(TradeHistoryData{LastUpdate: time.Now()}, 120, 0)
	if !strings.Contains(out, "No trades yet") {
		t.Fatal("expected empty state message")
	}
}

func TestRenderTradeHistory_WithTrades(t *testing.T) {
	data := TradeHistoryData{
		Trades: []TradeRow{
			{
				ID: "abc-123", Ticker: "AAPL", Direction: "buy", Status: "closed",
				EntryPrice: 150, ExitPrice: 160, Quantity: 10,
				PnL: 100, PnLPct: 6.67, OpenedAt: time.Now(), ClosedAt: time.Now(),
			},
			{
				ID: "def-456", Ticker: "TSLA", Direction: "sell", Status: "open",
				EntryPrice: 200, Quantity: 5, OpenedAt: time.Now(),
			},
		},
		Selected:   0,
		LastUpdate: time.Now(),
	}
	out := RenderTradeHistory(data, 120, 0)
	if !strings.Contains(out, "AAPL") {
		t.Fatal("expected AAPL in output")
	}
	if !strings.Contains(out, "TSLA") {
		t.Fatal("expected TSLA in output")
	}
	if !strings.Contains(out, "2 trades") {
		t.Fatal("expected trade count")
	}
	// Selected row should have marker
	if !strings.Contains(out, "▸") {
		t.Fatal("expected selection marker")
	}
}

func TestRenderTradeHistory_PnLStyling(t *testing.T) {
	data := TradeHistoryData{
		Trades: []TradeRow{
			{Ticker: "WIN", Direction: "buy", Status: "closed", EntryPrice: 100, ExitPrice: 110, PnL: 10, PnLPct: 10, OpenedAt: time.Now()},
			{Ticker: "LOSE", Direction: "buy", Status: "closed", EntryPrice: 100, ExitPrice: 90, PnL: -10, PnLPct: -10, OpenedAt: time.Now()},
		},
		LastUpdate: time.Now(),
	}
	out := RenderTradeHistory(data, 120, 0)
	if !strings.Contains(out, "WIN") || !strings.Contains(out, "LOSE") {
		t.Fatal("expected both tickers")
	}
}

func TestRenderTradeDetail(t *testing.T) {
	d := TradeDetailData{
		Trade: TradeRow{
			ID: "abc-123", Ticker: "AAPL", Direction: "buy", Strategy: "momentum",
			Status: "closed", EntryPrice: 150, ExitPrice: 160, Quantity: 10,
			PnL: 100, PnLPct: 6.67, OpenedAt: time.Now(), ClosedAt: time.Now(),
		},
		SignalID:          "sig-789",
		AnalysisReasoning: "Strong momentum with volume confirmation",
		AnalysisAction:    "BUY",
		AnalysisModel:     "anthropic/claude-sonnet-4-5",
		Orders: []OrderRow{
			{ID: "ord-001", Side: "buy", Type: "market", Quantity: 10, FilledPrice: 150, Status: "filled"},
			{ID: "ord-002", Side: "sell", Type: "market", Quantity: 10, FilledPrice: 160, Status: "filled", IsTakeProfit: true},
		},
	}
	out := RenderTradeDetail(d)
	if !strings.Contains(out, "AAPL") {
		t.Fatal("expected ticker")
	}
	if !strings.Contains(out, "momentum") {
		t.Fatal("expected analysis reasoning")
	}
	if !strings.Contains(out, "[TP]") {
		t.Fatal("expected take-profit tag")
	}
	if !strings.Contains(out, "sig-789") {
		t.Fatal("expected signal ID")
	}
}
