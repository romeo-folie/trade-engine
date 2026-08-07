package recon

import (
	"math"
	"testing"
	"time"
)

// fill builds a Fill at minute-offset m so ordering is explicit in each test.
func fill(sym, side string, qty, price float64, m int) Fill {
	return Fill{
		ID:     time.Duration(m).String() + sym + side,
		Symbol: sym,
		Side:   side,
		Qty:    qty,
		Price:  price,
		Time:   time.Date(2026, 7, 1, 14, m, 0, 0, time.UTC),
	}
}

func near(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 0.005 {
		t.Errorf("%s = %.4f, want %.4f", what, got, want)
	}
}

func TestRealizedLongRoundTrip(t *testing.T) {
	got := RealizedBySymbol([]Fill{
		fill("AAA", "buy", 100, 10, 1),
		fill("AAA", "sell", 100, 12, 2),
	})["AAA"]
	near(t, got.Realized, 200, "realized")
	if !got.Flat() {
		t.Errorf("position = %v, want flat", got.Position)
	}
	if got.Shorted {
		t.Error("Shorted = true, want false for a long-only round trip")
	}
}

// A short profits when the price FALLS. Getting this sign wrong turns the
// July 2026 short experiment's losses into gains and vice versa.
func TestRealizedShortRoundTripProfitsWhenPriceFalls(t *testing.T) {
	got := RealizedBySymbol([]Fill{
		fill("BBB", "sell_short", 50, 40, 1),
		fill("BBB", "buy", 50, 36, 2), // covered $4 lower
	})["BBB"]
	near(t, got.Realized, 200, "short realized")
	if !got.Flat() {
		t.Errorf("position = %v, want flat", got.Position)
	}
	if !got.Shorted {
		t.Error("Shorted = false, want true — sell_short must be recorded")
	}
}

func TestRealizedShortLosesWhenPriceRises(t *testing.T) {
	got := RealizedBySymbol([]Fill{
		fill("CCC", "sell_short", 10, 100, 1),
		fill("CCC", "buy", 10, 110, 2),
	})["CCC"]
	near(t, got.Realized, -100, "short realized")
}

// Averaging in on the way down, then closing in pieces.
func TestRealizedAveragesBasisAcrossPartialFills(t *testing.T) {
	got := RealizedBySymbol([]Fill{
		fill("DDD", "buy", 100, 10, 1),
		fill("DDD", "buy", 100, 20, 2),  // avg basis now 15
		fill("DDD", "sell", 50, 25, 3),  // +10 × 50 = 500
		fill("DDD", "sell", 150, 15, 4), // +0 × 150
	})["DDD"]
	near(t, got.Realized, 500, "realized")
	if !got.Flat() {
		t.Errorf("position = %v, want flat", got.Position)
	}
}

// The case a naive walk gets wrong: a sale larger than the position closes the
// long AND opens a short, and the short's basis is this fill's price — not the
// old long basis.
func TestRealizedFlipThroughZero(t *testing.T) {
	got := RealizedBySymbol([]Fill{
		fill("EEE", "buy", 100, 10, 1),
		fill("EEE", "sell_short", 150, 12, 2), // close 100 (+200), short 50 @ 12
		fill("EEE", "buy", 50, 11, 3),         // cover 50 @ 11 → +50
	})["EEE"]
	near(t, got.Realized, 250, "realized across the flip")
	if !got.Flat() {
		t.Errorf("position = %v, want flat", got.Position)
	}
}

// Regression guard for the defect that made a first pass of this reconciliation
// report +$322,037 of realized P&L on a symbol that had traded ~$65 stock: once
// a position closes, float residue leaves pos at ~1e-16, and the NEXT sale
// divides a residual cost by that dust to get an astronomical average basis.
//
// The quantities matter. Buying and selling the identical float64 cancels
// exactly (a-a == 0 in IEEE-754), which leaves no residue and lets a `pos == 0`
// implementation pass — the first version of this test made exactly that
// mistake and pinned nothing. Ten 0.1 buys sum to 0.9999999999999999, so
// selling 1.0 leaves -1.11e-16 behind, which is the state that breaks a naive
// walk. The trailing round trip is the assertion that would have caught the
// +$322,037: it must price off 100, not off dust.
func TestRealizedNoPhantomPnLFromFloatResidue(t *testing.T) {
	fills := []Fill{}
	at := 0
	for range 10 {
		at++
		fills = append(fills, fill("FFF", "buy", 0.1, 65.91, at))
	}
	at++
	fills = append(fills, fill("FFF", "sell", 1.0, 65.91, at))
	at++
	fills = append(fills, fill("FFF", "buy", 1, 100, at))
	at++
	fills = append(fills, fill("FFF", "sell", 1, 101, at))

	got := RealizedBySymbol(fills)["FFF"]
	// Exact zero, not merely "small": a residual position is the hazard itself.
	if got.Position != 0 {
		t.Errorf("Position = %g, want exactly 0 — residue must be snapped, not tolerated", got.Position)
	}
	near(t, got.Realized, 1.00, "realized (the trailing 100→101 round trip)")
	if math.Abs(got.Realized) > 100 {
		t.Fatalf("phantom P&L %.2f — basis was computed from float dust", got.Realized)
	}
}

