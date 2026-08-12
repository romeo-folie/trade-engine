package execution

import "testing"

// IsTerminalOrderStatus: a fill price appearing is NOT terminal —
// partially_filled must keep the fill poll alive (the NOK partial-fill freeze),
// while filled/cancelled/rejected/expired/done_for_day end it.
func TestIsTerminalOrderStatus(t *testing.T) {
	terminal := []string{"filled", "canceled", "cancelled", "rejected", "expired", "done_for_day"}
	for _, s := range terminal {
		if !IsTerminalOrderStatus(s) {
			t.Errorf("%q must be terminal", s)
		}
	}
	nonTerminal := []string{"partially_filled", "partial", "new", "accepted", "pending_new", "held", ""}
	for _, s := range nonTerminal {
		if IsTerminalOrderStatus(s) {
			t.Errorf("%q must NOT be terminal", s)
		}
	}
}

func TestNormalizeOrderStatusDoneForDayIsCancelled(t *testing.T) {
	if got := NormalizeOrderStatus("done_for_day"); got != "cancelled" {
		t.Fatalf("NormalizeOrderStatus(done_for_day)=%q, want cancelled", got)
	}
}
