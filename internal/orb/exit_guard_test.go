package orb

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestExitConsistentWithBroker pins the phantom-prevention rule against today's shapes and
// the multi-trade netting edge.
func TestExitConsistentWithBroker(t *testing.T) {
	cases := []struct {
		name             string
		side             string
		brokerNet        float64
		managed, closing int
		wantAllow        bool
	}{
		// Today's XAG phantom: short still fully held at the broker → BLOCK the close.
		{"phantom short still held", "sell", -850, 850, 850, false},
		// Today's XAU real TP: broker flat → ALLOW the close.
		{"real close, broker flat", "sell", 0, 22, 22, true},
		{"long real close, broker flat", "buy", 0, 10, 10, true},
		// Two shorts (1800 open); broker reduced by 850 (holds -950). Closing the reduced
		// 850 is consistent; closing the still-held 950 is a phantom.
		{"multi: broker reduced, close the reduced leg", "sell", -950, 1800, 850, true},
		{"multi: broker reduced, close the STILL-held leg", "sell", -950, 1800, 950, false},
		// Opposite-side broker position never blocks a close on this side.
		{"broker long, closing a sell → no held short", "sell", 500, 500, 500, true},
	}
	for _, c := range cases {
		if got := exitConsistentWithBroker(c.side, c.brokerNet, c.managed, c.closing); got != c.wantAllow {
			t.Errorf("%s: exitConsistentWithBroker = %v; want %v", c.name, got, c.wantAllow)
		}
	}
}

// TestExitGuard_DisabledAndFailClosed: a non-forex scheduler disables the guard (closes
// proceed); a forex scheduler whose positions read failed blocks (fail closed).
func TestExitGuard_DisabledAndFailClosed(t *testing.T) {
	trade := &store.ORBTrade{Ticker: "XAG/USD", Side: "sell", Qty: 850}

	// Disabled (no guarded scheduler) → typed allow.
	if d := (&exitGuard{active: false}).check(trade); d.Code != guardAllow || d.Closing != 850 {
		t.Errorf("disabled decision = %+v, want allow with closing qty", d)
	}
	// Failed broker/DB truth read is distinct from a still-held position.
	if d := (&exitGuard{active: true, readFailed: true}).check(trade); d.Code != guardReadFailed {
		t.Errorf("failed-read decision = %+v, want guardReadFailed", d)
	}
	// Active with a held short returns the broker-held state and its evidence.
	g := &exitGuard{
		active:  true,
		net:     map[string]float64{positionIdentity("XAG/USD", ""): -850},
		managed: map[string]int{managedKey("XAG/USD", "", "sell"): 850},
		claimed: map[string]int{},
	}
	if d := g.check(trade); d.Code != guardBrokerStillHeld || d.BrokerNet != -850 || d.Managed != 850 || d.Closing != 850 {
		t.Errorf("held-position decision = %+v, want broker-held with -850/850/850 evidence", d)
	}
}

// TestExitGuard_EqualQtyPyramidNoDoubleApprove is the regression guard for the reviewer's
// Critical: two open SELL legs of EQUAL qty (850+850=1700), broker reduced by only one leg
// (net -850). The first close is legitimate; the second — matched against a stale record — is
// a phantom. The stateful claim budget must allow exactly ONE and block the other.
func TestExitGuard_EqualQtyPyramidNoDoubleApprove(t *testing.T) {
	g := &exitGuard{
		active:  true,
		net:     map[string]float64{positionIdentity("XAG/USD", ""): -850}, // one leg really closed
		managed: map[string]int{managedKey("XAG/USD", "", "sell"): 1700},
		claimed: map[string]int{},
	}
	t1 := &store.ORBTrade{Ticker: "XAG/USD", Side: "sell", Qty: 850}
	t2 := &store.ORBTrade{Ticker: "XAG/USD", Side: "sell", Qty: 850}

	if g.check(t1).Code != guardAllow {
		t.Fatal("first equal-qty leg (broker genuinely reduced by 850) must be allowed")
	}
	if g.check(t2).Code == guardAllow {
		t.Fatal("second equal-qty leg must be BLOCKED — broker still holds it; static check would have re-approved the phantom")
	}
}

