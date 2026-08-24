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
// evidence. It deliberately excludes a current partial session from indicators,
// volumes, and 52-week history; the current mark is separately supplied by the
// analysis candle or validated broker ask.
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
	// A partial current session is excluded from completed, while mark remains
	// current. In that case the third prior completed session is the correct
	// three-session reference; after the close the latest completed bar itself
	// is the mark, so it is four array positions back.
	returnIndex := n - 4
	if len(completed) < len(daily) {
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
		// The prior high intentionally excludes the latest completed session so
		// a freshly completed breakout cannot erase its own extension evidence.
		for _, candle := range completed[n-antichase.TrueHighSessions : n-1] {
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

// executionAntiChaseSnapshot refreshes the same daily evidence used during
// analysis immediately before an order is released. A missing refresh is a
// deliberate fail-closed condition for BUYs: an old analysis snapshot cannot
// prove that an entry is still timely.
func (e *Engine) executionAntiChaseSnapshot(ctx context.Context, ticker string, mark float64, source string, now time.Time) *antichase.Snapshot {
	if _, err := e.fetchAndStoreCandlesObserved(ctx, ticker, data.Timeframe1Day); err != nil {
		return &antichase.Snapshot{
			SchemaVersion: antichase.PolicyVersion, Ticker: ticker, ObservedAt: now.UTC(),
			ReferencePrice: mark, ReferenceSource: source, EvidenceStatus: "unavailable",
			HistoryScope: "limited_history", EvidenceReason: "anti-chase: unable to refresh daily evidence",
		}
	}
	daily, err := store.GetCandles(ctx, ticker, data.Timeframe1Day, now.AddDate(-2, 0, 0), now)
	if err != nil {
		return &antichase.Snapshot{
			SchemaVersion: antichase.PolicyVersion, Ticker: ticker, ObservedAt: now.UTC(),
			ReferencePrice: mark, ReferenceSource: source, EvidenceStatus: "unavailable",
			HistoryScope: "limited_history", EvidenceReason: "anti-chase: unable to load refreshed daily evidence",
		}
	}
	items, err := store.GetRecentNews(ctx, ticker, now.AddDate(0, 0, -30))
	if err != nil {
		items = nil // price/technical evidence remains valid; catalyst rule is simply absent.
	}
	news := make([]analysis.NewsSnippet, 0, len(items))
	for _, item := range items {
		news = append(news, analysis.NewsSnippet{
			Headline: item.Headline, Source: item.Source, PublishedAt: item.PublishedAt,
			Sentiment: sentimentLabel(item.SentimentScore),
		})
	}
	return buildAntiChaseSnapshot(ticker, daily, mark, source, news, now)
}

func (e *Engine) freshAntiChaseAsk(ctx context.Context, ticker string) float64 {
	if e.marketData == nil {
		return 0
	}
	quote, err := e.marketData.FetchQuote(ctx, ticker)
	if err != nil || quote == nil || quote.Bid <= 0 {
		return 0
	}
	ask, err := validateStockAsk(quote, ticker, time.Now().UTC())
	if err != nil {
		return 0
	}
	return ask
}
