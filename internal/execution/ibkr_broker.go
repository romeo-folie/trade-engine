package execution

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/ibkrconids"
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
		gatewayURL:  strings.TrimRight(cfg.GatewayURL, "/"),
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
	if err := b.do(ctx, http.MethodPost, "/v1/api/iserver/account/"+b.accountID+"/orders", payload, &rawResp); err != nil {
		return OrderResult{}, fmt.Errorf("ibkr PlaceOrder: %w", err)
	}

	result, err := b.resolveOrderResponse(ctx, rawResp)
	if err != nil {
		return result, err
	}
	if req.OrderClass == "bracket" {
		result = b.verifyBracketLegIDs(ctx, req.TradeID, result)
	}
	return result, nil
}

// verifyBracketLegIDs re-derives a bracket's parent/TP/SL order IDs from the live order
// list instead of trusting the placement response's array order. When placement goes
// through the confirmation-prompt chain, IBKR returns the order-result array in whatever
// order the prompts cleared — NOT [parent, TP, SL]. On 2026-07-14 a rotated array made the
// scheduler poll the ENTRY order as its "SL"/"TP" leg, book phantom exits at the entry's own
// fill price, and re-enter until the broker held 3× the intended XAG position.
//
// Leg identity is structural and unambiguous in the live order list: the parent carries the
// cOID we set (surfaced as order_ref), and each child carries parentId plus its orderType
// (Limit = take-profit, Stop = stop-loss). If the legs cannot all be identified after a few
// attempts (gateway snapshot lag), the child IDs are DROPPED rather than returned
// positionally: positional IDs are known-untrusted, and a mislabeled leg can book a
// wrong-type/price exit even past the broker-truth guard once the position genuinely
// flattens (Codex P1). Without child IDs the scheduler's order-ID exit paths stand down
// and exit attribution falls to the trades-feed reconciler, which matches on broker fill
// records and is guarded. The parent ID is kept as the positional best guess — the order
// is already live at the broker, so the row must reference something cancellable, and a
// wrong parent guess is backstopped by the broker-truth entry-fill reconciler.
func (b *IBKRBroker) verifyBracketLegIDs(ctx context.Context, tradeID string, result OrderResult) OrderResult {
	if tradeID == "" {
		return result
	}
	// A parent verified by order_ref on ANY attempt is trustworthy even when the
	// children never resolve — prefer it over the positional guess in the fallbacks.
	var verifiedParentID string
	fallback := func(reason string, lastErr error) OrderResult {
		if verifiedParentID != "" {
			result.BrokerOrderID = verifiedParentID
		}
		slog.Error("ibkr: could not verify bracket leg IDs — dropping untrusted child IDs, exits fall back to trades-feed reconcile",
			"trade_id", tradeID, "reason", reason, "verified_parent", verifiedParentID,
			"children_dropped", result.ChildOrderIDs, "err", lastErr)
		result.ChildOrderIDs = nil
		return result
	}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return fallback("context cancelled", lastErr)
			case <-time.After(500 * time.Millisecond):
			}
		}
		var resp struct {
			Orders []struct {
				OrderID   ibkrID `json:"orderId"`
				ParentID  ibkrID `json:"parentId"`
				OrderRef  string `json:"order_ref"`
				OrderType string `json:"orderType"`
			} `json:"orders"`
		}
		if err := b.do(ctx, http.MethodGet, "/v1/api/iserver/account/orders", nil, &resp); err != nil {
			lastErr = err
			continue
		}
		var parentID, tpID, slID string
		for _, o := range resp.Orders {
			if o.OrderRef == tradeID {
				parentID = o.OrderID.String()
				break
			}
		}
		if parentID == "" {
			lastErr = fmt.Errorf("parent with order_ref %q not in live orders", tradeID)
			continue
		}
		verifiedParentID = parentID
		for _, o := range resp.Orders {
			if o.ParentID.String() != parentID {
				continue
			}
			switch t := strings.ToLower(o.OrderType); {
			case t == "limit" || t == "lmt":
				tpID = o.OrderID.String()
			case strings.HasPrefix(t, "stop") || t == "stp":
				slID = o.OrderID.String()
			}
		}
		if tpID == "" || slID == "" {
			lastErr = fmt.Errorf("bracket children of parent %s not fully identified (tp=%q sl=%q)", parentID, tpID, slID)
			continue
		}
		if result.BrokerOrderID != parentID || len(result.ChildOrderIDs) < 2 ||
			result.ChildOrderIDs[0] != tpID || result.ChildOrderIDs[1] != slID {
			slog.Warn("ibkr: bracket leg IDs corrected from live orders — placement response was not [parent, TP, SL]",
				"trade_id", tradeID,
				"response_parent", result.BrokerOrderID, "response_children", result.ChildOrderIDs,
				"verified_parent", parentID, "verified_tp", tpID, "verified_sl", slID)
		}
		result.BrokerOrderID = parentID
		result.ChildOrderIDs = []string{tpID, slID}
		return result
	}
	return fallback("retries exhausted", lastErr)
}

