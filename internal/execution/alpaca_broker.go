package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// parseFloat safely converts a numeric string from the Alpaca API.
// Returns 0 on empty string; returns error on malformed input.
func parseFloat(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

// defaultAlpacaDataURL is the market-data host (options chain snapshots live here,
// not on the trading base URL).
const defaultAlpacaDataURL = "https://data.alpaca.markets"

// AlpacaBroker implements Executor against the Alpaca paper (or live) trading API.
type AlpacaBroker struct {
	apiKey     string
	secret     string
	baseURL    string // trading host: orders, positions, /v2/options/contracts
	dataURL    string // market-data host: /v1beta1/options/snapshots
	optionFeed string // "opra" (real-time) | "indicative" (free, 15-min delayed)
	httpClient *http.Client
}

// NewAlpacaBroker creates an Alpaca broker adapter from market config.
// Uses the paper endpoint when cfg.Paper is true.
func NewAlpacaBroker(cfg config.StocksConfig) *AlpacaBroker {
	dataURL := cfg.DataURL
	if dataURL == "" {
		dataURL = defaultAlpacaDataURL
	}
	feed := cfg.OptionFeed
	if feed == "" {
		feed = "indicative"
	}
	return &AlpacaBroker{
		apiKey:     cfg.APIKey,
		secret:     cfg.Secret,
		baseURL:    cfg.BaseURL,
		dataURL:    dataURL,
		optionFeed: feed,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// BrokerName identifies this adapter without exposing its credentials or URL.
func (*AlpacaBroker) BrokerName() string { return "alpaca" }

// PlaceOrder submits a market or limit order to Alpaca.
func (b *AlpacaBroker) PlaceOrder(ctx context.Context, req OrderRequest) (OrderResult, error) {
	if req.SecType == "OPT" {
		return b.placeOptionOrder(ctx, req)
	}
	tif := req.TimeInForce
	if tif == "" {
		tif = "day"
	}
	body := map[string]any{
		"symbol":        req.Ticker,
		"qty":           fmt.Sprintf("%.6f", req.Quantity),
		"side":          string(req.Side),
		"type":          string(req.Type),
		"time_in_force": tif,
	}
	if req.ClientOrderID != "" {
		body["client_order_id"] = req.ClientOrderID
	}
	// Market orders must not include price fields — Alpaca returns 422 if they are present.
	if req.Type != OrderTypeMarket {
		if req.LimitPrice > 0 {
			body["limit_price"] = formatAlpacaStockPrice(req.LimitPrice)
		}
		if req.StopPrice > 0 {
			body["stop_price"] = formatAlpacaStockPrice(req.StopPrice)
		}
	}
	if req.OrderClass == "bracket" {
		body["order_class"] = "bracket"
		body["take_profit"] = map[string]string{
			"limit_price": formatAlpacaStockPrice(req.TakeProfitPrice),
		}
		body["stop_loss"] = map[string]string{
			"stop_price": formatAlpacaStockPrice(req.StopLossPrice),
		}
	}

	var resp struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		Qty         string `json:"qty"`
		FilledAvgPx string `json:"filled_avg_price"`
		FilledQty   string `json:"filled_qty"`
		FilledAt    string `json:"filled_at"`
		Legs        []struct {
			ID          string `json:"id"`
			Type        string `json:"type"` // "limit" (take-profit) | "stop" (stop-loss)
			Status      string `json:"status"`
			Qty         string `json:"qty"`
			FilledQty   string `json:"filled_qty"`
			FilledAvgPx string `json:"filled_avg_price"`
			FilledAt    string `json:"filled_at"`
			LimitPrice  string `json:"limit_price"`
			StopPrice   string `json:"stop_price"`
		} `json:"legs"`
	}
	requestID, err := b.doWithRequestID(ctx, http.MethodPost, "/v2/orders", body, &resp)
	if err != nil {
		return OrderResult{}, fmt.Errorf("alpaca PlaceOrder: %w", err)
	}

	filledPrice, err := parseFloat(resp.FilledAvgPx)
	if err != nil {
		return OrderResult{}, fmt.Errorf("alpaca PlaceOrder: parse filled_avg_price %q: %w", resp.FilledAvgPx, err)
	}
	filledQty, err := parseFloat(resp.FilledQty)
	if err != nil {
		return OrderResult{}, fmt.Errorf("alpaca PlaceOrder: parse filled_qty %q: %w", resp.FilledQty, err)
	}
	quantity, err := parseFloat(resp.Qty)
	if err != nil {
		return OrderResult{}, fmt.Errorf("alpaca PlaceOrder: parse qty %q: %w", resp.Qty, err)
	}

	result := OrderResult{
		BrokerOrderID: resp.ID,
		Status:        resp.Status,
		Quantity:      quantity,
		RemainingQty:  math.Max(0, quantity-filledQty),
		FilledPrice:   filledPrice,
		FilledQty:     filledQty,
		RequestID:     requestID,
	}
	result.FilledAt, _ = time.Parse(time.RFC3339, resp.FilledAt)
	// Bracket child legs, ordered [take-profit, stop-loss] per the
	// ChildOrderIDs contract. Identified by leg type, not array position —
	// Alpaca does not document leg ordering.
	if len(resp.Legs) > 0 {
		var tpID, slID string
		for _, leg := range resp.Legs {
			qty, qtyErr := parseFloat(leg.Qty)
			filled, filledErr := parseFloat(leg.FilledQty)
			filledPrice, filledPriceErr := parseFloat(leg.FilledAvgPx)
			limit, limitErr := parseFloat(leg.LimitPrice)
			stop, stopErr := parseFloat(leg.StopPrice)
			if qtyErr != nil || filledErr != nil || filledPriceErr != nil || limitErr != nil || stopErr != nil {
				return OrderResult{}, fmt.Errorf("alpaca PlaceOrder: parse bracket child %s quantities/prices", leg.ID)
			}
			result.ChildOrders = append(result.ChildOrders, OrderLeg{
				BrokerOrderID: leg.ID, Type: leg.Type, Status: leg.Status,
				Quantity: qty, RemainingQty: math.Max(0, qty-filled), FilledQty: filled,
				FilledPrice: filledPrice,
				LimitPrice:  limit, StopPrice: stop,
			})
			result.ChildOrders[len(result.ChildOrders)-1].FilledAt, _ = time.Parse(time.RFC3339, leg.FilledAt)
			switch leg.Type {
			case "limit":
				tpID = leg.ID
			case "stop", "stop_limit":
				slID = leg.ID
			}
		}
		if tpID != "" && slID != "" {
			result.ChildOrderIDs = []string{tpID, slID}
		}
	}
	return result, nil
}

// GetOrderByClientID looks up an order by the immutable client id supplied by
// the durable order-intent coordinator after an ambiguous submit.
func (b *AlpacaBroker) GetOrderByClientID(ctx context.Context, clientOrderID string) (OrderResult, error) {
	var resp struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		Qty         string `json:"qty"`
		FilledAvgPx string `json:"filled_avg_price"`
		FilledQty   string `json:"filled_qty"`
		FilledAt    string `json:"filled_at"`
		Legs        []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			Status      string `json:"status"`
			Qty         string `json:"qty"`
			FilledQty   string `json:"filled_qty"`
			FilledAvgPx string `json:"filled_avg_price"`
			FilledAt    string `json:"filled_at"`
			LimitPrice  string `json:"limit_price"`
			StopPrice   string `json:"stop_price"`
		} `json:"legs"`
	}
	requestID, err := b.doWithRequestID(ctx, http.MethodGet, "/v2/orders:by_client_order_id?nested=true&client_order_id="+url.QueryEscape(clientOrderID), nil, &resp)
	if err != nil {
		if isBrokerNotFound(err) {
			return OrderResult{}, fmt.Errorf("alpaca GetOrderByClientID: %w", ErrOrderNotFound)
		}
		return OrderResult{}, fmt.Errorf("alpaca GetOrderByClientID: %w", err)
	}
	price, err := parseFloat(resp.FilledAvgPx)
	if err != nil {
		return OrderResult{}, err
	}
	qty, err := parseFloat(resp.FilledQty)
	if err != nil {
		return OrderResult{}, err
	}
	orderQty, err := parseFloat(resp.Qty)
	if err != nil {
		return OrderResult{}, fmt.Errorf("alpaca GetOrderByClientID: parse qty %q: %w", resp.Qty, err)
	}
	filledAt, _ := time.Parse(time.RFC3339, resp.FilledAt)
	result := OrderResult{BrokerOrderID: resp.ID, Status: resp.Status, Quantity: orderQty, RemainingQty: math.Max(0, orderQty-qty), FilledPrice: price, FilledQty: qty, FilledAt: filledAt, RequestID: requestID}
	var tpID, slID string
	for _, leg := range resp.Legs {
		legQty, qtyErr := parseFloat(leg.Qty)
		legFilled, filledErr := parseFloat(leg.FilledQty)
		legFilledPrice, filledPriceErr := parseFloat(leg.FilledAvgPx)
		limit, limitErr := parseFloat(leg.LimitPrice)
		stop, stopErr := parseFloat(leg.StopPrice)
		if qtyErr != nil || filledErr != nil || filledPriceErr != nil || limitErr != nil || stopErr != nil {
			return OrderResult{}, fmt.Errorf("alpaca GetOrderByClientID: parse bracket child %s quantities/prices", leg.ID)
		}
		result.ChildOrders = append(result.ChildOrders, OrderLeg{
			BrokerOrderID: leg.ID, Type: leg.Type, Status: leg.Status,
			Quantity: legQty, RemainingQty: math.Max(0, legQty-legFilled), FilledQty: legFilled,
			FilledPrice: legFilledPrice,
			LimitPrice:  limit, StopPrice: stop,
		})
		result.ChildOrders[len(result.ChildOrders)-1].FilledAt, _ = time.Parse(time.RFC3339, leg.FilledAt)
		switch leg.Type {
		case "limit":
			tpID = leg.ID
		case "stop", "stop_limit":
			slID = leg.ID
		}
	}
	if tpID != "" && slID != "" {
		result.ChildOrderIDs = []string{tpID, slID}
	}
	return result, nil
}

