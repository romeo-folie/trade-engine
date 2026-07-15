package orb

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
)

// newTestScheduler builds an ORBScheduler with no external deps — safe for unit tests.
func newTestScheduler() *ORBScheduler {
	return &ORBScheduler{
		cfg:          config.ORBConfig{},
		state:        StateIdle,
		rangeBuffers: make(map[string]*RangeBuffer),
		orderIDs:     make(map[string][2]string),
	}
}

var testFuturesCfg = &config.FuturesORBConfig{
	Enabled:        true,
	SessionOpenET:  "09:30",
	RangeMinutes:   15,
	RiskReward:     2.0,
	MaxRiskPct:     1.0,
	MaxPositionPct: 20.0,
	Instruments: []config.FuturesInstrument{
		{Symbol: "ES", Exchange: "CME", AssetClass: "equity_index", ExpiryCycle: "quarterly", TickSize: 0.25, TickValue: 12.50, TickBuffer: 2},
		{Symbol: "NQ", Exchange: "CME", AssetClass: "equity_index", ExpiryCycle: "quarterly", TickSize: 0.25, TickValue: 5.00, TickBuffer: 2},
	},
}

func TestSchedulerFuturesMode_IsFuturesMode(t *testing.T) {
	s := newTestScheduler()
	if s.isFuturesMode() {
		t.Error("should not be futures mode before SetFuturesConfig")
	}
	s.SetFuturesConfig(testFuturesCfg)
	if !s.isFuturesMode() {
		t.Error("should be futures mode after SetFuturesConfig with Enabled=true")
	}
}

func TestSchedulerFuturesMode_NotForexMode(t *testing.T) {
	s := newTestScheduler()
	s.SetFuturesConfig(testFuturesCfg)
	if s.isForexMode() {
		t.Error("futures mode must not report as forex mode")
	}
}

func TestSchedulerFuturesMode_SessionName(t *testing.T) {
	s := newTestScheduler()
	s.SetFuturesConfig(testFuturesCfg)
	if got := s.sessionName(); got != "futures" {
		t.Errorf("sessionName: want %q, got %q", "futures", got)
	}
}

func TestSchedulerFuturesMode_SessionNameDistinctFromForex(t *testing.T) {
	// Verify futures session name doesn't collide with known forex session names.
	forbiddenNames := []string{"london", "newyork", "both", ""}
	s := newTestScheduler()
	s.SetFuturesConfig(testFuturesCfg)
	name := s.sessionName()
	for _, bad := range forbiddenNames {
		if name == bad {
			t.Errorf("futures sessionName must not be %q to avoid DB collision with equities/forex", bad)
		}
	}
}

func TestSchedulerFuturesMode_SessionOpenMinutes_Default(t *testing.T) {
	// Default SessionOpenET "09:30" → 570 minutes.
	s := newTestScheduler()
	s.SetFuturesConfig(testFuturesCfg)
	got := s.sessionOpenMinutes()
	if got != 9*60+30 {
		t.Errorf("sessionOpenMinutes: want 570 (09:30), got %d", got)
	}
}

func TestSchedulerFuturesMode_SessionOpenMinutes_MetalsOpen(t *testing.T) {
	// Override to "08:00" simulates a metals ORB config.
	cfg := *testFuturesCfg
	cfg.SessionOpenET = "08:00"
	s := newTestScheduler()
	s.SetFuturesConfig(&cfg)
	got := s.sessionOpenMinutes()
	if got != 8*60 {
		t.Errorf("sessionOpenMinutes: want 480 (08:00), got %d", got)
	}
}

