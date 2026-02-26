package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// TradeRow is a single row in the trade history table.
type TradeRow struct {
	ID         string
	Ticker     string
	Direction  string
	Strategy   string
	EntryPrice float64
	ExitPrice  float64
	Quantity   float64
	PnL        float64
	PnLPct     float64
	Status     string // open | closed
	OpenedAt   time.Time
	ClosedAt   time.Time
}

// TradeHistoryData holds trade history for the TUI view.
type TradeHistoryData struct {
	Trades     []TradeRow
	Selected   int
	LastUpdate time.Time
}

// TradeDetailData holds expanded detail for a single trade.
type TradeDetailData struct {
	Trade    TradeRow
	SignalID string
	Orders   []OrderRow

	// Analysis context (from the triggering analysis, if available)
	AnalysisReasoning string
	AnalysisAction    string
	AnalysisModel     string
}

// OrderRow is a single order linked to a trade.
type OrderRow struct {
	ID           string
	Side         string
	Type         string
	Quantity     float64
	FilledPrice  float64
	Status       string
	IsStopLoss   bool
	IsTakeProfit bool
}

// FetchTradeHistory loads recent trades from the database.
func FetchTradeHistory(ctx context.Context, limit int) TradeHistoryData {
	if limit == 0 {
		limit = 50
	}
	if !store.IsConnected() {
		return TradeHistoryData{LastUpdate: time.Now()}
	}
	trades, err := store.GetRecentTrades(ctx, limit)
	if err != nil {
		return TradeHistoryData{LastUpdate: time.Now()}
	}

	rows := make([]TradeRow, 0, len(trades))
	for _, t := range trades {
		rows = append(rows, TradeRow{
			ID:         t.ID,
			Ticker:     t.Ticker,
			Direction:  t.Direction,
			Strategy:   t.Strategy,
			EntryPrice: t.EntryPrice,
			ExitPrice:  t.ExitPrice,
			Quantity:   t.Quantity,
			PnL:        t.PnL,
			PnLPct:     t.PnLPct,
			Status:     t.Status,
			OpenedAt:   t.OpenedAt,
			ClosedAt:   t.ClosedAt,
		})
	}
	return TradeHistoryData{Trades: rows, LastUpdate: time.Now()}
}

