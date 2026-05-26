package orb

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
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
	trackedTrades      map[string]*trackedTrade // entry order ID → trade context + exit state
	reconciledSessions   map[string]struct{}  // session IDs already processed by reconcileDoneSessionTrades
	backfilledSessions   map[string]struct{}  // session IDs already processed by backfillRecentExits
	backfillFired        bool                 // true once the startup trades-feed backfill has run
	lastExitReconcileAt  time.Time            // wall time of the last per-tick trades-feed reconcile
	lastScan             time.Time            // Eastern Time of the last successful scan
	fetchFailures      map[string]int       // ticker → consecutive candle-fetch failures
	fetchLastAttempt   map[string]time.Time // ticker → wall time of last candle-fetch attempt
}

// trackedTrade holds per-order context needed to observe the full trade lifecycle.
// It is registered when the entry bracket is placed and removed when the exit fills.
type trackedTrade struct {
	dbID      string  // orb_trades primary key
	tpOrderID string  // take-profit child order ID (empty if broker doesn't expose it)
	slOrderID string  // stop-loss child order ID
	side      string  // "buy" | "sell"
	ticker    string
	qty       int
	entryFill float64 // set when entry stop order fills; 0 while still pending
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
		trackedTrades:      make(map[string]*trackedTrade),
		reconciledSessions: make(map[string]struct{}),
		backfilledSessions: make(map[string]struct{}),
		fetchFailures:    make(map[string]int),
		fetchLastAttempt: make(map[string]time.Time),
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
			return 13*60 + 30 // 13:30 UTC (9:30 AM ET)
		case "both":
			return 8 * 60 // start with London
		}
	}
	return marketOpenMinutes // 9:30 AM ET for equities
}

