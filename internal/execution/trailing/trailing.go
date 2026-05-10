// Package trailing implements a server-side trailing stop manager (#322).
//
// On each tick the manager iterates open long stock trades, recomputes the
// trailing stop using the formula:
//
//	new_stop = max(current_stop, high_water_mark − atr_multiple × ATR)
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

	// candleHistory is the number of 1h candles fetched per tick to compute
	// ATR. Wilder's ATR is path-dependent; period+1 is the strict minimum but
	// in practice 30+ stabilises the smoothing.
	candleHistory = 50
)

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
	InsertOrder(ctx context.Context, o store.Order) (string, error)
	UpdateOrderStopPriceAndID(ctx context.Context, orderID, newBrokerOrderID string, newStopPrice float64) error
}

// Config controls the trailing-stop manager.
type Config struct {
	Enabled     bool
	ATRMultiple float64
	Interval    time.Duration
	ATRPeriod   int
}

// Normalize fills in defaults so callers can hand the manager a partially
// configured Config and still get sensible behaviour.
func (c Config) Normalize() Config {
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
	cfg    Config
	store  TradeStore
	prices PriceFetcher
	exec   execution.Executor
}

// NewManager constructs a trailing-stop Manager.
func NewManager(cfg Config, ts TradeStore, prices PriceFetcher, exec execution.Executor) *Manager {
	return &Manager{
		cfg:    cfg.Normalize(),
		store:  ts,
		prices: prices,
		exec:   exec,
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
	if t.Direction != "buy" {
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

	// Defensive sanity: a trailing stop above the current price would fill
	// instantly. Skip the broker call but keep the HWM update so we don't
	// spam logs on a noisy print.
	if newStop >= currentPrice {
		logger.Debug("trailing-stop: computed stop >= current price; skipping broker update",
			"trade_id", t.ID,
			"ticker", t.Ticker,
			"new_stop", newStop,
			"current_price", currentPrice,
		)
		return m.store.UpdateTradeTrailing(ctx, t.ID, 0, hwm)
	}

	// If the stop hasn't moved (and the HWM hasn't either), nothing to do.
	if newStop == t.StopLoss && hwm == t.HighWaterMark {
		return nil
	}

	// Submit/replace broker STOP order whenever the stop_loss actually
	// moved. HWM-only updates skip the broker call.
	if newStop != t.StopLoss {
		if err := m.submitOrReplaceStop(ctx, t, newStop); err != nil {
			// Persist HWM even on broker failure so the next tick has the
			// updated baseline; log WARN and retry next tick.
			_ = m.store.UpdateTradeTrailing(ctx, t.ID, 0, hwm)
			return fmt.Errorf("broker stop submit/replace: %w", err)
		}
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

// priceAndATR fetches recent 1h candles and returns (last close, ATR).
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
	set, err := indicators.Compute(candles)
	if err != nil {
		return 0, 0, err
	}
	return candles[len(candles)-1].Close, set.ATR, nil
}

// submitOrReplaceStop sends the new stop price to the broker. If a broker
// stop already exists it is replaced (PATCH on Alpaca / modify on IBKR) when
// the executor implements StopOrderReplacer. Otherwise the manager submits a
// fresh STOP order. The orders table is updated to reflect the live broker
// id and stop price.
func (m *Manager) submitOrReplaceStop(ctx context.Context, t store.Trade, newStop float64) error {
	existing, err := m.store.LatestStopLossOrderForTrade(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("lookup existing stop: %w", err)
	}

	side := execution.OrderSideSell // long-only scope: exit a long via SELL
	if existing != nil && existing.BrokerOrderID != "" {
		// Replace path.
		if replacer, ok := m.exec.(execution.StopOrderReplacer); ok {
			res, err := replacer.ReplaceStopOrder(ctx, existing.BrokerOrderID, newStop)
			if err == nil {
				return m.store.UpdateOrderStopPriceAndID(ctx, existing.ID, res.BrokerOrderID, newStop)
			}
			logger.Warn("trailing-stop: replace failed, falling back to cancel+resubmit",
				"trade_id", t.ID,
				"err", err,
			)
		}
		// Fallback: cancel then resubmit.
		if err := m.exec.CancelOrder(ctx, existing.BrokerOrderID); err != nil {
			logger.Warn("trailing-stop: cancel of stale stop failed; will still attempt resubmit",
				"trade_id", t.ID,
				"broker_order_id", existing.BrokerOrderID,
				"err", err,
			)
		}
	}

	res, err := m.exec.PlaceOrder(ctx, execution.OrderRequest{
		Ticker:    t.Ticker,
		Side:      side,
		Type:      execution.OrderTypeStop,
		Quantity:  t.Quantity,
		StopPrice: newStop,
		TradeID:   t.ID,
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
