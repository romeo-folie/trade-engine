package orb

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestDecideOptionExit(t *testing.T) {
	base := OptionExitInputs{
		Side: "buy", UnderlyingStop: 548, UnderlyingTarget: 555, CurrentUnderlying: 551,
		EntryOptionPrice: 1.25, TimeExitMinutes: 8,
	}
	cases := []struct {
		name   string
		mutate func(*OptionExitInputs)
		want   string
		exit   bool
	}{
		{"buy stop cross", func(i *OptionExitInputs) { i.CurrentUnderlying = 547.9 }, optExitStop, true},
		{"buy target cross", func(i *OptionExitInputs) { i.CurrentUnderlying = 555.1 }, optExitTarget, true},
		{"buy hold in range", func(i *OptionExitInputs) {}, "", false},
		{"sell stop cross", func(i *OptionExitInputs) {
			i.Side = "sell"
			i.UnderlyingStop = 555
			i.UnderlyingTarget = 548
			i.CurrentUnderlying = 555.1
		}, optExitStop, true},
		{"sell target cross", func(i *OptionExitInputs) {
			i.Side = "sell"
			i.UnderlyingStop = 555
			i.UnderlyingTarget = 548
			i.CurrentUnderlying = 547.9
		}, optExitTarget, true},
		{"time exit not in profit", func(i *OptionExitInputs) {
			i.HeldMinutes = 10
			i.OptionPriceKnown = true
			i.CurrentOptionPrice = 1.00 // below entry 1.25
		}, optExitTime, true},
		{"time exit skipped when in profit", func(i *OptionExitInputs) {
			i.HeldMinutes = 10
			i.OptionPriceKnown = true
			i.CurrentOptionPrice = 1.50 // in profit -> hold
		}, "", false},
		{"time exit skipped when option price unknown", func(i *OptionExitInputs) {
			i.HeldMinutes = 10
			i.OptionPriceKnown = false
		}, "", false},
		{"eod flatten", func(i *OptionExitInputs) { i.SessionEnding = true }, optExitEOD, true},
		{"stop beats eod (priority)", func(i *OptionExitInputs) {
			i.CurrentUnderlying = 547.9
			i.SessionEnding = true
		}, optExitStop, true},
		{"eod flattens a profitable in-window position", func(i *OptionExitInputs) {
			i.HeldMinutes = 2
			i.OptionPriceKnown = true
			i.CurrentOptionPrice = 2.00
			i.SessionEnding = true
		}, optExitEOD, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			reason, exit := decideOptionExit(in)
			if exit != tc.exit || reason != tc.want {
				t.Errorf("decideOptionExit = (%q,%v); want (%q,%v)", reason, exit, tc.want, tc.exit)
			}
		})
	}
}

type fakePricer struct {
	px float64
	ok bool
}

func (f fakePricer) UnderlyingPrice(_ context.Context, _ string) (float64, bool) { return f.px, f.ok }

type fakeQuoter struct {
	mid float64
	ok  bool
}

func (f fakeQuoter) OptionMid(_ context.Context, _, _ string) (float64, bool) { return f.mid, f.ok }

func optTrade() store.ORBTrade {
	filled := time.Date(2026, 7, 13, 14, 0, 0, 0, time.UTC)
	exp := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	return store.ORBTrade{
		ID: "t1", Ticker: "SPY", Side: "buy", OrderID: "e1",
		StopLoss: 548, TakeProfit: 555, FillPrice: 1.25, Qty: 4,
		AssetClass: "option", Underlying: "SPY", Expiry: &exp, Strike: 551,
		Right: "C", OCCSymbol: "SPY260717C00551000", Multiplier: 100, FilledAt: &filled,
	}
}

