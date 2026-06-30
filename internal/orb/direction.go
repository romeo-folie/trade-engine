package orb

import (
	"context"
	"log/slog"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/indicators"
)

// directionEligibility (Phase 3) reports which breakout directions are aligned with the
// trend, so the strategy stops placing blind dual-sided brackets and only arms the side a
// single wick can't fake. Long is eligible when the latest price is at/above the
// trend-timeframe EMA, short when at/below. ok=false when the EMA can't be computed (thin
// data) — the caller then leaves BOTH sides eligible (fail open), matching the entry-quality
// filters: a missing feed must never silently halt trading.
//
// At a price exactly on the EMA both sides are eligible (a flat trend gives no edge to
// either), preserving the dual-sided default rather than dropping the session.
func (s *ORBScheduler) directionEligibility(ctx context.Context, ticker string) (longOK, shortOK, ok bool) {
	if s.marketData == nil || s.forexCfg == nil {
		return false, false, false
	}
	period := s.forexCfg.TrendEMAPeriod
	if period <= 0 {
		period = 20
	}
	tf := data.Timeframe(s.forexCfg.TrendTimeframe)
	if tf == "" {
		tf = data.Timeframe1Hour
	}
	to := s.now()
	from := to.Add(-atrLookback(period, tf)) // reuse the weekend-padded lookback
	candles, err := s.marketData.FetchCandles(ctx, ticker, tf, from, to)
	if err != nil || len(candles) < period {
		slog.Warn("orb: direction eligibility: trend data unavailable — leaving both sides eligible", "ticker", ticker)
		return false, false, false
	}
	emaVal := indicators.ComputeEMA(candles, period)
	if emaVal <= 0 {
		return false, false, false
	}
	last := candles[len(candles)-1].Close
	return last >= emaVal, last <= emaVal, true
}

// directionDecision returns the latched direction eligibility for a ticker: the trend is
// evaluated ONCE (the first executing tick, at range lock) and cached for the rest of the
// session. Otherwise, with confirmation modes keeping the scheduler in Executing across
// ticks, the trend EMA would drift as new candles print and could admit a side that was
// ineligible at the decision point — diverging from the replay, which decides once. A
// fail-open result (ok=false) is latched as both-eligible so it isn't re-evaluated either.
func (s *ORBScheduler) directionDecision(ctx context.Context, ticker string) (longOK, shortOK bool) {
	if d, ok := s.directionDecisions[ticker]; ok {
		return d[0], d[1]
	}
	long, short, ok := s.directionEligibility(ctx, ticker)
	if !ok {
		long, short = true, true // fail open, latched for the session
	}
	s.directionDecisions[ticker] = [2]bool{long, short}
	return long, short
}
