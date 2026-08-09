// Package currencystrengthresearch runs leakage-resistant rolling-origin and
// pair-ablation experiments over currency-strength replay results.
package currencystrengthresearch

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
)

const SchemaVersion = 1

type Param struct {
	Start         string  `json:"start"`
	RankEnd       string  `json:"rank_end"`
	Exit          string  `json:"exit"`
	MinGap        float64 `json:"min_gap"`
	BroadMin      int     `json:"broad_min"`
	Confirm       float64 `json:"confirm"`
	Units         int     `json:"units"`
	CurrencyModel string  `json:"currency_model"`
	AllowPartial  bool    `json:"allow_partial"`
}

func (p Param) ID() string {
	return strings.Join([]string{
		p.CurrencyModel, p.Start + "-" + p.RankEnd, p.Exit,
		"g" + strconv.FormatFloat(p.MinGap, 'g', -1, 64),
		fmt.Sprintf("b%d", p.BroadMin),
		"c" + strconv.FormatFloat(p.Confirm, 'g', -1, 64),
		fmt.Sprintf("u%d", p.Units), fmt.Sprintf("partial%t", p.AllowPartial),
	}, "/")
}

type Fold struct {
	ID            string    `json:"id"`
	SelectionFrom time.Time `json:"selection_from"`
	SelectionTo   time.Time `json:"selection_to"`
	TestFrom      time.Time `json:"test_from"`
	TestTo        time.Time `json:"test_to"`
}

func ExpandingAnnualFolds(from, to time.Time) ([]Fold, error) {
	from, to = dayStart(from), dayEnd(to)
	if !to.After(from) || to.Year() <= from.Year() {
		return nil, fmt.Errorf("rolling-origin research requires data spanning at least two calendar years")
	}
	var folds []Fold
	for testYear := from.Year() + 1; testYear <= to.Year(); testYear++ {
		testFrom := time.Date(testYear, 1, 1, 0, 0, 0, 0, time.UTC)
		testTo := dayEnd(time.Date(testYear, 12, 31, 0, 0, 0, 0, time.UTC))
		if testTo.After(to) {
			testTo = to
		}
		folds = append(folds, Fold{
			ID: fmt.Sprintf("test-%d", testYear), SelectionFrom: from,
			SelectionTo: testFrom.Add(-time.Nanosecond), TestFrom: testFrom, TestTo: testTo,
		})
	}
	return folds, nil
}

type Scope string
type Policy string

const (
	ScopeScoring   Scope  = "scoring"
	ScopeExecution Scope  = "execution"
	PolicyFrozen   Policy = "frozen"
	PolicyReselect Policy = "reselected"
)

type Request struct {
	Param          Param
	ScoreSymbols   []string
	TradeSymbols   []string
	CostMultiplier float64
}

type Simulator interface {
	Run(context.Context, Request) ([]orbreplay.SimTrade, error)
}

type Criteria struct {
	MinTradesPerSelectionYear       int       `json:"min_trades_per_selection_year"`
	CostMultipliers                 []float64 `json:"cost_multipliers"`
	RequiredImprovedFolds           int       `json:"required_improved_folds"`
	ExpectedFolds                   int       `json:"expected_folds"`
	MinAffectedDays                 int       `json:"min_affected_days"`
	MinImprovementPerAffectedDayUSD float64   `json:"min_improvement_per_affected_day_usd"`
	MinAggregateImprovementUSD      float64   `json:"min_aggregate_improvement_usd"`
}

func DefaultCriteria() Criteria {
	return Criteria{MinTradesPerSelectionYear: 75, CostMultipliers: []float64{1, 2}, RequiredImprovedFolds: 3, ExpectedFolds: 4, MinAffectedDays: 50, MinImprovementPerAffectedDayUSD: 5, MinAggregateImprovementUSD: 250}
}

