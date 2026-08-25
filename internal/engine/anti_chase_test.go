package engine

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestSignalNeedsAdmissionRetry(t *testing.T) {
	decisions, err := json.Marshal([]store.EntryGateDecision{
		{Name: "universal_anti_chase_release", Status: "passed"},
		{Name: store.AdmissionRetryGateName, Status: "retry_pending_auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !signalNeedsAdmissionRetry(store.Signal{Status: "pending", Deferred: true, EntryGateDecisions: decisions}) {
		t.Fatal("admission retry marker was not recognized")
	}
	if signalNeedsAdmissionRetry(store.Signal{Status: "pending", Deferred: true, EntryGateDecisions: []byte(`[{"name":"broker_admission_refresh","status":"manual_reapproval_required"}]`)}) {
		t.Fatal("manual reapproval was treated as an automatic retry")
	}
	manual := []byte(`[{"name":"broker_admission_refresh","status":"manual_reapproval_required"}]`)
	if !signalRequiresManualAdmissionReapproval(store.Signal{Status: "pending", EntryGateDecisions: manual}) {
		t.Fatal("manual admission retry marker was not recognized")
	}
	if signalRequiresManualAdmissionReapproval(store.Signal{Status: "executed", EntryGateDecisions: manual}) {
		t.Fatal("historical manual retry marker remained active after execution")
	}
	if signalRequiresManualAdmissionReapproval(store.Signal{Status: "pending", Deferred: true, EntryGateDecisions: manual}) {
		t.Fatal("automatic queue signal required manual reapproval")
	}
	if signalNeedsAdmissionRetry(store.Signal{Status: "executed", Deferred: true, EntryGateDecisions: decisions}) {
		t.Fatal("historical automatic retry marker remained active after execution")
	}
	if signalNeedsAdmissionRetry(store.Signal{Status: "pending", EntryGateDecisions: decisions}) {
		t.Fatal("nondeferred pending signal was treated as automatic retry")
	}
	if signalNeedsAdmissionRetry(store.Signal{Status: "pending", Deferred: true, EntryGateDecisions: []byte(`[{"name":"broker_admission_refresh","status":"passed"}]`)}) {
		t.Fatal("completed admission refresh was treated as retry-pending")
	}
	if signalNeedsAdmissionRetry(store.Signal{Status: "pending", Deferred: true, EntryGateDecisions: []byte(`not-json`)}) {
		t.Fatal("malformed audit JSON was treated as a retry marker")
	}
}

type failingNewsProvider struct{ calls int }

func (p *failingNewsProvider) FetchNews(context.Context, string, time.Time) ([]data.NewsItem, error) {
	p.calls++
	return nil, errors.New("provider unavailable")
}

func antiChaseDailyCandles(count int, now time.Time) []data.Candle {
	candles := make([]data.Candle, count)
	latest := MostRecentCompletedMarketSession(now, 0)
	sessions := make([]USMarketSession, count)
	for i := count - 1; i >= 0; i-- {
		sessions[i] = latest
		for candidate := latest.Date.AddDate(0, 0, -1); ; candidate = candidate.AddDate(0, 0, -1) {
			if previous, ok := MarketSessionForDate(candidate); ok {
				latest = previous
				break
			}
		}
	}
	for i, session := range sessions {
		closePrice := 50 + float64(i)*.1
		candles[i] = data.Candle{
			Ticker: "TEST", Timeframe: data.Timeframe1Day,
			Timestamp: session.Close, Open: closePrice - .2,
			High: closePrice + .5, Low: closePrice - .5, Close: closePrice,
			Volume: 1000,
		}
	}
	return candles
}

func TestBuildAntiChaseSnapshotUsesCompletedDailyHistory(t *testing.T) {
	now := time.Date(2026, time.August, 24, 22, 0, 0, 0, time.UTC)
	candles := antiChaseDailyCandles(254, now)
	// The first of 253 prior sessions must be included, while the latest close
	// (the reference mark) must be excluded from its own prior-high window.
	candles[0].High = 200
	candles[len(candles)-1].High = 300
	mark := candles[len(candles)-1].Close
	snapshot := buildAntiChaseSnapshot("TEST", candles, mark, "test", nil, now)
	if snapshot.EvidenceStatus != "available" {
		t.Fatalf("snapshot evidence = %q (%s)", snapshot.EvidenceStatus, snapshot.EvidenceReason)
	}
	if snapshot.HistoryScope != "true_52w" || snapshot.CompletedSessions != 254 {
		t.Fatalf("history scope=%q sessions=%d, want true_52w/254", snapshot.HistoryScope, snapshot.CompletedSessions)
	}
	if snapshot.PriorHigh52Week != 200 || snapshot.PriorHighProximity <= 0 {
		t.Fatalf("prior high evidence = %.2f/%.3f, want 200.00 from the 253 prior sessions", snapshot.PriorHigh52Week, snapshot.PriorHighProximity)
	}
}

func TestBuildAntiChaseSnapshotAfterCloseRequires253PriorSessions(t *testing.T) {
	now := time.Date(2026, time.August, 24, 22, 0, 0, 0, time.UTC)
	candles := antiChaseDailyCandles(253, now)
	// The newest candle is the close being evaluated, leaving only 252 earlier
	// observations. It must not be presented as a full prior-52-week scope.
	snapshot := buildAntiChaseSnapshot("TEST", candles, candles[len(candles)-1].Close, "test", nil, now)
	if snapshot.HistoryScope != "limited_history" || snapshot.PriorHigh52Week != 0 || snapshot.PriorHighProximity != 0 {
		t.Fatalf("after-close 253-bar history = %+v, want limited prior-high evidence", snapshot)
	}
}

func TestBuildAntiChaseSnapshotFailsClosedForShortHistory(t *testing.T) {
	now := time.Date(2026, time.August, 24, 18, 0, 0, 0, time.UTC)
	candles := antiChaseDailyCandles(20, now)
	snapshot := buildAntiChaseSnapshot("TEST", candles, 55, "test", nil, now)
	if snapshot.EvidenceStatus != "unavailable" || snapshot.CompletedSessions != 20 {
		t.Fatalf("short history snapshot = %+v", snapshot)
	}
}

func TestBuildAntiChaseSnapshotPartialSessionUsesThreeSessionReference(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc)
	candles := antiChaseDailyCandles(253, now)
	// Today's incomplete bar follows, rather than replaces, the latest
	// completed Friday session.
	candles = append(candles, data.Candle{Ticker: "TEST", Timeframe: data.Timeframe1Day, Timestamp: now, Open: 100, High: 101, Low: 99, Close: 100, Volume: 1000})
	mark := 100.0
	snapshot := buildAntiChaseSnapshot("TEST", candles, mark, "test", nil, now)
	if snapshot.EvidenceStatus != "available" || snapshot.CompletedSessions != 253 {
		t.Fatalf("partial-session snapshot = %+v", snapshot)
	}
	want := (mark - candles[250].Close) / candles[250].Close * 100
	if math.Abs(snapshot.Return3SessionPct-want) > 1e-9 {
		t.Fatalf("three-session return = %.12f, want %.12f", snapshot.Return3SessionPct, want)
	}
}

func TestBuildAntiChaseSnapshotLiveSessionUsesThreeSessionReferenceWithoutPartialBar(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc)
	candles := antiChaseDailyCandles(253, now)
	mark := 100.0
	snapshot := buildAntiChaseSnapshot("TEST", candles, mark, "test", nil, now)
	if snapshot.EvidenceStatus != "available" || snapshot.CompletedSessions != len(candles) {
		t.Fatalf("completed-bar live-session snapshot = %+v", snapshot)
	}
	want := (mark - candles[250].Close) / candles[250].Close * 100
	if math.Abs(snapshot.Return3SessionPct-want) > 1e-9 {
		t.Fatalf("three-session return = %.12f, want %.12f", snapshot.Return3SessionPct, want)
	}
}

