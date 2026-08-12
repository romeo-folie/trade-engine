// Package execution defines the Executor interface and all broker adapters.
// PRD §18 — Executor interface contract.
package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrOrderNotFound is returned by Executor implementations when a broker
// reports that the order ID is unknown — typically a 404 from Alpaca's
// /v2/orders/{id} endpoint or IBKR's "not found in live orders + trades"
// fallthrough. Callers can detect this with errors.Is and treat it as a
// terminal state (the order is effectively gone) rather than as an
// uncertain status that blocks resubmission.
var ErrOrderNotFound = errors.New("execution: order not found at broker")

// BrokerRequestError preserves an adapter's definitive HTTP response. It lets
// durable callers distinguish a broker-declared rejection from a transport or
// 5xx ambiguity that must be looked up/replayed safely.
type BrokerRequestError struct {
	StatusCode int
	Message    string
}

func (e *BrokerRequestError) Error() string {
	return fmt.Sprintf("broker request failed with HTTP %d: %s", e.StatusCode, e.Message)
}

// IsDefinitiveOrderRejection reports broker responses that conclusively mean
// no order was accepted. 408/409/429 are deliberately excluded because they
// can be retried or race broker state; transport and 5xx errors are ambiguous.
func IsDefinitiveOrderRejection(err error) bool {
	var requestErr *BrokerRequestError
	if !errors.As(err, &requestErr) {
		return false
	}
	return requestErr.StatusCode >= 400 && requestErr.StatusCode < 500 &&
		requestErr.StatusCode != 408 && requestErr.StatusCode != 409 && requestErr.StatusCode != 429
}

// IsDuplicateClientOrderError is ambiguous: Alpaca can reject a replay before
// its prior accepted order has become visible to client-id lookup.
func IsDuplicateClientOrderError(err error) bool {
	var requestErr *BrokerRequestError
	return errors.As(err, &requestErr) && requestErr.StatusCode == 422 &&
		(strings.Contains(strings.ToLower(requestErr.Message), "client order") || strings.Contains(strings.ToLower(requestErr.Message), "client_order_id")) &&
		strings.Contains(strings.ToLower(requestErr.Message), "duplicate")
}

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

// OptionContract identifies a single option contract. It is carried, optionally,
// alongside an OrderRequest/Position when the instrument is an equity option
// (SecType "OPT"); it is nil for shares, forex, and futures. The OCC symbol is
// the broker's canonical contract key (e.g. "AAPL240223C00095000"); Multiplier
// is the contract multiplier (100 for standard US equity options) and makes
// notional/P&L math instrument-correct.
type OptionContract struct {
	Underlying string    // e.g. "AAPL" — the equity the option is written on
	Expiry     time.Time // contract expiration date
	Strike     float64   // strike price
	Right      string    // "C" (call) | "P" (put)
	OCCSymbol  string    // 21-char OCC symbol; the broker's tradable contract id
	Multiplier int       // shares per contract (100 for standard US equity options)
}

// OrderRequest is the input to PlaceOrder.
type OrderRequest struct {
	Ticker     string
	Side       OrderSide
	Type       OrderType
	Quantity   float64
	LimitPrice float64 // 0 for market orders
	StopPrice  float64 // for stop orders
	// ReferencePrice supplies an observed price for local/paper valuation of a
	// market order. Live broker adapters deliberately never serialize it.
	ReferencePrice float64
	TradeID        string // internal trade UUID for linkage

	// Bracket order fields (Phase 15 ORB). When OrderClass is "bracket",
	// the broker submits a parent + take-profit + stop-loss as one atomic order.
	OrderClass      string  // "" (simple) | "bracket"
	TakeProfitPrice float64 // limit price for take-profit leg
	StopLossPrice   float64 // stop price for stop-loss leg

	// TimeInForce overrides the broker default ("day" on Alpaca). Swing-hold
	// bracket exits use "gtc" so the child legs survive the session close —
	// day-TIF legs expire at 16:00 ET and would leave overnight positions
	// unprotected, exactly the hole broker-resident exits exist to plug.
	TimeInForce string // "" (broker default) | "day" | "gtc"

	// SecType overrides the IBKR security type for this order.
	// "" defaults to "CASH" (forex). Use "CFD" for metals like XAU/USD.
	SecType string // "" (default/CASH) | "CASH" (forex) | "CFD" (metals) | "OPT" (equity option)
	// ContractID pins an IBKR order to the exact persisted contract. Lifecycle
	// exits use it across futures rolls; empty keeps normal symbol resolution.
	ContractID string

	// Option carries the contract identity when SecType is "OPT"; nil otherwise.
	// The broker adapter uses Option.OCCSymbol as the order's symbol and
	// Quantity as the number of contracts.
	Option *OptionContract
	// ClientOrderID is Alpaca's caller-supplied idempotency key. The durable
	// stock-order coordinator assigns it before a broker request.
	ClientOrderID string
	IntentKey     string // stable engine operation key; never serialized to Alpaca
	// ReplacesBrokerOrderID marks a durable stop-replacement intent. It is
	// coordinator metadata, never part of an Alpaca order payload.
	ReplacesBrokerOrderID string
	// Entry policy is durable coordinator metadata. Alpaca ignores these fields;
	// they let recovery reproduce the trade's exit semantics after a broker
	// acceptance before engine projection.
	PolicyStopLoss     float64
	PolicyTakeProfit   float64
	UncappedTakeProfit bool
}