func TestSessionCleanupMinutes(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(*ORBScheduler)
		wantMin int // minute-of-day in session TZ
	}{
		{
			name: "equities (09:30 ET open → 10:30 ET cleanup)",
			setup: func(s *ORBScheduler) {
				// No forex/futures config → equities default.
			},
			wantMin: 10*60 + 30,
		},
		{
			name: "london forex (08:00 UTC open → 09:00 UTC cleanup)",
			setup: func(s *ORBScheduler) {
				s.SetForexConfig(&config.ForexORBConfig{Enabled: true, Session: "london", Timezone: "UTC"})
			},
			wantMin: 9 * 60,
		},
		{
			name: "newyork forex (13:30 UTC open → 14:30 UTC cleanup)",
			setup: func(s *ORBScheduler) {
				s.SetForexConfig(&config.ForexORBConfig{Enabled: true, Session: "newyork", Timezone: "UTC"})
			},
			wantMin: 14*60 + 30,
		},
		{
			name: "futures 09:30 ET open → 10:30 ET cleanup",
			setup: func(s *ORBScheduler) {
				s.SetFuturesConfig(testFuturesCfg) // SessionOpenET = "09:30"
			},
			wantMin: 10*60 + 30,
		},
		{
			name: "futures late-evening open (23:00) does not overflow 1439",
			setup: func(s *ORBScheduler) {
				cfg := *testFuturesCfg
				cfg.SessionOpenET = "23:00"
				s.SetFuturesConfig(&cfg)
			},
			wantMin: 0, // (23*60 + 60) % 1440 = 1440 % 1440 = 0
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestScheduler()
			tc.setup(s)
			got := s.sessionCleanupMinutes()
			if got != tc.wantMin {
				t.Errorf("sessionCleanupMinutes: want %d, got %d", tc.wantMin, got)
			}
		})
	}
}

func TestSchedulerFuturesMode_InstrumentLookup_Found(t *testing.T) {
	s := newTestScheduler()
	s.SetFuturesConfig(testFuturesCfg)

	inst := s.futuresInstrumentForTicker("ES")
	if inst == nil {
		t.Fatal("futuresInstrumentForTicker(ES): expected non-nil")
	}
	if inst.Symbol != "ES" {
		t.Errorf("Symbol: want ES, got %q", inst.Symbol)
	}
	if inst.TickSize != 0.25 {
		t.Errorf("TickSize: want 0.25, got %v", inst.TickSize)
	}
}

func TestSchedulerFuturesMode_InstrumentLookup_NotFound(t *testing.T) {
	s := newTestScheduler()
	s.SetFuturesConfig(testFuturesCfg)

	inst := s.futuresInstrumentForTicker("GC") // gold — not in test config
	if inst != nil {
		t.Errorf("futuresInstrumentForTicker(GC): expected nil, got %+v", inst)
	}
}

func TestSchedulerFuturesMode_InstrumentLookup_NilConfig(t *testing.T) {
	s := newTestScheduler()
	// futuresCfg is nil — must not panic.
	inst := s.futuresInstrumentForTicker("ES")
	if inst != nil {
		t.Errorf("expected nil when futuresCfg not set, got %+v", inst)
	}
}

func TestSchedulerFuturesMode_EquitiesSessionOpenUnchanged(t *testing.T) {
	// When neither forex nor futures is set, sessionOpenMinutes must be 9:30 ET.
	s := newTestScheduler()
	got := s.sessionOpenMinutes()
	if got != marketOpenMinutes {
		t.Errorf("equities default: want %d (09:30), got %d", marketOpenMinutes, got)
	}
}

func TestSchedulerFuturesMode_DisabledConfig_NotFuturesMode(t *testing.T) {
	disabled := &config.FuturesORBConfig{Enabled: false}
	s := newTestScheduler()
	s.SetFuturesConfig(disabled)
	if s.isFuturesMode() {
		t.Error("Enabled=false should not activate futures mode")
	}
}

// TestSchedulerFutures_EffectiveInstrument_Maps verifies futures resolve to a forex-shaped
// Instrument (so they route through the dual-sided/gated path) with derived buffer/point
// value and strategy fields carried across.
func TestSchedulerFutures_EffectiveInstrument_Maps(t *testing.T) {
	cfg := *testFuturesCfg
	cfg.Instruments = []config.FuturesInstrument{
		{Symbol: "MNQ", TickSize: 0.25, TickValue: 0.50, TickBuffer: 2, MarginRate: 0.10,
			BreakoutConfirmation: "close_3m", MaxHoldMinutes: 240, DirectionEligibility: true},
	}
	s := newTestScheduler()
	s.SetFuturesConfig(&cfg)

	inst := s.effectiveInstrument("MNQ")
	if inst == nil {
		t.Fatal("effectiveInstrument(MNQ) = nil; want mapped instrument")
	}
	if inst.SecType != "FUT" || inst.PipBuffer != 0.5 || inst.PointValue != 2.0 || inst.TickSize != 0.25 {
		t.Errorf("mapping wrong: SecType=%q PipBuffer=%v PointValue=%v TickSize=%v",
			inst.SecType, inst.PipBuffer, inst.PointValue, inst.TickSize)
	}
	if inst.BreakoutConfirmation != "close_3m" || inst.MaxHoldMinutes != 240 || !inst.DirectionEligibility {
		t.Errorf("strategy fields not carried: %+v", inst)
	}
	// A fresh mapped value, not shared state: mutating it must not affect the config.
	inst.PipBuffer = 999
	if again := s.effectiveInstrument("MNQ"); again.PipBuffer != 0.5 {
		t.Errorf("effectiveInstrument returned shared state: second call PipBuffer=%v", again.PipBuffer)
	}
	if s.effectiveInstrument("GC") != nil {
		t.Error("effectiveInstrument(GC) should be nil — not configured")
	}
}

