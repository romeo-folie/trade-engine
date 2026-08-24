package engine

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/strategy/antichase"
)

const antiChaseDailyHistoryCandles = 260

func antiChasePolicy(cfg config.AntiChaseConfig) antichase.Policy {
	return antichase.Policy{
		Enabled:                   cfg.Enabled,
		StaleCatalystRunupEnabled: cfg.StaleCatalystRunupEnabled,
		StaleAfterHours:           cfg.StaleAfterHours,
		MinReturn3SessionPct:      cfg.MinReturn3SessionPct,
		WeakVolumeRatioMax:        cfg.WeakVolumeRatioMax,
		StaleEMAExtensionATR:      cfg.StaleEMAExtensionATR,
		PriorHighExtensionEnabled: cfg.PriorHighExtensionEnabled,
		NearPriorHighPct:          cfg.NearPriorHighPct,
		HighEMAExtensionATR:       cfg.HighEMAExtensionATR,
		MomentumExhaustionEnabled: cfg.MomentumExhaustionEnabled,
		RSIMin:                    cfg.RSIMin,
		StochKMin:                 cfg.StochKMin,
		ExhaustionReturn3Pct:      cfg.ExhaustionReturn3Pct,
	}
}

// buildAntiChaseSnapshot turns full daily history into compact, immutable prompt
// evidence. It deliberately excludes a current partial session from indicators
// and volumes; the current mark is separately supplied by the analysis candle
// or validated broker ask. During a live session, yesterday is a completed,
// legitimate prior-high observation and remains in the 52-week history.
func buildAntiChaseSnapshot(
	ticker string,
	daily []data.Candle,
	mark float64,
	markSource string,
	news []analysis.NewsSnippet,
	now time.Time,
) *antichase.Snapshot {
	s := &antichase.Snapshot{
		SchemaVersion: antichase.PolicyVersion,
		Ticker:        ticker, ObservedAt: now.UTC(), ReferencePrice: mark,
		ReferenceSource: markSource, EvidenceStatus: "unavailable", HistoryScope: "limited_history",
	}
	completed := completedEntryDailyCandles(daily, now)
	s.CompletedSessions = len(completed)
	if len(completed) < antichase.MinimumCompletedSessions {
		s.EvidenceReason = fmt.Sprintf("anti-chase: only %d completed daily sessions are available; need %d", len(completed), antichase.MinimumCompletedSessions)
		return s
	}
	if !antiChaseDailyHistoryFresh(completed, now) {
		expected := MostRecentCompletedMarketSession(now, 0).Date.In(nyLoc).Format("2006-01-02")
		latest := completed[len(completed)-1].Timestamp
		latestDate := "unknown"
		if !latest.IsZero() {
			latestDate = latest.In(nyLoc).Format("2006-01-02")
		}
		s.EvidenceReason = fmt.Sprintf("anti-chase: latest completed daily session %s does not match expected session %s", latestDate, expected)
		return s
	}
	if mark <= 0 || math.IsNaN(mark) || math.IsInf(mark, 0) {
		s.EvidenceReason = "anti-chase: reference price is unavailable"
		return s
	}
	set, err := indicators.Compute(completed)
	if err != nil || set == nil || set.EMA20 <= 0 || set.ATR <= 0 || set.BBUpper <= 0 {
		s.EvidenceReason = "anti-chase: daily indicators are unavailable"
		return s
	}
	n := len(completed)
	var priorVolume float64
	for _, candle := range completed[n-21 : n-1] {
		priorVolume += float64(candle.Volume)
	}
	if priorVolume <= 0 || completed[n-1].Volume <= 0 {
		s.EvidenceReason = "anti-chase: completed-session volume evidence is unavailable"
		return s
	}
	// During a live equity session the reference mark is intraday even when a
	// provider returns completed daily bars only, so compare it to the third
	// prior completed session. Once the session is closed, the latest completed
	// daily close itself is the mark and the three-session reference is four
	// array positions back.
	returnIndex := n - 4
	if IsMarketOpen(now) {
		returnIndex = n - 3
	}
	returnRef := completed[returnIndex].Close
	if returnRef <= 0 {
		s.EvidenceReason = "anti-chase: three-session reference close is unavailable"
		return s
	}
	s.EvidenceStatus = "available"
	s.EMA20, s.ATR14 = set.EMA20, set.ATR
	s.EMAExtensionATR = (mark - set.EMA20) / set.ATR
	s.RSI14, s.StochK, s.BBUpper = set.RSI, set.StochK, set.BBUpper
	s.VolumeRatio = float64(completed[n-1].Volume) / (priorVolume / 20)
	s.Return3SessionPct = (mark - returnRef) / returnRef * 100
	if n >= antichase.TrueHighSessions {
		s.HistoryScope = "true_52w"
		// At the close the reference mark is the latest completed close, so omit
		// that bar to preserve breakout-extension evidence. During a live
		// session the mark is intraday and the latest completed bar is yesterday,
		// which must remain part of the prior-high history.
		highEnd := n - 1
		if IsMarketOpen(now) {
			highEnd = n
		}
		for _, candle := range completed[n-antichase.TrueHighSessions : highEnd] {
			if candle.High > s.PriorHigh52Week {
				s.PriorHigh52Week = candle.High
			}
		}
		if s.PriorHigh52Week > 0 {
			s.PriorHighProximity = mark / s.PriorHigh52Week
		}
	}
	for _, item := range news {
		if item.PublishedAt.IsZero() || item.PublishedAt.After(now) || item.Sentiment != "bullish" {
			continue
		}
		if s.CatalystAt == nil || item.PublishedAt.After(*s.CatalystAt) {
			at := item.PublishedAt.UTC()
			s.CatalystAt, s.CatalystSource = &at, item.Source
		}
	}
	if s.CatalystAt != nil {
		s.CatalystAgeHours = now.Sub(*s.CatalystAt).Hours()
	}
	return s
}

