// multi_timeframe.go — Issue #321: when the arbiter resolves to WAIT in a
// narrow borderline confidence band, run a second analyst pass on the
// alternate timeframe (e.g. 1h ↔ 4h, 1d → 1h) and re-arbitrate. This catches
// WAITs that flip BUY/SELL once the swing view (or the intra-day view) is
// considered. Cost is 2× tokens on borderline cases only.
package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/depth"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// multiTFCostSuffix is appended to the model name when recording token spend
// for a second-pass analysis, so the cost tracker reports a distinct bucket
// per provider/model. The cost report can sum entries with this suffix to
// measure the marginal spend of the multi-timeframe feature (Issue #321).
const multiTFCostSuffix = "#mtf2"

// shouldRunSecondPass returns true when the first arbiter result is WAIT and
// confidence falls inside the configured borderline band [min, max] (inclusive),
// the feature is enabled, this is not already a recursive call, and the
// alternate timeframe map has an entry for the current timeframe. The
// alternate-timeframe candle availability check is performed by the caller —
// shouldRunSecondPass is a pure decision function so it can be unit-tested
// without DB or provider plumbing.
func shouldRunSecondPass(cfg config.AnalysisConfig, resp analysis.AnalysisResponse, primaryTF string, isSecondPass bool) (string, bool) {
	if isSecondPass {
		return "", false // recursion guard — never trigger a third pass.
	}
	if !cfg.MultiTimeframeBorderlineEnabled {
		return "", false
	}
	if resp.Action != analysis.ActionWait {
		return "", false
	}
	min := cfg.MultiTimeframeBorderlineConfMin
	max := cfg.MultiTimeframeBorderlineConfMax
	if min == 0 && max == 0 {
		// Defensive: a zero-zero band would match nothing meaningful — skip.
		return "", false
	}
	if resp.Confidence < min || resp.Confidence > max {
		return "", false
	}
	if cfg.MultiTimeframeAltMap == nil {
		return "", false
	}
	alt, ok := cfg.MultiTimeframeAltMap[strings.ToLower(primaryTF)]
	if !ok {
		return "", false
	}
	// Normalise alt value: lowercase + trim whitespace so config like
	// `1h: "4H"` resolves to "4h" and matches the lowercase candle keys
	// used downstream (Codex/Copilot review, Issue #321 PR #336).
	alt = strings.ToLower(strings.TrimSpace(alt))
	if alt == "" {
		return "", false
	}
	return alt, true
}

// combineForArbiter merges analyst results from the primary and alternate
// timeframe passes into a single slice the arbiter can re-evaluate.
//
// Each result's Reasoning is prefixed with a "[<tf>]" tag so the arbiter
// prompt makes the timeframe attribution explicit. We intentionally do not
// touch Provider/Model because the arbiter prompt anonymises labels (Model-A
// etc.) and cost recording has already happened at the call-site.
func combineForArbiter(primaryTF, altTF string, primary, alt []analysis.AnalystResult) []analysis.AnalystResult {
	out := make([]analysis.AnalystResult, 0, len(primary)+len(alt))
	tag := func(tf string, results []analysis.AnalystResult) {
		for _, r := range results {
			cp := r
			if r.Err == nil {
				cp.Response.Reasoning = fmt.Sprintf("[%s] %s", tf, r.Response.Reasoning)
			}
			out = append(out, cp)
		}
	}
	tag(primaryTF, primary)
	tag(altTF, alt)
	return out
}

