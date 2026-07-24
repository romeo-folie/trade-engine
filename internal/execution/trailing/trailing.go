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
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
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
	// OpenExitLegsForTrade returns the trade's still-working stop/take-profit
	// legs. Non-empty ParentOrderID marks OCO-linked bracket children, which
	// hold the trade's shares — ALL of them must be cancelled before a market
	// exit or the broker rejects the sell for insufficient free quantity.
	OpenExitLegsForTrade(ctx context.Context, tradeID string) ([]store.Order, error)
	InsertOrder(ctx context.Context, o store.Order) (string, error)
	UpdateOrderStopPriceAndID(ctx context.Context, orderID, newBrokerOrderID string, newStopPrice float64) error
	// CloseTrade flips the trade row to status='closed' with exit_price + pnl
	// + closed_at. Called after a successful trail-driven market exit so the
	// trade isn't picked up again on the next tick (which would place a
	// duplicate market sell). Mirrors what engine.monitorPositions does on
	// a stop/take-profit fill.
	CloseTrade(ctx context.Context, id string, exitPrice, pnl, pnlPct float64) error
}

// Config controls the trailing-stop manager.
type Config struct {
	Enabled     bool
	Rule        string
	ATRMultiple float64
	Interval    time.Duration
	ATRPeriod   int
	// MarketOpen is the production market-calendar predicate. The command
	// wiring supplies engine.IsMarketOpen (including exchange holidays);
	// tests and other callers fall back to regular weekday session hours.
	MarketOpen func(time.Time) bool
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
	cfg        Config
	store      TradeStore
	prices     PriceFetcher
	exec       execution.Executor
	now        func() time.Time
	marketOpen func(time.Time) bool
}

