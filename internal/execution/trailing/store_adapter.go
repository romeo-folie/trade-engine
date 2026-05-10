package trailing

import (
	"context"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// liveStore adapts the package-level store helpers to the TradeStore
// interface so production can use the global DB connection without the
// manager holding a *pgxpool.Pool itself. Tests inject their own fake.
type liveStore struct{}

// LiveStore returns a TradeStore backed by the package-level store helpers.
// Use in production wiring; unit tests should construct a fake instead.
func LiveStore() TradeStore { return liveStore{} }

func (liveStore) GetOpenTrades(ctx context.Context) ([]store.Trade, error) {
	return store.GetOpenTrades(ctx)
}

func (liveStore) UpdateTradeTrailing(ctx context.Context, id string, stopLoss, highWaterMark float64) error {
	return store.UpdateTradeTrailing(ctx, id, stopLoss, highWaterMark)
}

func (liveStore) LatestStopLossOrderForTrade(ctx context.Context, tradeID string) (*store.Order, error) {
	return store.LatestStopLossOrderForTrade(ctx, tradeID)
}

func (liveStore) InsertOrder(ctx context.Context, o store.Order) (string, error) {
	return store.InsertOrder(ctx, o)
}

func (liveStore) UpdateOrderStopPriceAndID(ctx context.Context, orderID, newBrokerOrderID string, newStopPrice float64) error {
	return store.UpdateOrderStopPriceAndID(ctx, orderID, newBrokerOrderID, newStopPrice)
}