// sessionCleanupMinutes returns the minute-of-day (in session TZ) after which tickDone
// should cancel unfilled orders and mark the session done. Always 60 minutes after open,
// which preserves the existing 10:30 ET behaviour for equities/NY-forex while giving
// London (08:00 UTC open) a 09:00 UTC cutoff instead of the previous 14:30 UTC.
// The result is clamped to the 0–1439 range so late-evening session opens don't overflow.
func (s *ORBScheduler) sessionCleanupMinutes() int {
	return (s.sessionOpenMinutes() + 60) % (24 * 60)
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
				slog.Error("orb tick error", "err", err)
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
		return s.tickDone(ctx)
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

	// Sweep any prior-day sessions that never reached "done" — e.g. because the
	// process was killed before the cleanup window fired.
	if err := store.MarkStaleORBSessionsDone(ctx, s.db, today, s.sessionName()); err != nil {
		slog.Warn("orb: mark stale sessions done failed", "err", err)
	}

	// Once per process lifetime: reconcile any filled-but-unclosed trades across
	// all recent done sessions (up to 7 days) using the IBKR trades feed.
	if !s.backfillFired {
		s.backfillFired = true
		s.backfillRecentExits(ctx)
	}

	existing, err := store.GetORBSessionByDate(ctx, s.db, today, s.sessionName())
	if err == nil && existing != nil {
		if existing.Status == "done" || existing.Status == "skipped" {
			// Reconcile any filled-but-unclosed trades whose TP/SL may have
			// filled in IBKR while the engine was down.
			if existing.Status == "done" && s.executor != nil {
				s.reconcileDoneSessionTrades(ctx, existing.ID)
			}
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

	// Restore orderIDs and trackedTrades from DB on restart.
	if targetState == StateExecuting || targetState == StateDone {
		trades, err := store.GetORBTrades(ctx, s.db, existing.ID)
		if err != nil {
			return fmt.Errorf("load orb trades for resume: %w", err)
		}
		for _, t := range trades {
			if t.OrderID == "" {
				continue
			}
			// Restore pending entry orders into orderIDs so fill-polling resumes.
			if t.Status == "pending" {
				ids := s.orderIDs[t.Ticker]
				switch t.Side {
				case "buy":
					ids[0] = t.OrderID
				case "sell":
					ids[1] = t.OrderID
				}
				s.orderIDs[t.Ticker] = ids
			}
			// Restore exit watching for filled entries that haven't closed yet.
			if t.Status == "filled" && (t.TpOrderID != "" || t.SlOrderID != "") {
				s.trackedTrades[t.OrderID] = &trackedTrade{
					dbID:      t.ID,
					tpOrderID: t.TpOrderID,
					slOrderID: t.SlOrderID,
					side:      t.Side,
					ticker:    t.Ticker,
					qty:       t.Qty,
					entryFill: t.FillPrice,
				}
			}
		}
		slog.Info("orb: restored orderIDs from DB", "count", len(s.orderIDs),
			"exit_watching", len(s.trackedTrades))
	}

	s.setState(targetState)
	slog.Info("orb: resumed session", "session_id", existing.ID, "state", existing.Status, "candidates", len(candidates))
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

	// 1. Strict Cutoff: stop scanning once the session opens (equities/futures only).
	// Forex instruments are pre-configured — no gap scanning is needed — so the
	// cutoff doesn't apply; the scanner immediately returns the instrument list
	// and transitions to range_watch at or after session open.
	if !s.isForexMode() && sessionMinutes >= openMinutes {
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
					slog.Warn("orb: news fetch failed", "ticker", gap.Ticker, "err", newsErr)
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
				slog.Error("orb: LLM scoring failed", "ticker", gap.Ticker, "err", analyzeErr)
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

	const (
		maxFetchFailures  = 3               // drop instrument after this many consecutive failures
		fetchRetryBackoff = 60 * time.Second // minimum wait between candle-fetch attempts
	)

	allLocked := true
	for ticker, buf := range s.rangeBuffers {
		if buf.Range() != nil {
			continue // already locked
		}
		// Rate-limit retries: skip this tick if the last attempt was too recent.
		// This gives IBKR's data feed time to activate after a fresh gateway session
		// rather than burning all retries within 90 seconds.
		if last, ok := s.fetchLastAttempt[ticker]; ok && time.Since(last) < fetchRetryBackoff {
			allLocked = false
			continue
		}
		s.fetchLastAttempt[ticker] = s.now()
		candles, err := s.marketData.FetchCandles(ctx, ticker, data.Timeframe1Min, marketOpen, now)
		if err != nil {
			s.fetchFailures[ticker]++
			if s.fetchFailures[ticker] >= maxFetchFailures {
				slog.Warn("orb: dropping instrument after candle-fetch failures", "ticker", ticker, "failures", s.fetchFailures[ticker], "err", err)
				delete(s.rangeBuffers, ticker)
				delete(s.fetchLastAttempt, ticker)
			} else {
				slog.Warn("orb: fetch candles failed", "ticker", ticker, "attempt", s.fetchFailures[ticker], "max", maxFetchFailures, "err", err)
				allLocked = false
			}
			continue
		}
		s.fetchFailures[ticker] = 0    // reset on success
		delete(s.fetchLastAttempt, ticker)
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
		// Skip tickers that already have brackets placed (e.g. after restart resume).
		if ids, ok := s.orderIDs[ticker]; ok && (ids[0] != "" || ids[1] != "") {
			continue
		}
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
			slog.Info("orb: skipping instrument — opening range too narrow", "ticker", ticker, "high", r.High, "low", r.Low, "min_range", minRange)
			continue
		}

		// Forex mode: place dual-sided brackets (buy + sell) so the market
		// decides direction. Equities/futures: single side from gap direction.
		if s.isForexMode() {
			inst := s.instrumentForTicker(ticker)
			if inst == nil {
				slog.Warn("orb: skipping instrument — no config found", "ticker", ticker)
				continue
			}

			buyParams := ComputeForexBracket(r, "buy", portfolioVal, *s.forexCfg, *inst)
			sellParams := ComputeForexBracket(r, "sell", portfolioVal, *s.forexCfg, *inst)

			if buyParams.Qty == 0 && sellParams.Qty == 0 {
				slog.Warn("orb: skipping instrument — min lot size exceeds position cap", "ticker", ticker)
				continue
			}

			var ids [2]string
			for i, p := range []struct {
				side   string
				params BracketParams
			}{{"buy", buyParams}, {"sell", sellParams}} {
				if p.params.Qty == 0 {
					continue
				}
				orderID, tpID, slID, err := PlaceBracket(ctx, s.executor, p.params)
				if err != nil {
					slog.Error("orb: place bracket failed", "side", p.side, "ticker", ticker, "err", err)
					continue
				}
				ids[i] = orderID
				s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
					OrderID: orderID,
					Ticker:  ticker,
					Side:    p.side,
					Status:  "pending",
				})
				dbID, err := store.CreateORBTrade(ctx, s.db, store.ORBTrade{
					SessionID:  sessionID,
					Ticker:     ticker,
					Side:       p.side,
					EntryStop:  p.params.EntryStop,
					StopLoss:   p.params.StopLoss,
					TakeProfit: p.params.TakeProfit,
					Qty:        p.params.Qty,
					OrderID:    orderID,
					Status:     "pending",
					TpOrderID:  tpID,
					SlOrderID:  slID,
				})
				if err != nil {
					slog.Error("orb: store trade failed", "side", p.side, "ticker", ticker, "err", err)
				} else {
					s.trackedTrades[orderID] = &trackedTrade{
						dbID: dbID, tpOrderID: tpID, slOrderID: slID,
						side: p.side, ticker: ticker, qty: p.params.Qty,
					}
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
					slog.Warn("orb: skipping — no futures instrument config", "ticker", ticker)
					continue
				}
				params = ComputeFuturesBracket(r, side, portfolioVal, *s.futuresCfg, *fi)
			} else {
				params = ComputeBracket(r, side, portfolioVal, s.cfg)
			}

			if params.Qty == 0 {
				slog.Warn("orb: skipping — position size too small", "ticker", ticker)
				continue
			}

			orderID, tpID, slID, err := PlaceBracket(ctx, s.executor, params)
			if err != nil {
				slog.Error("orb: place bracket failed", "ticker", ticker, "err", err)
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
			dbID, err := store.CreateORBTrade(ctx, s.db, store.ORBTrade{
				SessionID:  sessionID,
				Ticker:     ticker,
				Side:       side,
				EntryStop:  params.EntryStop,
				StopLoss:   params.StopLoss,
				TakeProfit: params.TakeProfit,
				Qty:        params.Qty,
				OrderID:    orderID,
				Status:     "pending",
				TpOrderID:  tpID,
				SlOrderID:  slID,
			})
			if err != nil {
				slog.Error("orb: store trade failed", "ticker", ticker, "err", err)
			} else {
				s.trackedTrades[orderID] = &trackedTrade{
					dbID: dbID, tpOrderID: tpID, slOrderID: slID,
					side: side, ticker: ticker, qty: params.Qty,
				}
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

func (s *ORBScheduler) tickDone(ctx context.Context) error {
	// Paper mode: simulate stop order fills by checking current prices.
	// Real brokers fill stops server-side; PaperExecutor keeps them pending
	// until we drive fills from market data here.
	if sim, ok := s.executor.(execution.StopOrderSimulator); ok {
		pending := sim.PendingStopOrders()
		if len(pending) > 0 {
			// Collect unique tickers from pending stop orders.
			tickers := make(map[string]struct{})
			for _, req := range pending {
				tickers[req.Ticker] = struct{}{}
			}
			// Fetch current quotes and check for stop crosses.
			for ticker := range tickers {
				quote, err := s.marketData.FetchQuote(ctx, ticker)
				if err != nil {
					slog.Warn("orb: paper sim: fetch quote failed", "ticker", ticker, "err", err)
					continue
				}
				price := quote.Last
				if price == 0 {
					price = (quote.Bid + quote.Ask) / 2
				}
				if price <= 0 {
					slog.Warn("orb: paper sim: no valid quote price", "ticker", ticker, "last", quote.Last, "bid", quote.Bid, "ask", quote.Ask)
					continue
				}
				for orderID, req := range pending {
					if req.Ticker != ticker {
						continue
					}
					// Buy stop triggers when price rises to/above stop level.
					// Sell stop triggers when price falls to/below stop level.
					triggered := false
					if req.Side == execution.OrderSideBuy && price >= req.StopPrice {
						triggered = true
					} else if req.Side == execution.OrderSideSell && price <= req.StopPrice {
						triggered = true
					}
					if triggered {
						slog.Info("orb: paper sim: stop triggered", "side", req.Side, "ticker", ticker, "price", price, "stop", req.StopPrice)
						if err := sim.SimulateFill(orderID, req.StopPrice); err != nil {
							slog.Error("orb: paper sim: fill failed", "order_id", orderID, "err", err)
						}
					}
				}
			}
		}
	}

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
					slog.Warn("orb: GetOrderStatus failed", "side", s2.side, "ticker", ticker, "order_id", s2.id, "err", err)
				} else {
					s2.res = res
					s2.ok = true
					s2.filled = res.Status == "filled"
				}
			}

			// persistFill marks a side as filled in DB and emits the event only
			// if the DB row was actually updated (idempotent on retry ticks).
			// It also stamps entryFill on the trackedTrade so exit polling can begin.
			persistFill := func(s2 *sideStatus) {
				updated, dbErr := store.FillORBTradeByOrderID(ctx, s.db, s2.id, s2.res.FilledPrice)
				if dbErr != nil {
					slog.Error("orb: update filled trade failed", "side", s2.side, "ticker", ticker, "err", dbErr)
					return
				}
				if updated {
					s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
						OrderID: s2.id, Ticker: ticker, Side: s2.side, Status: "filled", FillPrice: s2.res.FilledPrice,
					})
					if tc := s.trackedTrades[s2.id]; tc != nil {
						tc.entryFill = s2.res.FilledPrice
					}
				}
			}

			// Both sides filled (rare — volatile market filled both stops before poll).
			if ss[0].filled && ss[1].filled {
				slog.Info("orb: both sides filled — persisting double fill", "ticker", ticker)
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
				slog.Info("orb: side filled — cancelling opposing", "ticker", ticker, "filled_side", filled.side, "fill_price", filled.res.FilledPrice, "cancel_side", other.side)
				persistFill(filled)
				if cancelErr := CancelOrder(ctx, s.executor, other.id); cancelErr != nil {
					// Cancel failed — check if other side also filled before retrying.
					if otherRes, err := s.executor.GetOrderStatus(ctx, other.id); err == nil && otherRes.Status == "filled" {
						slog.Info("orb: opposing side also filled — double fill", "ticker", ticker, "side", other.side, "fill_price", otherRes.FilledPrice)
						other.res = otherRes
						persistFill(other)
						delete(s.orderIDs, ticker)
					} else {
						slog.Warn("orb: cancel failed, will retry next tick", "side", other.side, "ticker", ticker, "err", cancelErr)
					}
				} else {
					if dbErr := store.CancelORBTradeByOrderID(ctx, s.db, other.id); dbErr != nil {
						slog.Error("orb: update cancelled trade failed", "side", other.side, "ticker", ticker, "err", dbErr)
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

	// Poll exit legs (TP / stop-loss) for trades whose entries have already filled.
	if s.executor != nil {
		for entryOrderID, tc := range s.trackedTrades {
			if tc.entryFill == 0 || (tc.tpOrderID == "" && tc.slOrderID == "") {
				continue // entry not yet filled, or no child IDs available
			}
			type exitCandidate struct {
				orderID  string
				exitType string
			}
			for _, ec := range []exitCandidate{
				{tc.tpOrderID, "tp"},
				{tc.slOrderID, "stop"},
			} {
				if ec.orderID == "" {
					continue
				}
				res, err := s.executor.GetOrderStatus(ctx, ec.orderID)
				if err != nil || res.Status != "filled" {
					continue
				}
				pnl := exitPnL(tc.side, tc.entryFill, res.FilledPrice, tc.qty)
				slog.Info("orb: exit filled",
					"ticker", tc.ticker, "side", tc.side,
					"exit_type", ec.exitType, "exit_price", res.FilledPrice,
					"entry_price", tc.entryFill, "pnl", pnl,
				)
				if dbErr := store.CloseORBTrade(ctx, s.db, tc.dbID, ec.exitType, res.FilledPrice, pnl); dbErr != nil {
					slog.Error("orb: close trade failed", "ticker", tc.ticker, "err", dbErr)
				}
				delete(s.trackedTrades, entryOrderID)
				break // one exit leg closes the trade
			}
		}
	}

	// Trades-feed reconcile: catch any exits that the order-ID-based loop missed
	// (e.g. when child IDs were not captured at placement due to confirmation-prompt
	// flow). Throttled to once per minute to stay well within IBKR's 10 req/s limit.
	if time.Since(s.lastExitReconcileAt) >= time.Minute {
		s.mu.Lock()
		var liveSessionID string
		if s.session != nil {
			liveSessionID = s.session.ID
		}
		s.mu.Unlock()
		if liveSessionID != "" && s.db != nil {
			if trades, err := store.GetORBTrades(ctx, s.db, liveSessionID); err == nil {
				s.reconcileExitsFromTrades(ctx, liveSessionID, trades)
				s.lastExitReconcileAt = time.Now()
			}
		}
	}

	// Cancel unfilled orders and wrap up once we are past the cleanup cutoff.
	// The cutoff is 60 minutes after session open in the session's own timezone
	// (09:00 UTC for London, 10:30 ET for New York — matching the prior hardcoded value).
	sessionTZ := s.sessionTimezone()
	sessionNow := s.now().In(sessionTZ)
	sh, sm, _ := sessionNow.Clock()
	sessionMinutes := sh*60 + sm
	if sessionMinutes >= s.sessionCleanupMinutes() {
		if s.executor != nil {
			sides := [2]string{"buy", "sell"}
			for ticker, ids := range s.orderIDs {
				for i, orderID := range ids {
					if orderID == "" {
						continue
					}
					// Reconcile already-filled orders instead of cancelling them.
					if res, err := s.executor.GetOrderStatus(ctx, orderID); err == nil && res.Status == "filled" {
						slog.Info("orb: session cleanup: already filled — persisting", "side", sides[i], "ticker", ticker, "fill_price", res.FilledPrice)
						updated, dbErr := store.FillORBTradeByOrderID(ctx, s.db, orderID, res.FilledPrice)
						if dbErr != nil {
							slog.Error("orb: update filled trade failed", "ticker", ticker, "err", dbErr)
						}
						if updated {
							s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
								OrderID: orderID, Ticker: ticker, Side: sides[i], Status: "filled", FillPrice: res.FilledPrice,
							})
						}
						continue
					}
					cancelErr := CancelOrder(ctx, s.executor, orderID)
					if cancelErr != nil {
						slog.Warn("orb: cancel order failed", "ticker", ticker, "order_id", orderID, "err", cancelErr)
					}
					if dbErr := store.CancelORBTradeByOrderID(ctx, s.db, orderID); dbErr != nil {
						slog.Error("orb: update cancelled trade failed", "ticker", ticker, "err", dbErr)
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
			slog.Error("orb: update session to done failed", "err", err)
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
		// Mark any tracked trades whose exits we never observed as expired.
		for entryOrderID, tc := range s.trackedTrades {
			if tc.entryFill != 0 {
				if dbErr := store.CloseORBTrade(ctx, s.db, tc.dbID, "expired", 0, 0); dbErr != nil {
					slog.Warn("orb: mark expired trade failed", "ticker", tc.ticker, "err", dbErr)
				}
			}
			delete(s.trackedTrades, entryOrderID)
		}
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

// reconcileDoneSessionTrades checks filled-but-unclosed trades from a session
// that was already marked done (e.g. TE was restarted after session ended).
// It polls IBKR once for each outstanding TP/SL order and closes the DB record
// if it finds a fill, preventing trades from staying "filled" indefinitely.
func (s *ORBScheduler) reconcileDoneSessionTrades(ctx context.Context, sessionID string) {
	if _, done := s.reconciledSessions[sessionID]; done {
		return
	}
	s.reconciledSessions[sessionID] = struct{}{}

	if s.db == nil {
		return
	}
	trades, err := store.GetORBTrades(ctx, s.db, sessionID)
	if err != nil {
		slog.Warn("orb: reconcile: load trades failed", "session_id", sessionID, "err", err)
		return
	}

	// Fast path: order-ID-based reconcile for trades where we captured child IDs.
	for _, t := range trades {
		if t.Status != "filled" || (t.TpOrderID == "" && t.SlOrderID == "") {
			continue
		}
		type exitCandidate struct {
			orderID  string
			exitType string
		}
		for _, ec := range []exitCandidate{
			{t.TpOrderID, "tp"},
			{t.SlOrderID, "stop"},
		} {
			if ec.orderID == "" {
				continue
			}
			res, err := s.executor.GetOrderStatus(ctx, ec.orderID)
			if err != nil || res.Status != "filled" {
				continue
			}
			pnl := exitPnL(t.Side, t.FillPrice, res.FilledPrice, t.Qty)
			slog.Info("orb: reconcile: exit found",
				"ticker", t.Ticker, "exit_type", ec.exitType,
				"exit_price", res.FilledPrice, "pnl", pnl,
			)
			if dbErr := store.CloseORBTrade(ctx, s.db, t.ID, ec.exitType, res.FilledPrice, pnl); dbErr != nil {
				slog.Error("orb: reconcile: close trade failed", "ticker", t.Ticker, "err", dbErr)
			}
			break
		}
	}

	// Fallback: trades-feed reconcile catches fills where child IDs were not captured
	// (e.g. confirmation-prompt flow dropped them) or where GetOrderStatus aged out.
	s.reconcileExitsFromTrades(ctx, sessionID, trades)
}

func (s *ORBScheduler) notify(ctx context.Context, msg string) {
	if s.notifier == nil {
		return
	}
	_ = s.notifier.Notify(ctx, msg)
}

// reconcileExitsFromTrades fetches recent fills from the IBKR trades endpoint and
// matches them to any filled-but-unclosed ORB trades for the given session.
// It is the primary reconciliation mechanism when child order IDs are unavailable.
func (s *ORBScheduler) reconcileExitsFromTrades(ctx context.Context, sessionID string, trades []store.ORBTrade) {
	fetcher, ok := s.executor.(TradesFetcher)
	if !ok {
		return // executor doesn't expose trade history (e.g. Alpaca, PaperExecutor)
	}
	var hasFilled bool
	for _, t := range trades {
		if t.Status == "filled" {
			hasFilled = true
			break
		}
	}
	if !hasFilled {
		return
	}
	records, err := fetcher.FetchTradeRecords(ctx, 7)
	if err != nil {
		slog.Warn("orb: exit reconcile: fetch trade records failed", "session_id", sessionID, "err", err)
		return
	}
	s.matchExitsFromRecords(ctx, trades, records)
}

// matchExitsFromRecords attributes IBKR fill records to filled-but-unclosed ORB trades
// using a tuple match: same symbol, opposite side, same qty, fill time after entry, and
// price within 10% of the TP–SL range of one of the bracket levels.
func (s *ORBScheduler) matchExitsFromRecords(ctx context.Context, trades []store.ORBTrade, records []execution.IBKRTradeRecord) {
	for i := range trades {
		t := &trades[i]
		if t.Status != "filled" || t.FillPrice == 0 {
			continue
		}

		// Use FilledAt (entry fill time) as the lower bound, minus a 60s buffer to
		// account for the poll lag between the actual IBKR fill and when we persisted
		// it. This prevents filtering out legitimate exits that arrived quickly after
		// the entry fill while still rejecting fills that predate the order placement.
		// Falls back to CreatedAt (bracket placement time) when filled_at is unset.
		entryTime := t.CreatedAt
		if t.FilledAt != nil {
			buffered := t.FilledAt.Add(-60 * time.Second)
			if buffered.After(t.CreatedAt) {
				entryTime = buffered
			}
		}

		exitSide := "sell"
		if t.Side == "sell" {
			exitSide = "buy"
		}

		// Allow a 10% tolerance relative to the bracket range so minor slippage
		// on the stop leg doesn't prevent attribution.
		tolerance := math.Abs(t.TakeProfit-t.StopLoss) * 0.1
		if tolerance == 0 {
			tolerance = t.FillPrice * 0.001 // fallback: 0.1% of fill price
		}

		for _, rec := range records {
			if rec.Symbol != t.Ticker {
				continue
			}
			if rec.Side != exitSide {
				continue
			}
			if math.Abs(rec.Qty-float64(t.Qty)) > 0.5 {
				continue
			}
			if !rec.FilledAt.After(entryTime) {
				continue
			}

			diffTP := math.Abs(rec.Price - t.TakeProfit)
			diffSL := math.Abs(rec.Price - t.StopLoss)
			if diffTP > tolerance && diffSL > tolerance {
				continue // fill price not near either bracket level
			}

			exitType := "tp"
			if diffSL < diffTP {
				exitType = "stop"
			}

			pnl := exitPnL(t.Side, t.FillPrice, rec.Price, t.Qty)
			slog.Info("orb: exit reconcile: matched",
				"ticker", t.Ticker, "side", t.Side,
				"exit_type", exitType, "exit_price", rec.Price,
				"entry_price", t.FillPrice, "pnl", pnl,
			)
			// Mark in-memory so subsequent iterations in this pass skip it.
			t.Status = "closed"
			if s.db != nil {
				if dbErr := store.CloseORBTrade(ctx, s.db, t.ID, exitType, rec.Price, pnl); dbErr != nil {
					slog.Error("orb: exit reconcile: close trade failed", "ticker", t.Ticker, "err", dbErr)
				}
			}
			break
		}
	}
}

// backfillRecentExits scans done sessions from the last 7 days for any
// filled-but-unclosed trades and reconciles them via the IBKR trades feed.
// Runs synchronously once per process lifetime from tickIdle.
func (s *ORBScheduler) backfillRecentExits(ctx context.Context) {
	fetcher, ok := s.executor.(TradesFetcher)
	if !ok {
		return
	}
	if s.db == nil {
		return
	}

	records, err := fetcher.FetchTradeRecords(ctx, 7)
	if err != nil {
		slog.Warn("orb: backfill: fetch trade records failed", "err", err)
		return
	}

	sessions, err := store.GetRecentORBSessions(ctx, s.db, 20)
	if err != nil {
		slog.Warn("orb: backfill: load sessions failed", "err", err)
		return
	}

	for _, sess := range sessions {
		if sess.Status != "done" {
			continue
		}
		if _, already := s.backfilledSessions[sess.ID]; already {
			continue
		}
		trades, err := store.GetORBTrades(ctx, s.db, sess.ID)
		if err != nil {
			continue
		}
		var hasFilled bool
		for _, t := range trades {
			if t.Status == "filled" {
				hasFilled = true
				break
			}
		}
		if !hasFilled {
			continue
		}
		slog.Info("orb: backfill: reconciling session",
			"session_id", sess.ID, "date", sess.Date.Format("2006-01-02"), "name", sess.SessionName)
		s.matchExitsFromRecords(ctx, trades, records)
		s.backfilledSessions[sess.ID] = struct{}{}
	}
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