func formatAlpacaStockPrice(price float64) string {
	decimals := 2
	scale := 100.0
	if price < 1 {
		decimals = 4
		scale = 10000.0
	}
	rounded := math.Round(price*scale) / scale
	return strconv.FormatFloat(rounded, 'f', decimals, 64)
}

// GetWorkingOrders lists Alpaca's currently-open (working) orders. Options-ORB uses
// it to detect an in-flight exit sell after a restart, so it polls that order rather
// than submitting a second sell-to-close. For an option order the Ticker is the OCC
// symbol. Implements WorkingOrdersLister.
func (b *AlpacaBroker) GetWorkingOrders(ctx context.Context) ([]WorkingOrder, error) {
	var resp []struct {
		ID         string `json:"id"`
		Symbol     string `json:"symbol"`
		AssetClass string `json:"asset_class"`
		Side       string `json:"side"`
		Status     string `json:"status"`
		Qty        string `json:"qty"`
		FilledQty  string `json:"filled_qty"`
		LimitPx    string `json:"limit_price"`
		ClientID   string `json:"client_order_id"`
	}
	const workingOrderPageLimit = 500
	if err := b.do(ctx, http.MethodGet, "/v2/orders?status=open&nested=false&limit=500", nil, &resp); err != nil {
		return nil, fmt.Errorf("alpaca GetWorkingOrders: %w", err)
	}
	// Alpaca's list endpoint has no cursor in this response shape. A full page
	// therefore cannot prove that every live order was returned. Consumers use
	// this inventory to retire dangerous residual exits, so fail closed instead
	// of silently treating the first 500 as complete.
	if len(resp) == workingOrderPageLimit {
		return nil, fmt.Errorf("alpaca GetWorkingOrders: open-order inventory reached %d-item page limit", workingOrderPageLimit)
	}
	out := make([]WorkingOrder, 0, len(resp))
	for _, o := range resp {
		qty, _ := parseFloat(o.Qty)
		filled, _ := parseFloat(o.FilledQty)
		px, _ := parseFloat(o.LimitPx)
		out = append(out, WorkingOrder{
			OrderID:      o.ID,
			TradeID:      o.ClientID,
			Ticker:       o.Symbol,
			AssetClass:   o.AssetClass,
			Side:         o.Side,
			Status:       NormalizeOrderStatus(o.Status),
			RemainingQty: qty - filled,
			Price:        px,
		})
	}
	return out, nil
}

