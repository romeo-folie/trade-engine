package engine

import (
	"context"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/store"
)

type fakeAdoptionStore struct{ inserted []store.Trade }

func (f *fakeAdoptionStore) InsertTrade(_ context.Context, t store.Trade) (string, error) {
	f.inserted = append(f.inserted, t)
	return "trade-" + t.Ticker, nil
}

// AdoptBrokerOrphans: broker_only adopts in full; a same-direction mismatch
// adopts only the untracked surplus; over-recorded DB and side_mismatch/db_only
// are never adopted. Stops anchor on the broker's CURRENT price so a deep
// underwater orphan (NOK: entry 16.69, current 11.78) is not instantly
// stopped out at market on the first monitor pass.
func TestAdoptBrokerOrphans(t *testing.T) {
	fs := &fakeAdoptionStore{}
	drifts := []BrokerDrift{
		// NOK shape: full orphan, deep underwater.
		{Ticker: "NOK", BrokerQty: 178.009073, DBQty: 0, Source: "broker_only", AvgEntryPrice: 16.6925, CurrentPrice: 11.775},
		// SBET shape: same-direction mismatch — adopt only the surplus.
		{Ticker: "SBET", BrokerQty: 518.877852, DBQty: 349.82628, Source: "mismatch", AvgEntryPrice: 10, CurrentPrice: 8},
		// DB over-records the broker: different bug class, alert-only.
		{Ticker: "OVER", BrokerQty: 5, DBQty: 9, Source: "mismatch", AvgEntryPrice: 1, CurrentPrice: 1},
		// Never adopted.
		{Ticker: "SIDE", BrokerQty: -50, DBQty: 50, Source: "side_mismatch", CurrentPrice: 1},
		{Ticker: "GONE", BrokerQty: 0, DBQty: 30, Source: "db_only"},
		// Orphan short: adopted as short with stop ABOVE current.
		{Ticker: "SHRT", BrokerQty: -10, DBQty: 0, Source: "broker_only", AvgEntryPrice: 20, CurrentPrice: 18},
		// No current price: adopt without fabricating a stop.
		{Ticker: "NOPX", BrokerQty: 7, DBQty: 0, Source: "broker_only", AvgEntryPrice: 3, CurrentPrice: 0},
	}

	adopted := AdoptBrokerOrphans(context.Background(), fs, drifts, 2.0, 4.0)
	if len(adopted) != 4 {
		t.Fatalf("adopted = %v, want NOK, SBET, SHRT, NOPX", adopted)
	}

	byTicker := map[string]store.Trade{}
	for _, tr := range fs.inserted {
		byTicker[tr.Ticker] = tr
	}

	nok := byTicker["NOK"]
	if nok.Quantity != 178.009073 || nok.Direction != "long" || nok.Strategy != "recovery" || nok.Status != "open" {
		t.Errorf("NOK adoption wrong: %+v", nok)
	}
	// Stop from CURRENT price (11.775 × 0.98), far below the 16.36 an
	// entry-anchored stop would have set (which would instantly fire).
	if nok.StopLoss < 11.5 || nok.StopLoss >= 11.775 {
		t.Errorf("NOK stop must anchor on current price: got %v", nok.StopLoss)
	}
	if nok.TakeProfit <= 11.775 {
		t.Errorf("NOK TP must be above current price: got %v", nok.TakeProfit)
	}

	sbet := byTicker["SBET"]
	if got, want := sbet.Quantity, 518.877852-349.82628; got < want-1e-6 || got > want+1e-6 {
		t.Errorf("SBET must adopt only the surplus: got %v want %v", got, want)
	}

	shrt := byTicker["SHRT"]
	if shrt.Direction != "short" || shrt.Quantity != 10 {
		t.Errorf("short orphan adoption wrong: %+v", shrt)
	}
	if shrt.StopLoss <= 18 {
		t.Errorf("short stop must be ABOVE current price: got %v", shrt.StopLoss)
	}
	if shrt.TakeProfit >= 18 {
		t.Errorf("short TP must be BELOW current price: got %v", shrt.TakeProfit)
	}

	nopx := byTicker["NOPX"]
	if nopx.StopLoss != 0 || nopx.TakeProfit != 0 {
		t.Errorf("no current price must mean no fabricated stop/TP: %+v", nopx)
	}

	for _, bad := range []string{"OVER", "SIDE", "GONE"} {
		if _, ok := byTicker[bad]; ok {
			t.Errorf("%s must not be adopted", bad)
		}
	}
}
