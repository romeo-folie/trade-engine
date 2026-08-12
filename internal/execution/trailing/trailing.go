// Package trailing implements a server-side trailing stop manager (#322).
//
// On each tick the manager iterates open long stock trades and applies one
// of two rules:
//
//   - atr: ratchet a broker stop using
//     new_stop = max(current_stop, high_water_mark − atr_multiple × ATR)
//   - ema9: retain the original broker hard stop and exit at market when the
//     latest daily candle closes below its daily 9-EMA
//
// where ATR is the absolute-price Average True Range (same units as
// high_water_mark) and atr_multiple is a unitless coefficient (default 1.5).
// The max clause guarantees the stop ratchets upward only — it never widens.
//
// On the first tick for a trade the high-water-mark column is NULL: the
// manager initialises it to MAX(entry_price, current_price) and persists it
// before computing the trail.
//
// Stop submission goes through the broker abstraction. When the broker
// implements StopOrderReplacer (Alpaca PATCH, IBKR modify) the manager
// replaces the existing stop in place; otherwise it falls back to
// cancel+resubmit. Failures are logged WARN and retried on the next tick;
// the manager never crashes on a single trade.
//
// Initial scope is stocks/long-only — forex/ORB exit logic is unchanged.
package trailing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const (
	RuleATR  = "atr"
	RuleEMA9 = "ema9"

	// DefaultATRMultiple is the unitless coefficient applied to absolute-price
	// ATR to derive the trailing band. 1.5 is the recommended default per the
	// task spec — small enough to lock in gains, wide enough to ride normal
	// volatility.
	DefaultATRMultiple = 1.5

	// DefaultIntervalSec is the manager's recompute cadence when the operator
	// hasn't set risk.trailing_stop.interval_sec.
	DefaultIntervalSec = 60

	// DefaultATRPeriod is Wilder's standard ATR lookback.
	DefaultATRPeriod = 14

	// EMAPeriod is the daily close EMA used by the evidence-backed exit-policy
	// study. It is deliberately fixed so "ema9" has one stable meaning across
	// backtest and live execution.
	EMAPeriod = 9

	// candleHistory is the number of 1h candles fetched per tick to compute
	// ATR. Wilder's ATR is path-dependent; period+1 is the strict minimum but
	// in practice 30+ stabilises the smoothing.
	candleHistory = 50
)

var errStopFilledDuringRollback = errors.New("stop filled before rollback completed")
var errExitClaimMustRemain = errors.New("partial broker exit is durable; exit claim must remain")

const maxTerminalStopGenerationSubmissions = 32

var newYorkLocation = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.UTC
	}
	return loc
}()

// ComputeNewStop applies the trailing-stop formula. The clamp guarantees the
// stop ratchets upward only — even if a transient ATR spike or stale price
// would loosen the trail, the returned value never falls below currentStop.
//
// Formula: max(currentStop, highWaterMark − atrMultiple × atr).
//
// All inputs are in absolute price units except atrMultiple which is unitless.
// Negative or zero atr/highWaterMark return currentStop unchanged so a single
// bad tick can't widen a live stop.
func ComputeNewStop(currentStop, highWaterMark, atr, atrMultiple float64) float64 {
	if atr <= 0 || highWaterMark <= 0 || atrMultiple <= 0 {
		return currentStop
	}
	candidate := highWaterMark - atrMultiple*atr
	if candidate > currentStop {
		return candidate
	}
	return currentStop
}

// TrailArmed reports whether the trailing rule has earned the right to manage
// this position's exit yet.
//
// Without a gate the trail is live from the first tick, so a position that
// ticks up slightly and pulls back is closed by the trail before it ever had a
// thesis — observed live across 12 ATR exits, six of which peaked under 5% and
// five of those exited flat or at a loss. One (CRDO) never traded above its
// entry at all and the trail still owned its exit, at -9.41%. The original
// protective stop is the correct owner for trades in that state.
//
// The gate keys on the HIGH-WATER MARK, not the current price, and that is what
// makes arming latch: hwm only ratchets up, so once a trade has earned the
// trail it keeps it even if price falls back. Gating on current price would arm
// and disarm on every oscillation across the threshold — a materially different
// and worse policy, since the trail would repeatedly forget the peak it is
// supposed to be protecting.
//
// activateGainPct <= 0 disables the gate, preserving the original behaviour.
// Callers must pass the ENTRY price, not the original stop: this threshold is a
// percentage move, deliberately distinct from the R-multiple gate the forex/ORB
// instrument config uses (Instrument.TrailAfterR).
func TrailArmed(entryPrice, highWaterMark, activateGainPct float64) bool {
	if activateGainPct <= 0 {
		return true
	}
	if entryPrice <= 0 || highWaterMark <= 0 {
		return false
	}
	return (highWaterMark-entryPrice)/entryPrice*100 >= activateGainPct
}

// PriceFetcher is the narrow data dependency the manager has on the market
// data layer. The package's data.MarketDataProvider satisfies this directly;
// tests inject in-memory fakes.
type PriceFetcher interface {
	FetchHistory(ctx context.Context, ticker string, tf data.Timeframe, limit int) ([]data.Candle, error)
}

// TradeStore is the narrow store surface the manager touches. Production
// uses the package-level store helpers; tests inject a fake.
type TradeStore interface {
	GetOpenTrades(ctx context.Context) ([]store.Trade, error)
	UpdateTradeTrailing(ctx context.Context, id string, stopLoss, highWaterMark float64) error
	LatestStopLossOrderForTrade(ctx context.Context, tradeID string) (*store.Order, error)
	// ExitLegsForMarketExit returns one DB snapshot containing both working
	// protective legs and terminal legs with fills. It closes the lookup gap
	// where a leg can fill between separate working/filled queries.
	ExitLegsForMarketExit(ctx context.Context, tradeID string) ([]store.Order, error)
	ClaimTradeExitWithIntent(ctx context.Context, id string, intent store.ExitIntent) (bool, error)
	ReleaseTradeExitClaim(ctx context.Context, id string) error
	InsertOrder(ctx context.Context, o store.Order) (string, error)
	UpdateOrderFill(ctx context.Context, orderID string, filledPrice, filledQty float64, status string) error
	ApplyPartialExitAt(ctx context.Context, tradeID, orderID string, filledPrice, cumulativeFilledQty float64, status string, filledAt time.Time) (remaining, applied float64, err error)
	UpdateExitingTradeQuantity(ctx context.Context, id string, quantity float64) error
	UpdateOrderStopReplacement(ctx context.Context, orderID, expectedBrokerOrderID, newBrokerOrderID string, expectedFilledQty, expectedTradeQuantity, newStopPrice, newQuantity float64, status string, filledPrice, filledQty float64, filledAt time.Time, intent store.ExitIntent) error
	// CloseTradeRealizedWithIntent closes the trade after a confirmed market
	// exit and returns lifecycle-cumulative P&L across earlier partial exits.
	CloseTradeRealizedWithIntent(
		ctx context.Context,
		id string,
		exitPrice float64,
		filledAt time.Time,
		intent store.ExitIntent,
	) (store.TradeRealization, error)
}

// Config controls the trailing-stop manager.
type Config struct {
	Enabled     bool
	Rule        string
	ATRMultiple float64
	Interval    time.Duration
	ATRPeriod   int
	// ActivateAtGainPct withholds the trail until the position's peak gain from
	// entry reaches this percentage. 0 disables the gate (trail live from the
	// first tick). Applies to the ATR rule only — see TrailArmed and updateOne.
	ActivateAtGainPct float64
	// MarketOpen is the production market-calendar predicate. The command
	// wiring supplies engine.IsMarketOpen (including exchange holidays);
	// tests and other callers fall back to regular weekday session hours.
	MarketOpen func(time.Time) bool
	// RecordLoss updates engine-level cooldown state after a trailing exit
	// realizes a loss. It is optional for standalone/tests callers.
	RecordLoss func()
	// OnTradeClosed publishes the same lifecycle event as engine-owned exit
	// paths after a trailing-policy close commits successfully.
	OnTradeClosed func(events.TradeClosedPayload)
}

