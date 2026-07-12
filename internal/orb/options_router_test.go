package orb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/options"
)

var (
	routerAsOf = time.Date(2026, 7, 13, 14, 0, 0, 0, time.UTC)
	routerExp  = time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC) // DTE 4
)

type rChain struct {
	snaps []execution.OptionSnapshot
	err   error
}

func (c rChain) GetOptionChain(_ context.Context, _ string, _ execution.OptionChainFilter) ([]execution.OptionSnapshot, error) {
	return c.snaps, c.err
}

type rIVR struct {
	rank float64
	ok   bool
}

func (r rIVR) IVRank(_ context.Context, _ string, _ time.Time) (float64, bool) { return r.rank, r.ok }

type rPlacer struct {
	got   execution.OrderRequest
	res   execution.OrderResult
	err   error
	calls int
}

func (p *rPlacer) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	p.calls++
	p.got = req
	return p.res, p.err
}

func optSnap(underlying, right string, strike float64, delta *float64, bid, ask float64, exp time.Time) execution.OptionSnapshot {
	oc := execution.OptionContract{Underlying: underlying, Expiry: exp, Strike: strike, Right: right}
	return execution.OptionSnapshot{OCCSymbol: execution.OCCSymbol(oc), Bid: bid, Ask: ask, Delta: delta}
}

func fp(v float64) *float64 { return &v }

// engine with SPY as an ETF (earnings-exempt) so a nil earnings provider is fine.
func rEngine(snaps []execution.OptionSnapshot, chainErr error, rank float64, rankOK bool) *options.Engine {
	cfg := options.DefaultGateConfig()
	cfg.ETFUnderlyings = []string{"SPY"}
	return options.NewEngine(cfg, rChain{snaps, chainErr}, rIVR{rank, rankOK}, nil)
}

func TestOptionsRouter_Handles(t *testing.T) {
	r := NewOptionsRouter(rEngine(nil, nil, 40, true), &rPlacer{}, []string{"SPY", "QQQ"}, 500, 0)
	if !r.Handles("spy") || !r.Handles("QQQ") {
		t.Error("should handle configured underlyings (case-insensitive)")
	}
	if r.Handles("AAPL") {
		t.Error("should not handle unconfigured ticker")
	}
	var nilRouter *OptionsRouter
	if nilRouter.Handles("SPY") {
		t.Error("nil router must report Handles=false")
	}
}

func TestOptionsRouter_HappyPath_BuysCall_SizesByPremium(t *testing.T) {
	snaps := []execution.OptionSnapshot{
		optSnap("SPY", "C", 551, fp(0.55), 1.22, 1.28, routerExp), // mid 1.25, spread 4.8%
	}
	placer := &rPlacer{res: execution.OrderResult{BrokerOrderID: "o1", Status: "accepted"}}
	r := NewOptionsRouter(rEngine(snaps, nil, 40, true), placer, []string{"SPY"}, 500, 0)

	res := r.Route(context.Background(), BracketParams{Ticker: "SPY", Side: "buy", EntryStop: 550, StopLoss: 548, TakeProfit: 555, Qty: 10}, routerAsOf)
	if !res.Placed {
		t.Fatalf("expected placed; reason=%s", res.Reason)
	}
	// premium budget 500 / (mid 1.25 × 100) = 4 contracts.
	if res.Contracts != 4 {
		t.Errorf("contracts = %d; want 4", res.Contracts)
	}
	if placer.got.SecType != "OPT" || placer.got.Side != execution.OrderSideBuy || placer.got.Type != execution.OrderTypeLimit {
		t.Errorf("order req = %+v; want OPT/buy/limit", placer.got)
	}
	if placer.got.Option == nil || placer.got.Option.Right != "C" {
		t.Errorf("expected a call contract, got %+v", placer.got.Option)
	}
	if placer.got.LimitPrice != 1.25 || placer.got.Quantity != 4 {
		t.Errorf("limit/qty = %v/%v; want 1.25/4", placer.got.LimitPrice, placer.got.Quantity)
	}
	// The broker order ID must surface on the result — routeOptionsEntry returns it
	// so the scheduler records it into the session's re-entry guard (without which a
	// fresh order would be placed every tick, since options have no broker bracket).
	if res.Order.BrokerOrderID != "o1" {
		t.Errorf("result order id = %q; want o1 (needed for the re-entry guard)", res.Order.BrokerOrderID)
	}
}

