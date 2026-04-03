package orb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/depth"
	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// nyLoc is the US/Eastern timezone for ORB time gates.
// LoadLocation failure is fatal: the EST fallback is DST-unaware and would
// shift all time gates by 1 hour during summer, causing missed or misfired scans.
var nyLoc = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		// tzdata package not embedded and system timezone DB missing.
		// This is a hard misconfiguration — do not silently fall back to fixed EST.
		panic(fmt.Sprintf("orb: cannot load America/New_York timezone: %v\n"+
			"Hint: import _ \"time/tzdata\" in main or ensure the system tzdata is installed.", err))
	}
	return loc
}()

// maxNewsPerTicker caps the number of news items passed to the LLM per candidate.
// Limits context window usage while keeping the most recent catalyst signals.
const maxNewsPerTicker = 10

// sentimentPositiveThreshold and sentimentNegativeThreshold define the boundaries
// used when mapping a Finnhub float sentiment score to a POSITIVE/NEUTRAL/NEGATIVE label.
const (
	sentimentPositiveThreshold = 0.1
	sentimentNegativeThreshold = -0.1
)

// State represents the ORB scheduler's current phase.
type State string

const (
	StateIdle           State = "idle"
	StateScanning       State = "scanning"
	StateCandidateReady State = "candidate_ready"
	StateRangeWatch     State = "range_watch"
	StateExecuting      State = "executing"
	StateDone           State = "done"
)

// ScoredCandidate pairs a gap mover with its LLM analysis result.
type ScoredCandidate struct {
	Gap   data.GapCandidate `json:"gap"`
	Score ORBAnalysisResult `json:"score"`
}

// PortfolioValuer provides the current portfolio value for position sizing.
type PortfolioValuer interface {
	PortfolioValue() float64
}

// Notifier delivers ORB notifications (e.g. Telegram).
type Notifier interface {
	Notify(ctx context.Context, msg string) error
}

// ORBScheduler implements the time-gated ORB state machine.
type ORBScheduler struct {
	cfg        config.ORBConfig
	db         *pgxpool.Pool
	marketData *data.AlpacaClient
	analyzer   *analysis.Analyzer
	executor   execution.Executor
	portfolio  PortfolioValuer
	notifier   Notifier
	news       data.NewsProvider // optional; nil disables news enrichment
	bus        *events.Bus       // optional; nil means no event publishing
	now        func() time.Time  // injectable for tests

	mu           sync.Mutex // protects state, session, candidates for TUI reads
	state        State
	session      *store.ORBSession
	candidates   []ScoredCandidate
	rangeBuffers map[string]*RangeBuffer
	orderIDs     map[string]string // ticker → orderID
	lastScan     time.Time         // Eastern Time of the last successful scan
}

// NewORBScheduler creates a new ORB scheduler.
func NewORBScheduler(
	cfg config.ORBConfig,
	db *pgxpool.Pool,
	marketData *data.AlpacaClient,
	analyzer *analysis.Analyzer,
	executor execution.Executor,
	portfolio PortfolioValuer,
	notifier Notifier,
) *ORBScheduler {
	return &ORBScheduler{
		cfg:          cfg,
		db:           db,
		marketData:   marketData,
		analyzer:     analyzer,
		executor:     executor,
		portfolio:    portfolio,
		notifier:     notifier,
		now:          time.Now,
		state:        StateIdle,
		rangeBuffers: make(map[string]*RangeBuffer),
		orderIDs:     make(map[string]string),
	}
}

// SetNewsProvider wires a news source for pre-market catalyst enrichment.
// Call before Run. If not called, the LLM scores on price/volume data only.
func (s *ORBScheduler) SetNewsProvider(news data.NewsProvider) {
	s.news = news
}

// SetBus wires an event bus so the ORB scheduler publishes pipeline events.
// Call before Run. If not called, events are silently dropped (safe for tests).
func (s *ORBScheduler) SetBus(bus *events.Bus) {
	s.bus = bus
}

// publish emits an event on the bus. No-op when bus is nil.
func (s *ORBScheduler) publish(t events.Type, payload any) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(t, payload)
}

// Run starts the ORB scheduler loop. Blocks until ctx is cancelled.
func (s *ORBScheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.tick(ctx); err != nil {
				fmt.Printf("orb tick error: %v\n", err)
			}
		}
	}
}

