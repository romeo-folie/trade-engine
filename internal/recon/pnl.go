// Package recon reconciles the trades ledger against broker fill history.
//
// The ledger is per-trade and is written by the engine as it decides; the broker
// knows only fills. When the two disagree the broker is truth, and historically
// they have disagreed in ways nobody noticed — see the JMIA incident (#329),
// where 3,324 shares were acquired with no corresponding ledger row at all.
package recon

import (
	"sort"
	"time"
)

// qtyEpsilon is the dust threshold below which a position counts as flat.
// Fractional-share fills carry six decimals, so this is three orders of
// magnitude below any real quantity — while still large enough to absorb the
// float error that accumulates over hundreds of partial fills. Dividing a
// residual cost by a residual position is how a naive walk invents six-figure
// P&L out of rounding noise.
const qtyEpsilon = 1e-6

// Fill is one broker execution. Side is the broker's own verb: "buy", "sell",
// or "sell_short" — the distinction matters, see RealizedBySymbol.
type Fill struct {
	ID     string
	Symbol string
	Side   string
	Qty    float64
	Price  float64
	Time   time.Time
}

// IsBuy reports whether the fill increases the signed position. Every non-buy
// side ("sell", "sell_short") decreases it.
func (f Fill) IsBuy() bool { return f.Side == "buy" }

// SymbolPnL is the broker-truth outcome for one symbol.
type SymbolPnL struct {
	Realized float64 // closed P&L, average-cost basis
	Position float64 // signed quantity still open; ~0 means flat
	Bought   float64 // gross shares bought
	Sold     float64 // gross shares sold (including short sales)
	Shorted  bool    // the symbol was short at some point
}

// Flat reports whether the position closed out entirely. Only flat symbols
// compare exactly against the ledger: an open position carries unrealized P&L
// that neither side books, and per-lot accounting can legitimately diverge from
// average cost while shares remain.
func (s SymbolPnL) Flat() bool { return s.Position > -qtyEpsilon && s.Position < qtyEpsilon }

// RealizedBySymbol walks each symbol's fills chronologically and returns
// average-cost realized P&L.
//
// Signed positions are handled throughout, because the account has been short:
// the July 2026 short-side experiment opened and covered shorts on eight
// symbols before being parked. A walk that treats "sell_short" as an ordinary
// sale reports those as "sold shares that were never bought" — a data-integrity
// alarm where there is only a documented experiment. It also mis-signs their
// P&L: a short profits when price falls.
//
// Three cases per fill, and the third is the one that is easy to get wrong:
//
//  1. flat, or same direction as the position → open/add, re-average the basis
//  2. opposite direction, not exceeding the position → close some, realize
//  3. opposite direction, exceeding the position → close all, realize, then
//     re-open on the other side at this fill's price (a flip through zero)
func RealizedBySymbol(fills []Fill) map[string]SymbolPnL {
	bySymbol := map[string][]Fill{}
	for _, f := range fills {
		bySymbol[f.Symbol] = append(bySymbol[f.Symbol], f)
	}

	out := make(map[string]SymbolPnL, len(bySymbol))
	for symbol, symFills := range bySymbol {
		// Stable order: fills inside one second share a timestamp, so the ID
		// breaks ties deterministically rather than leaving order to the map.
		sort.SliceStable(symFills, func(i, j int) bool {
			if symFills[i].Time.Equal(symFills[j].Time) {
				return symFills[i].ID < symFills[j].ID
			}
			return symFills[i].Time.Before(symFills[j].Time)
		})

		var pos, avg, realized float64
		var res SymbolPnL
		for _, f := range symFills {
			signed := f.Qty
			if !f.IsBuy() {
				signed = -f.Qty
				res.Sold += f.Qty
			} else {
				res.Bought += f.Qty
			}
			if f.Side != "buy" && f.Side != "sell" {
				res.Shorted = true
			}

			switch {
			case pos > -qtyEpsilon && pos < qtyEpsilon:
				pos, avg = signed, f.Price

			case (pos > 0) == (signed > 0):
				newPos := pos + signed
				avg = (avg*pos + f.Price*signed) / newPos
				pos = newPos

			default:
				closing := min(abs(signed), abs(pos))
				direction := 1.0 // long profits when price rises
				if pos < 0 {
					direction = -1.0 // short profits when price falls
				}
				realized += closing * (f.Price - avg) * direction

				remaining := abs(signed) - closing
				pos += signed
				switch {
				case pos > -qtyEpsilon && pos < qtyEpsilon:
					pos, avg = 0, 0
				case remaining > qtyEpsilon:
					avg = f.Price // flipped through zero; new basis is this fill
				}
			}
		}
		// Never leave dust behind: a residual position is what makes the next
		// caller divide by ~0.
		if pos > -qtyEpsilon && pos < qtyEpsilon {
			pos = 0
		}
		res.Realized, res.Position = realized, pos
		out[symbol] = res
	}
	return out
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
