package study

import (
	"fmt"
	"sort"
	"strings"
)

// Report renders the aggregated study as a human-readable text block.
func Report(results []TradeResult) string {
	var sb strings.Builder
	usable := make([]TradeResult, 0, len(results))
	skipped := map[string]int{}
	for _, r := range results {
		if r.Skipped != "" {
			skipped[r.Skipped]++
			continue
		}
		usable = append(usable, r)
	}

	fmt.Fprintf(&sb, "═══ Stock-path execution study ═══\n")
	fmt.Fprintf(&sb, "trades: %d usable / %d total\n", len(usable), len(results))
	for reason, n := range skipped {
		fmt.Fprintf(&sb, "  skipped %d: %s\n", n, reason)
	}
	if len(usable) == 0 {
		return sb.String()
	}

	reportChase(&sb, usable)
	reportExitFidelity(&sb, usable)
	reportTactics(&sb, usable)
	reportExcursions(&sb, usable)
	reportTrailing(&sb, usable)
	return sb.String()
}

// reportChase — what live market orders would have paid vs signal_price.
func reportChase(sb *strings.Builder, results []TradeResult) {
	byStrategy := map[string][]float64{}
	for _, r := range results {
		if !r.Case.HasSignal || r.Case.SignalPrice <= 0 {
			continue
		}
		byStrategy[r.Case.Strategy] = append(byStrategy[r.Case.Strategy], r.ChasePct)
	}
	if len(byStrategy) == 0 {
		return
	}
	fmt.Fprintf(sb, "\n── 1. Entry chasing cost (live market order vs signal_price; + = paid worse) ──\n")
	for _, s := range sortedKeys(byStrategy) {
		v := byStrategy[s]
		fmt.Fprintf(sb, "  %-16s n=%-3d avg %+.2f%%  median %+.2f%%  worst %+.2f%%\n",
			s, len(v), mean(v), median(v), maxOf(v))
	}
	all := flatten(byStrategy)
	fmt.Fprintf(sb, "  %-16s n=%-3d avg %+.2f%%  median %+.2f%%\n", "ALL", len(all), mean(all), median(all))
}

// reportExitFidelity — broker-bracket sim vs the recorded (hourly-close) exit.
func reportExitFidelity(sb *strings.Builder, results []TradeResult) {
	var slips, lags []float64
	gap, intraday := 0, 0
	reasonMismatch := 0
	for _, r := range results {
		if r.SimExitReason == "" || r.SimExitReason == "unresolved" {
			continue
		}
		slips = append(slips, r.ExitSlipPct)
		if !r.SimExitAt.IsZero() {
			lags = append(lags, r.ExitLagMinutes)
		}
		switch r.BreachClass {
		case BreachGap:
			gap++
		case BreachIntraday:
			intraday++
		}
		if r.SimExitReason == "target" && r.Case.PnLPct < 0 {
			reasonMismatch++
		}
	}
	if len(slips) == 0 {
		return
	}
	fmt.Fprintf(sb, "\n── 2. Exit fidelity (intrabar bracket sim vs recorded exit; + = bracket better) ──\n")
	fmt.Fprintf(sb, "  resolved trades: %d\n", len(slips))
	fmt.Fprintf(sb, "  exit-price improvement: avg %+.2f%%  median %+.2f%%\n", mean(slips), median(slips))
	fmt.Fprintf(sb, "  detection lag (sim trigger → recorded close): avg %.0f min  median %.0f min  max %.0f min\n",
		mean(lags), median(lags), maxOf(lags))
	fmt.Fprintf(sb, "  stop breaches: %d intraday (fixable: broker-resident stops) / %d gap (sizing/flatten only)\n", intraday, gap)
	if reasonMismatch > 0 {
		fmt.Fprintf(sb, "  %d trades hit their TARGET intrabar but were recorded as losses\n", reasonMismatch)
	}
}

