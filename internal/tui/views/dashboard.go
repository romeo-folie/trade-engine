// Package views contains individual TUI view models for the dashboard.
package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// DashboardData holds all data needed to render the main dashboard view.
// Populated by polling the DB or derived from events.
type DashboardData struct {
	// Header
	Mode          string
	PaperMode     bool
	EngineRunning bool

	// Portfolio summary
	TotalValue    float64
	Cash          float64
	Invested      float64
	TodayPnL      float64
	UnrealizedPnL float64

	// Open positions
	Positions []PositionRow

	// Pending approvals (manual mode)
	PendingSignals []SignalRow

	// Recent signals
	RecentSignals []SignalRow

	// Risk
	DailyLossUsed   float64 // percentage of daily loss limit used
	DrawdownPct     float64
	OpenPositionCnt int
	LargestPosPct   float64 // largest position as % of portfolio

	LastUpdate time.Time
}

// PositionRow is a single row in the open positions table.
type PositionRow struct {
	Ticker       string
	EntryPrice   float64
	CurrentPrice float64
	Quantity     float64
	PnL          float64
	PnLPct       float64
	StopLoss     float64
	TakeProfit   float64
	Strategy     string
}

// SignalRow is a single row in the signal feed.
type SignalRow struct {
	ID         string
	Ticker     string
	Action     string
	Confidence float64
	Status     string
	Reasoning  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// FetchDashboardData loads dashboard data from the database.
// Returns zero-value DashboardData on errors (TUI stays functional).
// Safe to call when no database is connected — returns defaults.
func FetchDashboardData(ctx context.Context, mode string) (d DashboardData) {
	d = DashboardData{
		Mode:       mode,
		PaperMode:  mode == "paper",
		LastUpdate: time.Now(),
	}

	// All store calls below handle errors gracefully (skip section on error).
	// No recover() — real panics should surface.
	if !store.IsConnected() {
		return d
	}

	// Open trades → positions
	trades, err := store.GetOpenTrades(ctx)
	if err == nil {
		d.OpenPositionCnt = len(trades)
		for _, t := range trades {
			// Get latest price for unrealized P&L
			var currentPrice float64
			if c, err := store.GetLatestCandle(ctx, t.Ticker, "1d"); err == nil && c != nil {
				currentPrice = c.Close
			} else {
				currentPrice = t.EntryPrice
			}

			pnl := (currentPrice - t.EntryPrice) * t.Quantity
			pnlPct := 0.0
			if t.EntryPrice > 0 {
				pnlPct = (pnl / (t.EntryPrice * t.Quantity)) * 100
			}
			if t.Direction == "sell" {
				pnl = (t.EntryPrice - currentPrice) * t.Quantity
				if t.EntryPrice > 0 {
					pnlPct = (pnl / (t.EntryPrice * t.Quantity)) * 100
				}
			}

			posValue := currentPrice * t.Quantity
			d.Invested += posValue
			d.UnrealizedPnL += pnl

			d.Positions = append(d.Positions, PositionRow{
				Ticker:       t.Ticker,
				EntryPrice:   t.EntryPrice,
				CurrentPrice: currentPrice,
				Quantity:     t.Quantity,
				PnL:          pnl,
				PnLPct:       pnlPct,
				StopLoss:     t.StopLoss,
				TakeProfit:   t.TakeProfit,
				Strategy:     t.Strategy,
			})
		}
	}

	// Pending signals
	pending, err := store.GetPendingSignals(ctx)
	if err == nil {
		for _, s := range pending {
			d.PendingSignals = append(d.PendingSignals, SignalRow{
				ID:         s.ID,
				Ticker:     s.Ticker,
				Action:     s.Action,
				Confidence: s.Confidence,
				Status:     s.Status,
				CreatedAt:  s.CreatedAt,
				ExpiresAt:  s.ExpiresAt,
			})
		}
	}

	// Recent signals (last 20)
	recent, err := store.GetRecentSignals(ctx, 20)
	if err == nil {
		for _, s := range recent {
			d.RecentSignals = append(d.RecentSignals, SignalRow{
				ID:         s.ID,
				Ticker:     s.Ticker,
				Action:     s.Action,
				Confidence: s.Confidence,
				Status:     s.Status,
				CreatedAt:  s.CreatedAt,
			})
		}
	}

	// Risk: daily P&L from closed trades today
	closedTrades, _ := store.GetRecentTrades(ctx, 100)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	for _, t := range closedTrades {
		if t.Status == "closed" && t.ClosedAt.After(today) {
			d.TodayPnL += t.PnL
		}
	}

	// Largest position %
	if d.Invested > 0 {
		for _, p := range d.Positions {
			posVal := p.CurrentPrice * p.Quantity
			pct := (posVal / (d.Invested + d.Cash)) * 100
			if pct > d.LargestPosPct {
				d.LargestPosPct = pct
			}
		}
	}

	return d
}

// ── Rendering ─────────────────────────────────────────────────────────────────

var (
	sectionTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("117")).
			MarginLeft(1)

	dimText = lipgloss.NewStyle().
		Foreground(lipgloss.Color("243"))

	greenText = lipgloss.NewStyle().
			Foreground(lipgloss.Color("82"))

	redText = lipgloss.NewStyle().
		Foreground(lipgloss.Color("196"))

	yellowText = lipgloss.NewStyle().
			Foreground(lipgloss.Color("220"))

	tableHeader = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("250")).
			Background(lipgloss.Color("237")).
			Padding(0, 1)
)