type Metrics struct {
	Trades               int      `json:"trades"`
	WinRate              float64  `json:"win_rate"`
	ProfitFactor         *float64 `json:"profit_factor"`
	ProfitFactorInfinite bool     `json:"profit_factor_infinite,omitempty"`
	NetPnL               float64  `json:"net_pnl"`
	AvgPnL               float64  `json:"avg_pnl"`
}

func reportMetrics(trades []orbreplay.SimTrade) Metrics {
	m := orbreplay.Summarize("research", trades).Overall
	out := Metrics{Trades: m.Trades, WinRate: m.WinRate, NetPnL: m.NetPnL}
	if m.Trades > 0 {
		out.AvgPnL = m.NetPnL / float64(m.Trades)
	}
	if m.Wins+m.Losses == 0 {
		return out
	}
	if math.IsInf(m.ProfitFactor, 1) {
		out.ProfitFactorInfinite = true
	} else {
		pf := m.ProfitFactor
		out.ProfitFactor = &pf
	}
	return out
}

type SelectionYear struct {
	Year    int     `json:"year"`
	Metrics Metrics `json:"metrics"`
	Score   float64 `json:"score"`
}

type Selection struct {
	Param           Param           `json:"param"`
	Score           float64         `json:"score"`
	SelectionMetric Metrics         `json:"selection_metrics"`
	Years           []SelectionYear `json:"years"`
}

type CostResult struct {
	Multiplier float64 `json:"multiplier"`
	Metrics    Metrics `json:"metrics"`
}

type BaselineFold struct {
	Fold      Fold         `json:"fold"`
	Selection Selection    `json:"selection"`
	Test      []CostResult `json:"test"`
}

type Comparison struct {
	FoldID       string  `json:"fold_id"`
	Pair         string  `json:"pair"`
	Scope        Scope   `json:"scope"`
	Policy       Policy  `json:"policy"`
	Param        Param   `json:"param"`
	Cost         float64 `json:"cost_multiplier"`
	Control      Metrics `json:"control"`
	Ablated      Metrics `json:"ablated"`
	NetPnLDelta  float64 `json:"net_pnl_delta"`
	AffectedDays int     `json:"affected_days"`
}

type Evidence struct {
	Pair             string   `json:"pair"`
	Scope            Scope    `json:"scope"`
	Policy           Policy   `json:"policy"`
	ImprovedFolds1X  int      `json:"improved_folds_1x"`
	ImprovedFolds2X  int      `json:"improved_folds_2x"`
	ImprovedBoth     int      `json:"improved_folds_both_costs"`
	AffectedDays     int      `json:"affected_days"`
	AggregateDelta1X float64  `json:"aggregate_delta_1x"`
	AggregateDelta2X float64  `json:"aggregate_delta_2x"`
	Qualifies        bool     `json:"qualifies"`
	Reasons          []string `json:"reasons,omitempty"`
}

type PairReport struct {
	Pair        string       `json:"pair"`
	Comparisons []Comparison `json:"comparisons"`
	Evidence    []Evidence   `json:"evidence"`
	Verdict     string       `json:"verdict"`
}

type Report struct {
	SchemaVersion  int            `json:"schema_version"`
	GeneratedAt    time.Time      `json:"generated_at"`
	From           time.Time      `json:"from"`
	To             time.Time      `json:"to"`
	Universe       []string       `json:"universe"`
	CandidateCount int            `json:"candidate_count"`
	Criteria       Criteria       `json:"criteria"`
	Folds          []BaselineFold `json:"folds"`
	Pairs          []PairReport   `json:"pairs"`
}

type Runner struct {
	Simulator   Simulator
	Params      []Param
	Universe    []string
	Folds       []Fold
	Criteria    Criteria
	GeneratedAt func() time.Time
	Progress    func(string)
}

type cachedSimulation struct {
	trades []orbreplay.SimTrade
	err    error
}

