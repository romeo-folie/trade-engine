package options

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
)

var (
	asOf   = time.Date(2026, 7, 13, 14, 0, 0, 0, time.UTC) // Monday
	expFri = time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)  // Friday, DTE 4
)

type fakeChain struct {
	snaps []execution.OptionSnapshot
	err   error
}

func (f fakeChain) GetOptionChain(_ context.Context, _ string, _ execution.OptionChainFilter) ([]execution.OptionSnapshot, error) {
	return f.snaps, f.err
}

type fakeIVR struct {
	rank float64
	ok   bool
}

func (f fakeIVR) IVRank(_ context.Context, _ string, _ time.Time) (float64, bool) {
	return f.rank, f.ok
}

type fakeEarnings struct {
	when time.Time
	has  bool
}

func (f fakeEarnings) NextEarnings(_ context.Context, _ string) (time.Time, bool) {
	return f.when, f.has
}

// snap builds an option snapshot with the given contract terms and quote.
func snap(underlying, right string, strike float64, delta *float64, iv *float64, bid, ask float64, exp time.Time) execution.OptionSnapshot {
	oc := execution.OptionContract{Underlying: underlying, Expiry: exp, Strike: strike, Right: right}
	return execution.OptionSnapshot{OCCSymbol: execution.OCCSymbol(oc), Bid: bid, Ask: ask, Delta: delta, IV: iv}
}

func f64(v float64) *float64 { return &v }

// etfEngine builds an engine where SPY is an ETF (earnings-exempt).
func etfEngine(chain ChainProvider, ivr IVRankProvider) *Engine {
	cfg := DefaultGateConfig()
	cfg.ETFUnderlyings = []string{"SPY", "QQQ"}
	return NewEngine(cfg, chain, ivr, nil)
}

func TestSelect_HappyPath_PicksClosestToBandMid(t *testing.T) {
	chain := fakeChain{snaps: []execution.OptionSnapshot{
		snap("SPY", "C", 551, f64(0.54), nil, 1.20, 1.25, expFri), // |Δ| .54, dist .01 ← winner
		snap("SPY", "C", 549, f64(0.58), nil, 1.40, 1.46, expFri), // |Δ| .58, dist .03
		snap("SPY", "C", 560, f64(0.70), nil, 0.50, 0.52, expFri), // out of band
		snap("SPY", "C", 555, nil, nil, 0.90, 0.95, expFri),       // null delta, skipped
	}}
	dec := etfEngine(chain, fakeIVR{rank: 40, ok: true}).Select(context.Background(), Signal{Underlying: "SPY", Direction: "buy", AsOf: asOf})
	if !dec.Accepted {
		t.Fatalf("expected accept, got abort at %s: %s", dec.Gate, dec.Reason)
	}
	if dec.Contract == nil || dec.Contract.Strike != 551 || dec.Contract.Right != "C" {
		t.Errorf("chosen contract = %+v", dec.Contract)
	}
	if dec.DTE != 4 {
		t.Errorf("DTE = %d; want 4", dec.DTE)
	}
	if dec.Mid != 1.225 {
		t.Errorf("Mid = %v; want 1.225", dec.Mid)
	}
}

func TestSelect_SellPicksPut(t *testing.T) {
	chain := fakeChain{snaps: []execution.OptionSnapshot{
		snap("SPY", "P", 549, f64(-0.55), nil, 1.30, 1.35, expFri),
	}}
	dec := etfEngine(chain, fakeIVR{rank: 30, ok: true}).Select(context.Background(), Signal{Underlying: "SPY", Direction: "sell", AsOf: asOf})
	if !dec.Accepted {
		t.Fatalf("expected accept, got %s: %s", dec.Gate, dec.Reason)
	}
	if dec.Contract.Right != "P" {
		t.Errorf("right = %q; want P", dec.Contract.Right)
	}
}

func TestSelect_IVRankTooHigh(t *testing.T) {
	dec := etfEngine(fakeChain{}, fakeIVR{rank: 75, ok: true}).Select(context.Background(), Signal{Underlying: "SPY", Direction: "buy", AsOf: asOf})
	if dec.Accepted || dec.Gate != GateIVRank {
		t.Fatalf("expected iv_rank abort, got accepted=%v gate=%s", dec.Accepted, dec.Gate)
	}
}

