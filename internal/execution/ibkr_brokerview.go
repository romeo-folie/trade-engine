package execution

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ibkrFloat decodes a numeric field that the IBKR Client Portal may serialize
// either as a JSON number (1.1750) or as a quoted string ("1.1750"). The
// /iserver/account/trades endpoint returns size and price as strings, which a
// plain float64 field cannot decode — this previously failed every ORB exit
// reconcile with "cannot unmarshal string into Go struct field .price of type
// float64", leaving filled trades stranded with no exit price or PnL.
type ibkrFloat float64

func (f *ibkrFloat) UnmarshalJSON(data []byte) error {
	s := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("ibkrFloat: parse %q: %w", s, err)
	}
	*f = ibkrFloat(v)
	return nil
}

// ibkrID decodes an order identifier that the IBKR Client Portal may serialize
// as a JSON number (1234567) or a quoted string ("1234567"). The
// /iserver/account/orders endpoint has been observed to use both; String() always
// renders it without a decimal point so it round-trips into cancel-by-id calls.
type ibkrID int64

func (id *ibkrID) UnmarshalJSON(data []byte) error {
	s := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if s == "" || s == "null" {
		*id = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("ibkrID: parse %q: %w", s, err)
	}
	*id = ibkrID(v)
	return nil
}

func (id ibkrID) String() string { return strconv.FormatInt(int64(id), 10) }

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
// returns them in the BrokerView wire shape. Uses the /portfolio positions
// route (the /iserver/account variant 404s — see GetPositions) and primes the
// portfolio session first.
func (b *IBKRBroker) PositionsSnapshot(ctx context.Context) ([]BrokerPosition, error) {
	b.primePortfolio(ctx)
	var resp []struct {
		Conid        int     `json:"conid"`
		Position     float64 `json:"position"`
		AvgCost      float64 `json:"avgCost"`
		MktPrice     float64 `json:"mktPrice"`
		MktValue     float64 `json:"mktValue"`
		UnrealizedPL float64 `json:"unrealizedPnl"`
	}
	if err := b.do(ctx, http.MethodGet, "/v1/api/portfolio/"+b.accountID+"/positions/0", nil, &resp); err != nil {
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
	Symbol   string // human-readable when conid is resolved (e.g. "EUR/USD"); otherwise the raw API symbol
	Side     string // "buy" | "sell"
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
		OrderID   int64     `json:"orderId"`
		Symbol    string    `json:"symbol"`
		Side      string    `json:"side"` // "B" | "S"
		Size      ibkrFloat `json:"size"`
		Price     ibkrFloat `json:"price"`
		Status    string    `json:"status"`
		TradeTime string    `json:"trade_time"` // "YYYYMMDD-HH:MM:SS" UTC
		Conid     int       `json:"conid"`
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
			Qty:      float64(o.Size),
			Price:    float64(o.Price),
			FilledAt: t,
		})
	}
	return out, nil
}

// GetWorkingOrders returns the account's live (still-working) orders from
// /iserver/account/orders. Only orders that are not terminal (i.e. normalize to
// pending/partial) are returned — filled, cancelled and inactive rows are dropped.
// The ORB time-exit uses this to locate and cancel a filled trade's remaining
// bracket legs by symbol when their child order IDs were never captured.
func (b *IBKRBroker) GetWorkingOrders(ctx context.Context) ([]WorkingOrder, error) {
	// The Client Portal returns {"orders": [...], "snapshot": bool}. A bare array
	// is also tolerated in case the gateway shape changes.
	var wrapped struct {
		Orders []ibkrOrderRow `json:"orders"`
	}
	if err := b.do(ctx, http.MethodGet, "/v1/api/iserver/account/orders", nil, &wrapped); err != nil {
		return nil, fmt.Errorf("ibkr GetWorkingOrders: %w", err)
	}

	out := make([]WorkingOrder, 0, len(wrapped.Orders))
	for _, o := range wrapped.Orders {
		status := NormalizeIBKROrderStatus(o.Status)
		// Keep only working orders; terminal states are not cancellable legs.
		if status != "pending" && status != "partial" {
			continue
		}
		conidStr := ""
		if o.Conid != 0 {
			conidStr = fmt.Sprintf("%d", o.Conid)
		}
		side := "buy"
		if strings.EqualFold(o.Side, "S") || strings.EqualFold(o.Side, "SELL") {
			side = "sell"
		}
		ticker := o.Ticker
		if resolved := b.reverseConid(conidStr); conidStr != "" && resolved != conidStr {
			ticker = resolved
		}
		out = append(out, WorkingOrder{
			OrderID:      o.OrderID.String(),
			TradeID:      o.OrderRef,
			Ticker:       ticker,
			Conid:        conidStr,
			Side:         side,
			Status:       status,
			RemainingQty: float64(o.RemainingQty),
			Price:        float64(o.Price),
			StopPrice:    float64(o.StopPrice),
		})
	}
	return out, nil
}

// ibkrOrderRow models one row of /iserver/account/orders. The gateway encodes
// orderId as a number but ticker/side/status as strings; remainingQuantity and the
// price fields may be string- or number-encoded, so they reuse ibkrFloat (which
// tolerates both). price/stop_price let a caller match an order to a known
// take-profit/stop-loss leg; both are 0 when the gateway omits them.
type ibkrOrderRow struct {
	OrderID      ibkrID    `json:"orderId"`
	OrderRef     string    `json:"order_ref"`
	Conid        int       `json:"conid"`
	Ticker       string    `json:"ticker"`
	Side         string    `json:"side"`   // "BUY" | "SELL" (sometimes "B" | "S")
	Status       string    `json:"status"` // "Submitted" | "PreSubmitted" | "Filled" | ...
	RemainingQty ibkrFloat `json:"remainingQuantity"`
	Price        ibkrFloat `json:"price"`
	StopPrice    ibkrFloat `json:"stop_price"`
}

// RecentFills fetches today's filled orders from /iserver/account/trades.
func (b *IBKRBroker) RecentFills(ctx context.Context) ([]BrokerOrder, error) {
	var resp []struct {
		OrderID   int64     `json:"orderId"`
		Symbol    string    `json:"symbol"`
		Side      string    `json:"side"` // "B" | "S"
		Size      ibkrFloat `json:"size"`
		Price     ibkrFloat `json:"price"`
		Status    string    `json:"status"`
		TradeTime string    `json:"trade_time"` // e.g. "20240101-12:00:00"
		Conid     int       `json:"conid"`
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
			Qty:           float64(o.Size),
			FilledQty:     float64(o.Size),
			FilledAvgPx:   float64(o.Price),
			Status:        "filled",
			FilledAt:      tradeTime,
		})
	}
	return out, nil
}
