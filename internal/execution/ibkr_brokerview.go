package execution

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// IBKRBroker satisfies BrokerView. IBKR Client Portal does not expose a
// portfolio-history endpoint comparable to Alpaca's, so EquityCurve returns
// ErrBrokerUnavailable and the web layer falls back to our DB series.

// AccountSnapshot fetches account summary from /v1/api/portfolio/:id/summary
// and translates it into the BrokerView wire shape.
func (b *IBKRBroker) AccountSnapshot(ctx context.Context) (BrokerAccount, error) {
	var resp struct {
		TotalCashValue struct {
			Amount   float64 `json:"amount"`
			Currency string  `json:"currency"`
		} `json:"totalcashvalue"`
		NetLiquidation struct {
			Amount float64 `json:"amount"`
		} `json:"netliquidation"`
		BuyingPower struct {
			Amount float64 `json:"amount"`
		} `json:"buyingpower"`
	}
	if err := b.do(ctx, http.MethodGet, "/v1/api/portfolio/"+b.accountID+"/summary", nil, &resp); err != nil {
		return BrokerAccount{}, fmt.Errorf("ibkr AccountSnapshot: %w", err)
	}

	return BrokerAccount{
		AccountID:   b.accountID,
		Equity:      resp.NetLiquidation.Amount,
		LastEquity:  0, // IBKR summary does not expose prior-day close directly
		Cash:        resp.TotalCashValue.Amount,
		BuyingPower: resp.BuyingPower.Amount,
		Currency:    resp.TotalCashValue.Currency,
		FetchedAt:   time.Now().UTC(),
	}, nil
}

// PositionsSnapshot fetches open positions from the IBKR Client Portal and
// returns them in the BrokerView wire shape.
func (b *IBKRBroker) PositionsSnapshot(ctx context.Context) ([]BrokerPosition, error) {
	var resp []struct {
		Conid        int     `json:"conid"`
		Position     float64 `json:"position"`
		AvgCost      float64 `json:"avgCost"`
		MktPrice     float64 `json:"mktPrice"`
		MktValue     float64 `json:"mktValue"`
		UnrealizedPL float64 `json:"unrealizedPnl"`
	}
	if err := b.do(ctx, http.MethodGet, "/v1/api/iserver/account/"+b.accountID+"/positions/0", nil, &resp); err != nil {
		return nil, fmt.Errorf("ibkr PositionsSnapshot: %w", err)
	}

	out := make([]BrokerPosition, 0, len(resp))
	for _, p := range resp {
		conidStr := fmt.Sprintf("%d", p.Conid)
		symbol := b.reverseConid(conidStr)
		side := "long"
		if p.Position < 0 {
			side = "short"
		}
		out = append(out, BrokerPosition{
			Symbol:        symbol,
			Quantity:      p.Position,
			AvgEntryPrice: p.AvgCost,
			CurrentPrice:  p.MktPrice,
			MarketValue:   p.MktValue,
			UnrealizedPL:  p.UnrealizedPL,
			Side:          side,
		})
	}
	return out, nil
}

// EquityCurve is unavailable for IBKR — the Client Portal API does not expose
// a portfolio history endpoint equivalent to Alpaca's. The web layer falls back
// to our TradeEngine DB series for the overlay chart.
func (b *IBKRBroker) EquityCurve(_ context.Context, _, _ string) (EquityHistory, error) {
	return EquityHistory{}, ErrBrokerUnavailable
}

// IBKRTradeRecord is a single execution (fill) from the IBKR trades endpoint.
// Used by the ORB exit reconciler to match bracket exit fills back to DB records
// without relying on child order IDs that may not have been captured at placement.
type IBKRTradeRecord struct {
	OrderID  string
	Conid    string
	Symbol   string    // human-readable when conid is resolved (e.g. "EUR/USD"); otherwise the raw API symbol field
	Side     string    // "buy" | "sell"
	Qty      float64
	Price    float64
	FilledAt time.Time
}

