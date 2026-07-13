package optionsreplay

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/options"
)

var (
	asOf    = time.Date(2026, 7, 13, 14, 0, 0, 0, time.UTC) // Monday
	expFri  = time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)  // DTE 4
	sessEnd = time.Date(2026, 7, 13, 19, 50, 0, 0, time.UTC)
)

type fakeChain struct {
	snaps []execution.OptionSnapshot
	err   error
}

func (c fakeChain) GetOptionChain(_ context.Context, _ string, _ execution.OptionChainFilter) ([]execution.OptionSnapshot, error) {
	return c.snaps, c.err
}

type fakeIVR struct {
	rank float64
	ok   bool
}

func (r fakeIVR) IVRank(_ context.Context, _ string, _ time.Time) (float64, bool) {
	return r.rank, r.ok
}

func callSnap(strike, delta, bid, ask float64) execution.OptionSnapshot {
	d := delta
	oc := execution.OptionContract{Underlying: "SPY", Expiry: expFri, Strike: strike, Right: "C"}
	return execution.OptionSnapshot{OCCSymbol: execution.OCCSymbol(oc), Bid: bid, Ask: ask, Delta: &d}
}

// engine that accepts a near-ATM SPY call (SPY is an ETF → earnings-exempt).
func acceptingEngine(cfg Config) (*Engine, *options.Engine) {
	gcfg := options.DefaultGateConfig()
	gcfg.ETFUnderlyings = []string{"SPY"}
	gates := options.NewEngine(gcfg,
		fakeChain{snaps: []execution.OptionSnapshot{callSnap(551, 0.55, 1.22, 1.28)}}, // mid 1.25, spread 4.8%
		fakeIVR{rank: 40, ok: true}, nil)
	return NewEngine(gates, cfg), gates
}

func buySignal(path []Bar) Signal {
	return Signal{
		Underlying: "SPY", Direction: "buy", AsOf: asOf,
		UnderlyingStop: 548, UnderlyingTarget: 555, SessionEnd: sessEnd, Path: path,
	}
}

// TestReplay_ExitModeledSpread: the modeled spread costs the EXIT (market sell →
// bid = mid − half-spread) but NOT the entry (limit at mid).
func TestReplay_ExitModeledSpread(t *testing.T) {
	// 10% modeled spread → exit fills 5% below the option mid.
	eng, _ := acceptingEngine(Config{PremiumBudget: 500, ModeledSpreadPct: 0.10})
	// Target cross; option mid at exit = 2.00.
	sim := eng.Replay(context.Background(), buySignal([]Bar{
		{Time: asOf.Add(3 * time.Minute), Underlying: 555.1, OptionMid: 2.00},
	}))
	if !sim.Accepted || sim.ExitReason != "tp" {
		t.Fatalf("expected accepted tp exit, got accepted=%v reason=%q", sim.Accepted, sim.ExitReason)
	}
	// Exit fills at bid = 2.00 × (1 − 0.05) = 1.90, not the 2.00 mid.
	if sim.ExitPrice != 1.90 {
		t.Errorf("exit price = %.4f; want 1.90 (mid 2.00 − 5%% half-spread)", sim.ExitPrice)
	}
	// Entry is unchanged at the mid (1.25), so the spread only bit the exit.
	if sim.EntryMid != 1.25 {
		t.Errorf("entry mid = %.4f; want 1.25 (no spread on the limit entry)", sim.EntryMid)
	}
}

