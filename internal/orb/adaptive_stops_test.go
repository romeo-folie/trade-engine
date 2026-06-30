package orb

import (
	"context"
	"math"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const eps = 1e-9

// adaptiveStop: break-even moves the stop to the entry fill once the favourable excursion
// reaches break_even_r; it never widens; it is a no-op until the threshold is met.
func TestAdaptiveStop_BreakEven(t *testing.T) {
	inst := config.Instrument{BreakEvenR: 1.0}
	entry, stop, R := 1.1012, 1.0998, 0.0014 // 1R below entry

	// Not yet at +1R favourable → no move.
	if got, tighter := adaptiveStop("buy", entry, stop, R, 1.1015, 0, inst); tighter || got != stop {
		t.Errorf("below threshold: got=%v tighter=%v; want unchanged %v/false", got, tighter, stop)
	}
	// At +1R favourable (hwm = entry+R) → stop ratchets to entry.
	got, tighter := adaptiveStop("buy", entry, stop, R, entry+R, 0, inst)
	if !tighter || math.Abs(got-entry) > eps {
		t.Errorf("at +1R: got=%v tighter=%v; want %v/true", got, tighter, entry)
	}
}

// Mirror image for a short: break-even drops the stop down to the entry fill.
func TestAdaptiveStop_BreakEven_Short(t *testing.T) {
	inst := config.Instrument{BreakEvenR: 1.0}
	entry, stop, R := 1.0998, 1.1012, 0.0014 // 1R above entry
	got, tighter := adaptiveStop("sell", entry, stop, R, entry-R, 0, inst)
	if !tighter || math.Abs(got-entry) > eps {
		t.Errorf("short at +1R: got=%v tighter=%v; want %v/true", got, tighter, entry)
	}
}

// Trailing rides the stop up to hwm − mult×ATR once trail_after_r is met, only when that
// is tighter than the current stop.
func TestAdaptiveStop_Trailing(t *testing.T) {
	inst := config.Instrument{TrailATRMult: 1.0, TrailAfterR: 0}
	entry, stop, R, atr := 1.1012, 1.0998, 0.0014, 0.0010
	hwm := 1.1040
	got, tighter := adaptiveStop("buy", entry, stop, R, hwm, atr, inst)
	want := hwm - inst.TrailATRMult*atr // 1.1030
	if !tighter || math.Abs(got-want) > eps {
		t.Errorf("trail: got=%v tighter=%v; want %v/true", got, tighter, want)
	}
	// A candidate looser than the current stop must not widen it.
	if got, tighter := adaptiveStop("buy", entry, 1.1035, R, hwm, atr, inst); tighter || got != 1.1035 {
		t.Errorf("trail must not widen: got=%v tighter=%v; want 1.1035/false", got, tighter)
	}
}

// R<=0 (degenerate geometry) is a safe no-op rather than a divide-by-zero.
func TestAdaptiveStop_ZeroRiskNoop(t *testing.T) {
	if got, tighter := adaptiveStop("buy", 1.1, 1.09, 0, 1.2, 0.001, config.Instrument{BreakEvenR: 1}); tighter || got != 1.09 {
		t.Errorf("R<=0: got=%v tighter=%v; want 1.09/false", got, tighter)
	}
}

func TestHighWaterMark(t *testing.T) {
	candles := []data.Candle{
		{High: 1.1020, Low: 1.1005},
		{High: 1.1045, Low: 1.1010}, // peak high here
		{High: 1.1030, Low: 1.0990}, // trough low here
	}
	if got := highWaterMark("buy", 1.1012, candles); math.Abs(got-1.1045) > eps {
		t.Errorf("buy hwm = %v; want 1.1045", got)
	}
	if got := highWaterMark("sell", 1.1012, candles); math.Abs(got-1.0990) > eps {
		t.Errorf("sell hwm = %v; want 1.0990", got)
	}
	// No candle beats the entry → hwm stays at the entry fill.
	if got := highWaterMark("buy", 1.2000, candles); math.Abs(got-1.2000) > eps {
		t.Errorf("entry-only hwm = %v; want 1.2000", got)
	}
}

// moveStopLeg resolves the live stop-loss leg by symbol/side/price and replaces it,
// ignoring the take-profit leg (which carries no stop price) and unrelated symbols.
func TestMoveStopLeg_ResolvesAndReplaces(t *testing.T) {
	exec := &flattenExec{working: []execution.WorkingOrder{
		{OrderID: "tp", Ticker: "EUR/USD", Side: "sell", Price: 1.1040},        // TP leg: no StopPrice
		{OrderID: "sl", Ticker: "EUR/USD", Side: "sell", StopPrice: 1.0998},    // SL leg: matches
		{OrderID: "other", Ticker: "GBP/USD", Side: "sell", StopPrice: 1.0998}, // wrong symbol
	}}
	s := &ORBScheduler{executor: exec}
	t0 := store.ORBTrade{Ticker: "EUR/USD", Side: "buy", StopLoss: 1.0998}

	if !s.moveStopLeg(context.Background(), t0, 1.1012) {
		t.Fatal("moveStopLeg returned false; want true")
	}
	if len(exec.replaced) != 1 || exec.replaced[0].orderID != "sl" || math.Abs(exec.replaced[0].newStop-1.1012) > eps {
		t.Errorf("replaced = %+v; want one replace of sl @ 1.1012", exec.replaced)
	}
}

// A short's protective stop is a buy-stop — moveStopLeg must match the opposite side.
func TestMoveStopLeg_ShortMatchesBuyStop(t *testing.T) {
	exec := &flattenExec{working: []execution.WorkingOrder{
		{OrderID: "sl", Ticker: "EUR/USD", Side: "buy", StopPrice: 1.1012},
	}}
	s := &ORBScheduler{executor: exec}
	t0 := store.ORBTrade{Ticker: "EUR/USD", Side: "sell", StopLoss: 1.1012}
	if !s.moveStopLeg(context.Background(), t0, 1.0998) || len(exec.replaced) != 1 {
		t.Errorf("short SL not moved: ok=%v replaced=%+v", len(exec.replaced) == 1, exec.replaced)
	}
}

// No working order matches the stored stop → no replace, returns false (never guesses).
func TestMoveStopLeg_NoMatch(t *testing.T) {
	exec := &flattenExec{working: []execution.WorkingOrder{
		{OrderID: "sl", Ticker: "EUR/USD", Side: "sell", StopPrice: 1.0950}, // price off
	}}
	s := &ORBScheduler{executor: exec}
	t0 := store.ORBTrade{Ticker: "EUR/USD", Side: "buy", StopLoss: 1.0998}
	if s.moveStopLeg(context.Background(), t0, 1.1012) || len(exec.replaced) != 0 {
		t.Errorf("unexpected move: replaced=%+v", exec.replaced)
	}
}

// A dual-sided ORB's pending entry is a sell-stop at the same price as the buy's SL leg.
// moveStopLeg must skip the current-session entry order and only touch the real SL leg.
func TestMoveStopLeg_ProtectsCurrentSessionEntry(t *testing.T) {
	exec := &flattenExec{working: []execution.WorkingOrder{
		{OrderID: "sell-entry", Ticker: "EUR/USD", Side: "sell", StopPrice: 1.0998}, // current-session sell entry — protected
		{OrderID: "sl", Ticker: "EUR/USD", Side: "sell", StopPrice: 1.0998},         // the buy's real SL leg — move this
	}}
	s := &ORBScheduler{
		executor: exec,
		orderIDs: map[string][2]string{"EUR/USD": {"buy-entry", "sell-entry"}},
	}
	t0 := store.ORBTrade{Ticker: "EUR/USD", Side: "buy", StopLoss: 1.0998}
	if !s.moveStopLeg(context.Background(), t0, 1.1012) {
		t.Fatal("moveStopLeg returned false; want true (SL leg should still be found)")
	}
	if len(exec.replaced) != 1 || exec.replaced[0].orderID != "sl" {
		t.Errorf("replaced = %+v; want only [sl] (the pending sell entry must be protected)", exec.replaced)
	}
}

// scaleOutTargetR picks the leg's take-profit R-multiple so the planned risk R is recovered
// correctly: a 1R partial under a 2R strategy must use mult=1 (R = |TP−entry|/1), not the
// flat RR=2 that would halve R and fire break-even/trailing at the wrong prices.
func TestScaleOutTargetR(t *testing.T) {
	inst := config.Instrument{PartialTakeProfitR: 1.0, RunnerTakeProfitR: 3.0}
	const rr = 2.0
	cases := []struct {
		role string
		inst config.Instrument
		want float64
	}{
		{"", inst, rr},                      // ordinary trade → RR
		{"partial", inst, 1.0},              // partial → partial_take_profit_r
		{"runner", inst, 3.0},               // runner → runner_take_profit_r
		{"runner", config.Instrument{}, rr}, // runner, runner_r unset → RR (base target)
		{"partial", config.Instrument{PartialFraction: 0.5}, rr}, // partial_r unset → RR fallback
	}
	for _, c := range cases {
		if got := scaleOutTargetR(c.role, c.inst, rr); got != c.want {
			t.Errorf("scaleOutTargetR(%q) = %v; want %v", c.role, got, c.want)
		}
	}

	// End-to-end: a 1R partial under a 2R strategy. Entry 1.1000, R_actual 0.0050, so the
	// partial TP = 1.1050. Recovered R must be 0.0050 (mult=1), NOT 0.0025 (flat RR=2).
	gotR := 0.0050 / 1.0 // |1.1050-1.1000| / scaleOutTargetR("partial",...) = 0.0050/1
	if r := 0.0050 / scaleOutTargetR("partial", inst, rr); r != gotR {
		t.Errorf("partial R = %v; want %v (mult must be 1, not RR)", r, gotR)
	}
}

// Once a stop has been adaptively moved, the bar-replay backstop disables the stop leg
// (passes stopLoss=0) and replays only the immutable take-profit. This guards against
// fabricating a stop-out at the MOVED level over a pre-move pullback: a long fills at
// 1.1000 with an original stop at 1.0950, pulls back to 1.0970 (never touching the
// original stop), then break-even moves the stop to 1.1000. Replaying the moved stop over
// the early bar would falsely close it; with the stop leg disabled it stays open.
func TestBarReplay_DisabledStopLegDoesNotFabricate(t *testing.T) {
	candles := []data.Candle{
		{High: 1.1010, Low: 1.0970}, // early pullback — below a 1.1000 break-even stop, above the original 1.0950
		{High: 1.1030, Low: 1.1005},
	}
	// Stop leg disabled (0), TP far away (1.1200, unreached) → no exit fabricated.
	if _, _, _, found := ResolveExitFromCandles("buy", 0, 1.1200, candles); found {
		t.Error("disabled stop leg fabricated an exit over a pre-move pullback; want still-open")
	}
	// Sanity: had the moved stop (1.1000) been replayed, the early low (1.0970) WOULD have
	// fabricated a stop-out — confirming the disable is what prevents it.
	if _, _, _, found := ResolveExitFromCandles("buy", 1.1000, 1.1200, candles); !found {
		t.Error("expected the moved-stop replay to (wrongly) fire — test premise is invalid otherwise")
	}
}

// An executor that cannot replace stops (no StopOrderReplacer) is a safe no-op.
func TestMoveStopLeg_UnsupportedExecutor(t *testing.T) {
	s := &ORBScheduler{executor: &stubExecutor{}}
	t0 := store.ORBTrade{Ticker: "EUR/USD", Side: "buy", StopLoss: 1.0998}
	if s.moveStopLeg(context.Background(), t0, 1.1012) {
		t.Error("moveStopLeg should be false for an executor without StopOrderReplacer")
	}
}