// CurrentState returns the current scheduler state (thread-safe for TUI reads).
func (s *ORBScheduler) CurrentState() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Session returns the current session (thread-safe for TUI reads).
func (s *ORBScheduler) Session() *store.ORBSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

// Candidates returns the current scored candidates (thread-safe for TUI reads).
func (s *ORBScheduler) Candidates() []ScoredCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ScoredCandidate, len(s.candidates))
	copy(out, s.candidates)
	return out
}

func (s *ORBScheduler) tick(ctx context.Context) error {
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()

	now := s.now()
	et := now.In(nyLoc)

	switch state {
	case StateIdle:
		return s.tickIdle(ctx, et)
	case StateScanning:
		return s.tickScanning(ctx, et)
	case StateCandidateReady:
		return s.tickCandidateReady(ctx, et)
	case StateRangeWatch:
		return s.tickRangeWatch(ctx, et)
	case StateExecuting:
		return s.tickExecuting(ctx)
	case StateDone:
		return s.tickDone(ctx, et)
	}
	return nil
}

func (s *ORBScheduler) setState(state State) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
}

func (s *ORBScheduler) tickIdle(ctx context.Context, et time.Time) error {
	// Skip weekends and market holidays (IsTradingDay uses the engine's calendar).
	if et.Weekday() == time.Saturday || et.Weekday() == time.Sunday || !IsTradingDay(s.now()) {
		return nil
	}
	h, m, _ := et.Clock()
	etMinutes := h*60 + m
	if etMinutes < 6*60 {
		return nil
	}

	today := time.Date(et.Year(), et.Month(), et.Day(), 0, 0, 0, 0, nyLoc)
	existing, err := store.GetORBSessionByDate(ctx, s.db, today)
	if err == nil && existing != nil {
		if existing.Status == "done" || existing.Status == "skipped" {
			return nil
		}
		s.mu.Lock()
		s.session = existing
		s.mu.Unlock()
	}

	s.mu.Lock()
	hasSession := s.session != nil
	s.mu.Unlock()

	if !hasSession {
		session, err := store.CreateORBSession(ctx, s.db, today)
		if err != nil {
			return fmt.Errorf("create orb session: %w", err)
		}
		s.mu.Lock()
		s.session = session
		s.mu.Unlock()
	}

	s.setState(StateScanning)
	return nil
}

