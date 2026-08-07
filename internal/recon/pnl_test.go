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
// a position closes, float residue leaves pos and cost at ~1e-13, and the next
// sale divides cost by that dust to get an astronomical average basis.
func TestRealizedNoPhantomPnLFromFloatResidue(t *testing.T) {
	fills := []Fill{}
	at := 0
	// 60 open/close cycles on fractional quantities — plenty of residue.
	for range 60 {
		at++
		fills = append(fills, fill("FFF", "buy", 37.923008, 65.91, at))
		at++
		fills = append(fills, fill("FFF", "sell", 37.923008, 65.91, at))
	}
	got := RealizedBySymbol(fills)["FFF"]
	near(t, got.Realized, 0, "realized after 60 flat round trips")
	if !got.Flat() {
		t.Errorf("position = %v, want flat", got.Position)
	}
	if math.Abs(got.Realized) > 1 {
		t.Fatalf("phantom P&L %.2f — residual position/cost must be snapped to zero", got.Realized)
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
func TestRealizedDeterministicWithinOneTimestamp(t *testing.T) {
	same := time.Date(2026, 7, 1, 14, 0, 0, 0, time.UTC)
	mk := func(id, side string, q, p float64) Fill {
		return Fill{ID: id, Symbol: "HHH", Side: side, Qty: q, Price: p, Time: same}
	}
	a := RealizedBySymbol([]Fill{mk("1", "buy", 10, 5), mk("2", "sell", 10, 7)})["HHH"]
	b := RealizedBySymbol([]Fill{mk("2", "sell", 10, 7), mk("1", "buy", 10, 5)})["HHH"]
	near(t, a.Realized, b.Realized, "order-independent realized")
	near(t, a.Realized, 20, "realized")
}
