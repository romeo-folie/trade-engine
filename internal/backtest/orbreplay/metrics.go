package orbreplay

import (
	"math"
	"sort"
	"time"
)

// Metrics summarises a set of simulated trades. Money figures are NET of commission;
// R figures are gross (planned-risk multiples), matching how the strategy doc reports.
type Metrics struct {
	Trades        int
	Wins          int
	Losses        int
	Scratches     int
	WinRate       float64       // wins / decided
	NetPnL        float64       // sum of net PnL
	GrossPnL      float64       // sum of gross PnL
	Commission    float64       // sum of commission
	ProfitFactor  float64       // NET profit factor: Σ net wins / Σ |net losses| (after commission); +Inf when no losses
	ExpectancyR   float64       // mean gross R per trade
	AvgWinnerR    float64       // mean gross R of net-positive trades
	AvgLoserR     float64       // mean gross R of net-negative trades
	MaxLossR      float64       // most negative gross R
	AvgHold       time.Duration // mean time in trade
	OvernightHold int           // trades whose entry and exit fall on different UTC days
	ByExitType    map[string]int
}

// Result bundles a variant run's trades with computed metrics, overall and per ticker.
type Result struct {
	Variant      string
	Overall      Metrics
	ByInstrument map[string]Metrics
	Trades       []SimTrade
}

// Summarize computes a Result from a flat list of simulated trades.
func Summarize(variant string, trades []SimTrade) Result {
	byInst := make(map[string][]SimTrade)
	for _, t := range trades {
		byInst[t.Ticker] = append(byInst[t.Ticker], t)
	}
	perInst := make(map[string]Metrics, len(byInst))
	for sym, ts := range byInst {
		perInst[sym] = computeMetrics(ts)
	}
	return Result{
		Variant:      variant,
		Overall:      computeMetrics(trades),
		ByInstrument: perInst,
		Trades:       trades,
	}
}

// computeMetrics aggregates one slice of trades.
func computeMetrics(trades []SimTrade) Metrics {
	m := Metrics{ByExitType: make(map[string]int)}
	m.Trades = len(trades)
	if m.Trades == 0 {
		return m
	}

	var netWins, netLosses float64 // net PnL magnitudes for the (net) profit factor
	var sumR, sumHold float64
	var winnerR, loserR []float64
	for _, t := range trades {
		m.NetPnL += t.NetPnL
		m.GrossPnL += t.GrossPnL
		m.Commission += t.Commission
		m.ByExitType[t.ExitType]++
		sumR += t.R
		sumHold += float64(t.HoldDur)

		switch {
		case t.NetPnL > 0:
			m.Wins++
			netWins += t.NetPnL
			winnerR = append(winnerR, t.R)
		case t.NetPnL < 0:
			m.Losses++
			netLosses += -t.NetPnL
			loserR = append(loserR, t.R)
		default:
			m.Scratches++
		}
		if t.R < m.MaxLossR {
			m.MaxLossR = t.R
		}
		if !sameUTCDay(t.EntryTime, t.ExitTime) {
			m.OvernightHold++
		}
	}

	if decided := m.Wins + m.Losses; decided > 0 {
		m.WinRate = float64(m.Wins) / float64(decided)
	}
	switch {
	case netLosses > 0:
		m.ProfitFactor = netWins / netLosses
	case netWins > 0:
		m.ProfitFactor = math.Inf(1) // all winners: undefined ratio → +Inf sentinel
	}
	m.ExpectancyR = sumR / float64(m.Trades)
	m.AvgWinnerR = mean(winnerR)
	m.AvgLoserR = mean(loserR)
	m.AvgHold = time.Duration(sumHold / float64(m.Trades))
	return m
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func sameUTCDay(a, b time.Time) bool {
	a, b = a.UTC(), b.UTC()
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// SortedInstruments returns the per-instrument symbols in stable alphabetical order,
// so CLI output is deterministic.
func (r Result) SortedInstruments() []string {
	out := make([]string, 0, len(r.ByInstrument))
	for sym := range r.ByInstrument {
		out = append(out, sym)
	}
	sort.Strings(out)
	return out
}
