// Package execution defines the Executor interface and all broker adapters.
// PRD §18 — Executor interface contract.
package execution

import (
	"context"
	"errors"
)

// ErrOrderNotFound is returned by Executor implementations when a broker
// reports that the order ID is unknown — typically a 404 from Alpaca's
// /v2/orders/{id} endpoint or IBKR's "not found in live orders + trades"
// fallthrough. Callers can detect this with errors.Is and treat it as a
// terminal state (the order is effectively gone) rather than as an
// uncertain status that blocks resubmission.
var ErrOrderNotFound = errors.New("execution: order not found at broker")

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

	// SecType overrides the IBKR security type for this order.
	// "" defaults to "CASH" (forex). Use "CFD" for metals like XAU/USD.
	SecType string // "" (default/CASH) | "CASH" (forex) | "CFD" (metals)
}

// OrderResult is the immediate response from the broker.
type OrderResult struct {
	BrokerOrderID string
	Status        string  // pending | filled | partial | cancelled
	FilledPrice   float64 // 0 if not yet filled
	FilledQty     float64
	// ChildOrderIDs holds the TP and stop-loss order IDs for bracket orders
	// (index 0 = take-profit, index 1 = stop-loss). Empty for single-leg orders.
	ChildOrderIDs []string
}

// Position represents a current open position at the broker.
type Position struct {
	Ticker       string
	Quantity     float64
	AvgCostBasis float64 // per-share average entry price
	CurrentPrice float64 // per-share current price (0 if broker doesn't provide)
	CurrentValue float64 // total market value (qty × current price)
	UnrealizedPL float64
	Side         string // "long" or "short" (empty if broker doesn't provide)
}

// NormalizeOrderStatus maps Alpaca's transient/broker-specific order statuses to
// the values accepted by the DB order_status enum: pending | filled | partial | cancelled | rejected.
// Alpaca returns states like "pending_new", "accepted", "held" before a fill — all map to "pending".
func NormalizeOrderStatus(status string) string {
	switch status {
	case "filled":
		return "filled"
	case "partially_filled":
		return "partial"
	case "cancelled", "canceled":
		return "cancelled"
	case "rejected", "expired":
		return "rejected"
	default:
		// pending_new, accepted, accepted_for_bidding, held, new, etc.
		return "pending"
	}
}

// AccountInfo contains the broker account's cash and portfolio equity.
// Used as the single source of truth for portfolio valuation when a real
// broker is configured.
type AccountInfo struct {
	Cash            float64 // cash balance (not buying power — Alpaca tracks those separately)
	PortfolioEquity float64 // total account equity (cash + positions)
	DayPnL          float64 // realized + unrealized profit/loss for the current session
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

// StopOrderReplacer is an optional capability implemented by broker adapters
// that support modifying an existing STOP order's price in place. The
// trailing-stop manager (#322) prefers replace over cancel+submit when the
// broker supports it. Implementations should treat ReplaceStopOrder as
// idempotent: returning a fresh OrderResult whose BrokerOrderID may differ
// from the input (Alpaca returns a new id on PATCH).
type StopOrderReplacer interface {
	ReplaceStopOrder(ctx context.Context, brokerOrderID string, newStopPrice float64) (OrderResult, error)
}