func TestOptionsRouter_MaxLossCap(t *testing.T) {
	snaps := []execution.OptionSnapshot{optSnap("SPY", "C", 551, fp(0.55), 1.22, 1.28, routerExp)}
	placer := &rPlacer{res: execution.OrderResult{BrokerOrderID: "o1"}}
	// budget would allow 4, but max loss $300 / $125 = 2 contracts.
	r := NewOptionsRouter(rEngine(snaps, nil, 40, true), placer, []string{"SPY"}, 500, 300)
	res := r.Route(context.Background(), BracketParams{Ticker: "SPY", Side: "buy"}, routerAsOf)
	if !res.Placed || res.Contracts != 2 {
		t.Fatalf("contracts = %d placed=%v; want 2 placed", res.Contracts, res.Placed)
	}
}

func TestOptionsRouter_BudgetTooSmall_NotPlaced(t *testing.T) {
	snaps := []execution.OptionSnapshot{optSnap("SPY", "C", 551, fp(0.55), 1.22, 1.28, routerExp)}
	placer := &rPlacer{}
	r := NewOptionsRouter(rEngine(snaps, nil, 40, true), placer, []string{"SPY"}, 50, 0) // 50 < 125
	res := r.Route(context.Background(), BracketParams{Ticker: "SPY", Side: "buy"}, routerAsOf)
	if res.Placed || placer.calls != 0 {
		t.Fatalf("expected not placed and no order; placed=%v calls=%d", res.Placed, placer.calls)
	}
}

func TestOptionsRouter_GateAbort_NotPlaced(t *testing.T) {
	placer := &rPlacer{}
	// IV rank unavailable -> gate fails closed.
	r := NewOptionsRouter(rEngine(nil, nil, 0, false), placer, []string{"SPY"}, 500, 0)
	res := r.Route(context.Background(), BracketParams{Ticker: "SPY", Side: "buy"}, routerAsOf)
	if res.Placed || placer.calls != 0 {
		t.Fatalf("expected gate abort with no order; placed=%v calls=%d gate=%s", res.Placed, placer.calls, res.Decision.Gate)
	}
	if res.Decision.Gate != options.GateIVRank {
		t.Errorf("gate = %s; want iv_rank", res.Decision.Gate)
	}
}

func TestOptionsRouter_Sell_BuysPut(t *testing.T) {
	snaps := []execution.OptionSnapshot{optSnap("SPY", "P", 549, fp(-0.55), 1.22, 1.28, routerExp)}
	placer := &rPlacer{res: execution.OrderResult{BrokerOrderID: "o1"}}
	r := NewOptionsRouter(rEngine(snaps, nil, 40, true), placer, []string{"SPY"}, 500, 0)
	res := r.Route(context.Background(), BracketParams{Ticker: "SPY", Side: "sell"}, routerAsOf)
	if !res.Placed {
		t.Fatalf("expected placed; reason=%s", res.Reason)
	}
	// Short breakout -> buy a PUT (still a BUY order).
	if placer.got.Side != execution.OrderSideBuy || placer.got.Option.Right != "P" {
		t.Errorf("expected BUY of a put, got side=%v right=%v", placer.got.Side, placer.got.Option.Right)
	}
}

func TestOptionsRouter_BrokerError_NotPlaced(t *testing.T) {
	snaps := []execution.OptionSnapshot{optSnap("SPY", "C", 551, fp(0.55), 1.22, 1.28, routerExp)}
	placer := &rPlacer{err: errors.New("rejected")}
	r := NewOptionsRouter(rEngine(snaps, nil, 40, true), placer, []string{"SPY"}, 500, 0)
	res := r.Route(context.Background(), BracketParams{Ticker: "SPY", Side: "buy"}, routerAsOf)
	if res.Placed {
		t.Fatal("expected not placed on broker error")
	}
}