func (r Runner) Run(ctx context.Context) (Report, error) {
	if r.Simulator == nil || len(r.Params) == 0 || len(r.Folds) == 0 {
		return Report{}, fmt.Errorf("research runner requires simulator, parameters, and folds")
	}
	criteria := r.Criteria
	if criteria.MinTradesPerSelectionYear <= 0 {
		criteria = DefaultCriteria()
	}
	if err := validateCriteria(criteria); err != nil {
		return Report{}, err
	}
	universe := append([]string(nil), r.Universe...)
	sort.Strings(universe)
	now := time.Now
	if r.GeneratedAt != nil {
		now = r.GeneratedAt
	}
	report := Report{SchemaVersion: SchemaVersion, GeneratedAt: now().UTC(), Universe: universe, CandidateCount: len(r.Params), Criteria: criteria}
	report.From, report.To = r.Folds[0].SelectionFrom, r.Folds[len(r.Folds)-1].TestTo
	persistentCache := make(map[string]cachedSimulation)
	runCached := func(cache map[string]cachedSimulation, req Request) ([]orbreplay.SimTrade, error) {
		key := requestKey(req)
		if cached, ok := cache[key]; ok {
			return cached.trades, cached.err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		trades, err := r.Simulator.Run(ctx, req)
		cache[key] = cachedSimulation{trades: trades, err: err}
		return trades, err
	}

	baselineHistories := make(map[string][]orbreplay.SimTrade, len(r.Params))
	if r.Progress != nil {
		r.Progress(fmt.Sprintf("baseline: evaluating %d candidates", len(r.Params)))
	}
	for _, param := range r.Params {
		trades, err := runCached(persistentCache, Request{Param: param, CostMultiplier: 1})
		if err != nil {
			return Report{}, fmt.Errorf("baseline %s: %w", param.ID(), err)
		}
		baselineHistories[param.ID()] = trades
	}
	baselineSelections := make(map[string]Selection, len(r.Folds))
	for _, fold := range r.Folds {
		selection, ok := selectParam(r.Params, baselineHistories, fold, criteria.MinTradesPerSelectionYear)
		if !ok {
			return Report{}, fmt.Errorf("%s: no baseline candidate met %d trades in every selection year", fold.ID, criteria.MinTradesPerSelectionYear)
		}
		baselineSelections[fold.ID] = selection
		row := BaselineFold{Fold: fold, Selection: selection}
		for _, cost := range criteria.CostMultipliers {
			trades, err := runCached(persistentCache, Request{Param: selection.Param, CostMultiplier: cost})
			if err != nil {
				return Report{}, err
			}
			row.Test = append(row.Test, CostResult{Multiplier: cost, Metrics: reportMetrics(filterTrades(trades, fold.TestFrom, fold.TestTo))})
		}
		report.Folds = append(report.Folds, row)
	}

	for _, pair := range universe {
		pairReport := PairReport{Pair: pair}
		pairCache := make(map[string]cachedSimulation)
		run := func(req Request) ([]orbreplay.SimTrade, error) {
			if req.ScoreSymbols == nil && req.TradeSymbols == nil {
				return runCached(persistentCache, req)
			}
			return runCached(pairCache, req)
		}
		for _, scope := range []Scope{ScopeScoring, ScopeExecution} {
			if r.Progress != nil {
				r.Progress(fmt.Sprintf("%s: %s ablation", pair, scope))
			}
			scoreSymbols, tradeSymbols := scenarioSymbols(universe, pair, scope)
			histories := make(map[string][]orbreplay.SimTrade, len(r.Params))
			for _, param := range r.Params {
				if scope == ScopeExecution {
					histories[param.ID()] = excludeTicker(baselineHistories[param.ID()], pair)
					continue
				}
				trades, err := run(Request{Param: param, ScoreSymbols: scoreSymbols, TradeSymbols: tradeSymbols, CostMultiplier: 1})
				if err != nil {
					return Report{}, fmt.Errorf("%s scoring ablation %s: %w", pair, param.ID(), err)
				}
				histories[param.ID()] = trades
			}
			for _, policy := range []Policy{PolicyFrozen, PolicyReselect} {
				for _, fold := range r.Folds {
					selection := baselineSelections[fold.ID]
					if policy == PolicyReselect {
						var ok bool
						selection, ok = selectParam(r.Params, histories, fold, criteria.MinTradesPerSelectionYear)
						if !ok {
							continue
						}
					}
					for _, cost := range criteria.CostMultipliers {
						controlTrades, err := run(Request{Param: baselineSelections[fold.ID].Param, CostMultiplier: cost})
						if err != nil {
							return Report{}, err
						}
						var ablatedTrades []orbreplay.SimTrade
						if scope == ScopeExecution {
							trades, err := run(Request{Param: selection.Param, CostMultiplier: cost})
							if err != nil {
								return Report{}, err
							}
							ablatedTrades = excludeTicker(trades, pair)
						} else {
							ablatedTrades, err = run(Request{Param: selection.Param, ScoreSymbols: scoreSymbols, TradeSymbols: tradeSymbols, CostMultiplier: cost})
							if err != nil {
								return Report{}, err
							}
						}
						controlTest := filterTrades(controlTrades, fold.TestFrom, fold.TestTo)
						ablatedTest := filterTrades(ablatedTrades, fold.TestFrom, fold.TestTo)
						controlMetrics, ablatedMetrics := reportMetrics(controlTest), reportMetrics(ablatedTest)
						pairReport.Comparisons = append(pairReport.Comparisons, Comparison{
							FoldID: fold.ID, Pair: pair, Scope: scope, Policy: policy, Param: selection.Param, Cost: cost,
							Control: controlMetrics, Ablated: ablatedMetrics, NetPnLDelta: ablatedMetrics.NetPnL - controlMetrics.NetPnL,
							AffectedDays: affectedDays(controlTest, ablatedTest),
						})
					}
				}
				pairReport.Evidence = append(pairReport.Evidence, assessEvidence(pair, scope, policy, pairReport.Comparisons, criteria))
			}
		}
		pairReport.Verdict = pairVerdict(pairReport.Evidence)
		report.Pairs = append(report.Pairs, pairReport)
	}
	return report, nil
}

func selectParam(params []Param, histories map[string][]orbreplay.SimTrade, fold Fold, minPerYear int) (Selection, bool) {
	var candidates []Selection
	for _, param := range params {
		all := filterTrades(histories[param.ID()], fold.SelectionFrom, fold.SelectionTo)
		selection := Selection{Param: param, SelectionMetric: reportMetrics(all)}
		worst := math.Inf(1)
		eligible := true
		for year := fold.SelectionFrom.Year(); year <= fold.SelectionTo.Year(); year++ {
			from := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
			if from.Before(fold.SelectionFrom) {
				from = fold.SelectionFrom
			}
			to := dayEnd(time.Date(year, 12, 31, 0, 0, 0, 0, time.UTC))
			if to.After(fold.SelectionTo) {
				to = fold.SelectionTo
			}
			yearTrades := filterTrades(histories[param.ID()], from, to)
			metrics := reportMetrics(yearTrades)
			score := score(metrics, minPerYear)
			selection.Years = append(selection.Years, SelectionYear{Year: year, Metrics: metrics, Score: score})
			if metrics.Trades < minPerYear {
				eligible = false
			}
			if score < worst {
				worst = score
			}
		}
		if !eligible {
			continue
		}
		selection.Score = worst + score(selection.SelectionMetric, minPerYear*len(selection.Years))*0.25
		candidates = append(candidates, selection)
	}
	if len(candidates) == 0 {
		return Selection{}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if candidates[i].SelectionMetric.NetPnL != candidates[j].SelectionMetric.NetPnL {
			return candidates[i].SelectionMetric.NetPnL > candidates[j].SelectionMetric.NetPnL
		}
		return candidates[i].Param.ID() < candidates[j].Param.ID()
	})
	return candidates[0], true
}

