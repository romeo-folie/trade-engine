package trailing

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// StockIntentIDForKey is an optional extension consumed only by standard-stock
// durable execution; non-durable test stores need not implement it.
func (liveStore) StockIntentIDForKey(ctx context.Context, key string) (string, error) {
	if !store.IsConnected() {
		return "", errStoreNotConnected
	}
	intent, err := store.GetStockOrderIntentByKey(ctx, key)
	if err != nil || intent == nil {
		return "", err
	}
	return intent.ID, nil
}

// ResolveStockOrderIntentRetryGeneration follows durable terminal stop
// generations and returns either the already-existing active generation or
// one fresh deterministic successor key.
func (liveStore) ResolveStockOrderIntentRetryGeneration(ctx context.Context, key string) (string, error) {
	if !store.IsConnected() {
		return "", errStoreNotConnected
	}
	_, retryKey, err := store.ResolveStockOrderIntentRetryGeneration(ctx, key)
	return retryKey, err
}

// LinkReplacementOrderIntent advances durable intent lineage after the
// trailing manager, which owns normal replacement projection, repoints the
// broker order id.
func (liveStore) LinkReplacementOrderIntent(ctx context.Context, orderID, brokerOrderID string) error {
	if !store.IsConnected() {
		return errStoreNotConnected
	}
	intent, err := store.GetStockOrderIntentByBrokerOrderID(ctx, brokerOrderID)
	if err != nil {
		return err
	}
	if intent == nil {
		return fmt.Errorf("replacement intent for broker order %s not found", brokerOrderID)
	}
	return store.RepointOrderStockIntentAndSupersede(ctx, orderID, intent.ID)
}

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

func (liveStore) ExitLegsForMarketExit(ctx context.Context, tradeID string) ([]store.Order, error) {
	if !store.IsConnected() {
		return nil, errStoreNotConnected
	}
	return store.ExitLegsForMarketExit(ctx, tradeID)
}

func (liveStore) ClaimTradeExitWithIntent(ctx context.Context, id string, intent store.ExitIntent) (bool, error) {
	if !store.IsConnected() {
		return false, errStoreNotConnected
	}
	return store.ClaimTradeExitWithIntent(ctx, id, intent)
}

func (liveStore) ReleaseTradeExitClaim(ctx context.Context, id string) error {
	if !store.IsConnected() {
		return errStoreNotConnected
	}
	return store.ReleaseTradeExitClaim(ctx, id)
}

func (liveStore) InsertOrder(ctx context.Context, o store.Order) (string, error) {
	if !store.IsConnected() {
		return "", errStoreNotConnected
	}
	return store.InsertOrderWithRetry(ctx, o)
}

func (liveStore) UpdateOrderFill(ctx context.Context, orderID string, filledPrice, filledQty float64, status string) error {
	if !store.IsConnected() {
		return errStoreNotConnected
	}
	return store.UpdateOrderFill(ctx, orderID, filledPrice, filledQty, status)
}

func (liveStore) ApplyPartialExitAt(
	ctx context.Context,
	tradeID, orderID string,
	filledPrice, cumulativeFilledQty float64,
	status string,
	filledAt time.Time,
) (float64, float64, error) {
	if !store.IsConnected() {
		return 0, 0, errStoreNotConnected
	}
	return store.ApplyPartialExitAt(
		ctx, tradeID, orderID, filledPrice, cumulativeFilledQty, status, filledAt,
	)
}

func (liveStore) UpdateExitingTradeQuantity(ctx context.Context, id string, quantity float64) error {
	if !store.IsConnected() {
		return errStoreNotConnected
	}
	return store.UpdateExitingTradeQuantity(ctx, id, quantity)
}

func (liveStore) UpdateOrderStopReplacement(
	ctx context.Context,
	orderID, expectedBrokerOrderID, newBrokerOrderID string,
	expectedFilledQty, expectedTradeQuantity, newStopPrice, newQuantity float64,
	status string,
	filledPrice, filledQty float64,
	filledAt time.Time,
	intent store.ExitIntent,
) error {
	if !store.IsConnected() {
		return errStoreNotConnected
	}
	return store.UpdateOrderStopReplacement(
		ctx, orderID, expectedBrokerOrderID, newBrokerOrderID, expectedFilledQty, expectedTradeQuantity, newStopPrice, newQuantity,
		status, filledPrice, filledQty, filledAt, intent,
	)
}

func (liveStore) CloseTradeRealizedWithIntent(
	ctx context.Context,
	id string,
	exitPrice float64,
	filledAt time.Time,
	intent store.ExitIntent,
) (store.TradeRealization, error) {
	if !store.IsConnected() {
		return store.TradeRealization{}, errStoreNotConnected
	}
	return store.CloseTradeRealizedWithIntent(ctx, id, exitPrice, filledAt, intent)
}
