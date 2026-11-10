package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/strategy/antichase"
)

// Short entry-gate names recorded in entry_gate_decisions (#599 PR 4).
const (
	shortGateAntiChase   = "short_anti_chase"
	shortGateTrend       = "short_trend_alignment"
	shortGateRunup       = "short_runup_anti_chase"
	shortGateReleaseName = "_release"
)

// shortGateEvidence is the daily and news evidence the short gates read. It
// is gathered before broker admission (network work) and evaluated against
// the admission bid afterwards (pure). Daily and news evidence are
// independent: a news outage must not discard good daily bars that the
// trend and run-up gates need.
type shortGateEvidence struct {
	Daily []data.Candle
	News  []analysis.NewsSnippet
	// Reason is non-empty when the anti-chase evidence (daily, or news when
	// the stale-catalyst rule needs it) is unavailable.
	Reason string
	// DailyReason is non-empty when the daily bars could not be refreshed.
	DailyReason string
	PrevClose   float64 // prior completed daily close; 0 = unknown
}

// gatherShortGateEvidence refreshes the daily bars, and the news only when
// the stale-catalyst rule will actually read it.
func (e *Engine) gatherShortGateEvidence(ctx context.Context, ticker string, now time.Time) shortGateEvidence {
	eg := e.cfg.Risk.EntryGates
	if !eg.AntiChase.Enabled && !eg.TrendAlignmentEnabled && eg.NewsMinRunupPct <= 0 && eg.MomentumMaxRunupPct <= 0 {
		return shortGateEvidence{}
	}
	var ev shortGateEvidence
	if _, err := e.fetchAndStoreCandlesObserved(ctx, ticker, data.Timeframe1Day); err != nil {
		ev.DailyReason = "unable to refresh daily evidence"
	} else if daily, err := store.GetCandles(ctx, ticker, data.Timeframe1Day, now.AddDate(-2, 0, 0), now); err != nil {
		ev.DailyReason = "unable to load refreshed daily evidence"
	} else {
		ev.Daily = daily
		ev.PrevClose = priorSessionClose(daily, now)
	}
	if ev.DailyReason != "" {
		ev.Reason = "anti-chase: " + ev.DailyReason
	}
	needNews := eg.AntiChase.Enabled && eg.AntiChase.StaleCatalystRunupEnabled
	if needNews && ev.Reason == "" {
		if err := e.fetchAndStoreNews(ctx, ticker); err != nil {
			ev.Reason = "anti-chase: unable to refresh timestamped catalyst evidence"
		} else {
			items, err := store.GetRecentNews(ctx, ticker, now.Add(-antiChaseNewsLookback))
			ev.News, ev.Reason = antiChaseNewsEvidence(items, err, true)
		}
	}
	return ev
}

// priorSessionClose is the close of the newest daily bar dated before today
// (New York). It matches the long gate's store.GetPrevDailyClose: after 16:00
// the completed-bar window includes today's bar, whose close is the mark
// itself, not the prior close.
func priorSessionClose(daily []data.Candle, now time.Time) float64 {
	today := now.In(nyLoc).Format("2006-01-02")
	for i := len(daily) - 1; i >= 0; i-- {
		c := daily[i]
		if c.Close > 0 && !c.Timestamp.IsZero() && c.Timestamp.In(nyLoc).Format("2006-01-02") < today {
			return c.Close
		}
	}
	return 0
}

