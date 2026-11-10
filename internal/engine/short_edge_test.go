package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/stockorders"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// openFilledShort opens the fixture's standard 5-share short at 98 with a
// working bracket (parent-1, legs parent-1-tp @92 and parent-1-sl @101).
func openFilledShort(t *testing.T, f *shortFixture) store.Trade {
	t.Helper()
	id, sig := f.signal()
	f.e.executeShortEntry(f.ctx, id, sig, false)
	trades := f.openTrades()
	if len(trades) != 1 || trades[0].Direction != "short" || trades[0].Quantity != 5 {
		t.Fatalf("setup: trades=%+v status=%v", trades, firstNonEmpty(f.signalStatus(id)))
	}
	return trades[0]
}

func (f *shortFixture) setLeg(id string, r execution.OrderResult) {
	f.broker.mu.Lock()
	defer f.broker.mu.Unlock()
	r.BrokerOrderID = id
	f.broker.byID[id] = r
}

// A take-profit (buy limit below entry) fill closes the short with a gain.
func TestShortBracketTargetFillClosesShort(t *testing.T) {
	f := newShortFixture(t, "SHTP")
	tr := openFilledShort(t, f)
	f.setLeg(f.id("parent-1-tp"), execution.OrderResult{Status: "filled", Quantity: 5, FilledQty: 5, FilledPrice: 92, FilledAt: time.Now().UTC()})
	f.setLeg(f.id("parent-1-sl"), execution.OrderResult{Status: "cancelled", Quantity: 5})
	if !f.e.reconcileBracketTrade(f.ctx, tr) {
		t.Fatal("short bracket trade was not broker-managed")
	}
	closed, _ := store.GetTradeByID(f.ctx, tr.ID)
	// (98 - 92) * 5 = +30.
	if closed == nil || closed.Status != "closed" || closed.PnL < 29.99 || closed.PnL > 30.01 {
		t.Fatalf("short after target fill = %+v, want closed with +30", closed)
	}
}

// Partial cover: 2 shares cover at the target, then the stop covers the other
// 3. The exit is fill-weighted and the P&L carries the short sign.
func TestShortBracketPartialTargetThenStop(t *testing.T) {
	f := newShortFixture(t, "SHPC")
	tr := openFilledShort(t, f)
	f.setLeg(f.id("parent-1-tp"), execution.OrderResult{Status: "cancelled", Quantity: 5, FilledQty: 2, FilledPrice: 92, FilledAt: time.Now().UTC()})
	f.setLeg(f.id("parent-1-sl"), execution.OrderResult{Status: "filled", Quantity: 3, FilledQty: 3, FilledPrice: 101, FilledAt: time.Now().UTC()})
	if !f.e.reconcileBracketTrade(f.ctx, tr) {
		t.Fatal("short bracket trade was not broker-managed")
	}
	closed, _ := store.GetTradeByID(f.ctx, tr.ID)
	// Exit (2×92 + 3×101)/5 = 97.4; short P&L (98 − 97.4) × 5 = +3.
	if closed == nil || closed.Status != "closed" || closed.ExitPrice < 97.39 || closed.ExitPrice > 97.41 || closed.PnL < 2.99 || closed.PnL > 3.01 {
		t.Fatalf("short after partial target + stop = %+v, want closed exit 97.4 pnl +3", closed)
	}
}

