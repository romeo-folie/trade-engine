package reports

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-echarts/go-echarts/v2/components"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// ── Performance Report ────────────────────────────────────────────────────────

// PerformanceData holds the pre-fetched data for a performance report.
type PerformanceData struct {
	From, To   time.Time
	Snapshots  []store.PortfolioSnapshot
	Summaries  []store.DailySummary
	Trades     []store.Trade
}

// BuildPerformancePage assembles the performance report HTML.
// Returns (bodyPrefix, page) where bodyPrefix contains the metrics section.
func BuildPerformancePage(d PerformanceData) (bodyPrefix string, page *components.Page, err error) {
	// ── Metrics ──────────────────────────────────────────────────────────────
	metrics := performanceMetrics(d)
	byStrategy := strategyBreakdown(d.Trades)
	byTicker := tickerBreakdown(d.Trades)
	topTrades := bestWorstTrades(d.Trades, 5)

	var sb strings.Builder
	sb.WriteString(`<div class="report-header">`)
	sb.WriteString(fmt.Sprintf(`<h1>Performance Report</h1><div class="subtitle">%s — %s</div>`,
		d.From.Format("Jan 2, 2006"), d.To.Format("Jan 2, 2006")))
	sb.WriteString("</div>")
	sb.WriteString(`<div class="metrics-grid">`)
	sb.WriteString(RenderMetricsTable("Key Metrics", metrics))
	sb.WriteString(RenderMetricsTable("By Strategy", byStrategy))
	sb.WriteString(RenderMetricsTable("By Ticker", byTicker))
	sb.WriteString("</div>")
	if len(topTrades) > 0 {
		sb.WriteString(`<div class="metrics-grid">`)
		sb.WriteString(RenderMetricsTable("Top & Bottom Trades", topTrades))
		sb.WriteString("</div>")
	}
	sb.WriteString(`<div class="section-title">Charts</div>`)

	// ── Charts ────────────────────────────────────────────────────────────────
	p := AssemblePage("Performance Report")
	if len(d.Snapshots) > 1 {
		p.AddCharts(
			EquityCurve("Equity Curve", d.Snapshots),
			DrawdownChart("Drawdown", d.Snapshots),
		)
	}
	if len(d.Summaries) > 0 {
		p.AddCharts(MonthlyHeatmap("Monthly Returns (%)", d.Summaries))
	}
	if len(d.Trades) > 0 {
		p.AddCharts(TradeScatter("Trade Distribution", d.Trades))
	}
	return sb.String(), p, nil
}

// ── Daily Report ──────────────────────────────────────────────────────────────

// DailyData holds the pre-fetched data for a daily report.
type DailyData struct {
	Date      time.Time
	Summary   *store.DailySummary
	Trades    []store.Trade
	Snapshots []store.PortfolioSnapshot
}

// BuildDailyPage assembles the daily summary report HTML.
func BuildDailyPage(d DailyData) (bodyPrefix string, page *components.Page, err error) {
	var sb strings.Builder
	sb.WriteString(`<div class="report-header">`)
	sb.WriteString(fmt.Sprintf(`<h1>Daily Report</h1><div class="subtitle">%s</div>`, d.Date.Format("Monday, January 2, 2006")))
	sb.WriteString("</div>")

	var metrics []MetricRow
	if d.Summary != nil {
		s := d.Summary
		metrics = []MetricRow{
			{"Starting Portfolio Value", fmt.Sprintf("$%.2f", s.StartingValue)},
			{"Ending Portfolio Value", fmt.Sprintf("$%.2f", s.EndingValue)},
			{"Daily P&L", fmt.Sprintf("$%.2f (%.2f%%)", s.PnL, s.PnLPct)},
			{"Trades Opened", fmt.Sprintf("%d", s.TradesOpened)},
			{"Trades Closed", fmt.Sprintf("%d", s.TradesClosed)},
			{"Wins / Losses", fmt.Sprintf("%d / %d", s.Wins, s.Losses)},
			{"Signals Generated", fmt.Sprintf("%d", s.SignalsGenerated)},
			{"Signals Blocked", fmt.Sprintf("%d", s.SignalsBlocked)},
			{"Model Tokens (In/Out)", fmt.Sprintf("%s / %s", fmtTokens(s.ModelTokensInput), fmtTokens(s.ModelTokensOutput))},
		}
	} else {
		metrics = []MetricRow{{"Note", "No summary data for this date"}}
	}

	sb.WriteString(`<div class="metrics-grid">`)
	sb.WriteString(RenderMetricsTable("Today's Summary", metrics))
	if len(d.Trades) > 0 {
		sb.WriteString(RenderMetricsTable("Trades Today", tradeRows(d.Trades)))
	}
	sb.WriteString("</div>")

	p := AssemblePage("Daily Report — " + d.Date.Format("Jan 2, 2006"))
	if len(d.Snapshots) > 1 {
		sb.WriteString(`<div class="section-title">Charts</div>`)
		p.AddCharts(EquityCurve("Intraday Portfolio Value", d.Snapshots))
	}
	return sb.String(), p, nil
}

