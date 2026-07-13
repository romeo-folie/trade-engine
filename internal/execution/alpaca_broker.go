package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
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
	// Market orders must not include price fields — Alpaca returns 422 if they are present.
	if req.Type != OrderTypeMarket {
		if req.LimitPrice > 0 {
			body["limit_price"] = fmt.Sprintf("%.4f", req.LimitPrice)
		}
		if req.StopPrice > 0 {
			body["stop_price"] = fmt.Sprintf("%.4f", req.StopPrice)
		}
	}
	if req.OrderClass == "bracket" {
		body["order_class"] = "bracket"
		body["take_profit"] = map[string]string{
			"limit_price": fmt.Sprintf("%.4f", req.TakeProfitPrice),
		}
		body["stop_loss"] = map[string]string{
			"stop_price": fmt.Sprintf("%.4f", req.StopLossPrice),
		}
	}

	var resp struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		FilledAvgPx string `json:"filled_avg_price"`
		FilledQty   string `json:"filled_qty"`
		Legs        []struct {
			ID   string `json:"id"`
			Type string `json:"type"` // "limit" (take-profit) | "stop" (stop-loss)
		} `json:"legs"`
	}
	if err := b.do(ctx, http.MethodPost, "/v2/orders", body, &resp); err != nil {
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

	result := OrderResult{
		BrokerOrderID: resp.ID,
		Status:        resp.Status,
		FilledPrice:   filledPrice,
		FilledQty:     filledQty,
	}
	// Bracket child legs, ordered [take-profit, stop-loss] per the
	// ChildOrderIDs contract. Identified by leg type, not array position —
	// Alpaca does not document leg ordering.
	if len(resp.Legs) > 0 {
		var tpID, slID string
		for _, leg := range resp.Legs {
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

// GetWorkingOrders lists Alpaca's currently-open (working) orders. Options-ORB uses
// it to detect an in-flight exit sell after a restart, so it polls that order rather
// than submitting a second sell-to-close. For an option order the Ticker is the OCC
// symbol. Implements WorkingOrdersLister.
func (b *AlpacaBroker) GetWorkingOrders(ctx context.Context) ([]WorkingOrder, error) {
	var resp []struct {
		ID        string `json:"id"`
		Symbol    string `json:"symbol"`
		Side      string `json:"side"`
		Status    string `json:"status"`
		Qty       string `json:"qty"`
		FilledQty string `json:"filled_qty"`
		LimitPx   string `json:"limit_price"`
	}
	if err := b.do(ctx, http.MethodGet, "/v2/orders?status=open&nested=false&limit=500", nil, &resp); err != nil {
		return nil, fmt.Errorf("alpaca GetWorkingOrders: %w", err)
	}
	out := make([]WorkingOrder, 0, len(resp))
	for _, o := range resp {
		qty, _ := parseFloat(o.Qty)
		filled, _ := parseFloat(o.FilledQty)
		px, _ := parseFloat(o.LimitPx)
		out = append(out, WorkingOrder{
			OrderID:      o.ID,
			Ticker:       o.Symbol,
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
	body := map[string]any{
		"stop_price": fmt.Sprintf("%.4f", newStopPrice),
	}
	var resp struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		FilledAvgPx string `json:"filled_avg_price"`
		FilledQty   string `json:"filled_qty"`
	}
	if err := b.do(ctx, http.MethodPatch, "/v2/orders/"+brokerOrderID, body, &resp); err != nil {
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
	return OrderResult{
		BrokerOrderID: resp.ID,
		Status:        resp.Status,
		FilledPrice:   filledPrice,
		FilledQty:     filledQty,
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
		FilledAvgPx string `json:"filled_avg_price"`
		FilledQty   string `json:"filled_qty"`
	}
	if err := b.do(ctx, http.MethodGet, "/v2/orders/"+brokerOrderID, nil, &resp); err != nil {
		// `do` formats 4xx responses as "alpaca METHOD path → CODE: body"
		// — sniff the 404 prefix to map onto the sentinel. Cleaner than
		// punching the status code through `do`'s signature for one
		// caller.
		if strings.Contains(err.Error(), "→ 404") {
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

	return OrderResult{
		BrokerOrderID: resp.ID,
		Status:        resp.Status,
		FilledPrice:   filledPrice,
		FilledQty:     filledQty,
	}, nil
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

// doData issues a request against the market-data host (b.dataURL) — options
// chain snapshots live there, not on the trading base URL.
func (b *AlpacaBroker) doData(ctx context.Context, method, path string, body any, out any) error {
	return b.doURL(ctx, method, b.dataURL+path, body, out)
}

// doURL is the shared HTTP core, parameterized by full URL so trading-host and
// data-host requests reuse the same auth, error handling, and body decoding.
func (b *AlpacaBroker) doURL(ctx context.Context, method, url string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		bodyReader = bytes.NewReader(bs)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("APCA-API-KEY-ID", b.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", b.secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB – equity history can exceed 8 KiB
	if resp.StatusCode >= 400 {
		return fmt.Errorf("alpaca %s %s → %d: %s", method, url, resp.StatusCode, bytes.TrimSpace(respBytes))
	}
	if out != nil && len(respBytes) > 0 {
		return json.Unmarshal(respBytes, out)
	}
	return nil
}
