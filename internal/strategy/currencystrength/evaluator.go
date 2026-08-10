package currencystrength

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

type Pair struct {
	Symbol string
	Base   string
	Quote  string
}

type Window struct {
	Start float64
	End   float64
	Error string
}

type Config struct {
	MinGap             float64
	BroadMin           int
	ConfirmMinMove     float64
	TradeInstruments   map[string]bool
	ScoreInstruments   map[string]bool // nil means every configured pair; a non-nil empty map scores none
	ScoreMethod        string          // "" | "sum" | "least_squares"
	AllowPartialBasket bool
}

type Signal struct {
	Symbol           string
	Side             string
	Strong           string
	Weak             string
	Gap              float64
	ConfirmMove      float64
	StrongVotes      int
	WeakVotes        int
	RoutedPairReturn float64
}

type Decision struct {
	Signal      *Signal
	SkipReason  string
	Scores      map[string]float64
	Votes       map[string]int
	Diagnostics DecisionDiagnostics
}

// DecisionDiagnostics is the versioned, durable explanation of one strategy
// evaluation. Keep additions backward-compatible: historical rows retain an
// empty JSON object and consumers must tolerate omitted fields.
type DecisionDiagnostics struct {
	Version      int                  `json:"version"`
	Outcome      string               `json:"outcome"`
	ReasonCode   string               `json:"reason_code,omitempty"`
	Thresholds   DiagnosticThresholds `json:"thresholds"`
	Pairs        []PairDiagnostic     `json:"pairs"`
	Currencies   []CurrencyDiagnostic `json:"currencies,omitempty"`
	Strongest    string               `json:"strongest,omitempty"`
	Weakest      string               `json:"weakest,omitempty"`
	Gap          float64              `json:"gap,omitempty"`
	RoutedSymbol string               `json:"routed_symbol,omitempty"`
	RoutedSide   string               `json:"routed_side,omitempty"`
	ConfirmMove  float64              `json:"confirm_move,omitempty"`
	ScoreMethod  string               `json:"score_method,omitempty"`
	Connected    bool                 `json:"connected"`
	MissingPairs []string             `json:"missing_pairs,omitempty"`
	ResidualRMSE float64              `json:"residual_rmse,omitempty"`
	Gates        []GateDiagnostic     `json:"gates"`
}

type DiagnosticThresholds struct {
	MinGap         float64 `json:"min_gap"`
	BroadMin       int     `json:"broad_min"`
	ConfirmMinMove float64 `json:"confirm_min_move"`
}

type PairDiagnostic struct {
	Symbol    string  `json:"symbol"`
	Base      string  `json:"base"`
	Quote     string  `json:"quote"`
	Start     float64 `json:"start,omitempty"`
	End       float64 `json:"end,omitempty"`
	LogReturn float64 `json:"log_return,omitempty"`
	Available bool    `json:"available"`
	Scored    bool    `json:"scored"`
	Error     string  `json:"error,omitempty"`
}

type CurrencyDiagnostic struct {
	Currency     string  `json:"currency"`
	Score        float64 `json:"score"`
	Votes        int     `json:"votes"`
	Observations int     `json:"observations"`
}

