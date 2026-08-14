# internal/reports — HTML Performance Reports

Generates self-contained HTML reports with interactive charts powered by [go-echarts](https://github.com/go-echarts/go-echarts). Chart JavaScript is fetched from CDN and inlined into the HTML file at generation time so reports are viewable offline.

> **Asset caching:** The `Generator` instance caches fetched JS in memory. Since the CLI creates a new `Generator` per command invocation, scripts are re-fetched on each run.

## Components

| File | Purpose |
|---|---|
| `generator.go` | `Generator` — output path management, JS asset inlining, optional browser open |
| `pages.go` | Page builders: `BuildPerformancePage`, `BuildBacktestPage`, `BuildDailyPage`, `BuildWeeklyPage`, `BuildBacktestComparePage` |
| `charts.go` | Chart builders: equity curve, drawdown, trade PnL, signal timeline |

## Report Types

### Performance Report (`te report performance`)
Covers live/paper trading over a date range:
- Portfolio equity curve (daily snapshots)
- Drawdown chart
- Realised PnL by trade (bar chart)
- Daily P&L summary
- Win rate, Sharpe ratio, profit factor, expectancy (metrics table)
- Strategy and ticker breakdowns

### Backtest Report (`te report backtest <label>`)
Covers a single backtest run:
- Simulated equity curve over the backtest window
- Per-trade PnL bars
- Signal timeline
- Full metrics: Sharpe, Sortino, max drawdown, win rate, profit factor, cache hit rate

### Other Reports
```bash
te report daily [--date YYYY-MM-DD]           # single trading day summary
te report weekly [--from YYYY-MM-DD]          # 7-day rolling summary
te report compare <label1> <label2>           # side-by-side backtest comparison
te report scanner-funnel                      # scanner conversion and outcomes
te report weekly-leaders                      # leadership research observations
te report potent-industries                   # industry potency and breadth
te report premarket-episodic                   # premarket gap/catalyst observations
```

Scanner research commands share the report command tree but may render their
own tabular/CSV-style outputs rather than the generic go-echarts page builders.

## Usage

```bash
te report performance --from 2024-01-01 --to 2024-06-30
te report backtest BT-00042
```

## Output

Reports are written to the configured output directory (default: `~/.tradeengine/reports/`) with timestamped filenames:

```
~/.tradeengine/reports/
  performance-<timestamp>.html
  backtest-BT-00042-<timestamp>.html
```

## Configuration

```yaml
reports:
  output_dir: ~/.tradeengine/reports
  auto_open_browser: false    # open the report in the default browser after generation
```
