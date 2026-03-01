package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const kalshiTradingBase = "https://trading-api.kalshi.com/trade-api/v2"

// KalshiExecutor places real orders on Kalshi via their v2 REST trading API.
// Requires a valid bearer API key set via config (markets.predictions.platforms.kalshi.api_key_env).
type KalshiExecutor struct {
	httpClient *http.Client
	apiKey     string
}

// NewKalshiExecutor creates a KalshiExecutor.
func NewKalshiExecutor(apiKey string) *KalshiExecutor {
	return &KalshiExecutor{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// PlaceOrder submits a limit order to Kalshi.
// OrderRequest conventions for Kalshi:
//   - Ticker     = Kalshi market ticker (e.g. "INXD-23DEC29-B4500")
//   - Side       = "buy"  → buy YES contracts;  "sell" → buy NO contracts
//   - LimitPrice = Yes-price in [0, 1]; converted to cents (0-99) internally
//   - Quantity   = number of contracts (integer)
func (k *KalshiExecutor) PlaceOrder(ctx context.Context, req OrderRequest) (OrderResult, error) {
	if k.apiKey == "" {
		return OrderResult{}, fmt.Errorf("kalshi executor: api key not configured")
	}

	side := "yes"
	yesPrice := int(req.LimitPrice * 100)
	if req.Side == OrderSideSell {
		side = "no"
		yesPrice = 100 - yesPrice
	}

	body := map[string]interface{}{
		"ticker":          req.Ticker,
		"client_order_id": fmt.Sprintf("te-%s", req.TradeID),
		"type":            "limit",
		"side":            side,
		"count":           int(req.Quantity),
		"yes_price":       yesPrice,
	}

	var resp kalshiOrderResponse
	if err := k.post(ctx, kalshiTradingBase+"/portfolio/orders", body, &resp); err != nil {
		return OrderResult{}, fmt.Errorf("kalshi PlaceOrder: %w", err)
	}

	status := "pending"
	if resp.Order.Status == "filled" || resp.Order.Status == "executed" {
		status = "filled"
	}
	return OrderResult{
		BrokerOrderID: resp.Order.OrderID,
		Status:        status,
		FilledPrice:   float64(resp.Order.YesPrice) / 100.0,
		FilledQty:     float64(resp.Order.CountFilled),
	}, nil
}

// CancelOrder cancels an open Kalshi order.
func (k *KalshiExecutor) CancelOrder(ctx context.Context, brokerOrderID string) error {
	if k.apiKey == "" {
		return fmt.Errorf("kalshi executor: api key not configured")
	}
	endpoint := fmt.Sprintf("%s/portfolio/orders/%s", kalshiTradingBase, brokerOrderID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.apiKey)

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		errBody := string(body)
		if len(errBody) > 200 {
			errBody = errBody[:200] + "…"
		}
		return fmt.Errorf("kalshi CancelOrder HTTP %d: %s", resp.StatusCode, errBody)
	}
	return nil
}

// GetOrderStatus returns the current status of an order.
func (k *KalshiExecutor) GetOrderStatus(ctx context.Context, brokerOrderID string) (OrderResult, error) {
	if k.apiKey == "" {
		return OrderResult{}, fmt.Errorf("kalshi executor: api key not configured")
	}
	endpoint := fmt.Sprintf("%s/portfolio/orders/%s", kalshiTradingBase, brokerOrderID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return OrderResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+k.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return OrderResult{}, err
	}
	defer resp.Body.Close()

	var result kalshiOrderResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return OrderResult{}, err
	}

	status := "pending"
	if result.Order.Status == "filled" || result.Order.Status == "executed" {
		status = "filled"
	} else if result.Order.Status == "canceled" {
		status = "cancelled"
	}
	return OrderResult{
		BrokerOrderID: result.Order.OrderID,
		Status:        status,
		FilledPrice:   float64(result.Order.YesPrice) / 100.0,
		FilledQty:     float64(result.Order.CountFilled),
	}, nil
}

// GetPositions returns all current Kalshi positions.
func (k *KalshiExecutor) GetPositions(ctx context.Context) ([]Position, error) {
	if k.apiKey == "" {
		return nil, fmt.Errorf("kalshi executor: api key not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		kalshiTradingBase+"/portfolio/positions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var page kalshiPositionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, err
	}

	positions := make([]Position, 0, len(page.MarketPositions))
	for _, mp := range page.MarketPositions {
		qty := float64(mp.Position) // net YES position
		if qty == 0 {
			continue
		}
		avgCost := float64(mp.MarketExposure) / qty / 100.0
		positions = append(positions, Position{
			Ticker:       mp.Ticker,
			Quantity:     qty,
			AvgCostBasis: avgCost,
			CurrentValue: qty * float64(mp.LastPrice) / 100.0,
		})
	}
	return positions, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

func (k *KalshiExecutor) post(ctx context.Context, endpoint string, body interface{}, out interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		errBody := string(respBody)
		if len(errBody) > 200 {
			errBody = errBody[:200] + "…"
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, errBody)
	}
	return json.Unmarshal(respBody, out)
}

// ── Kalshi API types ──────────────────────────────────────────────────────────

type kalshiOrderResponse struct {
	Order struct {
		OrderID     string `json:"order_id"`
		Status      string `json:"status"`
		YesPrice    int    `json:"yes_price"`
		CountFilled int    `json:"count_filled"`
	} `json:"order"`
}

type kalshiPositionsResponse struct {
	MarketPositions []struct {
		Ticker         string `json:"ticker"`
		Position       int    `json:"position"`         // net YES position
		MarketExposure int    `json:"market_exposure"`  // cents invested
		LastPrice      int    `json:"last_price"`       // cents
	} `json:"market_positions"`
}