// TestReplay_ParityWithLiveGate is the headline guarantee: the backtest's accept/
// abort decision and chosen contract are IDENTICAL to a direct call of the live
// gate engine on the same signal — because Replay calls that same Select.
func TestReplay_ParityWithLiveGate(t *testing.T) {
	ctx := context.Background()

	// Accepting case.
	eng, gates := acceptingEngine(Config{PremiumBudget: 500, TimeExitMinutes: 8})
	sig := buySignal([]Bar{{Time: asOf.Add(5 * time.Minute), Underlying: 551, OptionMid: 1.30}})
	live := gates.Select(ctx, options.Signal{Underlying: "SPY", Direction: "buy", AsOf: asOf})
	sim := eng.Replay(ctx, sig)
	if sim.Accepted != live.Accepted {
		t.Fatalf("parity: sim.Accepted=%v vs live.Accepted=%v", sim.Accepted, live.Accepted)
	}
	if live.Accepted && sim.OCCSymbol != live.Contract.OCCSymbol {
		t.Errorf("parity: contract %q vs live %q", sim.OCCSymbol, live.Contract.OCCSymbol)
	}

	// Aborting case: IV rank unavailable → the live gate aborts, and so must Replay,
	// with the SAME gate.
	gcfg := options.DefaultGateConfig()
	gcfg.ETFUnderlyings = []string{"SPY"}
	abortGates := options.NewEngine(gcfg, fakeChain{}, fakeIVR{ok: false}, nil)
	abortEng := NewEngine(abortGates, Config{PremiumBudget: 500})
	liveAbort := abortGates.Select(ctx, options.Signal{Underlying: "SPY", Direction: "buy", AsOf: asOf})
	simAbort := abortEng.Replay(ctx, buySignal(nil))
	if simAbort.Accepted || simAbort.Gate != liveAbort.Gate || liveAbort.Accepted {
		t.Fatalf("parity abort: sim gate=%q vs live gate=%q (accepted sim=%v live=%v)",
			simAbort.Gate, liveAbort.Gate, simAbort.Accepted, liveAbort.Accepted)
	}
	if simAbort.Gate != options.GateIVRank {
		t.Errorf("expected iv_rank abort, got %q", simAbort.Gate)
	}
}

func TestReplay_ExitPaths(t *testing.T) {
	ctx := context.Background()
	eng, _ := acceptingEngine(Config{PremiumBudget: 500, TimeExitMinutes: 8})

	cases := []struct {
		name       string
		path       []Bar
		wantReason string
		wantWin    bool // PnL > 0
	}{
		{
			name:       "stop cross (loss)",
			path:       []Bar{{Time: asOf.Add(3 * time.Minute), Underlying: 547.9, OptionMid: 0.80}}, // < stop 548
			wantReason: "stop", wantWin: false,
		},
		{
			name:       "target cross (win)",
			path:       []Bar{{Time: asOf.Add(3 * time.Minute), Underlying: 555.1, OptionMid: 2.10}}, // > target 555
			wantReason: "tp", wantWin: true,
		},
		{
			name: "time exit (not in profit)",
			path: []Bar{
				{Time: asOf.Add(2 * time.Minute), Underlying: 551, OptionMid: 1.20},
				{Time: asOf.Add(9 * time.Minute), Underlying: 551, OptionMid: 1.10}, // held 9m > 8, mid < entry 1.25
			},
			wantReason: "time_exit", wantWin: false,
		},
		{
			name:       "eod flatten",
			path:       []Bar{{Time: sessEnd.Add(1 * time.Minute), Underlying: 551, OptionMid: 1.40}}, // session ending, in window
			wantReason: "eod", wantWin: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sim := eng.Replay(ctx, buySignal(tc.path))
			if !sim.Accepted {
				t.Fatalf("expected accepted; gate=%s", sim.Gate)
			}
			if sim.ExitReason != tc.wantReason {
				t.Errorf("exit reason = %q; want %q", sim.ExitReason, tc.wantReason)
			}
			if tc.wantWin && sim.PnL <= 0 {
				t.Errorf("expected win, got PnL %.2f", sim.PnL)
			}
			if !tc.wantWin && sim.PnL >= 0 {
				t.Errorf("expected loss, got PnL %.2f", sim.PnL)
			}
			// Sizing: 500 / (1.25*100) = 4 contracts.
			if sim.Contracts != 4 {
				t.Errorf("contracts = %d; want 4", sim.Contracts)
			}
		})
	}
}

func TestReplay_SizingAbort(t *testing.T) {
	eng, _ := acceptingEngine(Config{PremiumBudget: 50}) // < 125/contract
	sim := eng.Replay(context.Background(), buySignal(nil))
	if sim.Accepted || sim.Gate != "sizing" {
		t.Fatalf("expected sizing abort, got accepted=%v gate=%s", sim.Accepted, sim.Gate)
	}
}