// antiChaseDailyHistoryFresh proves that daily indicators end at the latest
// completed US market session. A non-empty cached window is not enough: stale
// EMA, volume, return, and high evidence must not release a fresh BUY.
func antiChaseDailyHistoryFresh(completed []data.Candle, now time.Time) bool {
	if len(completed) == 0 {
		return false
	}
	latest := completed[len(completed)-1].Timestamp
	if latest.IsZero() {
		return false
	}
	expected := MostRecentCompletedMarketSession(now, 0)
	return sameDate(latest, expected.Date)
}

// buildAnalysisAntiChaseSnapshot keeps a failed catalyst read distinct from a
// confirmed empty result. Prompt-time evidence is persisted and gates BUYs, so
// an enabled stale-catalyst rule must fail closed rather than audit an empty
// article set as proof that no catalyst exists.
func buildAnalysisAntiChaseSnapshot(
	ticker string,
	daily []data.Candle,
	mark float64,
	markSource string,
	news []analysis.NewsSnippet,
	newsErr error,
	staleCatalystEnabled bool,
	now time.Time,
) *antichase.Snapshot {
	if newsErr != nil && staleCatalystEnabled {
		return &antichase.Snapshot{
			SchemaVersion:   antichase.PolicyVersion,
			Ticker:          ticker,
			ObservedAt:      now.UTC(),
			ReferencePrice:  mark,
			ReferenceSource: markSource,
			EvidenceStatus:  "unavailable",
			HistoryScope:    "limited_history",
			EvidenceReason:  "anti-chase: unable to load timestamped catalyst evidence",
		}
	}
	return buildAntiChaseSnapshot(ticker, daily, mark, markSource, news, now)
}

func antiChaseEntryGate(snapshot *antichase.Snapshot, policy antichase.Policy) (string, antichase.Decision) {
	decision := antichase.Evaluate(*snapshot, policy)
	switch decision.Status {
	case "blocked", "unavailable_block":
		return decision.Reason, decision
	default:
		return "", decision
	}
}

func (e *Engine) historyCandleLimit(tf data.Timeframe) int {
	if tf == data.Timeframe1Day && e.cfg != nil && e.cfg.Risk.EntryGates.AntiChase.Enabled {
		return antiChaseDailyHistoryCandles
	}
	return historyCandles
}

// executionAntiChaseEvidence refreshes daily evidence before the final quote
// is fetched. The final ask must be validated after this potentially slow work
// so the timing decision and submitted market order share the same mark.
func (e *Engine) executionAntiChaseEvidence(ctx context.Context, ticker string, now time.Time, staleCatalystEnabled bool) ([]data.Candle, []analysis.NewsSnippet, string) {
	if _, err := e.fetchAndStoreCandlesObserved(ctx, ticker, data.Timeframe1Day); err != nil {
		return nil, nil, "anti-chase: unable to refresh daily evidence"
	}
	daily, err := store.GetCandles(ctx, ticker, data.Timeframe1Day, now.AddDate(-2, 0, 0), now)
	if err != nil {
		return nil, nil, "anti-chase: unable to load refreshed daily evidence"
	}
	items, err := store.GetRecentNews(ctx, ticker, now.AddDate(0, 0, -30))
	news, reason := antiChaseNewsEvidence(items, err, staleCatalystEnabled)
	if reason != "" {
		return nil, nil, reason
	}
	return daily, news, ""
}

// antiChaseNewsEvidence treats a read failure differently from a confirmed
// empty result. A stale-catalyst rule cannot be evaluated safely when its
// timestamped evidence was unavailable, so the execution gate must fail closed.
func antiChaseNewsEvidence(items []data.NewsItem, err error, staleCatalystEnabled bool) ([]analysis.NewsSnippet, string) {
	if err != nil {
		if staleCatalystEnabled {
			return nil, "anti-chase: unable to load timestamped catalyst evidence"
		}
		items = nil
	}
	news := make([]analysis.NewsSnippet, 0, len(items))
	for _, item := range items {
		news = append(news, analysis.NewsSnippet{
			Headline: item.Headline, Source: item.Source, PublishedAt: item.PublishedAt,
			Sentiment: sentimentLabel(item.SentimentScore),
		})
	}
	return news, ""
}

func (e *Engine) freshAntiChaseQuote(ctx context.Context, ticker string, now time.Time) (*data.Quote, float64) {
	if e.marketData == nil {
		return nil, 0
	}
	quote, err := e.marketData.FetchQuote(ctx, ticker)
	if err != nil || quote == nil || quote.Bid <= 0 {
		return nil, 0
	}
	ask, err := validateStockAsk(quote, ticker, now)
	if err != nil {
		return nil, 0
	}
	return quote, ask
}

func (e *Engine) freshAntiChaseAsk(ctx context.Context, ticker string) float64 {
	_, ask := e.freshAntiChaseQuote(ctx, ticker, time.Now().UTC())
	return ask
}