// reportTactics — fill-rate-adjusted expectancy per entry tactic.
func reportTactics(sb *strings.Builder, results []TradeResult) {
	type agg struct {
		signals, fills, wins int
		sumFilled, sumAll    float64
		unresolved           int
	}
	byName := map[string]*agg{}
	for _, r := range results {
		for _, t := range r.Tactics {
			a := byName[t.Name]
			if a == nil {
				a = &agg{}
				byName[t.Name] = a
			}
			a.signals++
			if !t.Filled {
				continue
			}
			a.fills++
			a.sumFilled += t.ReturnPct
			a.sumAll += t.ReturnPct
			if t.ReturnPct > 0 {
				a.wins++
			}
			if t.ExitReason == "unresolved" {
				a.unresolved++
			}
		}
	}
	if len(byName) == 0 {
		return
	}
	fmt.Fprintf(sb, "\n── 3. Entry tactic sweep (same bracket, fill-rate-adjusted; unfilled = 0%%) ──\n")
	fmt.Fprintf(sb, "  %-16s %7s %6s %8s %14s %14s %6s\n",
		"tactic", "signals", "fills", "win%", "avg/filled", "expectancy", "unres")
	for _, name := range sortedKeys(byName) {
		a := byName[name]
		winPct, avgFilled := 0.0, 0.0
		if a.fills > 0 {
			winPct = float64(a.wins) / float64(a.fills) * 100
			avgFilled = a.sumFilled / float64(a.fills)
		}
		fmt.Fprintf(sb, "  %-16s %7d %6d %7.0f%% %+13.2f%% %+13.2f%% %6d\n",
			name, a.signals, a.fills, winPct, avgFilled, a.sumAll/float64(a.signals), a.unresolved)
	}
	fmt.Fprintf(sb, "  (expectancy = mean over ALL signals; the honest cross-tactic comparison)\n")
}

// reportExcursions — MAE/MFE over the actual hold, split by outcome.
func reportExcursions(sb *strings.Builder, results []TradeResult) {
	var winMAE, winMFE, lossMAE, lossMFE []float64
	for _, r := range results {
		if r.Case.PnLPct > 0 {
			winMAE = append(winMAE, r.MAEPct)
			winMFE = append(winMFE, r.MFEPct)
		} else {
			lossMAE = append(lossMAE, r.MAEPct)
			lossMFE = append(lossMFE, r.MFEPct)
		}
	}
	fmt.Fprintf(sb, "\n── 4. MAE/MFE over actual holds (adverse before it worked?) ──\n")
	if len(winMAE) > 0 {
		fmt.Fprintf(sb, "  winners (n=%d): avg MAE %.2f%%  median MAE %.2f%%  avg MFE %.2f%%\n",
			len(winMAE), mean(winMAE), median(winMAE), mean(winMFE))
	}
	if len(lossMAE) > 0 {
		fmt.Fprintf(sb, "  losers  (n=%d): avg MAE %.2f%%  median MAE %.2f%%  avg MFE %.2f%%\n",
			len(lossMAE), mean(lossMAE), median(lossMAE), mean(lossMFE))
	}
	fmt.Fprintf(sb, "  (winners with deep MAE = patient entries were on offer; losers with real MFE = trailing/target had room)\n")
}

// reportTrailing — 1.5×ATR ratchet vs what actually happened.
func reportTrailing(sb *strings.Builder, results []TradeResult) {
	var deltas []float64
	better := 0
	for _, r := range results {
		if !r.TrailComputed {
			continue
		}
		d := r.TrailReturnPct - r.ActualReturnPct
		deltas = append(deltas, d)
		if d > 0 {
			better++
		}
	}
	if len(deltas) == 0 {
		return
	}
	fmt.Fprintf(sb, "\n── 5. Trailing-stop counterfactual (ratchet ATR trail vs actual; + = trail better) ──\n")
	fmt.Fprintf(sb, "  n=%d  avg delta %+.2f%%  median %+.2f%%  trail better on %d/%d trades\n",
		len(deltas), mean(deltas), median(deltas), better, len(deltas))
}

// CSV renders one row per trade for offline slicing.
func CSV(results []TradeResult) string {
	var sb strings.Builder
	sb.WriteString("trade_id,ticker,strategy,direction,opened_at,closed_at,entry,exit,stop,target,actual_pct,chase_pct,sim_exit,sim_exit_reason,exit_slip_pct,exit_lag_min,breach_class,mae_pct,mfe_pct,trail_pct,skipped\n")
	for _, r := range results {
		c := r.Case
		fmt.Fprintf(&sb, "%s,%s,%s,%s,%s,%s,%.4f,%.4f,%.4f,%.4f,%.4f,%.4f,%.4f,%s,%.4f,%.1f,%s,%.4f,%.4f,%.4f,%q\n",
			c.ID, c.Ticker, c.Strategy, c.Direction,
			c.OpenedAt.Format("2006-01-02T15:04:05Z07:00"), c.ClosedAt.Format("2006-01-02T15:04:05Z07:00"),
			c.EntryPrice, c.ExitPrice, c.StopLoss, c.TakeProfit, c.PnLPct,
			r.ChasePct, r.SimExitPrice, r.SimExitReason, r.ExitSlipPct, r.ExitLagMinutes,
			r.BreachClass, r.MAEPct, r.MFEPct, r.TrailReturnPct, r.Skipped)
	}
	return sb.String()
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

func maxOf(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	m := v[0]
	for _, x := range v[1:] {
		m = maxF(m, x)
	}
	return m
}

func flatten(m map[string][]float64) []float64 {
	var out []float64
	for _, v := range m {
		out = append(out, v...)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