// ── Weekly Report ─────────────────────────────────────────────────────────────

// WeeklyData holds the pre-fetched data for a weekly report.
type WeeklyData struct {
	From, To  time.Time
	Summaries []store.DailySummary
	Trades    []store.Trade
	Snapshots []store.PortfolioSnapshot
}

// BuildWeeklyPage assembles the weekly summary report HTML.
func BuildWeeklyPage(d WeeklyData) (bodyPrefix string, page *components.Page, err error) {
	var wins, losses int
	var totalPnL, totalPnLPct float64
	for _, s := range d.Summaries {
		wins += s.Wins
		losses += s.Losses
		totalPnL += s.PnL
		totalPnLPct += s.PnLPct
	}

	var sb strings.Builder
	sb.WriteString(`<div class="report-header">`)
	sb.WriteString(fmt.Sprintf(`<h1>Weekly Report</h1><div class="subtitle">%s — %s</div>`,
		d.From.Format("Jan 2"), d.To.Format("Jan 2, 2006")))
	sb.WriteString("</div>")

	sb.WriteString(`<div class="metrics-grid">`)
	sb.WriteString(RenderMetricsTable("Week Summary", []MetricRow{
		{"Period P&L", fmt.Sprintf("$%.2f", totalPnL)},
		{"Period Return", fmt.Sprintf("%.2f%%", totalPnLPct)},
		{"Total Wins", fmt.Sprintf("%d", wins)},
		{"Total Losses", fmt.Sprintf("%d", losses)},
		{"Win Rate", winRateStr(wins, losses)},
		{"Trades Closed", fmt.Sprintf("%d", len(d.Trades))},
	}))
	sb.WriteString(RenderMetricsTable("By Strategy", strategyBreakdown(d.Trades)))
	sb.WriteString("</div>")
	sb.WriteString(`<div class="section-title">Charts</div>`)

	p := AssemblePage("Weekly Report")
	if len(d.Snapshots) > 1 {
		p.AddCharts(
			EquityCurve("Weekly Equity Curve", d.Snapshots),
			DrawdownChart("Drawdown", d.Snapshots),
		)
	}
	if len(d.Summaries) > 0 {
		p.AddCharts(MonthlyHeatmap("Daily Returns (%)", d.Summaries))
	}
	if len(d.Trades) > 0 {
		p.AddCharts(TradeScatter("Trade Distribution", d.Trades))
	}
	return sb.String(), p, nil
}

// ── Backtest Report ───────────────────────────────────────────────────────────

// BacktestData holds the pre-fetched data for a single backtest report.
type BacktestData struct {
	Run    store.BacktestRun
	Trades []store.BacktestTrade
}

// BuildBacktestPage assembles the backtest report HTML.
func BuildBacktestPage(d BacktestData) (bodyPrefix string, page *components.Page, err error) {
	var sb strings.Builder
	sb.WriteString(`<div class="report-header">`)
	sb.WriteString(fmt.Sprintf(
		`<h1>Backtest Report — %s</h1><div class="subtitle">%s · %s/%s · %s — %s</div>`,
		d.Run.Label,
		d.Run.Ticker,
		d.Run.ModelProvider, d.Run.ModelName,
		d.Run.FromDate.Format("Jan 2, 2006"), d.Run.ToDate.Format("Jan 2, 2006"),
	))
	sb.WriteString("</div>")

	sb.WriteString(`<div class="metrics-grid">`)
	sb.WriteString(RenderMetricsTable("Performance Metrics", backtestMetricRows(d.Run)))
	sb.WriteString("</div>")
	sb.WriteString(`<div class="section-title">Charts</div>`)

	p := AssemblePage("Backtest — " + d.Run.Label)
	initialCash := 100_000.0 // default; real config not available here
	pts := BuildBacktestEquityCurve(d.Trades, initialCash)
	if len(pts) > 1 {
		p.AddCharts(
			EquityCurveFromBacktest("Equity Curve", pts),
			DrawdownChartFromValues("Drawdown", pts),
		)
	}
	if len(d.Trades) > 0 {
		p.AddCharts(TradeScatterFromBacktest("Trade Distribution", d.Trades))
	}
	return sb.String(), p, nil
}

