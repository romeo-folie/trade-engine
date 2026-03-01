package reports

import (
	"bytes"
	"fmt"
	"math"
	"time"

	"github.com/go-echarts/go-echarts/v2/charts"
	"github.com/go-echarts/go-echarts/v2/components"
	"github.com/go-echarts/go-echarts/v2/opts"
	"github.com/go-echarts/go-echarts/v2/types"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// ── Dark-theme palette ────────────────────────────────────────────────────────

const (
	colorGreen  = "#00e676"
	colorRed    = "#ff1744"
	colorCyan   = "#00bcd4"
	colorAmber  = "#ffc107"
	colorPurple = "#9c27b0"
	colorGray   = "#546e7a"
	bgColor     = "#1a1a2e"
	textColor   = "#e0e0e0"
	gridColor   = "#2a2a4a"
)

// ── Equity Curve ──────────────────────────────────────────────────────────────

// EquityCurve builds a zoomable line chart of portfolio value over time.
func EquityCurve(title string, snapshots []store.PortfolioSnapshot) *charts.Line {
	line := charts.NewLine()
	line.SetGlobalOptions(
		charts.WithInitializationOpts(darkInit("100%", "320px")),
		charts.WithTitleOpts(opts.Title{Title: title, TitleStyle: &opts.TextStyle{Color: textColor}}),
		charts.WithTooltipOpts(opts.Tooltip{Show: opts.Bool(true), Trigger: "axis"}),
		charts.WithDataZoomOpts(opts.DataZoom{Type: "inside", Start: 0, End: 100}),
		charts.WithDataZoomOpts(opts.DataZoom{Type: "slider", Start: 0, End: 100}),
		charts.WithXAxisOpts(opts.XAxis{
			Type:        "category",
			AxisLabel:   &opts.AxisLabel{Color: textColor, Rotate: 30},
			SplitLine:   &opts.SplitLine{Show: opts.Bool(false)},
			AxisLine:    &opts.AxisLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithYAxisOpts(opts.YAxis{
			Name:        "Portfolio Value ($)",

			AxisLabel:   &opts.AxisLabel{Color: textColor, Formatter: "${value}"},
			SplitLine:   &opts.SplitLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithLegendOpts(opts.Legend{Show: opts.Bool(false)}),
	)

	xData := make([]string, len(snapshots))
	yData := make([]opts.LineData, len(snapshots))
	for i, s := range snapshots {
		xData[i] = s.CreatedAt.Format("2006-01-02 15:04")
		yData[i] = opts.LineData{Value: math.Round(s.TotalValue*100) / 100}
	}

	line.SetXAxis(xData).AddSeries("Portfolio", yData,
		charts.WithLineStyleOpts(opts.LineStyle{Color: colorCyan, Width: 2}),
		charts.WithAreaStyleOpts(opts.AreaStyle{Color: "rgba(0,188,212,0.1)"}),
		charts.WithItemStyleOpts(opts.ItemStyle{Color: colorCyan}),
	)
	return line
}

// EquityCurveFromBacktest builds an equity curve from a sequence of simulated
// portfolio values (one per trade exit event).
func EquityCurveFromBacktest(title string, values []BacktestPoint) *charts.Line {
	line := charts.NewLine()
	line.SetGlobalOptions(
		charts.WithInitializationOpts(darkInit("100%", "320px")),
		charts.WithTitleOpts(opts.Title{Title: title, TitleStyle: &opts.TextStyle{Color: textColor}}),
		charts.WithTooltipOpts(opts.Tooltip{Show: opts.Bool(true), Trigger: "axis"}),
		charts.WithDataZoomOpts(opts.DataZoom{Type: "inside", Start: 0, End: 100}),
		charts.WithDataZoomOpts(opts.DataZoom{Type: "slider", Start: 0, End: 100}),
		charts.WithXAxisOpts(opts.XAxis{
			Type:      "category",
			AxisLabel: &opts.AxisLabel{Color: textColor, Rotate: 30},
			SplitLine: &opts.SplitLine{Show: opts.Bool(false)},
			AxisLine:  &opts.AxisLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithYAxisOpts(opts.YAxis{
			Name:          "Portfolio Value ($)",

			AxisLabel:     &opts.AxisLabel{Color: textColor, Formatter: "${value}"},
			SplitLine:     &opts.SplitLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithLegendOpts(opts.Legend{Show: opts.Bool(false)}),
	)

	xData := make([]string, len(values))
	yData := make([]opts.LineData, len(values))
	for i, v := range values {
		xData[i] = v.Date.Format("2006-01-02")
		yData[i] = opts.LineData{Value: math.Round(v.Value*100) / 100}
	}

	line.SetXAxis(xData).AddSeries("Portfolio", yData,
		charts.WithLineStyleOpts(opts.LineStyle{Color: colorCyan, Width: 2}),
		charts.WithAreaStyleOpts(opts.AreaStyle{Color: "rgba(0,188,212,0.1)"}),
		charts.WithItemStyleOpts(opts.ItemStyle{Color: colorCyan}),
	)
	return line
}

// ── Drawdown Chart ────────────────────────────────────────────────────────────

// DrawdownChart builds an area chart showing drawdown from peak.
func DrawdownChart(title string, snapshots []store.PortfolioSnapshot) *charts.Line {
	dds := computeDrawdownSeries(func(i int) float64 { return snapshots[i].TotalValue }, len(snapshots))

	line := charts.NewLine()
	line.SetGlobalOptions(
		charts.WithInitializationOpts(darkInit("100%", "280px")),
		charts.WithTitleOpts(opts.Title{Title: title, TitleStyle: &opts.TextStyle{Color: textColor}}),
		charts.WithTooltipOpts(opts.Tooltip{Show: opts.Bool(true), Trigger: "axis"}),
		charts.WithDataZoomOpts(opts.DataZoom{Type: "inside", Start: 0, End: 100}),
		charts.WithDataZoomOpts(opts.DataZoom{Type: "slider", Start: 0, End: 100}),
		charts.WithXAxisOpts(opts.XAxis{
			Type:      "category",
			AxisLabel: &opts.AxisLabel{Color: textColor, Rotate: 30},
			SplitLine: &opts.SplitLine{Show: opts.Bool(false)},
			AxisLine:  &opts.AxisLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithYAxisOpts(opts.YAxis{
			Name:          "Drawdown (%)",

			AxisLabel:     &opts.AxisLabel{Color: textColor, Formatter: "{value}%"},
			SplitLine:     &opts.SplitLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithLegendOpts(opts.Legend{Show: opts.Bool(false)}),
	)

	xData := make([]string, len(snapshots))
	yData := make([]opts.LineData, len(snapshots))
	for i, s := range snapshots {
		xData[i] = s.CreatedAt.Format("2006-01-02 15:04")
		yData[i] = opts.LineData{Value: math.Round(dds[i]*100) / 100}
	}

	line.SetXAxis(xData).AddSeries("Drawdown", yData,
		charts.WithLineStyleOpts(opts.LineStyle{Color: colorRed, Width: 1}),
		charts.WithAreaStyleOpts(opts.AreaStyle{Color: "rgba(255,23,68,0.25)"}),
		charts.WithItemStyleOpts(opts.ItemStyle{Color: colorRed}),
	)
	return line
}

// DrawdownChartFromValues builds a drawdown chart from an arbitrary value slice.
func DrawdownChartFromValues(title string, values []BacktestPoint) *charts.Line {
	dds := computeDrawdownSeries(func(i int) float64 { return values[i].Value }, len(values))

	line := charts.NewLine()
	line.SetGlobalOptions(
		charts.WithInitializationOpts(darkInit("100%", "280px")),
		charts.WithTitleOpts(opts.Title{Title: title, TitleStyle: &opts.TextStyle{Color: textColor}}),
		charts.WithTooltipOpts(opts.Tooltip{Show: opts.Bool(true), Trigger: "axis"}),
		charts.WithDataZoomOpts(opts.DataZoom{Type: "inside", Start: 0, End: 100}),
		charts.WithDataZoomOpts(opts.DataZoom{Type: "slider", Start: 0, End: 100}),
		charts.WithXAxisOpts(opts.XAxis{
			Type:      "category",
			AxisLabel: &opts.AxisLabel{Color: textColor, Rotate: 30},
			SplitLine: &opts.SplitLine{Show: opts.Bool(false)},
			AxisLine:  &opts.AxisLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithYAxisOpts(opts.YAxis{
			Name:          "Drawdown (%)",

			AxisLabel:     &opts.AxisLabel{Color: textColor, Formatter: "{value}%"},
			SplitLine:     &opts.SplitLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithLegendOpts(opts.Legend{Show: opts.Bool(false)}),
	)

	xData := make([]string, len(values))
	yData := make([]opts.LineData, len(values))
	for i, v := range values {
		xData[i] = v.Date.Format("2006-01-02")
		yData[i] = opts.LineData{Value: math.Round(dds[i]*100) / 100}
	}

	line.SetXAxis(xData).AddSeries("Drawdown", yData,
		charts.WithLineStyleOpts(opts.LineStyle{Color: colorRed, Width: 1}),
		charts.WithAreaStyleOpts(opts.AreaStyle{Color: "rgba(255,23,68,0.25)"}),
		charts.WithItemStyleOpts(opts.ItemStyle{Color: colorRed}),
	)
	return line
}

// computeDrawdownSeries returns drawdown-from-peak percentages for a value series.
func computeDrawdownSeries(val func(i int) float64, n int) []float64 {
	if n == 0 {
		return nil
	}
	dds := make([]float64, n)
	peak := val(0)
	for i := 0; i < n; i++ {
		v := val(i)
		if v > peak {
			peak = v
		}
		if peak > 0 {
			dds[i] = (v - peak) / peak * 100
		}
	}
	return dds
}

// ── Monthly Returns Heatmap ───────────────────────────────────────────────────

// MonthlyHeatmap builds a heatmap of month × year → P&L %.
func MonthlyHeatmap(title string, summaries []store.DailySummary) *charts.HeatMap {
	type key struct{ year, month int }
	agg := make(map[key]float64)
	for _, s := range summaries {
		k := key{s.Date.Year(), int(s.Date.Month())}
		agg[k] += s.PnLPct
	}

	// Determine year range.
	yearMin, yearMax := 9999, 0
	for k := range agg {
		if k.year < yearMin {
			yearMin = k.year
		}
		if k.year > yearMax {
			yearMax = k.year
		}
	}
	if yearMin == 9999 {
		yearMin, yearMax = time.Now().Year(), time.Now().Year()
	}

	months := []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun",
		"Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

	var years []string
	for y := yearMin; y <= yearMax; y++ {
		years = append(years, fmt.Sprintf("%d", y))
	}

	var data []opts.HeatMapData
	for y := yearMin; y <= yearMax; y++ {
		for m := 1; m <= 12; m++ {
			v := agg[key{y, m}]
			data = append(data, opts.HeatMapData{
				Value: [3]interface{}{m - 1, y - yearMin, math.Round(v*100) / 100},
			})
		}
	}

	hm := charts.NewHeatMap()
	hm.SetGlobalOptions(
		charts.WithInitializationOpts(darkInit("100%", "300px")),
		charts.WithTitleOpts(opts.Title{Title: title, TitleStyle: &opts.TextStyle{Color: textColor}}),
		charts.WithTooltipOpts(opts.Tooltip{Show: opts.Bool(true)}),
		charts.WithXAxisOpts(opts.XAxis{
			Type: "category", Data: months,
			AxisLabel: &opts.AxisLabel{Color: textColor},
			SplitArea: &opts.SplitArea{Show: opts.Bool(true)},
		}),
		charts.WithYAxisOpts(opts.YAxis{
			Type: "category", Data: years,
			AxisLabel: &opts.AxisLabel{Color: textColor},
			SplitArea: &opts.SplitArea{Show: opts.Bool(true)},
		}),
		charts.WithVisualMapOpts(opts.VisualMap{
			Calculable: opts.Bool(true),
			Min:        -10, Max: 10,
			InRange: &opts.VisualMapInRange{Color: []string{colorRed, bgColor, colorGreen}},
			TextStyle: &opts.TextStyle{Color: textColor},
		}),
		charts.WithLegendOpts(opts.Legend{Show: opts.Bool(false)}),
	)
	hm.SetXAxis(months).AddSeries("Returns", data)
	return hm
}

// ── Trade Scatter ─────────────────────────────────────────────────────────────

// TradeScatter builds a scatter chart of trades: date × P&L %, coloured by win/loss.
func TradeScatter(title string, trades []store.Trade) *charts.Scatter {
	sc := charts.NewScatter()
	sc.SetGlobalOptions(
		charts.WithInitializationOpts(darkInit("100%", "300px")),
		charts.WithTitleOpts(opts.Title{Title: title, TitleStyle: &opts.TextStyle{Color: textColor}}),
		charts.WithTooltipOpts(opts.Tooltip{Show: opts.Bool(true)}),
		charts.WithXAxisOpts(opts.XAxis{
			Type:      "time",
			AxisLabel: &opts.AxisLabel{Color: textColor, Rotate: 30},
			SplitLine: &opts.SplitLine{Show: opts.Bool(false)},
			AxisLine:  &opts.AxisLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithYAxisOpts(opts.YAxis{
			Name:          "P&L (%)",

			AxisLabel:     &opts.AxisLabel{Color: textColor, Formatter: "{value}%"},
			SplitLine:     &opts.SplitLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithLegendOpts(opts.Legend{
			Show:      opts.Bool(true),
			TextStyle: &opts.TextStyle{Color: textColor},
		}),
	)

	var wins, losses []opts.ScatterData
	for _, t := range trades {
		pt := opts.ScatterData{
			Name:       t.Ticker,
			Value:      []interface{}{t.ClosedAt.Format(time.RFC3339), math.Round(t.PnLPct*100) / 100},
			SymbolSize: 8,
		}
		if t.PnL >= 0 {
			wins = append(wins, pt)
		} else {
			losses = append(losses, pt)
		}
	}

	sc.AddSeries("Win", wins,
		charts.WithItemStyleOpts(opts.ItemStyle{Color: colorGreen, Opacity: floatOpt(0.8)}),
	)
	sc.AddSeries("Loss", losses,
		charts.WithItemStyleOpts(opts.ItemStyle{Color: colorRed, Opacity: floatOpt(0.8)}),
	)
	return sc
}

// TradeScatterFromBacktest builds a scatter chart from backtest trades.
func TradeScatterFromBacktest(title string, trades []store.BacktestTrade) *charts.Scatter {
	sc := charts.NewScatter()
	sc.SetGlobalOptions(
		charts.WithInitializationOpts(darkInit("100%", "300px")),
		charts.WithTitleOpts(opts.Title{Title: title, TitleStyle: &opts.TextStyle{Color: textColor}}),
		charts.WithTooltipOpts(opts.Tooltip{Show: opts.Bool(true)}),
		charts.WithXAxisOpts(opts.XAxis{
			Type:      "time",
			AxisLabel: &opts.AxisLabel{Color: textColor, Rotate: 30},
			SplitLine: &opts.SplitLine{Show: opts.Bool(false)},
			AxisLine:  &opts.AxisLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithYAxisOpts(opts.YAxis{
			Name:          "P&L (%)",

			AxisLabel:     &opts.AxisLabel{Color: textColor, Formatter: "{value}%"},
			SplitLine:     &opts.SplitLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithLegendOpts(opts.Legend{
			Show:      opts.Bool(true),
			TextStyle: &opts.TextStyle{Color: textColor},
		}),
	)

	var wins, losses []opts.ScatterData
	for _, t := range trades {
		pnlPct := 0.0
		if t.PnLPct != nil {
			pnlPct = *t.PnLPct
		}
		exitTS := t.EntryTS
		if t.ExitTS != nil {
			exitTS = *t.ExitTS
		}
		pt := opts.ScatterData{
			Name:       t.Ticker,
			Value:      []interface{}{exitTS.Format(time.RFC3339), math.Round(pnlPct*100) / 100},
			SymbolSize: 8,
		}
		if pnlPct >= 0 {
			wins = append(wins, pt)
		} else {
			losses = append(losses, pt)
		}
	}

	sc.AddSeries("Win", wins,
		charts.WithItemStyleOpts(opts.ItemStyle{Color: colorGreen, Opacity: floatOpt(0.8)}),
	)
	sc.AddSeries("Loss", losses,
		charts.WithItemStyleOpts(opts.ItemStyle{Color: colorRed, Opacity: floatOpt(0.8)}),
	)
	return sc
}

// ── Metrics Summary Table ─────────────────────────────────────────────────────

// MetricRow is a key-value pair for the metrics summary table.
type MetricRow struct {
	Name  string
	Value string
}

// RenderMetricsTable returns an HTML table for the given metric rows.
func RenderMetricsTable(title string, rows []MetricRow) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, `<div class="metrics-block"><h3>%s</h3><table class="metrics-table">`, title)
	for _, r := range rows {
		fmt.Fprintf(&buf, `<tr><td class="metric-name">%s</td><td class="metric-value">%s</td></tr>`, r.Name, r.Value)
	}
	buf.WriteString("</table></div>")
	return buf.String()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// floatOpt converts a float32 to a types.Float pointer (for Opacity etc.).
func floatOpt(v float32) types.Float {
	return &v
}

// darkInit returns an Initialization option for a dark-themed chart.
func darkInit(width, height string) opts.Initialization {
	return opts.Initialization{
		BackgroundColor: bgColor,
		Width:           width,
		Height:          height,
	}
}

// BacktestPoint is a (date, value) pair used for equity curve construction.
type BacktestPoint struct {
	Date  time.Time
	Value float64
}

// BuildBacktestEquityCurve converts backtest trades into a portfolio value series.
// It starts from initialCash and adds P&L after each trade exit.
func BuildBacktestEquityCurve(trades []store.BacktestTrade, initialCash float64) []BacktestPoint {
	if len(trades) == 0 {
		return nil
	}
	pts := make([]BacktestPoint, 0, len(trades)+1)
	cash := initialCash
	for _, t := range trades {
		if t.PnL != nil {
			cash += *t.PnL
		}
		exitTS := t.EntryTS
		if t.ExitTS != nil {
			exitTS = *t.ExitTS
		}
		pts = append(pts, BacktestPoint{Date: exitTS, Value: cash})
	}
	return pts
}

// ── Overlay Line Chart (comparison) ──────────────────────────────────────────

// OverlayEquityCurve builds a single line chart with two equity-curve series
// overlaid for side-by-side backtest comparison.
func OverlayEquityCurve(labelA, labelB string, ptsA, ptsB []BacktestPoint) *charts.Line {
	line := charts.NewLine()
	line.SetGlobalOptions(
		charts.WithInitializationOpts(darkInit("100%", "360px")),
		charts.WithTitleOpts(opts.Title{
			Title:      "Equity Curves — " + labelA + " vs " + labelB,
			TitleStyle: &opts.TextStyle{Color: textColor},
		}),
		charts.WithTooltipOpts(opts.Tooltip{Show: opts.Bool(true), Trigger: "axis"}),
		charts.WithDataZoomOpts(opts.DataZoom{Type: "inside", Start: 0, End: 100}),
		charts.WithDataZoomOpts(opts.DataZoom{Type: "slider", Start: 0, End: 100}),
		charts.WithXAxisOpts(opts.XAxis{
			Type:      "category",
			AxisLabel: &opts.AxisLabel{Color: textColor, Rotate: 30},
			SplitLine: &opts.SplitLine{Show: opts.Bool(false)},
			AxisLine:  &opts.AxisLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithYAxisOpts(opts.YAxis{
			Name:          "Portfolio Value ($)",

			AxisLabel:     &opts.AxisLabel{Color: textColor, Formatter: "${value}"},
			SplitLine:     &opts.SplitLine{LineStyle: &opts.LineStyle{Color: gridColor}},
		}),
		charts.WithLegendOpts(opts.Legend{
			Show:      opts.Bool(true),
			TextStyle: &opts.TextStyle{Color: textColor},
		}),
	)

	if len(ptsA) > 0 {
		xData := make([]string, len(ptsA))
		yData := make([]opts.LineData, len(ptsA))
		for i, v := range ptsA {
			xData[i] = v.Date.Format("2006-01-02")
			yData[i] = opts.LineData{Value: math.Round(v.Value*100) / 100}
		}
		line.SetXAxis(xData).AddSeries(labelA, yData,
			charts.WithLineStyleOpts(opts.LineStyle{Color: colorCyan, Width: 2}),
			charts.WithItemStyleOpts(opts.ItemStyle{Color: colorCyan}),
		)
	}

	if len(ptsB) > 0 {
		xData := make([]string, len(ptsB))
		yData := make([]opts.LineData, len(ptsB))
		for i, v := range ptsB {
			xData[i] = v.Date.Format("2006-01-02")
			yData[i] = opts.LineData{Value: math.Round(v.Value*100) / 100}
		}
		// If A has no data, set the X axis here.
		if len(ptsA) == 0 {
			line.SetXAxis(xData)
		}
		line.AddSeries(labelB, yData,
			charts.WithLineStyleOpts(opts.LineStyle{Color: colorAmber, Width: 2}),
			charts.WithItemStyleOpts(opts.ItemStyle{Color: colorAmber}),
		)
	}

	return line
}

// ── Page helpers ──────────────────────────────────────────────────────────────

// AssemblePage creates a go-echarts Page from a variable number of charts and
// applies a consistent dark-background style via page configuration.
func AssemblePage(title string, chartItems ...components.Charter) *components.Page {
	page := components.NewPage()
	page.SetPageTitle(title)
	page.SetLayout(components.PageFlexLayout)
	page.AddCharts(chartItems...)
	return page
}

// RenderPage renders a page to a bytes.Buffer, ready to pass to Generator.Write.
func RenderPage(page *components.Page) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	if err := page.Render(&buf); err != nil {
		return nil, fmt.Errorf("render page: %w", err)
	}
	return &buf, nil
}