// Both legs die with only a partial cover. A strict (broker-protected) short
// is then unprotected, so it is contained, exactly as a strict long would be:
// the 2 covered shares are booked and exactly the remaining 3 are bought
// back. It is never handed to software exits, and never over-bought.
func TestShortBracketLegsDeadAfterPartialIsContained(t *testing.T) {
	f := newShortFixture(t, "SHLD")
	tr := openFilledShort(t, f)
	f.setLeg(f.id("parent-1-tp"), execution.OrderResult{Status: "cancelled", Quantity: 5, FilledQty: 2, FilledPrice: 92, FilledAt: time.Now().UTC()})
	f.setLeg(f.id("parent-1-sl"), execution.OrderResult{Status: "cancelled", Quantity: 5})
	// The broker's parent snapshot reflects the dead children.
	f.broker.mu.Lock()
	parent := f.broker.byID[f.id("parent-1")]
	for i := range parent.ChildOrders {
		parent.ChildOrders[i].Status = "cancelled"
		parent.ChildOrders[i].RemainingQty = 0
	}
	parent.ChildOrders[0].FilledQty, parent.ChildOrders[0].FilledPrice = 2, 92
	f.broker.byID[f.id("parent-1")] = parent
	f.broker.mu.Unlock()

	if !f.e.reconcileBracketTrade(f.ctx, tr) {
		t.Fatal("a strict short must stay broker-owned while its protection is verified")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(f.broker.placedOrders()) < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	placed := f.broker.placedOrders()
	if len(placed) != 2 || placed[1].Side != execution.OrderSideBuy || placed[1].Quantity != 3 {
		t.Fatalf("containment orders = %+v, want one BUY of the 3 shares still short", placed)
	}
}

// A partially filled short parent cannot activate its bracket children, so
// the filled shares are unprotected: containment cancels the parent and
// buys back exactly the filled quantity. It never sells more.
func TestShortPartialEntryIsContainedWithBuy(t *testing.T) {
	f := newShortFixture(t, "SHPE")
	f.broker.placeResult = func(req execution.OrderRequest, n int) (execution.OrderResult, error) {
		if req.OrderClass != "bracket" {
			return execution.OrderResult{BrokerOrderID: fmt.Sprintf("%s-flat-%d", req.Ticker, n), Status: "filled",
				Quantity: req.Quantity, FilledQty: req.Quantity, FilledPrice: req.ReferencePrice, FilledAt: time.Now().UTC()}, nil
		}
		r := filledBracket(req, n)
		r.Status, r.FilledQty, r.RemainingQty = "partial", 2, req.Quantity-2
		for i := range r.ChildOrders {
			r.ChildOrders[i].Status = "held"
		}
		return r, nil
	}
	id, sig := f.signal()
	f.e.executeShortEntry(f.ctx, id, sig, false)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(f.broker.placedOrders()) < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	placed := f.broker.placedOrders()
	if len(placed) < 2 {
		t.Fatalf("partial unprotected short was not contained: placed=%+v cancelled=%v", placed, f.broker.cancelled)
	}
	flatten := placed[1]
	if flatten.Side != execution.OrderSideBuy || flatten.Quantity != 2 {
		t.Fatalf("containment = %+v, want a BUY of exactly the 2 filled shares", flatten)
	}
	f.broker.mu.Lock()
	cancelledParent := false
	for _, c := range f.broker.cancelled {
		if c == f.id("parent-1") {
			cancelledParent = true
		}
	}
	f.broker.mu.Unlock()
	if !cancelledParent {
		t.Fatal("containment must cancel the partially filled parent before flattening")
	}
}

// Recovery from broker truth: an accepted short that the broker later
// cancels with no fill leaves no trade and releases its reservation; one that
// partially filled while the engine was down recovers into exactly one short
// trade for the filled quantity.
func TestShortRecoveryCancelledAndPartialEntries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    string
		filled    float64
		wantTrade bool
	}{
		{"cancelled with no fill", "cancelled", 0, false},
		{"partial fill while down", "partial", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShortFixture(t, "SHRC")
			f.broker.placeResult = func(req execution.OrderRequest, n int) (execution.OrderResult, error) {
				return execution.OrderResult{BrokerOrderID: fmt.Sprintf("%s-acc-%d", req.Ticker, n), Status: "accepted", Quantity: req.Quantity, RemainingQty: req.Quantity}, nil
			}
			id, sig := f.signal()
			f.e.executeShortEntry(f.ctx, id, sig, false)
			placed := f.broker.placedOrders()
			if len(placed) != 1 {
				t.Fatalf("placed %d", len(placed))
			}
			res := filledBracket(placed[0], 1)
			res.BrokerOrderID, res.Status, res.FilledQty = f.id("acc-1"), tc.status, tc.filled
			res.RemainingQty = placed[0].Quantity - tc.filled
			if tc.filled == 0 {
				res.FilledPrice = 0
			}
			f.broker.mu.Lock()
			f.broker.byClient[placed[0].ClientOrderID] = res
			f.broker.byID[f.id("acc-1")] = res
			f.broker.mu.Unlock()
			coordinator := f.e.executor.(*stockorders.Coordinator)
			for i := 0; i < 2; i++ {
				if err := coordinator.Recover(f.ctx); err != nil && containsAny(err.Error(), placed[0].ClientOrderID) {
					t.Fatalf("Recover: %v", err)
				}
			}
			trades := f.openTrades()
			if tc.wantTrade {
				if len(trades) != 1 || trades[0].Direction != "short" || trades[0].Quantity != tc.filled {
					t.Fatalf("recovered trades = %+v, want one short of %v", trades, tc.filled)
				}
				return
			}
			if len(trades) != 0 {
				t.Fatalf("cancelled short left trades %+v", trades)
			}
			res2, err := store.ListActiveStockEntryReservations(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range res2 {
				if r.IdempotencyKey == "entry:"+id {
					t.Fatalf("cancelled short still holds a reservation: %+v", r)
				}
			}
		})
	}
}

func containsAny(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// Liveness regression (pre-existing on main, both sides): once containment
// books a parent-reported child fill, it must flatten the remainder instead
// of deferring on every later pass. Repeated passes buy exactly once.
func TestContainmentAfterBookedChildFillFlattensOnceShort(t *testing.T) {
	f := newShortFixture(t, "SHPR")
	tr := openFilledShort(t, f)
	f.setLeg(f.id("parent-1-tp"), execution.OrderResult{Status: "cancelled", Quantity: 5, FilledQty: 2, FilledPrice: 92, FilledAt: time.Now().UTC()})
	f.setLeg(f.id("parent-1-sl"), execution.OrderResult{Status: "cancelled", Quantity: 5})
	f.broker.mu.Lock()
	parent := f.broker.byID[f.id("parent-1")]
	for i := range parent.ChildOrders {
		parent.ChildOrders[i].Status = "cancelled"
		parent.ChildOrders[i].RemainingQty = 0
	}
	parent.ChildOrders[0].FilledQty, parent.ChildOrders[0].FilledPrice = 2, 92
	f.broker.byID[f.id("parent-1")] = parent
	f.broker.mu.Unlock()
	for pass := 0; pass < 3; pass++ {
		f.e.containUnprotectedBrokerEntry(f.ctx, tr.ID, f.id("parent-1"), f.ticker, "regression")
	}
	placed := f.broker.placedOrders()
	if len(placed) != 2 || placed[1].Side != execution.OrderSideBuy || placed[1].Quantity != 3 {
		t.Fatalf("orders = %+v, want the entry plus exactly one BUY of the 3 remaining shares", placed)
	}
	after, _ := store.GetTradeByID(f.ctx, tr.ID)
	if after == nil || after.Quantity != 3 || after.Status != "exiting" {
		t.Fatalf("trade = %+v, want 3 shares exiting", after)
	}
}
