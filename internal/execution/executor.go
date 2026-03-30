// Package execution defines the Executor interface and all broker adapters.
// PRD §18 — Executor interface contract.
package execution

import "context"

// OrderSide identifies the direction of an order.
type OrderSide string

const (
	OrderSideBuy  OrderSide = "buy"
	OrderSideSell OrderSide = "sell"
)

// OrderType classifies the order's price semantics.
type OrderType string

const (
	OrderTypeMarket OrderType = "market"
	OrderTypeLimit  OrderType = "limit"
	OrderTypeStop   OrderType = "stop"
)

// OrderRequest is the input to PlaceOrder.
type OrderRequest struct {
	Ticker     string
	Side       OrderSide
	Type       OrderType
	Quantity   float64
	LimitPrice float64 // 0 for market orders
	StopPrice  float64 // for stop orders
	TradeID    string  // internal trade UUID for linkage

	// Bracket order fields (Phase 15 ORB). When OrderClass is "bracket",
	// the broker submits a parent + take-profit + stop-loss as one atomic order.
	OrderClass       string  // "" (simple) | "bracket"
	TakeProfitPrice  float64 // limit price for take-profit leg
	StopLossPrice    float64 // stop price for stop-loss leg
}

// OrderResult is the immediate response from the broker.
type OrderResult struct {
	BrokerOrderID string
	Status        string  // pending | filled | partial | cancelled
	FilledPrice   float64 // 0 if not yet filled
	FilledQty     float64
}

// Position represents a current open position at the broker.
type Position struct {
	Ticker       string
	Quantity     float64
	AvgCostBasis float64
	CurrentValue float64
	UnrealizedPL float64
}

// AccountInfo contains the broker account's cash and portfolio equity.
// Used as the single source of truth for portfolio valuation when a real
// broker is configured.
type AccountInfo struct {
	Cash            float64 // buying power / available cash
	PortfolioEquity float64 // total account equity (cash + positions)
}

// Executor is the interface all broker adapters must satisfy.
type Executor interface {
	PlaceOrder(ctx context.Context, req OrderRequest) (OrderResult, error)
	CancelOrder(ctx context.Context, brokerOrderID string) error
	GetOrderStatus(ctx context.Context, brokerOrderID string) (OrderResult, error)
	GetPositions(ctx context.Context) ([]Position, error)
	// GetAccount returns the broker account's cash and portfolio equity.
	// Implementations that don't have a real broker (PaperExecutor) return
	// zeroed AccountInfo and a nil error; callers fall back to local derivation.
	GetAccount(ctx context.Context) (AccountInfo, error)
}
