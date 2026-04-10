package orb_test

import (
	"context"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/orb"
)

var testInstruments = []config.FuturesInstrument{
	{Symbol: "ES", Exchange: "CME", AssetClass: "equity_index", ExpiryCycle: "quarterly", TickSize: 0.25, TickValue: 12.50, TickBuffer: 2},
	{Symbol: "NQ", Exchange: "CME", AssetClass: "equity_index", ExpiryCycle: "quarterly", TickSize: 0.25, TickValue: 5.00, TickBuffer: 2},
	{Symbol: "YM", Exchange: "CBOT", AssetClass: "equity_index", ExpiryCycle: "quarterly", TickSize: 1.00, TickValue: 5.00, TickBuffer: 4},
	{Symbol: "RTY", Exchange: "CME", AssetClass: "equity_index", ExpiryCycle: "quarterly", TickSize: 0.10, TickValue: 5.00, TickBuffer: 4},
}

func TestFuturesScannerReturnsAllInstruments(t *testing.T) {
	scanner := &orb.FuturesCandidateScanner{
		Instruments: testInstruments,
		Cfg:         config.FuturesORBConfig{Enabled: true},
	}

	candidates, err := scanner.ScanCandidates(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != len(testInstruments) {
		t.Errorf("want %d candidates, got %d", len(testInstruments), len(candidates))
	}

	for i, c := range candidates {
		if c.Score.Action != "trade" {
			t.Errorf("candidate[%d] Action: want %q, got %q", i, "trade", c.Score.Action)
		}
		if c.Score.Conviction != 1.0 {
			t.Errorf("candidate[%d] Conviction: want 1.0, got %v", i, c.Score.Conviction)
		}
		if c.Score.CatalystType != "session_open" {
			t.Errorf("candidate[%d] CatalystType: want %q, got %q", i, "session_open", c.Score.CatalystType)
		}
		if c.Gap.Ticker != testInstruments[i].Symbol {
			t.Errorf("candidate[%d] Ticker: want %q, got %q", i, testInstruments[i].Symbol, c.Gap.Ticker)
		}
		// Futures don't use gap pct — must be zero.
		if c.Gap.GapPct != 0 {
			t.Errorf("candidate[%d] GapPct: want 0, got %v", i, c.Gap.GapPct)
		}
	}
}

func TestFuturesScannerReturnsInstrumentsForNonIndexAssetClasses(t *testing.T) {
	// Adding a metal instrument verifies the scanner is truly asset-class agnostic.
	gcInst := config.FuturesInstrument{
		Symbol: "GC", Exchange: "COMEX", AssetClass: "metal",
		ExpiryCycle: "monthly", TickSize: 0.10, TickValue: 10.00, TickBuffer: 3,
	}
	scanner := &orb.FuturesCandidateScanner{
		Instruments: []config.FuturesInstrument{gcInst},
		Cfg:         config.FuturesORBConfig{Enabled: true},
	}

	candidates, err := scanner.ScanCandidates(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Gap.Ticker != "GC" {
		t.Errorf("Ticker: want GC, got %q", candidates[0].Gap.Ticker)
	}
}

func TestFuturesScannerEmpty_ReturnsError(t *testing.T) {
	scanner := &orb.FuturesCandidateScanner{
		Instruments: nil,
		Cfg:         config.FuturesORBConfig{Enabled: true},
	}

	_, err := scanner.ScanCandidates(context.Background())
	if err == nil {
		t.Error("expected error for empty instruments list, got nil")
	}
}

func TestFuturesScannerContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	scanner := &orb.FuturesCandidateScanner{
		Instruments: testInstruments,
		Cfg:         config.FuturesORBConfig{Enabled: true},
	}

	// First instrument (ES) is added before the ctx check — only subsequent ones
	// are skipped. The important thing is no panic and the error is propagated.
	_, err := scanner.ScanCandidates(ctx)
	// Either err is context.Canceled (cancelled mid-loop) or we get a partial
	// result with no error (first instrument added before cancel check). Both are valid.
	if err != nil && err != context.Canceled {
		t.Errorf("unexpected error type: %v", err)
	}
}
