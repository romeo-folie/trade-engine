package execution

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// AlpacaBroker satisfies BrokerView. Method names are intentionally distinct
// from the Executor methods (GetAccount/GetPositions) so signatures don't
// collide — see brokerview.go for the interface contract.

// AccountSnapshot fetches the canonical account state from /v2/account.
func (b *AlpacaBroker) AccountSnapshot(ctx context.Context) (BrokerAccount, error) {
	var resp struct {
		ID          string `json:"id"`
		Equity      string `json:"equity"`
		LastEquity  string `json:"last_equity"`
		Cash        string `json:"cash"`
		BuyingPower string `json:"buying_power"`
		Currency    string `json:"currency"`
	}
	if err := b.do(ctx, http.MethodGet, "/v2/account", nil, &resp); err != nil {
		return BrokerAccount{}, fmt.Errorf("alpaca AccountSnapshot: %w", err)
	}

	parse := func(label, s string) (float64, error) {
		v, err := parseFloat(s)
		if err != nil {
			return 0, fmt.Errorf("alpaca AccountSnapshot: parse %s %q: %w", label, s, err)
		}
		return v, nil
	}

	equity, err := parse("equity", resp.Equity)
	if err != nil {
		return BrokerAccount{}, err
	}
	lastEquity, err := parse("last_equity", resp.LastEquity)
	if err != nil {
		return BrokerAccount{}, err
	}
	cash, err := parse("cash", resp.Cash)
	if err != nil {
		return BrokerAccount{}, err
	}
	buyingPower, err := parse("buying_power", resp.BuyingPower)
	if err != nil {
		return BrokerAccount{}, err
	}

	return BrokerAccount{
		AccountID:   resp.ID,
		Equity:      equity,
		LastEquity:  lastEquity,
		Cash:        cash,
		BuyingPower: buyingPower,
		Currency:    resp.Currency,
		FetchedAt:   time.Now().UTC(),
	}, nil
}

// PositionsSnapshot fetches all open positions from /v2/positions and returns
// them in BrokerView's wire shape (distinct from execution.Position which is
// shared with the engine's Executor surface).
func (b *AlpacaBroker) PositionsSnapshot(ctx context.Context) ([]BrokerPosition, error) {
	var resp []struct {
		Symbol       string `json:"symbol"`
		Qty          string `json:"qty"`
		AvgEntryPx   string `json:"avg_entry_price"`
		CurrentPrice string `json:"current_price"`
		MarketValue  string `json:"market_value"`
		UnrealizedPL string `json:"unrealized_pl"`
		Side         string `json:"side"`
	}
	if err := b.do(ctx, http.MethodGet, "/v2/positions", nil, &resp); err != nil {
		return nil, fmt.Errorf("alpaca PositionsSnapshot: %w", err)
	}

	out := make([]BrokerPosition, 0, len(resp))
	for _, p := range resp {
		qty, err := parseFloat(p.Qty)
		if err != nil {
			return nil, fmt.Errorf("alpaca PositionsSnapshot: parse qty %q: %w", p.Qty, err)
		}
		avg, err := parseFloat(p.AvgEntryPx)
		if err != nil {
			return nil, fmt.Errorf("alpaca PositionsSnapshot: parse avg_entry_price %q: %w", p.AvgEntryPx, err)
		}
		cp, err := parseFloat(p.CurrentPrice)
		if err != nil {
			return nil, fmt.Errorf("alpaca PositionsSnapshot: parse current_price %q: %w", p.CurrentPrice, err)
		}
		mv, err := parseFloat(p.MarketValue)
		if err != nil {
			return nil, fmt.Errorf("alpaca PositionsSnapshot: parse market_value %q: %w", p.MarketValue, err)
		}
		upl, err := parseFloat(p.UnrealizedPL)
		if err != nil {
			return nil, fmt.Errorf("alpaca PositionsSnapshot: parse unrealized_pl %q: %w", p.UnrealizedPL, err)
		}
		out = append(out, BrokerPosition{
			Symbol:        p.Symbol,
			Quantity:      qty,
			AvgEntryPrice: avg,
			CurrentPrice:  cp,
			MarketValue:   mv,
			UnrealizedPL:  upl,
			Side:          p.Side,
		})
	}
	return out, nil
}

