package orb

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// findOrphanedPositions must surface the July metals shape (a broker short with
// no filled ORB row), partial coverage, and side flips — while staying silent
// when the managed rows fully explain the broker's positions.
func TestFindOrphanedPositions(t *testing.T) {
	positions := []execution.Position{
		{Ticker: "XAU/USD", Quantity: -57}, // orphan: filled as 'cancelled', no managed row
		{Ticker: "XAG/USD", Quantity: -700},
		{Ticker: "EUR/USD", Quantity: 20000},  // fully managed — must NOT alert
		{Ticker: "GBP/USD", Quantity: 30000},  // partial: managed 20000
		{Ticker: "USD/CHF", Quantity: -10000}, // side flip: DB thinks long
		{Ticker: "AUD/USD", Quantity: 0},      // flat — ignored
	}
	filled := []store.ORBTrade{
		{Ticker: "EUR/USD", Side: "buy", Qty: 20000},
		{Ticker: "GBP/USD", Side: "buy", Qty: 20000}, // 10k short of broker
		{Ticker: "USD/CHF", Side: "buy", Qty: 10000}, // long, but broker is short
	}

	got := findOrphanedPositions(positions, filled, orphanPositionTolerance)

	byTicker := map[string]orphanFinding{}
	for _, f := range got {
		byTicker[f.Ticker] = f
	}

	// EUR/USD fully explained → no finding.
	if _, ok := byTicker["EUR/USD"]; ok {
		t.Error("fully-managed EUR/USD must not be flagged")
	}
	// AUD/USD flat → no finding.
	if _, ok := byTicker["AUD/USD"]; ok {
		t.Error("flat AUD/USD must not be flagged")
	}
	// The two clean orphans.
	for _, tk := range []string{"XAU/USD", "XAG/USD"} {
		f, ok := byTicker[tk]
		if !ok {
			t.Fatalf("%s orphan not detected", tk)
		}
		if f.ManagedQty != 0 {
			t.Errorf("%s managed should be 0, got %v", tk, f.ManagedQty)
		}
	}
	if byTicker["XAU/USD"].BrokerQty != -57 {
		t.Errorf("XAU/USD broker qty = %v, want -57", byTicker["XAU/USD"].BrokerQty)
	}
	// Partial: 10k unaccounted.
	gbp, ok := byTicker["GBP/USD"]
	if !ok || gbp.ManagedQty != 20000 {
		t.Fatalf("GBP/USD partial not detected correctly: %+v", gbp)
	}
	// Side flip: broker short, DB long → flagged regardless of equal magnitude.
	if _, ok := byTicker["USD/CHF"]; !ok {
		t.Error("side-flip USD/CHF must be flagged even at equal magnitude")
	}

	// Deterministic, sorted output (grep-friendly logs).
	for i := 1; i < len(got); i++ {
		if got[i-1].Ticker > got[i].Ticker {
			t.Errorf("findings not sorted: %q before %q", got[i-1].Ticker, got[i].Ticker)
		}
	}
}

func TestFindORBRowHealthSurfacesFlatBrokerIdentityDefects(t *testing.T) {
	rows := []store.ORBTrade{
		{ID: "mym", Ticker: "MYM", Side: "buy", Qty: 3, FillPrice: 52874, OwnerScope: "futures_index_orb", AssetClass: "equity"},
		{ID: "mnq", Ticker: "MNQ", Side: "sell", Qty: 1, FillPrice: 22000, OwnerScope: "futures_index_orb", AssetClass: "futures", PointValue: 2, ConID: "123"},
		{ID: "legacy", Ticker: "ES", OwnerScope: "", AssetClass: "futures"},
	}
	got := findORBRowHealth(rows, "futures_index_orb", "futures")
	codes := make(map[string]bool)
	for _, f := range got {
		codes[f.TradeID+"|"+f.Code] = true
	}
	for _, want := range []string{"mym|asset_owner_mismatch", "mym|point_value_missing", "mym|conid_missing", "legacy|owner_scope_invalid"} {
		if !codes[want] {
			t.Errorf("missing row-health finding %s; got %+v", want, got)
		}
	}
	if len(got) != 4 {
		t.Fatalf("findings = %+v; want exactly four", got)
	}
}

func TestFindORBRowHealthFlagsInvalidEntryEconomics(t *testing.T) {
	rows := []store.ORBTrade{{ID: "bad", Ticker: "MNQ", OwnerScope: "futures_index_orb", AssetClass: "futures", PointValue: 2, ConID: "123"}}
	got := findORBRowHealth(rows, "futures_index_orb", "futures")
	if len(got) != 1 || got[0].Code != "entry_economics_invalid" {
		t.Fatalf("findings = %+v; want entry_economics_invalid", got)
	}
}

