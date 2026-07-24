package backtest

import (
	"math"
	"time"
)

// Metrics holds all performance statistics for a completed backtest run.
// All percentage fields are expressed as percentages (e.g. 12.5 = 12.5%).
type Metrics struct {
	TotalReturnPct      float64
	AnnualizedReturnPct float64
	SharpeRatio         float64
	SortinoRatio        float64
	CalmarRatio         float64
	MaxDrawdownPct      float64
	WinRatePct          float64
	ProfitFactor        float64
	Expectancy          float64 // average $ P&L per trade
	TotalTrades         int
	AvgHoldHours        float64
	BestTradePct        float64
	WorstTradePct       float64

	// Asymmetry / risk-relative stats (exit-policy-study). These are the metrics
	// that reveal a low-win-rate / high-payoff edge, which raw win rate hides.
	AvgWinPct      float64        // mean PnL% of winning trades
	AvgLossPct     float64        // mean PnL% of losing trades (negative)
	PayoffRatio    float64        // |AvgWinPct| / |AvgLossPct| — the asymmetry ratio
	AvgRMultiple   float64        // mean trade P&L in units of entry risk (1R)
	MaxRMultiple   float64        // largest single-trade R
	RunnersOver10R int            // trades that reached ≥10R (the fat right tail)
	ExitReasons    map[string]int // exit-reason breakdown (stop_loss/take_profit/trail_*/signal/…)
}

// Compute calculates all metrics from a completed trade list.
//
//   - trades:      all closed simulated trades (open/unclosed trades are ignored)
//   - startCash:   initial portfolio value
//   - finalCash:   portfolio value at end of backtest (cash + unrealised P&L)
//   - from / to:   backtest date range (used for annualisation)
//   - rfRate:      annual risk-free rate (e.g. 0.04 for 4%)
func Compute(trades []SimulatedTrade, startCash, finalCash float64, from, to time.Time, rfRate float64) Metrics {
	closed := closedTrades(trades)
	m := Metrics{TotalTrades: len(closed)}

	if len(closed) == 0 {
		return m
	}

	// ── Total and annualised return ───────────────────────────────────────────
	m.TotalReturnPct = (finalCash/startCash - 1) * 100
	years := to.Sub(from).Hours() / 8760
	if years > 0 {
		m.AnnualizedReturnPct = (math.Pow(finalCash/startCash, 1/years) - 1) * 100
	}

	// ── Per-trade stats ───────────────────────────────────────────────────────
	var wins, losses int
	var totalWin, totalLoss float64
	var totalHoldHours float64
	m.BestTradePct = math.Inf(-1)
	m.WorstTradePct = math.Inf(1)
	var totalExpectancy float64

	var winPctSum, lossPctSum, rSum float64
	var rCount, over10R int
	maxR := math.Inf(-1)
	exitReasons := map[string]int{}

	for _, t := range closed {
		pnlPct := t.PnLPct
		if pnlPct > m.BestTradePct {
			m.BestTradePct = pnlPct
		}
		if pnlPct < m.WorstTradePct {
			m.WorstTradePct = pnlPct
		}
		totalExpectancy += t.PnL
		totalHoldHours += t.ExitTS.Sub(t.EntryTS).Hours()
		if t.PnL >= 0 {
			wins++
			totalWin += t.PnL
			winPctSum += pnlPct
		} else {
			losses++
			totalLoss += math.Abs(t.PnL)
			lossPctSum += pnlPct
		}
		if t.ExitReason != "" {
			exitReasons[t.ExitReason]++
		}
		// R-multiples only meaningful when an entry risk was recorded.
		if t.RiskPerShare > 0 {
			rSum += t.RMultiple
			rCount++
			if t.RMultiple > maxR {
				maxR = t.RMultiple
			}
			if t.RMultiple >= 10 {
				over10R++
			}
		}
	}

	if wins > 0 {
		m.AvgWinPct = winPctSum / float64(wins)
	}
	if losses > 0 {
		m.AvgLossPct = lossPctSum / float64(losses)
	}
	if m.AvgLossPct != 0 {
		m.PayoffRatio = math.Abs(m.AvgWinPct) / math.Abs(m.AvgLossPct)
	} else if m.AvgWinPct != 0 {
		m.PayoffRatio = math.Inf(1) // wins but no losses (mirrors ProfitFactor)
	}
	if rCount > 0 {
		m.AvgRMultiple = rSum / float64(rCount)
	}
	if !math.IsInf(maxR, -1) {
		m.MaxRMultiple = maxR
	}
	m.RunnersOver10R = over10R
	m.ExitReasons = exitReasons

	if math.IsInf(m.BestTradePct, -1) {
		m.BestTradePct = 0
	}
	if math.IsInf(m.WorstTradePct, 1) {
		m.WorstTradePct = 0
	}

	m.WinRatePct = float64(wins) / float64(len(closed)) * 100
	m.AvgHoldHours = totalHoldHours / float64(len(closed))
	m.Expectancy = totalExpectancy / float64(len(closed))
	if totalLoss > 0 {
		m.ProfitFactor = totalWin / totalLoss
	} else if totalWin > 0 {
		m.ProfitFactor = math.Inf(1) // all wins, no losses
	}

	// ── Max drawdown ──────────────────────────────────────────────────────────
	m.MaxDrawdownPct = maxDrawdown(closed, startCash)

	// ── Daily return series for Sharpe / Sortino ──────────────────────────────
	daily := dailyReturns(closed, startCash, from, to)
	if len(daily) > 1 {
		dailyRF := rfRate / 252
		m.SharpeRatio = sharpe(daily, dailyRF)
		m.SortinoRatio = sortino(daily, dailyRF)
	}

	// ── Calmar ────────────────────────────────────────────────────────────────
	if m.MaxDrawdownPct > 0 {
		m.CalmarRatio = m.AnnualizedReturnPct / m.MaxDrawdownPct
	}

	return m
}

