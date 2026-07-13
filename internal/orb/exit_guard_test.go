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

	// Disabled (no forex config) → allows.
	if !(&exitGuard{active: false}).allows(trade) {
		t.Error("disabled guard must allow the close")
	}
	// Fail closed (positions read failed) → blocks.
	if (&exitGuard{active: true, readFailed: true}).allows(trade) {
		t.Error("failed-read guard must block the close (fail closed against fabrication)")
	}
	// Active with a held short → blocks.
	g := &exitGuard{
		active:  true,
		net:     map[string]float64{"XAG/USD": -850},
		managed: map[string]int{managedKey("XAG/USD", "sell"): 850},
		claimed: map[string]int{},
	}
	if g.allows(trade) {
		t.Error("guard must block a close while the broker still holds the full short")
	}
}

// TestExitGuard_EqualQtyPyramidNoDoubleApprove is the regression guard for the reviewer's
// Critical: two open SELL legs of EQUAL qty (850+850=1700), broker reduced by only one leg
// (net -850). The first close is legitimate; the second — matched against a stale record — is
// a phantom. The stateful claim budget must allow exactly ONE and block the other.
func TestExitGuard_EqualQtyPyramidNoDoubleApprove(t *testing.T) {
	g := &exitGuard{
		active:  true,
		net:     map[string]float64{"XAG/USD": -850}, // one leg really closed
		managed: map[string]int{managedKey("XAG/USD", "sell"): 1700},
		claimed: map[string]int{},
	}
	t1 := &store.ORBTrade{Ticker: "XAG/USD", Side: "sell", Qty: 850}
	t2 := &store.ORBTrade{Ticker: "XAG/USD", Side: "sell", Qty: 850}

	if !g.allows(t1) {
		t.Fatal("first equal-qty leg (broker genuinely reduced by 850) must be allowed")
	}
	if g.allows(t2) {
		t.Fatal("second equal-qty leg must be BLOCKED — broker still holds it; static check would have re-approved the phantom")
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