// CancelOrder cancels an open order by broker order ID.
func (b *AlpacaBroker) CancelOrder(ctx context.Context, brokerOrderID string) error {
	return b.do(ctx, http.MethodDelete, "/v2/orders/"+brokerOrderID, nil, nil)
}

// ReplaceStopOrder updates the stop price on an existing open STOP order via
// Alpaca's PATCH /v2/orders/{order_id} endpoint. Used by the server-side
// trailing-stop manager (#322) to walk the stop up without having to cancel
// and resubmit. Alpaca returns a fresh order id on success.
func (b *AlpacaBroker) ReplaceStopOrder(ctx context.Context, brokerOrderID string, newStopPrice float64) (OrderResult, error) {
	return b.ReplaceStopOrderWithClientID(ctx, brokerOrderID, newStopPrice, "")
}

// ReplaceStopOrderWithClientID assigns the replacement order its own durable
// Alpaca client id. The predecessor remains independently reconcilable because
// Alpaca can fill it while the PATCH is in flight.
func (b *AlpacaBroker) ReplaceStopOrderWithClientID(ctx context.Context, brokerOrderID string, newStopPrice float64, clientOrderID string) (OrderResult, error) {
	body := map[string]any{
		"stop_price": fmt.Sprintf("%.4f", newStopPrice),
	}
	if clientOrderID != "" {
		body["client_order_id"] = clientOrderID
	}
	var resp struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		FilledAvgPx string `json:"filled_avg_price"`
		FilledQty   string `json:"filled_qty"`
		FilledAt    string `json:"filled_at"`
	}
	requestID, err := b.doWithRequestID(ctx, http.MethodPatch, "/v2/orders/"+brokerOrderID, body, &resp)
	if err != nil {
		return OrderResult{}, fmt.Errorf("alpaca ReplaceStopOrder: %w", err)
	}
	filledPrice, err := parseFloat(resp.FilledAvgPx)
	if err != nil {
		return OrderResult{}, fmt.Errorf("alpaca ReplaceStopOrder: parse filled_avg_price %q: %w", resp.FilledAvgPx, err)
	}
	filledQty, err := parseFloat(resp.FilledQty)
	if err != nil {
		return OrderResult{}, fmt.Errorf("alpaca ReplaceStopOrder: parse filled_qty %q: %w", resp.FilledQty, err)
	}
	filledAt, _ := time.Parse(time.RFC3339, resp.FilledAt)
	return OrderResult{
		BrokerOrderID: resp.ID,
		Status:        resp.Status,
		FilledPrice:   filledPrice,
		FilledQty:     filledQty,
		FilledAt:      filledAt,
		RequestID:     requestID,
	}, nil
}