func TestBuildAntiChaseSnapshotLiveSessionIncludesYesterdayInPriorHigh(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc)
	candles := antiChaseDailyCandles(253, now)
	// Today's mark is intraday, so yesterday remains a valid prior session even
	// when it set a higher wick than its close.
	candles[len(candles)-1].High = 150
	snapshot := buildAntiChaseSnapshot("TEST", candles, 145, "test", nil, now)
	if snapshot.PriorHigh52Week != 150 {
		t.Fatalf("live-session prior high = %.2f, want yesterday's 150.00", snapshot.PriorHigh52Week)
	}
}

func TestBuildAntiChaseSnapshotIncludesEarlyCloseBarAfterClose(t *testing.T) {
	now := time.Date(2024, time.November, 29, 14, 0, 0, 0, nyLoc) // Black Friday, 13:00 close.
	candles := antiChaseDailyCandles(253, now)
	candles[len(candles)-1].Timestamp = now
	mark := candles[len(candles)-1].Close
	snapshot := buildAntiChaseSnapshot("TEST", candles, mark, "test", nil, now)
	if snapshot.CompletedSessions != len(candles) {
		t.Fatalf("early-close completed sessions = %d, want %d", snapshot.CompletedSessions, len(candles))
	}
	want := (mark - candles[249].Close) / candles[249].Close * 100
	if math.Abs(snapshot.Return3SessionPct-want) > 1e-9 {
		t.Fatalf("early-close three-session return = %.12f, want %.12f", snapshot.Return3SessionPct, want)
	}
}