// OrderResult is the immediate response from the broker.
type OrderResult struct {
	BrokerOrderID string
	Status        string  // pending | filled | partial | cancelled
	FilledPrice   float64 // 0 if not yet filled
	FilledQty     float64
	FilledAt      time.Time // zero when the broker does not supply execution time
	// ChildOrderIDs holds the TP and stop-loss order IDs for bracket orders
	// (index 0 = take-profit, index 1 = stop-loss). Empty for single-leg orders.
	ChildOrderIDs []string
	RequestID     string // Alpaca X-Request-ID, for audit/support correlation
}

// ClientOrderLookup is the optional broker capability used after an ambiguous
// submit. It identifies engine-owned activity without ticker/side inference.
type ClientOrderLookup interface {
	GetOrderByClientID(ctx context.Context, clientOrderID string) (OrderResult, error)
}

// WorkingOrder is a live (unfilled / still-working) order at the broker.
// It exists so callers can find and cancel dangling bracket legs by symbol
// when the child order IDs were never captured at placement time (the IBKR
// bracket confirmation flow does not reliably return them).
type WorkingOrder struct {
	OrderID      string
	TradeID      string  // client/order reference supplied when the order was placed
	Ticker       string  // resolved symbol when the broker provides one (else raw conid)
	Conid        string  // IBKR contract id (empty for non-IBKR brokers)
	Side         string  // "buy" | "sell"
	Status       string  // normalized: pending | partial (only working orders are returned)
	RemainingQty float64 // quantity still working
	Price        float64 // limit price (0 if none / not exposed)
	StopPrice    float64 // stop trigger price (0 if none / not exposed)
}

// WorkingOrdersLister is an optional Executor capability: it lists the broker's
// live working orders. The ORB time-exit uses it to cancel a filled trade's
// remaining TP/SL bracket legs before flattening, since those leg IDs are not
// tracked. IBKRBroker implements it; executors that don't (PaperExecutor) are
// detected via a type assertion and the caller skips leg cancellation.
type WorkingOrdersLister interface {
	GetWorkingOrders(ctx context.Context) ([]WorkingOrder, error)
}

// Position represents a current open position at the broker.
type Position struct {
	Ticker       string
	ConID        string // exact broker contract id when available (IBKR)
	Quantity     float64
	AvgCostBasis float64 // per-share average entry price
	CurrentPrice float64 // per-share current price (0 if broker doesn't provide)
	CurrentValue float64 // total market value (qty × current price)
	UnrealizedPL float64
	Side         string // "long" or "short" (empty if broker doesn't provide)

	// Option is populated when the broker reports this position as an equity
	// option (Alpaca asset_class "us_option"); nil for shares/forex/futures.
	// Quantity is in contracts and market value already reflects the multiplier.
	Option *OptionContract
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

// IsTerminalOrderStatus reports whether a broker order status is final — the
// order will receive no further fills. A fill PRICE appearing is NOT terminal:
// a partially_filled fractional order has a price while more fills are coming,
// and treating that as done is what froze NOK's 148-share entry at its first
// 30-share partial. Accepts raw broker statuses or normalized ones.
func IsTerminalOrderStatus(status string) bool {
	switch NormalizeOrderStatus(status) {
	case "filled", "cancelled", "rejected":
		return true
	}
	// done_for_day: no more fills this session; the engine's day-TIF orders
	// won't resume tomorrow, so treat as terminal for fill polling.
	return status == "done_for_day"
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