func score(m Metrics, minTrades int) float64 {
	if m.Trades == 0 {
		return math.Inf(-1)
	}
	pf := 1.0
	if m.ProfitFactorInfinite {
		pf = 10
	} else if m.ProfitFactor != nil {
		pf = *m.ProfitFactor
	}
	s := m.AvgPnL/100 + (pf-1)*0.8 + (m.WinRate-0.5)*0.4
	if m.Trades < minTrades {
		s -= 1000 + float64(minTrades-m.Trades)/float64(minTrades)
	}
	return s
}

func assessEvidence(pair string, scope Scope, policy Policy, comparisons []Comparison, criteria Criteria) Evidence {
	e := Evidence{Pair: pair, Scope: scope, Policy: policy}
	byFold := make(map[string]map[float64]Comparison)
	for _, c := range comparisons {
		if c.Pair != pair || c.Scope != scope || c.Policy != policy {
			continue
		}
		if byFold[c.FoldID] == nil {
			byFold[c.FoldID] = make(map[float64]Comparison)
		}
		byFold[c.FoldID][c.Cost] = c
	}
	for _, costs := range byFold {
		one, oneOK := costs[1]
		two, twoOK := costs[2]
		if oneOK {
			e.AffectedDays += one.AffectedDays
			e.AggregateDelta1X += one.NetPnLDelta
		}
		if twoOK {
			e.AggregateDelta2X += two.NetPnLDelta
		}
		if oneOK && materialImprovement(one, criteria) {
			e.ImprovedFolds1X++
		}
		if twoOK && materialImprovement(two, criteria) {
			e.ImprovedFolds2X++
		}
		if oneOK && twoOK && materialImprovement(one, criteria) && materialImprovement(two, criteria) {
			e.ImprovedBoth++
		}
	}
	if len(byFold) != criteria.ExpectedFolds {
		e.Reasons = append(e.Reasons, fmt.Sprintf("have %d/%d folds", len(byFold), criteria.ExpectedFolds))
	}
	if e.ImprovedBoth < criteria.RequiredImprovedFolds {
		e.Reasons = append(e.Reasons, fmt.Sprintf("improved at both costs in %d/%d required folds", e.ImprovedBoth, criteria.RequiredImprovedFolds))
	}
	if e.AffectedDays < criteria.MinAffectedDays {
		e.Reasons = append(e.Reasons, fmt.Sprintf("affected %d/%d required days", e.AffectedDays, criteria.MinAffectedDays))
	}
	if e.AggregateDelta1X < criteria.MinAggregateImprovementUSD || e.AggregateDelta2X < criteria.MinAggregateImprovementUSD {
		e.Reasons = append(e.Reasons, fmt.Sprintf("aggregate improvement $%.2f/$%.2f at 1x/2x; require $%.2f at both", e.AggregateDelta1X, e.AggregateDelta2X, criteria.MinAggregateImprovementUSD))
	}
	e.Qualifies = len(e.Reasons) == 0
	return e
}

