package currencystrengthresearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
)

func TestExpandingAnnualFolds(t *testing.T) {
	from := time.Date(2022, 3, 15, 13, 0, 0, 0, time.FixedZone("x", 3600))
	to := time.Date(2024, 6, 10, 8, 0, 0, 0, time.UTC)
	folds, err := ExpandingAnnualFolds(from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(folds) != 2 {
		t.Fatalf("folds=%d, want 2", len(folds))
	}
	if got := folds[0].SelectionFrom; !got.Equal(time.Date(2022, 3, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("selection from=%s", got)
	}
	if got := folds[0].SelectionTo; !got.Equal(time.Date(2022, 12, 31, 23, 59, 59, 999999999, time.UTC)) {
		t.Fatalf("selection to=%s", got)
	}
	if got := folds[1].TestTo; !got.Equal(time.Date(2024, 6, 10, 23, 59, 59, 999999999, time.UTC)) {
		t.Fatalf("partial test to=%s", got)
	}
	if _, err := ExpandingAnnualFolds(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("single year accepted")
	}
}

func TestSelectParamIgnoresTestAndRequiresEverySelectionYear(t *testing.T) {
	a := Param{Start: "06:00", RankEnd: "09:00", Exit: "18:00", MinGap: .001}
	b := Param{Start: "06:00", RankEnd: "09:00", Exit: "18:00", MinGap: .002}
	fold := Fold{
		ID: "test-2024", SelectionFrom: utc(2022, 1, 1), SelectionTo: endUTC(2023, 12, 31),
		TestFrom: utc(2024, 1, 1), TestTo: endUTC(2024, 12, 31),
	}
	histories := map[string][]orbreplay.SimTrade{
		a.ID(): append(makeTrades(2022, 100, 10, "EUR/USD"), makeTrades(2023, 74, 100, "EUR/USD")...),
		b.ID(): append(makeTrades(2022, 75, 5, "GBP/USD"), makeTrades(2023, 75, 5, "GBP/USD")...),
	}
	// Test-period riches cannot rescue A's ineligible 2023 selection sample.
	histories[a.ID()] = append(histories[a.ID()], makeTrades(2024, 200, 10000, "EUR/USD")...)
	selection, ok := selectParam([]Param{a, b}, histories, fold, 75)
	if !ok || selection.Param.ID() != b.ID() {
		t.Fatalf("selection=%+v ok=%v, want B", selection, ok)
	}
	if _, ok := selectParam([]Param{a}, histories, fold, 75); ok {
		t.Fatal("all-ineligible selection succeeded")
	}
}

func TestSelectParamTieBreakDeterministic(t *testing.T) {
	a := Param{Start: "06:00", RankEnd: "09:00", Exit: "18:00", MinGap: .001}
	b := Param{Start: "06:00", RankEnd: "09:00", Exit: "18:00", MinGap: .002}
	fold := Fold{SelectionFrom: utc(2022, 1, 1), SelectionTo: endUTC(2022, 12, 31)}
	trades := makeTrades(2022, 75, 10, "EUR/USD")
	histories := map[string][]orbreplay.SimTrade{a.ID(): trades, b.ID(): trades}
	want := a
	if b.ID() < a.ID() {
		want = b
	}
	for _, order := range [][]Param{{a, b}, {b, a}} {
		got, ok := selectParam(order, histories, fold, 75)
		if !ok || got.Param.ID() != want.ID() {
			t.Fatalf("order=%v selected=%s want=%s", order, got.Param.ID(), want.ID())
		}
	}
}

func TestAffectedDaysUsesTradeIdentityMultisets(t *testing.T) {
	entry := utc(2024, 1, 2).Add(9 * time.Hour)
	one := orbreplay.SimTrade{Ticker: "EUR/USD", Side: "buy", EntryTime: entry, ExitTime: entry.Add(time.Hour), NetPnL: 10}
	two := orbreplay.SimTrade{Ticker: "GBP/USD", Side: "sell", EntryTime: entry.Add(time.Minute), ExitTime: entry.Add(2 * time.Hour), NetPnL: -5}
	changedPnL := one
	changedPnL.NetPnL = 999
	if got := affectedDays([]orbreplay.SimTrade{one, two}, []orbreplay.SimTrade{two, changedPnL}); got != 0 {
		t.Fatalf("reorder/PnL affected days=%d", got)
	}
	changedSide := two
	changedSide.Side = "buy"
	if got := affectedDays([]orbreplay.SimTrade{one, two}, []orbreplay.SimTrade{one, changedSide}); got != 1 {
		t.Fatalf("identity change affected days=%d", got)
	}
}

func TestAssessEvidenceExactThresholds(t *testing.T) {
	comparisons := evidenceComparisons(3, 50)
	criteria := DefaultCriteria()
	got := assessEvidence("EUR/USD", ScopeScoring, PolicyFrozen, comparisons, criteria)
	if !got.Qualifies || got.ImprovedBoth != 3 || got.AffectedDays != 50 {
		t.Fatalf("threshold evidence=%+v", got)
	}
	for _, tc := range []struct {
		name string
		rows []Comparison
	}{
		{"two folds", evidenceComparisons(2, 50)},
		{"49 days", evidenceComparisons(3, 49)},
		{"missing fold", evidenceComparisons(3, 50)[:6]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if e := assessEvidence("EUR/USD", ScopeScoring, PolicyFrozen, tc.rows, criteria); e.Qualifies {
				t.Fatalf("qualified: %+v", e)
			}
		})
	}
	duplicated := append(append([]Comparison(nil), comparisons...), comparisons[0])
	if e := assessEvidence("EUR/USD", ScopeScoring, PolicyFrozen, duplicated, criteria); e.AffectedDays != 50 {
		t.Fatalf("duplicate row affected=%d", e.AffectedDays)
	}
}

func TestPairVerdictRequiresBothPolicies(t *testing.T) {
	qual := func(scope Scope, policy Policy) Evidence {
		return Evidence{Scope: scope, Policy: policy, Qualifies: true}
	}
	if got := pairVerdict([]Evidence{qual(ScopeScoring, PolicyFrozen), qual(ScopeScoring, PolicyReselect)}); got != "exclude_from_scoring" {
		t.Fatalf("verdict=%s", got)
	}
	if got := pairVerdict([]Evidence{qual(ScopeExecution, PolicyFrozen), qual(ScopeExecution, PolicyReselect)}); got != "exclude_from_execution" {
		t.Fatalf("verdict=%s", got)
	}
	if got := pairVerdict([]Evidence{qual(ScopeScoring, PolicyFrozen)}); got != "keep_insufficient_exclusion_evidence" {
		t.Fatalf("verdict=%s", got)
	}
}

func TestRequestKeyUsesExactNumbersAndCanonicalSymbols(t *testing.T) {
	base := Request{Param: Param{MinGap: .0002001}, ScoreSymbols: []string{"GBP/USD", "EUR/USD"}, CostMultiplier: 1.00001}
	reordered := base
	reordered.ScoreSymbols = []string{"EUR/USD", "GBP/USD"}
	if requestKey(base) != requestKey(reordered) {
		t.Fatal("symbol order changed cache key")
	}
	closeParam := base
	closeParam.Param.MinGap = .0002002
	closeCost := base
	closeCost.CostMultiplier = 1.00002
	if requestKey(base) == requestKey(closeParam) || requestKey(base) == requestKey(closeCost) {
		t.Fatal("distinct numeric requests collided")
	}
}

func TestWriteJSONRepresentsInfiniteProfitFactor(t *testing.T) {
	metrics := reportMetrics([]orbreplay.SimTrade{{NetPnL: 10}})
	report := Report{SchemaVersion: 1, Folds: []BaselineFold{{Test: []CostResult{{Multiplier: 1, Metrics: metrics}}}}}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, report); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "+Inf") || strings.Contains(buf.String(), "NaN") {
		t.Fatalf("non-finite JSON: %s", buf.String())
	}
	var decoded Report
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	got := decoded.Folds[0].Test[0].Metrics
	if !got.ProfitFactorInfinite || got.ProfitFactor != nil {
		t.Fatalf("PF encoding=%+v", got)
	}
}

