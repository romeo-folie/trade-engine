package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/strategy/antichase"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

// unifiedTimeframeEvidence loads each configured supplemental timeframe before
// the ensemble starts. A missing configured context is an explicit failure,
// rather than silently recreating the old primary-pass / confirmation-pass
// split with incomplete evidence.
func (e *Engine) unifiedTimeframeEvidence(ctx context.Context, ticker string, workflow timeframeWorkflow, req analysis.AnalysisRequest, from time.Time, refreshErrors map[data.Timeframe]error) ([]analysis.TimeframeContext, map[string][]data.Candle, error) {
	contexts := make([]analysis.TimeframeContext, 0, len(workflow.FetchTimeframes)-1)
	candlesByTimeframe := make(map[string][]data.Candle)
	for _, raw := range workflow.FetchTimeframes {
		tf := data.Timeframe(strings.ToLower(strings.TrimSpace(raw)))
		if tf == "" || tf == workflow.PrimaryTimeframe {
			continue
		}
		if err := timeframeRefreshError(tf, refreshErrors); err != nil {
			return nil, nil, fmt.Errorf("refresh %s evidence: %w", tf, err)
		}
		candles, err := store.GetCandles(ctx, ticker, tf, from, time.Now())
		if err != nil {
			return nil, nil, fmt.Errorf("load %s evidence: %w", tf, err)
		}
		if len(candles) < 20 {
			return nil, nil, fmt.Errorf("need 20 %s candles; got %d", tf, len(candles))
		}
		set, err := indicators.Compute(candles)
		if err != nil {
			return nil, nil, fmt.Errorf("compute %s indicators: %w", tf, err)
		}
		limit := promptCandleLimit(len(candles), e.cfg.Analysis.MaxCandlesPerPrompt,
			e.cfg.Analysis.HistoryWindowByStrategy, req.Strategy, req.LensSet())
		snaps := make([]analysis.CandleSnapshot, 0, limit)
		for i := len(candles) - 1; i >= 0 && len(snaps) < limit; i-- {
			c := candles[i]
			snaps = append(snaps, analysis.CandleSnapshot{Timestamp: c.Timestamp, Open: c.Open, High: c.High, Low: c.Low, Close: c.Close, Volume: c.Volume, VWAP: c.VWAP})
		}
		role := "supporting"
		if tf == workflow.EntryTimingTimeframe {
			role = "entry_timing"
		}
		contexts = append(contexts, analysis.TimeframeContext{
			Timeframe: string(tf), Role: role, RecentCandles: snaps,
			Indicators: indicatorSnapshot(set, candles, req.Indicators.TimingSnapshot),
		})
		candlesByTimeframe[string(tf)] = candles
	}
	return contexts, candlesByTimeframe, nil
}

func timeframeRefreshError(tf data.Timeframe, refreshErrors map[data.Timeframe]error) error {
	if refreshErrors == nil {
		return nil
	}
	return refreshErrors[tf]
}

func indicatorSnapshot(set *indicators.IndicatorSet, candles []data.Candle, timing *antichase.Snapshot) analysis.IndicatorSnapshot {
	snapshot := analysis.IndicatorSnapshot{
		EMA20: set.EMA20, EMA50: set.EMA50, EMA200: set.EMA200,
		SMA20: set.SMA20, SMA50: set.SMA50, SMA200: set.SMA200, RSI: set.RSI,
		MACDLine: set.MACDLine, MACDSignal: set.MACDSignal, MACDHistogram: set.MACDHistogram,
		BBUpper: set.BBUpper, BBMiddle: set.BBMiddle, BBLower: set.BBLower,
		ADX: set.ADX, ATR: set.ATR, OBV: set.OBV, StochK: set.StochK, StochD: set.StochD,
		VWAP: set.VWAP, VolSMA20: set.VolumeSMA20, VolRatio: volRatio(candles, set.VolumeSMA20),
	}
	snapshot.TimingSnapshot = timing
	return snapshot
}