func TestBuildAntiChaseSnapshotRejectsStaleDailyHistory(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc) // Monday; Friday is latest completed session.
	candles := antiChaseDailyCandles(253, now)
	candles[len(candles)-1].Timestamp = candles[len(candles)-2].Timestamp // Thursday instead of Friday.
	snapshot := buildAntiChaseSnapshot("TEST", candles, 100, "test", nil, now)
	if snapshot.EvidenceStatus != "unavailable" || snapshot.EvidenceReason == "" {
		t.Fatalf("stale history snapshot = %+v", snapshot)
	}
}

func TestBuildAntiChaseSnapshotRejectsUntimestampedDailyHistory(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc)
	candles := antiChaseDailyCandles(253, now)
	candles[len(candles)-1].Timestamp = time.Time{}
	snapshot := buildAntiChaseSnapshot("TEST", candles, 100, "test", nil, now)
	if snapshot.EvidenceStatus != "unavailable" {
		t.Fatalf("untimestamped history snapshot = %+v", snapshot)
	}
}

func TestBuildAnalysisAntiChaseSnapshotFailsClosedOnlyForEnabledCatalystRule(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc)
	candles := antiChaseDailyCandles(253, now)
	failed := buildAnalysisAntiChaseSnapshot("TEST", candles, 100, "test", nil, context.DeadlineExceeded, true, now)
	if failed.EvidenceStatus != "unavailable" || failed.EvidenceReason == "" {
		t.Fatalf("enabled catalyst rule accepted unread news: %+v", failed)
	}
	disabled := buildAnalysisAntiChaseSnapshot("TEST", candles, 100, "test", nil, context.DeadlineExceeded, false, now)
	if disabled.EvidenceStatus != "available" {
		t.Fatalf("disabled catalyst rule should retain daily evidence: %+v", disabled)
	}
	empty := buildAnalysisAntiChaseSnapshot("TEST", candles, 100, "test", nil, nil, true, now)
	if empty.EvidenceStatus != "available" {
		t.Fatalf("empty successful news read should remain valid: %+v", empty)
	}
}

func TestAnalysisAntiChaseMarkRequiresCurrentCandleDuringLiveSession(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc)
	yesterday := MostRecentCompletedMarketSession(now, 0).Close
	observation := func(candle data.Candle, timeframe data.Timeframe) candleFetchObservation {
		return candleFetchObservation{ObservedAt: now, Ticker: "TEST", Timeframe: timeframe, LatestFetched: &candle}
	}
	if mark, source := analysisAntiChaseMark("TEST", []data.Candle{{Close: 100, Timestamp: yesterday}}, data.Timeframe1Day, now, observation(data.Candle{Close: 100, Timestamp: yesterday}, data.Timeframe1Day)); mark != 0 || source != "analysis_current_mark_unavailable" {
		t.Fatalf("completed-only live mark = %.2f/%s, want unavailable", mark, source)
	}
	if mark, source := analysisAntiChaseMark("TEST", []data.Candle{{Close: 101, Timestamp: now.Add(-time.Minute)}}, data.Timeframe1Hour, now, observation(data.Candle{Close: 101, Timestamp: now.Add(-time.Minute)}, data.Timeframe1Hour)); mark != 101 || source != "analysis_current_candle" {
		t.Fatalf("current-session mark = %.2f/%s, want current candle", mark, source)
	}
	if mark, source := analysisAntiChaseMark("TEST", []data.Candle{{Close: 101, Timestamp: now.Add(-2*time.Hour - time.Minute)}}, data.Timeframe1Hour, now, observation(data.Candle{Close: 101, Timestamp: now.Add(-2*time.Hour - time.Minute)}, data.Timeframe1Hour)); mark != 0 || source != "analysis_current_mark_unavailable" {
		t.Fatalf("stale current-session mark = %.2f/%s, want unavailable", mark, source)
	}
	afterClose := time.Date(2026, time.August, 24, 17, 0, 0, 0, nyLoc)
	if mark, source := analysisAntiChaseMark("TEST", []data.Candle{{Close: 100, Timestamp: yesterday}}, data.Timeframe1Day, afterClose, candleFetchObservation{}); mark != 100 || source != "analysis_latest_close" {
		t.Fatalf("post-close mark = %.2f/%s, want completed close", mark, source)
	}
}

