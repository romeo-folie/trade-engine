package sweep

import (
	"context"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

// Runner evaluates parameter sets against the orbreplay engine. Each parameter set gets its own
// Engine (session/range/risk-reward differ per set), all sharing the same data provider and cost
// models. The provider's month cache makes concurrent runs cheap after the first touch.
type Runner struct {
	provider    data.MarketDataProvider
	baseConfig  config.ForexORBConfig
	instrument  []config.Instrument
	costs       map[string]orbreplay.CostModel
	portfolio   float64
	concurrency int
}

// NewRunner builds a Runner. concurrency bounds how many parameter sets run at once (<=0 → 1).
func NewRunner(provider data.MarketDataProvider, base config.ForexORBConfig, instruments []config.Instrument, costs map[string]orbreplay.CostModel, portfolio float64, concurrency int) *Runner {
	if concurrency <= 0 {
		concurrency = 1
	}
	return &Runner{
		provider: provider, baseConfig: base, instrument: instruments,
		costs: costs, portfolio: portfolio, concurrency: concurrency,
	}
}

// ParamResult is one parameter set's backtest outcome over a period.
type ParamResult struct {
	Params ParamSet
	Result orbreplay.Result // Overall + per-instrument metrics
	Err    error            // non-nil if the run failed (the set is skipped downstream)
}

// RunParamSet resolves a set and runs the backtest over [from, to], returning the summarised
// result (overall + per instrument).
func (r *Runner) RunParamSet(ctx context.Context, ps ParamSet, from, to time.Time) (orbreplay.Result, error) {
	cfg, insts, v := ps.Resolve(r.baseConfig, r.instrument)
	eng := orbreplay.NewEngine(cfg, r.provider, r.costs, 0)
	trades, err := eng.Run(ctx, v, from, to, insts, r.portfolio)
	if err != nil {
		return orbreplay.Result{}, err
	}
	return orbreplay.Summarize(ps.Label(), trades), nil
}

// RunAll evaluates every parameter set over [from, to] with bounded concurrency, preserving input
// order. progress (may be nil) is called after each set completes with the running done count.
// A set whose run errors is returned with Err set rather than aborting the whole sweep — one bad
// combination must not sink an overnight search.
func (r *Runner) RunAll(ctx context.Context, sets []ParamSet, from, to time.Time, progress func(done, total int)) []ParamResult {
	out := make([]ParamResult, len(sets))
	sem := make(chan struct{}, r.concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0

	for i, ps := range sets {
		if ctx.Err() != nil {
			out[i] = ParamResult{Params: ps, Err: ctx.Err()}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, ps ParamSet) {
			defer wg.Done()
			defer func() { <-sem }()
			res, err := r.RunParamSet(ctx, ps, from, to)
			out[i] = ParamResult{Params: ps, Result: res, Err: err}
			mu.Lock()
			done++
			d := done
			mu.Unlock()
			if progress != nil {
				progress(d, len(sets))
			}
		}(i, ps)
	}
	wg.Wait()
	return out
}
