package trailing

import (
	"context"
	"errors"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// errStoreNotConnected is returned when the trailing manager is invoked
// before store.Connect() succeeded. We surface a clean error rather than
// letting store.GetDB() panic and take down the whole goroutine.
var errStoreNotConnected = errors.New("trailing: store not connected")

// liveStore adapts the package-level store helpers to the TradeStore
// interface so production can use the global DB connection without the
// manager holding a *pgxpool.Pool itself. Tests inject their own fake.
//
// All methods short-circuit with errStoreNotConnected (or an empty result
// for the read path) when store.IsConnected() returns false. This keeps
// the manager's UpdateAll loop alive and predictable when the DB is down,
// rather than panicking in production.
type liveStore struct{}

// LiveStore returns a TradeStore backed by the package-level store helpers.
// Use in production wiring; unit tests should construct a fake instead.
func LiveStore() TradeStore { return liveStore{} }

func (liveStore) GetOpenTrades(ctx context.Context) ([]store.Trade, error) {
	if !store.IsConnected() {
		// Empty result + nil error: UpdateAll iterates zero trades, logs
		// nothing, and silently retries on the next tick once Connect
		// finally succeeds. Returning an error would spam Warn logs.
		return nil, nil
	}
	return store.GetOpenTrades(ctx)
}

func (liveStore) UpdateTradeTrailing(ctx context.Context, id string, stopLoss, highWaterMark float64) error {
	if !store.IsConnected() {
		return errStoreNotConnected
	}
	return store.UpdateTradeTrailing(ctx, id, stopLoss, highWaterMark)
}

func (liveStore) LatestStopLossOrderForTrade(ctx context.Context, tradeID string) (*store.Order, error) {
	if !store.IsConnected() {
		return nil, errStoreNotConnected
	}
	return store.LatestStopLossOrderForTrade(ctx, tradeID)
}

func (liveStore) OpenExitLegsForTrade(ctx context.Context, tradeID string) ([]store.Order, error) {
	if !store.IsConnected() {
		return nil, errStoreNotConnected
	}
	return store.OpenExitLegsForTrade(ctx, tradeID)
}

func (liveStore) InsertOrder(ctx context.Context, o store.Order) (string, error) {
	if !store.IsConnected() {
		return "", errStoreNotConnected
	}
	return store.InsertOrder(ctx, o)
}

func (liveStore) UpdateOrderStopPriceAndID(
	ctx context.Context,
	orderID, newBrokerOrderID string,
	newStopPrice, newQuantity float64,
) error {
	if !store.IsConnected() {
		return errStoreNotConnected
	}
	return store.UpdateOrderStopPriceAndID(
		ctx, orderID, newBrokerOrderID, newStopPrice, newQuantity,
	)
}

func (liveStore) CloseTrade(ctx context.Context, id string, exitPrice, pnl, pnlPct float64) error {
	if !store.IsConnected() {
		return errStoreNotConnected
	}
	return store.CloseTrade(ctx, id, exitPrice, pnl, pnlPct)
}