func TestAggregate(t *testing.T) {
	trades := []SimTrade{
		{Accepted: true, PriceKnown: true, ExitReason: "tp", PnL: 120},
		{Accepted: true, PriceKnown: true, ExitReason: "stop", PnL: -80},
		{Accepted: true, PriceKnown: true, ExitReason: "eod", PnL: 0},
		// Exit quote missing: a real "stop" reason but P&L fell back to 0. Must NOT be
		// counted as a scratch/win/loss, nor pollute TotalPnL — only Unpriced.
		{Accepted: true, PriceKnown: false, ExitReason: "stop", PnL: 0},
		{Accepted: false, Gate: "iv_rank"},
		{Accepted: false, Gate: "spread"},
		{Accepted: false, Gate: "iv_rank"},
	}
	r := Aggregate(trades)
	if r.Total != 7 || r.Accepted != 4 || r.Wins != 1 || r.Losses != 1 || r.Scratches != 1 || r.Unpriced != 1 {
		t.Errorf("report counts = %+v", r)
	}
	if r.TotalPnL != 40 { // unpriced trade's 0 is excluded, not added
		t.Errorf("total pnl = %v; want 40", r.TotalPnL)
	}
	if r.AbortsByGate["iv_rank"] != 2 || r.AbortsByGate["spread"] != 1 {
		t.Errorf("aborts by gate = %v", r.AbortsByGate)
	}
	if r.ExitsByReason["tp"] != 1 || r.ExitsByReason["stop"] != 2 || r.ExitsByReason["eod"] != 1 {
		t.Errorf("exits by reason = %v", r.ExitsByReason) // both stops counted by reason
	}
	if r.WinRate() != 0.5 { // 1 win / (1 win + 1 loss); scratch + unpriced excluded
		t.Errorf("win rate = %v; want 0.5", r.WinRate())
	}
}

// TestReplay_PathExhausted: a Path that never triggers a live exit (no stop/target
// cross, no time/session exit) is closed under the distinct path_exhausted reason —
// a data-coverage marker — NOT fabricated as an "eod" flatten.
func TestReplay_PathExhausted(t *testing.T) {
	eng, _ := acceptingEngine(Config{PremiumBudget: 500, TimeExitMinutes: 60})
	// Bars stay between stop (548) and target (555), held < 60m, all before sessEnd.
	sig := buySignal([]Bar{
		{Time: asOf.Add(1 * time.Minute), Underlying: 551, OptionMid: 1.30},
		{Time: asOf.Add(2 * time.Minute), Underlying: 552, OptionMid: 1.35},
	})
	sim := eng.Replay(context.Background(), sig)
	if !sim.Accepted {
		t.Fatalf("expected accepted; gate=%s", sim.Gate)
	}
	if sim.ExitReason != ReasonPathExhausted {
		t.Errorf("exit reason = %q; want %q", sim.ExitReason, ReasonPathExhausted)
	}
	// The last bar carried a quote (mid 1.35) so PriceKnown is true and PnL is
	// non-zero — but because no modeled exit fired, Aggregate must keep it OUT of
	// win/loss/TotalPnL and bucket it as Unresolved.
	r := Aggregate([]SimTrade{sim})
	if r.Unresolved != 1 || r.Wins != 0 || r.Losses != 0 || r.Scratches != 0 || r.TotalPnL != 0 {
		t.Errorf("path_exhausted leaked into performance totals: %+v", r)
	}
}

// TestReplay_UnpricedExit: when the option quote is missing at the exit bar, the exit
// still fires off the underlying but P&L falls back to 0 and PriceKnown is false so
// Aggregate can exclude it from the win rate.
func TestReplay_UnpricedExit(t *testing.T) {
	eng, _ := acceptingEngine(Config{PremiumBudget: 500, TimeExitMinutes: 8})
	// Underlying crosses the stop (a real loss) but the option mid is missing (0).
	sig := buySignal([]Bar{{Time: asOf.Add(3 * time.Minute), Underlying: 547.9, OptionMid: 0}})
	sim := eng.Replay(context.Background(), sig)
	if !sim.Accepted {
		t.Fatalf("expected accepted; gate=%s", sim.Gate)
	}
	if sim.ExitReason != "stop" {
		t.Errorf("exit reason = %q; want stop", sim.ExitReason)
	}
	if sim.PriceKnown {
		t.Errorf("expected PriceKnown=false when option mid missing")
	}
	if sim.PnL != 0 {
		t.Errorf("expected fallback PnL 0, got %.2f", sim.PnL)
	}
	if r := Aggregate([]SimTrade{sim}); r.Unpriced != 1 || r.Losses != 0 || r.Wins != 0 || r.Scratches != 0 {
		t.Errorf("unpriced trade miscounted: %+v", r)
	}
}