// NewManager constructs a trailing-stop Manager.
func NewManager(cfg Config, ts TradeStore, prices PriceFetcher, exec execution.Executor) *Manager {
	normalized := cfg.Normalize()
	marketOpen := normalized.MarketOpen
	if marketOpen == nil {
		marketOpen = regularStockSession
	}
	return &Manager{
		cfg:        normalized,
		store:      ts,
		prices:     prices,
		exec:       exec,
		now:        time.Now,
		marketOpen: marketOpen,
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
		"atr_period", m.cfg.ATRPeriod,
	)
	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()
	// Run immediately so the first ratchet doesn't wait a full interval.
	m.UpdateAll(ctx)
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
	trades, err := m.store.GetOpenTrades(ctx)
	if err != nil {
		logger.Warn("trailing-stop: GetOpenTrades failed", "err", err)
		return
	}
	for _, t := range trades {
		if !isLongStockTrade(t) {
			continue
		}
		if err := m.updateOne(ctx, t); err != nil {
			logger.Warn("trailing-stop: update failed",
				"trade_id", t.ID,
				"ticker", t.Ticker,
				"err", err,
			)
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
func (m *Manager) updateOne(ctx context.Context, t store.Trade) error {
	if m.cfg.Rule == RuleEMA9 {
		return m.updateOneEMA9(ctx, t)
	}
	if t.Quantity <= 0 {
		return fmt.Errorf("non-positive quantity %f", t.Quantity)
	}
	currentPrice, atr, err := m.priceAndATR(ctx, t.Ticker)
	if err != nil {
		return fmt.Errorf("price/atr: %w", err)
	}
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

	// If the trail says we should already be out (stop ≥ current price)
	// the broker stop would fill instantly anyway. Don't leave a stale stop
	// sitting at the old price — exit at market immediately so the band's
	// implied "we're done" actually plays out.
	if newStop >= currentPrice {
		logger.Info("trailing-stop: trail crossed price; exiting at market",
			"trade_id", t.ID,
			"ticker", t.Ticker,
			"new_stop", newStop,
			"current_price", currentPrice,
		)
		if err := m.exitAndClose(ctx, t, currentPrice); err != nil {
			// Persist HWM so the next tick has fresh baseline; surface error.
			_ = m.store.UpdateTradeTrailing(ctx, t.ID, 0, hwm)
			return fmt.Errorf("market exit: %w", err)
		}
		// Persist HWM after CloseTrade so the trail snapshot is intact for
		// post-mortem queries.
		return m.store.UpdateTradeTrailing(ctx, t.ID, 0, hwm)
	}

	// Submit/replace the broker STOP whenever it moved. Also seed a missing
	// broker stop even when the ATR candidate still equals the original hard
	// stop; uncapped entries are deliberately not brackets, so their first
	// manager tick must establish protection immediately.
	existingStop, err := m.store.LatestStopLossOrderForTrade(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("lookup protective stop: %w", err)
	}
	if newStop != persistedStop || existingStop == nil {
		if err := m.submitOrReplaceStop(ctx, t, newStop); err != nil {
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

	now := m.now()
	closePrice, ema, err := m.priceAndEMA9(ctx, t.Ticker, now)
	if err != nil {
		return fmt.Errorf("price/ema9: %w", err)
	}
	if closePrice <= 0 || ema <= 0 {
		return errors.New("non-positive close or ema9")
	}

	if closePrice < ema {
		if !m.marketOpen(now) {
			// A market sell submitted after 16:00 ET can sit queued until the
			// next open. Keep the trade row and broker hard stop live overnight,
			// then retry this same completed-close signal during the next
			// eligible session.
			if err := m.ensureProtectiveStop(ctx, t); err != nil {
				return err
			}
			logger.Info("trailing-stop: daily close below ema9; deferring market exit until regular session",
				"trade_id", t.ID,
				"ticker", t.Ticker,
				"daily_close", closePrice,
				"ema9", ema,
			)
			return nil
		}
		logger.Info("trailing-stop: daily close below ema9; exiting at market",
			"trade_id", t.ID,
			"ticker", t.Ticker,
			"daily_close", closePrice,
			"ema9", ema,
		)
		return m.exitAndClose(ctx, t, closePrice)
	}

	// Uncapped entries do not have OCO bracket children. Ensure their original
	// hard stop is resident at the broker while the EMA winning exit waits.
	return m.ensureProtectiveStop(ctx, t)
}

func (m *Manager) ensureProtectiveStop(ctx context.Context, t store.Trade) error {
	existing, err := m.store.LatestStopLossOrderForTrade(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("lookup protective stop: %w", err)
	}
	if existing == nil {
		if err := m.submitOrReplaceStop(ctx, t, t.StopLoss); err != nil {
			return fmt.Errorf("seed protective stop: %w", err)
		}
	}
	return nil
}

// priceAndATR fetches recent 1h candles and returns (last close, ATR).
// ATR honours the configured `risk.trailing_stop.atr_period` so the operator
// knob actually flows through to the trailing band.
func (m *Manager) priceAndATR(ctx context.Context, ticker string) (float64, float64, error) {
	if m.prices == nil {
		return 0, 0, errors.New("no price provider configured")
	}
	candles, err := m.prices.FetchHistory(ctx, ticker, data.Timeframe1Hour, candleHistory)
	if err != nil {
		return 0, 0, err
	}
	if len(candles) == 0 {
		return 0, 0, errors.New("no candles returned")
	}
	atrVal := indicators.ComputeATR(candles, m.cfg.ATRPeriod)
	return candles[len(candles)-1].Close, atrVal, nil
}

// priceAndEMA9 fetches daily candles so the live rule uses the same timeframe
// as the exit-policy study. The provider's latest returned daily bar is the
// close evidence used by the rule.
func (m *Manager) priceAndEMA9(ctx context.Context, ticker string, now time.Time) (float64, float64, error) {
	if m.prices == nil {
		return 0, 0, errors.New("no price provider configured")
	}
	candles, err := m.prices.FetchHistory(ctx, ticker, data.Timeframe1Day, candleHistory)
	if err != nil {
		return 0, 0, err
	}
	candles = completedDailyCandles(candles, now)
	if len(candles) < EMAPeriod {
		return 0, 0, fmt.Errorf("need at least %d daily candles, got %d", EMAPeriod, len(candles))
	}
	ema := emaClose(candles, EMAPeriod)
	return candles[len(candles)-1].Close, ema, nil
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
func (m *Manager) submitOrReplaceStop(ctx context.Context, t store.Trade, newStop float64) error {
	existing, err := m.store.LatestStopLossOrderForTrade(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("lookup existing stop: %w", err)
	}

	side := execution.OrderSideSell // long-only scope: exit a long via SELL
	if existing != nil && existing.BrokerOrderID != "" {
		// Replace path — atomic on the broker side, no double-stop window.
		if replacer, ok := m.exec.(execution.StopOrderReplacer); ok {
			res, err := replacer.ReplaceStopOrder(ctx, existing.BrokerOrderID, newStop)
			if err == nil {
				return m.store.UpdateOrderStopPriceAndID(ctx, existing.ID, res.BrokerOrderID, newStop)
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

	res, err := m.exec.PlaceOrder(ctx, execution.OrderRequest{
		Ticker:      t.Ticker,
		Side:        side,
		Type:        execution.OrderTypeStop,
		Quantity:    t.Quantity,
		StopPrice:   newStop,
		TradeID:     t.ID,
		TimeInForce: protectiveStopTIF(t.Quantity),
	})
	if err != nil {
		return fmt.Errorf("place stop: %w", err)
	}

	if existing != nil {
		// Repoint the existing row at the freshly-submitted broker id.
		return m.store.UpdateOrderStopPriceAndID(ctx, existing.ID, res.BrokerOrderID, newStop)
	}
	_, err = m.store.InsertOrder(ctx, store.Order{
		TradeID:       t.ID,
		BrokerOrderID: res.BrokerOrderID,
		Ticker:        t.Ticker,
		Side:          string(side),
		OrderType:     string(execution.OrderTypeStop),
		Quantity:      t.Quantity,
		StopPrice:     newStop,
		Status:        execution.NormalizeOrderStatus(res.Status),
		IsStopLoss:    true,
	})
	return err
}

func protectiveStopTIF(quantity float64) string {
	if quantity != math.Trunc(quantity) {
		// Alpaca accepts fractional equity stop/stop-limit orders only as DAY.
		return "day"
	}
	return "gtc"
}

// exitAndClose submits the market exit and immediately closes the trade row so
// the next manager tick cannot submit a duplicate sell. The broker fill wins
// when available; asynchronous brokers fall back to the candle close that
// triggered the policy and are later reconciled to broker truth.
func (m *Manager) exitAndClose(ctx context.Context, t store.Trade, fallbackPrice float64) error {
	exitRes, err := m.exitAtMarket(ctx, t)
	if err != nil {
		return err
	}
	exitPrice := exitRes.FilledPrice
	if exitPrice == 0 {
		exitPrice = fallbackPrice
	}
	pnl := (exitPrice - t.EntryPrice) * t.Quantity
	pnlPct := 0.0
	if t.EntryPrice > 0 {
		pnlPct = pnl / (t.EntryPrice * t.Quantity) * 100
	}
	if err := m.store.CloseTrade(ctx, t.ID, exitPrice, pnl, pnlPct); err != nil {
		return fmt.Errorf("close trade after market exit: %w", err)
	}
	return nil
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
	case "cancelled", "filled", "rejected":
		return nil
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
func (m *Manager) exitAtMarket(ctx context.Context, t store.Trade) (execution.OrderResult, error) {
	// Cancel every working exit leg first — not just the latest stop. For
	// bracket-managed trades the stop AND take-profit legs each hold the
	// trade's shares at the broker; a market sell with either leg still
	// working is rejected for insufficient free quantity. Failure is logged
	// but doesn't block the market exit (the exit is the safety move).
	if legs, err := m.store.OpenExitLegsForTrade(ctx, t.ID); err == nil {
		for _, leg := range legs {
			if leg.BrokerOrderID == "" {
				continue
			}
			if err := m.exec.CancelOrder(ctx, leg.BrokerOrderID); err != nil {
				logger.Warn("trailing-stop: cancel of exit leg before market exit failed",
					"trade_id", t.ID,
					"broker_order_id", leg.BrokerOrderID,
					"err", err,
				)
			}
		}
	} else {
		logger.Warn("trailing-stop: exit-leg lookup before market exit failed",
			"trade_id", t.ID, "err", err)
	}
	res, err := m.exec.PlaceOrder(ctx, execution.OrderRequest{
		Ticker:   t.Ticker,
		Side:     execution.OrderSideSell,
		Type:     execution.OrderTypeMarket,
		Quantity: t.Quantity,
		TradeID:  t.ID,
	})
	if err != nil {
		return execution.OrderResult{}, fmt.Errorf("place market sell: %w", err)
	}
	// Persist the order with the broker's actual fill data when present so
	// downstream pnl/order-history queries see the real exit, not just the
	// snapshot price that triggered the trail. Falls back to t.Quantity for
	// FilledQty when the broker hasn't reported a fill yet (Status=pending).
	filledQty := res.FilledQty
	if filledQty == 0 {
		filledQty = t.Quantity
	}
	if _, insErr := m.store.InsertOrder(ctx, store.Order{
		TradeID:       t.ID,
		BrokerOrderID: res.BrokerOrderID,
		Ticker:        t.Ticker,
		Side:          string(execution.OrderSideSell),
		OrderType:     string(execution.OrderTypeMarket),
		Quantity:      t.Quantity,
		FilledPrice:   res.FilledPrice,
		FilledQty:     filledQty,
		Status:        execution.NormalizeOrderStatus(res.Status),
		IsStopLoss:    true, // counts toward the trade's exit bookkeeping
	}); insErr != nil {
		return res, insErr
	}
	return res, nil
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