// evaluateShortGates applies the mirrored entry gates to a prospective SHORT
// at mark. It returns the first block reason (empty when every gate passes),
// the failed check, and every gate's audit decision in a fixed order.
func evaluateShortGates(
	cfg config.EntryGatesConfig,
	ticker, strategy string,
	mark float64,
	markSource string,
	ev shortGateEvidence,
	now time.Time,
	release bool,
) (string, string, []store.EntryGateDecision) {
	suffix := ""
	if release {
		suffix = shortGateReleaseName
	}
	var decisions []store.EntryGateDecision
	blockReason, failedCheck := "", ""
	record := func(name, status, reason string, details []byte) {
		decisions = append(decisions, store.EntryGateDecision{Name: name + suffix, Status: status, Reason: reason, Details: details})
		if status == "blocked" && blockReason == "" {
			blockReason, failedCheck = reason, name+suffix
		}
	}

	// 1. Anti-chase: never short an extended, exhausted, or stale-news selloff.
	if cfg.AntiChase.Enabled {
		policy := antiChasePolicy(cfg.AntiChase)
		var snapshot *antichase.Snapshot
		if ev.Reason != "" {
			snapshot = &antichase.Snapshot{
				SchemaVersion: antichase.PolicyVersion, Ticker: ticker, ObservedAt: now.UTC(),
				ReferencePrice: mark, ReferenceSource: markSource, EvidenceStatus: "unavailable",
				HistoryScope: "limited_history", EvidenceReason: ev.Reason, Side: antichase.SideShort,
			}
		} else {
			snapshot = buildShortAntiChaseSnapshot(ticker, ev.Daily, mark, markSource, ev.News, now)
		}
		reason, decision := antiChaseEntryGate(snapshot, policy)
		details, _ := json.Marshal(struct {
			Snapshot *antichase.Snapshot `json:"snapshot"`
			Decision antichase.Decision  `json:"decision"`
		}{snapshot, decision})
		status := "passed"
		if reason != "" {
			status = "blocked"
		}
		record(shortGateAntiChase, status, reason, details)
	} else {
		record(shortGateAntiChase, "disabled", "", nil)
	}

	// 2. Trend alignment: a short requires a proven daily downtrend.
	if cfg.TrendAlignmentEnabled {
		reason := evaluateShortTrendGate(mark, ev.Daily, now)
		if ev.DailyReason != "" {
			reason = "trend alignment (short): " + ev.DailyReason
		}
		status := "passed"
		if reason != "" {
			status = "blocked"
		}
		record(shortGateTrend, status, reason, nil)
	} else {
		record(shortGateTrend, "disabled", "", nil)
	}

	// 3. Run-up gates mirrored: no chasing a drop, and the tape must confirm
	// a bearish headline. Like the long gates, unknown prior close fails open.
	if cfg.NewsMinRunupPct > 0 || cfg.MomentumMaxRunupPct > 0 {
		reason := evaluateShortRunupGates(strategy, mark, ev.PrevClose, cfg)
		status := entryGateAuditStatus(ev.PrevClose > 0, reason)
		record(shortGateRunup, status, reason, nil)
	}
	return blockReason, failedCheck, decisions
}

// evaluateShortTrendGate passes a SHORT only in a proven daily downtrend:
// price below both EMA9 and EMA21, and both EMAs below their prior
// completed-session values (the exact condition the long gate blocks on).
// Unlike the long alpha filter, missing evidence blocks: a short must prove
// the downtrend (#599 §4).
func evaluateShortTrendGate(mark float64, daily []data.Candle, now time.Time) string {
	completed := completedEntryDailyCandles(daily, now)
	if mark <= 0 || len(completed) < entryTrendSlowEMAPeriod+1 {
		return "trend alignment (short): daily downtrend evidence is unavailable"
	}
	// A downtrend must be proven on current history: the window has to end at
	// the latest completed session with no missing sessions, or an old
	// downtrend could authorize a short after the trend reversed.
	window := completed[len(completed)-(entryTrendSlowEMAPeriod+1):]
	if !antiChaseDailyHistoryFresh(completed, now) || !antiChaseDailyHistoryContinuous(window, now) {
		return "trend alignment (short): daily history is stale or has missing sessions"
	}
	ema9 := indicators.ComputeEMA(completed, entryTrendFastEMAPeriod)
	ema21 := indicators.ComputeEMA(completed, entryTrendSlowEMAPeriod)
	previous := completed[:len(completed)-1]
	prevEMA9 := indicators.ComputeEMA(previous, entryTrendFastEMAPeriod)
	prevEMA21 := indicators.ComputeEMA(previous, entryTrendSlowEMAPeriod)
	if ema9 <= 0 || ema21 <= 0 || prevEMA9 <= 0 || prevEMA21 <= 0 {
		return "trend alignment (short): daily downtrend evidence is unavailable"
	}
	if mark < ema9 && mark < ema21 && ema9 < prevEMA9 && ema21 < prevEMA21 {
		return ""
	}
	return fmt.Sprintf(
		"trend alignment (short): price %.2f vs daily EMA9 %.2f (prev %.2f) and EMA21 %.2f (prev %.2f) is not a confirmed downtrend — short weakness in weakness only",
		mark, ema9, prevEMA9, ema21, prevEMA21,
	)
}

// evaluateShortRunupGates mirrors evaluateRunupGates for a SHORT.
func evaluateShortRunupGates(strategy string, mark, prevClose float64, cfg config.EntryGatesConfig) string {
	if mark <= 0 || prevClose <= 0 {
		return ""
	}
	movePct := (mark - prevClose) / prevClose * 100
	if strategy == "news_sentiment" && cfg.NewsMinRunupPct > 0 && movePct > -cfg.NewsMinRunupPct {
		return fmt.Sprintf("news tape confirmation (short): price %+.2f%% vs prior close, needs <= -%.2f%% — market has not confirmed the bearish headline", movePct, cfg.NewsMinRunupPct)
	}
	if strategy == "momentum" && cfg.MomentumMaxRunupPct > 0 && movePct < -cfg.MomentumMaxRunupPct {
		return fmt.Sprintf("momentum anti-chase (short): price %+.2f%% vs prior close, max -%.2f%% — entry would chase the drop", movePct, cfg.MomentumMaxRunupPct)
	}
	return ""
}
