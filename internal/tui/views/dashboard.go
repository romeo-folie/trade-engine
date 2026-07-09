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

	// Open positions, aggregated per (ticker, direction).
	Positions []PositionRow

	// Position quality / freshness metadata.
	PositionLots    int       // total valid open trade rows before aggregation
	MalformedTrades int       // open trades with entry_price<=0 or quantity<=0 (excluded)
	PriceAsOf       time.Time // timestamp of the freshest candle used to mark positions

	// Pending approvals (manual mode)
	PendingSignals []SignalRow

	// Recent signals
	RecentSignals []SignalRow

	// Risk
	DailyLossUsed   float64 // percentage of daily loss limit used
	DrawdownPct     float64
	OpenPositionCnt int
	LargestPosPct   float64 // largest position as % of portfolio

	// Watchlist summary
	Watchlist []string

	LastUpdate time.Time
}

// PositionRow is a single row in the open positions table. When positions are
// aggregated per ticker, EntryPrice is the quantity-weighted average across
// lots and Lots is how many open trades were folded into this row.
type PositionRow struct {
	Ticker       string
	Direction    string
	EntryPrice   float64
	CurrentPrice float64
	Quantity     float64
	PnL          float64
	PnLPct       float64
	StopLoss     float64
	TakeProfit   float64
	Strategy     string
	Lots         int
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

// deriveCashAndTotal computes the estimated cash balance and total portfolio
// value from the configured initial capital, the accumulated cost basis of all
// open positions, and their current mark-to-market value (invested).
//
// cash      = max(0, initialCapital - costBasis)
// total     = cash + invested
//
// Cost basis must be accumulated per-trade as entryPrice×qty (not derived from
// aggregated Invested/UnrealizedPnL, which breaks for short positions).
func deriveCashAndTotal(initialCapital, costBasis, invested float64) (cash, total float64) {
	cash = initialCapital - costBasis
	if cash < 0 {
		cash = 0
	}
	total = cash + invested
	return
}

// FetchDashboardData loads dashboard data from the database.
// Returns zero-value DashboardData on errors (TUI stays functional).
// Safe to call when no database is connected — returns defaults.
//
// initialCapital is the engine.initial_capital config value; it is used to
// derive Cash (initial_capital minus open position cost bases) and TotalValue.
func FetchDashboardData(ctx context.Context, mode string, initialCapital float64) (d DashboardData) {
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

	// Initial summary values from config/local derivation (fallback).
	d.Cash = initialCapital
	d.TotalValue = initialCapital

	// Prefer the latest portfolio snapshot if available (Alpaca-backed).
	if snap, err := store.GetLatestPortfolioSnapshot(ctx); err == nil && snap != nil {
		d.Cash = snap.Cash
		d.TotalValue = snap.TotalValue
		d.Invested = snap.Invested
		d.UnrealizedPnL = snap.UnrealizedPnL
		d.OpenPositionCnt = snap.OpenPositions
	}

	// Fetch open trades and aggregate them per (ticker, direction) into position
	// rows, marked to the freshest available candle.
	trades, err := store.GetOpenTrades(ctx)
	if err == nil {
		markPrice := func(ticker string) (float64, time.Time) {
			if c, cerr := store.GetFreshestCandle(ctx, ticker); cerr == nil && c != nil && c.Close > 0 {
				return c.Close, c.Timestamp
			}
			return 0, time.Time{}
		}
		d.Positions, d.PositionLots, d.MalformedTrades, d.PriceAsOf = aggregateOpenPositions(trades, markPrice)

		// Aggregate figures derived from the marked rows.
		var visibleInvested, largestVisiblePosValue, localUnrealizedPnL, totalCostBasis float64
		for _, p := range d.Positions {
			posValue := p.CurrentPrice * p.Quantity
			costBasis := p.EntryPrice * p.Quantity
			visibleInvested += posValue
			totalCostBasis += costBasis
			localUnrealizedPnL += p.PnL
			if posValue > largestVisiblePosValue {
				largestVisiblePosValue = posValue
			}
		}

		// Distinct-position count (not raw lot count) is the honest exposure.
		if len(d.Positions) > 0 {
			d.OpenPositionCnt = len(d.Positions)
			// Report unrealized P&L as the sum of the marked positions shown in
			// the table. The Alpaca snapshot's unrealized_pnl field is often
			// unpopulated (0), which would otherwise contradict the P&L column.
			d.UnrealizedPnL = localUnrealizedPnL
		}

		if d.TotalValue == initialCapital && d.Cash == initialCapital {
			// Fallback: derive cash and total from cost basis when no snapshot.
			d.Invested = visibleInvested
			d.Cash, d.TotalValue = deriveCashAndTotal(initialCapital, totalCostBasis, d.Invested)
		}

		// Risk: largest position % (stays consistent with displayed rows)
		if visibleInvested > 0 {
			d.LargestPosPct = (largestVisiblePosValue / (visibleInvested + d.Cash)) * 100
		}
	}

	// Pending signals (manual mode only)
	if mode == "manual" {
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
	}

	// Recent signals summary (last 20, global across all tickers/statuses)
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
				ExpiresAt:  s.ExpiresAt,
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

	// Drawdown calculation from peak
	var peak float64
	_ = store.GetDB().QueryRow(ctx, "SELECT COALESCE(MAX(total_value),0) FROM portfolio_snapshots").Scan(&peak)
	if peak > d.TotalValue && peak > 0 {
		d.DrawdownPct = ((peak - d.TotalValue) / peak) * 100
	}

	// Active watchlist summary
	watchlist, err := store.GetWatchlistItems(ctx, true)
	if err == nil {
		d.Watchlist = make([]string, 0, len(watchlist))
		for _, item := range watchlist {
			d.Watchlist = append(d.Watchlist, item.Ticker)
		}
	}

	return d
}

// aggregateOpenPositions folds open trade lots into one row per
// (ticker, direction), marked to the price returned by markPrice (which returns
// a zero price + zero time when no usable mark exists, in which case the lot's
// entry price is used). EntryPrice on each row is the quantity-weighted average
// across its lots; PnL uses the correct sign for shorts. Malformed lots
// (entry_price <= 0 or quantity <= 0) are excluded and counted separately.
//
// Extracted from FetchDashboardData so the money math is unit-testable without
// a database. Rows are returned in first-seen (opened_at DESC) order.
func aggregateOpenPositions(
	trades []store.Trade,
	markPrice func(ticker string) (price float64, ts time.Time),
) (rows []PositionRow, lots, malformed int, freshest time.Time) {
	type group struct {
		row       PositionRow
		costBasis float64
	}
	groups := make(map[string]*group)
	order := make([]string, 0, len(trades))

	for _, t := range trades {
		if t.EntryPrice <= 0 || t.Quantity <= 0 {
			malformed++
			continue
		}
		lots++

		currentPrice := t.EntryPrice
		if px, ts := markPrice(t.Ticker); px > 0 {
			currentPrice = px
			if ts.After(freshest) {
				freshest = ts
			}
		}

		key := t.Ticker + "|" + t.Direction
		g := groups[key]
		if g == nil {
			g = &group{row: PositionRow{
				Ticker:     t.Ticker,
				Direction:  t.Direction,
				StopLoss:   t.StopLoss, // representative: first (most-recent) lot
				TakeProfit: t.TakeProfit,
				Strategy:   t.Strategy,
			}}
			groups[key] = g
			order = append(order, key)
		}
		g.row.Quantity += t.Quantity
		g.costBasis += t.EntryPrice * t.Quantity
		g.row.CurrentPrice = currentPrice // identical across a ticker's lots
		g.row.Lots++
	}

	for _, key := range order {
		g := groups[key]
		if g.row.Quantity > 0 {
			g.row.EntryPrice = g.costBasis / g.row.Quantity // weighted average
		}
		posValue := g.row.CurrentPrice * g.row.Quantity
		pnl := posValue - g.costBasis
		if g.row.Direction == "short" {
			pnl = g.costBasis - posValue
		}
		if g.costBasis > 0 {
			g.row.PnLPct = (pnl / g.costBasis) * 100
		}
		g.row.PnL = pnl
		rows = append(rows, g.row)
	}
	return rows, lots, malformed, freshest
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
// selectedPos is the index of the highlighted open position; selectedPending is
// the index of the highlighted pending signal (manual mode). When height > 0
// the open-positions table is windowed around selectedPos so the surrounding
// sections (watchlist, signal feed, risk bar) stay on screen no matter how many
// positions are open.
func RenderDashboard(d DashboardData, width, height, selectedPos, selectedPending int) string {
	// Build every section except the positions table first, so we can measure
	// how much vertical space is left for the (scrollable) positions block.
	var top strings.Builder
	top.WriteString(renderModeHeader(d))
	top.WriteString("\n\n")
	top.WriteString(renderPortfolioSummary(d))

	var bottom strings.Builder
	bottom.WriteString(renderWatchlistSummary(d.Watchlist, width))
	bottom.WriteString("\n\n")
	if len(d.PendingSignals) > 0 {
		bottom.WriteString(renderPendingApprovals(d.PendingSignals, width, selectedPending))
		bottom.WriteString("\n")
	}
	bottom.WriteString(renderSignalFeed(d.RecentSignals, width))
	bottom.WriteString("\n")
	bottom.WriteString(renderRiskBar(d, width))

	// The positions block gets whatever height remains after the surrounding
	// sections and the two blank separators that bracket it. 0 (or a negative
	// budget on a tiny terminal) means "no windowing" — the View() clip is the
	// final backstop against overflow.
	posBudget := 0
	if height > 0 {
		posBudget = height - lipgloss.Height(top.String()) - lipgloss.Height(bottom.String()) - 2
		if posBudget < 3 {
			posBudget = 3 // always leave room for the title + header + one row
		}
	}

	var b strings.Builder
	b.WriteString(top.String())
	b.WriteString("\n\n")
	b.WriteString(renderPositionsTable(d, width, selectedPos, posBudget))
	b.WriteString("\n\n")
	b.WriteString(bottom.String())

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

// positionsNote builds the dim annotation shown next to the "Open Positions"
// title: the raw lot count when it differs from the aggregated row count, any
// excluded malformed trades, and how fresh the marked prices are.
func positionsNote(d DashboardData) string {
	var parts []string
	if d.PositionLots > len(d.Positions) {
		parts = append(parts, fmt.Sprintf("%d lots", d.PositionLots))
	}
	if d.MalformedTrades > 0 {
		parts = append(parts, fmt.Sprintf("%d malformed hidden", d.MalformedTrades))
	}
	if !d.PriceAsOf.IsZero() {
		age := time.Since(d.PriceAsOf)
		mark := "marked " + formatDuration(age) + " ago"
		if age > 26*time.Hour {
			mark += " (stale)"
		}
		parts = append(parts, mark)
	}
	if len(parts) == 0 {
		return ""
	}
	return "  · " + strings.Join(parts, " · ")
}

// renderPositionsTable renders the open-positions block. maxLines caps the
// block's total height (title + column header + rows + scroll indicator); the
// row list is windowed around `selected` so it stays within budget. maxLines
// <= 0 renders every row. Rows are aggregated per ticker; the title line notes
// the lot count, any excluded malformed trades, and how fresh the marks are.
func renderPositionsTable(d DashboardData, width, selected, maxLines int) string {
	positions := d.Positions
	header := sectionTitle.Render(fmt.Sprintf("Open Positions (%d)", len(positions)))
	header += dimText.Render(positionsNote(d))

	if len(positions) == 0 {
		if d.MalformedTrades > 0 {
			return header + "\n" + dimText.Render("  No valid open positions")
		}
		return header + "\n" + dimText.Render("  No open positions")
	}

	// The column header carries a leading marker column so the ▸ selection
	// cursor lines up with the rows below it.
	colFmt := "  %-8s %10s %10s %10s %8s %10s %10s %-10s"
	hdr := fmt.Sprintf(colFmt, "Ticker", "Entry", "Current", "P&L", "P&L%", "Stop", "Target", "Strategy")

	// Fixed chrome inside this block: title + column header (2).
	start, end := tableWindow(len(positions), selected, maxLines, 2)

	var rows strings.Builder
	rows.WriteString(header + "\n")
	rows.WriteString(tableHeader.Render(hdr) + "\n")

	for i := start; i < end; i++ {
		p := positions[i]
		pnlStr := colorPnL(p.PnL)
		pnlPctStr := colorPnLPct(p.PnLPct)
		marker := " "
		if i == selected {
			marker = "▸"
		}
		row := fmt.Sprintf("%s %-8s %10.2f %10.2f %10s %8s %10.2f %10.2f %-10s",
			marker, p.Ticker, p.EntryPrice, p.CurrentPrice, pnlStr, pnlPctStr,
			p.StopLoss, p.TakeProfit, p.Strategy)
		rows.WriteString(row + "\n")
	}

	if ind := scrollIndicator(start, end, len(positions)); ind != "" {
		rows.WriteString(ind + "\n")
	}

	// Trim the trailing newline so this block joins predictably with the "\n\n"
	// separators in RenderDashboard (each separator then adds exactly one blank
	// line, keeping the height budget accurate).
	return strings.TrimRight(rows.String(), "\n")
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

func renderWatchlistSummary(items []string, width int) string {
	header := sectionTitle.Render(fmt.Sprintf("Watchlist (%d)", len(items)))
	if len(items) == 0 {
		return header + "\n" + dimText.Render("  No active watchlist items")
	}
	// Hard-wrap the ticker list to the viewport width. Without this the terminal
	// wraps it visually into an unknown number of rows, which would break the
	// dashboard's height budget for the positions table above.
	body := "  " + strings.Join(items, ", ")
	if width > 2 {
		body = lipgloss.NewStyle().Width(width).Render(body)
	}
	return header + "\n" + body
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
	posStr := fmt.Sprintf("Positions: %d", d.OpenPositionCnt)
	if d.PositionLots > d.OpenPositionCnt {
		posStr += fmt.Sprintf(" (%d lots)", d.PositionLots)
	}
	parts := []string{
		fmt.Sprintf("Daily Loss: %.1f%%", d.DailyLossUsed),
		fmt.Sprintf("Drawdown: %.1f%%", d.DrawdownPct),
		posStr,
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