func TestExitGuard_InvalidManagedBudgetIsNotReportedAsBrokerHeld(t *testing.T) {
	g := &exitGuard{active: true, net: map[string]float64{positionIdentity("MNQ", ""): 0}, managed: map[string]int{}, claimed: map[string]int{}}
	d := g.check(&store.ORBTrade{Ticker: "MNQ", Side: "sell", Qty: 1})
	if d.Code != guardInvalidManagedBudget {
		t.Fatalf("guard code = %v, want invalid managed budget", d.Code)
	}
	if d.Managed != 0 || d.Closing != 1 {
		t.Fatalf("decision quantities = managed %d closing %d; want 0/1", d.Managed, d.Closing)
	}
}

func TestExitGuardSeparatesFuturesExpiriesByConID(t *testing.T) {
	g := &exitGuard{
		active:  true,
		net:     map[string]float64{positionIdentity("MYM", "new-contract"): 3},
		managed: map[string]int{managedKey("MYM", "old-contract", "buy"): 3},
		claimed: make(map[string]int),
	}
	trade := &store.ORBTrade{Ticker: "MYM", ConID: "old-contract", Side: "buy", Qty: 3}
	if decision := g.check(trade); decision.Code != guardAllow {
		t.Fatalf("different-expiry position contaminated old contract guard: %+v", decision)
	}
}

func TestLegacyTradePositionUsesTickerIdentityDespiteBrokerConID(t *testing.T) {
	trade := store.ORBTrade{Ticker: "MYM", Side: "buy", Qty: 3}
	positions := positionsForOwnedTrades(
		[]execution.Position{{Ticker: "MYM", ConID: "815824200", Quantity: 3}},
		[]store.ORBTrade{trade},
	)
	if len(positions) != 1 || positions[0].ConID != "" {
		t.Fatalf("legacy position identity = %+v; want ticker-keyed position", positions)
	}
	g := &exitGuard{
		active:  true,
		net:     map[string]float64{positionIdentity(positions[0].Ticker, positions[0].ConID): positions[0].Quantity},
		managed: map[string]int{managedKey(trade.Ticker, trade.ConID, trade.Side): trade.Qty},
		claimed: map[string]int{},
	}
	if decision := g.check(&trade); decision.Code != guardBrokerStillHeld {
		t.Fatalf("legacy held position decision = %+v; want broker-held block", decision)
	}
}

func TestMixedLegacyAndDurableIdentityFailsClosed(t *testing.T) {
	if !hasMixedLegacyAndDurableIdentity([]store.ORBTrade{
		{Ticker: "MYM"},
		{Ticker: "MYM", ConID: "815824200"},
	}) {
		t.Fatal("mixed legacy/durable rows for one ticker were not detected")
	}
	if hasMixedLegacyAndDurableIdentity([]store.ORBTrade{
		{Ticker: "MYM", ConID: "old"},
		{Ticker: "MYM", ConID: "new"},
	}) {
		t.Fatal("two durable expiries were misclassified as legacy overlap")
	}
}

func TestMixedIdentityBlocksOnlyAffectedTicker(t *testing.T) {
	g := &exitGuard{
		active: true,
		ambiguous: mixedIdentityTickers([]store.ORBTrade{
			{Ticker: "MYM"}, {Ticker: "MYM", ConID: "new"}, {Ticker: "MNQ", ConID: "mnq"},
		}),
		net:     map[string]float64{positionIdentity("MNQ", "mnq"): 0},
		managed: map[string]int{managedKey("MNQ", "mnq", "buy"): 1},
		claimed: make(map[string]int),
	}
	if d := g.check(&store.ORBTrade{Ticker: "MYM", Qty: 1}); d.Code != guardAmbiguousIdentity {
		t.Fatalf("mixed ticker decision = %+v", d)
	}
	if d := g.check(&store.ORBTrade{Ticker: "MNQ", ConID: "mnq", Side: "buy", Qty: 1}); d.Code != guardAllow {
		t.Fatalf("healthy sibling decision = %+v; want allow", d)
	}
}

func TestSignedPositionFor(t *testing.T) {
	positions := []execution.Position{
		{Ticker: "XAG/USD", Quantity: -850},
		{Ticker: "XAU/USD", Quantity: 22},
	}
	if got := signedPositionFor(positions, "XAG/USD"); got != -850 {
		t.Errorf("XAG signed = %v; want -850", got)
	}
	if got := signedPositionFor(positions, "MISSING"); got != 0 {
		t.Errorf("missing ticker = %v; want 0", got)
	}
}