// Normalize fills in defaults so callers can hand the manager a partially
// configured Config and still get sensible behaviour.
func (c Config) Normalize() Config {
	c.Rule = strings.ToLower(strings.TrimSpace(c.Rule))
	if c.Rule == "" {
		c.Rule = RuleATR
	}
	if c.ATRMultiple <= 0 {
		c.ATRMultiple = DefaultATRMultiple
	}
	if c.Interval <= 0 {
		c.Interval = time.Duration(DefaultIntervalSec) * time.Second
	}
	if c.ATRPeriod <= 0 {
		c.ATRPeriod = DefaultATRPeriod
	}
	return c
}

// Manager runs the trailing stop loop.
type Manager struct {
	cfg         Config
	store       TradeStore
	prices      PriceFetcher
	exec        execution.Executor
	now         func() time.Time
	marketOpen  func(time.Time) bool
	deferredEMA map[string]time.Time
	// deferredATRExit dedupes the "trail crossed price but the market is
	// closed" log so an overnight defer doesn't emit one line per tick.
	deferredATRExit map[string]atrExitDeferral
}

type trailingObservation struct {
	Timestamp time.Time
	Price     float64
	ATR       float64
}

type atrExitDeferral struct {
	Observation  trailingObservation
	RequireFresh bool
}

func (o trailingObservation) advancedFrom(previous trailingObservation) bool {
	return o.Timestamp.After(previous.Timestamp) ||
		math.Abs(o.Price-previous.Price) > 1e-9 ||
		math.Abs(o.ATR-previous.ATR) > 1e-9
}

// NewManager constructs a trailing-stop Manager.
func NewManager(cfg Config, ts TradeStore, prices PriceFetcher, exec execution.Executor) *Manager {
	normalized := cfg.Normalize()
	marketOpen := normalized.MarketOpen
	if marketOpen == nil {
		marketOpen = regularStockSession
	}
	return &Manager{
		cfg:             normalized,
		store:           ts,
		prices:          prices,
		exec:            exec,
		now:             time.Now,
		marketOpen:      marketOpen,
		deferredEMA:     make(map[string]time.Time),
		deferredATRExit: make(map[string]atrExitDeferral),
	}
}

// Run drives UpdateAll on the configured interval. Blocks until ctx is
// cancelled. Disabled (Config.Enabled=false) is a fast-return.
func (m *Manager) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		logger.Info("trailing-stop manager disabled (risk.trailing_stop.enabled=false)")
		return
	}
	logger.Info("trailing-stop manager started",
		"interval", m.cfg.Interval,
		"rule", m.cfg.Rule,
		"atr_multiple", m.cfg.ATRMultiple,
		"activate_at_gain_pct", m.cfg.ActivateAtGainPct,
		"atr_period", m.cfg.ATRPeriod,
	)
	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()
	// Run immediately so the first ratchet doesn't wait a full interval. A
	// crossed ATR band discovered by this startup sweep is re-validated on the
	// first normal tick before it is allowed to submit a market exit. Broker
	// protection remains live while that one-tick deferral is in effect.
	m.updateAll(ctx, true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.UpdateAll(ctx)
		}
	}
}

// UpdateAll iterates all open long stock trades and walks each trailing stop
// up. Failures on a single trade are logged WARN and skipped so a bad row
// never poisons the whole tick.
func (m *Manager) UpdateAll(ctx context.Context) {
	m.updateAll(ctx, false)
}

// updateAll performs one manager sweep. startupSweep is deliberately private
// and passed by Run only for its immediate first pass: keeping it out of
// Manager state means the very next interval tick necessarily resumes normal
// crossed-band behaviour, without a restart flag that can become stale.
func (m *Manager) updateAll(ctx context.Context, startupSweep bool) {
	trades, err := m.store.GetOpenTrades(ctx)
	if err != nil {
		logger.Warn("trailing-stop: GetOpenTrades failed", "err", err)
		return
	}
	active := make(map[string]bool, len(trades))
	for _, t := range trades {
		if !isLongStockTrade(t) {
			continue
		}
		active[t.ID] = true
		if err := m.updateOne(ctx, t, startupSweep); err != nil {
			logger.Warn("trailing-stop: update failed",
				"trade_id", t.ID,
				"ticker", t.Ticker,
				"err", err,
			)
		}
	}
	for tradeID := range m.deferredEMA {
		if !active[tradeID] {
			delete(m.deferredEMA, tradeID)
		}
	}
	for tradeID := range m.deferredATRExit {
		if !active[tradeID] {
			delete(m.deferredATRExit, tradeID)
		}
	}
}

// isLongStockTrade returns true only for trades the manager should touch.
// Forex / ORB / metals strategies have their own bracket-managed exits and
// must not be modified here.
func isLongStockTrade(t store.Trade) bool {
	if t.Status != "open" {
		return false
	}
	// Engine persists `direction` as "long"/"short" (see executeSignal in
	// internal/engine/engine.go). The original "buy"/"sell" guard silently
	// skipped every real long trade in production — tests passed because the
	// fixtures matched the broken value. Use the production value.
	if t.Direction != "long" {
		return false
	}
	strat := strings.ToLower(t.Strategy)
	if strings.Contains(strat, "orb") || strings.Contains(strat, "forex") {
		return false
	}
	return true
}

