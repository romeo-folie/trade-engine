package orb

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
)

// --- pure force-watch helpers -------------------------------------------------

func tickers(cs []ScoredCandidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Gap.Ticker
	}
	return out
}

func TestAppendForcedOptionsUnderlyings(t *testing.T) {
	router := NewOptionsRouter(rEngine(nil, nil, 40, true), &rPlacer{}, []string{"SPY", "IWM", "NVDA"}, 500, 0)

	t.Run("injects missing, dedups an organic candidate", func(t *testing.T) {
		// SPY already present as an organic gap candidate; must not be duplicated.
		scored := []ScoredCandidate{{Gap: data.GapCandidate{Ticker: "SPY", GapPct: 3.1}, Score: ORBAnalysisResult{Action: "trade", Conviction: 0.9}}}
		got := appendForcedOptionsUnderlyings(scored, router)
		if len(got) != 3 {
			t.Fatalf("len = %d; want 3 (SPY kept, IWM+NVDA added)", len(got))
		}
		// The organic SPY entry is untouched (conviction preserved, not overwritten by force-watch).
		if got[0].Gap.Ticker != "SPY" || got[0].Score.Conviction != 0.9 {
			t.Errorf("organic SPY entry mutated: %+v", got[0])
		}
	})

	t.Run("nil router is a no-op", func(t *testing.T) {
		var nilRouter *OptionsRouter
		scored := []ScoredCandidate{{Gap: data.GapCandidate{Ticker: "AAPL"}}}
		got := appendForcedOptionsUnderlyings(scored, nilRouter)
		if len(got) != 1 {
			t.Errorf("nil router should not inject anything; len = %d", len(got))
		}
	})
}

func TestEnsureForcedOptionsSelected(t *testing.T) {
	router := NewOptionsRouter(rEngine(nil, nil, 40, true), &rPlacer{}, []string{"SPY", "IWM"}, 500, 0)
	// scored has both forced underlyings; selected was truncated by MaxCandidates and dropped them.
	scored := []ScoredCandidate{
		{Gap: data.GapCandidate{Ticker: "SPY"}, Score: ORBAnalysisResult{Action: "trade"}},
		{Gap: data.GapCandidate{Ticker: "IWM"}, Score: ORBAnalysisResult{Action: "trade"}},
	}
	selected := []ScoredCandidate{} // cap evicted everything

	got := ensureForcedOptionsSelected(selected, scored, router)
	names := tickers(got)
	if len(names) != 2 || !slices.Contains(names, "SPY") || !slices.Contains(names, "IWM") {
		t.Errorf("forced underlyings not restored after cap: %v", names)
	}
}

// --- range-lock-bar exclusion (parity with AssembleSignals watch = bars[rangeEndIdx+1:]) ---

func TestCandlesAfter(t *testing.T) {
	base := time.Date(2026, 7, 14, 13, 45, 0, 0, time.UTC)
	candles := []data.Candle{
		{Timestamp: base},                      // == lock bar → excluded
		{Timestamp: base.Add(-time.Minute)},    // before → excluded
		{Timestamp: base.Add(time.Minute)},     // after → kept
		{Timestamp: base.Add(2 * time.Minute)}, // after → kept
	}
	got := candlesAfter(candles, base)
	if len(got) != 2 {
		t.Fatalf("kept %d; want 2 (strictly after the lock bar)", len(got))
	}
	for _, c := range got {
		if !c.Timestamp.After(base) {
			t.Errorf("kept a candle at/before the lock bar: %s", c.Timestamp)
		}
	}
}

// --- optionsBreakoutEntry (no DB touched on the no-breakout / gate-abort paths) ---

// breakoutScheduler wires a scheduler for optionsBreakoutEntry unit tests: equities
// session (nyLoc, 09:30 open), now() at 10:00 ET so the 10:30 entry deadline is open.
func breakoutScheduler(md data.MarketDataProvider, router *OptionsRouter) *ORBScheduler {
	return &ORBScheduler{
		cfg:              config.ORBConfig{RiskReward: 2},
		marketData:       md,
		optionsRouter:    router,
		orderIDs:         make(map[string][2]string),
		optionsAttempted: make(map[string]bool),
		now:              func() time.Time { return time.Date(2026, 7, 14, 14, 0, 0, 0, time.UTC) }, // 10:00 ET
	}
}

var testRangeDefinedAt = time.Date(2026, 7, 14, 13, 45, 0, 0, time.UTC) // 09:45 ET

