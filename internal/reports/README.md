# internal/reports — HTML Performance Reports

Generates self-contained, offline-capable HTML reports with interactive charts powered by [go-echarts](https://github.com/go-echarts/go-echarts). All chart JS is inlined at generation time so reports work without an internet connection.

## Components

| File | Purpose |
|---|---|
| `generator.go` | `Generator` — output path management, JS asset inlining, browser open |
| `pages.go` | `BuildPerformancePage()`, `BuildBacktestPage()` — assemble report HTML |
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

### Backtest Report (`te report backtest`)
Covers a single backtest run:
- Simulated equity curve over the backtest window
- Per-trade PnL bars
- Signal timeline
- Full metrics: Sharpe, Sortino, max drawdown, win rate, profit factor, cache hit rate

## Usage

```bash
te report performance --from 2024-01-01 --to 2024-06-30
te report backtest --run BT-00042
te report backtest --run BT-00042 --open   # open in browser after generating
```

## Asset Inlining

`Generator` fetches CDN-hosted chart JavaScript on first run and caches it in memory. The inlined `<script>` tags replace CDN `<script src="...">` references, producing a fully self-contained HTML file. This ensures reports remain viewable after network access is lost or CDN URLs change.

## Output

Reports are written to the configured output directory (default: `~/.tradeengine/reports/`):

```
~/.tradeengine/reports/
  performance_2024-01-01_2024-06-30.html
  backtest_BT-00042.html
```

## Configuration

```yaml
reports:
  output_dir: ~/.tradeengine/reports
  open_on_generate: false   # auto-open in browser after generation
```