// updateOne does a full trailing-stop tick for a single trade: fetches a
// recent price + ATR, ratchets the high-water-mark, computes a new stop, and
// places or replaces the broker STOP order.
func (m *Manager) updateOne(ctx context.Context, t store.Trade, startupSweep bool) error {
	if m.cfg.Rule == RuleEMA9 {
		return m.updateOneEMA9(ctx, t)
	}
	if t.Quantity <= 0 {
		return fmt.Errorf("non-positive quantity %f", t.Quantity)
	}
	observation, err := m.priceAndATR(ctx, t.Ticker)
	if err != nil {
		return fmt.Errorf("price/atr: %w", err)
	}
	currentPrice, atr := observation.Price, observation.ATR
	if currentPrice <= 0 {
		return errors.New("non-positive current price")
	}

	// First-tick HWM seed: max(entry_price, current_price). Subsequent ticks
	// only ratchet the HWM upward when price prints a fresh high.
	hwm := t.HighWaterMark
	if hwm <= 0 {
		hwm = math.Max(t.EntryPrice, currentPrice)
	} else if currentPrice > hwm {
		hwm = currentPrice
	}

	// Withhold the trail until the position has earned it. Everything below
	// this point either ratchets the broker stop or exits at market, and both
	// are premature for a trade that has not yet run.
	//
	// Unlike the transient `newStop <= 0` case further down, an un-armed trade
	// can stay in this state for its whole life, so the protective stop must be
	// confirmed present rather than assumed — an uncapped trade whose entry
	// bracket leg never landed would otherwise sit naked until it gained 5%.
	// Capped trades stay app-managed: a standalone stop beside their software
	// take-profit creates two exit owners and can double-sell.
	if !TrailArmed(t.EntryPrice, hwm, m.cfg.ActivateAtGainPct) {
		// Persist the peak FIRST, before anything that can fail. This branch runs
		// every tick for the whole disarmed life of a trade, so a transient broker
		// error below must not discard a high that already printed: losing it
		// un-arms a trade that had already earned the trail, permanently, and
		// recomputes the eventual band from a peak lower than the market made.
		// 0 leaves stop_loss untouched (see UpdateTradeTrailing).
		if hwm != t.HighWaterMark {
			if err := m.store.UpdateTradeTrailing(ctx, t.ID, 0, hwm); err != nil {
				return fmt.Errorf("persist high-water mark while disarmed: %w", err)
			}
		}
		if t.TakeProfit <= 0 && t.StopLoss > 0 {
			if err := m.ensureProtectiveStop(ctx, t); err != nil {
				return fmt.Errorf("protective stop while trail disarmed: %w", err)
			}
		}
		return nil
	}

	// `current_stop` is the most recent persisted stop_loss. The max clamp
	// in ComputeNewStop guarantees the new stop is never lower than this,
	// so the stop never widens (and never sinks below the original
	// stop_loss recorded at trade open, which is the floor on the first
	// tick).
	newStop := ComputeNewStop(t.StopLoss, hwm, atr, m.cfg.ATRMultiple)

	// Round to broker tick size before persistence + comparison so tiny
	// floating-point noise (e.g. 48.50000001 vs 48.5) doesn't trigger a
	// pointless REPLACE API call every tick. Round BOTH sides: t.StopLoss
	// is scanned from a NUMERIC column and binary roundtrip can leave it
	// as 48.509999… for what the broker stored as 48.51 — comparing the
	// rounded candidate against the unrounded persisted value would then
	// fire a no-op REPLACE on every tick.
	dp := brokerTickDecimals(t.Ticker)
	newStop = roundToTick(newStop, dp)
	persistedStop := roundToTick(t.StopLoss, dp)

	if newStop <= 0 {
		// A missing original stop plus unavailable/zero ATR cannot produce a
		// valid broker order. Preserve the HWM and retry when price history can
		// derive a positive stop instead of submitting stop_price=0 every tick.
		if hwm != t.HighWaterMark {
			return m.store.UpdateTradeTrailing(ctx, t.ID, 0, hwm)
		}
		return nil
	}

	// If the trail says we should already be out (stop ≥ current price)
	// the broker stop would fill instantly anyway. Don't leave a stale stop
	// sitting at the old price — exit at market immediately so the band's
	// implied "we're done" actually plays out.
	if newStop >= currentPrice {
		marketOpen := m.marketOpen(m.now())
		deferred, wasDeferred := m.deferredATRExit[t.ID]
		waitingForFresh := wasDeferred && deferred.RequireFresh && !observation.advancedFrom(deferred.Observation)
		if startupSweep || !marketOpen || waitingForFresh {
			// Same hazard the EMA9 rule guards against: a market sell
			// submitted outside regular hours is a DAY order that sits queued
			// until the next open, then fills on a decision derived from the
			// prior session's stale close and never re-validated. The resting
			// broker stop stays live meanwhile — if price really is through
			// the band it fills on the open, which is the protection the trail
			// is asking for — and this condition is re-evaluated on the first
			// tick of the next eligible session.
			//
			// Only the market exit is gated — deferring must never leave the
			// position naked. The trail candidate itself is unusable here (a
			// stop at or above the market would fill or be rejected on sight),
			// so an uncapped trade missing its leg is seeded at the original
			// hard stop instead, exactly as the EMA9 rule does. Capped trades
			// stay app-managed: a standalone stop beside their software
			// take-profit creates two exit owners and can double-sell.
			// Record the deferral before any fallible persistence or broker work.
			// Otherwise a transient startup error can return without RequireFresh,
			// allowing the next ordinary tick to exit from the same observation.
			if !wasDeferred {
				message := "trailing-stop: trail crossed price outside regular session; deferring market exit"
				if startupSweep {
					message = "trailing-stop: trail crossed price during startup sweep; deferring market exit until next tick"
				}
				logger.Info(message,
					"trade_id", t.ID,
					"ticker", t.Ticker,
					"trailing_stop", newStop,
					"current_price", currentPrice,
				)
				m.deferredATRExit[t.ID] = atrExitDeferral{Observation: observation, RequireFresh: startupSweep}
			}

			// Persist the peak before the fallible seed, for the same reason as
			// the disarmed branch above: a broker hiccup must never cost a high
			// that already printed.
			if hwm != t.HighWaterMark {
				if err := m.store.UpdateTradeTrailing(ctx, t.ID, 0, hwm); err != nil {
					return fmt.Errorf("persist high-water mark before deferral: %w", err)
				}
				t.HighWaterMark = hwm
			}
			if t.TakeProfit <= 0 && t.StopLoss > 0 {
				if err := m.ensureProtectiveStop(ctx, t); err != nil {
					return fmt.Errorf("seed protective stop before deferral: %w", err)
				}
			}
			return nil
		}
		delete(m.deferredATRExit, t.ID)
		logger.Info("trailing-stop: trail crossed price; exiting at market",
			"trade_id", t.ID,
			"ticker", t.Ticker,
			"new_stop", newStop,
			"current_price", currentPrice,
		)
		intent := store.ExitIntent{
			Reason: store.ExitReasonTrailingATR,
			Evidence: exitEvidence(map[string]any{
				"current_price": currentPrice, "trailing_stop": newStop,
				"high_water_mark": hwm, "atr": atr,
				"atr_multiple": m.cfg.ATRMultiple,
			}),
		}
		if err := m.exitAndClose(ctx, t, currentPrice, intent, true); err != nil {
			// Persist HWM so the next tick has fresh baseline; surface error.
			_ = m.store.UpdateTradeTrailing(ctx, t.ID, 0, hwm)
			return fmt.Errorf("market exit: %w", err)
		}
		// Persist HWM after CloseTrade so the trail snapshot is intact for
		// post-mortem queries.
		return m.store.UpdateTradeTrailing(ctx, t.ID, 0, hwm)
	}

	// The trail sits below price again, so any earlier deferral is resolved.
	delete(m.deferredATRExit, t.ID)

	// Submit/replace the broker STOP whenever it moved. Also seed a missing
	// broker stop even when the ATR candidate still equals the original hard
	// stop; uncapped entries are deliberately not brackets, so their first
	// manager tick must establish protection immediately.
	existingStop, err := m.store.LatestStopLossOrderForTrade(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("lookup protective stop: %w", err)
	}
	needsStopUpdate := existingStop != nil &&
		(newStop != persistedStop || stopQuantityDiffers(existingStop, t.Quantity))
	needsUncappedSeed := existingStop == nil && t.TakeProfit <= 0
	if needsStopUpdate || needsUncappedSeed {
		intent := stopExitIntent(newStop)
		if newStop > persistedStop {
			intent = store.ExitIntent{Reason: store.ExitReasonTrailingATR, Evidence: exitEvidence(map[string]any{
				"current_price": currentPrice, "trailing_stop": newStop,
				"high_water_mark": hwm, "atr": atr, "atr_multiple": m.cfg.ATRMultiple,
			})}
		} else if existingStop != nil && existingStop.ExitReason != "" {
			intent = store.ExitIntent{Reason: existingStop.ExitReason, Evidence: existingStop.ExitEvidence}
		}
		if err := m.submitOrReplaceStop(ctx, t, newStop, intent); err != nil {
			// Persist HWM even on broker failure so the next tick has the
			// updated baseline; log WARN and retry next tick.
			_ = m.store.UpdateTradeTrailing(ctx, t.ID, 0, hwm)
			return fmt.Errorf("broker stop submit/replace: %w", err)
		}
	}

	// If neither trailing state nor the stop price changed, the only work on
	// this tick was seeding a previously-missing broker stop.
	if newStop == persistedStop && hwm == t.HighWaterMark {
		return nil
	}

	if err := m.store.UpdateTradeTrailing(ctx, t.ID, newStop, hwm); err != nil {
		return fmt.Errorf("persist trailing: %w", err)
	}

	logger.Info("trailing-stop updated",
		"trade_id", t.ID,
		"ticker", t.Ticker,
		"old_stop", t.StopLoss,
		"new_stop", newStop,
		"hwm", hwm,
		"atr", atr,
		"current_price", currentPrice,
	)
	return nil
}

