package execution

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// IBKRBroker implements Executor against the Interactive Brokers Client Portal REST API.
// The Client Portal gateway runs locally and uses cookie-based SSO — no API keys are
// sent in request headers.
type IBKRBroker struct {
	accountID  string
	gatewayURL string
	httpClient *http.Client

	mu          sync.Mutex
	lastRequest time.Time
	minInterval time.Duration // 100 ms → ≤ 10 req/s

	conidMu    sync.RWMutex
	conidCache map[string]string
}

// NewIBKRBroker creates an IBKRBroker from the supplied config.
// When cfg.InsecureTLS is true the TLS certificate check is skipped — required for
// the default IBKR Client Portal gateway which uses a self-signed certificate.
func NewIBKRBroker(cfg config.IBKRConfig) *IBKRBroker {
	transport := &http.Transport{}
	if cfg.InsecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return &IBKRBroker{
		accountID:   cfg.AccountID,
		gatewayURL:  cfg.GatewayURL,
		httpClient:  &http.Client{Timeout: 30 * time.Second, Transport: transport},
		minInterval: 100 * time.Millisecond,
		conidCache:  make(map[string]string),
	}
}

// throttle enforces the IBKR gateway's 10 req/s rate limit.
func (b *IBKRBroker) throttle() {
	b.mu.Lock()
	defer b.mu.Unlock()
	since := time.Since(b.lastRequest)
	if since < b.minInterval {
		time.Sleep(b.minInterval - since)
	}
	b.lastRequest = time.Now()
}

// PlaceOrder submits a simple or bracket order to the IBKR Client Portal gateway.
func (b *IBKRBroker) PlaceOrder(ctx context.Context, req OrderRequest) (OrderResult, error) {
	secType := req.SecType
	if secType == "" {
		secType = "CASH"
	}
	conid, err := b.resolveConid(ctx, req.Ticker, secType)
	if err != nil {
		return OrderResult{}, fmt.Errorf("ibkr PlaceOrder: resolve conid for %q: %w", req.Ticker, err)
	}

	orderType := mapOrderType(req.Type)

	var orders []map[string]any
	if req.OrderClass == "bracket" {
		orders = buildBracketOrders(conid, secType, orderType, req)
	} else {
		orders = []map[string]any{buildSingleOrder(conid, secType, orderType, req)}
	}

	payload := map[string]any{"orders": orders}

	// POST orders — IBKR may return a confirmation prompt instead of an order ID.
	var rawResp json.RawMessage
	if err := b.do(ctx, http.MethodPost, "/iserver/account/"+b.accountID+"/orders", payload, &rawResp); err != nil {
		return OrderResult{}, fmt.Errorf("ibkr PlaceOrder: %w", err)
	}

	return b.resolveOrderResponse(ctx, rawResp)
}

// resolveOrderResponse handles IBKR's two-step order confirmation flow.
// On success the gateway returns [{order_id, order_status, ...}].
// On a compliance/confirmation prompt it returns [{id, message, ...}]; we
// auto-confirm by POSTing {"confirmed": true} to /iserver/reply/{replyId}.
func (b *IBKRBroker) resolveOrderResponse(ctx context.Context, raw json.RawMessage) (OrderResult, error) {
	// Try direct order result first.
	var orderResults []struct {
		OrderID     string `json:"order_id"`
		OrderStatus string `json:"order_status"`
	}
	if err := json.Unmarshal(raw, &orderResults); err == nil && len(orderResults) > 0 && orderResults[0].OrderID != "" {
		r := orderResults[0]
		return OrderResult{
			BrokerOrderID: r.OrderID,
			Status:        NormalizeIBKROrderStatus(r.OrderStatus),
		}, nil
	}

	// Check for a reply (confirmation) prompt.
	var replyPrompts []struct {
		ID      string `json:"id"`
		Message []string `json:"message"`
	}
	if err := json.Unmarshal(raw, &replyPrompts); err != nil || len(replyPrompts) == 0 || replyPrompts[0].ID == "" {
		return OrderResult{}, fmt.Errorf("ibkr PlaceOrder: unexpected response: %s", raw)
	}

	replyID := replyPrompts[0].ID
	var confirmed json.RawMessage
	if err := b.do(ctx, http.MethodPost, "/iserver/reply/"+replyID, map[string]bool{"confirmed": true}, &confirmed); err != nil {
		return OrderResult{}, fmt.Errorf("ibkr PlaceOrder: confirm reply %s: %w", replyID, err)
	}

	return b.resolveOrderResponse(ctx, confirmed)
}

