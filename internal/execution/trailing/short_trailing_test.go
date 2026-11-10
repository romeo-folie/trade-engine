package trailing

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// ─── Pure formulas (#599 PR 4b) ──────────────────────────────────────────────

func TestComputeNewShortStop(t *testing.T) {
	// low 90 + 1.5 × ATR 2 = 93, below the current 105 → ratchets down.
	if got := ComputeNewShortStop(105, 90, 2, 1.5); got != 93 {
		t.Fatalf("ratchet = %v, want 93", got)
	}
	// A candidate above the current stop never loosens it.
	if got := ComputeNewShortStop(92, 90, 2, 1.5); got != 92 {
		t.Fatalf("widened to %v, want 92", got)
	}
	// No current stop: the candidate seeds it.
	if got := ComputeNewShortStop(0, 90, 2, 1.5); got != 93 {
		t.Fatalf("seed = %v, want 93", got)
	}
	// Bad inputs leave the stop unchanged.
	for _, c := range [][3]float64{{0, 2, 1.5}, {90, 0, 1.5}, {90, 2, 0}} {
		if got := ComputeNewShortStop(105, c[0], c[1], c[2]); got != 105 {
			t.Fatalf("bad input %v changed the stop to %v", c, got)
		}
	}
}

func TestComputeNewShortStopMonotonicDown(t *testing.T) {
	stop := 120.0
	for _, low := range []float64{100, 98, 99, 95, 97, 96} {
		next := ComputeNewShortStop(stop, low, 1, 2)
		if next > stop {
			t.Fatalf("short stop rose from %v to %v at low %v", stop, next, low)
		}
		stop = next
	}
}

func TestTrailArmedShort(t *testing.T) {
	if TrailArmedShort(100, 95, 8) {
		t.Fatal("a 5% drop must not arm an 8% gate")
	}
	if !TrailArmedShort(100, 92, 8) {
		t.Fatal("an 8% drop must arm an 8% gate")
	}
	if !TrailArmedShort(100, 100, 0) {
		t.Fatal("a zero gate is always armed")
	}
	if TrailArmedShort(0, 92, 8) || TrailArmedShort(100, 0, 8) {
		t.Fatal("unusable inputs must not arm")
	}
}

// ─── Manager: ATR rule for a short ───────────────────────────────────────────

func shortTrade() store.Trade {
	return store.Trade{
		ID: "s1", Ticker: "TSLA", Direction: "short", Status: "open",
		EntryPrice: 100, StopLoss: 105, TakeProfit: 80, Quantity: 10,
	}
}

func TestManager_ShortFirstTickSeedsLowWaterMark(t *testing.T) {
	fs := newFakeStore()
	fs.trades["s1"] = shortTrade()
	fp := &fakePrices{candles: makeFlatPriceCandles(97)}
	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, &fakeBroker{})
	m.UpdateAll(context.Background())
	// Seed is min(entry 100, price 97), never the entry for a short in profit.
	if got := fs.trades["s1"].HighWaterMark; got != 97 {
		t.Fatalf("low-water mark seed = %v, want 97", got)
	}
}

// Mirrors TestManager_RatchetsStopOnNewHigh: an uncapped trade (no software
// take-profit) with no broker stop yet gets a standalone protective stop.
// Capped shorts carry the bracket's stop leg, which is PATCHed instead.
func TestManager_ShortRatchetsStopDownOnNewLow(t *testing.T) {
	fs := newFakeStore()
	tr := shortTrade()
	tr.HighWaterMark, tr.TakeProfit = 100, 0
	fs.trades["s1"] = tr
	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(92, 93, 90, 90))}
	fb := &fakeBroker{}
	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fb)
	m.UpdateAll(context.Background())

	got := fs.trades["s1"]
	if got.HighWaterMark != 90 {
		t.Fatalf("low-water mark = %v, want 90", got.HighWaterMark)
	}
	if got.StopLoss >= 105 || got.StopLoss <= 90 {
		t.Fatalf("short stop = %v, want ratcheted down from 105 and still above price 90", got.StopLoss)
	}
	if len(fb.placed) != 1 || fb.placed[0].Side != execution.OrderSideBuy || fb.placed[0].Type != execution.OrderTypeStop {
		t.Fatalf("placed = %+v, want one BUY stop", fb.placed)
	}
	if fb.placed[0].StopPrice != got.StopLoss || fb.placed[0].Quantity != 10 {
		t.Fatalf("stop order %+v does not match the trade stop %v", fb.placed[0], got.StopLoss)
	}
}

