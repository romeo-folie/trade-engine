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
		{Ticker: "MES", Quantity: 6},        // even a resolved futures symbol isn't a forex instrument — dropped
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