// TestSchedulerFutures_EffectiveForexCfg_Maps verifies the futures strategy config maps onto
// the ForexORBConfig shape the shared bracket/gate functions consume.
func TestSchedulerFutures_EffectiveForexCfg_Maps(t *testing.T) {
	cfg := *testFuturesCfg
	cfg.RangeMinutes, cfg.RiskReward, cfg.MaxRiskPct = 30, 1.5, 0.5
	cfg.ATRPeriod, cfg.ATRTimeframe, cfg.TrendEMAPeriod = 14, "1h", 20
	s := newTestScheduler()
	s.SetFuturesConfig(&cfg)

	f := s.effectiveForexCfg()
	if f.RangeMinutes != 30 || f.RiskReward != 1.5 || f.MaxRiskPct != 0.5 ||
		f.ATRPeriod != 14 || f.ATRTimeframe != "1h" || f.TrendEMAPeriod != 20 {
		t.Errorf("effectiveForexCfg mismapped in futures mode: %+v", f)
	}
}

// TestSchedulerFutures_ExitBackstopScoping verifies the mode-agnostic helpers the exit-side
// backstops (time exit, adaptive stops, exit guard) use, so futures trades are covered.
func TestSchedulerFutures_ExitBackstopScoping(t *testing.T) {
	cfg := *testFuturesCfg
	cfg.MaxHoldMinutes = 120 // strategy-level default
	cfg.Instruments = []config.FuturesInstrument{
		{Symbol: "MNQ", TickSize: 0.25, TickValue: 0.50, TickBuffer: 2, MaxHoldMinutes: 240}, // per-contract override
		{Symbol: "MES", TickSize: 0.25, TickValue: 1.25, TickBuffer: 2},                      // inherits strategy default
	}
	s := newTestScheduler()
	s.SetFuturesConfig(&cfg)

	// ownsInstrument: futures symbols yes, others no.
	if !s.ownsInstrument("MNQ") || !s.ownsInstrument("MES") {
		t.Error("ownsInstrument should be true for configured futures")
	}
	if s.ownsInstrument("EUR/USD") || s.ownsInstrument("AAPL") {
		t.Error("ownsInstrument should be false for non-configured tickers")
	}

	// effectiveMaxHoldMinutes: per-contract override wins; else strategy default.
	if got := s.effectiveMaxHoldMinutes("MNQ"); got != 240 {
		t.Errorf("MNQ max-hold = %d; want 240 (per-contract override)", got)
	}
	if got := s.effectiveMaxHoldMinutes("MES"); got != 120 {
		t.Errorf("MES max-hold = %d; want 120 (strategy default — AsForexConfig drops Instruments)", got)
	}

	// instrumentSecType: futures flatten order must carry FUT.
	if got := s.instrumentSecType("MNQ"); got != "FUT" {
		t.Errorf("MNQ secType = %q; want FUT", got)
	}

	// ownedPositions: keeps this scheduler's futures positions (resolved to symbols by
	// reverseConid), drops others — so the exit guard counts them against the managed budget.
	positions := []execution.Position{
		{Ticker: "MNQ", Quantity: 2},
		{Ticker: "MES", Quantity: -1},
		{Ticker: "AAPL", Quantity: 100},    // equities scheduler's — must be dropped
		{Ticker: "747387490", Quantity: 3}, // an UNRESOLVED conid (belt-and-suspenders) — dropped
	}
	owned := s.ownedPositions(positions)
	if len(owned) != 2 {
		t.Fatalf("ownedPositions kept %d; want 2 (MNQ, MES)", len(owned))
	}
	for _, p := range owned {
		if p.Ticker != "MNQ" && p.Ticker != "MES" {
			t.Errorf("ownedPositions kept unexpected %q", p.Ticker)
		}
	}
}