// FetchTradeDetail loads full detail for a single trade by ID.
func FetchTradeDetail(ctx context.Context, tradeID string) (d TradeDetailData, ok bool) {
	if !store.IsConnected() {
		return d, false
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil {
		return d, false
	}

	d.Trade = TradeRow{
		ID:         trade.ID,
		Ticker:     trade.Ticker,
		Direction:  trade.Direction,
		Strategy:   trade.Strategy,
		EntryPrice: trade.EntryPrice,
		ExitPrice:  trade.ExitPrice,
		Quantity:   trade.Quantity,
		PnL:        trade.PnL,
		PnLPct:     trade.PnLPct,
		Status:     trade.Status,
		OpenedAt:   trade.OpenedAt,
		ClosedAt:   trade.ClosedAt,
	}
	d.SignalID = trade.SignalID

	// Load linked orders.
	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err == nil {
		for _, o := range orders {
			d.Orders = append(d.Orders, OrderRow{
				ID:           o.ID[:8],
				Side:         o.Side,
				Type:         o.OrderType,
				Quantity:     o.Quantity,
				FilledPrice:  o.FilledPrice,
				Status:       o.Status,
				IsStopLoss:   o.IsStopLoss,
				IsTakeProfit: o.IsTakeProfit,
			})
		}
	}

	// Load analysis context from the signal's analysis.
	if trade.SignalID != "" {
		sig, err := store.GetSignalByID(ctx, trade.SignalID)
		if err == nil && sig != nil && len(sig.AnalysisIDs) > 0 {
			analysis, err := store.GetAnalysisByID(ctx, sig.AnalysisIDs[0])
			if err == nil && analysis != nil {
				d.AnalysisReasoning = analysis.Reasoning
				d.AnalysisAction = analysis.Action
				d.AnalysisModel = analysis.ModelProvider + "/" + analysis.ModelName
			}
		}
	}

	return d, true
}

// ─── Rendering ────────────────────────────────────────────────────────────────

var (
	tradeHeaderStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	winStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	lossStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	neutralTradeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
)

// RenderTradeHistory renders the scrollable trade history table.
func RenderTradeHistory(data TradeHistoryData, width int) string {
	var b strings.Builder

	b.WriteString(tradeHeaderStyle.Render("Trade History"))
	b.WriteString("\n\n")

	if len(data.Trades) == 0 {
		b.WriteString("  No trades yet.\n")
		return b.String()
	}

	// Header
	b.WriteString(fmt.Sprintf("  %-8s  %-5s  %-6s  %-11s  %-11s  %-11s  %-8s  %s\n",
		"TICKER", "DIR", "STATUS", "ENTRY", "EXIT", "P&L", "P&L %", "DATE"))
	b.WriteString(fmt.Sprintf("  %s\n", strings.Repeat("─", 80)))

	for i, t := range data.Trades {
		dir := strings.ToUpper(t.Direction)
		exitStr := "—"
		pnlStr := "—"
		pnlPctStr := "—"
		dateStr := t.OpenedAt.Format("01-02 15:04")

		if t.Status == "closed" {
			exitStr = fmt.Sprintf("%.4f", t.ExitPrice)
			pnlStr = fmt.Sprintf("%.2f", t.PnL)
			pnlPctStr = fmt.Sprintf("%.2f%%", t.PnLPct)
		}

		style := neutralTradeStyle
		if t.PnL > 0 {
			style = winStyle
		} else if t.PnL < 0 {
			style = lossStyle
		}

		prefix := "  "
		if i == data.Selected {
			prefix = "▸ "
		}

		line := fmt.Sprintf("%s%-8s  %-5s  %-6s  %-11.4f  %-11s  %-11s  %-8s  %s",
			prefix, t.Ticker, dir, t.Status,
			t.EntryPrice, exitStr, pnlStr, pnlPctStr, dateStr)

		b.WriteString(style.Render(line))
		b.WriteString("\n")
	}

	b.WriteString(fmt.Sprintf("\n  %d trades  |  Updated %s  |  ↑↓ navigate  Enter detail  Esc back",
		len(data.Trades), data.LastUpdate.Format("15:04:05")))
	return b.String()
}

// RenderTradeDetail renders the expanded detail view for a single trade.
func RenderTradeDetail(d TradeDetailData) string {
	var b strings.Builder
	t := d.Trade

	b.WriteString(tradeHeaderStyle.Render("Trade Detail"))
	b.WriteString("\n\n")

	b.WriteString(fmt.Sprintf("  Trade ID:    %s\n", t.ID))
	b.WriteString(fmt.Sprintf("  Ticker:      %s\n", t.Ticker))
	b.WriteString(fmt.Sprintf("  Direction:   %s\n", strings.ToUpper(t.Direction)))
	b.WriteString(fmt.Sprintf("  Strategy:    %s\n", t.Strategy))
	b.WriteString(fmt.Sprintf("  Status:      %s\n", t.Status))
	b.WriteString(fmt.Sprintf("  Entry:       %.4f\n", t.EntryPrice))
	b.WriteString(fmt.Sprintf("  Quantity:    %.6f\n", t.Quantity))

	if t.Status == "closed" {
		pnlStyle := winStyle
		if t.PnL < 0 {
			pnlStyle = lossStyle
		}
		b.WriteString(fmt.Sprintf("  Exit:        %.4f\n", t.ExitPrice))
		b.WriteString(fmt.Sprintf("  P&L:         %s\n",
			pnlStyle.Render(fmt.Sprintf("%.2f (%.2f%%)", t.PnL, t.PnLPct))))
		b.WriteString(fmt.Sprintf("  Closed At:   %s\n", t.ClosedAt.Format(time.RFC3339)))
	}
	b.WriteString(fmt.Sprintf("  Opened At:   %s\n", t.OpenedAt.Format(time.RFC3339)))

	if d.SignalID != "" {
		b.WriteString(fmt.Sprintf("  Signal ID:   %s\n", d.SignalID))
	}

	// Analysis context
	if d.AnalysisReasoning != "" {
		b.WriteString("\n")
		b.WriteString(tradeHeaderStyle.Render("Analysis"))
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("  Action: %s  |  Model: %s\n", d.AnalysisAction, d.AnalysisModel))
		b.WriteString(fmt.Sprintf("  %s\n", d.AnalysisReasoning))
	}

	// Orders
	if len(d.Orders) > 0 {
		b.WriteString("\n")
		b.WriteString(tradeHeaderStyle.Render(fmt.Sprintf("Orders (%d)", len(d.Orders))))
		b.WriteString("\n")
		for _, o := range d.Orders {
			tag := ""
			if o.IsStopLoss {
				tag = " [SL]"
			} else if o.IsTakeProfit {
				tag = " [TP]"
			}
			b.WriteString(fmt.Sprintf("  %s  %s %s  qty=%.6f  fill=%.4f  %s%s\n",
				o.ID, strings.ToUpper(o.Side), o.Type,
				o.Quantity, o.FilledPrice, o.Status, tag))
		}
	}

	b.WriteString("\n  Esc to go back")
	return b.String()
}