// RenderDashboard renders the main dashboard view.
// selectedPending is the index of the currently highlighted pending signal (-1 for none).
func RenderDashboard(d DashboardData, width, height, selectedPending int) string {
	var b strings.Builder

	// Header bar
	b.WriteString(renderModeHeader(d))
	b.WriteString("\n\n")

	// Portfolio summary
	b.WriteString(renderPortfolioSummary(d))
	b.WriteString("\n\n")

	// Open positions
	b.WriteString(renderPositionsTable(d.Positions, width))
	b.WriteString("\n")

	// Pending approvals (only in manual mode or when signals exist)
	if len(d.PendingSignals) > 0 {
		b.WriteString(renderPendingApprovals(d.PendingSignals, width, selectedPending))
		b.WriteString("\n")
	}

	// Signal feed
	b.WriteString(renderSignalFeed(d.RecentSignals, width))
	b.WriteString("\n")

	// Risk status bar
	b.WriteString(renderRiskBar(d, width))

	return b.String()
}

func renderModeHeader(d DashboardData) string {
	modeStyle := yellowText
	switch d.Mode {
	case "auto":
		modeStyle = greenText
	case "paper":
		modeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))
	}

	mode := modeStyle.Bold(true).Render(strings.ToUpper(d.Mode))
	paper := ""
	if d.PaperMode {
		paper = dimText.Render(" (paper trading)")
	}

	return fmt.Sprintf(" ⚡ Mode: %s%s    Updated: %s",
		mode, paper, dimText.Render(d.LastUpdate.Format("15:04:05")))
}

func renderPortfolioSummary(d DashboardData) string {
	total := fmt.Sprintf("$%.2f", d.TotalValue)
	cash := fmt.Sprintf("$%.2f", d.Cash)
	invested := fmt.Sprintf("$%.2f", d.Invested)
	pnl := colorPnL(d.TodayPnL)
	unrealized := colorPnL(d.UnrealizedPnL)

	return sectionTitle.Render("Portfolio") + "\n" +
		fmt.Sprintf("  Total: %-14s  Cash: %-14s  Invested: %-14s", total, cash, invested) + "\n" +
		fmt.Sprintf("  Today P&L: %-14s  Unrealized: %s", pnl, unrealized)
}

func renderPositionsTable(positions []PositionRow, width int) string {
	header := sectionTitle.Render(fmt.Sprintf("Open Positions (%d)", len(positions)))

	if len(positions) == 0 {
		return header + "\n" + dimText.Render("  No open positions")
	}

	colFmt := "  %-8s %10s %10s %10s %8s %10s %10s %-10s"
	hdr := fmt.Sprintf(colFmt, "Ticker", "Entry", "Current", "P&L", "P&L%", "Stop", "Target", "Strategy")

	var rows strings.Builder
	rows.WriteString(header + "\n")
	rows.WriteString(tableHeader.Render(hdr) + "\n")

	for _, p := range positions {
		pnlStr := colorPnL(p.PnL)
		pnlPctStr := colorPnLPct(p.PnLPct)
		row := fmt.Sprintf("  %-8s %10.2f %10.2f %10s %8s %10.2f %10.2f %-10s",
			p.Ticker, p.EntryPrice, p.CurrentPrice, pnlStr, pnlPctStr,
			p.StopLoss, p.TakeProfit, p.Strategy)
		rows.WriteString(row + "\n")
	}

	return rows.String()
}

