package execution

import "testing"

// IsTerminalOrderStatus: a fill price appearing is NOT terminal —
// partially_filled must keep the fill poll alive (the NOK partial-fill freeze),
// while filled/cancelled/rejected/expired end it. A TIF-less done_for_day is
// intentionally conservative because it can describe a still-live GTC stop.
func TestIsTerminalOrderStatus(t *testing.T) {
	terminal := []string{"filled", "canceled", "cancelled", "rejected", "expired"}
	for _, s := range terminal {
		if !IsTerminalOrderStatus(s) {
			t.Errorf("%q must be terminal", s)
		}
	}
	nonTerminal := []string{"partially_filled", "partial", "new", "accepted", "pending_new", "held", "", "done_for_day"}
	for _, s := range nonTerminal {
		if IsTerminalOrderStatus(s) {
			t.Errorf("%q must NOT be terminal", s)
		}
	}
}

func TestNormalizeOrderStatusDoneForDayIsConservativelyPending(t *testing.T) {
	if got := NormalizeOrderStatus("done_for_day"); got != "pending" {
		t.Fatalf("NormalizeOrderStatus(done_for_day)=%q, want pending", got)
	}
}

func TestDoneForDayGTCProtectionRemainsWorking(t *testing.T) {
	if got := NormalizeOrderStatusForTimeInForce("done_for_day", "gtc"); got != "pending" {
		t.Fatalf("GTC done_for_day status=%q, want pending", got)
	}
	if IsTerminalOrderStatusForTimeInForce("done_for_day", "gtc") {
		t.Fatal("GTC done_for_day must not terminalize protective coverage")
	}
	if got := NormalizeOrderStatusForTimeInForce("done_for_day", "day"); got != "cancelled" {
		t.Fatalf("DAY done_for_day status=%q, want cancelled", got)
	}
	if !IsTerminalOrderStatusForTimeInForce("done_for_day", "day") {
		t.Fatal("DAY done_for_day must remain terminal")
	}
}
