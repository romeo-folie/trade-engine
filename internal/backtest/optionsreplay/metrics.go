package optionsreplay

import "sort"

// Report aggregates a set of replayed trades.
type Report struct {
	Total         int            // signals replayed
	Accepted      int            // passed the gates and sized ≥1 contract
	Wins          int            // accepted, priced, RESOLVED trades with PnL > 0
	Losses        int            // accepted, priced, resolved trades with PnL < 0
	Scratches     int            // accepted, priced, resolved trades with PnL == 0
	Unpriced      int            // resolved trades whose exit quote was missing (P&L unreliable — excluded from win/loss/PnL)
	Unresolved    int            // trades where the Path ran out before any modeled exit fired (path_exhausted) — no real outcome, excluded from win/loss/PnL
	Carried       int            // trades INCLUDED in P&L whose exit price was forward-filled (stale) because the option didn't trade at the exit minute
	TotalPnL      float64        // realized P&L across accepted, priced, RESOLVED trades (multiplier-correct)
	AbortsByGate  map[string]int // count of NOT-accepted signals per aborting gate
	ExitsByReason map[string]int // count of accepted trades per exit reason (stop/tp/time_exit/eod/path_exhausted)
	DTEByDay      map[int]int    // count of accepted trades per calendar-days-to-expiry of the chosen contract
}

// WinRate is wins / (wins + losses); 0 when there are no decided trades. Scratches
// (P&L exactly 0) are excluded from the denominator.
func (r Report) WinRate() float64 {
	decided := r.Wins + r.Losses
	if decided == 0 {
		return 0
	}
	return float64(r.Wins) / float64(decided)
}

// Aggregate summarizes replayed trades. Abort reasons are bucketed by their gate so
// coverage/rejection can be inspected; a silent drop would be visible as a gate
// count, never hidden.
func Aggregate(trades []SimTrade) Report {
	r := Report{
		AbortsByGate:  map[string]int{},
		ExitsByReason: map[string]int{},
		DTEByDay:      map[int]int{},
	}
	for _, t := range trades {
		r.Total++
		if !t.Accepted {
			gate := t.Gate
			if gate == "" {
				gate = "unknown"
			}
			r.AbortsByGate[gate]++
			continue
		}
		r.Accepted++
		r.DTEByDay[t.DTE]++             // DTE is fixed at entry, so every accepted position counts
		r.ExitsByReason[t.ExitReason]++ // the exit REASON is real even when the price wasn't
		if t.ExitReason == ReasonPathExhausted {
			// The Path ran out before any modeled exit fired. There is NO real trade
			// outcome here — closing at the last available bar is a data-coverage
			// artifact, not a decision. Never let its (arbitrary) last-bar P&L enter
			// win/loss/TotalPnL, even when that bar happened to carry a quote.
			r.Unresolved++
			continue
		}
		if !t.PriceKnown {
			// No option price ever observed (entry-side gap) → P&L is a fallback (0), not
			// a real result. Keep it out of win/loss/P&L. Surfaced via Unpriced.
			r.Unpriced++
			continue
		}
		if t.PriceCarried {
			// Exit price forward-filled from an earlier bar (stale) — still counted in
			// P&L (dropping it would bias comparisons), but flagged for transparency.
			r.Carried++
		}
		switch {
		case t.PnL > 0:
			r.Wins++
		case t.PnL < 0:
			r.Losses++
		default:
			r.Scratches++
		}
		r.TotalPnL += t.PnL
	}
	return r
}

// SortedGates returns the aborting-gate keys in deterministic order (for stable
// reporting/log output).
func (r Report) SortedGates() []string {
	keys := make([]string, 0, len(r.AbortsByGate))
	for k := range r.AbortsByGate {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