func TestManager_ShortStopNeverRisesOnRally(t *testing.T) {
	fs := newFakeStore()
	tr := shortTrade()
	tr.HighWaterMark = 100
	fs.trades["s1"] = tr
	fb := &fakeBroker{}
	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, nil, fb)

	m.prices = &fakePrices{candles: makeFlatPriceCandles(100, candle(92, 93, 90, 90))}
	m.UpdateAll(context.Background())
	afterDrop := fs.trades["s1"].StopLoss
	if afterDrop >= 105 {
		t.Fatalf("stop did not ratchet down: %v", afterDrop)
	}

	// Price rallies back toward the stop. The low-water mark stays 90 and the
	// stop must not rise.
	m.prices = &fakePrices{candles: makeFlatPriceCandles(100, candle(90, 91.2, 90, 91))}
	m.UpdateAll(context.Background())
	if got := fs.trades["s1"]; got.StopLoss > afterDrop || got.HighWaterMark != 90 {
		t.Fatalf("after rally stop=%v (was %v) low=%v; a short stop must never rise", got.StopLoss, afterDrop, got.HighWaterMark)
	}
}

// The mandatory short bracket's stop leg is a BUY child: it is PATCHed in
// place, never cancelled and resubmitted.
func TestManager_ShortReplacesBracketStopLegViaPATCH(t *testing.T) {
	fs := newFakeStore()
	tr := shortTrade()
	tr.HighWaterMark = 100
	fs.trades["s1"] = tr
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: "s1", BrokerOrderID: "short-sl", Ticker: "TSLA",
		Side: string(execution.OrderSideBuy), OrderType: string(execution.OrderTypeStop),
		Quantity: 10, StopPrice: 105, Status: "pending", IsStopLoss: true, ParentOrderID: "short-parent",
	})
	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(92, 93, 90, 90))}
	base := &fakeBroker{}
	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5}, fs, fp, fakeBrokerWithReplace{fakeBroker: base})
	m.UpdateAll(context.Background())
	if len(base.replaced) != 1 || base.replaced[0].BrokerOrderID != "short-sl" || base.replaced[0].NewStopPrice >= 105 {
		t.Fatalf("replaced = %+v, want one PATCH of the short stop leg below 105", base.replaced)
	}
	if len(base.placed) != 0 || len(base.cancelled) != 0 {
		t.Fatalf("placed=%v cancelled=%v; a bracket leg must be PATCHed, never cancelled", base.placed, base.cancelled)
	}
}

// A trail already at or below price is through the market: cover the full
// held short at market with a BUY, and close it with short P&L.
func TestManager_ShortCrossedTrailCoversAtMarket(t *testing.T) {
	fs := newFakeStore()
	tr := shortTrade()
	tr.StopLoss, tr.HighWaterMark = 61, 50 // low 50 + 0.1 × ATR ≈ 50.1 ≤ price 60
	fs.trades["s1"] = tr
	fp := &fakePrices{candles: makeFlatPriceCandles(60, candle(60, 60.5, 59.5, 60))}
	fb := &fakeBroker{nextFillPrice: 60, nextFillQty: 10, nextStatus: "filled"}
	m := NewManager(Config{Enabled: true, ATRMultiple: 0.1, MarketOpen: alwaysOpenSession}, fs, fp, fb)
	m.UpdateAll(context.Background())

	if len(fb.placed) != 1 || fb.placed[0].Type != execution.OrderTypeMarket ||
		fb.placed[0].Side != execution.OrderSideBuy || fb.placed[0].Quantity != 10 {
		t.Fatalf("placed = %+v, want one market BUY of the 10 shares short", fb.placed)
	}
	closed := fs.trades["s1"]
	// (100 − 60) × 10 = +400.
	if closed.Status != "closed" || closed.PnL != 400 || closed.ExitReason != store.ExitReasonTrailingATR {
		t.Fatalf("closed = %+v, want closed +400 trailing_atr", closed)
	}
	for _, o := range fs.orders {
		if o.TradeID == "s1" && o.OrderType == string(execution.OrderTypeMarket) && o.Side != string(execution.OrderSideBuy) {
			t.Fatalf("recorded cover order side = %s, want buy", o.Side)
		}
	}
	// A second tick must not cover again.
	before := len(fb.placed)
	m.UpdateAll(context.Background())
	if len(fb.placed) != before {
		t.Fatal("closed short covered twice")
	}
}

// Before the peak-gain gate, the trail leaves the original bracket stop alone.
func TestManager_ShortDisarmedBelowGainGate(t *testing.T) {
	fs := newFakeStore()
	tr := shortTrade()
	tr.HighWaterMark = 100
	fs.trades["s1"] = tr
	fp := &fakePrices{candles: makeFlatPriceCandles(100, candle(97, 98, 97, 97))} // 3% gain
	fb := &fakeBroker{}
	m := NewManager(Config{Enabled: true, ATRMultiple: 1.5, ActivateAtGainPct: 8}, fs, fp, fb)
	m.UpdateAll(context.Background())
	if got := fs.trades["s1"]; got.StopLoss != 105 || got.HighWaterMark != 97 || len(fb.placed) != 0 {
		t.Fatalf("disarmed short: stop=%v low=%v placed=%v; want stop 105 untouched, low 97 recorded, no orders",
			got.StopLoss, got.HighWaterMark, fb.placed)
	}
}

