package views

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// PositionDetail holds expanded data for a single open position.
type PositionDetail struct {
	Trade        store.Trade
	CurrentPrice float64
	PnL          float64
	PnLPct       float64

	// Analysis context
	AnalysisReasoning string
	AnalysisAction    string
	AnalysisModel     string

	// Indicators at entry (from the analysis that triggered the signal)
	Indicators map[string]string

	// Related signals
	RelatedSignals []SignalRow
}

// FetchPositionDetail loads full detail for a trade by ID.
func FetchPositionDetail(ctx context.Context, tradeID string) (d PositionDetail, ok bool) {
	if !store.IsConnected() {
		return d, false
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil {
		return d, false
	}
	d.Trade = *trade

	// Current price — freshest candle across all timeframes, entry as fallback.
	if c, err := store.GetFreshestCandle(ctx, trade.Ticker); err == nil && c != nil && c.Close > 0 {
		d.CurrentPrice = c.Close
	} else {
		d.CurrentPrice = trade.EntryPrice
	}

	// P&L
	d.PnL = (d.CurrentPrice - trade.EntryPrice) * trade.Quantity
	if trade.EntryPrice > 0 {
		d.PnLPct = (d.PnL / (trade.EntryPrice * trade.Quantity)) * 100
	}
	if trade.Direction == "short" {
		d.PnL = (trade.EntryPrice - d.CurrentPrice) * trade.Quantity
		if trade.EntryPrice > 0 {
			d.PnLPct = (d.PnL / (trade.EntryPrice * trade.Quantity)) * 100
		}
	}

	// Analysis that triggered this trade's signal
	if trade.SignalID != "" {
		sig, err := store.GetSignalByID(ctx, trade.SignalID)
		if err == nil && sig != nil {
			// Get recent signals for context
			sigs, _ := store.GetSignalsByTicker(ctx, trade.Ticker, 5)
			for _, s := range sigs {
				d.RelatedSignals = append(d.RelatedSignals, SignalRow{
					ID:         s.ID,
					Ticker:     s.Ticker,
					Action:     s.Action,
					Confidence: s.Confidence,
					Status:     s.Status,
					CreatedAt:  s.CreatedAt,
				})
			}
		}
	}

	// Load analysis reasoning if available
	analyses, err := store.GetAnalysesByTicker(ctx, trade.Ticker, 1)
	if err == nil && len(analyses) > 0 {
		a := analyses[0]
		d.AnalysisReasoning = a.Reasoning
		d.AnalysisAction = a.Action
		d.AnalysisModel = a.ModelName
	}

	return d, true
}

// RenderPositionDetail renders the expanded position view.
func RenderPositionDetail(d PositionDetail, width, height int) string {
	var b strings.Builder

	// Title
	title := sectionTitle.Render(fmt.Sprintf("Position: %s (%s)", d.Trade.Ticker, strings.ToUpper(d.Trade.Direction)))
	b.WriteString(title + "\n")
	b.WriteString(dimText.Render("  Press [esc] to return to dashboard") + "\n\n")

	// Trade details
	b.WriteString(sectionTitle.Render("Trade") + "\n")
	pnlStr := colorPnL(d.PnL)
	pnlPctStr := colorPnLPct(d.PnLPct)
	b.WriteString(fmt.Sprintf("  Entry: $%.2f   Current: $%.2f   Qty: %.4f\n", d.Trade.EntryPrice, d.CurrentPrice, d.Trade.Quantity))
	b.WriteString(fmt.Sprintf("  P&L: %s (%s)\n", pnlStr, pnlPctStr))
	b.WriteString(fmt.Sprintf("  Stop Loss: $%.2f   Take Profit: $%.2f\n", d.Trade.StopLoss, d.Trade.TakeProfit))
	b.WriteString(fmt.Sprintf("  Strategy: %s   Status: %s\n", d.Trade.Strategy, d.Trade.Status))
	b.WriteString("\n")

	// Analysis reasoning
	if d.AnalysisReasoning != "" {
		b.WriteString(sectionTitle.Render("Analysis Reasoning") + "\n")
		b.WriteString(fmt.Sprintf("  Model: %s   Action: %s\n", d.AnalysisModel, d.AnalysisAction))
		// Wrap reasoning text
		wrapped := wrapText(d.AnalysisReasoning, width-4)
		for _, line := range strings.Split(wrapped, "\n") {
			b.WriteString("  " + dimText.Render(line) + "\n")
		}
		b.WriteString("\n")
	}

	// Indicators
	if len(d.Indicators) > 0 {
		b.WriteString(sectionTitle.Render("Indicators at Entry") + "\n")
		for k, v := range d.Indicators {
			b.WriteString(fmt.Sprintf("  %-15s %s\n", k+":", v))
		}
		b.WriteString("\n")
	}

	// Related signals
	if len(d.RelatedSignals) > 0 {
		b.WriteString(sectionTitle.Render(fmt.Sprintf("Related Signals (%d)", len(d.RelatedSignals))) + "\n")
		for _, s := range d.RelatedSignals {
			action := actionStyle(s.Action).Render(fmt.Sprintf("%-4s", s.Action))
			status := statusStyle(s.Status).Render(s.Status)
			marker := " "
			if s.ID == d.Trade.SignalID {
				marker = "→"
			}
			b.WriteString(fmt.Sprintf("  %s %s %-6s %s conf:%.0f%%  %s\n",
				marker, dimText.Render(s.CreatedAt.Format("15:04")),
				s.Ticker, action, s.Confidence*100, status))
		}
	}

	return b.String()
}

// RenderPositionDetailStatic renders a position detail from a PositionRow
// (no DB required, for testing/standalone use).
func RenderPositionDetailStatic(p PositionRow, width, height int) string {
	var b strings.Builder

	titleText := fmt.Sprintf("Position: %s", p.Ticker)
	if p.Direction != "" {
		titleText += fmt.Sprintf(" (%s)", strings.ToUpper(p.Direction))
	}
	b.WriteString(sectionTitle.Render(titleText) + "\n")
	b.WriteString(dimText.Render("  Press [esc] to return") + "\n\n")

	pnlStr := colorPnL(p.PnL)
	pnlPctStr := colorPnLPct(p.PnLPct)

	entryLabel := "Entry"
	if p.Lots > 1 {
		entryLabel = fmt.Sprintf("Avg entry (%d lots)", p.Lots)
	}
	b.WriteString(fmt.Sprintf("  %s: $%.2f   Current: $%.2f   Qty: %.4f\n", entryLabel, p.EntryPrice, p.CurrentPrice, p.Quantity))
	b.WriteString(fmt.Sprintf("  P&L: %s (%s)\n", pnlStr, pnlPctStr))
	b.WriteString(fmt.Sprintf("  Stop Loss: $%.2f   Take Profit: $%.2f\n", p.StopLoss, p.TakeProfit))
	b.WriteString(fmt.Sprintf("  Strategy: %s\n", p.Strategy))

	return b.String()
}

// wrapText wraps text to the given width.
func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}

	var lines []string
	current := words[0]

	for _, w := range words[1:] {
		if len(current)+1+len(w) > width {
			lines = append(lines, current)
			current = w
		} else {
			current += " " + w
		}
	}
	lines = append(lines, current)
	return strings.Join(lines, "\n")
}

// ── Styles (reuse from dashboard) ─────────────────────────────────────────────

var (
	detailLabel = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("117"))
)
