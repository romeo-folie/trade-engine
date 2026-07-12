package options

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
)

func TestAtmIVFromChain_NearestWithIV(t *testing.T) {
	exp := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	snaps := []execution.OptionSnapshot{
		snap("SPY", "C", 549, nil, f64(0.19), 1, 1.1, exp),
		snap("SPY", "C", 551, nil, f64(0.18), 1, 1.1, exp), // nearest to 551.2
		snap("SPY", "C", 560, nil, f64(0.25), 1, 1.1, exp),
		snap("SPY", "C", 551.5, nil, nil, 1, 1.1, exp), // even nearer but no IV -> skipped
	}
	iv, ok := atmIVFromChain(snaps, 551.2)
	if !ok || math.Abs(iv-0.18) > 1e-9 {
		t.Fatalf("atmIVFromChain = %v,%v; want 0.18", iv, ok)
	}
	// No IV anywhere -> not ok.
	if _, ok := atmIVFromChain([]execution.OptionSnapshot{snap("SPY", "C", 551, nil, nil, 1, 1.1, exp)}, 551); ok {
		t.Error("expected not ok when no snapshot carries IV")
	}
}

func TestRecordLiveATMIV(t *testing.T) {
	st := newFakeIVStore()
	exp := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	asOf := time.Date(2026, 7, 13, 15, 0, 0, 0, time.UTC)
	snaps := []execution.OptionSnapshot{
		snap("SPY", "C", 551, nil, f64(0.21), 1.2, 1.25, exp),
	}
	if err := RecordLiveATMIV(context.Background(), st, "SPY", asOf, 551, snaps); err != nil {
		t.Fatalf("RecordLiveATMIV: %v", err)
	}
	hist, _ := st.GetIVHistory(context.Background(), "SPY", asOf.AddDate(0, 0, -1), asOf)
	if len(hist) != 1 || math.Abs(hist[0].ATMIV-0.21) > 1e-9 {
		t.Errorf("stored history = %+v; want one day at 0.21", hist)
	}
	// Stored at the precise snapshot INSTANT (not day-floored), so the as-of filter is
	// lookahead-safe — a regression to dayFloor(asOf) would fail here.
	if !hist[0].Date.Equal(asOf) {
		t.Errorf("stored date = %v; want the instant %v (not day-floored)", hist[0].Date, asOf)
	}
}

// fakeHistProvider yields quotes whose OptionMid is the exact Black-Scholes price
// at a known IV, so BackfillIVHistory should recover that IV.
type fakeHistProvider struct {
	quotes []HistoricalATMQuote
	err    error
}

func (f fakeHistProvider) ATMQuotes(_ context.Context, _ string, _, _ time.Time) ([]HistoricalATMQuote, error) {
	return f.quotes, f.err
}

func TestBackfillIVHistory_RecoversKnownIV_AndSkipsBadPrices(t *testing.T) {
	const trueIV = 0.22
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	var quotes []HistoricalATMQuote
	for i := range 45 {
		spot := 550.0 + float64(i) // vary spot a bit
		strike := math.Round(spot) // ATM
		dteY := 4.0 / 365          // ~weekly
		price := BSPrice("C", spot, strike, dteY, 0.04, 0, trueIV)
		quotes = append(quotes, HistoricalATMQuote{
			Date: base.AddDate(0, 0, i), Right: "C", Spot: spot, Strike: strike, OptionMid: price, DTEYears: dteY,
		})
	}
	// One un-invertible quote (price above spot) that must be skipped.
	quotes = append(quotes, HistoricalATMQuote{
		Date: base.AddDate(0, 0, 45), Right: "C", Spot: 100, Strike: 100, OptionMid: 200, DTEYears: 0.05,
	})

	st := newFakeIVStore()
	n, err := BackfillIVHistory(context.Background(), st, fakeHistProvider{quotes: quotes}, "SPY", base, base.AddDate(0, 0, 45), 0.04, 0)
	if err != nil {
		t.Fatalf("BackfillIVHistory: %v", err)
	}
	if n != 45 {
		t.Errorf("recorded = %d; want 45 (bad price skipped)", n)
	}
	hist, _ := st.GetIVHistory(context.Background(), "SPY", base, base.AddDate(0, 0, 45))
	if len(hist) != 45 {
		t.Fatalf("stored = %d; want 45", len(hist))
	}
	for _, d := range hist {
		if math.Abs(d.ATMIV-trueIV) > 1e-3 {
			t.Errorf("derived IV %.4f; want ≈%.2f", d.ATMIV, trueIV)
		}
	}
}