// GetOrderStatus returns the current state of an order.
//
// Returns an error wrapping ErrOrderNotFound when Alpaca responds with 404
// (the order has been purged from the broker's lookup, typically because it
// already settled into a terminal state). Callers using errors.Is can treat
// this as "the order is effectively gone" rather than as an unconfirmed
// status.
func (b *AlpacaBroker) GetOrderStatus(ctx context.Context, brokerOrderID string) (OrderResult, error) {
	var resp struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		Qty         string `json:"qty"`
		FilledAvgPx string `json:"filled_avg_price"`
		FilledQty   string `json:"filled_qty"`
		FilledAt    string `json:"filled_at"`
		Legs        []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			Status      string `json:"status"`
			Qty         string `json:"qty"`
			FilledQty   string `json:"filled_qty"`
			FilledAvgPx string `json:"filled_avg_price"`
			FilledAt    string `json:"filled_at"`
			LimitPrice  string `json:"limit_price"`
			StopPrice   string `json:"stop_price"`
		} `json:"legs"`
	}
	if err := b.do(ctx, http.MethodGet, "/v2/orders/"+brokerOrderID+"?nested=true", nil, &resp); err != nil {
		// `do` formats 4xx responses as "alpaca METHOD path → CODE: body"
		// — sniff the 404 prefix to map onto the sentinel. Cleaner than
		// punching the status code through `do`'s signature for one
		// caller.
		if isBrokerNotFound(err) {
			return OrderResult{}, fmt.Errorf("alpaca GetOrderStatus %s: %w", brokerOrderID, ErrOrderNotFound)
		}
		return OrderResult{}, fmt.Errorf("alpaca GetOrderStatus: %w", err)
	}

	filledPrice, err := parseFloat(resp.FilledAvgPx)
	if err != nil {
		return OrderResult{}, fmt.Errorf("alpaca GetOrderStatus: parse filled_avg_price %q: %w", resp.FilledAvgPx, err)
	}
	filledQty, err := parseFloat(resp.FilledQty)
	if err != nil {
		return OrderResult{}, fmt.Errorf("alpaca GetOrderStatus: parse filled_qty %q: %w", resp.FilledQty, err)
	}
	quantity, err := parseFloat(resp.Qty)
	if err != nil {
		return OrderResult{}, fmt.Errorf("alpaca GetOrderStatus: parse qty %q: %w", resp.Qty, err)
	}
	filledAt, _ := time.Parse(time.RFC3339, resp.FilledAt)
	result := OrderResult{
		BrokerOrderID: resp.ID,
		Status:        resp.Status,
		Quantity:      quantity,
		RemainingQty:  math.Max(0, quantity-filledQty),
		FilledPrice:   filledPrice,
		FilledQty:     filledQty,
		FilledAt:      filledAt,
	}
	var tpID, slID string
	for _, leg := range resp.Legs {
		qty, qtyErr := parseFloat(leg.Qty)
		filled, filledErr := parseFloat(leg.FilledQty)
		filledPrice, filledPriceErr := parseFloat(leg.FilledAvgPx)
		limit, limitErr := parseFloat(leg.LimitPrice)
		stop, stopErr := parseFloat(leg.StopPrice)
		if qtyErr != nil || filledErr != nil || filledPriceErr != nil || limitErr != nil || stopErr != nil {
			return OrderResult{}, fmt.Errorf("alpaca GetOrderStatus: parse bracket child %s quantities/prices", leg.ID)
		}
		result.ChildOrders = append(result.ChildOrders, OrderLeg{
			BrokerOrderID: leg.ID, Type: leg.Type, Status: leg.Status,
			Quantity: qty, RemainingQty: math.Max(0, qty-filled), FilledQty: filled,
			FilledPrice: filledPrice,
			LimitPrice:  limit, StopPrice: stop,
		})
		result.ChildOrders[len(result.ChildOrders)-1].FilledAt, _ = time.Parse(time.RFC3339, leg.FilledAt)
		switch leg.Type {
		case "limit":
			tpID = leg.ID
		case "stop", "stop_limit":
			slID = leg.ID
		}
	}
	if tpID != "" && slID != "" {
		result.ChildOrderIDs = []string{tpID, slID}
	}
	return result, nil
}