func TestReportMetricsLeavesProfitFactorUndefinedWithoutWinsOrLosses(t *testing.T) {
	got := reportMetrics(nil)
	if got.ProfitFactor != nil || got.ProfitFactorInfinite {
		t.Fatalf("zero-trade profit factor=%+v", got)
	}
}

type countingSimulator struct {
	seen map[string]bool
}

func (s *countingSimulator) Run(_ context.Context, req Request) ([]orbreplay.SimTrade, error) {
	key := requestKey(req)
	if s.seen[key] {
		return nil, fmt.Errorf("duplicate simulator request %s", key)
	}
	s.seen[key] = true
	tickers := []string{"EUR/USD", "GBP/USD"}
	if len(req.TradeSymbols) > 0 {
		tickers = req.TradeSymbols
	}
	pnl := 10 / req.CostMultiplier
	var trades []orbreplay.SimTrade
	for year := 2022; year <= 2026; year++ {
		for _, ticker := range tickers {
			trades = append(trades, makeTrades(year, 75, pnl, ticker)...)
		}
	}
	return trades, nil
}

func TestRunnerTinyEndToEndUsesCacheAndStableCardinality(t *testing.T) {
	from, to := utc(2022, 1, 1), endUTC(2026, 8, 7)
	folds, err := ExpandingAnnualFolds(from, to)
	if err != nil {
		t.Fatal(err)
	}
	simulator := &countingSimulator{seen: make(map[string]bool)}
	runner := Runner{
		Simulator: simulator,
		Params:    []Param{{Start: "06:00", RankEnd: "09:00", Exit: "18:00", MinGap: .001}},
		Universe:  []string{"GBP/USD", "EUR/USD"}, Folds: folds,
		Criteria:    DefaultCriteria(),
		GeneratedAt: func() time.Time { return utc(2026, 8, 9) },
	}
	report, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Folds) != 4 || len(report.Pairs) != 2 {
		t.Fatalf("folds=%d pairs=%d", len(report.Folds), len(report.Pairs))
	}
	if report.Universe[0] != "EUR/USD" || !report.GeneratedAt.Equal(utc(2026, 8, 9)) {
		t.Fatalf("metadata=%+v", report)
	}
	for _, pair := range report.Pairs {
		if len(pair.Comparisons) != 32 || len(pair.Evidence) != 4 {
			t.Fatalf("%s comparisons=%d evidence=%d", pair.Pair, len(pair.Comparisons), len(pair.Evidence))
		}
	}
}

