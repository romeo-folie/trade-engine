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

// defaultMaxScanCandidates caps how many top gap movers the equities ORB scan
// LLM-scores when config leaves max_scan_candidates unset. Bounds rate-limited
// model calls so the scan finishes before the opening range forms.
const defaultMaxScanCandidates = 15

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
	cfg                     config.ORBConfig
	db                      *pgxpool.Pool
	marketData              data.MarketDataProvider
	analyzer                *analysis.Analyzer
	executor                execution.Executor
	portfolio               PortfolioValuer
	notifier                Notifier
	news                    data.NewsProvider        // optional; nil disables news enrichment
	bus                     *events.Bus              // optional; nil means no event publishing
	now                     func() time.Time         // injectable for tests
	scanner                 CandidateScanner         // optional; nil uses inline equities scanning
	forexCfg                *config.ForexORBConfig   // optional; nil means equities mode
	futuresCfg              *config.FuturesORBConfig // optional; nil means not futures mode
	feedbackCfg             *config.FeedbackConfig   // optional; nil disables feedback enrichment
	optionsRouter           *OptionsRouter           // optional; nil disables options-ORB execution (default). Wired by #418.
	optionExit              *OptionExitManager       // optional; nil disables options-ORB app-managed exits (default). Wired by #418.
	lastOptionExitCheckAt   time.Time                // wall time of the last options-ORB exit sweep
	lastOptionOrphanCheckAt time.Time                // wall time of the last options-ORB orphan sweep
	alertedOrphanOptions    map[string]struct{}      // OCC symbols already alerted as orphans (re-armed on resolve)
	pendingOptionExits      map[string]pendingExit   // DB trade ID → in-flight exit sell (polled, not re-sent)

	mu                      sync.Mutex // protects state, session, candidates for TUI reads
	state                   State
	session                 *store.ORBSession
	candidates              []ScoredCandidate
	rangeBuffers            map[string]*RangeBuffer
	orderIDs                map[string][2]string     // ticker → [buyOrderID, sellOrderID] (runner / primary entry)
	scaleOutEntryIDs        map[string][2]string     // ticker → [buyPartialID, sellPartialID]; the split-bracket PARTIAL entry alongside orderIDs' runner. Empty unless partial-TP is configured, so the no-partial path is unchanged.
	splitPlaced             bool                     // true once a split-bracket pair was placed this session; gates the orphaned-split-entry sweep so it is a no-op otherwise
	lastSplitOrphanCheckAt  time.Time                // wall time of the last orphaned-split-entry sweep
	trackedTrades           map[string]*trackedTrade // entry order ID → trade context + exit state
	reconciledSessions      map[string]struct{}      // session IDs already processed by reconcileDoneSessionTrades
	backfilledSessions      map[string]struct{}      // session IDs already processed by backfillRecentExits
	backfillFired           bool                     // true once the startup trades-feed backfill has run
	lastExitReconcileAt     time.Time                // wall time of the last per-tick trades-feed reconcile
	lastPaperExitCheckAt    time.Time                // wall time of the last paper-mode exit-monitor sweep
	lastTimeExitCheckAt     time.Time                // wall time of the last max-hold time-exit sweep
	lastAdaptiveStopCheckAt time.Time                // wall time of the last adaptive-stop (break-even/trailing) sweep
	lastOrphanLegCheckAt    time.Time                // wall time of the last orphan-leg reconcile
	alertedOrphanLegs       map[string]struct{}      // order IDs already alerted as unprovable orphans (de-dups notifications)
	lastOrphanPosCheckAt    time.Time                // wall time of the last broker-position orphan detection
	alertedOrphanPositions  map[string]struct{}      // tickers already alerted as unmanaged broker positions (re-armed on resolve)
	lastOrphanFillReconcile time.Time                // wall time of the last broker-truth entry-fill reconcile
	directionDecisions      map[string][2]bool       // ticker → [longEligible, shortEligible], latched at the first executing tick
	lastScan                time.Time                // Eastern Time of the last successful scan
	fetchFailures           map[string]int           // ticker → consecutive candle-fetch failures
	fetchLastAttempt        map[string]time.Time     // ticker → wall time of last candle-fetch attempt
}