type GateDiagnostic struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // passed | failed | not_evaluated
	Actual   string `json:"actual,omitempty"`
	Required string `json:"required,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

var diagnosticGateNames = []string{
	"price_window", "rank", "min_gap", "breadth", "route",
	"trade_instrument_filter", "confirm_min_move",
}

func PairsFromSymbols(symbols []string) ([]Pair, error) {
	pairs := make([]Pair, 0, len(symbols))
	for _, symbol := range symbols {
		base, quote, ok := SplitPair(symbol)
		if !ok {
			return nil, fmt.Errorf("invalid FX pair %q: expected BASE/QUOTE", symbol)
		}
		pairs = append(pairs, Pair{Symbol: strings.ToUpper(strings.TrimSpace(symbol)), Base: base, Quote: quote})
	}
	return pairs, nil
}

func SplitPair(symbol string) (base, quote string, ok bool) {
	parts := strings.Split(strings.ToUpper(strings.TrimSpace(symbol)), "/")
	if len(parts) != 2 || len(parts[0]) != 3 || len(parts[1]) != 3 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func Evaluate(pairs []Pair, windows map[string]Window, cfg Config) Decision {
	scores := make(map[string]float64)
	votes := make(map[string]int)
	pairReturns := make(map[string]float64, len(pairs))
	diagnostics := newDecisionDiagnostics(cfg)
	var missing []string
	scorePairs := make([]Pair, 0, len(pairs))
	routePairs := make([]Pair, 0, len(pairs))
	observations := make(map[string]int)

	for _, pair := range pairs {
		scored := cfg.ScoreInstruments == nil || cfg.ScoreInstruments[pair.Symbol]
		window, ok := windows[pair.Symbol]
		if !ok || !finitePositive(window.Start) || !finitePositive(window.End) || window.Error != "" {
			reason := window.Error
			if reason == "" {
				reason = "missing, non-positive, or non-finite start/end price"
			}
			start, end := window.Start, window.End
			if math.IsNaN(start) || math.IsInf(start, 0) {
				start = 0
			}
			if math.IsNaN(end) || math.IsInf(end, 0) {
				end = 0
			}
			diagnostics.Pairs = append(diagnostics.Pairs, PairDiagnostic{
				Symbol: pair.Symbol, Base: pair.Base, Quote: pair.Quote,
				Start: start, End: end, Available: false, Scored: scored,
				Error: reason,
			})
			if scored {
				missing = append(missing, pair.Symbol)
			}
			continue
		}
		ret := math.Log(window.End) - math.Log(window.Start)
		routePairs = append(routePairs, pair)
		diagnostics.Pairs = append(diagnostics.Pairs, PairDiagnostic{
			Symbol: pair.Symbol, Base: pair.Base, Quote: pair.Quote,
			Start: window.Start, End: window.End, LogReturn: ret, Available: true, Scored: scored,
		})
		pairReturns[pair.Symbol] = ret
		if !scored {
			continue
		}
		scorePairs = append(scorePairs, pair)
		observations[pair.Base]++
		observations[pair.Quote]++
		scores[pair.Base] += ret
		scores[pair.Quote] -= ret
		switch {
		case ret > 0:
			votes[pair.Base]++
			votes[pair.Quote]--
		case ret < 0:
			votes[pair.Base]--
			votes[pair.Quote]++
		}
	}
	diagnostics.MissingPairs = append([]string(nil), missing...)
	method := strings.ToLower(strings.TrimSpace(cfg.ScoreMethod))
	if method == "" {
		method = "sum"
	}
	diagnostics.ScoreMethod = method
	if len(missing) > 0 && !(cfg.AllowPartialBasket && method == "least_squares") {
		setDiagnosticGate(&diagnostics, "price_window", "failed", strings.Join(missing, ", "), "all configured pairs", "missing or invalid windows")
		return finishDecision(nil, "missing_price_window", scores, votes, observations, diagnostics)
	}
	if method == "least_squares" {
		var connected bool
		scores, connected, diagnostics.ResidualRMSE = leastSquaresStrength(pairs, scorePairs, pairReturns)
		diagnostics.Connected = connected
		if !connected {
			setDiagnosticGate(&diagnostics, "price_window", "failed", fmt.Sprintf("%d/%d scored pairs connected", len(scorePairs), len(pairs)), "connected configured currency graph", strings.Join(missing, ", "))
			return finishDecision(nil, "insufficient_basket", scores, votes, observations, diagnostics)
		}
	} else {
		diagnostics.Connected = graphConnected(pairs, scorePairs)
	}
	windowDetail := ""
	if len(missing) > 0 {
		windowDetail = "degraded; missing " + strings.Join(missing, ", ")
	}
	setDiagnosticGate(&diagnostics, "price_window", "passed", fmt.Sprintf("%d/%d scored", len(scorePairs), len(pairs)), "connected scoring basket", windowDetail)

	strong, weak, gap, ok := StrongestWeakest(scores)
	if !ok {
		setDiagnosticGate(&diagnostics, "rank", "failed", "no distinct extremes", "strongest and weakest", "")
		return finishDecision(nil, "no_rank", scores, votes, observations, diagnostics)
	}
	diagnostics.Strongest, diagnostics.Weakest, diagnostics.Gap = strong, weak, gap
	setDiagnosticGate(&diagnostics, "rank", "passed", strong+"/"+weak, "distinct extremes", "")
	if gap < cfg.MinGap {
		setDiagnosticGate(&diagnostics, "min_gap", "failed", fmt.Sprintf("%.6f", gap), fmt.Sprintf(">= %.6f", cfg.MinGap), "")
		return finishDecision(nil, "min_gap", scores, votes, observations, diagnostics)
	}
	setDiagnosticGate(&diagnostics, "min_gap", "passed", fmt.Sprintf("%.6f", gap), fmt.Sprintf(">= %.6f", cfg.MinGap), "")
	if votes[strong] < cfg.BroadMin || votes[weak] > -cfg.BroadMin {
		setDiagnosticGate(&diagnostics, "breadth", "failed", fmt.Sprintf("%s=%d, %s=%d", strong, votes[strong], weak, votes[weak]), fmt.Sprintf("strong >= %d, weak <= -%d", cfg.BroadMin, cfg.BroadMin), "")
		return finishDecision(nil, "breadth", scores, votes, observations, diagnostics)
	}
	setDiagnosticGate(&diagnostics, "breadth", "passed", fmt.Sprintf("%s=%d, %s=%d", strong, votes[strong], weak, votes[weak]), fmt.Sprintf("strong >= %d, weak <= -%d", cfg.BroadMin, cfg.BroadMin), "")

	pair, side, ok := RoutePair(routePairs, strong, weak)
	if !ok {
		setDiagnosticGate(&diagnostics, "route", "failed", strong+"/"+weak, "direct or inverse configured pair", "")
		return finishDecision(nil, "no_routed_pair", scores, votes, observations, diagnostics)
	}
	diagnostics.RoutedSymbol, diagnostics.RoutedSide = pair.Symbol, side
	setDiagnosticGate(&diagnostics, "route", "passed", strings.ToUpper(side)+" "+pair.Symbol, "direct or inverse configured pair", "")
	if len(cfg.TradeInstruments) > 0 && !cfg.TradeInstruments[pair.Symbol] {
		setDiagnosticGate(&diagnostics, "trade_instrument_filter", "failed", pair.Symbol, "configured trade instrument", "")
		return finishDecision(nil, "trade_instrument_filter", scores, votes, observations, diagnostics)
	}
	setDiagnosticGate(&diagnostics, "trade_instrument_filter", "passed", pair.Symbol, "configured trade instrument", "")

	confirm := pairReturns[pair.Symbol]
	if side == "sell" {
		confirm = -confirm
	}
	diagnostics.ConfirmMove = confirm
	if confirm < cfg.ConfirmMinMove {
		setDiagnosticGate(&diagnostics, "confirm_min_move", "failed", fmt.Sprintf("%.6f", confirm), fmt.Sprintf(">= %.6f", cfg.ConfirmMinMove), "")
		return finishDecision(nil, "confirm_min_move", scores, votes, observations, diagnostics)
	}
	setDiagnosticGate(&diagnostics, "confirm_min_move", "passed", fmt.Sprintf("%.6f", confirm), fmt.Sprintf(">= %.6f", cfg.ConfirmMinMove), "")

	signal := &Signal{
		Symbol:           pair.Symbol,
		Side:             side,
		Strong:           strong,
		Weak:             weak,
		Gap:              gap,
		ConfirmMove:      confirm,
		StrongVotes:      votes[strong],
		WeakVotes:        votes[weak],
		RoutedPairReturn: pairReturns[pair.Symbol],
	}
	return finishDecision(signal, "", scores, votes, observations, diagnostics)
}

func newDecisionDiagnostics(cfg Config) DecisionDiagnostics {
	d := DecisionDiagnostics{
		Version:    1,
		Thresholds: DiagnosticThresholds{MinGap: cfg.MinGap, BroadMin: cfg.BroadMin, ConfirmMinMove: cfg.ConfirmMinMove},
	}
	for _, name := range diagnosticGateNames {
		d.Gates = append(d.Gates, GateDiagnostic{Name: name, Status: "not_evaluated"})
	}
	return d
}

func setDiagnosticGate(d *DecisionDiagnostics, name, status, actual, required, detail string) {
	for i := range d.Gates {
		if d.Gates[i].Name == name {
			d.Gates[i] = GateDiagnostic{Name: name, Status: status, Actual: actual, Required: required, Detail: detail}
			return
		}
	}
}

func finishDecision(signal *Signal, reason string, scores map[string]float64, votes map[string]int, observations map[string]int, diagnostics DecisionDiagnostics) Decision {
	keys := make([]string, 0, len(scores))
	for currency := range scores {
		keys = append(keys, currency)
	}
	sort.Strings(keys)
	for _, currency := range keys {
		diagnostics.Currencies = append(diagnostics.Currencies, CurrencyDiagnostic{Currency: currency, Score: scores[currency], Votes: votes[currency], Observations: observations[currency]})
	}
	diagnostics.ReasonCode = reason
	if signal != nil {
		diagnostics.Outcome = "signal"
	} else {
		diagnostics.Outcome = "no_signal"
	}
	return Decision{Signal: signal, SkipReason: reason, Scores: scores, Votes: votes, Diagnostics: diagnostics}
}

// FormatDecisionSummary renders a concise operator explanation while retaining
// ReasonCode as the stable machine-readable value used for aggregation.
func FormatDecisionSummary(d DecisionDiagnostics, fallback string) string {
	if d.Version == 0 {
		return fallback
	}
	strongScore, strongVotes := diagnosticCurrency(d, d.Strongest)
	weakScore, weakVotes := diagnosticCurrency(d, d.Weakest)
	context := ""
	if d.Strongest != "" && d.Weakest != "" {
		context = fmt.Sprintf("; %s strongest (%+.4f, %d votes), %s weakest (%+.4f, %d votes)", d.Strongest, strongScore, strongVotes, d.Weakest, weakScore, weakVotes)
	}
	switch d.ReasonCode {
	case "min_gap":
		return fmt.Sprintf("strength gap %.4f < required %.4f%s", d.Gap, d.Thresholds.MinGap, context)
	case "breadth":
		return fmt.Sprintf("breadth insufficient: %s %d votes, %s %d votes; require at least ±%d", d.Strongest, strongVotes, d.Weakest, weakVotes, d.Thresholds.BroadMin)
	case "no_routed_pair":
		return fmt.Sprintf("no configured pair routes %s strongest against %s weakest", d.Strongest, d.Weakest)
	case "trade_instrument_filter":
		return fmt.Sprintf("candidate %s %s is not enabled for execution%s", strings.ToUpper(d.RoutedSide), d.RoutedSymbol, context)
	case "confirm_min_move":
		return fmt.Sprintf("%s %s confirmation %.4f < required %.4f%s", strings.ToUpper(d.RoutedSide), d.RoutedSymbol, d.ConfirmMove, d.Thresholds.ConfirmMinMove, context)
	case "missing_price_window":
		for _, pair := range d.Pairs {
			if !pair.Available {
				return fmt.Sprintf("missing price window for %s: %s", pair.Symbol, pair.Error)
			}
		}
	case "no_rank":
		return "currency scores are tied; no distinct strongest and weakest currencies"
	case "insufficient_basket":
		return fmt.Sprintf("usable pairs do not form a connected currency graph; missing %s", strings.Join(d.MissingPairs, ", "))
	}
	if d.Outcome == "signal" {
		return fmt.Sprintf("%s %s; gap %.4f; confirmation %.4f%s", strings.ToUpper(d.RoutedSide), d.RoutedSymbol, d.Gap, d.ConfirmMove, context)
	}
	return fallback
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func leastSquaresStrength(configured, usable []Pair, returns map[string]float64) (map[string]float64, bool, float64) {
	currencies := pairCurrencies(configured)
	if len(currencies) < 2 || !graphConnected(configured, usable) {
		return map[string]float64{}, false, 0
	}
	index := make(map[string]int, len(currencies))
	for i, currency := range currencies {
		index[currency] = i
	}
	n := len(currencies)
	a := make([][]float64, n)
	b := make([]float64, n)
	for i := range a {
		a[i] = make([]float64, n)
	}
	for _, pair := range usable {
		i, j := index[pair.Base], index[pair.Quote]
		a[i][i]++
		a[j][j]++
		a[i][j]--
		a[j][i]--
		b[i] += returns[pair.Symbol]
		b[j] -= returns[pair.Symbol]
	}
	// L + 11ᵀ/n is invertible for a connected graph and chooses the
	// zero-sum solution without singling out a reference currency.
	for i := range a {
		for j := range a[i] {
			a[i][j] += 1 / float64(n)
		}
	}
	x, ok := solveLinearSystem(a, b)
	if !ok {
		return map[string]float64{}, false, 0
	}
	scores := make(map[string]float64, n)
	for i, currency := range currencies {
		scores[currency] = x[i]
	}
	var squared float64
	for _, pair := range usable {
		residual := returns[pair.Symbol] - (scores[pair.Base] - scores[pair.Quote])
		squared += residual * residual
	}
	return scores, true, math.Sqrt(squared / float64(len(usable)))
}

func graphConnected(configured, usable []Pair) bool {
	currencies := pairCurrencies(configured)
	if len(currencies) == 0 {
		return false
	}
	adj := make(map[string][]string, len(currencies))
	for _, pair := range usable {
		adj[pair.Base] = append(adj[pair.Base], pair.Quote)
		adj[pair.Quote] = append(adj[pair.Quote], pair.Base)
	}
	seen := map[string]bool{currencies[0]: true}
	queue := []string{currencies[0]}
	for len(queue) > 0 {
		currency := queue[0]
		queue = queue[1:]
		for _, next := range adj[currency] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return len(seen) == len(currencies)
}

func pairCurrencies(pairs []Pair) []string {
	set := make(map[string]bool)
	for _, pair := range pairs {
		set[pair.Base], set[pair.Quote] = true, true
	}
	out := make([]string, 0, len(set))
	for currency := range set {
		out = append(out, currency)
	}
	sort.Strings(out)
	return out
}

func solveLinearSystem(a [][]float64, b []float64) ([]float64, bool) {
	n := len(b)
	for pivot := 0; pivot < n; pivot++ {
		best := pivot
		for row := pivot + 1; row < n; row++ {
			if math.Abs(a[row][pivot]) > math.Abs(a[best][pivot]) {
				best = row
			}
		}
		if math.Abs(a[best][pivot]) < 1e-12 {
			return nil, false
		}
		a[pivot], a[best] = a[best], a[pivot]
		b[pivot], b[best] = b[best], b[pivot]
		divisor := a[pivot][pivot]
		for col := pivot; col < n; col++ {
			a[pivot][col] /= divisor
		}
		b[pivot] /= divisor
		for row := 0; row < n; row++ {
			if row == pivot {
				continue
			}
			factor := a[row][pivot]
			for col := pivot; col < n; col++ {
				a[row][col] -= factor * a[pivot][col]
			}
			b[row] -= factor * b[pivot]
		}
	}
	return b, true
}

func diagnosticCurrency(d DecisionDiagnostics, currency string) (float64, int) {
	for _, item := range d.Currencies {
		if item.Currency == currency {
			return item.Score, item.Votes
		}
	}
	return 0, 0
}

func StrongestWeakest(scores map[string]float64) (strong, weak string, gap float64, ok bool) {
	currencies := make([]string, 0, len(scores))
	for currency := range scores {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	first := true
	var strongScore, weakScore float64
	for _, cur := range currencies {
		score := scores[cur]
		if first || score > strongScore {
			strong, strongScore = cur, score
		}
		if first || score < weakScore {
			weak, weakScore = cur, score
		}
		first = false
	}
	return strong, weak, strongScore - weakScore, strong != "" && weak != "" && strong != weak
}

func RoutePair(pairs []Pair, strong, weak string) (Pair, string, bool) {
	for _, pair := range pairs {
		if pair.Base == strong && pair.Quote == weak {
			return pair, "buy", true
		}
		if pair.Base == weak && pair.Quote == strong {
			return pair, "sell", true
		}
	}
	return Pair{}, "", false
}
