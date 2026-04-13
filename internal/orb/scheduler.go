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
	"github.com/romeo-folie/trade-engine/internal/feedback"
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

// marketOpenMinutes is the Eastern Time minute-of-day when the US equity market opens (9:30 AM ET).
const marketOpenMinutes = 9*60 + 30

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

// CandidateScanner finds tradeable candidates for the ORB strategy.
type CandidateScanner interface {
	ScanCandidates(ctx context.Context) ([]ScoredCandidate, error)
}

// ORBScheduler implements the time-gated ORB state machine.
type ORBScheduler struct {
	cfg        config.ORBConfig
	db         *pgxpool.Pool
	marketData data.MarketDataProvider
	analyzer   *analysis.Analyzer
	executor   execution.Executor
	portfolio  PortfolioValuer
	notifier   Notifier
	news        data.NewsProvider      // optional; nil disables news enrichment
	bus         *events.Bus            // optional; nil means no event publishing
	now         func() time.Time       // injectable for tests
	scanner     CandidateScanner       // optional; nil uses inline equities scanning
	forexCfg    *config.ForexORBConfig   // optional; nil means equities mode
	futuresCfg  *config.FuturesORBConfig // optional; nil means not futures mode
	feedbackCfg *config.FeedbackConfig   // optional; nil disables feedback enrichment

	mu             sync.Mutex // protects state, session, candidates for TUI reads
	state          State
	session        *store.ORBSession
	candidates     []ScoredCandidate
	rangeBuffers   map[string]*RangeBuffer
	orderIDs       map[string][2]string // ticker → [buyOrderID, sellOrderID]
	lastScan       time.Time         // Eastern Time of the last successful scan
	fetchFailures  map[string]int    // ticker → consecutive candle-fetch failures
}