// ── Internal helpers ──────────────────────────────────────────────────────────

func closedTrades(trades []SimulatedTrade) []SimulatedTrade {
	var out []SimulatedTrade
	for _, t := range trades {
		if !t.ExitTS.IsZero() {
			out = append(out, t)
		}
	}
	return out
}

// maxDrawdown computes the maximum peak-to-trough drawdown as a positive %.
// It reconstructs a running equity curve from the ordered closed trades.
func maxDrawdown(trades []SimulatedTrade, startCash float64) float64 {
	equity := startCash
	peak := equity
	maxDD := 0.0

	for _, t := range trades {
		equity += t.PnL
		if equity > peak {
			peak = equity
		}
		if peak > 0 {
			dd := (peak - equity) / peak * 100
			if dd > maxDD {
				maxDD = dd
			}
		}
	}
	return maxDD
}

// dailyReturns builds a slice of trading-day portfolio returns (as fractions, not %).
// Only Monday–Friday are included; weekends are skipped so the resulting slice
// has ~252 observations per year — matching the √252 annualisation factor used
// in Sharpe and Sortino. Including weekends (~365/yr) would systematically
// dilute mean daily return by ~31% and distort the volatility denominator.
func dailyReturns(trades []SimulatedTrade, startCash float64, from, to time.Time) []float64 {
	// Build a map of date → cumulative P&L on that day.
	pnlByDay := make(map[string]float64)
	for _, t := range trades {
		day := t.ExitTS.Format("2006-01-02")
		pnlByDay[day] += t.PnL
	}

	var returns []float64
	equity := startCash
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		// Skip weekends — stock markets are closed; including them would add
		// zero-return days that dilute Sharpe/Sortino vs the √252 convention.
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		key := d.Format("2006-01-02")
		if equity == 0 {
			returns = append(returns, 0)
			continue
		}
		dailyPnL := pnlByDay[key]
		returns = append(returns, dailyPnL/equity)
		equity += dailyPnL
	}
	return returns
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func stddev(xs []float64, mu float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	sum := 0.0
	for _, x := range xs {
		d := x - mu
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(xs)-1))
}

// sharpe computes the annualised Sharpe ratio from daily return series.
func sharpe(daily []float64, dailyRF float64) float64 {
	excess := make([]float64, len(daily))
	for i, r := range daily {
		excess[i] = r - dailyRF
	}
	mu := mean(excess)
	sd := stddev(excess, mu)
	if sd == 0 {
		return 0
	}
	return mu / sd * math.Sqrt(252)
}

// sortino uses only downside deviation in the denominator.
func sortino(daily []float64, dailyRF float64) float64 {
	mu := mean(daily) - dailyRF
	var downside []float64
	for _, r := range daily {
		if r < dailyRF {
			d := r - dailyRF
			downside = append(downside, d*d)
		}
	}
	if len(downside) == 0 {
		return 0
	}
	sumSq := 0.0
	for _, d := range downside {
		sumSq += d
	}
	dd := math.Sqrt(sumSq / float64(len(daily)))
	if dd == 0 {
		return 0
	}
	return mu / dd * math.Sqrt(252)
}