// recordMultiTFCost records second-pass token usage into a distinct cost
// tracker bucket by suffixing each model name with multiTFCostSuffix. The
// cost tracker keys spend by "provider/model"; suffixing the model keeps the
// provider attribution intact while giving us a way to slice marginal spend.
func (e *Engine) recordMultiTFCost(result analysis.AnalyzeResult) {
	if e == nil || e.costTracker == nil {
		return
	}
	if len(result.AnalystResults) > 0 {
		for _, ar := range result.AnalystResults {
			if ar.Meta.Provider == "" || ar.Meta.Model == "" {
				continue
			}
			if ar.Meta.InputTokens == 0 && ar.Meta.OutputTokens == 0 {
				continue
			}
			e.costTracker.Record(ar.Meta.Provider, ar.Meta.Model+multiTFCostSuffix,
				ar.Meta.InputTokens, ar.Meta.OutputTokens)
		}
		if result.Arbiter != nil && result.Arbiter.Meta.Provider != "" && result.Arbiter.Meta.Model != "" &&
			(result.Arbiter.Meta.InputTokens > 0 || result.Arbiter.Meta.OutputTokens > 0) {
			e.costTracker.Record(result.Arbiter.Meta.Provider, result.Arbiter.Meta.Model+multiTFCostSuffix,
				result.Arbiter.Meta.InputTokens, result.Arbiter.Meta.OutputTokens)
		}
		return
	}
	if result.Meta.Provider == "" || result.Meta.Model == "" {
		return
	}
	if result.Meta.InputTokens == 0 && result.Meta.OutputTokens == 0 {
		return
	}
	e.costTracker.Record(result.Meta.Provider, result.Meta.Model+multiTFCostSuffix,
		result.Meta.InputTokens, result.Meta.OutputTokens)
}

// runSecondTimeframePass loads candles for the alternate timeframe, recomputes
// indicators, builds an analysis request that mirrors the primary-pass request
// (same news/strategy/portfolio context, just different timeframe + indicators
// + candles), and fans out to the analysts only. Returns (result, true) on
// success or (_, false) when alt-timeframe candles are unavailable /
// insufficient — the caller should then fall back to the original WAIT.
//
// We deliberately call AnalyzeEnsembleWithSet rather than AnalyzeFull because
// the alt pass is only used to collect additional analyst votes; the caller
// then re-arbitrates over the combined primary+alt set. Calling AnalyzeFull
// here would spend an extra arbiter call (LLM cost + latency) on every
// borderline WAIT whose result is then discarded (Codex P2 / Copilot review,
// Issue #321 PR #336).
//
// `impact` is threaded through from the primary pass so the alt fan-out runs
// at the same adaptive depth tier as the primary — otherwise the alt pass
// would always consult every provider regardless of impact (Copilot review).
func (e *Engine) runSecondTimeframePass(ctx context.Context, primaryReq analysis.AnalysisRequest, altTF string, ticker string, impact depth.ImpactScore) (analysis.AnalyzeResult, bool) {
	historyFrom := time.Now().AddDate(-2, 0, 0)
	candles, err := store.GetCandles(ctx, ticker, data.Timeframe(altTF), historyFrom, time.Now())
	if err != nil {
		logger.Debug("multi-tf: alt candles fetch failed — falling back",
			"ticker", ticker, "alt_tf", altTF, "err", err.Error())
		return analysis.AnalyzeResult{}, false
	}
	if len(candles) < 20 {
		logger.Debug("multi-tf: insufficient alt-timeframe candles — falling back",
			"ticker", ticker, "alt_tf", altTF, "count", len(candles))
		return analysis.AnalyzeResult{}, false
	}
	set, err := indicators.Compute(candles)
	if err != nil {
		logger.Debug("multi-tf: alt-timeframe indicators failed — falling back",
			"ticker", ticker, "alt_tf", altTF, "err", err.Error())
		return analysis.AnalyzeResult{}, false
	}

	// Build candle snapshots — mirror the primary pass's cap so prompt size is
	// comparable.
	candleCap := e.cfg.Analysis.MaxCandlesPerPrompt
	if candleCap <= 0 {
		candleCap = 20
	}
	if windows := e.cfg.Analysis.HistoryWindowByStrategy; len(windows) > 0 {
		if n, ok := windows[primaryReq.Strategy]; ok && n > 0 {
			candleCap = n
		}
	}
	if max := e.cfg.Analysis.MaxCandlesPerPrompt; max > 0 && candleCap > max {
		candleCap = max
	}
	limit := len(candles)
	if limit > candleCap {
		limit = candleCap
	}
	snaps := make([]analysis.CandleSnapshot, 0, limit)
	for i := len(candles) - 1; i >= 0 && len(snaps) < limit; i-- {
		c := candles[i]
		snaps = append(snaps, analysis.CandleSnapshot{
			Timestamp: c.Timestamp, Open: c.Open, High: c.High, Low: c.Low,
			Close: c.Close, Volume: c.Volume, VWAP: c.VWAP,
		})
	}

	altReq := primaryReq
	altReq.Timeframe = altTF
	altReq.RecentCandles = snaps
	altReq.Indicators = analysis.IndicatorSnapshot{
		EMA20: set.EMA20, EMA50: set.EMA50, EMA200: set.EMA200,
		SMA20: set.SMA20, SMA50: set.SMA50, SMA200: set.SMA200,
		RSI:           set.RSI,
		MACDLine:      set.MACDLine,
		MACDSignal:    set.MACDSignal,
		MACDHistogram: set.MACDHistogram,
		BBUpper:       set.BBUpper,
		BBMiddle:      set.BBMiddle,
		BBLower:       set.BBLower,
		ADX:           set.ADX, ATR: set.ATR, OBV: set.OBV,
		StochK: set.StochK, StochD: set.StochD,
		VWAP:     set.VWAP,
		VolSMA20: set.VolumeSMA20,
		VolRatio: volRatio(candles, set.VolumeSMA20),
	}

	// Reuse the primary pass's impact score so the alt fan-out runs at the
	// same adaptive depth tier — otherwise alt would always consult every
	// provider regardless of impact (Copilot review, Issue #321 PR #336).
	//
	// We call AnalyzeEnsembleWithSet directly (not AnalyzeFull) so the alt
	// pass produces analyst votes only — no per-timeframe arbiter call. The
	// caller re-arbitrates once over the combined primary+alt set, so a
	// per-timeframe arbiter here would be discarded LLM spend (Codex P2).
	providers := e.analyzer.Providers()
	activeAnalysts := providers
	if impact.Depth > 0 && impact.Depth < len(providers) {
		activeAnalysts = providers[:impact.Depth]
	}
	if len(activeAnalysts) == 0 {
		logger.Debug("multi-tf: no analysts available for alt pass — falling back",
			"ticker", ticker, "alt_tf", altTF)
		return analysis.AnalyzeResult{}, false
	}
	altAnalysts, err := e.analyzer.AnalyzeEnsembleWithSet(ctx, altReq, activeAnalysts)
	if err != nil {
		// AnalyzeEnsembleWithSet returns a non-nil slice alongside an error
		// when fewer than 2 analysts succeeded. Log and check usability.
		logger.Warn("multi-tf: alt-timeframe analysis returned error",
			"ticker", ticker, "alt_tf", altTF, "err", err.Error())
		if len(altAnalysts) == 0 {
			return analysis.AnalyzeResult{}, false
		}
	}
	return analysis.AnalyzeResult{AnalystResults: altAnalysts}, true
}

