package execution

import (
	"context"
	"fmt"
	"sync"
)

// PredictionPaperExecutor simulates prediction market order execution without
// connecting to any real exchange.  It is used when the platform is not
// configured for live trading or during backtests.
//
// Contract conventions:
//   - OrderRequest.Ticker  = platform-scoped contract ID, e.g. "polymarket:1234"
//   - OrderRequest.Side    = "buy"  → buy YES contracts
//                          = "sell" → buy NO contracts (short Yes)
//   - OrderRequest.Type    = "market" or "limit"
//   - LimitPrice           = Yes-price in [0, 1] (implied probability)
//   - Quantity             = number of $1-face-value contracts
type PredictionPaperExecutor struct {
	mu        sync.Mutex
	nextID    int
	orders    map[string]OrderResult // brokerOrderID → result
	positions map[string]Position    // ticker → position
}

// NewPredictionPaperExecutor returns a ready-to-use paper executor.
func NewPredictionPaperExecutor() *PredictionPaperExecutor {
	return &PredictionPaperExecutor{
		orders:    make(map[string]OrderResult),
		positions: make(map[string]Position),
	}
}

// PlaceOrder simulates immediate fill at the provided limit price (or 0.5
// as a fallback for market orders).
func (p *PredictionPaperExecutor) PlaceOrder(_ context.Context, req OrderRequest) (OrderResult, error) {
	fillPrice := req.LimitPrice
	if fillPrice <= 0 || req.Type == OrderTypeMarket {
		fillPrice = 0.50 // neutral assumption for market orders
	}

	// nextID++ is inside the lock to prevent a data race when PlaceOrder is
	// called concurrently from analysis goroutines.
	p.mu.Lock()
	defer p.mu.Unlock()

	p.nextID++
	id := fmt.Sprintf("PRED-PAPER-%06d", p.nextID)

	result := OrderResult{
		BrokerOrderID: id,
		Status:        "filled",
		FilledPrice:   fillPrice,
		FilledQty:     req.Quantity,
	}

	p.orders[id] = result

	// Update paper position.
	existing := p.positions[req.Ticker]
	existing.Ticker = req.Ticker
	if req.Side == OrderSideBuy {
		newQty := existing.Quantity + req.Quantity
		if newQty > 0 {
			// Weighted average cost basis.
			existing.AvgCostBasis = (existing.Quantity*existing.AvgCostBasis + req.Quantity*fillPrice) / newQty
		}
		existing.Quantity = newQty
	} else {
		existing.Quantity -= req.Quantity
	}
	existing.CurrentValue = existing.Quantity * fillPrice
	p.positions[req.Ticker] = existing

	return result, nil
}

// CancelOrder marks an order cancelled (paper orders always cancel instantly).
func (p *PredictionPaperExecutor) CancelOrder(_ context.Context, brokerOrderID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	res, ok := p.orders[brokerOrderID]
	if !ok {
		return fmt.Errorf("prediction paper: order %s not found", brokerOrderID)
	}
	if res.Status == "filled" {
		return fmt.Errorf("prediction paper: order %s already filled", brokerOrderID)
	}
	res.Status = "cancelled"
	p.orders[brokerOrderID] = res
	return nil
}

// GetOrderStatus returns the stored result for the given order ID.
func (p *PredictionPaperExecutor) GetOrderStatus(_ context.Context, brokerOrderID string) (OrderResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	res, ok := p.orders[brokerOrderID]
	if !ok {
		return OrderResult{}, fmt.Errorf("prediction paper: order %s not found", brokerOrderID)
	}
	return res, nil
}

// GetPositions returns all current paper prediction positions.
func (p *PredictionPaperExecutor) GetPositions(_ context.Context) ([]Position, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	positions := make([]Position, 0, len(p.positions))
	for _, pos := range p.positions {
		if pos.Quantity != 0 {
			positions = append(positions, pos)
		}
	}
	return positions, nil
}

// SettlePosition resolves a prediction contract position, realising P&L.
// resolved = true  → YES resolves (each YES share worth $1.00)
// resolved = false → NO resolves  (each YES share worth $0.00)
// Returns the realised P&L.
func (p *PredictionPaperExecutor) SettlePosition(ticker string, resolved bool) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	pos, ok := p.positions[ticker]
	if !ok || pos.Quantity == 0 {
		return 0
	}
	var pnl float64
	if resolved {
		// YES contracts worth $1 each.
		pnl = pos.Quantity*1.0 - pos.Quantity*pos.AvgCostBasis
	} else {
		// YES contracts worth $0.
		pnl = -pos.Quantity * pos.AvgCostBasis
	}
	delete(p.positions, ticker)
	return pnl
}