// updateOneEMA9 implements the study's exact EMA rule: preserve the original
// hard stop at the broker, then exit when the latest daily close is below the
// daily 9-EMA. It intentionally does not move the broker stop to the EMA;
// doing so would create an intraday touch exit and would not match the tested
// close-confirmed policy.
func (m *Manager) updateOneEMA9(ctx context.Context, t store.Trade) error {
	if t.Quantity <= 0 {
		return fmt.Errorf("non-positive quantity %f", t.Quantity)
	}
	if t.StopLoss <= 0 {
		return errors.New("ema9 rule requires a positive protective stop")
	}

	// Uncapped EMA trades have no software take-profit, so their hard stop
	// must live at the broker even when market data is unavailable. Capped
	// trades remain entirely app-managed here: adding a standalone stop
	// alongside their software take-profit creates two independent exit
	// owners and can double-sell when the broker stop fills.
	if t.TakeProfit <= 0 {
		if err := m.ensureProtectiveStop(ctx, t); err != nil {
			return err
		}
	}

	now := m.now()
	closePrice, ema, closeCompletedAt, err := m.priceAndEMA9(ctx, t.Ticker, now)
	if err != nil {
		return fmt.Errorf("price/ema9: %w", err)
	}
	if closePrice <= 0 || ema <= 0 {
		return errors.New("non-positive close or ema9")
	}

	if closePrice < ema {
		if !t.OpenedAt.IsZero() && !closeCompletedAt.IsZero() &&
			!closeCompletedAt.After(t.OpenedAt) {
			// The close signal existed before this position did. Acting on it
			// would churn a brand-new BUY on the manager's first tick.
			return nil
		}
		if !m.marketOpen(now) {
			// A market sell submitted after 16:00 ET can sit queued until the
			// next open. Keep the trade row and broker hard stop live overnight,
			// then retry this same completed-close signal during the next
			// eligible session.
			if previous, ok := m.deferredEMA[t.ID]; !ok || !previous.Equal(closeCompletedAt) {
				logger.Info("trailing-stop: daily close below ema9; deferring market exit until regular session",
					"trade_id", t.ID,
					"ticker", t.Ticker,
					"daily_close", closePrice,
					"ema9", ema,
				)
				m.deferredEMA[t.ID] = closeCompletedAt
			}
			return nil
		}
		delete(m.deferredEMA, t.ID)
		logger.Info("trailing-stop: daily close below ema9; exiting at market",
			"trade_id", t.ID,
			"ticker", t.Ticker,
			"daily_close", closePrice,
			"ema9", ema,
		)
		intent := store.ExitIntent{
			Reason: store.ExitReasonEMA9Close,
			Evidence: exitEvidence(map[string]any{
				"daily_close": closePrice, "ema9": ema,
				"signal_candle_closed_at": closeCompletedAt.UTC(),
				"observed_at":             now.UTC(),
			}),
		}
		return m.exitAndClose(ctx, t, closePrice, intent, false)
	}
	delete(m.deferredEMA, t.ID)

	// For uncapped trades, the original hard stop seeded above remains resident
	// at the broker while the EMA winning exit waits.
	return nil
}

func (m *Manager) ensureProtectiveStop(ctx context.Context, t store.Trade) error {
	existing, err := m.store.LatestStopLossOrderForTrade(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("lookup protective stop: %w", err)
	}
	if existing == nil || stopQuantityDiffers(existing, t.Quantity) {
		if err := m.submitOrReplaceStop(ctx, t, t.StopLoss, stopExitIntent(t.StopLoss)); err != nil {
			return fmt.Errorf("seed/resize protective stop: %w", err)
		}
	}
	return nil
}

func stopQuantityDiffers(existing *store.Order, quantity float64) bool {
	return existing != nil && math.Abs(existing.Quantity-quantity) > 1e-9
}

// priceAndATR fetches recent 1h candles and returns the latest observation.
// ATR honours the configured `risk.trailing_stop.atr_period` so the operator
// knob actually flows through to the trailing band.
func (m *Manager) priceAndATR(ctx context.Context, ticker string) (trailingObservation, error) {
	if m.prices == nil {
		return trailingObservation{}, errors.New("no price provider configured")
	}
	candles, err := m.prices.FetchHistory(ctx, ticker, data.Timeframe1Hour, candleHistory)
	if err != nil {
		return trailingObservation{}, err
	}
	// This path reads the provider directly and never touches the DB, so the
	// UpsertCandles guard does not cover it (issue #519). A corrupt bar here
	// feeds ATR and the high-water mark, and can therefore place a resting
	// stop above the real market — closing a healthy position on bad data.
	candles = sanitizeForTrailing(ticker, "1h", candles)
	if len(candles) == 0 {
		return trailingObservation{}, errors.New("no candles returned")
	}
	atrVal := indicators.ComputeATR(candles, m.cfg.ATRPeriod)
	latest := candles[len(candles)-1]
	return trailingObservation{Timestamp: latest.Timestamp, Price: latest.Close, ATR: atrVal}, nil
}

// sanitizeForTrailing drops corrupt bars before they reach ATR/EMA/high-water
// computation and logs each rejection. Kept separate from the fetch so both
// rules share one guard and one log shape.
func sanitizeForTrailing(ticker, tf string, candles []data.Candle) []data.Candle {
	clean, rejected := data.SanitizeCandles(candles)
	for _, r := range rejected {
		logger.Warn("trailing: rejected corrupt candle",
			"ticker", ticker,
			"timeframe", tf,
			"ts", r.Candle.Timestamp.UTC().Format(time.RFC3339),
			"reason", r.Reason,
			"detail", r.Detail,
		)
	}
	return clean
}

// priceAndEMA9 fetches daily candles so the live rule uses the same timeframe
// as the exit-policy study. The provider's latest returned daily bar is the
// close evidence used by the rule.
func (m *Manager) priceAndEMA9(ctx context.Context, ticker string, now time.Time) (float64, float64, time.Time, error) {
	if m.prices == nil {
		return 0, 0, time.Time{}, errors.New("no price provider configured")
	}
	candles, err := m.prices.FetchHistory(ctx, ticker, data.Timeframe1Day, candleHistory)
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	candles = sanitizeForTrailing(ticker, "1d", candles)
	candles = completedDailyCandles(candles, now)
	if len(candles) < EMAPeriod {
		return 0, 0, time.Time{}, fmt.Errorf("need at least %d daily candles, got %d", EMAPeriod, len(candles))
	}
	ema := emaClose(candles, EMAPeriod)
	last := candles[len(candles)-1]
	return last.Close, ema, dailyCloseCompletedAt(last), nil
}

// dailyCloseCompletedAt converts a daily bar's session date into the regular
// US equity close instant. Provider timestamps identify the daily session's
// start; comparing that raw timestamp with Trade.OpenedAt would incorrectly
// reject a same-day close that completed after a morning entry.
func dailyCloseCompletedAt(candle data.Candle) time.Time {
	if candle.Timestamp.IsZero() {
		return time.Time{}
	}
	session := candle.Timestamp.In(newYorkLocation)
	return time.Date(session.Year(), session.Month(), session.Day(), 16, 0, 0, 0, newYorkLocation)
}

// regularStockSession is deliberately conservative: EMA exits are eligible
// only during the normal US equity session. Outside this window the existing
// broker stop remains live and the manager retries on its next tick.
func regularStockSession(now time.Time) bool {
	nowNY := now.In(newYorkLocation)
	if nowNY.Weekday() == time.Saturday || nowNY.Weekday() == time.Sunday {
		return false
	}
	minuteOfDay := nowNY.Hour()*60 + nowNY.Minute()
	const (
		openMinute  = 9*60 + 30
		closeMinute = 16 * 60
	)
	return minuteOfDay >= openMinute && minuteOfDay < closeMinute
}

// completedDailyCandles excludes today's still-forming US session bar before
// the regular 16:00 New York close. Some providers expose a partial 1Day bar
// during the session; treating it as a close would turn the tested close-based
// rule into an intraday touch rule. Zero timestamps are retained for providers
// and deterministic fixtures that do not timestamp history.
func completedDailyCandles(candles []data.Candle, now time.Time) []data.Candle {
	if len(candles) == 0 {
		return candles
	}
	last := candles[len(candles)-1]
	if last.Timestamp.IsZero() {
		return candles
	}
	nowNY := now.In(newYorkLocation)
	barNY := last.Timestamp.In(newYorkLocation)
	sameSessionDate := nowNY.Year() == barNY.Year() &&
		nowNY.YearDay() == barNY.YearDay()
	beforeRegularClose := nowNY.Hour() < 16
	if sameSessionDate && beforeRegularClose {
		return candles[:len(candles)-1]
	}
	return candles
}

// emaClose returns the final EMA value for candle closes, seeded with the
// simple average of the first period values (the same convention used by the
// backtest harness).
func emaClose(candles []data.Candle, period int) float64 {
	if period <= 0 || len(candles) < period {
		return 0
	}
	var ema float64
	for i := 0; i < period; i++ {
		ema += candles[i].Close
	}
	ema /= float64(period)
	k := 2.0 / float64(period+1)
	for i := period; i < len(candles); i++ {
		ema = candles[i].Close*k + ema*(1-k)
	}
	return ema
}