// ─── Manager: EMA9 rule for a short ──────────────────────────────────────────

func TestManager_ShortEMA9CloseAboveCovers(t *testing.T) {
	fs := newFakeStore()
	tr := shortTrade()
	tr.TakeProfit = 0
	fs.trades["s1"] = tr
	_, _ = fs.InsertOrder(context.Background(), store.Order{
		TradeID: "s1", BrokerOrderID: "hard-stop", Ticker: "TSLA",
		Side: "buy", OrderType: "stop", Quantity: 10, StopPrice: 105, Status: "pending", IsStopLoss: true,
	})
	// Nine closes at 100 seed EMA=100; a final 104 close is above the EMA.
	fp := &fakePrices{candles: dailyCloses(100, 100, 100, 100, 100, 100, 100, 100, 100, 104)}
	fb := &fakeBroker{nextFillPrice: 104, nextFillQty: 10, nextStatus: "filled"}
	m := NewManager(Config{Enabled: true, Rule: RuleEMA9}, fs, fp, fb)
	m.now = func() time.Time { return time.Date(2026, time.July, 24, 10, 0, 0, 0, newYorkLocation) }
	m.UpdateAll(context.Background())
	if fs.trades["s1"].Status != "closed" {
		t.Fatal("a daily close above ema9 must cover the short")
	}
	if len(fb.placed) != 1 || fb.placed[0].Side != execution.OrderSideBuy || fb.placed[0].Type != execution.OrderTypeMarket {
		t.Fatalf("placed = %+v, want one market BUY", fb.placed)
	}
}

func TestManager_ShortEMA9CloseBelowKeepsShort(t *testing.T) {
	fs := newFakeStore()
	tr := shortTrade()
	fs.trades["s1"] = tr
	fp := &fakePrices{candles: dailyCloses(100, 100, 100, 100, 100, 100, 100, 100, 100, 90)}
	fb := &fakeBroker{}
	m := NewManager(Config{Enabled: true, Rule: RuleEMA9}, fs, fp, fb)
	m.now = func() time.Time { return time.Date(2026, time.July, 24, 10, 0, 0, 0, newYorkLocation) }
	m.UpdateAll(context.Background())
	if fs.trades["s1"].Status != "open" || len(fb.placed) != 0 {
		t.Fatalf("a close below ema9 is in the short's favour: status=%s placed=%v", fs.trades["s1"].Status, fb.placed)
	}
}

// ─── Broker truth and stale workers ──────────────────────────────────────────

// A short's protective BUY stop requires a broker-held short; a broker long
// (or nothing) is never "protected" with a buy, which would add to it.
func TestSubmitOrReplaceStopShortRequiresBrokerShort(t *testing.T) {
	for _, tc := range []struct {
		name      string
		positions []execution.BrokerPosition
		wantPlace int
	}{
		{"no broker position", nil, 0},
		{"broker long only", []execution.BrokerPosition{{Symbol: "TSLA", Side: "long", Quantity: 10}}, 0},
		{"broker short", []execution.BrokerPosition{{Symbol: "TSLA", Side: "short", Quantity: -10}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			tr := shortTrade()
			fs.trades[tr.ID] = tr
			broker := &brokerViewFake{fakeBroker: &fakeBroker{}, positions: tc.positions}
			m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, broker)
			if err := m.submitOrReplaceStop(context.Background(), tr, 103, stopExitIntent(103)); err != nil {
				t.Fatalf("submitOrReplaceStop: %v", err)
			}
			broker.mu.Lock()
			defer broker.mu.Unlock()
			if len(broker.placed) != tc.wantPlace {
				t.Fatalf("placed %d, want %d", len(broker.placed), tc.wantPlace)
			}
			if tc.wantPlace == 1 && (broker.placed[0].Side != execution.OrderSideBuy || broker.placed[0].StopPrice != 103) {
				t.Fatalf("short protective stop = %+v, want BUY stop 103", broker.placed[0])
			}
		})
	}
}

// A delayed worker can never raise a short's durable stop or lose its low.
func TestPersistTrailingStateShortMergesTowardTighter(t *testing.T) {
	fs := newFakeStore()
	tr := shortTrade()
	tr.StopLoss, tr.HighWaterMark = 95, 88 // a newer tick already tightened
	fs.trades[tr.ID] = tr
	m := NewManager(Config{Enabled: true}, fs, &fakePrices{}, &fakeBroker{})
	stale := shortTrade() // the stale worker's snapshot
	if err := m.persistTrailingState(context.Background(), stale, 99, 92); err != nil {
		t.Fatal(err)
	}
	if got := fs.trades[tr.ID]; got.StopLoss != 95 || got.HighWaterMark != 88 {
		t.Fatalf("stale merge = stop %v low %v, want 95 and 88 preserved", got.StopLoss, got.HighWaterMark)
	}
}