func isBrokerNotFound(err error) bool {
	var requestErr *BrokerRequestError
	return errors.As(err, &requestErr) && requestErr.StatusCode == http.StatusNotFound
}

// GetPositions returns all open positions from Alpaca.
func (b *AlpacaBroker) GetPositions(ctx context.Context) ([]Position, error) {
	var resp []struct {
		Symbol       string `json:"symbol"`
		AssetClass   string `json:"asset_class"` // "us_equity" | "us_option"
		Qty          string `json:"qty"`
		AvgEntryPx   string `json:"avg_entry_price"`
		CurrentPrice string `json:"current_price"`
		MarketValue  string `json:"market_value"`
		UnrealizedPL string `json:"unrealized_pl"`
		Side         string `json:"side"`
	}
	if err := b.do(ctx, http.MethodGet, "/v2/positions", nil, &resp); err != nil {
		return nil, fmt.Errorf("alpaca GetPositions: %w", err)
	}

	out := make([]Position, 0, len(resp))
	for _, p := range resp {
		qty, err := parseFloat(p.Qty)
		if err != nil {
			return nil, fmt.Errorf("alpaca GetPositions: parse qty %q: %w", p.Qty, err)
		}
		avg, err := parseFloat(p.AvgEntryPx)
		if err != nil {
			return nil, fmt.Errorf("alpaca GetPositions: parse avg_entry_price %q: %w", p.AvgEntryPx, err)
		}
		cp, err := parseFloat(p.CurrentPrice)
		if err != nil {
			return nil, fmt.Errorf("alpaca GetPositions: parse current_price %q: %w", p.CurrentPrice, err)
		}
		mv, err := parseFloat(p.MarketValue)
		if err != nil {
			return nil, fmt.Errorf("alpaca GetPositions: parse market_value %q: %w", p.MarketValue, err)
		}
		upl, err := parseFloat(p.UnrealizedPL)
		if err != nil {
			return nil, fmt.Errorf("alpaca GetPositions: parse unrealized_pl %q: %w", p.UnrealizedPL, err)
		}
		pos := Position{
			Ticker:       p.Symbol,
			Quantity:     qty,
			AvgCostBasis: avg,
			CurrentPrice: cp,
			CurrentValue: mv,
			UnrealizedPL: upl,
			Side:         p.Side,
		}
		// Option positions carry the OCC symbol as their ticker; parse it into the
		// contract identity so options-ORB reconciliation (issue #416) can key on it.
		if p.AssetClass == "us_option" {
			if oc, err := ParseOCCSymbol(p.Symbol); err == nil {
				pos.Option = &oc
			}
		}
		out = append(out, pos)
	}
	return out, nil
}