// submitOrReplaceStop sends the new stop price to the broker. If a broker
// stop already exists it is replaced (PATCH on Alpaca / modify on IBKR) when
// the executor implements StopOrderReplacer. Otherwise the manager cancels
// the prior stop and submits a fresh STOP order — but only after the cancel
// is confirmed. The orders table is updated to reflect the live broker id
// and stop price.
//
// Safety: if the fallback cancel fails (broker timeout, etc.) we DO NOT
// place a new stop — that would leave two live STOP orders on the same long
// and a later price drop could over-exit / flip the account short. Instead
// we surface the error and rely on the next tick to retry.
func (m *Manager) submitOrReplaceStop(ctx context.Context, t store.Trade, newStop float64, intent store.ExitIntent) error {
	existing, err := m.store.LatestStopLossOrderForTrade(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("lookup existing stop: %w", err)
	}

	side := execution.OrderSideSell // long-only scope: exit a long via SELL
	if existing != nil && existing.BrokerOrderID != "" {
		quantityChanged := stopQuantityDiffers(existing, t.Quantity)
		// Replace path — atomic on the broker side, no double-stop window.
		if replacer, ok := m.exec.(execution.StopOrderReplacer); ok && !quantityChanged {
			res, err := replacer.ReplaceStopOrder(ctx, existing.BrokerOrderID, newStop)
			if err == nil {
				status := execution.NormalizeOrderStatus(res.Status)
				fillQty := res.FilledQty
				if status == "filled" && fillQty <= 0 {
					fillQty = existing.Quantity
				}
				fillPrice := res.FilledPrice
				if fillQty > 0 && fillPrice <= 0 {
					fillPrice = newStop
				}
				fillObserved := fillQty > 0
				if fillObserved {
					// Transfer ownership before terminalizing the only durable
					// order row. A crash after this point leaves an orderless
					// exiting trade, which the recovery sweep can safely resolve.
					claimed, claimErr := m.store.ClaimTradeExitWithIntent(ctx, t.ID, intent)
					if claimErr != nil {
						return fmt.Errorf("claim replacement-stop fill: %w", claimErr)
					}
					if !claimed {
						logger.Warn("trailing-stop: replacement fill joined an existing exit owner",
							"trade_id", t.ID, "broker_order_id", res.BrokerOrderID)
					}
				}
				if persistErr := m.store.UpdateOrderStopReplacement(
					ctx, existing.ID, existing.BrokerOrderID, res.BrokerOrderID, existing.FilledQty, t.Quantity, newStop, existing.Quantity,
					status, fillPrice, fillQty, res.FilledAt, intent,
				); persistErr != nil {
					if rollbackErr := m.rollbackUnrecordedStop(ctx, t, newStop, res.BrokerOrderID, "", intent); rollbackErr != nil {
						return fmt.Errorf("persist replaced stop: %w; rollback: %v", persistErr, rollbackErr)
					}
					return fmt.Errorf("persist replaced stop: %w (broker replacement cancelled)", persistErr)
				}
				if linker, ok := m.store.(interface {
					LinkReplacementOrderIntent(context.Context, string, string) error
				}); ok {
					if err := linker.LinkReplacementOrderIntent(ctx, existing.ID, res.BrokerOrderID); err != nil {
						return fmt.Errorf("link replacement stop intent: %w", err)
					}
				}
				if fillObserved {
					return errStopFilledDuringRollback
				}
				if status == "cancelled" || status == "rejected" {
					return fmt.Errorf("replacement stop became %s before providing coverage", status)
				}
				return nil
			}
			if errors.Is(err, execution.ErrAmbiguousStopReplacement) {
				// The coordinator retained a durable, unresolved replacement. The
				// old broker id may already be gone because that first PATCH won,
				// so cancel+resubmit here risks a second live stop. Recovery owns
				// the client-id lookup and will project the winning broker order.
				return fmt.Errorf("ambiguous stop replacement; refusing cancel+resubmit fallback: %w", err)
			}
			if existing.ParentOrderID != "" {
				// Bracket child leg (#435): cancel+resubmit is FORBIDDEN —
				// cancelling one OCO leg kills or orphans the sibling
				// take-profit, and a standalone replacement stop would leave
				// the position half-managed. Surface the error and let the
				// next sweep retry the atomic replace.
				return fmt.Errorf("replace of bracket stop leg failed (no cancel+resubmit fallback for OCO children): %w", err)
			}
			logger.Warn("trailing-stop: replace failed, falling back to cancel+resubmit",
				"trade_id", t.ID,
				"err", err,
			)
		} else if existing.ParentOrderID != "" {
			return fmt.Errorf("executor cannot replace stops in place; refusing cancel+resubmit on a bracket stop leg")
		}
		// Fallback: cancel, CONFIRM cancellation, then resubmit. If we
		// can't confirm the prior stop is gone, bail out — placing a new
		// stop now risks two live stops on the same position.
		if err := m.cancelAndConfirm(ctx, existing.BrokerOrderID); err != nil {
			return fmt.Errorf("cancel of stale stop unconfirmed; refusing to submit duplicate stop: %w", err)
		}
	}

	stopReq := execution.OrderRequest{
		Ticker:      t.Ticker,
		Side:        side,
		Type:        execution.OrderTypeStop,
		Quantity:    t.Quantity,
		StopPrice:   newStop,
		TradeID:     t.ID,
		TimeInForce: protectiveStopTIF(t.Quantity),
		IntentKey:   trailingStopIntentKey(t.ID, existing, newStop, t.Quantity),
	}
	var res execution.OrderResult
	for generation := 0; generation < maxTerminalStopGenerationSubmissions; generation++ {
		res, err = m.exec.PlaceOrder(ctx, stopReq)
		status := execution.NormalizeOrderStatus(res.Status)
		if err == nil && !isTerminalZeroFill(status, res.FilledQty) {
			break
		}
		if err != nil && !errors.Is(err, execution.ErrTerminalZeroFillPlacement) {
			break
		}
		retryKey, resolveErr := m.resolveTerminalStopIntentKey(ctx, stopReq.IntentKey)
		if resolveErr != nil {
			return fmt.Errorf("resolve terminal trailing stop generation: %w", resolveErr)
		}
		if retryKey == stopReq.IntentKey {
			return errors.New("terminal trailing stop generation resolver did not advance")
		}
		stopReq.IntentKey = retryKey
		stopReq.ClientOrderID = ""
		if generation == maxTerminalStopGenerationSubmissions-1 {
			return fmt.Errorf("trailing stop remained terminal through %d generations", maxTerminalStopGenerationSubmissions)
		}
	}
	if err != nil {
		return fmt.Errorf("place stop: %w", err)
	}
	status := execution.NormalizeOrderStatus(res.Status)
	fillQty := res.FilledQty
	if status == "filled" && fillQty <= 0 {
		fillQty = t.Quantity
	}
	fillPrice := res.FilledPrice
	if fillQty > 0 && fillPrice <= 0 {
		fillPrice = newStop
	}
	fillObserved := fillQty > 0
	if fillObserved {
		if _, claimErr := m.store.ClaimTradeExitWithIntent(ctx, t.ID, intent); claimErr != nil {
			return fmt.Errorf("claim immediate stop fill: %w", claimErr)
		}
	}

	if existing != nil {
		// Repoint the existing row at the freshly-submitted broker id.
		if err := m.store.UpdateOrderStopReplacement(
			ctx, existing.ID, existing.BrokerOrderID, res.BrokerOrderID, existing.FilledQty, t.Quantity, newStop, t.Quantity,
			status, fillPrice, fillQty, res.FilledAt, intent,
		); err != nil {
			if rollbackErr := m.rollbackUnrecordedStop(ctx, t, newStop, res.BrokerOrderID, "", intent); rollbackErr != nil {
				return fmt.Errorf("persist replacement stop: %w; rollback: %v", err, rollbackErr)
			}
			return fmt.Errorf("persist replacement stop: %w (broker stop cancelled)", err)
		}
		if linker, ok := m.store.(interface {
			LinkReplacementOrderIntent(context.Context, string, string) error
		}); ok {
			if err := linker.LinkReplacementOrderIntent(ctx, existing.ID, res.BrokerOrderID); err != nil {
				return fmt.Errorf("link fallback replacement intent: %w", err)
			}
		}
		if fillObserved {
			return errStopFilledDuringRollback
		}
		if status == "cancelled" || status == "rejected" {
			return fmt.Errorf("replacement stop became %s before providing coverage", status)
		}
		return nil
	}
	stockIntentID, err := m.stockIntentIDForAcceptedOrder(ctx, stopReq.IntentKey)
	if err != nil {
		return fmt.Errorf("resolve accepted trailing stop intent: %w", err)
	}
	_, err = m.store.InsertOrder(ctx, store.Order{
		TradeID:       t.ID,
		BrokerOrderID: res.BrokerOrderID,
		Ticker:        t.Ticker,
		Side:          string(side),
		OrderType:     string(execution.OrderTypeStop),
		Quantity:      t.Quantity,
		StopPrice:     newStop,
		FilledPrice:   fillPrice,
		FilledQty:     fillQty,
		Status:        status,
		IsStopLoss:    true,
		ExitReason:    intent.Reason,
		ExitEvidence:  intent.Evidence,
		FilledAt:      timePtr(res.FilledAt),
		StockIntentID: stockIntentID,
	})
	if err != nil {
		if rollbackErr := m.rollbackUnrecordedStop(ctx, t, newStop, res.BrokerOrderID, stockIntentID, intent); rollbackErr != nil {
			return fmt.Errorf("persist new stop: %w; rollback: %v", err, rollbackErr)
		}
		return fmt.Errorf("persist new stop: %w (broker stop cancelled)", err)
	}
	if fillObserved {
		return errStopFilledDuringRollback
	}
	if status == "cancelled" || status == "rejected" {
		return fmt.Errorf("new stop became %s before providing coverage", status)
	}
	return nil
}