// ── Backtest Comparison Report ────────────────────────────────────────────────

// BuildBacktestComparePage assembles a side-by-side backtest comparison HTML.
func BuildBacktestComparePage(a, b BacktestData) (bodyPrefix string, page *components.Page, err error) {
	var sb strings.Builder
	sb.WriteString(`<div class="report-header">`)
	sb.WriteString(fmt.Sprintf(`<h1>Backtest Comparison</h1><div class="subtitle">%s vs %s</div>`,
		a.Run.Label, b.Run.Label))
	sb.WriteString("</div>")

	// Side-by-side metrics.
	aRows := backtestMetricRows(a.Run)
	bRows := backtestMetricRows(b.Run)
	sb.WriteString(`<div class="compare-grid">`)
	sb.WriteString(fmt.Sprintf(`<div class="compare-col"><h3>%s — %s (%s)</h3>%s</div>`,
		a.Run.Label, a.Run.Ticker, a.Run.Strategy,
		RenderMetricsTable("", aRows)))
	sb.WriteString(fmt.Sprintf(`<div class="compare-col"><h3>%s — %s (%s)</h3>%s</div>`,
		b.Run.Label, b.Run.Ticker, b.Run.Strategy,
		RenderMetricsTable("", bRows)))
	sb.WriteString("</div>")
	sb.WriteString(`<div class="section-title">Equity Curves</div>`)

	// Overlaid equity curves.
	ptsA := BuildBacktestEquityCurve(a.Trades, 100_000)
	ptsB := BuildBacktestEquityCurve(b.Trades, 100_000)

	p := AssemblePage(fmt.Sprintf("Compare %s vs %s", a.Run.Label, b.Run.Label))
	if len(ptsA) > 1 || len(ptsB) > 1 {
		p.AddCharts(OverlayEquityCurve(a.Run.Label, b.Run.Label, ptsA, ptsB))
	}
	if len(a.Trades) > 0 {
		p.AddCharts(TradeScatterFromBacktest(a.Run.Label+" Trades", a.Trades))
	}
	if len(b.Trades) > 0 {
		p.AddCharts(TradeScatterFromBacktest(b.Run.Label+" Trades", b.Trades))
	}
	return sb.String(), p, nil
}

// ── Data helpers ──────────────────────────────────────────────────────────────

func performanceMetrics(d PerformanceData) []MetricRow {
	var startVal, endVal float64
	if len(d.Snapshots) > 0 {
		startVal = d.Snapshots[0].TotalValue
		endVal = d.Snapshots[len(d.Snapshots)-1].TotalValue
	}
	totalReturn := 0.0
	if startVal > 0 {
		totalReturn = (endVal - startVal) / startVal * 100
	}
	var wins, losses int
	for _, t := range d.Trades {
		if t.PnL >= 0 {
			wins++
		} else {
			losses++
		}
	}
	return []MetricRow{
		{"Period", fmt.Sprintf("%s — %s", d.From.Format("Jan 2, 2006"), d.To.Format("Jan 2, 2006"))},
		{"Starting Value", fmt.Sprintf("$%.2f", startVal)},
		{"Ending Value", fmt.Sprintf("$%.2f", endVal)},
		{"Total Return", fmt.Sprintf("%.2f%%", totalReturn)},
		{"Total Trades", fmt.Sprintf("%d", len(d.Trades))},
		{"Win Rate", winRateStr(wins, losses)},
	}
}