// reArbitrate runs the configured arbiter over the combined analyst set and
// returns the synthesised result. When no arbiter is configured (or the
// arbiter is unavailable) it returns an error so the caller can preserve the
// original WAIT. There is intentionally no majority-vote fallback here:
// majorityVote lives in internal/analysis and is unexported, and a borderline
// re-arbitration without a real arbiter would simply re-emit a WAIT-tilted
// vote, which is what we already had.
func reArbitrate(ctx context.Context, analyzer *analysis.Analyzer, req analysis.AnalysisRequest, combined []analysis.AnalystResult) (analysis.AnalyzeResult, error) {
	if analyzer == nil {
		return analysis.AnalyzeResult{}, fmt.Errorf("nil analyzer")
	}
	arb := analyzer.Arbiter()
	if arb == nil || !arb.IsAvailable() {
		// No arbiter configured/available — surface as an error so the caller
		// keeps the original WAIT instead of inventing a fallback signal.
		return analysis.AnalyzeResult{AnalystResults: combined}, fmt.Errorf("no arbiter available for re-arbitration")
	}
	arbResult, err := analysis.Arbitrate(ctx, arb, req, combined)
	if err != nil {
		return analysis.AnalyzeResult{AnalystResults: combined}, err
	}
	return analysis.AnalyzeResult{
		Response:       arbResult.Final,
		Meta:           arbResult.Meta,
		AnalystResults: combined,
		Arbiter:        &arbResult,
	}, nil
}