func TestOptionExitManager_Plan(t *testing.T) {
	now := time.Date(2026, 7, 13, 14, 20, 0, 0, time.UTC) // 20 min after fill
	m := NewOptionExitManager(fakePricer{px: 547.5, ok: true}, fakeQuoter{mid: 1.5, ok: true}, &rPlacer{}, 8)
	if reason, exit := m.plan(context.Background(), optTrade(), now, false); !exit || reason != optExitStop {
		t.Errorf("plan stop = (%q,%v); want stop,true", reason, exit)
	}

	// Underlying price unavailable, not session-ending -> hold (don't act on missing data).
	mNoPx := NewOptionExitManager(fakePricer{ok: false}, nil, &rPlacer{}, 8)
	if _, exit := mNoPx.plan(context.Background(), optTrade(), now, false); exit {
		t.Error("plan should hold when underlying price unavailable and not session-ending")
	}
	// Same, but session ending -> flatten anyway.
	if reason, exit := mNoPx.plan(context.Background(), optTrade(), now, true); !exit || reason != optExitEOD {
		t.Errorf("plan eod-on-missing-price = (%q,%v); want eod,true", reason, exit)
	}
}

func TestOptionExitManager_CloseOption_PriceFallback(t *testing.T) {
	tr := optTrade()

	// 1) Broker returns a fill price -> used directly; verify it's a SELL market order.
	p1 := &rPlacer{res: execution.OrderResult{FilledPrice: 2.10}}
	m1 := NewOptionExitManager(fakePricer{ok: true}, nil, p1, 8)
	px, err := m1.closeOption(context.Background(), tr)
	if err != nil || px != 2.10 {
		t.Fatalf("closeOption price = %v (err %v); want 2.10", px, err)
	}
	if p1.got.Side != execution.OrderSideSell || p1.got.Type != execution.OrderTypeMarket ||
		p1.got.Option == nil || p1.got.Option.OCCSymbol != tr.OCCSymbol || p1.got.Quantity != 4 {
		t.Errorf("close order = %+v; want SELL market x4 of the OCC contract", p1.got)
	}

	// 2) Broker fill 0, quoter has a mid -> falls back to the mid.
	p2 := &rPlacer{res: execution.OrderResult{FilledPrice: 0}}
	m2 := NewOptionExitManager(fakePricer{ok: true}, fakeQuoter{mid: 1.90, ok: true}, p2, 8)
	if px, _ := m2.closeOption(context.Background(), tr); px != 1.90 {
		t.Errorf("fallback to mid = %v; want 1.90", px)
	}

	// 3) Broker fill 0, no quoter -> last-resort entry price (P&L 0), reconciled later.
	p3 := &rPlacer{res: execution.OrderResult{FilledPrice: 0}}
	m3 := NewOptionExitManager(fakePricer{ok: true}, nil, p3, 8)
	if px, _ := m3.closeOption(context.Background(), tr); px != tr.FillPrice {
		t.Errorf("last-resort exit price = %v; want entry %v", px, tr.FillPrice)
	}
}

func TestOptionExitManager_EstimateExitPrice(t *testing.T) {
	tr := optTrade() // FillPrice 1.25
	// Quoter with a live mid -> use it.
	m1 := NewOptionExitManager(fakePricer{ok: true}, fakeQuoter{mid: 1.80, ok: true}, &rPlacer{}, 8)
	if px := m1.estimateExitPrice(context.Background(), tr); px != 1.80 {
		t.Errorf("estimate with quoter = %v; want 1.80", px)
	}
	// Quoter unavailable -> entry price.
	m2 := NewOptionExitManager(fakePricer{ok: true}, fakeQuoter{ok: false}, &rPlacer{}, 8)
	if px := m2.estimateExitPrice(context.Background(), tr); px != tr.FillPrice {
		t.Errorf("estimate without quote = %v; want entry %v", px, tr.FillPrice)
	}
	// Nil quoter -> entry price.
	m3 := NewOptionExitManager(fakePricer{ok: true}, nil, &rPlacer{}, 8)
	if px := m3.estimateExitPrice(context.Background(), tr); px != tr.FillPrice {
		t.Errorf("estimate nil quoter = %v; want entry %v", px, tr.FillPrice)
	}
}

func TestOptionsSessionEnding(t *testing.T) {
	// 15:49 ET -> not ending; 15:50 ET -> ending.
	before := time.Date(2026, 7, 13, 15, 49, 0, 0, nyLoc)
	after := time.Date(2026, 7, 13, 15, 51, 0, 0, nyLoc)
	if optionsSessionEnding(before) {
		t.Error("15:49 ET should not be session-ending")
	}
	if !optionsSessionEnding(after) {
		t.Error("15:51 ET should be session-ending")
	}
}
