// Package views contains individual TUI view models for the dashboard.
package views

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// drawdownLookbackDays bounds the window over which the portfolio peak is taken
// for the drawdown metric, so a single ancient/glitched snapshot can't inflate
// it indefinitely.
const drawdownLookbackDays = 90

// DashboardData holds all data needed to render the main dashboard view.
// Populated by polling the DB or derived from events.
type DashboardData struct {
	// Header
	Mode          string
	PaperMode     bool
	EngineRunning bool

	// Portfolio summary
	TotalValue         float64
	Cash               float64
	Invested           float64
	TodayPnL           float64
	TodayPnLAvailable  bool
	UnrealizedPnL      float64
	PortfolioAvailable bool
	PortfolioEstimated bool
	PortfolioAsOf      time.Time
	PortfolioSource    string

	// Open positions, aggregated per (ticker, direction).
	Positions []PositionRow

	// Position quality / freshness metadata.
	PositionLots        int       // total valid open trade rows before aggregation
	MalformedTrades     int       // open trades with entry_price<=0 or quantity<=0 (excluded)
	OldestMarkAt        time.Time // oldest non-zero mark timestamp across displayed positions
	UnmarkedCount       int       // positions without a usable broker/candle mark
	ReconcileOnlyCount  int       // DB-owned rows absent from the broker book
	BrokerAuthoritative bool      // position count/economics came from a live broker
	DBExposureSignature string    // deterministic identity of DB-owned position lifecycle
	DBExposureKnown     bool      // signature came from a successful DB query

	// Pending approvals (manual mode)
	PendingSignals []SignalRow

	// Recent signals
	RecentSignals []SignalRow

	// Risk
	DailyLossUsed       float64 // percentage of the configured daily loss limit consumed
	DailyLossAvailable  bool
	DrawdownPct         float64
	DrawdownPeak        float64 // rolling peak used to keep broker overlays coherent
	DrawdownAvailable   bool
	OpenPositionCnt     int
	LargestPosPct       float64 // largest position as % of portfolio
	LargestPosAvailable bool

	// Watchlist summary
	Watchlist []string

	// BrokerRecon is the latest broker-vs-DB position reconciliation, populated
	// out-of-band by the TUI (not by FetchDashboardData). Zero value renders
	// nothing.
	BrokerRecon BrokerReconResult

	LastUpdate time.Time
}

// PositionRow is a single row in the open positions table. When positions are
// aggregated per ticker, EntryPrice is the quantity-weighted average across
// lots and Lots is how many open trades were folded into this row.
type PositionRow struct {
	Ticker          string
	Direction       string
	EntryPrice      float64
	CurrentPrice    float64
	Quantity        float64
	PnL             float64
	PnLPct          float64
	StopLoss        float64
	TakeProfit      float64
	Strategy        string
	Lots            int
	Status          string
	MarkAsOf        time.Time
	MarkSource      string // "broker" | "candle" | ""
	MarkAvailable   bool
	ExitPolicyMixed bool // constituent lots disagree on stop or fixed target
	StrategyMixed   bool // constituent lots have different strategies
	StatusMixed     bool // constituent lots are in different lifecycle states
	ReconcileOnly   bool // DB lifecycle metadata only; no broker exposure
}

