package currencystrength

import (
	"fmt"
	"math"
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
}

type Config struct {
	MinGap           float64
	BroadMin         int
	ConfirmMinMove   float64
	TradeInstruments map[string]bool
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
	Signal     *Signal
	SkipReason string
	Scores     map[string]float64
	Votes      map[string]int
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

	for _, pair := range pairs {
		window, ok := windows[pair.Symbol]
		if !ok || window.Start <= 0 || window.End <= 0 {
			return Decision{SkipReason: "missing_price_window", Scores: scores, Votes: votes}
		}
		ret := math.Log(window.End / window.Start)
		pairReturns[pair.Symbol] = ret
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

	strong, weak, gap, ok := StrongestWeakest(scores)
	if !ok {
		return Decision{SkipReason: "no_rank", Scores: scores, Votes: votes}
	}
	if gap < cfg.MinGap {
		return Decision{SkipReason: "min_gap", Scores: scores, Votes: votes}
	}
	if votes[strong] < cfg.BroadMin || votes[weak] > -cfg.BroadMin {
		return Decision{SkipReason: "breadth", Scores: scores, Votes: votes}
	}

	pair, side, ok := RoutePair(pairs, strong, weak)
	if !ok {
		return Decision{SkipReason: "no_routed_pair", Scores: scores, Votes: votes}
	}
	if len(cfg.TradeInstruments) > 0 && !cfg.TradeInstruments[pair.Symbol] {
		return Decision{SkipReason: "trade_instrument_filter", Scores: scores, Votes: votes}
	}

	confirm := pairReturns[pair.Symbol]
	if side == "sell" {
		confirm = -confirm
	}
	if confirm < cfg.ConfirmMinMove {
		return Decision{SkipReason: "confirm_min_move", Scores: scores, Votes: votes}
	}

	return Decision{
		Signal: &Signal{
			Symbol:           pair.Symbol,
			Side:             side,
			Strong:           strong,
			Weak:             weak,
			Gap:              gap,
			ConfirmMove:      confirm,
			StrongVotes:      votes[strong],
			WeakVotes:        votes[weak],
			RoutedPairReturn: pairReturns[pair.Symbol],
		},
		Scores: scores,
		Votes:  votes,
	}
}

func StrongestWeakest(scores map[string]float64) (strong, weak string, gap float64, ok bool) {
	first := true
	var strongScore, weakScore float64
	for cur, score := range scores {
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