// NewORBScheduler creates a new ORB scheduler.
func NewORBScheduler(
	cfg config.ORBConfig,
	db *pgxpool.Pool,
	marketData data.MarketDataProvider,
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
		now:           time.Now,
		state:         StateIdle,
		rangeBuffers:  make(map[string]*RangeBuffer),
		orderIDs:      make(map[string][2]string),
		fetchFailures: make(map[string]int),
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

// SetScanner wires a custom candidate scanner. If not called, the scheduler
// uses its inline equities gap-mover + LLM scoring logic.
func (s *ORBScheduler) SetScanner(scanner CandidateScanner) {
	s.scanner = scanner
}

// SetForexConfig enables forex mode with the given configuration.
// Call before Run to switch the scheduler from equities to forex session timing.
func (s *ORBScheduler) SetForexConfig(cfg *config.ForexORBConfig) {
	s.forexCfg = cfg
}

// SetFuturesConfig enables futures mode with the given configuration.
// Call before Run to switch the scheduler from equities to futures session timing.
func (s *ORBScheduler) SetFuturesConfig(cfg *config.FuturesORBConfig) {
	s.futuresCfg = cfg
}

// isFuturesMode reports whether the scheduler is running in futures mode.
func (s *ORBScheduler) isFuturesMode() bool {
	return s.futuresCfg != nil && s.futuresCfg.Enabled
}

// futuresInstrumentForTicker returns the FuturesInstrument config for the given symbol,
// or nil if not found or futures mode is not configured.
func (s *ORBScheduler) futuresInstrumentForTicker(ticker string) *config.FuturesInstrument {
	if s.futuresCfg == nil {
		return nil
	}
	for i := range s.futuresCfg.Instruments {
		if s.futuresCfg.Instruments[i].Symbol == ticker {
			return &s.futuresCfg.Instruments[i]
		}
	}
	return nil
}

// SetFeedbackConfig wires feedback configuration for ORB prompt enrichment.
// Call before Run. If not called, prompt enrichment is disabled.
func (s *ORBScheduler) SetFeedbackConfig(cfg *config.FeedbackConfig) {
	s.feedbackCfg = cfg
}

// isForexMode reports whether the scheduler is running in forex mode.
func (s *ORBScheduler) isForexMode() bool {
	return s.forexCfg != nil && s.forexCfg.Enabled
}

// sessionName returns the session name used as the uniqueness key for orb_sessions.
// Equities ORB uses "" (the default). Forex ORB uses the configured session name.
// Futures ORB uses "futures" — distinct from all forex session names ("london", "newyork", "both", "").
func (s *ORBScheduler) sessionName() string {
	if s.isFuturesMode() {
		return "futures"
	}
	if s.forexCfg != nil && s.forexCfg.Enabled {
		return s.forexCfg.Session
	}
	return ""
}

// instrumentForTicker returns the Instrument config for the given ticker symbol,
// or nil if not found (e.g. in equities mode or ticker not configured).
func (s *ORBScheduler) instrumentForTicker(ticker string) *config.Instrument {
	if s.forexCfg == nil {
		return nil
	}
	for i := range s.forexCfg.Instruments {
		if s.forexCfg.Instruments[i].Symbol == ticker {
			return &s.forexCfg.Instruments[i]
		}
	}
	return nil
}

// sessionOpenMinutes returns the minute-of-day for market/session open
// in the scheduler's timezone (ET for equities/futures, configured TZ for forex).
func (s *ORBScheduler) sessionOpenMinutes() int {
	if s.isFuturesMode() && s.futuresCfg.SessionOpenET != "" {
		var h, m int
		if _, err := fmt.Sscanf(s.futuresCfg.SessionOpenET, "%d:%d", &h, &m); err == nil {
			return h*60 + m
		}
	}
	if s.forexCfg != nil && s.forexCfg.Enabled {
		switch s.forexCfg.Session {
		case "london":
			return 8 * 60 // 08:00 UTC
		case "newyork":
			return 13 * 60 // 13:00 UTC
		case "both":
			return 8 * 60 // start with London
		}
	}
	return marketOpenMinutes // 9:30 AM ET for equities
}

// sessionTimezone returns the timezone location for the scheduler.
// Forex uses the configured timezone (default UTC); equities uses US/Eastern.
func (s *ORBScheduler) sessionTimezone() *time.Location {
	if s.forexCfg != nil && s.forexCfg.Enabled && s.forexCfg.Timezone != "" {
		loc, err := time.LoadLocation(s.forexCfg.Timezone)
		if err == nil {
			return loc
		}
	}
	return nyLoc
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
	sessionTZ := s.sessionTimezone()
	sessionNow := s.now().In(sessionTZ)

	if s.isForexMode() {
		// Forex trades 24/5 — only skip weekends
		weekday := sessionNow.Weekday()
		if weekday == time.Saturday || weekday == time.Sunday {
			return nil
		}
	} else {
		// Skip weekends and market holidays (IsTradingDay uses the engine's calendar).
		if !IsTradingDay(s.now()) {
			return nil
		}
	}
	h, m, _ := sessionNow.Clock()
	sessionMinutes := h*60 + m
	if sessionMinutes < 6*60 {
		return nil
	}

	today := time.Date(sessionNow.Year(), sessionNow.Month(), sessionNow.Day(), 0, 0, 0, 0, sessionTZ)
	existing, err := store.GetORBSessionByDate(ctx, s.db, today, s.sessionName())
	if err == nil && existing != nil {
		if existing.Status == "done" || existing.Status == "skipped" {
			return nil
		}

		// Resume an in-progress session after a restart instead of
		// unconditionally restarting from StateScanning (which would
		// hit the "pre-market window closed" cutoff if past session open).
		if existing.Status == "candidate_ready" || existing.Status == "range_watch" || existing.Status == "executing" {
			if err := s.resumeSession(ctx, existing); err != nil {
				return fmt.Errorf("resume session %s from %s: %w", existing.ID, existing.Status, err)
			}
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
		session, err := store.CreateORBSession(ctx, s.db, today, s.sessionName())
		if err != nil {
			return fmt.Errorf("create orb session: %w", err)
		}
		s.mu.Lock()
		s.session = session
		s.mu.Unlock()
	}

	s.setState(StateScanning)
	s.mu.Lock()
	session := s.session
	s.mu.Unlock()
	if session != nil {
		s.publish(events.ORBStateChanged, events.ORBStateChangedPayload{
			State:     string(StateScanning),
			SessionID: session.ID,
			Date:      session.Date.Format("2006-01-02"),
		})
	}
	return nil
}

// resumeSession restores in-memory state from a persisted in-progress session
// after a process restart, then transitions to the appropriate state so the
// scheduler continues from where it left off instead of re-entering StateScanning
// (which would hit the "pre-market window closed" cutoff).
func (s *ORBScheduler) resumeSession(ctx context.Context, existing *store.ORBSession) error {
	// Restore candidates from the persisted JSON.
	var candidates []ScoredCandidate
	if len(existing.Candidates) > 0 {
		if err := json.Unmarshal(existing.Candidates, &candidates); err != nil {
			return fmt.Errorf("unmarshal candidates: %w", err)
		}
	}

	s.mu.Lock()
	s.session = existing
	s.candidates = candidates
	s.mu.Unlock()

	targetState := State(existing.Status)

	// For range_watch or executing, rebuild range buffers so tickRangeWatch
	// can resume fetching candles. Fresh buffers are fine — they will
	// re-accumulate from session open on the next tick.
	if targetState == StateRangeWatch || targetState == StateExecuting {
		rangeMinutes := s.cfg.RangeMinutes
		if s.isForexMode() && s.forexCfg != nil {
			rangeMinutes = s.forexCfg.RangeMinutes
		} else if s.isFuturesMode() && s.futuresCfg != nil {
			rangeMinutes = s.futuresCfg.RangeMinutes
		}
		s.mu.Lock()
		s.rangeBuffers = make(map[string]*RangeBuffer)
		for _, sc := range s.candidates {
			s.rangeBuffers[sc.Gap.Ticker] = NewRangeBuffer(sc.Gap.Ticker, rangeMinutes)
		}
		s.mu.Unlock()
	}

	s.setState(targetState)
	fmt.Printf("orb: resumed session %s in state %s with %d candidates\n", existing.ID, existing.Status, len(candidates))
	s.publish(events.ORBStateChanged, events.ORBStateChangedPayload{
		State:     existing.Status,
		SessionID: existing.ID,
		Date:      existing.Date.Format("2006-01-02"),
	})
	return nil
}

func (s *ORBScheduler) tickScanning(ctx context.Context, et time.Time) error {
	sessionTZ := s.sessionTimezone()
	now := s.now()
	sessionNow := now.In(sessionTZ)
	h, m, _ := sessionNow.Clock()
	sessionMinutes := h*60 + m
	openMinutes := s.sessionOpenMinutes()

	// 1. Strict Cutoff: stop scanning once the session opens.
	// Once the session open time is reached, we can no longer add new candidates
	// as the Opening Range window has already begun.
	if sessionMinutes >= openMinutes {
		s.notify(ctx, "ORB: Pre-market scan window closed. No candidates found today.")
		s.setState(StateDone)
		s.mu.Lock()
		session := s.session
		s.mu.Unlock()
		if session != nil {
			if err := store.UpdateORBSession(ctx, s.db, session.ID, "skipped", nil, nil); err != nil {
				return fmt.Errorf("update session to skipped: %w", err)
			}
			s.publish(events.ORBStateChanged, events.ORBStateChangedPayload{
				State:     string(StateDone),
				SessionID: session.ID,
				Date:      session.Date.Format("2006-01-02"),
			})
		}
		return nil
	}

	// 2. Scan Cadence: minimize LLM costs by only scanning every N minutes.
	// The scheduler ticks every 30s, but gap movers and news don't shift that fast.
	cadence := time.Duration(s.cfg.ScanCadenceMinutes) * time.Minute
	if !s.lastScan.IsZero() && sessionNow.Sub(s.lastScan) < cadence {
		return nil
	}

	var scored []ScoredCandidate
	var err error
	if s.scanner != nil {
		scored, err = s.scanner.ScanCandidates(ctx)
		if err != nil {
			return fmt.Errorf("scan candidates: %w", err)
		}
	} else {
		// Inline equities scanning: gap movers + LLM scoring
		alpacaClient, ok := s.marketData.(*data.AlpacaClient)
		if !ok {
			return fmt.Errorf("equities scanning requires AlpacaClient as MarketDataProvider; set a CandidateScanner for other providers")
		}
		gaps, gapErr := data.FetchGapMovers(ctx, alpacaClient, s.cfg.GapThresholdPct)
		if gapErr != nil {
			return fmt.Errorf("scan gap movers: %w", gapErr)
		}
		s.lastScan = sessionNow

		if len(gaps) == 0 {
			return nil
		}

		// LLM-score each candidate
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

			var orbPerfCtx string
			if s.feedbackCfg != nil && s.feedbackCfg.Enabled && s.feedbackCfg.ORBPromptEnrichment {
				if pc, err := feedback.GetORBPerformanceContext(ctx, gap.Ticker, ""); err == nil {
					orbPerfCtx = pc
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
				orbPerfCtx,
			)

			resp, meta, analyzeErr := s.analyzer.Analyze(ctx, analysis.AnalysisRequest{
				Ticker:             gap.Ticker,
				CustomSystemPrompt: sysPrompt,
				CustomUserMessage:  userMsg,
			}, depth.ImpactScore{Score: 1.0, Depth: 3})
			if analyzeErr != nil {
				fmt.Printf("orb: LLM scoring %s failed: %v\n", gap.Ticker, analyzeErr)
				continue
			}

			// Parse ORB-specific fields (catalyst_type, catalyst_quality, risks) from
			// the raw LLM response. The standard AnalysisResponse doesn't carry these
			// fields — they exist only in the ORB custom prompt schema.
			result := parseORBResult(meta.RawResponse, resp)
			scored = append(scored, ScoredCandidate{Gap: gap, Score: result})
		}
	}
	s.lastScan = sessionNow

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

	// Guard: if the session has already opened by the time scoring completes,
	// discard the candidates. The opening-range window has already started and
	// we would miss part of it, skewing the range calculation.
	// Re-read the clock here because LLM scoring is synchronous and can take
	// several seconds per ticker, potentially crossing the session-open boundary.
	postScanNow := s.now().In(sessionTZ)
	postScanHour, postScanMinute, _ := postScanNow.Clock()
	if postScanHour*60+postScanMinute >= openMinutes {
		s.notify(ctx, "ORB: Scan completed after market open — discarding candidates to avoid a skewed opening range.")
		s.setState(StateDone)
		s.mu.Lock()
		session := s.session
		s.mu.Unlock()
		if session != nil {
			if err := store.UpdateORBSession(ctx, s.db, session.ID, "skipped", nil, nil); err != nil {
				return fmt.Errorf("update session to skipped: %w", err)
			}
			s.publish(events.ORBStateChanged, events.ORBStateChangedPayload{
				State:     string(StateDone),
				SessionID: session.ID,
				Date:      session.Date.Format("2006-01-02"),
			})
		}
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
	s.mu.Lock()
	pubSession := s.session
	pubCandidates := s.candidates
	s.mu.Unlock()
	if pubSession != nil {
		s.publish(events.ORBStateChanged, events.ORBStateChangedPayload{
			State:     string(StateCandidateReady),
			SessionID: pubSession.ID,
			Date:      pubSession.Date.Format("2006-01-02"),
		})
		s.publish(events.ORBCandidatesReady, events.ORBCandidatesReadyPayload{
			SessionID:  pubSession.ID,
			Candidates: pubCandidates,
			Selected:   selectedTickers,
		})
	}
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
	sessionNow := s.now().In(s.sessionTimezone())
	h, m, _ := sessionNow.Clock()
	sessionMinutes := h*60 + m
	if sessionMinutes < s.sessionOpenMinutes() {
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
	s.mu.Lock()
	rwSession := s.session
	s.mu.Unlock()
	if rwSession != nil {
		s.publish(events.ORBStateChanged, events.ORBStateChangedPayload{
			State:     string(StateRangeWatch),
			SessionID: rwSession.ID,
			Date:      rwSession.Date.Format("2006-01-02"),
		})
	}
	return nil
}

func (s *ORBScheduler) tickRangeWatch(ctx context.Context, et time.Time) error {
	sessionTZ := s.sessionTimezone()
	openMinutes := s.sessionOpenMinutes()
	openHour := openMinutes / 60
	openMin := openMinutes % 60
	sessionDay := s.now().In(sessionTZ)
	marketOpen := time.Date(sessionDay.Year(), sessionDay.Month(), sessionDay.Day(), openHour, openMin, 0, 0, sessionTZ)
	now := s.now()

	const maxFetchFailures = 3 // drop instrument after this many consecutive failures

	allLocked := true
	for ticker, buf := range s.rangeBuffers {
		if buf.Range() != nil {
			continue // already locked
		}
		candles, err := s.marketData.FetchCandles(ctx, ticker, data.Timeframe1Min, marketOpen, now)
		if err != nil {
			s.fetchFailures[ticker]++
			if s.fetchFailures[ticker] >= maxFetchFailures {
				fmt.Printf("orb: dropping %s after %d consecutive candle-fetch failures: %v\n", ticker, s.fetchFailures[ticker], err)
				delete(s.rangeBuffers, ticker)
			} else {
				fmt.Printf("orb: fetch candles %s (attempt %d/%d): %v\n", ticker, s.fetchFailures[ticker], maxFetchFailures, err)
				allLocked = false
			}
			continue
		}
		s.fetchFailures[ticker] = 0 // reset on success
		for _, c := range candles {
			locked, _ := buf.Add(c)
			if locked {
				r := buf.Range()
				if r != nil {
					s.publish(events.ORBRangeLocked, events.ORBRangeLockedPayload{
						Ticker:       ticker,
						RangeHigh:    r.High,
						RangeLow:     r.Low,
						RangeMinutes: s.cfg.RangeMinutes,
					})
				}
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
		exSession := s.session
		s.mu.Unlock()
		if err := store.UpdateORBSession(ctx, s.db, sessionID, "executing", sessionCandidates, sessionSelected); err != nil {
			return fmt.Errorf("update session to executing: %w", err)
		}
		s.setState(StateExecuting)
		s.publish(events.ORBStateChanged, events.ORBStateChangedPayload{
			State:     string(StateExecuting),
			SessionID: exSession.ID,
			Date:      exSession.Date.Format("2006-01-02"),
		})
	}
	return nil
}

func (s *ORBScheduler) tickExecuting(ctx context.Context) error {
	if s.executor == nil {
		s.notify(ctx, "ORB: No broker configured — cannot place bracket orders.")
		s.setState(StateDone)
		s.mu.Lock()
		noExecSession := s.session
		s.mu.Unlock()
		if noExecSession != nil {
			s.publish(events.ORBStateChanged, events.ORBStateChangedPayload{
				State:     string(StateDone),
				SessionID: noExecSession.ID,
				Date:      noExecSession.Date.Format("2006-01-02"),
			})
		}
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

		// Reject flat/near-flat ranges to avoid unbounded order quantity.
		// Forex/CFD instruments trade at very different price scales than equities,
		// so use 2× the pip buffer as the minimum range width when available.
		minRange := 0.01
		if s.isForexMode() {
			if inst := s.instrumentForTicker(ticker); inst != nil && inst.PipBuffer > 0 {
				minRange = inst.PipBuffer * 2
			}
		}
		if r.High-r.Low < minRange {
			fmt.Printf("orb: skipping %s — opening range too narrow (high=%.4f, low=%.4f, min=%.4f)\n", ticker, r.High, r.Low, minRange)
			continue
		}

		// Forex mode: place dual-sided brackets (buy + sell) so the market
		// decides direction. Equities/futures: single side from gap direction.
		if s.isForexMode() {
			inst := s.instrumentForTicker(ticker)
			if inst == nil {
				fmt.Printf("orb: skipping %s — no instrument config found\n", ticker)
				continue
			}

			buyParams := ComputeForexBracket(r, "buy", portfolioVal, *s.forexCfg, *inst)
			sellParams := ComputeForexBracket(r, "sell", portfolioVal, *s.forexCfg, *inst)

			if buyParams.Qty == 0 && sellParams.Qty == 0 {
				fmt.Printf("orb: skipping %s — min lot size exceeds position cap\n", ticker)
				continue
			}

			// NOTE: In paper mode, PaperExecutor fills stop orders immediately,
			// so the OCO poll will deterministically cancel the sell side.
			// A proper fix requires PaperExecutor to keep stops pending until
			// a simulated price cross — tracked separately from this PR.
			var ids [2]string
			for i, p := range []struct {
				side   string
				params BracketParams
			}{{"buy", buyParams}, {"sell", sellParams}} {
				if p.params.Qty == 0 {
					continue
				}
				orderID, err := PlaceBracket(ctx, s.executor, p.params)
				if err != nil {
					fmt.Printf("orb: place bracket %s %s: %v\n", p.side, ticker, err)
					continue
				}
				ids[i] = orderID
				s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
					OrderID: orderID,
					Ticker:  ticker,
					Side:    p.side,
					Status:  "pending",
				})
				if err := store.CreateORBTrade(ctx, s.db, store.ORBTrade{
					SessionID:  sessionID,
					Ticker:     ticker,
					Side:       p.side,
					EntryStop:  p.params.EntryStop,
					StopLoss:   p.params.StopLoss,
					TakeProfit: p.params.TakeProfit,
					Qty:        p.params.Qty,
					OrderID:    orderID,
					Status:     "pending",
				}); err != nil {
					fmt.Printf("orb: store trade %s %s: %v\n", p.side, ticker, err)
				}
				msgs = append(msgs, fmt.Sprintf("  %s %s: entry=%.4f stop=%.4f target=%.4f qty=%d",
					strings.ToUpper(p.side), ticker, p.params.EntryStop, p.params.StopLoss, p.params.TakeProfit, p.params.Qty))
			}
			s.orderIDs[ticker] = ids
			// Emit per-side events only for brackets that were actually placed.
			for i, side := range []string{"buy", "sell"} {
				if ids[i] != "" {
					s.publish(events.TradeSimulated, events.TradeExecutedPayload{
						Ticker:    ticker,
						Direction: side,
						Strategy:  "orb",
					})
				}
			}
		} else {
			side := "buy"
			for _, sc := range candidates {
				if sc.Gap.Ticker == ticker && sc.Gap.GapPct < 0 {
					side = "sell"
				}
			}

			var params BracketParams
			if s.isFuturesMode() {
				fi := s.futuresInstrumentForTicker(ticker)
				if fi == nil {
					fmt.Printf("orb: skipping %s — no futures instrument config found\n", ticker)
					continue
				}
				params = ComputeFuturesBracket(r, side, portfolioVal, *s.futuresCfg, *fi)
			} else {
				params = ComputeBracket(r, side, portfolioVal, s.cfg)
			}

			if params.Qty == 0 {
				fmt.Printf("orb: skipping %s — position size too small\n", ticker)
				continue
			}

			orderID, err := PlaceBracket(ctx, s.executor, params)
			if err != nil {
				fmt.Printf("orb: place bracket %s: %v\n", ticker, err)
				continue
			}
			if side == "sell" {
				s.orderIDs[ticker] = [2]string{"", orderID}
			} else {
				s.orderIDs[ticker] = [2]string{orderID, ""}
			}
			s.publish(events.TradeSimulated, events.TradeExecutedPayload{
				Ticker:    ticker,
				Direction: side,
				Strategy:  "orb",
			})
			s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
				OrderID: orderID,
				Ticker:  ticker,
				Side:    side,
				Status:  "pending",
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
			msgs = append(msgs, fmt.Sprintf("  %s %s: entry=%.4f stop=%.4f target=%.4f qty=%d",
				strings.ToUpper(side), ticker, params.EntryStop, params.StopLoss, params.TakeProfit, params.Qty))
		}
	}

	if len(msgs) > 0 {
		s.notify(ctx, "ORB Brackets Placed:\n"+strings.Join(msgs, "\n"))
	}

	s.setState(StateDone)
	s.mu.Lock()
	doneSession := s.session
	s.mu.Unlock()
	if doneSession != nil {
		s.publish(events.ORBStateChanged, events.ORBStateChangedPayload{
			State:     string(StateDone),
			SessionID: doneSession.ID,
			Date:      doneSession.Date.Format("2006-01-02"),
		})
	}
	return nil
}

func (s *ORBScheduler) tickDone(ctx context.Context, et time.Time) error {
	h, m, _ := et.Clock()
	etMinutes := h*60 + m

	// Poll for fills and cancel the opposing side (dual-sided forex OCO).
	if s.executor != nil {
		for ticker, ids := range s.orderIDs {
			buyID, sellID := ids[0], ids[1]
			if buyID == "" || sellID == "" {
				continue // single-sided — no opposing cancel needed
			}

			// Fetch both statuses upfront to detect double fills.
			type sideStatus struct {
				id     string
				side   string
				res    execution.OrderResult
				ok     bool
				filled bool
			}
			ss := [2]*sideStatus{
				{id: buyID, side: "buy"},
				{id: sellID, side: "sell"},
			}
			for _, s2 := range ss {
				res, err := s.executor.GetOrderStatus(ctx, s2.id)
				if err != nil {
					fmt.Printf("orb: GetOrderStatus %s %s (%s): %v\n", s2.side, ticker, s2.id, err)
				} else {
					s2.res = res
					s2.ok = true
					s2.filled = res.Status == "filled"
				}
			}

			// persistFill is a helper to mark a side as filled in DB and emit event.
			persistFill := func(s2 *sideStatus) {
				if dbErr := store.FillORBTradeByOrderID(ctx, s.db, s2.id, s2.res.FilledPrice); dbErr != nil {
					fmt.Printf("orb: update filled %s trade %s: %v\n", s2.side, ticker, dbErr)
				}
				s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
					OrderID: s2.id, Ticker: ticker, Side: s2.side, Status: "filled", FillPrice: s2.res.FilledPrice,
				})
			}

			// Both sides filled (rare — volatile market filled both stops before poll).
			if ss[0].filled && ss[1].filled {
				fmt.Printf("orb: %s both sides filled — persisting double fill\n", ticker)
				persistFill(ss[0])
				persistFill(ss[1])
				delete(s.orderIDs, ticker)
				continue
			}

			// One side filled → cancel the other.
			for i, filled := range ss {
				if !filled.filled {
					continue
				}
				other := ss[1-i]
				fmt.Printf("orb: %s %s filled (%.4f) — cancelling %s side\n", ticker, filled.side, filled.res.FilledPrice, other.side)
				persistFill(filled)
				if cancelErr := CancelOrder(ctx, s.executor, other.id); cancelErr != nil {
					// Cancel failed — check if other side also filled before retrying.
					if otherRes, err := s.executor.GetOrderStatus(ctx, other.id); err == nil && otherRes.Status == "filled" {
						fmt.Printf("orb: %s %s also filled (%.4f) — double fill\n", ticker, other.side, otherRes.FilledPrice)
						other.res = otherRes
						persistFill(other)
						delete(s.orderIDs, ticker)
					} else {
						fmt.Printf("orb: cancel %s %s: %v (will retry next tick)\n", other.side, ticker, cancelErr)
					}
				} else {
					if dbErr := store.CancelORBTradeByOrderID(ctx, s.db, other.id); dbErr != nil {
						fmt.Printf("orb: update cancelled %s trade %s: %v\n", other.side, ticker, dbErr)
					}
					s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
						OrderID: other.id, Ticker: ticker, Side: other.side, Status: "cancelled",
					})
					delete(s.orderIDs, ticker)
				}
				break // only one side can be the trigger
			}
		}
	}

	// After 10:30 ET, cancel unfilled orders and wrap up
	if etMinutes >= 10*60+30 {
		if s.executor != nil {
			sides := [2]string{"buy", "sell"}
			for ticker, ids := range s.orderIDs {
				for i, orderID := range ids {
					if orderID == "" {
						continue
					}
					// Reconcile already-filled orders instead of cancelling them.
					if res, err := s.executor.GetOrderStatus(ctx, orderID); err == nil && res.Status == "filled" {
						fmt.Printf("orb: 10:30 cleanup: %s %s already filled at %.4f — persisting\n", sides[i], ticker, res.FilledPrice)
						if dbErr := store.FillORBTradeByOrderID(ctx, s.db, orderID, res.FilledPrice); dbErr != nil {
							fmt.Printf("orb: update filled trade %s: %v\n", ticker, dbErr)
						}
						s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
							OrderID: orderID, Ticker: ticker, Side: sides[i], Status: "filled", FillPrice: res.FilledPrice,
						})
						continue
					}
					cancelErr := CancelOrder(ctx, s.executor, orderID)
					if cancelErr != nil {
						fmt.Printf("orb: cancel %s order %s: %v\n", ticker, orderID, cancelErr)
					}
					if dbErr := store.CancelORBTradeByOrderID(ctx, s.db, orderID); dbErr != nil {
						fmt.Printf("orb: update cancelled trade %s: %v\n", ticker, dbErr)
					}
					s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
						OrderID: orderID,
						Ticker:  ticker,
						Side:    sides[i],
						Status:  "cancelled",
					})
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

		// Capture session info before resetting for next day.
		s.mu.Lock()
		var sid, sdate string
		if s.session != nil {
			sid = s.session.ID
			sdate = s.session.Date.Format("2006-01-02")
		}
		s.session = nil
		s.candidates = nil
		s.rangeBuffers = make(map[string]*RangeBuffer)
		s.orderIDs = make(map[string][2]string)
		s.state = StateIdle
		s.mu.Unlock()

		s.publish(events.ORBStateChanged, events.ORBStateChangedPayload{
			State:     string(StateIdle),
			SessionID: sid,
			Date:      sdate,
		})
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
// The confidence field was previously named "conviction" — it is now "confidence"
// so that extractAnalysisJSON maps it to AnalysisResponse.Confidence, enabling
// the ensemble fast-path when all analysts unanimously agree.
type orbRawResponse struct {
	Action          string   `json:"action"`
	Confidence      float64  `json:"confidence"`
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
			Conviction:      parsed.Confidence,
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