// trackedTrade holds per-order context needed to observe the full trade lifecycle.
// It is registered when the entry bracket is placed and removed when the exit fills.
type trackedTrade struct {
	dbID      string // orb_trades primary key
	tpOrderID string // take-profit child order ID (empty if broker doesn't expose it)
	slOrderID string // stop-loss child order ID
	side      string // "buy" | "sell"
	ticker    string
	qty       int
	entryStop float64 // the entry stop trigger price; used to sanity-check the reported fill
	entryFill float64 // set when entry stop order fills; 0 while still pending
	// isOption marks an options-ORB entry. The fill-price sanitizer compares the
	// reported fill against entryStop, but for an option entryStop is the UNDERLYING
	// breakout level while the fill is the option PREMIUM — orders of magnitude apart
	// — so the sanitizer must be skipped or it would overwrite a $2 premium with the
	// $550 underlying level and corrupt all downstream P&L.
	isOption bool
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
		cfg:                    cfg,
		db:                     db,
		marketData:             marketData,
		analyzer:               analyzer,
		executor:               executor,
		portfolio:              portfolio,
		notifier:               notifier,
		now:                    time.Now,
		state:                  StateIdle,
		rangeBuffers:           make(map[string]*RangeBuffer),
		orderIDs:               make(map[string][2]string),
		scaleOutEntryIDs:       make(map[string][2]string),
		trackedTrades:          make(map[string]*trackedTrade),
		reconciledSessions:     make(map[string]struct{}),
		backfilledSessions:     make(map[string]struct{}),
		fetchFailures:          make(map[string]int),
		fetchLastAttempt:       make(map[string]time.Time),
		alertedOrphanLegs:      make(map[string]struct{}),
		alertedOrphanPositions: make(map[string]struct{}),
		pendingOptionExits:     make(map[string]pendingExit),
		directionDecisions:     make(map[string][2]bool),
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

// throttledInstrument applies the feedback auto-throttle to an instrument's risk multiplier:
// it MULTIPLIES the static per-instrument risk_multiplier by the nightly-derived multiplier
// stored from this instrument's rolling realised expectancy. A complete no-op (returns inst
// unchanged) unless the auto-throttle is enabled, a DB is available, and a stored multiplier
// exists for the configured window — so disabled / no-data sizing is exactly as before.
func (s *ORBScheduler) throttledInstrument(ctx context.Context, inst config.Instrument) config.Instrument {
	if s.feedbackCfg == nil || !s.feedbackCfg.ORBAutoThrottle.Enabled || s.db == nil {
		return inst
	}
	at := s.feedbackCfg.ORBAutoThrottle
	window := at.ThrottleWindow()
	// Only trust a recently-refreshed multiplier: a stale one (the window dried up / dropped
	// below min-trades, so the reviewer stopped rewriting it) must not keep sizing live trades.
	// GetFreshORBRiskMultiplier normalises this to a UTC calendar date, so a wall-clock value
	// with a time-of-day is fine.
	notBefore := s.now().AddDate(0, 0, -at.MaxAgeDaysOrDefault())
	auto, ok, err := store.GetFreshORBRiskMultiplier(ctx, inst.Symbol, window, notBefore)
	if err != nil {
		slog.Warn("orb: auto-throttle: read multiplier failed — using static risk", "ticker", inst.Symbol, "err", err)
		return inst
	}
	if !ok || auto <= 0 {
		return inst // no fresh opinion (no recent review, below min-trades, or window dried up)
	}
	base := inst.RiskMultiplier
	if base <= 0 {
		base = 1.0
	}
	inst.RiskMultiplier = base * auto // inst is a value copy; caller's config is untouched
	slog.Info("orb: auto-throttle applied", "ticker", inst.Symbol, "window", window,
		"static", base, "auto", auto, "effective", inst.RiskMultiplier)
	return inst
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

	// Run the state handler first: its broker reconcilers (in-session/done-session order
	// polling and the trades-feed matcher) get the first chance each tick to record a
	// *confirmed* exit. The bar-replay backstop runs after, so a late broker fill (one
	// that lands past the grace period) is recorded as 'broker' rather than pre-empted
	// by replay — both close paths guard on status='filled', and this ordering decides
	// which source wins when both could fire in the same tick.
	var err error
	switch state {
	case StateIdle:
		err = s.tickIdle(ctx, et)
	case StateScanning:
		err = s.tickScanning(ctx, et)
	case StateCandidateReady:
		err = s.tickCandidateReady(ctx, et)
	case StateRangeWatch:
		err = s.tickRangeWatch(ctx, et)
	case StateExecuting:
		err = s.tickExecuting(ctx)
	case StateDone:
		err = s.tickDone(ctx)
	}

	// Risk-containment backstop: flatten any filled trade past its per-instrument max-hold
	// limit (exit_type='time_exit'). Runs in every state because a filled bracket outlives
	// its session, and before reconcileBarReplayExits so a genuinely-open trade is recorded
	// as a confirmed time exit rather than pre-empted by a replayed TP/stop touch. No-op
	// outside forex mode or when max-hold is unconfigured. Throttled (see enforceTimeExits).
	s.enforceTimeExits(ctx)

	// Options-ORB app-managed exits (issue #415): watch the underlying against each
	// filled option's stored stop/target, apply the IV-crush time exit, and flatten
	// before the close. No-op when the exit manager is unset (non-options runs).
	s.enforceOptionExits(ctx)

	// Options-ORB orphan detection (issue #416): alert on broker option positions no
	// managed trade explains (detection-only). No-op unless options-ORB is active.
	s.detectOrphanOptionPositions(ctx)

	// Adaptive stop management (Phase 4 live): ratchet break-even / trailing stops on filled
	// forex trades whose instrument opts in. No-op when no instrument configures it. Runs
	// AFTER enforceTimeExits so a trade past max-hold is flattened rather than having its
	// stop pointlessly nudged the same tick. Throttled (see enforceAdaptiveStops).
	s.enforceAdaptiveStops(ctx)

	// Cross-tick cleanup of bracket legs left live after a time exit (the inline cancel
	// is best-effort; this retries off the working-order list, decoupled from the now-closed
	// trade row, and alerts on orders it can't prove ownership of). No-op outside forex mode.
	s.reconcileOrphanedLegs(ctx)

	// Broker-truth entry-fill reconcile (opt-in): when the broker holds a position
	// that a still-pending ORB entry explains, record the fill and cancel the
	// opposing OCO leg — the metals blind spot where GetOrderStatus can't confirm
	// the fill. Runs BEFORE the detector so resolved positions don't also alert.
	s.reconcileOrphanFills(ctx)

	// Detection-only: alert on any broker position no filled ORB trade explains
	// (the July metals-short orphan shape — filled at the broker, 'cancelled' in
	// the DB, invisible to every status='filled' reconciler). Places no orders.
	s.detectOrphanedPositions(ctx)

	// Cancel a split-bracket entry that never filled once its sibling has already exited: the
	// logical trade is over, so a still-pending entry must not fill on a breakout retest and
	// open a fresh position. No-op unless a split pair was placed this session.
	s.cancelOrphanedSplitEntries(ctx)

	// Universal exit backstop: record TP/stop exits for already-filled trades by replaying
	// their price path from market-data bars. Runs in every state because a bracket's exit
	// is effectively GTC — it can fill while a new session is scanning, or days after the
	// session that opened it went "done". It runs for every executor and is the only
	// mechanism that records an outcome when child IDs were never captured and the trades
	// feed has aged out. Throttled; gated by a grace period (see reconcileBarReplayExits).
	s.reconcileBarReplayExits(ctx)

	return err
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
			// Restore pending entry orders into the tracking maps so fill-polling resumes.
			// A split-bracket PARTIAL row routes into scaleOutEntryIDs so it doesn't clobber
			// its runner's slot in orderIDs (both share ticker+side); everything else (runner
			// or ordinary single bracket) routes into orderIDs.
			if t.Status == "pending" {
				m := s.orderIDs
				if t.ScaleOutRole == "partial" {
					m = s.scaleOutEntryIDs
				}
				if t.ScaleOutRole != "" {
					s.splitPlaced = true // a restored split leg can be orphaned by its sibling exiting
				}
				ids := m[t.Ticker]
				switch t.Side {
				case "buy":
					ids[0] = t.OrderID
				case "sell":
					ids[1] = t.OrderID
				}
				m[t.Ticker] = ids
				// Also track the entry stop so the implausible-fill guard (sanitizeEntryFill)
				// still fires if this order fills after a mid-session restart. entryFill stays
				// 0 until it actually fills; the exit-poll loop skips entryFill==0 trades.
				s.trackedTrades[t.OrderID] = &trackedTrade{
					dbID:      t.ID,
					tpOrderID: t.TpOrderID,
					slOrderID: t.SlOrderID,
					side:      t.Side,
					ticker:    t.Ticker,
					qty:       t.Qty,
					entryStop: t.EntryStop,
					// Preserve the option flag across restarts so the fill-poll skips
					// the underlying-level sanitizer when a pending option fills after
					// a resume (else the premium is corrupted to the underlying level).
					isOption: t.AssetClass == "option",
				}
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
					entryStop: t.EntryStop,
					entryFill: t.FillPrice,
					isOption:  t.AssetClass == "option",
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
		gaps, gapErr := data.FetchGapMovers(ctx, alpacaClient, data.GapScreen{
			MinGapPct:       s.cfg.GapThresholdPct,
			MinPrice:        s.cfg.MinPrice,
			MinDollarVolume: s.cfg.MinDollarVolume,
			RankByLiquidity: true,
		})
		if gapErr != nil {
			return fmt.Errorf("scan gap movers: %w", gapErr)
		}
		s.lastScan = sessionNow

		if len(gaps) == 0 {
			return nil
		}

		// Cap how many gap movers get LLM-scored. This is the production equities
		// path (te run --mode orb / orb-newyork construct the scheduler without a
		// CandidateScanner), so the cap MUST live here too — not only in
		// EquitiesCandidateScanner. FetchGapMovers ranks liquidity-first (|gap%| ×
		// log10 dollar-volume) after applying the price/volume floors, so we keep the
		// most tradeable movers and bound the rate-limited model calls; a large gap
		// universe otherwise sprays dozens of calls into the pre-open window and
		// stalls on the provider quota, finishing after the range opens.
		maxScan := s.cfg.MaxScanCandidates
		if maxScan <= 0 {
			maxScan = defaultMaxScanCandidates
		}
		if len(gaps) > maxScan {
			slog.Info("orb: gap universe capped for scoring", "found", len(gaps), "scoring", maxScan)
			gaps = gaps[:maxScan]
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

			// Score with the analysts DIRECTLY (no arbiter) and aggregate by confidence,
			// so a gap candidate is selectable with however many analysts are reachable
			// (even one) — the equities scan must not go dark when the arbiter is
			// unconfigured/out-of-credits or a provider is throttled. Errors only when
			// EVERY analyst failed.
			analystResults, analyzeErr := s.analyzer.AnalyzeAnalysts(ctx, analysis.AnalysisRequest{
				Ticker:             gap.Ticker,
				CustomSystemPrompt: sysPrompt,
				CustomUserMessage:  userMsg,
			}, depth.ImpactScore{Score: 1.0, Depth: 3})
			if analyzeErr != nil {
				slog.Error("orb: LLM scoring failed — no analysts available", "ticker", gap.Ticker, "err", analyzeErr)
				continue
			}

			// Winning-action mean over the successful analysts, gated on the confidence
			// threshold. parseORBResult (inside) pulls the ORB-specific catalyst/risks fields.
			result := aggregateORBScore(analystResults, s.scanConfidenceThreshold())
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
		maxFetchFailures  = 3                // drop instrument after this many consecutive failures
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
		s.fetchFailures[ticker] = 0 // reset on success
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

	// Don't open a new ORB position on a ticker that still holds an unexited filled
	// trade from a prior session. Two reasons: it would stack uncontrolled risk on the
	// same instrument, and — because a time-exit cancels dangling bracket legs by symbol
	// — a later flatten of the stale trade could otherwise cancel these fresh entries.
	// The stale trade is flattened by enforceTimeExits, freeing the ticker next session.
	heldTickers := s.tickersWithOpenFilledTrades(ctx)

	var msgs []string
	// pendingConfirmation becomes true when a confirmation-mode ticker has not yet
	// confirmed its breakout; the session then stays in Executing and retries next tick.
	pendingConfirmation := false
	for ticker, buf := range s.rangeBuffers {
		// Skip tickers that already have brackets placed (e.g. after restart resume, or a
		// confirmation that fired on an earlier tick). Check both maps so a ticker whose
		// runner already cleared but whose split-bracket partial is still working is not
		// re-placed.
		if ids, ok := s.orderIDs[ticker]; ok && (ids[0] != "" || ids[1] != "") {
			continue
		}
		if ids, ok := s.scaleOutEntryIDs[ticker]; ok && (ids[0] != "" || ids[1] != "") {
			continue
		}
		if _, held := heldTickers[ticker]; held {
			slog.Info("orb: skipping instrument — an unexited filled ORB trade is still open", "ticker", ticker)
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

			// Entry-quality gate 1: ATR-normalised range filter (Phase 2).
			if ok, ratio, reason := s.passesRangeQuality(ctx, ticker, r, *inst); !ok {
				slog.Info("orb: skipping instrument — range quality", "ticker", ticker, "ratio", ratio, "reason", reason)
				continue
			}

			// Feedback auto-throttle: scale this instrument's risk by its rolling realised
			// expectancy (a no-op unless enabled and data exists). Computed once, shared by
			// both sides — they have identical sizing inputs.
			tinst := s.throttledInstrument(ctx, *inst)
			buyParams := ComputeForexBracket(r, "buy", portfolioVal, *s.forexCfg, tinst)
			sellParams := ComputeForexBracket(r, "sell", portfolioVal, *s.forexCfg, tinst)

			if buyParams.Qty == 0 && sellParams.Qty == 0 {
				slog.Warn("orb: skipping instrument — min lot size exceeds position cap", "ticker", ticker)
				continue
			}

			// Entry-quality gate 2: spread/risk filter (Phase 2). Buy and sell brackets
			// have the same planned risk magnitude, so one check covers both sides.
			if ok, ratio, reason := s.passesSpread(ctx, ticker, buyParams.EntryStop, buyParams.StopLoss, inst.MaxSpreadToRisk); !ok {
				slog.Info("orb: skipping instrument — spread too wide", "ticker", ticker, "ratio", ratio, "reason", reason)
				continue
			}

			// Entry-quality gate 3: commission/dollar-risk filter. Checked per side — the two
			// brackets share the risk magnitude but the notional cap can round their qtys to
			// different lots, and the ratio depends on qty. A failing side is zeroed (skipped
			// by the placement loop), mirroring how direction eligibility disables a side.
			if ok, ratio, reason := passesCommission(buyParams.Qty, buyParams.EntryStop, buyParams.StopLoss, *inst); !ok && buyParams.Qty > 0 {
				slog.Info("orb: disabling buy side — commission too large for planned risk", "ticker", ticker, "ratio", ratio, "reason", reason)
				buyParams.Qty = 0
			}
			if ok, ratio, reason := passesCommission(sellParams.Qty, sellParams.EntryStop, sellParams.StopLoss, *inst); !ok && sellParams.Qty > 0 {
				slog.Info("orb: disabling sell side — commission too large for planned risk", "ticker", ticker, "ratio", ratio, "reason", reason)
				sellParams.Qty = 0
			}
			if buyParams.Qty == 0 && sellParams.Qty == 0 {
				slog.Info("orb: skipping instrument — commission too large for planned risk on both sides", "ticker", ticker)
				continue
			}

			// Direction eligibility (Phase 3): place only the trend-aligned side(s). The
			// decision is LATCHED at the first executing tick (directionDecision) so it can't
			// drift across confirmation retries. Zeroing a side's Qty makes the placement loop
			// (and confirmation) skip it; fail-open keeps both sides.
			if inst.DirectionEligibility {
				longOK, shortOK := s.directionDecision(ctx, ticker)
				if !longOK {
					buyParams.Qty = 0
				}
				if !shortOK {
					sellParams.Qty = 0
				}
				if buyParams.Qty == 0 && sellParams.Qty == 0 {
					slog.Info("orb: skipping instrument — neither direction is trend-eligible", "ticker", ticker)
					continue
				}
			}

			// Breakout confirmation (Phase 3). With stop_touch (default) we place resting
			// stops now and the broker fills on a touch. With a close_* mode we instead WAIT:
			// each tick, replay the 1-min candles since the range locked and only place once
			// a confirmed close prints — then place ONLY the confirmed side (zero the other so
			// the placement loop below skips it). No confirmation yet → leave the ticker
			// unplaced and keep the session in Executing (pendingConfirmation) to retry next tick.
			// Stop-touch (default) rests stop-entry brackets at the breakout level. Confirmation
			// modes instead enter at MARKET once a close confirms — placeFn switches accordingly.
			placeFn := PlaceBracket
			if RequiresConfirmation(inst.BreakoutConfirmation) {
				side := s.confirmedSide(ctx, ticker, r, buyParams, sellParams, inst.BreakoutConfirmation)
				if side == "" {
					pendingConfirmation = true
					continue
				}
				slog.Info("orb: breakout confirmed", "ticker", ticker, "side", side, "mode", inst.BreakoutConfirmation)
				if side == "buy" {
					sellParams.Qty = 0
				} else {
					buyParams.Qty = 0
				}
				placeFn = PlaceMarketBracket
			}

			// placeAndRecord places one bracket, records its orb_trades row and starts
			// tracking it, returning the broker entry order ID and the DB row id. role is
			// "runner"/"partial" for the two legs of a split-bracket trade, "" for an
			// ordinary single bracket; parentDBID links a partial row to its runner row.
			// Returns ("","") only when the broker placement itself failed; a DB-record
			// failure still returns the orderID so the live order stays OCO-managed.
			placeAndRecord := func(side string, params BracketParams, role, parentDBID string) (string, string) {
				orderID, tpID, slID, err := placeFn(ctx, s.executor, params)
				if err != nil {
					slog.Error("orb: place bracket failed", "side", side, "ticker", ticker, "role", role, "err", err)
					return "", ""
				}
				s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
					OrderID: orderID, Ticker: ticker, Side: side, Status: "pending",
				})
				var parentPtr *string
				if parentDBID != "" {
					parentPtr = &parentDBID
				}
				dbID, err := store.CreateORBTrade(ctx, s.db, store.ORBTrade{
					SessionID:        sessionID,
					Ticker:           ticker,
					Side:             side,
					EntryStop:        params.EntryStop,
					StopLoss:         params.StopLoss,
					TakeProfit:       params.TakeProfit,
					Qty:              params.Qty,
					OrderID:          orderID,
					Status:           "pending",
					TpOrderID:        tpID,
					SlOrderID:        slID,
					ScaleOutRole:     role,
					ScaleOutParentID: parentPtr,
				})
				if err != nil {
					slog.Error("orb: store trade failed", "side", side, "ticker", ticker, "role", role, "err", err)
					return orderID, ""
				}
				s.trackedTrades[orderID] = &trackedTrade{
					dbID: dbID, tpOrderID: tpID, slOrderID: slID,
					side: side, ticker: ticker, qty: params.Qty,
					entryStop: params.EntryStop,
				}
				label := ticker
				if role != "" {
					label = ticker + " [" + role + "]"
				}
				msgs = append(msgs, fmt.Sprintf("  %s %s: entry=%.4f stop=%.4f target=%.4f qty=%d",
					strings.ToUpper(side), label, params.EntryStop, params.StopLoss, params.TakeProfit, params.Qty))
				return orderID, dbID
			}

			var ids, scaleIds [2]string
			for i, p := range []struct {
				side   string
				params BracketParams
			}{{"buy", buyParams}, {"sell", sellParams}} {
				if p.params.Qty == 0 {
					continue
				}
				// Options-ORB (issue #414): for options-mode underlyings, execute this
				// side as a long option instead of a bracket. Guarded by a nil-by-default
				// router, so the equity/forex/futures bracket path below is unchanged.
				// NOTE: there is a TWIN of this hook in the equities/futures branch
				// (the `else` below) — options-mode underlyings are equities, so that
				// twin is the one that actually fires in production. Keep both in sync.
				if s.optionsRouter.Handles(ticker) {
					orderID, line := s.routeOptionsEntry(ctx, sessionID, ticker, p.side, p.params)
					if orderID != "" {
						// Record the entry so the session's re-entry guard fires — options
						// have no broker bracket to mark the ticker as entered, so without
						// this a fresh order would be placed every tick.
						ids[i] = orderID
					}
					if line != "" {
						msgs = append(msgs, line)
					}
					continue
				}
				// Split-bracket partial take-profit (Phase 4 live): when the instrument
				// configures a partial, enter as a PARTIAL bracket (nearer target) plus a
				// RUNNER bracket (farther target) sharing the stop. split=false (the default,
				// and when the size is too small to split) keeps the single base bracket — the
				// no-partial path is byte-identical to before.
				partialParams, runnerParams, split := ScaleOutBrackets(p.params, *inst)
				runnerRole := ""
				if split {
					runnerRole = "runner"
				}
				// Place the runner first so its DB row exists to parent the partial. If the
				// broker placement fails, skip the side entirely — never place a naked partial.
				runnerID, runnerDBID := placeAndRecord(p.side, runnerParams, runnerRole, "")
				if runnerID == "" {
					continue
				}
				ids[i] = runnerID
				if split {
					// Link the partial to the runner row (parent nil if the runner row failed
					// to persist — both orders are still live and OCO-managed; the link is
					// reporting-only).
					partialID, _ := placeAndRecord(p.side, partialParams, "partial", runnerDBID)
					scaleIds[i] = partialID
				}
			}
			s.orderIDs[ticker] = ids
			if scaleIds[0] != "" || scaleIds[1] != "" {
				s.scaleOutEntryIDs[ticker] = scaleIds
				s.splitPlaced = true // arm the orphaned-split-entry sweep for this session
			}
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

			// Options-ORB (issues #414/#419): route options-mode underlyings to a
			// long option instead of a stock bracket. THIS equities/futures branch is
			// the path the options-mode (equities) scheduler actually takes — the
			// forex branch above has the parallel hook. Guarded by a nil-by-default
			// router and Handles() (only configured underlyings), so the stock/futures
			// bracket placement below is unchanged when options-ORB is off.
			if s.optionsRouter.Handles(ticker) {
				orderID, line := s.routeOptionsEntry(ctx, sessionID, ticker, side, params)
				if orderID != "" {
					// Record the entry so the re-entry guard fires (options have no
					// broker bracket to mark the ticker as entered).
					if side == "sell" {
						s.orderIDs[ticker] = [2]string{"", orderID}
					} else {
						s.orderIDs[ticker] = [2]string{orderID, ""}
					}
					// Emit the activity-feed event the bracket path emits below, so an
					// options entry shows up like any other ORB entry.
					s.publish(events.TradeSimulated, events.TradeExecutedPayload{
						Ticker: ticker, Direction: side, Strategy: "orb",
					})
				}
				if line != "" {
					msgs = append(msgs, line)
				}
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
					entryStop: params.EntryStop,
				}
			}
			msgs = append(msgs, fmt.Sprintf("  %s %s: entry=%.4f stop=%.4f target=%.4f qty=%d",
				strings.ToUpper(side), ticker, params.EntryStop, params.StopLoss, params.TakeProfit, params.Qty))
		}
	}

	if len(msgs) > 0 {
		s.notify(ctx, "ORB Brackets Placed:\n"+strings.Join(msgs, "\n"))
	}

	// Stay in Executing while a confirmation-mode ticker is still being watched and the
	// entry window is open — the breakout-watch re-runs next tick. Once everything has
	// confirmed/placed (or the entry window closes, cancelling unconfirmed setups), fall
	// through to Done and let tickDone clean up.
	if pendingConfirmation && s.withinEntryWindow() {
		// Don't starve trade management while we wait: run tickDone's reconciliation so an
		// already-placed side's fill is persisted, its opposing dual-sided stop is cancelled
		// (OCO), and paper stop fills are driven from market data. tickDone's cleanup block is
		// gated on the cutoff — the exact complement of withinEntryWindow — so it is skipped
		// here and no session-end transition happens while we're still inside the window.
		_ = s.tickDone(ctx)
		return nil
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

	// Poll entry fills and enforce app-managed cross-side OCO for forex brackets, including
	// split-bracket partials. Per ticker, gather every working entry order — runner + partial,
	// both sides (scaleOutEntryIDs is empty unless partial-TP is configured, so a no-partial
	// ticker reduces to the original [buy,sell] pair). Whichever SIDE fills first wins: its
	// fills are persisted and the opposing side's orders are cancelled (the broker only OCOs
	// the TP/SL legs WITHIN one bracket; cross-side and cross-bracket OCO is app-managed). A
	// single-sided entry (confirmation / direction eligibility) has nothing to cancel — its
	// fills are just persisted. Orders resolve individually, so a lagging partial fill is
	// picked up on a later tick; clearEntryID drops each resolved order and forgets the ticker
	// once no working entry remains.
	if s.executor != nil {
		seen := make(map[string]struct{})
		tickers := make([]string, 0, len(s.orderIDs)+len(s.scaleOutEntryIDs))
		for _, m := range []map[string][2]string{s.orderIDs, s.scaleOutEntryIDs} {
			for ticker := range m {
				if _, dup := seen[ticker]; !dup {
					seen[ticker] = struct{}{}
					tickers = append(tickers, ticker)
				}
			}
		}
		for _, ticker := range tickers {
			type entryOrder struct {
				id     string
				side   string
				res    execution.OrderResult
				filled bool
			}
			var entries []entryOrder
			for _, m := range []map[string][2]string{s.orderIDs, s.scaleOutEntryIDs} {
				ids := m[ticker]
				for i, id := range ids {
					if id == "" {
						continue
					}
					side := "buy"
					if i == 1 {
						side = "sell"
					}
					entries = append(entries, entryOrder{id: id, side: side})
				}
			}
			if len(entries) == 0 {
				continue
			}

			buyFilled, sellFilled := false, false
			for k := range entries {
				res, err := s.executor.GetOrderStatus(ctx, entries[k].id)
				if err != nil {
					slog.Warn("orb: GetOrderStatus failed", "side", entries[k].side, "ticker", ticker, "order_id", entries[k].id, "err", err)
					continue
				}
				entries[k].res = res
				entries[k].filled = res.Status == "filled"
				// Options enter with LIMIT orders, which can partially fill. Treat a
				// partial fill as a fill (so the held contracts are managed by the exit
				// watcher, never orphaned by the session-cutoff cancel) and cancel the
				// unfilled remainder so no further contracts land beyond the recorded row.
				if !entries[k].filled && res.FilledQty > 0 {
					if tc := s.trackedTrades[entries[k].id]; tc != nil && tc.isOption {
						if cerr := CancelOrder(ctx, s.executor, entries[k].id); cerr != nil {
							// The remainder is still working and could fill MORE contracts than
							// we'd persist. Leave the entry pending and retry the cancel next
							// tick rather than locking in a qty the broker may exceed.
							slog.Warn("orb: cancel option partial-fill remainder failed; leaving pending to retry",
								"ticker", ticker, "order_id", entries[k].id, "err", cerr)
							continue
						}
						slog.Info("orb: option entry partially filled — persisting filled portion, remainder cancelled",
							"ticker", ticker, "order_id", entries[k].id, "filled_qty", res.FilledQty, "fill_price", res.FilledPrice)
						// Persist the filled portion WITH its actual contract count so the row
						// reflects reality — the downstream (qty-agnostic) persist would leave the
						// ordered size, and the exit sweep's positions-unavailable fallback then
						// sells the full original quantity. This is guarded on status='pending', so
						// the downstream persistEntryFill for this winner is a harmless no-op.
						updated, dbErr := store.FillORBTradePartialByOrderID(ctx, s.db, entries[k].id, res.FilledPrice, int(res.FilledQty))
						if dbErr != nil {
							// Leave filled=false and retry next tick. Marking it filled now
							// would let the qty-agnostic downstream persist heal the row to the
							// FULL ordered size, re-introducing the over-sell this fix prevents.
							slog.Error("orb: persist option partial entry failed; will retry", "ticker", ticker, "order_id", entries[k].id, "err", dbErr)
							continue
						}
						if updated {
							s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
								OrderID: entries[k].id, Ticker: ticker, Side: entries[k].side, Status: "filled", FillPrice: res.FilledPrice,
							})
						}
						entries[k].filled = true
					}
				}
				if entries[k].filled {
					if entries[k].side == "buy" {
						buyFilled = true
					} else {
						sellFilled = true
					}
				}
			}
			if !buyFilled && !sellFilled {
				continue // no entry filled yet
			}

			doubleFill := buyFilled && sellFilled
			winning := "buy"
			if sellFilled && !buyFilled {
				winning = "sell"
			}
			if doubleFill {
				slog.Info("orb: both sides filled — persisting double fill", "ticker", ticker)
			}

			// allOpposingResolved tracks whether every order that must be cancelled this tick
			// actually was. A FILLED winner must NOT be cleared from tracking until then: if a
			// cancel fails and we forgot the winner, the next tick would see only pending
			// loser(s), hit the !buyFilled && !sellFilled guard, skip the ticker, and never
			// retry the cancel — leaving a live opposing stop that can fill into a reverse
			// position. Keeping the winner tracked makes the next tick re-enter and retry.
			allOpposingResolved := true
			for k := range entries {
				e := &entries[k]
				switch {
				case e.filled:
					// Persist now (idempotent); clearing is deferred until the opposing cancels
					// succeed (see below).
					s.persistEntryFill(ctx, ticker, e.side, e.id, e.res.FilledPrice)
				case !doubleFill && e.side == winning:
					// A not-yet-filled order on the winning side (e.g. a split-bracket partial
					// lagging its runner) — leave it for a later poll so it fills normally.
					// NOT in a double fill: there, both directions already triggered, so every
					// still-pending entry (on EITHER side) must be cancelled to avoid a lagging
					// partial firing on top and compounding the double position. (If such a
					// "pending" entry has in fact already filled at the broker, the cancel fails
					// and the recheck below persists it.)
				default:
					// Losing side (or any pending order in a double fill) — cancel. A cancel
					// failure may mean it just filled.
					slog.Info("orb: side filled — cancelling opposing", "ticker", ticker, "winning_side", winning, "cancel_side", e.side, "order_id", e.id)
					if err := CancelOrder(ctx, s.executor, e.id); err != nil {
						if res, rerr := s.executor.GetOrderStatus(ctx, e.id); rerr == nil && res.Status == "filled" {
							slog.Info("orb: opposing order also filled — double fill", "ticker", ticker, "side", e.side, "order_id", e.id, "fill_price", res.FilledPrice)
							s.persistEntryFill(ctx, ticker, e.side, e.id, res.FilledPrice)
							s.clearEntryID(ticker, e.id)
						} else {
							slog.Warn("orb: cancel failed, will retry next tick", "side", e.side, "ticker", ticker, "order_id", e.id, "err", err)
							allOpposingResolved = false
						}
						continue
					}
					if dbErr := store.CancelORBTradeByOrderID(ctx, s.db, e.id); dbErr != nil {
						slog.Error("orb: update cancelled trade failed", "side", e.side, "ticker", ticker, "err", dbErr)
					}
					s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
						OrderID: e.id, Ticker: ticker, Side: e.side, Status: "cancelled",
					})
					s.clearEntryID(ticker, e.id)
				}
			}
			// Clear filled winners only once every opposing order was cancelled. If a cancel
			// failed, keep them tracked so the next tick re-enters this block (the winner still
			// reports filled), persists idempotently, and retries the cancel.
			if allOpposingResolved {
				for k := range entries {
					if entries[k].filled {
						s.clearEntryID(ticker, entries[k].id)
					}
				}
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
				if dbErr := store.CloseORBTrade(ctx, s.db, tc.dbID, ec.exitType, "broker", res.FilledPrice, pnl); dbErr != nil {
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
		// Broker-truth guard (always on, forex/metals): record any held position
		// as a filled entry BEFORE the cancel loop, so a filled-but-unconfirmed
		// metals order (GetOrderStatus "order not found") is never cancelled into
		// the invisible-orphan trap. Filled entries are cleared from s.orderIDs
		// here, so reconcileOrCancel below only cancels genuinely-unfilled orders.
		s.reconcileHeldPositionsToFilled(ctx)

		if s.executor != nil {
			sides := [2]string{"buy", "sell"}
			// reconcileOrCancel applies the end-of-session reconcile-or-cancel to one
			// [buy,sell] order-ID pair. Run over BOTH the runner orders and the split-bracket
			// partial orders so a partial entry left working at the cutoff is reconciled or
			// cancelled too (scaleOutEntryIDs is empty when partial-TP is unconfigured).
			reconcileOrCancel := func(ticker string, ids [2]string) {
				for i, orderID := range ids {
					if orderID == "" {
						continue
					}
					// Reconcile already-filled orders instead of cancelling them. For OPTIONS
					// a PARTIAL fill counts too (res.FilledQty > 0): recording the filled
					// portion — and cancelling the unfilled remainder — keeps the held
					// contracts managed instead of orphaning them. The partial branch is
					// gated on isOption so equity/futures brackets keep main's lifecycle: a
					// partial there still falls through to 'cancelled' (their TP/SL legs were
					// placed for the full size, so overwriting qty would desync DB vs broker).
					tc := s.trackedTrades[orderID]
					isOption := tc != nil && tc.isOption
					if res, err := s.executor.GetOrderStatus(ctx, orderID); err == nil && (res.Status == "filled" || (isOption && res.FilledQty > 0)) {
						price := res.FilledPrice
						if tc != nil && !tc.isOption {
							if corrected, was := sanitizeEntryFill(sides[i], tc.entryStop, price); was {
								slog.Warn("orb: session cleanup: implausible entry fill — recording stop level instead",
									"side", sides[i], "ticker", ticker, "reported", price, "entry_stop", tc.entryStop, "recorded", corrected)
								price = corrected
							}
						}
						var updated bool
						var dbErr error
						if res.Status != "filled" {
							if cerr := CancelOrder(ctx, s.executor, orderID); cerr != nil {
								// The unfilled remainder is still LIVE as a day order and could
								// fill more contracts than the partial we'd record. Do NOT persist
								// the reduced qty — that would orphan the extra contracts under a
								// smaller row. Leave the row pending; the next cleanup pass retries
								// the cancel (mirrors the live-poll partial path at ~1611).
								slog.Warn("orb: session cleanup: cancel partial-fill remainder failed; leaving pending to retry",
									"ticker", ticker, "order_id", orderID, "err", cerr)
								continue
							}
							slog.Info("orb: session cleanup: partial entry fill — persisting filled portion", "side", sides[i], "ticker", ticker, "filled_qty", res.FilledQty, "fill_price", price)
							// Persist the actual filled contract count (see the live-poll path).
							updated, dbErr = store.FillORBTradePartialByOrderID(ctx, s.db, orderID, price, int(res.FilledQty))
						} else {
							slog.Info("orb: session cleanup: already filled — persisting", "side", sides[i], "ticker", ticker, "fill_price", price)
							updated, dbErr = store.FillORBTradeByOrderID(ctx, s.db, orderID, price)
						}
						if dbErr != nil {
							slog.Error("orb: update filled trade failed", "ticker", ticker, "err", dbErr)
						}
						if updated {
							s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
								OrderID: orderID, Ticker: ticker, Side: sides[i], Status: "filled", FillPrice: price,
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
			for ticker, ids := range s.orderIDs {
				reconcileOrCancel(ticker, ids)
			}
			for ticker, ids := range s.scaleOutEntryIDs {
				reconcileOrCancel(ticker, ids)
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
		s.scaleOutEntryIDs = make(map[string][2]string)
		s.splitPlaced = false
		s.directionDecisions = make(map[string][2]bool)
		s.releaseTrackedTradesForReconcile()
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

// persistEntryFill records a forex entry fill against its orb_trades row, idempotent on
// retry ticks via the updated flag. It guards an implausible broker-reported price with
// sanitizeEntryFill (a buy below / sell above its own stop, which IBKR can return when both
// bracket legs fill at once) and stamps entryFill on the trackedTrade so exit polling begins.
func (s *ORBScheduler) persistEntryFill(ctx context.Context, ticker, side, orderID string, price float64) {
	// Skip the fill sanitizer for options: entryStop is the underlying breakout
	// level and price is the option premium, so the stock-shaped check would
	// wrongly "correct" the premium to the underlying level (see trackedTrade.isOption).
	if tc := s.trackedTrades[orderID]; tc != nil && !tc.isOption {
		if corrected, was := sanitizeEntryFill(side, tc.entryStop, price); was {
			slog.Warn("orb: implausible entry fill from broker — recording stop level instead",
				"side", side, "ticker", ticker, "reported", price, "entry_stop", tc.entryStop, "recorded", corrected)
			price = corrected
		}
	}
	updated, dbErr := store.FillORBTradeByOrderID(ctx, s.db, orderID, price)
	if dbErr != nil {
		slog.Error("orb: update filled trade failed", "side", side, "ticker", ticker, "err", dbErr)
		return
	}
	if updated {
		s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
			OrderID: orderID, Ticker: ticker, Side: side, Status: "filled", FillPrice: price,
		})
		if tc := s.trackedTrades[orderID]; tc != nil {
			tc.entryFill = price
		}
	}
}

// clearEntryID removes a resolved entry order (filled or cancelled) from the runner and
// partial tracking maps, forgetting the ticker entirely once neither map holds a working
// entry for it. Single-order granularity (vs the old delete(s.orderIDs, ticker)) so a
// split-bracket ticker with a still-working sibling order is retained for later polling.
func (s *ORBScheduler) clearEntryID(ticker, orderID string) {
	for _, m := range []map[string][2]string{s.orderIDs, s.scaleOutEntryIDs} {
		ids, ok := m[ticker]
		if !ok {
			continue
		}
		for i := range ids {
			if ids[i] == orderID {
				ids[i] = ""
			}
		}
		if ids[0] == "" && ids[1] == "" {
			delete(m, ticker)
		} else {
			m[ticker] = ids
		}
	}
}

// releaseTrackedTradesForReconcile clears the in-memory exit-tracking map at the
// end of a session WITHOUT touching the DB. Filled trades are deliberately left in
// status='filled': their bracket TP/SL stays live at the broker after the session
// is marked done, so the real exit fills later and the DB-backed reconcilers — the
// trades-feed matcher and reconcileDoneSessionTrades, both keyed on status='filled'
// — close them then with the true exit price and PnL. Closing them here as 'expired'
// (pnl=0) would lie about the outcome and, because every reconciler and CloseORBTrade
// guard on status='filled', permanently block recovery while polluting realized-PnL
// feedback stats. This method takes no store handle by design, so it cannot regress
// into writing a premature terminal state. Caller must hold s.mu.
func (s *ORBScheduler) releaseTrackedTradesForReconcile() {
	for entryOrderID, tc := range s.trackedTrades {
		if tc.entryFill != 0 {
			slog.Info("orb: session cleanup: leaving filled trade open for exit reconciliation",
				"ticker", tc.ticker, "trade_id", tc.dbID)
		}
		delete(s.trackedTrades, entryOrderID)
	}
}

// paperExitCheckInterval throttles the paper-mode exit reconciler. Resolution replays
// 1-minute bars, so a once-per-minute sweep is as fine-grained as the data allows while
// keeping candle requests light. A real broker fills exit legs server-side, so the
// reconciler never runs for live executors.
const paperExitCheckInterval = time.Minute

// paperExitHorizon bounds how far forward FROM THE FILL the reconciler replays bars for
// a single trade. The window is always anchored at fill time (never at "now"), so a
// trade stuck for weeks is replayed over the bars right after it filled — not an
// unrelated recent week, which would fabricate a bogus exit. (This relies on the data
// provider honouring the window end: see IBKRClient.FetchCandles' startTime anchor —
// without it the endpoint only ever returns the most-recent period.) Forex ORB brackets
// are intraday and resolve within hours; a week is generous headroom while capping the
// cost of the first post-fix sweep and staying within providers' intraday-history
// limits. A trade whose bars never touch a leg within this window — or whose history the
// provider no longer retains (e.g. trades older than IBKR's 1-minute retention, which
// therefore cannot be backfilled) — is left 'filled' rather than force-closed, so we
// never invent an outcome.
const barReplayHorizon = 7 * 24 * time.Hour

// barReplayGracePeriod is how long after fill the bar-replay backstop waits before
// closing a trade, leaving the broker reconcilers (in-session exit-leg polling and the
// trades-feed matcher) the first chance to record a *confirmed* exit. It is longer than
// the 60-minute post-open session cleanup so a normal same-session exit is broker-sourced;
// bar replay only steps in for trades the broker path missed. Both close paths guard on
// status='filled', so even if both fire the first one wins — the grace period just biases
// which source that is.
const barReplayGracePeriod = 90 * time.Minute

// reconcileBarReplayExits records TP/stop-loss exits for filled-but-unclosed ORB trades
// by replaying their price path from market-data bars. It is the universal exit backstop:
// it runs for every executor (paper or live IBKR), independent of whether bracket child
// IDs were captured or whether IBKR's 7-day trades feed still retains the fill. Without
// it, a trade whose exit the broker reconcilers miss stays 'filled' forever and never
// records an outcome — the bug that left realized PnL and feedback stats blind to ORB.
//
// For each trade it replays bars over [fill, fill+horizon] (capped at now), at the finest
// resolution the provider still serves for that lookback (1-min when fresh, hourly/daily
// when old), and closes at the first leg the path touches — stamping closed_at with that
// bar's time. Replaying the path (vs sampling one live quote) records the leg that fired
// first and stays correct whether the trade is closed live, after a restart, or backfilled
// long after the fact. Anchoring the window at fill (never at now) keeps an old stuck trade
// replayed over its own post-fill bars rather than an unrelated recent window.
//
// Exits it records are tagged exit_source='replay' (a model of the exit from fixed-price
// bracket legs), distinct from broker-confirmed fills. CloseORBTradeAt guards on
// status='filled', so this is idempotent against the broker reconcilers; the grace period
// (see barReplayGracePeriod) lets those record a confirmed exit first when one is available.
func (s *ORBScheduler) reconcileBarReplayExits(ctx context.Context) {
	if s.db == nil || s.marketData == nil {
		return
	}
	if time.Since(s.lastPaperExitCheckAt) < paperExitCheckInterval {
		return
	}
	s.lastPaperExitCheckAt = s.now()

	trades, err := store.GetOpenFilledORBTrades(ctx, s.db)
	if err != nil {
		slog.Warn("orb: bar replay: load filled trades failed", "err", err)
		return
	}

	// Broker-truth guard: bar replay infers a stop/TP touch from market data, which can
	// fabricate a close on a position the broker still fully holds. Refuse any such close.
	guard := s.newExitGuard(ctx, trades)

	now := s.now()
	for i := range trades {
		t := trades[i]

		// A zero entry fill price (NULL fill_price → COALESCE 0) would make exitPnL
		// compute against a 0 entry, writing an order-of-magnitude-wrong pnl that the
		// status='filled' guard then freezes permanently. Skip — matches matchExitsFromRecords.
		if t.FillPrice == 0 {
			continue
		}

		// Anchor the replay window at the entry fill (fallback: row creation).
		from := t.CreatedAt
		if t.FilledAt != nil {
			from = *t.FilledAt
		}
		// Grace period: give the broker reconcilers first crack at a confirmed exit.
		if now.Sub(from) < barReplayGracePeriod {
			continue
		}
		to := from.Add(barReplayHorizon)
		if to.After(now) {
			to = now
		}
		if !to.After(from) {
			continue // window not open yet — retry next sweep
		}

		// Once adaptive stop management has moved the stop, t.StopLoss no longer reflects the
		// level that was live across the whole post-fill window — replaying it would fabricate
		// a stop-out at the moved level over bars the stop had not yet reached (see migration
		// 035). Disable the stop leg (0 ⇒ resolveExitFromCandles skips it) and replay only the
		// immutable take-profit; the stop side is owned by the live broker order and captured
		// by the trades-feed reconciler and the time-exit backstop.
		replayStop := t.StopLoss
		if t.StopMovedAt != nil {
			replayStop = 0
		}
		res, rErr := ResolveExitViaBarReplay(ctx, s.marketData, t.Ticker, t.Side, replayStop, t.TakeProfit, from, to, now)
		if rErr != nil {
			slog.Warn("orb: bar replay: fetch candles failed", "ticker", t.Ticker, "err", rErr)
			continue
		}
		if !res.Found {
			continue // still open — price path never reached a leg within the window
		}
		if !guard.allows(&t) {
			slog.Warn("orb: bar replay: broker still holds the position — deferring replayed close to avoid a phantom",
				"ticker", t.Ticker, "side", t.Side, "qty", t.Qty, "replay_exit", res.ExitType)
			continue
		}

		pnl := exitPnL(t.Side, t.FillPrice, res.ExitPrice, t.Qty)
		slog.Info("orb: bar replay: resolved exit",
			"ticker", t.Ticker, "side", t.Side, "exit_type", res.ExitType, "timeframe", res.Timeframe,
			"exit_price", res.ExitPrice, "entry_price", t.FillPrice, "exit_time", res.ExitTime, "pnl", pnl,
		)
		if dbErr := store.CloseORBTradeAt(ctx, s.db, t.ID, res.ExitType, "replay", res.ExitPrice, pnl, res.ExitTime); dbErr != nil {
			slog.Error("orb: bar replay: close trade failed", "ticker", t.Ticker, "err", dbErr)
			continue
		}
		pnlVal := pnl
		s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
			OrderID: t.OrderID, Ticker: t.Ticker, Side: t.Side,
			Status: "closed", FillPrice: t.FillPrice, PnL: &pnlVal,
		})
	}
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
			if dbErr := store.CloseORBTrade(ctx, s.db, t.ID, ec.exitType, "broker", res.FilledPrice, pnl); dbErr != nil {
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
	// Broker-truth guard: a trades-feed match near a bracket level can be a stale/unrelated
	// record that never reduced the position (today's XAG phantom stop on a still-held -850
	// short). Read positions once and refuse any close the broker hasn't actually made.
	guard := s.newExitGuard(ctx, trades)
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

			// Broker-truth gate: only book the close if the broker actually reduced. A match
			// that leaves the full position held is a phantom — skip it; a genuine exit
			// records on a later pass once the position read confirms the reduction.
			if !guard.allows(t) {
				slog.Warn("orb: exit reconcile: broker still holds the position — deferring matched close to avoid a phantom",
					"ticker", t.Ticker, "side", t.Side, "qty", t.Qty, "matched_exit", exitType, "matched_price", rec.Price)
				break
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
				if dbErr := store.CloseORBTrade(ctx, s.db, t.ID, exitType, "broker", rec.Price, pnl); dbErr != nil {
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