func TestOptionsBreakoutEntry_NoBreakoutKeepsWatching(t *testing.T) {
	r := &OpeningRange{Ticker: "SPY", High: 100, Low: 99, DefinedAt: testRangeDefinedAt}
	placer := &rPlacer{}
	router := NewOptionsRouter(rEngine(nil, nil, 40, true), placer, []string{"SPY"}, 500, 0)
	// A bar strictly inside the range → neither side breaks.
	md := &qMarketData{candles: []data.Candle{
		{Timestamp: testRangeDefinedAt.Add(time.Minute), High: 99.5, Low: 99.2, Open: 99.3, Close: 99.4},
	}}
	s := breakoutScheduler(md, router)

	done, line := s.optionsBreakoutEntry(context.Background(), "sess", "SPY", r, 100000)
	if done {
		t.Error("done=true with no breakout; want false (keep watching)")
	}
	if line != "" {
		t.Errorf("unexpected line %q", line)
	}
	if s.optionsAttempted["SPY"] {
		t.Error("marked attempted without a breakout")
	}
	if placer.calls != 0 {
		t.Errorf("placer called %d times without a breakout", placer.calls)
	}
}

func TestOptionsBreakoutEntry_ExcludesRangeLockBar(t *testing.T) {
	r := &OpeningRange{Ticker: "SPY", High: 100, Low: 99, DefinedAt: testRangeDefinedAt}
	router := NewOptionsRouter(rEngine(nil, nil, 40, true), &rPlacer{}, []string{"SPY"}, 500, 0)
	buy := ComputeBracket(r, "buy", 100000, config.ORBConfig{RiskReward: 2})
	// The LOCK bar itself would break (High well above the buy entry stop); later bars do not.
	// If the lock bar is (wrongly) included, FirstBreakout fires "buy" → done=true.
	md := &qMarketData{candles: []data.Candle{
		{Timestamp: testRangeDefinedAt, High: buy.EntryStop + 5, Low: 99.5, Open: 99.6, Close: buy.EntryStop + 4},
		{Timestamp: testRangeDefinedAt.Add(time.Minute), High: buy.EntryStop - 0.01, Low: 99.5, Open: 99.6, Close: 99.7},
	}}
	s := breakoutScheduler(md, router)

	done, _ := s.optionsBreakoutEntry(context.Background(), "sess", "SPY", r, 100000)
	if done {
		t.Error("lock-bar breakout was not excluded — a bar at r.DefinedAt fired an entry (parity bug vs AssembleSignals)")
	}
}

func TestOptionsBreakoutEntry_BreakoutSpendsAttemptOnGateAbort(t *testing.T) {
	r := &OpeningRange{Ticker: "SPY", High: 100, Low: 99, DefinedAt: testRangeDefinedAt}
	buy := ComputeBracket(r, "buy", 100000, config.ORBConfig{RiskReward: 2})
	placer := &rPlacer{}
	// rankOK=false → the IV-rank gate fails closed → Route aborts BEFORE any placement/DB write.
	router := NewOptionsRouter(rEngine([]execution.OptionSnapshot{
		optSnap("SPY", "C", 100, fp(0.55), 1.22, 1.28, routerExp),
	}, nil, 0, false), placer, []string{"SPY"}, 500, 0)
	md := &qMarketData{candles: []data.Candle{
		{Timestamp: testRangeDefinedAt.Add(time.Minute), High: buy.EntryStop + 2, Low: 99.5, Open: 99.6, Close: buy.EntryStop + 1},
	}}
	s := breakoutScheduler(md, router)

	done, _ := s.optionsBreakoutEntry(context.Background(), "sess", "SPY", r, 100000)
	if !done {
		t.Error("done=false after the breakout fired; want true (entry attempt is spent even on a gate abort)")
	}
	if !s.optionsAttempted["SPY"] {
		t.Error("breakout fired but attempt not marked — it would re-evaluate against a drifted chain next tick")
	}

	// Second tick must be a no-op: the session's single attempt is already spent.
	placer.calls = 0
	done2, _ := s.optionsBreakoutEntry(context.Background(), "sess", "SPY", r, 100000)
	if !done2 {
		t.Error("second tick should short-circuit as done")
	}
	if placer.calls != 0 {
		t.Errorf("second tick re-attempted routing (placer calls=%d)", placer.calls)
	}
}