// GetAccount returns the Alpaca account's cash balance, equity, and daily P&L.
func (b *AlpacaBroker) GetAccount(ctx context.Context) (AccountInfo, error) {
	var resp struct {
		Cash       string `json:"cash"`
		Equity     string `json:"equity"`
		LastEquity string `json:"last_equity"`
	}
	if err := b.do(ctx, http.MethodGet, "/v2/account", nil, &resp); err != nil {
		return AccountInfo{}, fmt.Errorf("alpaca GetAccount: %w", err)
	}

	cash, err := parseFloat(resp.Cash)
	if err != nil {
		return AccountInfo{}, fmt.Errorf("alpaca GetAccount: parse cash %q: %w", resp.Cash, err)
	}

	equity, err := parseFloat(resp.Equity)
	if err != nil {
		return AccountInfo{}, fmt.Errorf("alpaca GetAccount: parse equity %q: %w", resp.Equity, err)
	}

	lastEquity, err := parseFloat(resp.LastEquity)
	if err != nil {
		return AccountInfo{}, fmt.Errorf("alpaca GetAccount: parse last_equity %q: %w", resp.LastEquity, err)
	}

	// DayPnL matches the Alpaca UI's "Daily Change" (equity - last_equity).
	dayPnL := equity - lastEquity

	return AccountInfo{
		Cash:            cash,
		PortfolioEquity: equity,
		DayPnL:          dayPnL,
	}, nil
}

// ─── HTTP helper ──────────────────────────────────────────────────────────────

func (b *AlpacaBroker) do(ctx context.Context, method, path string, body any, out any) error {
	return b.doURL(ctx, method, b.baseURL+path, body, out)
}

func (b *AlpacaBroker) doWithRequestID(ctx context.Context, method, path string, body any, out any) (string, error) {
	return b.doURLWithRequestID(ctx, method, b.baseURL+path, body, out)
}

// doData issues a request against the market-data host (b.dataURL) — options
// chain snapshots live there, not on the trading base URL.
func (b *AlpacaBroker) doData(ctx context.Context, method, path string, body any, out any) error {
	return b.doURL(ctx, method, b.dataURL+path, body, out)
}

// doURL is the shared HTTP core, parameterized by full URL so trading-host and
// data-host requests reuse the same auth, error handling, and body decoding.
func (b *AlpacaBroker) doURL(ctx context.Context, method, url string, body any, out any) error {
	_, err := b.doURLWithRequestID(ctx, method, url, body, out)
	return err
}

func (b *AlpacaBroker) doURLWithRequestID(ctx context.Context, method, url string, body any, out any) (string, error) {
	var bodyReader io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("marshal: %w", err)
		}
		bodyReader = bytes.NewReader(bs)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return "", err
	}
	req.Header.Set("APCA-API-KEY-ID", b.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", b.secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB – equity history can exceed 8 KiB
	if resp.StatusCode >= 400 {
		return resp.Header.Get("X-Request-ID"), &BrokerRequestError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("alpaca %s %s: %s", method, url, bytes.TrimSpace(respBytes))}
	}
	if out != nil && len(respBytes) > 0 {
		return resp.Header.Get("X-Request-ID"), json.Unmarshal(respBytes, out)
	}
	return resp.Header.Get("X-Request-ID"), nil
}