func TestSelect_IVRankUnavailable_FailsClosed(t *testing.T) {
	dec := etfEngine(fakeChain{}, fakeIVR{ok: false}).Select(context.Background(), Signal{Underlying: "SPY", Direction: "buy", AsOf: asOf})
	if dec.Accepted || dec.Gate != GateIVRank {
		t.Fatalf("expected iv_rank fail-closed abort, got accepted=%v gate=%s", dec.Accepted, dec.Gate)
	}
}

func TestSelect_NoContractsInDTEWindow(t *testing.T) {
	// Expiry 60 days out — outside the 5-DTE window.
	far := asOf.AddDate(0, 0, 60)
	chain := fakeChain{snaps: []execution.OptionSnapshot{snap("SPY", "C", 551, f64(0.55), nil, 1.2, 1.25, far)}}
	dec := etfEngine(chain, fakeIVR{rank: 40, ok: true}).Select(context.Background(), Signal{Underlying: "SPY", Direction: "buy", AsOf: asOf})
	if dec.Accepted || dec.Gate != GateExpiry {
		t.Fatalf("expected expiry abort, got accepted=%v gate=%s", dec.Accepted, dec.Gate)
	}
}

func TestSelect_DeltaOutOfBand(t *testing.T) {
	chain := fakeChain{snaps: []execution.OptionSnapshot{
		snap("SPY", "C", 540, f64(0.85), nil, 5.0, 5.1, expFri),  // deep ITM
		snap("SPY", "C", 570, f64(0.20), nil, 0.2, 0.21, expFri), // OTM
	}}
	dec := etfEngine(chain, fakeIVR{rank: 40, ok: true}).Select(context.Background(), Signal{Underlying: "SPY", Direction: "buy", AsOf: asOf})
	if dec.Accepted || dec.Gate != GateDelta {
		t.Fatalf("expected delta abort, got accepted=%v gate=%s", dec.Accepted, dec.Gate)
	}
}

func TestSelect_NullDeltaOnAll_FailsClosed(t *testing.T) {
	chain := fakeChain{snaps: []execution.OptionSnapshot{
		snap("SPY", "C", 551, nil, nil, 1.2, 1.25, expFri),
	}}
	dec := etfEngine(chain, fakeIVR{rank: 40, ok: true}).Select(context.Background(), Signal{Underlying: "SPY", Direction: "buy", AsOf: asOf})
	if dec.Accepted || dec.Gate != GateDelta {
		t.Fatalf("expected delta abort (null greeks), got accepted=%v gate=%s", dec.Accepted, dec.Gate)
	}
}

func TestSelect_SpreadTooWide(t *testing.T) {
	chain := fakeChain{snaps: []execution.OptionSnapshot{
		snap("SPY", "C", 551, f64(0.55), nil, 1.00, 1.20, expFri), // mid 1.10, spread ~18%
	}}
	dec := etfEngine(chain, fakeIVR{rank: 40, ok: true}).Select(context.Background(), Signal{Underlying: "SPY", Direction: "buy", AsOf: asOf})
	if dec.Accepted || dec.Gate != GateSpread {
		t.Fatalf("expected spread abort, got accepted=%v gate=%s", dec.Accepted, dec.Gate)
	}
	if dec.SpreadPct < 5 {
		t.Errorf("expected recorded spread ≥5%%, got %.2f", dec.SpreadPct)
	}
}

// A crossed quote (ask < bid) is bad data and must abort, not slip under the
// spread ceiling via a negative signed spread. Regression for the fail-closed
// spread gate.
func TestSelect_CrossedQuote_FailsClosed(t *testing.T) {
	chain := fakeChain{snaps: []execution.OptionSnapshot{
		snap("SPY", "C", 551, f64(0.55), nil, 1.30, 1.20, expFri), // bid 1.30 > ask 1.20 (crossed)
	}}
	dec := etfEngine(chain, fakeIVR{rank: 40, ok: true}).Select(context.Background(), Signal{Underlying: "SPY", Direction: "buy", AsOf: asOf})
	if dec.Accepted || dec.Gate != GateSpread {
		t.Fatalf("expected spread abort on crossed quote, got accepted=%v gate=%s", dec.Accepted, dec.Gate)
	}
}

