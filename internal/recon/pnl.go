// Package recon reconciles the trades ledger against broker fill history.
//
// The ledger is per-trade and is written by the engine as it decides; the broker
// knows only fills. When the two disagree the broker is truth, and historically
// they have disagreed in ways nobody noticed — see the JMIA incident (#329),
// where 3,324 shares were acquired with no corresponding ledger row at all.
package recon

import (
	"fmt"
	"math"
	"sort"
	"strings"
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

// IsOptionSymbol reports whether a symbol is an OCC option contract rather than
// an equity ticker.
//
// This account trades both: options-ORB routes single-leg option orders through
// the same Alpaca account as the stock path. Their outcomes live in orb_trades,
// NOT in `trades`, so a reconciliation against `trades` must exclude them — and
// their P&L would be wrong anyway, because Alpaca reports option qty in
// contracts and price per share, understating by the 100x contract multiplier.
//
// OCC format is 21 characters: up to 6 of root padded with spaces, then
// YYMMDD, then C/P, then an 8-digit strike. Equity tickers are never 21 chars.
func IsOptionSymbol(symbol string) bool {
	if len(symbol) != 21 {
		return false
	}
	// positions 6..11 are the date, 12 is C or P, 13..20 the strike
	if c := symbol[12]; c != 'C' && c != 'P' {
		return false
	}
	for _, i := range []int{6, 7, 8, 9, 10, 11, 13, 14, 15, 16, 17, 18, 19, 20} {
		if symbol[i] < '0' || symbol[i] > '9' {
			return false
		}
	}
	return true
}

// PartitionFills splits fills into equity and option contracts. Callers
// reconciling against `trades` want only the equity half, and should report the
// option count rather than dropping it silently.
func PartitionFills(fills []Fill) (equity, options []Fill) {
	for _, f := range fills {
		if IsOptionSymbol(f.Symbol) {
			options = append(options, f)
		} else {
			equity = append(equity, f)
		}
	}
	return equity, options
}

// SymbolPnL is the broker-truth outcome for one symbol.
type SymbolPnL struct {
	Realized float64 // closed P&L, average-cost basis
	Position float64 // signed quantity still open; exactly 0 means flat
	Bought   float64 // gross shares bought
	Sold     float64 // gross shares sold (including short sales)

	// Shorted reports whether the position was ever actually negative — a
	// property of the position, not of the broker's side vocabulary.
	Shorted bool

	// SoldWithoutBuying reports that an ordinary "sell" (not "sell_short") drove
	// the position below zero: the account disposed of shares this fill history
	// never saw it acquire. That is the JMIA signature, and it is also what a
	// truncated fetch window looks like from the inside — either way the walk's
	// basis for this symbol is not trustworthy.
	SoldWithoutBuying bool
}

// Flat reports whether the position closed out entirely. Only flat symbols
// compare exactly against the ledger: an open position carries unrealized P&L
// that neither side books, and per-lot accounting can legitimately diverge from
// average cost while shares remain.
func (s SymbolPnL) Flat() bool { return s.Position == 0 }

// Trustworthy reports whether this symbol's walk rests on a complete history.
func (s SymbolPnL) Trustworthy() bool { return !s.SoldWithoutBuying }

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
//
// The caller must supply a COMPLETE fill history per symbol. A window that
// starts mid-position inverts the sign of everything that follows; symbols
// where that happened are flagged via SoldWithoutBuying.
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
			if f.IsBuy() {
				res.Bought += f.Qty
			} else {
				signed = -f.Qty
				res.Sold += f.Qty
			}

			// A plain "sell" that drives the position negative means shares left
			// the account that this history never saw arrive. Checked against
			// the position, before it moves, so an explicit short never trips it.
			if f.Side == "sell" && pos-f.Qty < -qtyEpsilon {
				res.SoldWithoutBuying = true
			}

			switch {
			case math.Abs(pos) < qtyEpsilon:
				pos, avg = signed, f.Price

			case (pos > 0) == (signed > 0):
				newPos := pos + signed
				avg = (avg*pos + f.Price*signed) / newPos
				pos = newPos

			default:
				closing := min(math.Abs(signed), math.Abs(pos))
				direction := 1.0 // long profits when price rises
				if pos < 0 {
					direction = -1.0 // short profits when price falls
				}
				realized += closing * (f.Price - avg) * direction

				remaining := math.Abs(signed) - closing
				pos += signed
				switch {
				case math.Abs(pos) < qtyEpsilon:
					pos, avg = 0, 0
				case remaining > qtyEpsilon:
					avg = f.Price // flipped through zero; new basis is this fill
				}
			}
			if pos < -qtyEpsilon {
				res.Shorted = true
			}
		}
		// Never leave dust behind: a residual position is what makes the next
		// caller divide by ~0, and it is why Flat() can test against exact zero.
		if math.Abs(pos) < qtyEpsilon {
			pos = 0
		}
		res.Realized, res.Position = realized, pos
		out[symbol] = res
	}
	return out
}

// Note renders the operator-facing flags for one symbol, most severe first.
// Every applicable flag appears: a symbol can be simultaneously untrustworthy,
// open, and formerly short, and hiding any of those is how a real anomaly gets
// read as routine.
func (s SymbolPnL) Note(ledgerTrades int) string {
	var notes []string
	if s.SoldWithoutBuying {
		notes = append(notes, "SOLD SHARES NEVER BOUGHT — history incomplete")
	}
	if ledgerTrades == 0 && (s.Realized != 0 || !s.Flat()) {
		notes = append(notes, "NO LEDGER ROWS — traded entirely off-ledger")
	}
	if !s.Flat() {
		notes = append(notes, fmt.Sprintf("OPEN %g — not comparable", s.Position))
	}
	if s.Shorted {
		notes = append(notes, "had short position")
	}
	return strings.Join(notes, "; ")
}
