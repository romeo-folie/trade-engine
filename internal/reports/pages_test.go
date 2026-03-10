package reports

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestInitialCashFromSnapshot(t *testing.T) {
	if got := initialCashFromSnapshot(nil); got != 100000 {
		t.Fatalf("nil snapshot: got %v want 100000", got)
	}
	if got := initialCashFromSnapshot(json.RawMessage(`{"backtest":{"initial_cash":250000}}`)); got != 250000 {
		t.Fatalf("valid snapshot: got %v want 250000", got)
	}
	if got := initialCashFromSnapshot(json.RawMessage(`{"backtest":{"initial_cash":0}}`)); got != 100000 {
		t.Fatalf("zero snapshot fallback: got %v want 100000", got)
	}
	if got := initialCashFromSnapshot(json.RawMessage(`not-json`)); got != 100000 {
		t.Fatalf("invalid json fallback: got %v want 100000", got)
	}
}

func TestWinRateAndFmtTokens(t *testing.T) {
	if got := winRateStr(0, 0); got != "—" {
		t.Fatalf("winRateStr(0,0)=%q want em dash", got)
	}
	if got := winRateStr(3, 1); got != "75.0% (3/4)" {
		t.Fatalf("winRateStr(3,1)=%q want 75.0%% (3/4)", got)
	}
	if got := fmtTokens(1234); got != "1.2K" {
		t.Fatalf("fmtTokens(1234)=%q want 1.2K", got)
	}
}

func TestBuildDailyPage_NoSummaryIncludesNote(t *testing.T) {
	body, page, err := BuildDailyPage(DailyData{Date: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("BuildDailyPage error: %v", err)
	}
	if page == nil {
		t.Fatal("BuildDailyPage returned nil page")
	}
	if !strings.Contains(body, "No summary data for this date") {
		t.Fatalf("body missing no-summary note: %s", body)
	}
}

func TestBuildBacktestPageAndComparePage(t *testing.T) {
	runA := store.BacktestRun{
		Label:          "BT-00001",
		Ticker:         "AAPL",
		Strategy:       "momentum",
		ModelProvider:  "anthropic",
		ModelName:      "claude",
		FromDate:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ToDate:         time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
		ConfigSnapshot: json.RawMessage(`{"backtest":{"initial_cash":100000}}`),
	}
	runB := runA
	runB.Label = "BT-00002"
	runB.Ticker = "MSFT"

	tradesA := []store.BacktestTrade{{EntryTS: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), ExitTS: ptrTime(time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)), PnL: ptrF(100)}}
	tradesB := []store.BacktestTrade{{EntryTS: time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC), ExitTS: ptrTime(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)), PnL: ptrF(-50)}}

	body, page, err := BuildBacktestPage(BacktestData{Run: runA, Trades: tradesA})
	if err != nil || page == nil {
		t.Fatalf("BuildBacktestPage failed: page=%v err=%v", page, err)
	}
	if !strings.Contains(body, "Backtest Report") || !strings.Contains(body, runA.Label) {
		t.Fatalf("unexpected backtest body: %s", body)
	}

	cmpBody, cmpPage, err := BuildBacktestComparePage(
		BacktestData{Run: runA, Trades: tradesA},
		BacktestData{Run: runB, Trades: tradesB},
	)
	if err != nil || cmpPage == nil {
		t.Fatalf("BuildBacktestComparePage failed: page=%v err=%v", cmpPage, err)
	}
	if !strings.Contains(cmpBody, "Backtest Comparison") || !strings.Contains(cmpBody, runA.Label) || !strings.Contains(cmpBody, runB.Label) {
		t.Fatalf("unexpected compare body: %s", cmpBody)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
func ptrF(v float64) *float64       { return &v }