func TestAnalysisAntiChaseMarkUsesCurrentFetchNotCachedDailyPartial(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc)
	cached := []data.Candle{{Close: 150, Timestamp: now.Add(-6 * time.Hour)}}
	if mark, source := analysisAntiChaseMark("TEST", cached, data.Timeframe1Day, now, candleFetchObservation{}); mark != 0 || source != "analysis_current_mark_unavailable" {
		t.Fatalf("cached daily partial after failed fetch = %.2f/%s, want unavailable", mark, source)
	}
	yesterday := MostRecentCompletedMarketSession(now, 0).Close
	if mark, source := analysisAntiChaseMark("TEST", cached, data.Timeframe1Day, now, candleFetchObservation{
		ObservedAt: now, Ticker: "TEST", Timeframe: data.Timeframe1Day,
		LatestFetched: &data.Candle{Close: 100, Timestamp: yesterday},
	}); mark != 0 || source != "analysis_current_mark_unavailable" {
		t.Fatalf("completed-only daily refresh = %.2f/%s, want unavailable", mark, source)
	}
	fresh := data.Candle{Close: 101, Timestamp: now.Add(-time.Minute)}
	if mark, source := analysisAntiChaseMark("TEST", cached, data.Timeframe1Day, now, candleFetchObservation{
		ObservedAt: now, Ticker: "TEST", Timeframe: data.Timeframe1Day, LatestFetched: &fresh,
	}); mark != 101 || source != "analysis_current_candle" {
		t.Fatalf("current-cycle daily mark = %.2f/%s, want 101/current", mark, source)
	}
}

func TestFreshAntiChaseAskRejectsStaleQuote(t *testing.T) {
	now := time.Now().UTC()
	engine := &Engine{marketData: &pullbackEvidenceMarket{
		quote: &data.Quote{Ticker: "TEST", Bid: 99, Ask: 100, Timestamp: now.Add(-time.Minute)},
	}}
	if got := engine.freshAntiChaseAsk(context.Background(), "TEST"); got != 0 {
		t.Fatalf("stale quote ask = %.2f, want rejected", got)
	}
	engine.marketData = &pullbackEvidenceMarket{
		quote: &data.Quote{Ticker: "TEST", Bid: 99, Ask: 100, Timestamp: now},
	}
	if got := engine.freshAntiChaseAsk(context.Background(), "TEST"); got != 100 {
		t.Fatalf("fresh quote ask = %.2f, want 100", got)
	}
}

func TestFreshAntiChaseQuoteValidatesAtPostFetchClock(t *testing.T) {
	at := time.Date(2026, time.August, 24, 15, 0, 0, 0, time.UTC)
	market := &pullbackEvidenceMarket{quotes: []*data.Quote{{Ticker: "TEST", Bid: 99, Ask: 100, Timestamp: at}}}
	engine := &Engine{marketData: market}
	if quote, ask := engine.freshAntiChaseQuoteWithClock(context.Background(), "TEST", func() time.Time {
		if market.quoteCall == 0 {
			t.Fatal("validation clock ran before FetchQuote")
		}
		return at.Add(stockAdmissionMaxAge + time.Nanosecond)
	}); quote != nil || ask != 0 {
		t.Fatalf("post-fetch stale quote accepted: quote=%+v ask=%.2f", quote, ask)
	}
	if quote, ask := engine.freshAntiChaseQuoteWithClock(context.Background(), "TEST", func() time.Time {
		return at.Add(stockAdmissionMaxAge)
	}); quote == nil || ask != 100 {
		t.Fatalf("freshness-boundary quote rejected: quote=%+v ask=%.2f", quote, ask)
	}
}