func timeframeAuditContexts(primary string, workflow timeframeWorkflow, contexts []analysis.TimeframeContext, evidenceErr error) []store.TimeframePassAudit {
	out := []store.TimeframePassAudit{{Timeframe: primary, Role: "primary_thesis", Status: "included"}}
	byTimeframe := make(map[string]analysis.TimeframeContext, len(contexts))
	for _, context := range contexts {
		byTimeframe[context.Timeframe] = context
	}
	for _, raw := range workflow.FetchTimeframes {
		timeframe := strings.ToLower(strings.TrimSpace(raw))
		if timeframe == "" || timeframe == primary {
			continue
		}
		if context, ok := byTimeframe[timeframe]; ok {
			out = append(out, store.TimeframePassAudit{Timeframe: context.Timeframe, Role: context.Role, Status: "included"})
			continue
		}
		role := "supporting"
		if data.Timeframe(timeframe) == workflow.EntryTimingTimeframe {
			role = "entry_timing"
		}
		reason := "supplemental timeframe evidence unavailable"
		if evidenceErr != nil {
			reason = evidenceErr.Error()
		}
		out = append(out, store.TimeframePassAudit{Timeframe: timeframe, Role: role, Status: "unavailable", Reason: reason})
	}
	return out
}

func decisiveReason(resp analysis.AnalysisResponse) string {
	if reason := strings.TrimSpace(resp.DecisiveReason); reason != "" {
		return reason
	}
	return strings.TrimSpace(strings.SplitN(resp.Reasoning, "\n", 2)[0])
}

func finalLensSuppressionReason(audit analysis.LensSelectionAudit) string {
	switch audit.SelectionStatus {
	case analysis.LensSelectionInvalid:
		return "Entry suppressed because the selected lens is not valid for this scanner profile."
	case analysis.LensSelectionNone:
		return "Entry suppressed because no configured lens fits the current regime."
	default:
		return "Entry suppressed because the ensemble did not produce a valid selected lens."
	}
}

func promptCandleLimit(available, configuredMax int, windows map[string]int, strategy string, lenses []string) int {
	candleCap := configuredMax
	if candleCap <= 0 {
		candleCap = 20
	}
	if n := widestHistoryWindow(windows, strategy, lenses); n > 0 {
		candleCap = n
	}
	if configuredMax > 0 && candleCap > configuredMax {
		candleCap = configuredMax
	}
	if available < candleCap {
		return available
	}
	return candleCap
}

// timeframeWorkflow gives each analysis timeframe a stable role. Profile-owned
// workflows use the scanner contract's thesis timeframe first and reserve an
// optional distinct timeframe for entry confirmation. Legacy rows retain their
// current ordered-timeframe behaviour until they receive a valid profile.
//
// This is deliberately a pure planning type. The execution of a second pass
// and combination of its result are introduced separately, so defining the
// contract cannot change live analysis scheduling by itself.
type timeframeWorkflow struct {
	PrimaryTimeframe     data.Timeframe
	EntryTimingTimeframe data.Timeframe
	FetchTimeframes      []string
	ProfileOwned         bool
}

// resolveTimeframeWorkflow prefers a valid execution profile over the legacy
// watchlist timeframe list. Invalid or absent profiles deliberately fall back
// to the exact legacy normalization and primary-selection rules, preserving
// existing rows while profile migrations are rolled out.
func resolveTimeframeWorkflow(legacyTimeframes []string, profile *tradeprofile.ExecutionProfile) timeframeWorkflow {
	if profile != nil {
		if canonical, err := profile.Canonicalize(); err == nil {
			primary := profileTimeframe(canonical.PrimaryTimeframe)
			fetchTimeframes := []string{string(primary)}
			entryTiming := profileTimeframe(canonical.EntryTimingTimeframe)
			if entryTiming != "" {
				fetchTimeframes = append(fetchTimeframes, string(entryTiming))
			}
			return timeframeWorkflow{
				PrimaryTimeframe:     primary,
				EntryTimingTimeframe: entryTiming,
				FetchTimeframes:      fetchTimeframes,
				ProfileOwned:         true,
			}
		}
	}

	requested := stockTimeframesForEntry(legacyTimeframes, false)
	return timeframeWorkflow{
		PrimaryTimeframe: primaryStockTimeframe(requested),
		FetchTimeframes:  requested,
	}
}

func profileTimeframe(value string) data.Timeframe {
	return data.Timeframe(strings.ToLower(strings.TrimSpace(value)))
}

func profileMomentumPlay(timeframe, source string, profile *tradeprofile.ExecutionProfile) bool {
	if profile != nil {
		if canonical, err := profile.Canonicalize(); err == nil {
			return canonical.PrimaryTimeframe == "4H" || canonical.PrimaryTimeframe == "1H"
		}
	}
	return source == "scanner"
}

func populatePrimaryWorkflowAudit(workflow *store.TimeframeWorkflowAudit, resp analysis.AnalysisResponse, audit analysis.LensSelectionAudit) {
	workflow.Primary.Action = string(resp.Action)
	workflow.Primary.Confidence = resp.Confidence
	workflow.Primary.SelectedLens = audit.SelectedLens
}
