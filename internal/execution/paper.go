package execution

import (
	"context"
	"fmt"
	"sync"

	"github.com/romeo-folie/trade-engine/internal/logger"
)

// StopOrderSimulator is implemented by executors that support simulated stop
// order fills driven by external price data. The ORB scheduler uses this
// interface to trigger fills when the market price crosses a stop level.
type StopOrderSimulator interface {
	// PendingStopOrders returns all stop orders that are still pending.
	PendingStopOrders() map[string]OrderRequest
	// SimulateFill transitions a pending order to filled at the given price.
	SimulateFill(orderID string, price float64) error
}

// PaperExecutor simulates order execution locally without contacting any broker.
// Used in backtesting (Phase 7) and as a fallback when no broker is configured.
// Market and limit orders fill immediately. Stop orders stay pending until
// SimulateFill is called (typically by the ORB scheduler after checking prices).
type PaperExecutor struct {
	mu     sync.RWMutex
	orders map[string]*paperOrder
	nextID int
}

type paperOrder struct {
	result  OrderResult
	request OrderRequest
}

// NewPaperExecutor creates a local paper trading executor.
func NewPaperExecutor() *PaperExecutor {
	return &PaperExecutor{orders: make(map[string]*paperOrder)}
}

// PlaceOrder simulates order placement. Market and limit orders fill
// immediately at the provided price. Stop orders (OrderTypeStop) are kept
// pending until SimulateFill is called with a market price that has crossed
// the stop level.
func (p *PaperExecutor) PlaceOrder(_ context.Context, req OrderRequest) (OrderResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	id := fmt.Sprintf("PAPER-%06d", p.nextID)

	// Stop orders stay pending — they only fill when price crosses the trigger.
	if req.Type == OrderTypeStop && req.StopPrice > 0 {
		result := OrderResult{
			BrokerOrderID: id,
			Status:        "pending",
		}
		p.orders[id] = &paperOrder{result: result, request: req}
		logger.Debug("paper stop order pending",
			"id", id,
			"ticker", req.Ticker,
			"side", string(req.Side),
			"qty", req.Quantity,
			"stopPrice", req.StopPrice,
		)
		return result, nil
	}

	// Market and limit orders fill immediately.
	fillPrice := req.LimitPrice
	if fillPrice == 0 {
		fillPrice = req.StopPrice
	}

	result := OrderResult{
		BrokerOrderID: id,
		Status:        "filled",
		FilledPrice:   fillPrice,
		FilledQty:     req.Quantity,
	}
	p.orders[id] = &paperOrder{result: result, request: req}

	logger.Debug("paper order filled",
		"id", id,
		"ticker", req.Ticker,
		"side", string(req.Side),
		"qty", req.Quantity,
		"price", fillPrice,
	)
	return result, nil
}

func (p *PaperExecutor) CancelOrder(_ context.Context, brokerOrderID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.orders[brokerOrderID]
	if !ok {
		return fmt.Errorf("paper order %s not found", brokerOrderID)
	}
	o.result.Status = "cancelled"
	return nil
}

func (p *PaperExecutor) GetOrderStatus(_ context.Context, brokerOrderID string) (OrderResult, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	o, ok := p.orders[brokerOrderID]
	if !ok {
		return OrderResult{}, fmt.Errorf("paper order %s not found", brokerOrderID)
	}
	return o.result, nil
}

// GetPositions is a no-op for the paper executor — position state is tracked
// by the portfolio package, not the executor.
func (p *PaperExecutor) GetPositions(_ context.Context) ([]Position, error) {
	return nil, nil
}

// GetAccount returns zeroed AccountInfo for the paper executor.
// The engine falls back to local derivation from initial_capital when Cash == 0.
func (p *PaperExecutor) GetAccount(_ context.Context) (AccountInfo, error) {
	return AccountInfo{}, nil
}

// ─── StopOrderSimulator ──────────────────────────────────────────────────────

// PendingStopOrders returns all stop orders that are still pending.
func (p *PaperExecutor) PendingStopOrders() map[string]OrderRequest {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]OrderRequest)
	for id, o := range p.orders {
		if o.result.Status == "pending" && o.request.Type == OrderTypeStop {
			out[id] = o.request
		}
	}
	return out
}

// SimulateFill transitions a pending order to filled at the given price.
func (p *PaperExecutor) SimulateFill(orderID string, price float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.orders[orderID]
	if !ok {
		return fmt.Errorf("paper order %s not found", orderID)
	}
	if o.result.Status != "pending" {
		return fmt.Errorf("paper order %s is %s, not pending", orderID, o.result.Status)
	}
	o.result.Status = "filled"
	o.result.FilledPrice = price
	o.result.FilledQty = o.request.Quantity
	logger.Debug("paper stop order filled (simulated)",
		"id", orderID,
		"ticker", o.request.Ticker,
		"side", string(o.request.Side),
		"qty", o.request.Quantity,
		"price", price,
	)
	return nil
}

// ─── BrokerView ───────────────────────────────────────────────────────────────
//
// PaperExecutor has no real broker connection, so every BrokerView call returns
// ErrBrokerUnavailable. The web layer uses this to fall back to DB-derived data
// and surface a "stale" badge in the UI.

func (p *PaperExecutor) AccountSnapshot(_ context.Context) (BrokerAccount, error) {
	return BrokerAccount{}, ErrBrokerUnavailable
}

func (p *PaperExecutor) PositionsSnapshot(_ context.Context) ([]BrokerPosition, error) {
	return nil, ErrBrokerUnavailable
}

func (p *PaperExecutor) EquityCurve(_ context.Context, _, _ string) (EquityHistory, error) {
	return EquityHistory{}, ErrBrokerUnavailable
}

func (p *PaperExecutor) RecentFills(_ context.Context) ([]BrokerOrder, error) {
	return nil, ErrBrokerUnavailable
}
