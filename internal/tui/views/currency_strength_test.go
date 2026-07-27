package views

import (
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestRenderCurrencyStrengthViewShowsDedicatedTabAndDecision(t *testing.T) {
	pnl := 125.50
	data := CurrencyStrengthViewData{
		LastUpdate: time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC),
		Runs: []store.CurrencyStrengthRun{{
			SessionDate:       time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
			Status:            "exited",
			Timezone:          "Europe/London",
			StrengthStart:     "06:00",
			StrengthEnd:       "09:00",
			ExitTime:          "18:00",
			TradeInstruments:  []string{"GBP/USD", "USD/JPY"},
			Units:             100000,
			StrongestCurrency: "GBP",
			WeakestCurrency:   "USD",
			StrengthGap:       0.0123,
			SignalSymbol:      "GBP/USD",
			SignalSide:        "buy",
			EntryOrderID:      "ENTRY-1",
			EntryStatus:       "filled",
			ExitOrderID:       "EXIT-1",
			ExitStatus:        "filled",
			RealizedPnL:       &pnl,
		}},
	}

	out := RenderCurrencyStrengthView(data, 120, 40, 0)
	for _, want := range []string{
		"[o] ORB",
		"[c] Currency Strength",
		"GBP strongest",
		"USD weakest",
		"BUY GBP/USD",
		"ENTRY-1",
		"EXIT-1",
		"PnL:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("currency-strength view missing %q:\n%s", want, out)
		}
	}
}

func TestRenderCurrencyStrengthViewEmptyExplainsPersistence(t *testing.T) {
	out := RenderCurrencyStrengthView(CurrencyStrengthViewData{Page: 2}, 100, 20, 0)
	if !strings.Contains(out, "page 3") ||
		!strings.Contains(out, "No currency-strength sessions recorded") {
		t.Fatalf("unexpected empty currency-strength page:\n%s", out)
	}
}

func TestRenderCurrencyStrengthViewSurfacesHistoryFailure(t *testing.T) {
	out := RenderCurrencyStrengthView(
		CurrencyStrengthViewData{Err: "relation does not exist"}, 100, 20, 0,
	)
	if !strings.Contains(out, "history unavailable") ||
		!strings.Contains(out, "relation does not exist") {
		t.Fatalf("history failure was hidden:\n%s", out)
	}
}

func TestRenderCurrencyStrengthFailedEntryShowsPartialFillExposure(t *testing.T) {
	out := RenderCurrencyStrengthView(CurrencyStrengthViewData{
		Runs: []store.CurrencyStrengthRun{{
			SessionDate:    time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
			Status:         "entry_failed",
			SignalSymbol:   "GBP/USD",
			SignalSide:     "buy",
			EntryOrderID:   "ENTRY-PARTIAL",
			EntryStatus:    "cancelled",
			EntryFillPrice: 1.101,
			EntryFilledQty: 40000,
		}},
	}, 120, 30, 0)
	for _, want := range []string{
		"BUY GBP/USD",
		"ENTRY-PARTIAL",
		"fill 1.10100 × 40000",
		"broker exposure requires reconciliation",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("failed entry view missing %q:\n%s", want, out)
		}
	}
}

func TestCurrencyStrengthViewWindowingReachesOlderRuns(t *testing.T) {
	data := CurrencyStrengthViewData{LastUpdate: time.Now()}
	for i := 0; i < CurrencyStrengthPageSize; i++ {
		data.Runs = append(data.Runs, store.CurrencyStrengthRun{
			SessionDate:   time.Date(2026, 7, 27-i, 0, 0, 0, 0, time.UTC),
			Status:        "no_signal",
			Timezone:      "UTC",
			StrengthStart: "06:00",
			StrengthEnd:   "09:00",
			ExitTime:      "18:00",
			Units:         100000,
			SkipReason:    "test",
		})
	}
	const width, height = 100, 12
	maxScroll := CurrencyStrengthMaxScroll(data, width, height)
	if maxScroll <= 0 {
		t.Fatal("expected currency-strength history to require scrolling")
	}
	out := RenderCurrencyStrengthView(data, width, height, maxScroll)
	if !strings.Contains(out, "2026-07-20") {
		t.Fatalf("oldest run is not reachable at max scroll:\n%s", out)
	}
}
