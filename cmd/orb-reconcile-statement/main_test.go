package main

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestNormalizeSymbol(t *testing.T) {
	cases := map[string]string{
		"EUR.USD": "EUR/USD",
		"GBP.USD": "GBP/USD",
		"XAUUSD":  "XAU/USD",
		"XAGUSD":  "XAG/USD",
	}
	for in, want := range cases {
		if got := normalizeSymbol(in); got != want {
			t.Errorf("normalizeSymbol(%q) = %q; want %q", in, got, want)
		}
	}
}

func mkFill(sym string, signedQty, price, comm float64, t time.Time) *fill {
	sign := 1.0
	if signedQty < 0 {
		sign = -1.0
	}
	q := signedQty
	if q < 0 {
		q = -q
	}
	return &fill{symbol: sym, qty: q, sign: sign, price: price, comm: comm, t: t}
}

func at(h, m int) time.Time { return time.Date(2026, 4, 13, h, m, 0, 0, time.UTC) }

func ptr(tm time.Time) *time.Time { return &tm }

func TestReconcile_SingleBuyRoundTrip(t *testing.T) {
	fills := []*fill{
		mkFill("EUR/USD", 5000, 1.1700, -2, at(10, 0)),
		mkFill("EUR/USD", -5000, 1.1720, -2, at(11, 0)),
	}
	bySym := map[string][]*fill{"EUR/USD": fills}
	trades := []store.ORBTrade{{
		ID: "t1", Ticker: "EUR/USD", Side: "buy", Qty: 5000,
		FillPrice: 1.1700, FilledAt: ptr(at(10, 1)),
	}}
	got := reconcile(trades, bySym)
	if len(got) != 1 || !got[0].ok {
		t.Fatalf("expected 1 matched, got %+v", got)
	}
	if got[0].exitPrice != 1.1720 || got[0].exitType != "tp" {
		t.Errorf("exit = %v %s; want 1.1720 tp", got[0].exitPrice, got[0].exitType)
	}
	if d := got[0].pnl - 10.0; d < -1e-6 || d > 1e-6 {
		t.Errorf("pnl = %v; want +10.00", got[0].pnl)
	}
	if got[0].comm != -4 {
		t.Errorf("comm = %v; want -4", got[0].comm)
	}
}

// Entries-first must keep a short trade's entry fill from being consumed as a prior long
// trade's exit. Without two-pass, the buy's exit-search would grab the -5000 @1.30 that is
// actually the sell trade's entry.
func TestReconcile_NettingCollisionResolvedByTwoPass(t *testing.T) {
	fills := []*fill{
		mkFill("EUR/USD", 5000, 1.1700, -2, at(10, 0)),  // buy entry
		mkFill("EUR/USD", -5000, 1.1720, -2, at(11, 0)), // buy exit
		mkFill("EUR/USD", -5000, 1.3000, -2, at(12, 0)), // sell entry
		mkFill("EUR/USD", 5000, 1.2990, -2, at(13, 0)),  // sell exit
	}
	bySym := map[string][]*fill{"EUR/USD": fills}
	trades := []store.ORBTrade{
		{ID: "buy", Ticker: "EUR/USD", Side: "buy", Qty: 5000, FillPrice: 1.1700, FilledAt: ptr(at(10, 1))},
		{ID: "sell", Ticker: "EUR/USD", Side: "sell", Qty: 5000, FillPrice: 1.3000, FilledAt: ptr(at(12, 1))},
	}
	got := reconcile(trades, bySym)
	byID := map[string]match{}
	for _, m := range got {
		byID[m.t.ID] = m
	}
	if !byID["buy"].ok || byID["buy"].exitPrice != 1.1720 {
		t.Errorf("buy exit = %+v; want 1.1720", byID["buy"])
	}
	if !byID["sell"].ok || byID["sell"].exitPrice != 1.2990 {
		t.Errorf("sell exit = %+v; want 1.2990", byID["sell"])
	}
	// sell is a short: (1.3000-1.2990)*5000 = +5 win.
	if d := byID["sell"].pnl - 5.0; d < -1e-6 || d > 1e-6 {
		t.Errorf("sell pnl = %v; want +5.00", byID["sell"].pnl)
	}
}

func TestReconcile_NoEntryFillUnmatched(t *testing.T) {
	// Statement has only a sell fill; a buy trade finds no same-sign entry.
	fills := []*fill{mkFill("EUR/USD", -5000, 1.1720, -2, at(11, 0))}
	bySym := map[string][]*fill{"EUR/USD": fills}
	trades := []store.ORBTrade{{
		ID: "t1", Ticker: "EUR/USD", Side: "buy", Qty: 5000,
		FillPrice: 1.1700, FilledAt: ptr(at(10, 1)),
	}}
	got := reconcile(trades, bySym)
	if got[0].ok {
		t.Fatalf("expected unmatched, got %+v", got[0])
	}
}