func materialImprovement(comparison Comparison, criteria Criteria) bool {
	return comparison.AffectedDays > 0 && comparison.NetPnLDelta/float64(comparison.AffectedDays) >= criteria.MinImprovementPerAffectedDayUSD
}

func pairVerdict(evidence []Evidence) string {
	qualified := make(map[Scope]map[Policy]bool)
	for _, e := range evidence {
		if qualified[e.Scope] == nil {
			qualified[e.Scope] = make(map[Policy]bool)
		}
		qualified[e.Scope][e.Policy] = e.Qualifies
	}
	scoreCut := qualified[ScopeScoring][PolicyFrozen] && qualified[ScopeScoring][PolicyReselect]
	execCut := qualified[ScopeExecution][PolicyFrozen] && qualified[ScopeExecution][PolicyReselect]
	switch {
	case scoreCut && execCut:
		return "exclude_pair"
	case scoreCut:
		return "exclude_from_scoring"
	case execCut:
		return "exclude_from_execution"
	default:
		return "keep_insufficient_exclusion_evidence"
	}
}

func affectedDays(a, b []orbreplay.SimTrade) int {
	left, right := identitiesByDay(a), identitiesByDay(b)
	days := make(map[string]bool, len(left)+len(right))
	for day := range left {
		days[day] = true
	}
	for day := range right {
		days[day] = true
	}
	count := 0
	for day := range days {
		if left[day] != right[day] {
			count++
		}
	}
	return count
}