func TestSelect_ChainError_FailsClosed(t *testing.T) {
	chain := fakeChain{err: errors.New("boom")}
	dec := etfEngine(chain, fakeIVR{rank: 40, ok: true}).Select(context.Background(), Signal{Underlying: "SPY", Direction: "buy", AsOf: asOf})
	if dec.Accepted || dec.Gate != GateChain || dec.Err == nil {
		t.Fatalf("expected chain_error abort with Err set, got accepted=%v gate=%s err=%v", dec.Accepted, dec.Gate, dec.Err)
	}
}

func TestSelect_UnknownDirection(t *testing.T) {
	dec := etfEngine(fakeChain{}, fakeIVR{rank: 40, ok: true}).Select(context.Background(), Signal{Underlying: "SPY", Direction: "hold", AsOf: asOf})
	if dec.Accepted || dec.Gate != GateInput {
		t.Fatalf("expected input abort, got accepted=%v gate=%s", dec.Accepted, dec.Gate)
	}
}

// ─── earnings gate (single names) ───────────────────────────────────────────

func singleNameEngine(chain ChainProvider, ivr IVRankProvider, earn EarningsProvider) *Engine {
	cfg := DefaultGateConfig()
	cfg.ETFUnderlyings = []string{"SPY", "QQQ"} // NVDA is NOT an ETF
	return NewEngine(cfg, chain, ivr, earn)
}

func TestSelect_EarningsWithinBlackout_Aborts(t *testing.T) {
	chain := fakeChain{snaps: []execution.OptionSnapshot{snap("NVDA", "C", 120, f64(0.55), nil, 2.0, 2.05, expFri)}}
	earn := fakeEarnings{when: asOf.AddDate(0, 0, 2), has: true} // earnings in 2 days, within 3-day blackout
	dec := singleNameEngine(chain, fakeIVR{rank: 40, ok: true}, earn).Select(context.Background(), Signal{Underlying: "NVDA", Direction: "buy", AsOf: asOf})
	if dec.Accepted || dec.Gate != GateEarnings {
		t.Fatalf("expected earnings abort, got accepted=%v gate=%s", dec.Accepted, dec.Gate)
	}
}

func TestSelect_EarningsOutsideBlackout_Passes(t *testing.T) {
	chain := fakeChain{snaps: []execution.OptionSnapshot{snap("NVDA", "C", 120, f64(0.55), nil, 2.0, 2.05, expFri)}}
	earn := fakeEarnings{when: asOf.AddDate(0, 0, 30), has: true} // 30 days out — clear
	dec := singleNameEngine(chain, fakeIVR{rank: 40, ok: true}, earn).Select(context.Background(), Signal{Underlying: "NVDA", Direction: "buy", AsOf: asOf})
	if !dec.Accepted {
		t.Fatalf("expected accept, got %s: %s", dec.Gate, dec.Reason)
	}
}

func TestSelect_SingleNameNilEarningsProvider_FailsClosed(t *testing.T) {
	chain := fakeChain{snaps: []execution.OptionSnapshot{snap("NVDA", "C", 120, f64(0.55), nil, 2.0, 2.05, expFri)}}
	eng := NewEngine(GateConfig{ETFUnderlyings: []string{"SPY"}}, chain, fakeIVR{rank: 40, ok: true}, nil)
	dec := eng.Select(context.Background(), Signal{Underlying: "NVDA", Direction: "buy", AsOf: asOf})
	if dec.Accepted || dec.Gate != GateEarnings {
		t.Fatalf("expected earnings fail-closed abort for single name w/ nil provider, got accepted=%v gate=%s", dec.Accepted, dec.Gate)
	}
}

func TestSelect_ETFSkipsEarnings_EvenWithNilProvider(t *testing.T) {
	chain := fakeChain{snaps: []execution.OptionSnapshot{snap("SPY", "C", 551, f64(0.55), nil, 1.2, 1.25, expFri)}}
	// nil earnings provider, but SPY is an ETF → earnings gate skipped entirely.
	dec := etfEngine(chain, fakeIVR{rank: 40, ok: true}).Select(context.Background(), Signal{Underlying: "SPY", Direction: "buy", AsOf: asOf})
	if !dec.Accepted {
		t.Fatalf("ETF should skip earnings gate; got abort %s: %s", dec.Gate, dec.Reason)
	}
}