func renderPendingApprovals(signals []SignalRow, width int, selected int) string {
	header := sectionTitle.Render(fmt.Sprintf("⏳ Pending Approvals (%d)", len(signals)))
	var b strings.Builder
	b.WriteString(header + "\n")

	for i, s := range signals {
		action := actionStyle(s.Action).Render(s.Action)
		marker := " "
		if i == selected {
			marker = "▶"
		}
		id := s.ID
		if len(id) > 8 {
			id = id[:8]
		}
		expires := ""
		if !s.ExpiresAt.IsZero() {
			left := time.Until(s.ExpiresAt).Round(time.Second)
			if left < 0 {
				left = 0
			}
			expires = "  exp:" + left.String()
		}
		b.WriteString(fmt.Sprintf(" %s[%s] %s %-6s  conf:%.0f%%  %s%s\n",
			marker, dimText.Render(id), action, s.Ticker, s.Confidence*100,
			dimText.Render(s.CreatedAt.Format("15:04")), dimText.Render(expires)))
	}
	b.WriteString(dimText.Render("  [a]pprove  [r]eject  [d]etails  [j/k] select") + "\n")

	return b.String()
}

func renderSignalFeed(signals []SignalRow, width int) string {
	header := sectionTitle.Render("Signal Feed")
	var b strings.Builder
	b.WriteString(header + "\n")

	if len(signals) == 0 {
		b.WriteString(dimText.Render("  No recent signals") + "\n")
		return b.String()
	}

	limit := 10
	if len(signals) < limit {
		limit = len(signals)
	}

	for _, s := range signals[:limit] {
		action := actionStyle(s.Action).Render(fmt.Sprintf("%-4s", s.Action))
		status := statusStyle(s.Status).Render(s.Status)
		b.WriteString(fmt.Sprintf("  %s %-6s %s  conf:%.0f%%  %s  %s\n",
			dimText.Render(s.CreatedAt.Format("15:04")),
			s.Ticker, action, s.Confidence*100, status,
			dimText.Render(truncate(s.Reasoning, 40))))
	}

	return b.String()
}

func renderRiskBar(d DashboardData, width int) string {
	parts := []string{
		fmt.Sprintf("Daily Loss: %.1f%%", d.DailyLossUsed),
		fmt.Sprintf("Drawdown: %.1f%%", d.DrawdownPct),
		fmt.Sprintf("Positions: %d", d.OpenPositionCnt),
		fmt.Sprintf("Largest: %.1f%%", d.LargestPosPct),
	}

	style := lipgloss.NewStyle().
		Foreground(lipgloss.Color("250")).
		Background(lipgloss.Color("235")).
		Width(width).
		Padding(0, 1)

	return style.Render("Risk: " + strings.Join(parts, "  │  "))
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func colorPnL(v float64) string {
	s := fmt.Sprintf("$%.2f", v)
	if v > 0 {
		return greenText.Render("+" + s)
	}
	if v < 0 {
		return redText.Render(s)
	}
	return dimText.Render(s)
}

func colorPnLPct(v float64) string {
	s := fmt.Sprintf("%.1f%%", v)
	if v > 0 {
		return greenText.Render("+" + s)
	}
	if v < 0 {
		return redText.Render(s)
	}
	return dimText.Render(s)
}

func actionStyle(action string) lipgloss.Style {
	switch strings.ToUpper(action) {
	case "BUY":
		return greenText.Bold(true)
	case "SELL":
		return redText.Bold(true)
	default:
		return dimText
	}
}

func statusStyle(status string) lipgloss.Style {
	switch status {
	case "executed":
		return greenText
	case "blocked":
		return redText
	case "pending":
		return yellowText
	case "expired", "stale":
		return dimText
	default:
		return dimText
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// ApplyEvent updates dashboard data in-place from a bus event.
// Returns true if the event was relevant and the view should re-render.
func ApplyEvent(d *DashboardData, evt events.Event) bool {
	switch evt.Type {
	case events.TradeExecuted:
		return true // re-fetch positions
	case events.TradeClosed:
		return true
	case events.SignalGenerated, events.SignalBlocked, events.SignalQueued, events.SignalExpired:
		return true
	case events.PositionUpdated, events.PortfolioSnapshotTaken:
		return true
	default:
		return false
	}
}
