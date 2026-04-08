package execution

import (
	"context"
	"fmt"
	"sync"

	"github.com/romeo-folie/trade-engine/internal/logger"
)

// PaperExecutor simulates order execution locally without contacting any broker.
// Used in backtesting (Phase 7) and as a fallback when no broker is configured.
// All "fills" happen immediately at the provided market price.
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

// PlaceOrder simulates an immediate fill at the provided limit/stop price,
// or at a placeholder price of 0 for market orders (caller should substitute
// the fetched quote price before calling in live simulation).
func (p *PaperExecutor) PlaceOrder(_ context.Context, req OrderRequest) (OrderResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	id := fmt.Sprintf("PAPER-%06d", p.nextID)

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
