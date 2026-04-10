package orb

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// newTestScheduler builds an ORBScheduler with no external deps — safe for unit tests.
func newTestScheduler() *ORBScheduler {
	return &ORBScheduler{
		cfg:          config.ORBConfig{},
		state:        StateIdle,
		rangeBuffers: make(map[string]*RangeBuffer),
		orderIDs:     make(map[string]string),
	}
}

var testFuturesCfg = &config.FuturesORBConfig{
	Enabled:       true,
	SessionOpenET: "09:30",
	RangeMinutes:  15,
	RiskReward:    2.0,
	MaxRiskPct:    1.0,
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