type policySimulator struct{}

func (policySimulator) Run(_ context.Context, req Request) ([]orbreplay.SimTrade, error) {
	isA := req.Param.MinGap == .001
	selectionPnL := 1.0
	if isA {
		selectionPnL = 10
	}
	if len(req.ScoreSymbols) > 0 {
		selectionPnL = 10
		if isA {
			selectionPnL = 1
		}
	}
	var trades []orbreplay.SimTrade
	for year := 2022; year <= 2026; year++ {
		pnl := selectionPnL / req.CostMultiplier
		if year == 2023 && isA && len(req.ScoreSymbols) == 0 {
			pnl = -1000 // Unseen 2023 performance must not change test-2023 selection.
		}
		trades = append(trades, makeTrades(year, 75, pnl, "EUR/USD")...)
		trades = append(trades, makeTrades(year, 75, pnl, "GBP/USD")...)
	}
	return trades, nil
}

func TestRunnerPoliciesUseOnlySelectionHistoryAndRespondToAblation(t *testing.T) {
	folds, err := ExpandingAnnualFolds(utc(2022, 1, 1), endUTC(2026, 8, 7))
	if err != nil {
		t.Fatal(err)
	}
	a := Param{Start: "06:00", RankEnd: "09:00", Exit: "18:00", MinGap: .001}
	b := Param{Start: "06:00", RankEnd: "09:00", Exit: "18:00", MinGap: .002}
	report, err := (Runner{
		Simulator: policySimulator{}, Params: []Param{a, b}, Universe: []string{"EUR/USD", "GBP/USD"},
		Folds: folds, Criteria: DefaultCriteria(),
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Folds[0].Selection.Param.ID(); got != a.ID() {
		t.Fatalf("test-period result leaked into test-2023 selection: got %s want %s", got, a.ID())
	}
	var frozenParam, reselectedParam string
	var executionAffected int
	for _, pair := range report.Pairs {
		for _, comparison := range pair.Comparisons {
			if pair.Pair == "EUR/USD" && comparison.FoldID == "test-2023" && comparison.Cost == 1 {
				switch {
				case comparison.Scope == ScopeScoring && comparison.Policy == PolicyFrozen:
					frozenParam = comparison.Param.ID()
				case comparison.Scope == ScopeScoring && comparison.Policy == PolicyReselect:
					reselectedParam = comparison.Param.ID()
				case comparison.Scope == ScopeExecution && comparison.Policy == PolicyFrozen:
					executionAffected = comparison.AffectedDays
				}
			}
		}
	}
	if frozenParam != a.ID() || reselectedParam != b.ID() {
		t.Fatalf("policy params frozen=%s reselected=%s, want %s/%s", frozenParam, reselectedParam, a.ID(), b.ID())
	}
	if executionAffected == 0 {
		t.Fatal("execution ablation did not alter any routed trade days")
	}
}

func evidenceComparisons(improved, affected int) []Comparison {
	rows := make([]Comparison, 0, 8)
	remaining := affected
	for fold := 0; fold < 4; fold++ {
		foldAffected := 0
		if remaining > 0 {
			foldAffected = remaining
			if foldAffected > 13 {
				foldAffected = 13
			}
			remaining -= foldAffected
		}
		for _, cost := range []float64{1, 2} {
			delta := -1.0
			if fold < improved {
				delta = float64(foldAffected)*10 + 1
			}
			rows = append(rows, Comparison{FoldID: string(rune('A' + fold)), Pair: "EUR/USD", Scope: ScopeScoring, Policy: PolicyFrozen, Cost: cost, NetPnLDelta: delta, AffectedDays: foldAffected})
		}
	}
	return rows
}

func makeTrades(year, count int, pnl float64, ticker string) []orbreplay.SimTrade {
	trades := make([]orbreplay.SimTrade, 0, count)
	for i := 0; i < count; i++ {
		entry := time.Date(year, 1, 1, 9, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		trades = append(trades, orbreplay.SimTrade{Ticker: ticker, Side: "buy", EntryTime: entry, ExitTime: entry.Add(time.Hour), GrossPnL: pnl, NetPnL: pnl})
	}
	return trades
}

func utc(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
func endUTC(year int, month time.Month, day int) time.Time {
	return utc(year, month, day).Add(24*time.Hour - time.Nanosecond)
}

func TestScoreNeverReturnsNaN(t *testing.T) {
	if got := score(Metrics{Trades: 1, ProfitFactorInfinite: true, AvgPnL: 1, WinRate: 1}, 1); math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("score=%v", got)
	}
}

func TestScoreDoesNotTreatScratchOnlyAsInfiniteProfitFactor(t *testing.T) {
	scratch := score(Metrics{Trades: 75}, 75)
	allWinner := score(Metrics{Trades: 75, WinRate: 1, AvgPnL: 1, ProfitFactorInfinite: true}, 75)
	if scratch >= allWinner {
		t.Fatalf("scratch score=%v all-winner score=%v", scratch, allWinner)
	}
}
