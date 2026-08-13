package execution

import (
	"context"
	"errors"
	"fmt"
)

// ErrReadOnlyExecutor is returned when dry-run code attempts a broker mutation.
var ErrReadOnlyExecutor = errors.New("execution: broker mutation blocked in dry-run mode")

// ReadOnlyExecutor preserves broker reads while rejecting every mutation. It is
// used by the standard stock engine's dry-run path so dry-run cannot place,
// cancel, or replace an order at Alpaca.
type ReadOnlyExecutor struct {
	inner Executor
}

func NewReadOnlyExecutor(inner Executor) *ReadOnlyExecutor {
	return &ReadOnlyExecutor{inner: inner}
}

func (e *ReadOnlyExecutor) PlaceOrder(context.Context, OrderRequest) (OrderResult, error) {
	return OrderResult{}, fmt.Errorf("place order: %w", ErrReadOnlyExecutor)
}

func (e *ReadOnlyExecutor) CancelOrder(context.Context, string) error {
	return fmt.Errorf("cancel order: %w", ErrReadOnlyExecutor)
}

func (e *ReadOnlyExecutor) GetOrderStatus(ctx context.Context, brokerOrderID string) (OrderResult, error) {
	return e.inner.GetOrderStatus(ctx, brokerOrderID)
}

func (e *ReadOnlyExecutor) GetPositions(ctx context.Context) ([]Position, error) {
	return e.inner.GetPositions(ctx)
}

func (e *ReadOnlyExecutor) GetAccount(ctx context.Context) (AccountInfo, error) {
	return e.inner.GetAccount(ctx)
}

func (e *ReadOnlyExecutor) ReplaceStopOrder(context.Context, string, float64) (OrderResult, error) {
	return OrderResult{}, fmt.Errorf("replace stop order: %w", ErrReadOnlyExecutor)
}

// BrokerName preserves the underlying adapter identity for safe observability
// such as portfolio snapshot tags.
func (e *ReadOnlyExecutor) BrokerName() string {
	if named, ok := e.inner.(interface{ BrokerName() string }); ok {
		return named.BrokerName()
	}
	return ""
}

// AccountSnapshot and the remaining BrokerView methods forward read-only
// broker capabilities so dry-run monitoring and reconciliation stay available.
func (e *ReadOnlyExecutor) AccountSnapshot(ctx context.Context) (BrokerAccount, error) {
	bv, ok := e.inner.(BrokerView)
	if !ok {
		return BrokerAccount{}, ErrBrokerUnavailable
	}
	return bv.AccountSnapshot(ctx)
}

func (e *ReadOnlyExecutor) PositionsSnapshot(ctx context.Context) ([]BrokerPosition, error) {
	bv, ok := e.inner.(BrokerView)
	if !ok {
		return nil, ErrBrokerUnavailable
	}
	return bv.PositionsSnapshot(ctx)
}

func (e *ReadOnlyExecutor) EquityCurve(ctx context.Context, period, timeframe string) (EquityHistory, error) {
	bv, ok := e.inner.(BrokerView)
	if !ok {
		return EquityHistory{}, ErrBrokerUnavailable
	}
	return bv.EquityCurve(ctx, period, timeframe)
}

func (e *ReadOnlyExecutor) RecentFills(ctx context.Context) ([]BrokerOrder, error) {
	bv, ok := e.inner.(BrokerView)
	if !ok {
		return nil, ErrBrokerUnavailable
	}
	return bv.RecentFills(ctx)
}

// GetWorkingOrders forwards the optional working-order read used by recovery
// and reconciliation. It never changes broker state.
func (e *ReadOnlyExecutor) GetWorkingOrders(ctx context.Context) ([]WorkingOrder, error) {
	lister, ok := e.inner.(WorkingOrdersLister)
	if !ok {
		return nil, ErrBrokerUnavailable
	}
	return lister.GetWorkingOrders(ctx)
}

func (e *ReadOnlyExecutor) StockAssetSnapshot(ctx context.Context, symbol string) (StockAsset, error) {
	provider, ok := e.inner.(StockAssetProvider)
	if !ok {
		return StockAsset{}, ErrBrokerUnavailable
	}
	return provider.StockAssetSnapshot(ctx, symbol)
}

// GetOptionChain and GetOptionSnapshots preserve Alpaca's option-market-data
// reads for option selection and exit pricing. They expose no mutation path;
// option order placement remains blocked by PlaceOrder above.
func (e *ReadOnlyExecutor) GetOptionChain(ctx context.Context, underlying string, filt OptionChainFilter) ([]OptionSnapshot, error) {
	provider, ok := e.inner.(interface {
		GetOptionChain(context.Context, string, OptionChainFilter) ([]OptionSnapshot, error)
	})
	if !ok {
		return nil, ErrBrokerUnavailable
	}
	return provider.GetOptionChain(ctx, underlying, filt)
}

func (e *ReadOnlyExecutor) GetOptionSnapshots(ctx context.Context, symbols []string) ([]OptionSnapshot, error) {
	provider, ok := e.inner.(interface {
		GetOptionSnapshots(context.Context, []string) ([]OptionSnapshot, error)
	})
	if !ok {
		return nil, ErrBrokerUnavailable
	}
	return provider.GetOptionSnapshots(ctx, symbols)
}

func (e *ReadOnlyExecutor) Recover(ctx context.Context) error {
	// Recovery can replay a previously accepted-but-unprojected mutation. It
	// must therefore be treated as a mutation itself: dry-run deliberately
	// neither creates intents nor sends a broker request.
	return nil
}