func TestValidateFinalExecutionQuote(t *testing.T) {
	at := time.Date(2026, time.August, 24, 15, 0, 0, 0, time.UTC)
	quote := &data.Quote{Ticker: "TEST", Bid: 99, Ask: 100, Timestamp: at}
	if err := validateFinalExecutionQuote(true, quote, "TEST", at.Add(stockAdmissionMaxAge)); err != nil {
		t.Fatalf("freshness-boundary quote rejected: %v", err)
	}
	if err := validateFinalExecutionQuote(true, quote, "TEST", at.Add(stockAdmissionMaxAge+time.Nanosecond)); err == nil {
		t.Fatal("expired final quote was accepted")
	}
	if err := validateFinalExecutionQuote(false, nil, "TEST", at); err != nil {
		t.Fatalf("unrequired final quote rejected: %v", err)
	}
	if err := validateFinalExecutionQuote(true, &data.Quote{Ticker: "OTHER", Bid: 99, Ask: 100, Timestamp: at}, "TEST", at); err == nil {
		t.Fatal("wrong-ticker final quote was accepted")
	}
}

func TestStaleSignalPriceMove(t *testing.T) {
	if delta, stale := staleSignalPriceMove(100, 102, 2); stale || delta != 2 {
		t.Fatalf("threshold-boundary move = %.2f/%t, want 2/false", delta, stale)
	}
	if delta, stale := staleSignalPriceMove(100, 97.9, 2); !stale || delta <= 2 {
		t.Fatalf("downward move = %.2f/%t, want stale", delta, stale)
	}
	if _, stale := staleSignalPriceMove(100, 110, 0); stale {
		t.Fatal("disabled stale-price threshold rejected a move")
	}
}

func TestNeedsFreshStaleSignalQuote(t *testing.T) {
	tests := []struct {
		name              string
		admission         stockEntryAdmission
		antiChase         bool
		threshold         float64
		wantFreshQuote    bool
		wantFinalValidate bool
	}{
		{name: "nonbroker stale-only", threshold: 2, wantFreshQuote: true, wantFinalValidate: true},
		{name: "anti-chase already acquires quote", antiChase: true, threshold: 2, wantFinalValidate: true},
		{name: "disabled threshold preserves legacy price", threshold: 0},
		{name: "broker admission supplies execution ask", admission: stockEntryAdmission{BrokerAuthoritative: true}, threshold: 2},
		{name: "broker-held replay skips fresh release checks", admission: stockEntryAdmission{BrokerAuthoritative: true, BrokerHeldReplay: true}, threshold: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsFreshStaleSignalQuote(tt.admission, tt.antiChase, tt.threshold); got != tt.wantFreshQuote {
				t.Fatalf("needs fresh stale quote = %t, want %t", got, tt.wantFreshQuote)
			}
			if got := requiresFinalExecutionQuote(tt.admission, tt.antiChase, tt.threshold); got != tt.wantFinalValidate {
				t.Fatalf("requires final execution quote validation = %t, want %t", got, tt.wantFinalValidate)
			}
		})
	}
}

func TestAntiChaseNewsEvidenceFailsClosedOnlyForEnabledCatalystRule(t *testing.T) {
	if _, reason := antiChaseNewsEvidence(nil, context.DeadlineExceeded, true); reason == "" {
		t.Fatal("missing catalyst evidence passed with stale-catalyst rule enabled")
	}
	if news, reason := antiChaseNewsEvidence(nil, context.DeadlineExceeded, false); reason != "" || len(news) != 0 {
		t.Fatalf("disabled catalyst rule should tolerate missing news: news=%v reason=%q", news, reason)
	}
}

func TestSentimentLabelUsesSignedFinnhubScale(t *testing.T) {
	tests := []struct {
		score float64
		want  string
	}{
		{score: 0.4, want: "bullish"},
		{score: signedBullishSentimentMin, want: "neutral"},
		{score: 0, want: "neutral"},
		{score: signedBearishSentimentMax, want: "neutral"},
		{score: -0.4, want: "bearish"},
	}
	for _, tt := range tests {
		if got := sentimentLabel(tt.score); got != tt.want {
			t.Errorf("sentimentLabel(%v) = %q, want %q", tt.score, got, tt.want)
		}
	}
}