// resolveOrderResponse handles IBKR's multi-step order confirmation flow.
// On success the gateway returns [{order_id, order_status, ...}].
// On a compliance/confirmation prompt it returns [{id, message, ...}]; we
// auto-confirm by POSTing {"confirmed": true} to /iserver/reply/{replyId}.
// A multi-leg bracket (parent + TP + SL) can chain many sequential prompts —
// each stop leg draws the "you are about to submit a stop order" disclaimer,
// plus per-order size/value precautionary warnings — so we loop up to
// maxConfirmRounds rather than recursing once. Each prompt is logged: when a
// placement genuinely can't clear (an unexpected or repeating message), the
// log shows exactly which warning stalled it instead of a blind round-count.
func (b *IBKRBroker) resolveOrderResponse(ctx context.Context, raw json.RawMessage) (OrderResult, error) {
	// IBKR draws up to 3 precautionary prompts per order (size, value, stop-order
	// disclaimer); a 3-leg bracket can therefore chain well past the old cap of 5.
	const maxConfirmRounds = 15
	current := raw
	for round := 1; round <= maxConfirmRounds; round++ {
		if result, ok := parseIBKROrderResult(current); ok {
			return result, nil
		}

		// Check for a reply (confirmation) prompt.
		var replyPrompts []struct {
			ID      string   `json:"id"`
			Message []string `json:"message"`
		}
		if err := json.Unmarshal(current, &replyPrompts); err != nil || len(replyPrompts) == 0 || replyPrompts[0].ID == "" {
			return OrderResult{}, fmt.Errorf("ibkr PlaceOrder: unexpected response: %s", current)
		}

		replyID := replyPrompts[0].ID
		slog.Info("ibkr: auto-confirming order prompt",
			"round", round, "reply_id", replyID,
			"message", strings.Join(replyPrompts[0].Message, " | "))
		var confirmed json.RawMessage
		if err := b.do(ctx, http.MethodPost, "/v1/api/iserver/reply/"+replyID, map[string]bool{"confirmed": true}, &confirmed); err != nil {
			return OrderResult{}, fmt.Errorf("ibkr PlaceOrder: confirm reply %s: %w", replyID, err)
		}
		current = confirmed
	}
	// One final parse: the last confirmation round may have returned order IDs
	// directly and the loop exited before checking them.
	if result, ok := parseIBKROrderResult(current); ok {
		return result, nil
	}
	return OrderResult{}, fmt.Errorf("ibkr PlaceOrder: exceeded %d confirmation rounds", maxConfirmRounds)
}

// parseIBKROrderResult attempts to decode raw as a direct order-result array.
// Returns the populated OrderResult and true on success; zero value and false otherwise.
func parseIBKROrderResult(raw json.RawMessage) (OrderResult, bool) {
	var orderResults []struct {
		OrderID     string `json:"order_id"`
		OrderStatus string `json:"order_status"`
	}
	if err := json.Unmarshal(raw, &orderResults); err != nil || len(orderResults) == 0 || orderResults[0].OrderID == "" {
		return OrderResult{}, false
	}
	r := orderResults[0]
	result := OrderResult{
		BrokerOrderID: r.OrderID,
		Status:        NormalizeIBKROrderStatus(r.OrderStatus),
	}
	// Bracket responses include child order IDs at indices 1 (TP) and 2 (stop-loss).
	for _, child := range orderResults[1:] {
		if child.OrderID != "" {
			result.ChildOrderIDs = append(result.ChildOrderIDs, child.OrderID)
		}
	}
	return result, true
}

// CancelOrder sends a DELETE request to cancel an open order.
func (b *IBKRBroker) CancelOrder(ctx context.Context, brokerOrderID string) error {
	return b.do(ctx, http.MethodDelete, "/v1/api/iserver/account/"+b.accountID+"/order/"+brokerOrderID, nil, nil)
}