// EquityCurve fetches /v2/account/portfolio/history. period/timeframe follow
// Alpaca's conventions ("1D"/"1W"/"1M"/"3M"/"1A"/"all" and "5Min"/"15Min"/
// "1H"/"1D"). Empty values fall back to Alpaca's defaults.
func (b *AlpacaBroker) EquityCurve(ctx context.Context, period, timeframe string) (EquityHistory, error) {
	q := url.Values{}
	if period != "" {
		q.Set("period", period)
	}
	if timeframe != "" {
		q.Set("timeframe", timeframe)
	}
	q.Set("extended_hours", "true")
	path := "/v2/account/portfolio/history?" + q.Encode()

	var resp struct {
		Timestamp []int64   `json:"timestamp"`
		Equity    []float64 `json:"equity"`
		BaseValue float64   `json:"base_value"`
		Timeframe string    `json:"timeframe"`
	}
	if err := b.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return EquityHistory{}, fmt.Errorf("alpaca EquityCurve: %w", err)
	}

	if len(resp.Timestamp) != len(resp.Equity) {
		return EquityHistory{}, fmt.Errorf("alpaca EquityCurve: timestamp/equity length mismatch (%d vs %d)", len(resp.Timestamp), len(resp.Equity))
	}

	points := make([]EquityPoint, 0, len(resp.Timestamp))
	for i, ts := range resp.Timestamp {
		// Alpaca returns nulls for gaps as 0 in the equity array; preserve them
		// so the frontend can render them as gaps if desired.
		points = append(points, EquityPoint{
			Time:   time.Unix(ts, 0).UTC(),
			Equity: resp.Equity[i],
		})
	}

	return EquityHistory{
		Period:    period,
		Timeframe: resp.Timeframe,
		BaseValue: resp.BaseValue,
		Points:    points,
		FetchedAt: time.Now().UTC(),
	}, nil
}

// RecentFills returns orders that filled today, most-recent first. Uses
// /v2/orders?status=closed&after=<midnight UTC>.
func (b *AlpacaBroker) RecentFills(ctx context.Context) ([]BrokerOrder, error) {
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	q := url.Values{}
	q.Set("status", "closed")
	q.Set("after", midnight.Format(time.RFC3339))
	q.Set("direction", "desc")
	q.Set("limit", "500")
	q.Set("nested", "false")
	path := "/v2/orders?" + q.Encode()

	var resp []struct {
		ID          string `json:"id"`
		Symbol      string `json:"symbol"`
		Side        string `json:"side"`
		Qty         string `json:"qty"`
		FilledQty   string `json:"filled_qty"`
		FilledAvgPx string `json:"filled_avg_price"`
		Status      string `json:"status"`
		SubmittedAt string `json:"submitted_at"`
		FilledAt    string `json:"filled_at"`
	}
	if err := b.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("alpaca RecentFills: %w", err)
	}

	out := make([]BrokerOrder, 0, len(resp))
	for _, o := range resp {
		// Skip orders that never filled — caller asked for fills.
		if o.Status != "filled" && o.Status != "partially_filled" {
			continue
		}
		qty, err := parseFloat(o.Qty)
		if err != nil {
			return nil, fmt.Errorf("alpaca RecentFills: parse qty %q: %w", o.Qty, err)
		}
		filledQty, err := parseFloat(o.FilledQty)
		if err != nil {
			return nil, fmt.Errorf("alpaca RecentFills: parse filled_qty %q: %w", o.FilledQty, err)
		}
		filledPx, err := parseFloat(o.FilledAvgPx)
		if err != nil {
			return nil, fmt.Errorf("alpaca RecentFills: parse filled_avg_price %q: %w", o.FilledAvgPx, err)
		}
		// Timestamps are RFC3339; tolerate empty.
		submittedAt, _ := time.Parse(time.RFC3339, o.SubmittedAt)
		filledAt, _ := time.Parse(time.RFC3339, o.FilledAt)

		out = append(out, BrokerOrder{
			BrokerOrderID: o.ID,
			Symbol:        o.Symbol,
			Side:          o.Side,
			Qty:           qty,
			FilledQty:     filledQty,
			FilledAvgPx:   filledPx,
			Status:        o.Status,
			SubmittedAt:   submittedAt,
			FilledAt:      filledAt,
		})
	}
	return out, nil
}