// CancelOrder sends a DELETE request to cancel an open order.
func (b *IBKRBroker) CancelOrder(ctx context.Context, brokerOrderID string) error {
	return b.do(ctx, http.MethodDelete, "/iserver/account/"+b.accountID+"/order/"+brokerOrderID, nil, nil)
}

// GetOrderStatus fetches the current state of an order by scanning the live order list.
func (b *IBKRBroker) GetOrderStatus(ctx context.Context, brokerOrderID string) (OrderResult, error) {
	var resp struct {
		Orders []struct {
			OrderID     string `json:"orderId"`
			Status      string `json:"status"`
			FilledPrice float64 `json:"avgPrice"`
			FilledQty   float64 `json:"filledQuantity"`
		} `json:"orders"`
	}
	if err := b.do(ctx, http.MethodGet, "/iserver/account/orders", nil, &resp); err != nil {
		return OrderResult{}, fmt.Errorf("ibkr GetOrderStatus: %w", err)
	}

	for _, o := range resp.Orders {
		if o.OrderID == brokerOrderID {
			return OrderResult{
				BrokerOrderID: o.OrderID,
				Status:        NormalizeIBKROrderStatus(o.Status),
				FilledPrice:   o.FilledPrice,
				FilledQty:     o.FilledQty,
			}, nil
		}
	}
	return OrderResult{}, fmt.Errorf("ibkr GetOrderStatus: order %q not found", brokerOrderID)
}

// GetPositions returns all open positions from the IBKR account.
// The conid→ticker mapping is reverse-resolved via the local cache/hardcoded map.
func (b *IBKRBroker) GetPositions(ctx context.Context) ([]Position, error) {
	var resp []struct {
		Conid        int     `json:"conid"`
		Position     float64 `json:"position"`
		AvgCost      float64 `json:"avgCost"`
		MktValue     float64 `json:"mktValue"`
		UnrealizedPL float64 `json:"unrealizedPnl"`
	}
	if err := b.do(ctx, http.MethodGet, "/iserver/account/"+b.accountID+"/positions/0", nil, &resp); err != nil {
		return nil, fmt.Errorf("ibkr GetPositions: %w", err)
	}

	out := make([]Position, 0, len(resp))
	for _, p := range resp {
		conidStr := fmt.Sprintf("%d", p.Conid)
		ticker := b.reverseConid(conidStr)
		out = append(out, Position{
			Ticker:       ticker,
			Quantity:     p.Position,
			AvgCostBasis: p.AvgCost,
			CurrentValue: p.MktValue,
			UnrealizedPL: p.UnrealizedPL,
		})
	}
	return out, nil
}

// reverseConid resolves a conid string back to a ticker symbol.
// Checks the hardcoded conid map first, then the dynamic cache.
func (b *IBKRBroker) reverseConid(conid string) string {
	for pair, c := range ibkrConids {
		if c == conid {
			return pair
		}
	}
	b.conidMu.RLock()
	defer b.conidMu.RUnlock()
	for pair, c := range b.conidCache {
		if c == conid {
			return pair
		}
	}
	return conid // fall back to raw conid if unknown
}