func (s *ORBScheduler) tickScanning(ctx context.Context, et time.Time) error {
	h, m, _ := et.Clock()
	etMinutes := h*60 + m

	// 1. Strict Cutoff: stop scanning once the market opens.
	// Once 9:30 AM ET is reached, we can no longer add new candidates to the session
	// as the Opening Range window has already begun.
	if etMinutes >= 9*60+30 {
		s.notify(ctx, "ORB: Pre-market scan window closed. No candidates found today.")
		s.setState(StateDone)
		s.mu.Lock()
		session := s.session
		s.mu.Unlock()
		if session != nil {
			if err := store.UpdateORBSession(ctx, s.db, session.ID, "skipped", nil, nil); err != nil {
				return fmt.Errorf("update session to skipped: %w", err)
			}
		}
		return nil
	}

	// 2. Scan Cadence: minimize LLM costs by only scanning every 2 minutes.
	// The scheduler ticks every 30s, but gap movers and news don't shift that fast.
	if !s.lastScan.IsZero() && et.Sub(s.lastScan) < 2*time.Minute {
		return nil
	}

	gaps, err := data.FetchGapMovers(ctx, s.marketData, s.cfg.GapThresholdPct)
	if err != nil {
		return fmt.Errorf("scan gap movers: %w", err)
	}
	s.lastScan = et

	if len(gaps) == 0 {
		return nil
	}

	// LLM-score each candidate
	var scored []ScoredCandidate
	for _, gap := range gaps {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Fetch recent news for this ticker to give the LLM catalyst context.
		// Without news, catalyst_type/catalyst_quality are unreliable since the
		// model can only see gap magnitude and pre-market volume.
		var newsItems []prompts.ORBNewsItem
		if s.news != nil {
			since := s.now().Add(-time.Duration(s.cfg.NewsLookbackHours) * time.Hour)
			rawNews, newsErr := s.news.FetchNews(ctx, gap.Ticker, since)
			if newsErr != nil {
				fmt.Printf("orb: news fetch %s: %v\n", gap.Ticker, newsErr)
			} else {
				// Cap to most recent N items to avoid context window blowout on
				// high-volume tickers. FetchNews returns newest-first, so a simple
				// slice preserves recency ordering.
				if len(rawNews) > maxNewsPerTicker {
					rawNews = rawNews[:maxNewsPerTicker]
				}
				for _, n := range rawNews {
					sentiment := "NEUTRAL"
					if n.SentimentScore > sentimentPositiveThreshold {
						sentiment = "POSITIVE"
					} else if n.SentimentScore < sentimentNegativeThreshold {
						sentiment = "NEGATIVE"
					}
					newsItems = append(newsItems, prompts.ORBNewsItem{
						Headline:    n.Headline,
						Sentiment:   sentiment,
						PublishedAt: n.PublishedAt,
					})
				}
			}
		}

		sysPrompt, userMsg := prompts.BuildORBPrompt(
			prompts.ORBCandidate{
				Ticker:        gap.Ticker,
				GapPct:        gap.GapPct,
				PreMarketVol:  gap.PreMarketVol,
				PreMarketLast: gap.PreMarketLast,
				PriorClose:    gap.PriorClose,
			},
			newsItems,
		)

		resp, meta, err := s.analyzer.Analyze(ctx, analysis.AnalysisRequest{
			Ticker:             gap.Ticker,
			CustomSystemPrompt: sysPrompt,
			CustomUserMessage:  userMsg,
		}, depth.ImpactScore{Score: 1.0, Depth: 3})
		if err != nil {
			fmt.Printf("orb: LLM scoring %s failed: %v\n", gap.Ticker, err)
			continue
		}

		// Parse ORB-specific fields (catalyst_type, catalyst_quality, risks) from
		// the raw LLM response. The standard AnalysisResponse doesn't carry these
		// fields — they exist only in the ORB custom prompt schema.
		result := parseORBResult(meta.RawResponse, resp)
		scored = append(scored, ScoredCandidate{Gap: gap, Score: result})
	}

	// Filter to "trade" or "BUY" actions, sort by conviction, take top N
	var selected []ScoredCandidate
	for _, sc := range scored {
		if strings.EqualFold(sc.Score.Action, "trade") || strings.EqualFold(sc.Score.Action, "BUY") {
			selected = append(selected, sc)
		}
	}

	// Sort by conviction descending
	for i := 0; i < len(selected); i++ {
		for j := i + 1; j < len(selected); j++ {
			if selected[j].Score.Conviction > selected[i].Score.Conviction {
				selected[i], selected[j] = selected[j], selected[i]
			}
		}
	}

	if len(selected) > s.cfg.MaxCandidates {
		selected = selected[:s.cfg.MaxCandidates]
	}

	s.mu.Lock()
	sessionID := s.session.ID
	s.mu.Unlock()

	candidatesJSON, _ := json.Marshal(scored)

	if len(selected) == 0 {
		return nil
	}

	s.mu.Lock()
	s.candidates = selected
	s.mu.Unlock()

	var selectedTickers []string
	for _, sc := range selected {
		selectedTickers = append(selectedTickers, sc.Gap.Ticker)
	}
	if err := store.UpdateORBSession(ctx, s.db, sessionID, "candidate_ready", candidatesJSON, selectedTickers); err != nil {
		return fmt.Errorf("update session to candidate_ready: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("ORB Scan Complete:\n")
	for _, sc := range selected {
		sb.WriteString(fmt.Sprintf("  %s: gap %+.1f%%, conviction %.0f%%, %s\n",
			sc.Gap.Ticker, sc.Gap.GapPct, sc.Score.Conviction*100, sc.Score.Reasoning))
	}
	s.notify(ctx, sb.String())

	s.setState(StateCandidateReady)
	for _, sc := range selected {
		s.publish(events.AnalysisCompleted, events.AnalysisCompletedPayload{
			Ticker:     sc.Gap.Ticker,
			Action:     sc.Score.Action,
			Confidence: sc.Score.Conviction,
			Reasoning:  sc.Score.Reasoning,
		})
	}
	return nil
}

func (s *ORBScheduler) tickCandidateReady(ctx context.Context, et time.Time) error {
	h, m, _ := et.Clock()
	etMinutes := h*60 + m
	if etMinutes < 9*60+30 {
		return nil
	}

	s.mu.Lock()
	s.rangeBuffers = make(map[string]*RangeBuffer)
	for _, sc := range s.candidates {
		s.rangeBuffers[sc.Gap.Ticker] = NewRangeBuffer(sc.Gap.Ticker, s.cfg.RangeMinutes)
	}
	sessionID := s.session.ID
	sessionCandidates := s.session.Candidates
	sessionSelected := s.session.Selected
	s.mu.Unlock()

	// Only update status — preserve candidates and selected from previous state.
	if err := store.UpdateORBSession(ctx, s.db, sessionID, "range_watch", sessionCandidates, sessionSelected); err != nil {
		return fmt.Errorf("update session to range_watch: %w", err)
	}
	s.setState(StateRangeWatch)
	return nil
}

func (s *ORBScheduler) tickRangeWatch(ctx context.Context, et time.Time) error {
	marketOpen := time.Date(et.Year(), et.Month(), et.Day(), 9, 30, 0, 0, nyLoc)
	now := s.now()

	allLocked := true
	for ticker, buf := range s.rangeBuffers {
		if buf.Range() != nil {
			continue // already locked
		}
		candles, err := s.marketData.FetchCandles(ctx, ticker, data.Timeframe1Min, marketOpen, now)
		if err != nil {
			fmt.Printf("orb: fetch candles %s: %v\n", ticker, err)
			allLocked = false
			continue
		}
		for _, c := range candles {
			locked, _ := buf.Add(c)
			if locked {
				break
			}
		}
		if buf.Range() == nil {
			allLocked = false
		}
	}

	if allLocked && len(s.rangeBuffers) > 0 {
		s.mu.Lock()
		sessionID := s.session.ID
		sessionCandidates := s.session.Candidates
		sessionSelected := s.session.Selected
		s.mu.Unlock()
		if err := store.UpdateORBSession(ctx, s.db, sessionID, "executing", sessionCandidates, sessionSelected); err != nil {
			return fmt.Errorf("update session to executing: %w", err)
		}
		s.setState(StateExecuting)
	}
	return nil
}

func (s *ORBScheduler) tickExecuting(ctx context.Context) error {
	if s.executor == nil {
		s.notify(ctx, "ORB: No broker configured — cannot place bracket orders.")
		s.setState(StateDone)
		return nil
	}

	portfolioVal := 100000.0 // default fallback
	if s.portfolio != nil {
		portfolioVal = s.portfolio.PortfolioValue()
	}

	s.mu.Lock()
	sessionID := s.session.ID
	candidates := s.candidates
	s.mu.Unlock()

	var msgs []string
	for ticker, buf := range s.rangeBuffers {
		r := buf.Range()
		if r == nil {
			continue
		}

		// Reject flat/near-flat ranges to avoid unbounded order quantity
		if r.High-r.Low < 0.01 {
			fmt.Printf("orb: skipping %s — opening range too narrow (high=%.4f, low=%.4f)\n", ticker, r.High, r.Low)
			continue
		}

		side := "buy"
		for _, sc := range candidates {
			if sc.Gap.Ticker == ticker && sc.Gap.GapPct < 0 {
				side = "sell"
			}
		}

		params := ComputeBracket(r, side, portfolioVal, s.cfg)

		orderID, err := PlaceBracket(ctx, s.executor, params)
		if err != nil {
			fmt.Printf("orb: place bracket %s: %v\n", ticker, err)
			continue
		}
		s.orderIDs[ticker] = orderID
		s.publish(events.TradeSimulated, events.TradeExecutedPayload{
			Ticker:    ticker,
			Direction: side,
			Strategy:  "orb",
		})

		if err := store.CreateORBTrade(ctx, s.db, store.ORBTrade{
			SessionID:  sessionID,
			Ticker:     ticker,
			Side:       side,
			EntryStop:  params.EntryStop,
			StopLoss:   params.StopLoss,
			TakeProfit: params.TakeProfit,
			Qty:        params.Qty,
			OrderID:    orderID,
			Status:     "pending",
		}); err != nil {
			fmt.Printf("orb: store trade %s: %v\n", ticker, err)
		}

		msgs = append(msgs, fmt.Sprintf("  %s %s: entry=%.2f stop=%.2f target=%.2f qty=%d",
			strings.ToUpper(side), ticker, params.EntryStop, params.StopLoss, params.TakeProfit, params.Qty))
	}

	if len(msgs) > 0 {
		s.notify(ctx, "ORB Brackets Placed:\n"+strings.Join(msgs, "\n"))
	}

	s.setState(StateDone)
	return nil
}

func (s *ORBScheduler) tickDone(ctx context.Context, et time.Time) error {
	h, m, _ := et.Clock()
	etMinutes := h*60 + m

	// After 10:30 ET, cancel unfilled orders and wrap up
	if etMinutes >= 10*60+30 {
		if s.executor != nil {
			for ticker, orderID := range s.orderIDs {
				// Best-effort cancel; already-filled orders return an error which is fine.
				cancelErr := CancelOrder(ctx, s.executor, orderID)
				if cancelErr != nil {
					fmt.Printf("orb: cancel %s order %s: %v\n", ticker, orderID, cancelErr)
				}
				// Update trade status to "cancelled" regardless of broker response.
				// If the order was already filled, the status will only update rows
				// still in "pending" state (WHERE status='pending' guard in SQL).
				if dbErr := store.CancelORBTradeByOrderID(ctx, s.db, orderID); dbErr != nil {
					fmt.Printf("orb: update cancelled trade %s: %v\n", ticker, dbErr)
				}
			}
		}

		s.mu.Lock()
		sessionID := s.session.ID
		s.mu.Unlock()

		if err := store.UpdateORBSession(ctx, s.db, sessionID, "done", nil, nil); err != nil {
			fmt.Printf("orb: update session to done: %v\n", err)
		}
		s.publish(events.PortfolioSnapshotTaken, events.PortfolioSnapshotPayload{})
		s.notify(ctx, "ORB: Session complete for today.")

		// Reset for next day
		s.mu.Lock()
		s.session = nil
		s.candidates = nil
		s.rangeBuffers = make(map[string]*RangeBuffer)
		s.orderIDs = make(map[string]string)
		s.state = StateIdle
		s.mu.Unlock()
	}
	return nil
}

func (s *ORBScheduler) notify(ctx context.Context, msg string) {
	if s.notifier == nil {
		return
	}
	_ = s.notifier.Notify(ctx, msg)
}

// orbRawResponse is the full JSON schema returned by the ORB custom system prompt.
type orbRawResponse struct {
	Action          string   `json:"action"`
	Conviction      float64  `json:"conviction"`
	CatalystType    string   `json:"catalyst_type"`
	CatalystQuality string   `json:"catalyst_quality"`
	Reasoning       string   `json:"reasoning"`
	Risks           []string `json:"risks"`
}

// parseORBResult extracts the full ORB analysis result from the raw LLM response.
// Falls back to the standard AnalysisResponse fields when the raw JSON cannot be
// parsed (e.g. the arbiter returned a transformed response).
func parseORBResult(rawResponse string, fallback analysis.AnalysisResponse) ORBAnalysisResult {
	// Strip markdown fences if present
	raw := strings.TrimSpace(rawResponse)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var parsed orbRawResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil &&
		(parsed.Action == "trade" || parsed.Action == "skip") {
		return ORBAnalysisResult{
			Action:          parsed.Action,
			Conviction:      parsed.Conviction,
			CatalystType:    parsed.CatalystType,
			CatalystQuality: parsed.CatalystQuality,
			Reasoning:       parsed.Reasoning,
			Risks:           parsed.Risks,
		}
	}

	// Fallback: map standard AnalysisResponse fields
	return ORBAnalysisResult{
		Action:     string(fallback.Action),
		Conviction: fallback.Confidence,
		Reasoning:  fallback.Reasoning,
	}
}

// IsTradingDay checks if the given time falls on a trading day.
func IsTradingDay(t time.Time) bool {
	et := t.In(nyLoc)
	noon := time.Date(et.Year(), et.Month(), et.Day(), 12, 0, 0, 0, nyLoc)
	return engine.IsMarketOpen(noon)
}