func identitiesByDay(trades []orbreplay.SimTrade) map[string]string {
	grouped := make(map[string][]string, len(trades))
	for _, trade := range trades {
		day := trade.EntryTime.UTC().Format("2006-01-02")
		grouped[day] = append(grouped[day], strings.Join([]string{trade.Ticker, trade.Side, trade.EntryTime.UTC().Format(time.RFC3339Nano), trade.ExitTime.UTC().Format(time.RFC3339Nano)}, "|"))
	}
	out := make(map[string]string, len(grouped))
	for day, identities := range grouped {
		sort.Strings(identities)
		out[day] = strings.Join(identities, "\n")
	}
	return out
}

func filterTrades(trades []orbreplay.SimTrade, from, to time.Time) []orbreplay.SimTrade {
	out := make([]orbreplay.SimTrade, 0, len(trades))
	for _, trade := range trades {
		if !trade.EntryTime.Before(from) && !trade.EntryTime.After(to) {
			out = append(out, trade)
		}
	}
	return out
}

func excludeTicker(trades []orbreplay.SimTrade, pair string) []orbreplay.SimTrade {
	out := make([]orbreplay.SimTrade, 0, len(trades))
	for _, trade := range trades {
		if trade.Ticker != pair {
			out = append(out, trade)
		}
	}
	return out
}

func scenarioSymbols(universe []string, excluded string, scope Scope) ([]string, []string) {
	allowed := make([]string, 0, len(universe)-1)
	for _, symbol := range universe {
		if symbol != excluded {
			allowed = append(allowed, symbol)
		}
	}
	if scope == ScopeScoring {
		return allowed, nil
	}
	return nil, allowed
}

func requestKey(req Request) string {
	scoreSymbols, tradeSymbols := append([]string(nil), req.ScoreSymbols...), append([]string(nil), req.TradeSymbols...)
	sort.Strings(scoreSymbols)
	sort.Strings(tradeSymbols)
	return strings.Join([]string{req.Param.ID(), strings.Join(scoreSymbols, ","), strings.Join(tradeSymbols, ","), strconv.FormatFloat(req.CostMultiplier, 'g', -1, 64)}, "|")
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
func dayEnd(t time.Time) time.Time { return dayStart(t).Add(24*time.Hour - time.Nanosecond) }

func validateCriteria(criteria Criteria) error {
	if criteria.MinTradesPerSelectionYear <= 0 || criteria.RequiredImprovedFolds <= 0 || criteria.ExpectedFolds <= 0 || criteria.MinAffectedDays <= 0 || criteria.MinImprovementPerAffectedDayUSD <= 0 || criteria.MinAggregateImprovementUSD <= 0 {
		return fmt.Errorf("research criteria values must be positive")
	}
	if math.IsNaN(criteria.MinImprovementPerAffectedDayUSD) || math.IsInf(criteria.MinImprovementPerAffectedDayUSD, 0) || math.IsNaN(criteria.MinAggregateImprovementUSD) || math.IsInf(criteria.MinAggregateImprovementUSD, 0) {
		return fmt.Errorf("research materiality criteria must be finite")
	}
	seen := make(map[float64]bool, len(criteria.CostMultipliers))
	for _, cost := range criteria.CostMultipliers {
		if cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
			return fmt.Errorf("cost multiplier %v must be finite and positive", cost)
		}
		if seen[cost] {
			return fmt.Errorf("duplicate cost multiplier %v", cost)
		}
		seen[cost] = true
	}
	if !seen[1] || !seen[2] {
		return fmt.Errorf("cost multipliers must include 1 and 2")
	}
	return nil
}