// BrokerDashboardSnapshot is a coherent broker read captured outside the TUI
// render path. Broker economics are authoritative; DB rows only enrich them
// with strategy and exit-policy metadata.
type BrokerDashboardSnapshot struct {
	Account             *execution.BrokerAccount
	Positions           []execution.BrokerPosition
	FetchedAt           time.Time
	DBExposureSignature string
	DBExposureKnown     bool
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

// deriveCashAndTotal computes a long-only fallback cash balance and total from
// configured initial capital, cost basis, and current marked exposure. Callers
// must not use it for shorts or margin books.
//
// cash      = max(0, initialCapital - costBasis)
// total     = cash + invested
//
// Cost basis must be accumulated per-trade as entryPrice×qty.
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
func FetchDashboardData(
	ctx context.Context,
	mode string,
	initialCapital float64,
	portfolioMode, portfolioBroker string,
) (d DashboardData) {
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
	d.PortfolioAvailable = initialCapital > 0
	d.PortfolioEstimated = d.PortfolioAvailable

	hasPortfolioSnapshot := false
	// Prefer the latest portfolio snapshot if available (Alpaca-backed).
	if portfolioMode == "" {
		portfolioMode = "live"
	}
	if snap, err := store.GetLatestPortfolioSnapshotByScope(ctx, portfolioMode, portfolioBroker); err == nil && snap != nil {
		hasPortfolioSnapshot = true
		// A wildcard is only for discovering the newest book. Account-level
		// metrics must remain in that selected book; otherwise an older,
		// higher-valued broker account can create a false drawdown.
		portfolioMode, portfolioBroker = resolvedSnapshotScope(portfolioMode, portfolioBroker, snap)
		d.PortfolioAvailable = true
		d.PortfolioEstimated = false
		d.PortfolioAsOf = snap.CreatedAt
		d.PortfolioSource = snap.Source
		d.Cash = snap.Cash
		d.TotalValue = snap.TotalValue
		d.Invested = snap.Invested
		d.UnrealizedPnL = snap.UnrealizedPnL
		d.OpenPositionCnt = snap.OpenPositions
	}

	// Fetch owned trades and aggregate them per (ticker, direction), retaining
	// each displayed position's own candle timestamp.
	trades, err := store.GetOwnedPositionTrades(ctx)
	if err == nil {
		d.DBExposureKnown = true
		d.DBExposureSignature = DBExposureSignature(trades)
		markPrice := func(ticker string) (float64, time.Time) {
			if c, cerr := store.GetFreshestCandle(ctx, ticker); cerr == nil && c != nil && c.Close > 0 {
				return c.Close, c.Timestamp
			}
			return 0, time.Time{}
		}
		d.Positions, d.PositionLots, d.MalformedTrades, d.OldestMarkAt, d.UnmarkedCount = aggregateOpenPositions(trades, markPrice)

		// Aggregate figures derived from the marked rows.
		var visibleInvested, largestVisiblePosValue, localUnrealizedPnL, totalCostBasis float64
		hasShort := false
		for _, p := range d.Positions {
			posValue := p.CurrentPrice * p.Quantity
			costBasis := p.EntryPrice * p.Quantity
			visibleInvested += posValue
			totalCostBasis += costBasis
			localUnrealizedPnL += p.PnL
			hasShort = hasShort || normalizePositionDirection(p.Direction, p.Quantity) == "short"
			if posValue > largestVisiblePosValue {
				largestVisiblePosValue = posValue
			}
		}

		// Distinct-position count (not raw lot count) is the honest exposure.
		if !hasPortfolioSnapshot && len(d.Positions) > 0 {
			d.OpenPositionCnt = len(d.Positions)
		}

		if !hasPortfolioSnapshot && d.UnmarkedCount == 0 && !hasShort {
			// Fallback: derive the entire summary from the same candle marks.
			// Never splice candle P&L into an otherwise broker-backed snapshot.
			d.Invested = visibleInvested
			d.Cash, d.TotalValue = deriveCashAndTotal(initialCapital, totalCostBasis, d.Invested)
			d.UnrealizedPnL = localUnrealizedPnL
		} else if !hasPortfolioSnapshot && (d.UnmarkedCount > 0 || hasShort) {
			// A partial mark set or a short/margin book cannot produce a
			// coherent account summary from position rows alone.
			d.PortfolioAvailable = false
			d.PortfolioEstimated = false
		}

		// Risk: largest position % (stays consistent with displayed rows).
		// A partial mark set would understate the largest holding.
		setLargestPositionRisk(
			&d, largestVisiblePosValue,
			d.PortfolioAvailable && d.UnmarkedCount == 0,
		)
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

	// This is explicitly an all-ledger terminal-exit total. The underlying
	// ledgers cannot scope partial realizations to this dashboard account, so it
	// must not be used as the live account's daily-loss budget.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if pnl, pnlErr := store.GetRealizedPnLByDateRange(ctx, today, today.Add(24*time.Hour)); pnlErr == nil {
		d.TodayPnL = pnl
		d.TodayPnLAvailable = true
	}

	// Drawdown from a rolling peak. Using MAX(total_value) over all time let a
	// single glitched snapshot (e.g. an April spike to $208k on a $100k account)
	// dominate the figure forever; a bounded lookback tracks the recent peak and
	// ignores non-positive rows.
	// peak > 0 guards against a division by zero when the window has no positive
	// snapshots (peak == 0) and the latest value is itself a negative glitch.
	if peak, perr := store.GetPeakPortfolioValueByScope(
		ctx, time.Now().AddDate(0, 0, -drawdownLookbackDays),
		portfolioMode, portfolioBroker,
	); perr == nil && peak > 0 {
		d.DrawdownPeak = peak
		d.DrawdownAvailable = d.PortfolioAvailable
	}
	if d.DrawdownPeak > d.TotalValue {
		d.DrawdownPct = ((d.DrawdownPeak - d.TotalValue) / d.DrawdownPeak) * 100
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

// DBExposureSignature identifies the DB lifecycle state that a broker response
// was reconciled against. Ordering is removed so routine query-order changes do
// not invalidate an otherwise identical book.
func DBExposureSignature(trades []store.Trade) string {
	parts := make([]string, 0, len(trades))
	for _, trade := range trades {
		parts = append(parts, fmt.Sprintf(
			"%s|%s|%s|%s|%.8f",
			trade.ID, trade.Ticker,
			normalizePositionDirection(trade.Direction, trade.Quantity),
			trade.Status, trade.Quantity,
		))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

func resolvedSnapshotScope(mode, broker string, snap *store.PortfolioSnapshot) (string, string) {
	if snap == nil {
		return mode, broker
	}
	if mode == "*" {
		mode = snap.Mode
	}
	if broker == "*" {
		broker = snap.Broker
	}
	return mode, broker
}

func setLargestPositionRisk(d *DashboardData, largest float64, complete bool) {
	d.LargestPosPct = 0
	d.LargestPosAvailable = false
	if complete && largest > 0 && d.TotalValue > 0 {
		d.LargestPosPct = largest / d.TotalValue * 100
		d.LargestPosAvailable = true
	}
}

// aggregateOpenPositions folds open trade lots into one row per
// (ticker, direction), marked to the price returned by markPrice (which returns
// a zero price + zero time when no usable mark exists, in which case the row
// remains explicitly unavailable). EntryPrice on each row is the
// quantity-weighted average across its lots; PnL uses the correct sign for
// shorts. Malformed lots
// (entry_price <= 0 or quantity <= 0) are excluded and counted separately.
//
// Extracted from FetchDashboardData so the money math is unit-testable without
// a database. Rows are returned in first-seen (opened_at DESC) order.
func aggregateOpenPositions(
	trades []store.Trade,
	markPrice func(ticker string) (price float64, ts time.Time),
) (rows []PositionRow, lots, malformed int, oldest time.Time, unmarked int) {
	type group struct {
		row       PositionRow
		costBasis float64
		marked    bool // a real candle mark was found for this ticker
	}
	groups := make(map[string]*group)
	order := make([]string, 0, len(trades))
	type cachedMark struct {
		price float64
		at    time.Time
	}
	marks := make(map[string]cachedMark)

	for _, t := range trades {
		if t.EntryPrice <= 0 || t.Quantity <= 0 {
			malformed++
			continue
		}
		lots++

		// markPrice is deterministic per ticker, so all lots of a group share
		// the same result — either a real mark or none.
		mark, lookedUp := marks[t.Ticker]
		if !lookedUp {
			mark.price, mark.at = markPrice(t.Ticker)
			marks[t.Ticker] = mark
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
				Status:     t.Status,
			}}
			groups[key] = g
			order = append(order, key)
		} else {
			if g.row.StopLoss != t.StopLoss || g.row.TakeProfit != t.TakeProfit {
				g.row.ExitPolicyMixed = true
			}
			if g.row.Strategy != t.Strategy {
				g.row.StrategyMixed = true
			}
			if g.row.Status != t.Status {
				g.row.StatusMixed = true
			}
		}
		g.row.Quantity += t.Quantity
		g.costBasis += t.EntryPrice * t.Quantity
		g.row.Lots++
		if mark.price > 0 {
			g.row.CurrentPrice = mark.price // identical across a ticker's lots
			g.marked = true
			g.row.MarkAvailable = true
			g.row.MarkAsOf = mark.at
			g.row.MarkSource = "candle"
		}
	}

	for _, key := range order {
		g := groups[key]
		if g.row.Quantity > 0 {
			g.row.EntryPrice = g.costBasis / g.row.Quantity // weighted average
		}
		if g.row.StatusMixed {
			g.row.Status = "mixed"
		}
		if !g.marked {
			unmarked++
			rows = append(rows, g.row)
			continue
		}
		if !g.row.MarkAsOf.IsZero() && (oldest.IsZero() || g.row.MarkAsOf.Before(oldest)) {
			oldest = g.row.MarkAsOf
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
	return rows, lots, malformed, oldest, unmarked
}

func normalizePositionDirection(side string, qty float64) string {
	side = strings.ToLower(strings.TrimSpace(side))
	if side == "short" || qty < 0 {
		return "short"
	}
	return "long"
}

// ApplyBrokerDashboardSnapshot replaces estimated DB/candle economics with a
// single broker snapshot while retaining DB-owned strategy and exit metadata.
func ApplyBrokerDashboardSnapshot(d *DashboardData, snap BrokerDashboardSnapshot, dailyLossLimitPct float64) {
	if d == nil || snap.Account == nil {
		return
	}
	if snap.FetchedAt.IsZero() {
		snap.FetchedAt = time.Now()
	}

	metadataRows := d.Positions
	metadata := make(map[string]PositionRow, len(metadataRows))
	for _, row := range metadataRows {
		metadata[row.Ticker+"|"+normalizePositionDirection(row.Direction, row.Quantity)] = row
	}

	rows := make([]PositionRow, 0, len(snap.Positions))
	matched := make(map[string]bool, len(snap.Positions))
	var unrealized, largest, grossExposure float64
	var unmarked, brokerUnmarked int
	for _, position := range snap.Positions {
		direction := normalizePositionDirection(position.Side, position.Quantity)
		key := position.Symbol + "|" + direction
		row := metadata[key]
		matched[key] = true
		row.Ticker = position.Symbol
		row.Direction = direction
		row.EntryPrice = position.AvgEntryPrice
		row.CurrentPrice = position.CurrentPrice
		row.Quantity = math.Abs(position.Quantity)
		row.PnL = position.UnrealizedPL
		row.MarkAvailable = position.CurrentPrice > 0
		row.MarkAsOf = snap.FetchedAt
		row.MarkSource = "broker"
		if row.Strategy == "" {
			row.Strategy = "broker-only"
		}
		if !row.MarkAvailable {
			unmarked++
			brokerUnmarked++
		}
		if row.Status == "" {
			row.Status = "open"
		}
		costBasis := row.EntryPrice * row.Quantity
		if costBasis > 0 {
			row.PnLPct = row.PnL / costBasis * 100
		}
		marketValue := math.Abs(position.MarketValue)
		if marketValue == 0 && row.MarkAvailable {
			marketValue = row.CurrentPrice * row.Quantity
		}
		unrealized += row.PnL
		if marketValue > largest {
			largest = marketValue
		}
		grossExposure += marketValue
		rows = append(rows, row)
	}
	reconcileOnly := 0
	for _, row := range metadataRows {
		key := row.Ticker + "|" + normalizePositionDirection(row.Direction, row.Quantity)
		if matched[key] {
			continue
		}
		row.CurrentPrice = 0
		row.Quantity = 0
		row.PnL = 0
		row.PnLPct = 0
		row.MarkAvailable = false
		row.MarkAsOf = time.Time{}
		row.MarkSource = ""
		row.ReconcileOnly = true
		if strings.EqualFold(row.Status, "exiting") {
			row.Status = "awaiting"
		} else {
			row.Status = "db-only"
		}
		rows = append(rows, row)
		reconcileOnly++
		unmarked++
	}

	d.Positions = rows
	d.OpenPositionCnt = len(snap.Positions)
	d.BrokerAuthoritative = true
	d.ReconcileOnlyCount = reconcileOnly
	d.OldestMarkAt = snap.FetchedAt
	if len(snap.Positions) == 0 || brokerUnmarked == len(snap.Positions) {
		d.OldestMarkAt = time.Time{}
	}
	d.UnmarkedCount = unmarked

	d.TotalValue = snap.Account.Equity
	d.Cash = snap.Account.Cash
	d.Invested = grossExposure
	d.UnrealizedPnL = unrealized
	d.PortfolioAvailable = true
	d.PortfolioEstimated = false
	d.PortfolioAsOf = snap.FetchedAt
	d.PortfolioSource = "broker"
	d.DrawdownPct = 0
	d.DrawdownAvailable = d.DrawdownPeak > 0
	if d.DrawdownPeak > d.TotalValue && d.DrawdownPeak > 0 {
		d.DrawdownPct = (d.DrawdownPeak - d.TotalValue) / d.DrawdownPeak * 100
	}
	setLargestPositionRisk(d, largest, brokerUnmarked == 0)
	d.DailyLossAvailable = snap.Account.LastEquity > 0 && dailyLossLimitPct > 0
	d.DailyLossUsed = dailyLossBudgetUsed(
		snap.Account.Equity, snap.Account.LastEquity, dailyLossLimitPct,
	)
}

func dailyLossBudgetUsed(equity, lastEquity, limitPct float64) float64 {
	if equity >= lastEquity || lastEquity <= 0 || limitPct <= 0 {
		return 0
	}
	lossPct := (lastEquity - equity) / lastEquity * 100
	return lossPct / limitPct * 100
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
			Background(lipgloss.Color("237"))
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
	if line := renderBrokerReconLine(d.BrokerRecon); line != "" {
		bottom.WriteString(line)
		bottom.WriteString("\n")
	}
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
	title := "Portfolio"
	if d.PortfolioEstimated {
		title += " (estimated)"
	}
	if !d.PortfolioAvailable {
		title += " (summary unavailable)"
	}
	if d.PortfolioAvailable && !d.PortfolioAsOf.IsZero() {
		source := strings.TrimSpace(d.PortfolioSource)
		if source == "" {
			source = "snapshot"
		}
		age := time.Since(d.PortfolioAsOf)
		if age < 0 {
			age = 0
		}
		title += fmt.Sprintf(" · %s as of %s (%s ago)",
			source, d.PortfolioAsOf.Local().Format("15:04:05"), formatDuration(age))
	}
	total := fmt.Sprintf("$%.2f", d.TotalValue)
	cash := fmt.Sprintf("$%.2f", d.Cash)
	invested := fmt.Sprintf("$%.2f", d.Invested)
	unrealized := colorPnL(d.UnrealizedPnL)
	if !d.PortfolioAvailable {
		total, cash, invested, unrealized = "—", "—", "—", "—"
	}
	pnl := "—"
	if d.TodayPnLAvailable {
		pnl = colorPnL(d.TodayPnL)
	}
	exposureLabel := "Invested"
	if d.BrokerAuthoritative {
		exposureLabel = "Gross exposure"
	}

	return sectionTitle.Render(title) + "\n" +
		fmt.Sprintf("  Total: %-14s  Cash: %-14s  %s: %-14s", total, cash, exposureLabel, invested) + "\n" +
		fmt.Sprintf("  Closed today (all ledgers): %-14s  Unrealized: %s", pnl, unrealized)
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
	if d.UnmarkedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d unmarked", d.UnmarkedCount))
	}
	if d.ReconcileOnlyCount > 0 {
		parts = append(parts, fmt.Sprintf("%d DB-only lifecycle", d.ReconcileOnlyCount))
	}
	if !d.OldestMarkAt.IsZero() {
		age := time.Since(d.OldestMarkAt)
		mark := "oldest mark " + formatDuration(age) + " ago"
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
	positionCount := len(positions)
	if d.BrokerAuthoritative {
		positionCount = d.OpenPositionCnt
	}
	header := sectionTitle.Render(fmt.Sprintf("Open Positions (%d)", positionCount))
	header += dimText.Render(positionsNote(d))
	if width > 0 && ansi.StringWidth(header) > width {
		header = ansi.Truncate(header, width, "…")
	}

	if len(positions) == 0 {
		if d.MalformedTrades > 0 {
			return header + "\n" + dimText.Render("  No valid open positions")
		}
		return header + "\n" + dimText.Render("  No open positions")
	}

	// The column header carries a leading marker column so the ▸ selection
	// cursor lines up with the rows below it.
	layout := positionLayoutForWidth(width)
	hdr := layout.header()

	// Fixed chrome inside this block: title + column header (2).
	start, end := tableWindow(len(positions), selected, maxLines, 2)

	var rows strings.Builder
	rows.WriteString(header + "\n")
	rows.WriteString(tableHeader.Render(hdr) + "\n")

	for i := start; i < end; i++ {
		p := positions[i]
		currentField := fmt.Sprintf("%10s", "—")
		pnlField := fmt.Sprintf("%10s", "—")
		pnlPctField := fmt.Sprintf("%8s", "—")
		if positionHasMark(p) {
			currentField = fmt.Sprintf("%10.2f", p.CurrentPrice)
			// Pad before applying ANSI color. Passing a colored string through a
			// width verb makes fmt count invisible escape bytes and shifts every
			// column that follows it to the left.
			pnlField = colorPnLField(p.PnL, 10)
			pnlPctField = colorPnLPctField(p.PnLPct, 8)
		}
		marker := " "
		if i == selected {
			marker = "▸"
		}
		status := strings.ToUpper(p.Status)
		if status == "" {
			status = "OPEN"
		}
		stop, target := formatStop(p.StopLoss), formatTarget(p.StopLoss, p.TakeProfit)
		strategy := p.Strategy
		if p.ExitPolicyMixed {
			stop, target = "MIXED", "MIXED"
		}
		if p.StrategyMixed {
			strategy = "MIXED"
		}
		row := layout.row(
			marker, truncateCell(p.Ticker, 8), p.EntryPrice,
			currentField, pnlField, pnlPctField, stop, target,
			formatMarkAge(p), truncateCell(status, 8), truncateCell(strategy, 10),
		)
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
	dailyLoss := "Broker equity day-loss: —"
	if d.DailyLossAvailable {
		dailyLoss = fmt.Sprintf("Broker equity day-loss: %.1f%% of configured reference", d.DailyLossUsed)
	}
	largest := "Largest: —"
	if d.LargestPosAvailable {
		largest = fmt.Sprintf("Largest: %.1f%%", d.LargestPosPct)
	}
	drawdown := "Drawdown: —"
	if d.DrawdownAvailable {
		drawdown = fmt.Sprintf("Drawdown: %.1f%%", d.DrawdownPct)
	}
	parts := []string{
		dailyLoss,
		drawdown,
		posStr,
		largest,
	}

	style := lipgloss.NewStyle().
		Foreground(lipgloss.Color("250")).
		Background(lipgloss.Color("235")).
		Width(width).
		Padding(0, 1)

	return style.Render("Risk: " + strings.Join(parts, "  │  "))
}

func formatStop(stop float64) string {
	if stop <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f", stop)
}

func formatTarget(stop, target float64) string {
	switch {
	case target > 0:
		return fmt.Sprintf("%.2f", target)
	case stop > 0:
		return "UNCAPPED"
	default:
		return "—"
	}
}

func formatMarkAge(p PositionRow) string {
	if !positionHasMark(p) || p.MarkAsOf.IsZero() {
		return "—"
	}
	age := time.Since(p.MarkAsOf)
	if age < 0 {
		age = 0
	}
	prefix := "C"
	if p.MarkSource == "broker" {
		prefix = "B"
	}
	if age > 26*time.Hour {
		hours := int(age.Hours())
		if hours > 999 {
			hours = 999
		}
		return fmt.Sprintf("%s %dh!", prefix, hours)
	}
	value := formatDuration(age)
	return prefix + " " + value
}

func positionHasMark(p PositionRow) bool {
	return p.MarkAvailable
}

// ── Helpers ───────────────────────────────────────────────────────────────────

const (
	positionColumnFormat = "  %-8s %10s %10s %10s %8s %10s %10s %-8s %-8s %-10s"
	positionRowFormat    = "%s %-8s %10.2f %s %s %s %10s %10s %-8s %-8s %-10s"
)

type positionLayout int

const (
	positionLayoutNarrow positionLayout = iota
	positionLayoutCompact
	positionLayoutFull
)

func positionLayoutForWidth(width int) positionLayout {
	switch {
	case width <= 0 || width >= 110:
		return positionLayoutFull
	case width >= 96:
		return positionLayoutCompact
	default:
		return positionLayoutNarrow
	}
}

func (l positionLayout) header() string {
	switch l {
	case positionLayoutNarrow:
		return fmt.Sprintf("  %-8s %10s %10s %10s %-8s %-10s",
			"Ticker", "Mark", "P&L", "Target", "State", "Strategy")
	case positionLayoutCompact:
		return fmt.Sprintf("  %-8s %10s %10s %10s %8s %10s %-8s %-10s",
			"Ticker", "Entry", "Mark", "P&L", "P&L%", "Target", "State", "Strategy")
	default:
		return fmt.Sprintf(positionColumnFormat,
			"Ticker", "Entry", "Mark", "P&L", "P&L%", "Stop", "Target", "Age", "State", "Strategy")
	}
}

func (l positionLayout) row(
	marker, ticker string,
	entry float64,
	markField, pnlField, pnlPctField, stop, target, age, status, strategy string,
) string {
	switch l {
	case positionLayoutNarrow:
		return fmt.Sprintf("%s %-8s %s %s %10s %-8s %-10s",
			marker, ticker, markField, pnlField, target, status, strategy)
	case positionLayoutCompact:
		return fmt.Sprintf("%s %-8s %10.2f %s %s %s %10s %-8s %-10s",
			marker, ticker, entry, markField, pnlField, pnlPctField, target, status, strategy)
	default:
		return fmt.Sprintf(positionRowFormat,
			marker, ticker, entry, markField, pnlField, pnlPctField,
			stop, target, age, status, strategy)
	}
}

func truncateCell(s string, width int) string {
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width])
}

func pnlStyle(v float64) lipgloss.Style {
	switch {
	case v > 0:
		return greenText
	case v < 0:
		return redText
	default:
		return dimText
	}
}

func formatPnL(v float64) string {
	s := fmt.Sprintf("$%.2f", v)
	if v > 0 {
		return "+" + s
	}
	return s
}

func formatPnLPct(v float64) string {
	s := fmt.Sprintf("%.1f%%", v)
	if v > 0 {
		return "+" + s
	}
	return s
}

func colorPnL(v float64) string {
	return pnlStyle(v).Render(formatPnL(v))
}

func colorPnLPct(v float64) string {
	return pnlStyle(v).Render(formatPnLPct(v))
}

func colorPnLField(v float64, width int) string {
	return pnlStyle(v).Render(fmt.Sprintf("%*s", width, formatPnL(v)))
}

func colorPnLPctField(v float64, width int) string {
	return pnlStyle(v).Render(fmt.Sprintf("%*s", width, formatPnLPct(v)))
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