// ReplaceStopOrder updates the stop price on an open IBKR STOP order via the
// modify endpoint (POST /iserver/account/{acctId}/order/{orderId}). Used by
// the trailing-stop manager (#322). IBKR may return a confirmation prompt;
// the same resolveOrderResponse flow used by PlaceOrder handles it.
func (b *IBKRBroker) ReplaceStopOrder(ctx context.Context, brokerOrderID string, newStopPrice float64) (OrderResult, error) {
	body := map[string]any{
		"orderId":   brokerOrderID,
		"orderType": "STP",
		"price":     newStopPrice,
	}
	var raw json.RawMessage
	path := "/v1/api/iserver/account/" + b.accountID + "/order/" + brokerOrderID
	if err := b.do(ctx, http.MethodPost, path, body, &raw); err != nil {
		return OrderResult{}, fmt.Errorf("ibkr ReplaceStopOrder: %w", err)
	}
	return b.resolveOrderResponse(ctx, raw)
}

// GetOrderStatus fetches the current state of an order by scanning the live order list.
// If the order is absent from the live list (e.g. it filled or was cancelled before this
// poll), it falls back to the trades/executions endpoint which retains completed orders.
func (b *IBKRBroker) GetOrderStatus(ctx context.Context, brokerOrderID string) (OrderResult, error) {
	var resp struct {
		Orders []struct {
			OrderID   json.Number `json:"orderId"`
			Status    string      `json:"status"`
			AvgPrice  string      `json:"avgPrice"`
			FilledQty float64     `json:"filledQuantity"`
		} `json:"orders"`
	}
	if err := b.do(ctx, http.MethodGet, "/v1/api/iserver/account/orders", nil, &resp); err != nil {
		return OrderResult{}, fmt.Errorf("ibkr GetOrderStatus: %w", err)
	}

	for _, o := range resp.Orders {
		if o.OrderID.String() == brokerOrderID {
			var fillPrice float64
			if o.AvgPrice != "" {
				fmt.Sscanf(o.AvgPrice, "%f", &fillPrice)
			}
			return OrderResult{
				BrokerOrderID: o.OrderID.String(),
				Status:        NormalizeIBKROrderStatus(o.Status),
				FilledPrice:   fillPrice,
				FilledQty:     o.FilledQty,
			}, nil
		}
	}

	// Order not in live list — it may have already filled/cancelled.
	// Fall back to the executions endpoint which retains completed orders.
	// Schema matches RecentFills: orderId is a bare integer, status field is "status".
	// price and size arrive as quoted strings, so decode via ibkrFloat.
	var trades []struct {
		OrderID   int64     `json:"orderId"`
		Status    string    `json:"status"`
		Price     ibkrFloat `json:"price"`
		FilledQty ibkrFloat `json:"size"`
	}
	if err := b.do(ctx, http.MethodGet, "/v1/api/iserver/account/trades", nil, &trades); err != nil {
		return OrderResult{}, fmt.Errorf("ibkr GetOrderStatus: order %q not found in live orders, and trades lookup failed: %w", brokerOrderID, err)
	}
	for _, t := range trades {
		if fmt.Sprintf("%d", t.OrderID) == brokerOrderID {
			return OrderResult{
				BrokerOrderID: brokerOrderID,
				Status:        NormalizeIBKROrderStatus(t.Status),
				FilledPrice:   float64(t.Price),
				FilledQty:     float64(t.FilledQty),
			}, nil
		}
	}

	// Wrap ErrOrderNotFound so callers (e.g. trailing-stop's
	// cancelAndConfirm) can detect the terminal "order purged from broker"
	// case via errors.Is and treat it as cancelled rather than as an
	// unconfirmed status.
	return OrderResult{}, fmt.Errorf("ibkr GetOrderStatus: order %q: %w", brokerOrderID, ErrOrderNotFound)
}

// primePortfolio satisfies the Client Portal API's session precondition for
// /portfolio/* endpoints: /portfolio/accounts must have been called in the
// current gateway session first. Primed per call (one cheap GET) rather than
// once per client, because the gateway restarts underneath long-lived engine
// processes and a stale "already primed" flag would silently break positions.
func (b *IBKRBroker) primePortfolio(ctx context.Context) {
	var ignored any
	_ = b.do(ctx, http.MethodGet, "/v1/api/portfolio/accounts", nil, &ignored)
}