func (m *Manager) resolveTerminalStopIntentKey(ctx context.Context, key string) (string, error) {
	durable, ok := m.exec.(interface{ DurableStockOrderIntents() bool })
	if !ok || !durable.DurableStockOrderIntents() {
		return "", errors.New("terminal intent retry requires durable executor")
	}
	lookup, ok := m.store.(interface {
		ResolveStockOrderIntentRetryGeneration(context.Context, string) (string, error)
	})
	if !ok {
		return "", errors.New("durable trailing executor has no terminal intent generation resolver")
	}
	retryKey, err := lookup.ResolveStockOrderIntentRetryGeneration(ctx, key)
	if err != nil {
		return "", err
	}
	if retryKey == "" {
		return "", errors.New("terminal trailing intent has no retry generation key")
	}
	return retryKey, nil
}

// stockIntentIDForAcceptedOrder makes trailing projections obey the durable
// coordinator boundary. Paper/legacy executors intentionally remain
// intent-free, while a coordinator must have a readable immutable intent
// before this manager writes any projection row.
func (m *Manager) stockIntentIDForAcceptedOrder(ctx context.Context, key string) (string, error) {
	durable, ok := m.exec.(interface{ DurableStockOrderIntents() bool })
	if !ok || !durable.DurableStockOrderIntents() {
		return "", nil
	}
	lookup, ok := m.store.(interface {
		StockIntentIDForKey(context.Context, string) (string, error)
	})
	if !ok {
		return "", errors.New("durable trailing executor has no intent lookup")
	}
	intentID, err := lookup.StockIntentIDForKey(ctx, key)
	if err != nil {
		return "", err
	}
	if intentID == "" {
		return "", errors.New("accepted trailing order durable intent missing")
	}
	return intentID, nil
}

// trailingStopIntentKey names the one semantic replacement operation. The
// predecessor broker id (when present), price and protected quantity mean a
// replay is exact while a later ratchet gets its own durable intent.
func trailingStopIntentKey(tradeID string, existing *store.Order, stopPrice, quantity float64) string {
	predecessor := "new"
	if existing != nil && existing.BrokerOrderID != "" {
		predecessor = existing.BrokerOrderID
	}
	return fmt.Sprintf("trailing-stop:%s:%s:%.8f:%.8f", tradeID, predecessor, stopPrice, quantity)
}

func trailingStopRetryIntentKey(previousKey, cancelledBrokerOrderID string) string {
	return store.StockOrderIntentRetryKey(previousKey, cancelledBrokerOrderID)
}

func isTerminalZeroFill(status string, filledQty float64) bool {
	return filledQty <= 0 && (status == "cancelled" || status == "rejected")
}

func (m *Manager) rollbackUnrecordedStop(
	ctx context.Context,
	t store.Trade,
	stopPrice float64,
	brokerOrderID string,
	stockIntentID string,
	intent store.ExitIntent,
) error {
	cancelErr := m.exec.CancelOrder(ctx, brokerOrderID)
	status, statusErr := m.exec.GetOrderStatus(ctx, brokerOrderID)
	if statusErr != nil {
		if errors.Is(statusErr, execution.ErrOrderNotFound) {
			return nil
		}
		return fmt.Errorf("cancel=%v status=%w", cancelErr, statusErr)
	}
	terminalStatus := execution.NormalizeOrderStatus(status.Status)
	if durable, ok := m.exec.(interface{ DurableStockOrderIntents() bool }); ok && durable.DurableStockOrderIntents() && stockIntentID == "" {
		// The broker fill remains recoverable from its immutable intent. Do
		// not turn this exceptional rollback path into an unlinked legacy row.
		return errors.New("rollback stop durable intent unavailable")
	}
	switch terminalStatus {
	case "cancelled", "rejected":
		if status.FilledQty <= 0 {
			return nil
		}
		_, _ = m.store.ClaimTradeExitWithIntent(ctx, t.ID, intent)
		if _, err := m.store.InsertOrder(ctx, store.Order{
			TradeID: t.ID, BrokerOrderID: brokerOrderID,
			Ticker: t.Ticker, Side: string(execution.OrderSideSell),
			OrderType: string(execution.OrderTypeStop), Quantity: t.Quantity,
			StopPrice: stopPrice, Status: terminalStatus, IsStopLoss: true,
			FilledPrice: status.FilledPrice, FilledQty: status.FilledQty,
			FilledAt:   timePtr(status.FilledAt),
			ExitReason: intent.Reason, ExitEvidence: intent.Evidence,
			StockIntentID: stockIntentID,
		}); err != nil {
			return fmt.Errorf("%w; persist partial rollback stop: %v", errStopFilledDuringRollback, err)
		}
		return errStopFilledDuringRollback
	case "filled":
		_, _ = m.store.ClaimTradeExitWithIntent(ctx, t.ID, intent)
		if _, err := m.store.InsertOrder(ctx, store.Order{
			TradeID: t.ID, BrokerOrderID: brokerOrderID,
			Ticker: t.Ticker, Side: string(execution.OrderSideSell),
			OrderType: string(execution.OrderTypeStop), Quantity: t.Quantity,
			StopPrice: stopPrice, Status: terminalStatus, IsStopLoss: true,
			FilledPrice: status.FilledPrice, FilledQty: status.FilledQty,
			FilledAt:   timePtr(status.FilledAt),
			ExitReason: intent.Reason, ExitEvidence: intent.Evidence,
			StockIntentID: stockIntentID,
		}); err != nil {
			return fmt.Errorf("%w; persist filled rollback stop: %v", errStopFilledDuringRollback, err)
		}
		return errStopFilledDuringRollback
	default:
		return fmt.Errorf("cancel=%v order remains %s", cancelErr, status.Status)
	}
}

func protectiveStopTIF(quantity float64) string {
	if quantity != math.Trunc(quantity) {
		// Alpaca accepts fractional equity stop/stop-limit orders only as DAY.
		return "day"
	}
	return "gtc"
}