// TestAggregate_CountsCarriedInPnL: a carried (stale-priced) exit is INCLUDED in
// win/loss/P&L (and flagged Carried); only an entry-side unpriced gap is excluded.
func TestAggregate_CountsCarriedInPnL(t *testing.T) {
	r := Aggregate([]SimTrade{
		{Accepted: true, PriceKnown: true, PriceCarried: true, ExitReason: "eod", PnL: -50}, // carried loss — counted
		{Accepted: true, PriceKnown: true, ExitReason: "tp", PnL: 100},                      // real win
		{Accepted: true, PriceKnown: false, ExitReason: "stop", PnL: 0},                     // entry-side gap — excluded
	})
	if r.Carried != 1 {
		t.Errorf("Carried = %d; want 1", r.Carried)
	}
	if r.Wins != 1 || r.Losses != 1 {
		t.Errorf("carried loss must count in win/loss: W=%d L=%d", r.Wins, r.Losses)
	}
	if r.Unpriced != 1 {
		t.Errorf("entry-side gap must be Unpriced, got %d", r.Unpriced)
	}
	if r.TotalPnL != 50 {
		t.Errorf("TotalPnL = %v; want 50 (100 − 50, carried included)", r.TotalPnL)
	}
}

// TestAggregate_DTEHistogram: every accepted position counts toward DTEByDay at its
// entry DTE — including unpriced/unresolved ones (DTE is fixed at entry) — while
// gate-aborted signals never do.
func TestAggregate_DTEHistogram(t *testing.T) {
	r := Aggregate([]SimTrade{
		{Accepted: true, DTE: 1, PriceKnown: true, ExitReason: "tp", PnL: 100},
		{Accepted: true, DTE: 1, PriceKnown: true, ExitReason: "stop", PnL: -50},
		{Accepted: true, DTE: 3, PriceKnown: false, ExitReason: "eod"},                    // unpriced — still an opened position
		{Accepted: true, DTE: 5, ExitReason: ReasonPathExhausted, PriceKnown: true},       // unresolved — still opened
		{Accepted: false, Gate: "iv_rank"},                                                // aborted — never opened
	})
	if got := r.DTEByDay[1]; got != 2 {
		t.Errorf("DTEByDay[1] = %d; want 2", got)
	}
	if got := r.DTEByDay[3]; got != 1 {
		t.Errorf("DTEByDay[3] = %d; want 1 (unpriced still opened)", got)
	}
	if got := r.DTEByDay[5]; got != 1 {
		t.Errorf("DTEByDay[5] = %d; want 1 (unresolved still opened)", got)
	}
	if _, ok := r.DTEByDay[0]; ok {
		t.Errorf("aborted signal must not appear in the DTE histogram: %v", r.DTEByDay)
	}
}

// TestReplay_CarriedPriceNoTimeExit: a carried (forward-filled) option price must NOT
// drive the IV-crush time exit — live never acts on a stale quote. The position holds
// to a real exit (eod) instead, preserving parity; the exit is still priced.
func TestReplay_CarriedPriceNoTimeExit(t *testing.T) {
	eng, _ := acceptingEngine(Config{PremiumBudget: 500, TimeExitMinutes: 8})
	sim := eng.Replay(context.Background(), buySignal([]Bar{
		// Past the 8-min window, underlying flat (no stop/tp), option below entry (1.25)
		// but CARRIED — a real price here would time-exit; a carried one must not.
		{Time: asOf.Add(9 * time.Minute), Underlying: 551, OptionMid: 1.10, MidCarried: true},
		{Time: sessEnd.Add(1 * time.Minute), Underlying: 551, OptionMid: 1.40}, // eod, real
	}))
	if sim.ExitReason == "time_exit" {
		t.Errorf("time exit fired on a carried (stale) price — must not (breaks live parity): %+v", sim)
	}
	if sim.ExitReason != "eod" {
		t.Errorf("expected eod (a carried bar can't time-exit), got %q", sim.ExitReason)
	}
}