func TestPositionsForOwnedTradesUsesDurableConIDAfterDeconfiguration(t *testing.T) {
	positions := []execution.Position{
		{Ticker: "815824200", Quantity: 3},               // unresolved MYM contract
		{Ticker: "MNQ", Quantity: 1},                     // legacy/configured symbol fallback
		{Ticker: "MYM", ConID: "999999999", Quantity: 4}, // different expiry of same root
		{Ticker: "GC", Quantity: 2},                      // unrelated account position
	}
	rows := []store.ORBTrade{
		{Ticker: "MYM", ConID: "815824200", OwnerScope: "futures_index_orb", AssetClass: "futures"},
		{Ticker: "MNQ", OwnerScope: "futures_index_orb", AssetClass: "futures"},
	}

	got := positionsForOwnedTrades(positions, rows)
	if len(got) != 2 {
		t.Fatalf("owned positions = %+v, want MYM and MNQ only", got)
	}
	if got[0].Ticker != "MYM" || got[0].Quantity != 3 {
		t.Fatalf("conid position = %+v, want durable ticker MYM qty 3", got[0])
	}
	if got[1].Ticker != "MNQ" || got[1].Quantity != 1 {
		t.Fatalf("ticker fallback position = %+v, want MNQ qty 1", got[1])
	}
}

func TestLegacyForexPositionNormalizesBrokerConIDBeforeOrphanComparison(t *testing.T) {
	filled := []store.ORBTrade{{Ticker: "EUR/USD", Side: "buy", Qty: 20000}}
	positions := normalizeLegacyPositionIdentities(
		[]execution.Position{
			{Ticker: "EUR/USD", ConID: "12087792", Quantity: 20000},
			{Ticker: "XAU/USD", ConID: "457480516", Quantity: 2},
		},
		filled,
	)
	if len(positions) != 2 || positions[0].ConID != "" {
		t.Fatalf("legacy position = %+v; want ticker-keyed identity", positions)
	}
	got := findOrphanedPositions(positions, filled, orphanPositionTolerance)
	if len(got) != 1 || got[0].Ticker != "XAU/USD" {
		t.Fatalf("findings = %+v; want only unmatched XAU/USD preserved", got)
	}
}

// A short managed by a matching short row is fully explained.
// forexOnlyPositions must drop positions that aren't this scheduler's configured
// forex/metals instruments — specifically a concurrent futures scheduler's
// positions on a shared broker, which come back with raw numeric conids as their
// "ticker" and would otherwise be flagged as orphans (Codex #397).
func TestForexOnlyPositions_ExcludesFutures(t *testing.T) {
	s := &ORBScheduler{forexCfg: &config.ForexORBConfig{Enabled: true, Instruments: []config.Instrument{
		{Symbol: "XAU/USD"}, {Symbol: "XAG/USD"},
	}}}
	positions := []execution.Position{
		{Ticker: "XAU/USD", Quantity: -57}, // managed metals — kept
		{Ticker: "793356217", Quantity: 6}, // MES front-month conid (unresolved futures) — dropped
		{Ticker: "MES", Quantity: 6},       // even a resolved futures symbol isn't a forex instrument — dropped
	}
	got := s.forexOnlyPositions(positions)
	if len(got) != 1 || got[0].Ticker != "XAU/USD" {
		t.Fatalf("expected only XAU/USD retained, got %+v", got)
	}
}

func TestFindOrphanedPositions_ManagedShort(t *testing.T) {
	positions := []execution.Position{{Ticker: "XAG/USD", Quantity: -700}}
	filled := []store.ORBTrade{{Ticker: "XAG/USD", Side: "sell", Qty: 700}}
	if got := findOrphanedPositions(positions, filled, orphanPositionTolerance); len(got) != 0 {
		t.Errorf("managed short must not be flagged, got %+v", got)
	}
}

// Sub-tolerance fractional residue (broker rounding) is not drift.
func TestFindOrphanedPositions_Tolerance(t *testing.T) {
	positions := []execution.Position{{Ticker: "EUR/USD", Quantity: 20000.3}}
	filled := []store.ORBTrade{{Ticker: "EUR/USD", Side: "buy", Qty: 20000}}
	if got := findOrphanedPositions(positions, filled, orphanPositionTolerance); len(got) != 0 {
		t.Errorf("sub-tolerance residue must not be flagged, got %+v", got)
	}
}