// exitAndClose submits the market exit and closes the trade only after a full
// broker-confirmed fill. Asynchronous fills remain durably exiting for the
// independent reconciler, preventing duplicate sells and trigger-price P&L.
func (m *Manager) exitAndClose(ctx context.Context, t store.Trade, referencePrice float64, intent store.ExitIntent, isStopLoss bool) error {
	claimed, err := m.store.ClaimTradeExitWithIntent(ctx, t.ID, intent)
	if err != nil {
		return fmt.Errorf("claim trade exit: %w", err)
	}
	if !claimed {
		return nil
	}
	releaseClaim := true
	defer func() {
		if releaseClaim {
			_ = m.store.ReleaseTradeExitClaim(context.Background(), t.ID)
		}
	}()

	exitRes, exitQty, intentKey, err := m.exitAtMarket(ctx, t, referencePrice)
	if err != nil {
		if errors.Is(err, errExitClaimMustRemain) {
			releaseClaim = false
		}
		return err
	}
	t.Quantity = exitQty
	// Once the broker accepts the sell, keep the durable exit claim even if
	// persistence fails. Releasing it would let another worker submit a second
	// full sell on its next stale/open read.
	releaseClaim = false
	stockIntentID, err := m.stockIntentIDForAcceptedOrder(ctx, intentKey)
	if err != nil {
		return fmt.Errorf("resolve accepted trailing market-exit intent: %w", err)
	}
	if _, err := m.store.InsertOrder(ctx, store.Order{
		TradeID:       t.ID,
		BrokerOrderID: exitRes.BrokerOrderID,
		Ticker:        t.Ticker,
		Side:          string(execution.OrderSideSell),
		OrderType:     string(execution.OrderTypeMarket),
		Quantity:      t.Quantity,
		FilledPrice:   exitRes.FilledPrice,
		FilledQty:     exitRes.FilledQty,
		Status:        execution.NormalizeOrderStatus(exitRes.Status),
		IsStopLoss:    isStopLoss,
		ExitReason:    intent.Reason,
		ExitEvidence:  intent.Evidence,
		FilledAt:      timePtr(exitRes.FilledAt),
		StockIntentID: stockIntentID,
	}); err != nil {
		return fmt.Errorf("record accepted market exit: %w", err)
	}

	status := execution.NormalizeOrderStatus(exitRes.Status)
	if (status == "cancelled" || status == "rejected") && exitRes.FilledQty == 0 {
		releaseClaim = true
		return nil
	}
	fullFill := status == "filled" &&
		exitRes.FilledPrice > 0 && exitRes.FilledQty >= t.Quantity-1e-9
	if !fullFill {
		// The engine exit reconciler owns this durable `exiting` trade until
		// the broker reports a complete fill. Partial/pending positions remain
		// tracked and cannot be selected by another exit worker.
		return nil
	}
	exitPrice := exitRes.FilledPrice
	realized, err := m.store.CloseTradeRealizedWithIntent(ctx, t.ID, exitPrice, exitRes.FilledAt, intent)
	if err != nil {
		return fmt.Errorf("close trade after market exit: %w", err)
	}
	if realized.PnL < 0 && m.cfg.RecordLoss != nil {
		m.cfg.RecordLoss()
	}
	if m.cfg.OnTradeClosed != nil && realized.JustClosed {
		m.cfg.OnTradeClosed(events.TradeClosedPayload{
			TradeID: realized.TradeID, Ticker: realized.Ticker,
			Direction: realized.Direction, EntryPrice: realized.EntryPrice,
			ExitPrice: realized.ExitPrice, PnL: realized.PnL,
			PnLPct: realized.PnLPct, Reason: realized.ExitReason,
			Evidence: realized.ExitEvidence, FilledAt: realized.ExitFilledAt,
		})
	}
	return nil
}