func TestAntiChaseNewsEvidenceKeepsSignedBullishCatalyst(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, nyLoc)
	news, reason := antiChaseNewsEvidence([]data.NewsItem{{
		Headline: "strong results", Source: "Finnhub", PublishedAt: now.Add(-48 * time.Hour), SentimentScore: 0.4,
	}}, nil, true)
	if reason != "" || len(news) != 1 || news[0].Sentiment != "bullish" {
		t.Fatalf("signed bullish catalyst = %#v, reason=%q", news, reason)
	}
	snapshot := buildAntiChaseSnapshot("TEST", antiChaseDailyCandles(253, now), 100, "test", news, now)
	if snapshot.CatalystAt == nil {
		t.Fatalf("signed bullish catalyst was omitted from snapshot: %+v", snapshot)
	}
}

func TestNewsLookbackCoversEnabledStaleCatalystPolicy(t *testing.T) {
	if got := newsLookback(false, true); got != defaultNewsLookback {
		t.Fatalf("disabled anti-chase lookback = %s, want %s", got, defaultNewsLookback)
	}
	if got := newsLookback(true, false); got != defaultNewsLookback {
		t.Fatalf("disabled catalyst rule lookback = %s, want %s", got, defaultNewsLookback)
	}
	if got := newsLookback(true, true); got != antiChaseNewsLookback {
		t.Fatalf("enabled stale catalyst lookback = %s, want %s", got, antiChaseNewsLookback)
	}
}

func TestNewsRefreshCoversOnlyItsSuccessfulEvidenceWindow(t *testing.T) {
	now := time.Now()
	short := newsRefresh{at: now.Add(-time.Minute), lookback: defaultNewsLookback}
	if newsRefreshCovers(short, now, antiChaseNewsLookback) {
		t.Fatal("48-hour refresh was accepted as 30-day catalyst evidence")
	}
	long := newsRefresh{at: now.Add(-time.Minute), lookback: antiChaseNewsLookback}
	if !newsRefreshCovers(long, now, defaultNewsLookback) || !newsRefreshCovers(long, now, antiChaseNewsLookback) {
		t.Fatal("30-day successful refresh did not cover shorter and equal windows")
	}
	if newsRefreshCovers(newsRefresh{at: now.Add(-newsFetchInterval)}, now, defaultNewsLookback) {
		t.Fatal("expired refresh remained within cadence")
	}
}

func TestFetchAndStoreNewsRetriesFailedStaleCatalystRefresh(t *testing.T) {
	cfg := config.Defaults()
	cfg.Risk.EntryGates.AntiChase.Enabled = true
	cfg.Risk.EntryGates.AntiChase.StaleCatalystRunupEnabled = true
	provider := &failingNewsProvider{}
	engine := &Engine{cfg: cfg, newsProvider: provider, newsLastFetched: make(map[string]newsRefresh)}
	if err := engine.fetchAndStoreNews(context.Background(), "TEST"); err == nil {
		t.Fatal("failed catalyst refresh was accepted")
	}
	if !engine.newsLastFetched["TEST"].at.IsZero() {
		t.Fatal("failed catalyst refresh advanced cadence state")
	}
	if err := engine.fetchAndStoreNews(context.Background(), "TEST"); err == nil || provider.calls != 2 {
		t.Fatalf("failed catalyst refresh was cadence-suppressed: calls=%d err=%v", provider.calls, err)
	}
}

func TestPromptNewsSnippetsExcludesOlderTimingEvidence(t *testing.T) {
	now := time.Date(2026, time.August, 24, 15, 0, 0, 0, time.UTC)
	items := []analysis.NewsSnippet{
		{Headline: "newest", PublishedAt: now.Add(-time.Hour)},
		{Headline: "old catalyst", PublishedAt: now.Add(-72 * time.Hour)},
	}
	prompt := promptNewsSnippets(items, now, 5)
	if len(prompt) != 1 || prompt[0].Headline != "newest" {
		t.Fatalf("prompt news = %#v, want only 48-hour item", prompt)
	}
}