// GetPositions returns all open positions from the IBKR account.
// The conid→ticker mapping is reverse-resolved via the local cache/hardcoded map.
//
// Route: /v1/api/portfolio/{acct}/positions/{page} — the previous
// /v1/api/iserver/account/{acct}/positions/0 path does not exist in the
// Client Portal API and returned HTML 404 on every call since April (640
// occurrences): position-dependent logic (ORB time exits, reconciler) was
// flying blind, which is how two filled metals brackets ended up as live
// unprotected shorts the DB recorded as "cancelled".
func (b *IBKRBroker) GetPositions(ctx context.Context) ([]Position, error) {
	b.primePortfolio(ctx)
	var resp []struct {
		Conid        int     `json:"conid"`
		Position     float64 `json:"position"`
		AvgCost      float64 `json:"avgCost"`
		MktValue     float64 `json:"mktValue"`
		UnrealizedPL float64 `json:"unrealizedPnl"`
	}
	if err := b.do(ctx, http.MethodGet, "/v1/api/portfolio/"+b.accountID+"/positions/0", nil, &resp); err != nil {
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
	// Futures static map: keys are "SYMBOL/YYYYMM" (front-month), so a match resolves back
	// to the bare symbol. Without this, GetPositions returns statically-mapped futures
	// (MNQ/MES/…) as raw numeric conids that never match orb_trades.ticker — the gap that
	// forced the orphan detector and exit-side backstops to be forex-only (Codex #397).
	for key, c := range ibkrconids.FuturesConids {
		if c == conid {
			if sym, _, ok := strings.Cut(key, "/"); ok {
				return sym
			}
			return key
		}
	}
	b.conidMu.RLock()
	defer b.conidMu.RUnlock()
	for pair, c := range b.conidCache {
		if c == conid {
			// Dynamically-resolved futures are cached under "FUT:SYMBOL"; strip the prefix
			// so callers see the bare symbol, matching the static-map and forex behaviour.
			if sym, ok := strings.CutPrefix(pair, "FUT:"); ok {
				return sym
			}
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

// conidInt converts a string conid to an integer for the IBKR API,
// which rejects string-typed conids with a 400 "incorrect type" error.
func conidInt(conid string) any {
	if n, err := strconv.Atoi(conid); err == nil {
		return n
	}
	return conid // fallback to string if parse fails (shouldn't happen)
}

// qtyInt converts a float64 quantity to an integer for the IBKR API,
// which rejects float-typed quantities for certain instrument types (e.g. CFD).
func qtyInt(q float64) int {
	return int(math.Round(q))
}

func buildSingleOrder(conid, secType, orderType string, req OrderRequest) map[string]any {
	o := map[string]any{
		"conid":     conidInt(conid),
		"secType":   secType,
		"side":      mapSide(req.Side),
		"quantity":  qtyInt(req.Quantity),
		"orderType": orderType,
		"tif":       "DAY",
		"cOID":      req.TradeID,
	}
	applyPrice(o, orderType, req)
	return o
}

func buildBracketOrders(conid, secType, orderType string, req OrderRequest) []map[string]any {
	qty := qtyInt(req.Quantity)
	cid := conidInt(conid)

	parent := map[string]any{
		"conid":     cid,
		"secType":   secType,
		"side":      mapSide(req.Side),
		"quantity":  qty,
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
		"conid":     cid,
		"secType":   secType,
		"side":      exitSide,
		"quantity":  qty,
		"orderType": "LMT",
		"price":     req.TakeProfitPrice,
		"tif":       "DAY",
		"parentId":  req.TradeID,
	}

	stopLoss := map[string]any{
		"conid":     cid,
		"secType":   secType,
		"side":      exitSide,
		"quantity":  qty,
		"orderType": "STP",
		"price":     req.StopLossPrice,
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
// IBKR Client Portal API uses "price" for both LMT and STP order types.
func applyPrice(o map[string]any, orderType string, req OrderRequest) {
	switch orderType {
	case "LMT":
		if req.LimitPrice > 0 {
			o["price"] = req.LimitPrice
		}
	case "STP":
		if req.StopPrice > 0 {
			o["price"] = req.StopPrice
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

	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ibkr %s %s → %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(respBytes))
	}
	if out != nil && len(bytes.TrimSpace(respBytes)) > 0 {
		return json.Unmarshal(respBytes, out)
	}
	return nil
}