func strategyBreakdown(trades []store.Trade) []MetricRow {
	type stats struct{ wins, total int; pnl float64 }
	m := make(map[string]*stats)
	for _, t := range trades {
		s := t.Strategy
		if s == "" {
			s = "unknown"
		}
		if m[s] == nil {
			m[s] = &stats{}
		}
		m[s].total++
		m[s].pnl += t.PnL
		if t.PnL >= 0 {
			m[s].wins++
		}
	}
	var rows []MetricRow
	for strat, st := range m {
		wr := 0.0
		if st.total > 0 {
			wr = float64(st.wins) / float64(st.total) * 100
		}
		rows = append(rows, MetricRow{
			Name:  strat,
			Value: fmt.Sprintf("%d trades · %.0f%% win · $%.2f P&L", st.total, wr, st.pnl),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	if len(rows) == 0 {
		rows = append(rows, MetricRow{"—", "no closed trades"})
	}
	return rows
}

func tickerBreakdown(trades []store.Trade) []MetricRow {
	type stats struct{ wins, total int; pnl float64 }
	m := make(map[string]*stats)
	for _, t := range trades {
		if m[t.Ticker] == nil {
			m[t.Ticker] = &stats{}
		}
		m[t.Ticker].total++
		m[t.Ticker].pnl += t.PnL
		if t.PnL >= 0 {
			m[t.Ticker].wins++
		}
	}
	var rows []MetricRow
	for ticker, st := range m {
		wr := 0.0
		if st.total > 0 {
			wr = float64(st.wins) / float64(st.total) * 100
		}
		rows = append(rows, MetricRow{
			Name:  ticker,
			Value: fmt.Sprintf("%d trades · %.0f%% win · $%.2f P&L", st.total, wr, st.pnl),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	if len(rows) == 0 {
		rows = append(rows, MetricRow{"—", "no closed trades"})
	}
	return rows
}

func bestWorstTrades(trades []store.Trade, n int) []MetricRow {
	closed := make([]store.Trade, 0, len(trades))
	for _, t := range trades {
		if t.Status == "closed" {
			closed = append(closed, t)
		}
	}
	if len(closed) == 0 {
		return nil
	}
	sort.Slice(closed, func(i, j int) bool { return closed[i].PnLPct > closed[j].PnLPct })

	var rows []MetricRow
	for i := 0; i < n && i < len(closed); i++ {
		t := closed[i]
		rows = append(rows, MetricRow{
			Name:  fmt.Sprintf("Best #%d — %s %s", i+1, t.Ticker, t.ClosedAt.Format("Jan 2")),
			Value: fmt.Sprintf("+%.2f%% ($%.2f)", t.PnLPct, t.PnL),
		})
	}
	sort.Slice(closed, func(i, j int) bool { return closed[i].PnLPct < closed[j].PnLPct })
	for i := 0; i < n && i < len(closed); i++ {
		t := closed[i]
		rows = append(rows, MetricRow{
			Name:  fmt.Sprintf("Worst #%d — %s %s", i+1, t.Ticker, t.ClosedAt.Format("Jan 2")),
			Value: fmt.Sprintf("%.2f%% ($%.2f)", t.PnLPct, t.PnL),
		})
	}
	return rows
}

func tradeRows(trades []store.Trade) []MetricRow {
	var rows []MetricRow
	for _, t := range trades {
		direction := "↑"
		if t.Direction == "sell" {
			direction = "↓"
		}
		pnlStr := fmt.Sprintf("$%.2f (%.2f%%)", t.PnL, t.PnLPct)
		rows = append(rows, MetricRow{
			Name:  fmt.Sprintf("%s %s %s", direction, t.Ticker, t.Strategy),
			Value: pnlStr,
		})
	}
	return rows
}

func backtestMetricRows(r store.BacktestRun) []MetricRow {
	f := func(v *float64, format string) string {
		if v == nil {
			return "—"
		}
		return fmt.Sprintf(format, *v)
	}
	return []MetricRow{
		{"Ticker", r.Ticker},
		{"Strategy", r.Strategy},
		{"Period", fmt.Sprintf("%s — %s", r.FromDate.Format("Jan 2, 2006"), r.ToDate.Format("Jan 2, 2006"))},
		{"Model", r.ModelProvider + "/" + r.ModelName},
		{"Total Return", f(r.TotalReturnPct, "%.2f%%")},
		{"Annualized Return", f(r.AnnualizedReturnPct, "%.2f%%")},
		{"Sharpe Ratio", f(r.SharpeRatio, "%.3f")},
		{"Sortino Ratio", f(r.SortinoRatio, "%.3f")},
		{"Calmar Ratio", f(r.CalmarRatio, "%.3f")},
		{"Max Drawdown", f(r.MaxDrawdownPct, "%.2f%%")},
		{"Win Rate", f(r.WinRatePct, "%.1f%%")},
		{"Profit Factor", f(r.ProfitFactor, "%.3f")},
		{"Expectancy", f(r.Expectancy, "$%.2f")},
		{"Total Trades", fmt.Sprintf("%d", r.TotalTrades)},
		{"Avg Hold (hours)", f(r.AvgHoldHours, "%.1f")},
		{"Best Trade", f(r.BestTradePct, "+%.2f%%")},
		{"Worst Trade", f(r.WorstTradePct, "%.2f%%")},
		{"Prompt Cache Hit Rate", f(r.CacheHitRate, "%.1f%%")},
	}
}

func winRateStr(wins, losses int) string {
	total := wins + losses
	if total == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%% (%d/%d)", float64(wins)/float64(total)*100, wins, total)
}

func fmtTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