func exitEvidence(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func stopExitIntent(stopPrice float64) store.ExitIntent {
	return store.ExitIntent{
		Reason:   store.ExitReasonStopLoss,
		Evidence: exitEvidence(map[string]any{"stop_loss": stopPrice}),
	}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

func timeValue(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// cancelAndConfirm cancels a broker order and only returns nil once the
// broker reports the order in a terminal cancelled/filled state. If the
// cancel call itself errors we re-query the broker to learn the real
// status — the order may already be gone (race) or still live (timeout).
//
// This is the load-bearing safety check that prevents double-stop
// submission during the cancel+resubmit fallback path. Never short-circuit
// or relax this without re-doing the duplicate-stop analysis.
func (m *Manager) cancelAndConfirm(ctx context.Context, brokerOrderID string) error {
	cancelErr := m.exec.CancelOrder(ctx, brokerOrderID)
	// Independent of cancel success, query status: a 404 / "order not found"
	// is fine (it's gone), a "filled" / "cancelled" is fine (terminal).
	status, statusErr := m.exec.GetOrderStatus(ctx, brokerOrderID)
	if statusErr != nil {
		// Broker says the order doesn't exist — that IS the terminal
		// state we wanted. Treat as a confirmed cancel so resubmission
		// can proceed instead of stalling on the "we don't know"
		// branch. This is what the comment above promised but the
		// original code missed.
		if errors.Is(statusErr, execution.ErrOrderNotFound) {
			return nil
		}
		// Couldn't even confirm. If cancel succeeded the broker likely
		// purged the order; if cancel also failed we have no idea.
		if cancelErr != nil {
			return fmt.Errorf("cancel + status both failed: cancel=%v status=%v", cancelErr, statusErr)
		}
		// Cancel said OK but we can't verify — treat as unconfirmed and
		// refuse the duplicate-submission risk.
		return fmt.Errorf("cancel reported success but status check failed: %w", statusErr)
	}
	switch execution.NormalizeOrderStatus(status.Status) {
	case "cancelled", "rejected":
		return nil
	case "filled":
		return errors.New("stop filled while cancellation was in flight; refusing replacement")
	}
	if cancelErr != nil {
		return fmt.Errorf("cancel failed and order still %s: %w", status.Status, cancelErr)
	}
	return fmt.Errorf("cancel returned no error but order still %s; will retry next tick", status.Status)
}

// exitAtMarket flattens a long position immediately. Used when the trailing
// band has crossed the current price and a stop order would fill instantly
// anyway — submitting a market sell guarantees the exit prints rather than
// leaving a stale stop sitting at the old level.
//
// Returns the broker's OrderResult so the caller can use the actual
// FilledPrice/FilledQty for trade close + pnl. Mirrors the field set
// engine.monitorPositions persists on its non-trailing exit path.
func (m *Manager) exitAtMarket(ctx context.Context, t store.Trade, referencePrice float64) (execution.OrderResult, float64, string, error) {
	// Cancel every working exit leg first — not just the latest stop. For
	// bracket-managed trades the stop AND take-profit legs each hold the
	// trade's shares at the broker; a market sell with either leg still
	// working is rejected for insufficient free quantity. More importantly,
	// submitting while a stop remains live can double-sell. Fail closed and
	// retry on the next manager tick unless every leg is confirmed gone.
	exitQty, err := m.cancelExitLegsForMarketExit(ctx, t)
	if err != nil {
		return execution.OrderResult{}, 0, "", err
	}
	req := execution.OrderRequest{
		Ticker:         t.Ticker,
		Side:           execution.OrderSideSell,
		Type:           execution.OrderTypeMarket,
		Quantity:       exitQty,
		ReferencePrice: referencePrice,
		TradeID:        t.ID,
		IntentKey:      fmt.Sprintf("trailing-market-exit:%s:%.8f", t.ID, exitQty),
	}
	durable, ok := m.exec.(interface{ DurableStockOrderIntents() bool })
	if !ok || !durable.DurableStockOrderIntents() {
		res, err := m.exec.PlaceOrder(ctx, req)
		if err != nil {
			return execution.OrderResult{}, 0, "", fmt.Errorf(
				"%w: ambiguous market-sell placement: %v", errExitClaimMustRemain, err,
			)
		}
		return res, exitQty, req.IntentKey, nil
	}
	for generation := 0; generation < maxTerminalStopGenerationSubmissions; generation++ {
		res, err := m.exec.PlaceOrder(ctx, req)
		terminalZero := err == nil && isTerminalZeroFill(res.Status, res.FilledQty)
		if err == nil && !terminalZero {
			return res, exitQty, req.IntentKey, nil
		}
		if err != nil && !errors.Is(err, execution.ErrTerminalZeroFillPlacement) {
			return execution.OrderResult{}, 0, "", fmt.Errorf(
				"%w: ambiguous market-sell placement: %v", errExitClaimMustRemain, err,
			)
		}
		retryKey, resolveErr := m.resolveTerminalStopIntentKey(ctx, req.IntentKey)
		if resolveErr != nil {
			return execution.OrderResult{}, 0, "", fmt.Errorf("resolve terminal trailing market-exit generation: %w", resolveErr)
		}
		if retryKey == req.IntentKey {
			return execution.OrderResult{}, 0, "", errors.New("terminal trailing market-exit generation resolver did not advance")
		}
		req.IntentKey = retryKey
		req.ClientOrderID = ""
		if generation == maxTerminalStopGenerationSubmissions-1 {
			return execution.OrderResult{}, 0, "", fmt.Errorf("trailing market exit remained terminal through %d generations", maxTerminalStopGenerationSubmissions)
		}
	}
	return execution.OrderResult{}, 0, "", errors.New("unreachable terminal trailing market-exit generation state")
}

func (m *Manager) cancelExitLegsForMarketExit(ctx context.Context, t store.Trade) (float64, error) {
	legs, err := m.store.ExitLegsForMarketExit(ctx, t.ID)
	if err != nil {
		return 0, fmt.Errorf("lookup exit legs before market exit: %w", err)
	}
	remaining := t.Quantity
	for _, leg := range legs {
		if leg.Status != "filled" && leg.Status != "cancelled" && leg.Status != "rejected" {
			continue
		}
		if leg.BrokerOrderID == "" {
			continue
		}
		// The DB row was only marked filled by a broker-status path. Confirm
		// the broker still agrees before standing down; any disagreement is
		// unknown state and must remain fail-closed rather than risk a second
		// sell.
		status, statusErr := m.exec.GetOrderStatus(ctx, leg.BrokerOrderID)
		if statusErr != nil {
			if errors.Is(statusErr, execution.ErrOrderNotFound) {
				// Brokers may purge terminal orders after recording a fill. A
				// durable cancelled/rejected partial fill is still authoritative;
				// apply it from the DB row so the residual market exit is not
				// stranded. A filled row without a durable quantity remains
				// ambiguous and must fail closed.
				terminalStatus := execution.NormalizeOrderStatus(leg.Status)
				if terminalStatus == "filled" && leg.FilledQty <= 0 {
					return 0, fmt.Errorf("%w: purged filled exit leg %s has no durable fill quantity",
						errExitClaimMustRemain, leg.BrokerOrderID)
				}
				if leg.FilledQty > 0 {
					fillPrice := leg.FilledPrice
					if fillPrice <= 0 {
						if leg.IsTakeProfit {
							fillPrice = leg.LimitPrice
						} else {
							fillPrice = leg.StopPrice
						}
					}
					if fillPrice <= 0 {
						return 0, fmt.Errorf("%w: purged exit leg %s has a fill but no price",
							errExitClaimMustRemain, leg.BrokerOrderID)
					}
					remaining, _, err = m.store.ApplyPartialExitAt(
						ctx, t.ID, leg.ID, fillPrice, leg.FilledQty, terminalStatus, timeValue(leg.FilledAt),
					)
					if err != nil {
						return 0, fmt.Errorf("%w: persist purged exit-leg fill: %v",
							errExitClaimMustRemain, err)
					}
					if remaining <= 0 {
						return 0, fmt.Errorf("%w: purged exit leg %s filled the full position",
							errExitClaimMustRemain, leg.BrokerOrderID)
					}
				}
				continue
			}
			return 0, fmt.Errorf(
				"%w: cannot verify filled exit leg %s: %v",
				errExitClaimMustRemain, leg.BrokerOrderID, statusErr,
			)
		}
		brokerStatus := execution.NormalizeOrderStatus(status.Status)
		if brokerStatus != "filled" && brokerStatus != "cancelled" && brokerStatus != "rejected" {
			return 0, fmt.Errorf(
				"%w: terminal exit leg %s has non-terminal broker status %s",
				errExitClaimMustRemain, leg.BrokerOrderID, status.Status,
			)
		}
		fillQty := status.FilledQty
		if fillQty <= 0 {
			fillQty = leg.FilledQty
		}
		if fillQty > 0 {
			fillPrice := status.FilledPrice
			if fillPrice <= 0 {
				fillPrice = leg.FilledPrice
			}
			if fillPrice <= 0 {
				if leg.IsTakeProfit {
					fillPrice = leg.LimitPrice
				} else {
					fillPrice = leg.StopPrice
				}
			}
			if fillPrice <= 0 {
				return 0, fmt.Errorf("%w: exit leg %s has a fill but no price",
					errExitClaimMustRemain, leg.BrokerOrderID)
			}
			remaining, _, err = m.store.ApplyPartialExitAt(
				ctx, t.ID, leg.ID, fillPrice, fillQty, brokerStatus, status.FilledAt,
			)
			if err != nil {
				return 0, fmt.Errorf("%w: persist recorded exit-leg fill: %v",
					errExitClaimMustRemain, err)
			}
			if remaining <= 0 {
				return 0, fmt.Errorf("%w: exit leg %s filled the full position",
					errExitClaimMustRemain, leg.BrokerOrderID)
			}
			// A terminal protective leg filled only part of the position. Its
			// contribution is durable; continue with the residual quantity.
			continue
		}
		return 0, fmt.Errorf(
			"%w: exit leg %s is terminal without a fill (broker status %s)",
			errExitClaimMustRemain, leg.BrokerOrderID, status.Status,
		)
	}

	for _, leg := range legs {
		if leg.BrokerOrderID == "" {
			continue
		}
		if leg.Status == "filled" || leg.Status == "cancelled" || leg.Status == "rejected" {
			continue
		}
		cancelErr := m.exec.CancelOrder(ctx, leg.BrokerOrderID)
		status, statusErr := m.exec.GetOrderStatus(ctx, leg.BrokerOrderID)
		if statusErr != nil {
			if errors.Is(statusErr, execution.ErrOrderNotFound) {
				return 0, fmt.Errorf(
					"%w: working exit leg %s disappeared before cancellation was confirmed",
					errExitClaimMustRemain, leg.BrokerOrderID,
				)
			}
			return 0, fmt.Errorf("confirm cancellation of exit leg %s: cancel=%v status=%w",
				leg.BrokerOrderID, cancelErr, statusErr)
		}
		switch execution.NormalizeOrderStatus(status.Status) {
		case "cancelled", "rejected":
			if status.FilledQty > 0 {
				fillPrice := status.FilledPrice
				if fillPrice <= 0 {
					if leg.IsTakeProfit {
						fillPrice = leg.LimitPrice
					} else {
						fillPrice = leg.StopPrice
					}
				}
				var applied float64
				remaining, applied, err = m.store.ApplyPartialExitAt(
					ctx, t.ID, leg.ID, fillPrice, status.FilledQty,
					execution.NormalizeOrderStatus(status.Status), status.FilledAt,
				)
				if err != nil {
					return 0, fmt.Errorf("%w: persist partial exit-leg fill: %v",
						errExitClaimMustRemain, err)
				}
				_ = applied
			} else if err := m.store.UpdateOrderFill(
				ctx, leg.ID, status.FilledPrice, 0,
				execution.NormalizeOrderStatus(status.Status),
			); err != nil {
				return 0, fmt.Errorf("persist terminal exit-leg status: %w", err)
			}
		case "filled":
			return 0, fmt.Errorf(
				"%w: exit leg %s filled while cancelling; refusing duplicate market exit",
				errExitClaimMustRemain, leg.BrokerOrderID,
			)
		default:
			return 0, fmt.Errorf("exit leg %s remains %s after cancel: %v",
				leg.BrokerOrderID, status.Status, cancelErr)
		}
	}
	if remaining <= 0 {
		return 0, fmt.Errorf("%w: exit legs sold the full position while cancelling",
			errExitClaimMustRemain)
	}
	return remaining, nil
}

// brokerTickDecimals returns the price-tick precision for the ticker. US
// equities round to $0.01 on both Alpaca and IBKR; forex pairs (which the
// scope guard already excludes from this manager) would need 4–5. We default
// to 2 decimals which matches the long-stocks scope.
func brokerTickDecimals(_ string) int {
	return 2
}

// roundToTick rounds price to `decimals` places using bankers' rounding via
// math.Round. The trailing-stop comparison uses the rounded value so tiny
// float noise (e.g. 48.5000001 vs 48.5) doesn't churn REPLACE calls.
func roundToTick(price float64, decimals int) float64 {
	if decimals < 0 {
		decimals = 0
	}
	scale := math.Pow(10, float64(decimals))
	return math.Round(price*scale) / scale
}