// GetAccount fetches the IBKR account summary (cash + net liquidation).
func (b *IBKRBroker) GetAccount(ctx context.Context) (AccountInfo, error) {
	var resp struct {
		TotalCashValue struct {
			Amount float64 `json:"amount"`
		} `json:"totalcashvalue"`
		NetLiquidation struct {
			Amount float64 `json:"amount"`
		} `json:"netliquidation"`
	}
	if err := b.do(ctx, http.MethodGet, "/v1/api/portfolio/"+b.accountID+"/summary", nil, &resp); err != nil {
		return AccountInfo{}, fmt.Errorf("ibkr GetAccount: %w", err)
	}
	return AccountInfo{
		Cash:            resp.TotalCashValue.Amount,
		PortfolioEquity: resp.NetLiquidation.Amount,
	}, nil
}

// NormalizeIBKROrderStatus maps IBKR order statuses to the canonical set used by the
// trade engine: pending | filled | partial | cancelled | rejected.
func NormalizeIBKROrderStatus(status string) string {
	switch status {
	case "Filled":
		return "filled"
	case "Cancelled", "ApiCancelled":
		return "cancelled"
	case "Inactive":
		return "rejected"
	default:
		// Submitted, PreSubmitted, PendingSubmit, PendingCancel, etc.
		return "pending"
	}
}

// ─── order builders ───────────────────────────────────────────────────────────

func mapOrderType(t OrderType) string {
	switch t {
	case OrderTypeMarket:
		return "MKT"
	case OrderTypeLimit:
		return "LMT"
	case OrderTypeStop:
		return "STP"
	default:
		return "MKT"
	}
}

func buildSingleOrder(conid, secType, orderType string, req OrderRequest) map[string]any {
	o := map[string]any{
		"conid":     conid,
		"secType":   secType,
		"side":      mapSide(req.Side),
		"quantity":  req.Quantity,
		"orderType": orderType,
		"tif":       "DAY",
		"cOID":      req.TradeID,
	}
	applyPrice(o, orderType, req)
	return o
}

func buildBracketOrders(conid, secType, orderType string, req OrderRequest) []map[string]any {
	parent := map[string]any{
		"conid":     conid,
		"secType":   secType,
		"side":      mapSide(req.Side),
		"quantity":  req.Quantity,
		"orderType": orderType,
		"tif":       "DAY",
		"cOID":      req.TradeID,
	}
	applyPrice(parent, orderType, req)

	// Opposing side for exit legs.
	exitSide := "SELL"
	if req.Side == OrderSideSell {
		exitSide = "BUY"
	}

	takeProfit := map[string]any{
		"conid":     conid,
		"secType":   secType,
		"side":      exitSide,
		"quantity":  req.Quantity,
		"orderType": "LMT",
		"price":     req.TakeProfitPrice,
		"tif":       "DAY",
		"parentId":  req.TradeID,
	}

	stopLoss := map[string]any{
		"conid":     conid,
		"secType":   secType,
		"side":      exitSide,
		"quantity":  req.Quantity,
		"orderType": "STP",
		"auxPrice":  req.StopLossPrice,
		"tif":       "DAY",
		"parentId":  req.TradeID,
	}

	return []map[string]any{parent, takeProfit, stopLoss}
}

func mapSide(side OrderSide) string {
	if side == OrderSideSell {
		return "SELL"
	}
	return "BUY"
}

// applyPrice attaches the correct price field(s) for the given order type.
func applyPrice(o map[string]any, orderType string, req OrderRequest) {
	switch orderType {
	case "LMT":
		if req.LimitPrice > 0 {
			o["price"] = req.LimitPrice
		}
	case "STP":
		if req.StopPrice > 0 {
			o["auxPrice"] = req.StopPrice
		}
	}
}

// ─── HTTP helper ──────────────────────────────────────────────────────────────

func (b *IBKRBroker) do(ctx context.Context, method, path string, body any, out any) error {
	b.throttle()

	var bodyReader io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		bodyReader = bytes.NewReader(bs)
	}

	req, err := http.NewRequestWithContext(ctx, method, b.gatewayURL+path, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ibkr %s %s → %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(respBytes))
	}
	if out != nil && len(respBytes) > 0 {
		return json.Unmarshal(respBytes, out)
	}
	return nil
}
