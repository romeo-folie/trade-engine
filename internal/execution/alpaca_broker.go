package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// AlpacaBroker implements Executor against the Alpaca paper (or live) trading API.
type AlpacaBroker struct {
	apiKey     string
	secret     string
	baseURL    string
	httpClient *http.Client
}

// NewAlpacaBroker creates an Alpaca broker adapter from market config.
// Uses the paper endpoint when cfg.Paper is true.
func NewAlpacaBroker(cfg config.StocksConfig) *AlpacaBroker {
	return &AlpacaBroker{
		apiKey:     cfg.APIKey,
		secret:     cfg.Secret,
		baseURL:    cfg.BaseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// PlaceOrder submits a market or limit order to Alpaca.
func (b *AlpacaBroker) PlaceOrder(ctx context.Context, req OrderRequest) (OrderResult, error) {
	body := map[string]any{
		"symbol":        req.Ticker,
		"qty":           fmt.Sprintf("%.6f", req.Quantity),
		"side":          string(req.Side),
		"type":          string(req.Type),
		"time_in_force": "day",
	}
	if req.LimitPrice > 0 {
		body["limit_price"] = fmt.Sprintf("%.4f", req.LimitPrice)
	}
	if req.StopPrice > 0 {
		body["stop_price"] = fmt.Sprintf("%.4f", req.StopPrice)
	}

	var resp struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		FilledAvgPx string `json:"filled_avg_price"`
		FilledQty   string `json:"filled_qty"`
	}
	if err := b.do(ctx, http.MethodPost, "/v2/orders", body, &resp); err != nil {
		return OrderResult{}, fmt.Errorf("alpaca PlaceOrder: %w", err)
	}

	var filledPrice, filledQty float64
	fmt.Sscanf(resp.FilledAvgPx, "%f", &filledPrice)
	fmt.Sscanf(resp.FilledQty, "%f", &filledQty)

	return OrderResult{
		BrokerOrderID: resp.ID,
		Status:        resp.Status,
		FilledPrice:   filledPrice,
		FilledQty:     filledQty,
	}, nil
}

// CancelOrder cancels an open order by broker order ID.
func (b *AlpacaBroker) CancelOrder(ctx context.Context, brokerOrderID string) error {
	return b.do(ctx, http.MethodDelete, "/v2/orders/"+brokerOrderID, nil, nil)
}

// GetOrderStatus returns the current state of an order.
func (b *AlpacaBroker) GetOrderStatus(ctx context.Context, brokerOrderID string) (OrderResult, error) {
	var resp struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		FilledAvgPx string `json:"filled_avg_price"`
		FilledQty   string `json:"filled_qty"`
	}
	if err := b.do(ctx, http.MethodGet, "/v2/orders/"+brokerOrderID, nil, &resp); err != nil {
		return OrderResult{}, fmt.Errorf("alpaca GetOrderStatus: %w", err)
	}

	var filledPrice, filledQty float64
	fmt.Sscanf(resp.FilledAvgPx, "%f", &filledPrice)
	fmt.Sscanf(resp.FilledQty, "%f", &filledQty)

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
		Qty          string `json:"qty"`
		AvgEntryPx   string `json:"avg_entry_price"`
		MarketValue  string `json:"market_value"`
		UnrealizedPL string `json:"unrealized_pl"`
	}
	if err := b.do(ctx, http.MethodGet, "/v2/positions", nil, &resp); err != nil {
		return nil, fmt.Errorf("alpaca GetPositions: %w", err)
	}

	out := make([]Position, 0, len(resp))
	for _, p := range resp {
		var qty, avg, mv, upl float64
		fmt.Sscanf(p.Qty, "%f", &qty)
		fmt.Sscanf(p.AvgEntryPx, "%f", &avg)
		fmt.Sscanf(p.MarketValue, "%f", &mv)
		fmt.Sscanf(p.UnrealizedPL, "%f", &upl)
		out = append(out, Position{
			Ticker:       p.Symbol,
			Quantity:     qty,
			AvgCostBasis: avg,
			CurrentValue: mv,
			UnrealizedPL: upl,
		})
	}
	return out, nil
}

// ─── HTTP helper ──────────────────────────────────────────────────────────────

func (b *AlpacaBroker) do(ctx context.Context, method, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		bodyReader = bytes.NewReader(bs)
	}

	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, bodyReader)
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

	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("alpaca %s %s → %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(respBytes))
	}
	if out != nil && len(respBytes) > 0 {
		return json.Unmarshal(respBytes, out)
	}
	return nil
}