// FetchTradeRecords returns filled executions from /iserver/account/trades for the
// last N days (clamped to 1–7, the gateway's maximum). Unlike RecentFills it does
// not filter to today, making it suitable for multi-day exit reconciliation.
func (b *IBKRBroker) FetchTradeRecords(ctx context.Context, days int) ([]IBKRTradeRecord, error) {
	if days < 1 {
		days = 1
	}
	if days > 7 {
		days = 7
	}
	var resp []struct {
		OrderID   int64   `json:"orderId"`
		Symbol    string  `json:"symbol"`
		Side      string  `json:"side"` // "B" | "S"
		Size      float64 `json:"size"`
		Price     float64 `json:"price"`
		Status    string  `json:"status"`
		TradeTime string  `json:"trade_time"` // "YYYYMMDD-HH:MM:SS" UTC
		Conid     int     `json:"conid"`
	}
	path := fmt.Sprintf("/v1/api/iserver/account/trades?days=%d", days)
	if err := b.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("ibkr FetchTradeRecords: %w", err)
	}

	const ibkrTimeFmt = "20060102-15:04:05"
	out := make([]IBKRTradeRecord, 0, len(resp))
	for _, o := range resp {
		if NormalizeIBKROrderStatus(o.Status) != "filled" {
			continue
		}
		t, err := time.ParseInLocation(ibkrTimeFmt, o.TradeTime, time.UTC)
		if err != nil {
			continue
		}
		side := "buy"
		if o.Side == "S" {
			side = "sell"
		}
		conidStr := fmt.Sprintf("%d", o.Conid)
		symbol := o.Symbol
		if resolved := b.reverseConid(conidStr); resolved != conidStr {
			symbol = resolved
		}
		out = append(out, IBKRTradeRecord{
			OrderID:  fmt.Sprintf("%d", o.OrderID),
			Conid:    conidStr,
			Symbol:   symbol,
			Side:     side,
			Qty:      o.Size,
			Price:    o.Price,
			FilledAt: t,
		})
	}
	return out, nil
}

// RecentFills fetches today's filled orders from /iserver/account/trades.
func (b *IBKRBroker) RecentFills(ctx context.Context) ([]BrokerOrder, error) {
	var resp []struct {
		OrderID     int64   `json:"orderId"`
		Symbol      string  `json:"symbol"`
		Side        string  `json:"side"` // "B" | "S"
		Size        float64 `json:"size"`
		Price       float64 `json:"price"`
		Status      string  `json:"status"`
		TradeTime   string  `json:"trade_time"` // e.g. "20240101-12:00:00"
		Conid       int     `json:"conid"`
	}
	if err := b.do(ctx, http.MethodGet, "/v1/api/iserver/account/trades", nil, &resp); err != nil {
		return nil, fmt.Errorf("ibkr RecentFills: %w", err)
	}

	// IBKR trade_time format from Client Portal: "YYYYMMDD-HH:MM:SS"
	const ibkrTimeFmt = "20060102-15:04:05"
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	out := make([]BrokerOrder, 0, len(resp))
	for _, o := range resp {
		tradeTime, err := time.ParseInLocation(ibkrTimeFmt, o.TradeTime, time.UTC)
		if err != nil || tradeTime.Before(midnight) {
			continue // skip non-today or unparseable rows
		}
		side := "buy"
		if o.Side == "S" {
			side = "sell"
		}
		conidStr := fmt.Sprintf("%d", o.Conid)
		symbol := o.Symbol
		if resolved := b.reverseConid(conidStr); resolved != conidStr {
			symbol = resolved
		}
		out = append(out, BrokerOrder{
			BrokerOrderID: fmt.Sprintf("%d", o.OrderID),
			Symbol:        symbol,
			Side:          side,
			Qty:           o.Size,
			FilledQty:     o.Size,
			FilledAvgPx:   o.Price,
			Status:        "filled",
			FilledAt:      tradeTime,
		})
	}
	return out, nil
}