// A plain "sell" driving the position negative means shares left the account
// that this history never saw arrive — the JMIA signature, and also what a
// too-narrow fetch window looks like from the inside. It must NOT be silently
// modelled as a short.
func TestRealizedFlagsSellsWithNoAcquisition(t *testing.T) {
	got := RealizedBySymbol([]Fill{
		fill("III", "buy", 100, 10, 1),
		fill("III", "sell", 500, 12, 2), // 400 more than was ever bought
	})["III"]
	if !got.SoldWithoutBuying {
		t.Error("SoldWithoutBuying = false — an unexplained disposal must be flagged, not booked as a short")
	}
	if got.Trustworthy() {
		t.Error("Trustworthy() = true; a symbol with an incomplete history must be excluded from totals")
	}
}

// An explicit short is a legitimate strategy, not a data-integrity problem.
func TestRealizedDoesNotFlagExplicitShorts(t *testing.T) {
	got := RealizedBySymbol([]Fill{
		fill("JJJ", "sell_short", 50, 40, 1),
		fill("JJJ", "buy", 50, 36, 2),
	})["JJJ"]
	if got.SoldWithoutBuying {
		t.Error("SoldWithoutBuying = true for a clean short round trip — sell_short is not an anomaly")
	}
	if !got.Shorted {
		t.Error("Shorted = false; the position was genuinely negative")
	}
}

// Shorted describes the POSITION, not the broker's side vocabulary. A long-only
// symbol that happens to be sold down to zero was never short.
func TestShortedTracksPositionNotSideVocabulary(t *testing.T) {
	got := RealizedBySymbol([]Fill{
		fill("KKK", "buy", 10, 5, 1),
		fill("KKK", "sell", 10, 6, 2),
	})["KKK"]
	if got.Shorted {
		t.Error("Shorted = true for a long round trip closed with an ordinary sell")
	}
}

func TestIsOptionSymbol(t *testing.T) {
	for _, tc := range []struct {
		symbol string
		want   bool
	}{
		{"AAPL  260320C00150000", true},
		{"SPY   260117P00400000", true},
		{"AAPL", false},
		{"JMIA", false},
		{"BRK.B", false},
		{"AAPL  260320X00150000", false}, // neither call nor put
		{"AAPL  2603X0C00150000", false}, // non-digit in the date
	} {
		if got := IsOptionSymbol(tc.symbol); got != tc.want {
			t.Errorf("IsOptionSymbol(%q) = %v, want %v", tc.symbol, got, tc.want)
		}
	}
}

// Option contracts share the Alpaca account but are recorded in orb_trades, so
// reconciling them against `trades` would flag every one as off-ledger.
func TestPartitionFillsSeparatesOptions(t *testing.T) {
	equity, options := PartitionFills([]Fill{
		fill("JMIA", "buy", 10, 5, 1),
		fill("AAPL  260320C00150000", "buy", 1, 2.5, 2),
	})
	if len(equity) != 1 || equity[0].Symbol != "JMIA" {
		t.Errorf("equity = %+v, want just JMIA", equity)
	}
	if len(options) != 1 {
		t.Errorf("options = %+v, want the OCC contract", options)
	}
}

func TestRealizedOpenPositionIsNotFlat(t *testing.T) {
	got := RealizedBySymbol([]Fill{
		fill("GGG", "buy", 100, 10, 1),
		fill("GGG", "sell", 40, 12, 2),
	})["GGG"]
	near(t, got.Realized, 80, "realized on the closed portion")
	near(t, got.Position, 60, "open position")
	if got.Flat() {
		t.Error("Flat() = true with 60 shares open — open symbols must be excluded from exact comparison")
	}
}

// Fills inside the same second must order deterministically, or the basis walk
// varies run to run.
//
// Two fills cannot detect this: buy→sell and sell→buy both net the same P&L
// (the second opens a short and covers for the identical amount), so the first
// version of this test passed with the sort deleted entirely. Three fills at one
// timestamp discriminate — ID order averages the basis to 10 and realizes +100,
// while sell-first shorts at 20 and covers at 5 for +150.
func TestRealizedDeterministicWithinOneTimestamp(t *testing.T) {
	same := time.Date(2026, 7, 1, 14, 0, 0, 0, time.UTC)
	mk := func(id, side string, q, p float64) Fill {
		return Fill{ID: id, Symbol: "HHH", Side: side, Qty: q, Price: p, Time: same}
	}
	inOrder := []Fill{mk("1", "buy", 10, 5), mk("2", "buy", 10, 15), mk("3", "sell", 10, 20)}
	shuffled := []Fill{mk("3", "sell", 10, 20), mk("1", "buy", 10, 5), mk("2", "buy", 10, 15)}

	a := RealizedBySymbol(inOrder)["HHH"]
	b := RealizedBySymbol(shuffled)["HHH"]
	near(t, a.Realized, b.Realized, "order-independent realized")
	near(t, a.Realized, 100, "realized under ID ordering (basis 10, sold 10 @ 20)")
}
