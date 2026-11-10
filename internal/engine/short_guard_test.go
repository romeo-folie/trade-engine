package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// Until short execution exists (#599 PR 3), SHORT/COVER must never reach the
// broker: executeSignal treats every non-SELL action as a BUY, so a missing
// guard would place a long entry for a bearish decision.
func TestExecuteSignalRefusesShortAndCover(t *testing.T) {
	requireEngineDB(t)
	ctx := context.Background()
	for _, action := range []analysis.Action{analysis.ActionShort, analysis.ActionCover} {
		exec := &fakeBracketExec{}
		e := newBracketTestEngine(exec)
		ticker := uniqueTicker("SHG")
		sig := store.Signal{Ticker: ticker, Action: string(action), Strategy: "momentum", Confidence: 0.9,
			SignalPrice: 50, StopLoss: 55, TakeProfit: 40, SizePct: 1.5, Status: "pending",
			ExpiresAt: time.Now().Add(time.Hour)}
		id, err := store.InsertSignal(ctx, sig)
		if err != nil {
			t.Fatalf("InsertSignal(%s): %v", action, err)
		}
		e.executeSignal(ctx, id, sig, false)
		if len(exec.placed) != 0 {
			t.Fatalf("%s placed broker orders: %+v", action, exec.placed)
		}
		got, err := store.GetSignalByID(ctx, id)
		// The reason must be this guard's, not the market-hours guard's:
		// tests may run while the market is closed, which would also block.
		if err != nil || got == nil || got.Status != "blocked" || !strings.Contains(got.BlockReason, "not executable") {
			t.Fatalf("%s signal = %+v (%v), want blocked as non-executable", action, got, err)
		}
	}
}

func TestGenerateSignalCreatesNoSignalForShortOrCover(t *testing.T) {
	requireEngineDB(t)
	ctx := context.Background()
	exec := &fakeBracketExec{}
	e := newBracketTestEngine(exec)
	candles, _ := snapshotTestCandles(80)
	for _, action := range []analysis.Action{analysis.ActionShort, analysis.ActionCover} {
		ticker := uniqueTicker("SHN")
		resp := analysis.AnalysisResponse{Action: action, Confidence: 0.9, SizePct: 1.5, StopLoss: 160, TakeProfit: 120, Reasoning: "bearish"}
		e.generateSignal(ctx, ticker, "1d", "momentum", []string{"momentum"}, candles, resp, nil, "", nil, "test", "", false)
		var n int
		if err := store.GetDB().QueryRow(ctx, `SELECT count(*) FROM signals WHERE ticker = $1`, ticker).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s created %d signals (%v)", action, n, err)
		}
		if len(exec.placed) != 0 {
			t.Fatalf("%s placed broker orders", action)
		}
	}
}
