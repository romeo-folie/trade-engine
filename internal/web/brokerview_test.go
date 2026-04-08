package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
)

// ── Stub BrokerView ───────────────────────────────────────────────────────────

type stubBrokerView struct {
	account   execution.BrokerAccount
	accountErr error

	positions    []execution.BrokerPosition
	positionsErr error

	history    execution.EquityHistory
	historyErr error

	orders    []execution.BrokerOrder
	ordersErr error
}

func (s *stubBrokerView) AccountSnapshot(_ context.Context) (execution.BrokerAccount, error) {
	return s.account, s.accountErr
}
func (s *stubBrokerView) PositionsSnapshot(_ context.Context) ([]execution.BrokerPosition, error) {
	return s.positions, s.positionsErr
}
func (s *stubBrokerView) EquityCurve(_ context.Context, _, _ string) (execution.EquityHistory, error) {
	return s.history, s.historyErr
}
func (s *stubBrokerView) RecentFills(_ context.Context) ([]execution.BrokerOrder, error) {
	return s.orders, s.ordersErr
}

// stubCache builds a zero-TTL brokerCache around a stub (pass-through, no caching).
func stubCache(bv execution.BrokerView) *brokerCache {
	return NewBrokerCache(bv, 0, 0, 0, 0)
}

// ── handlePortfolio ───────────────────────────────────────────────────────────

func TestHandlePortfolio_BrokerPath(t *testing.T) {
	bv := &stubBrokerView{
		account: execution.BrokerAccount{
			Equity:     105_000,
			LastEquity: 100_000,
			Cash:       20_000,
		},
	}
	srv := NewServer(Config{}, nil, nil)
	srv.SetBrokerView(stubCache(bv))

	w := httptest.NewRecorder()
	srv.handlePortfolio(w, httptest.NewRequest("GET", "/api/node/portfolio", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp["source"]; got != "broker" {
		t.Errorf("source: want broker, got %v", got)
	}
	if got := resp["TotalValue"].(float64); got != 105_000 {
		t.Errorf("TotalValue: want 105000, got %v", got)
	}
	dayPnL := resp["DayPnL"].(float64)
	if dayPnL != 5_000 {
		t.Errorf("DayPnL: want 5000, got %v", dayPnL)
	}
}

func TestHandlePortfolio_BrokerError_FallbackNoTracker_NoDB(t *testing.T) {
	bv := &stubBrokerView{accountErr: execution.ErrBrokerUnavailable}
	srv := NewServer(Config{}, nil, nil)
	srv.SetBrokerView(stubCache(bv))

	w := httptest.NewRecorder()
	srv.handlePortfolio(w, httptest.NewRequest("GET", "/api/node/portfolio", nil))

	// No tracker and no DB → 500.
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// ── handlePortfolioPositions ──────────────────────────────────────────────────

func TestHandlePortfolioPositions_BrokerPath(t *testing.T) {
	bv := &stubBrokerView{
		positions: []execution.BrokerPosition{
			{
				Symbol:        "AAPL",
				Quantity:      10,
				AvgEntryPrice: 150,
				CurrentPrice:  160,
				MarketValue:   1600,
				UnrealizedPL:  100,
				Side:          "long",
			},
		},
	}
	srv := NewServer(Config{}, nil, nil)
	srv.SetBrokerView(stubCache(bv))

	w := httptest.NewRecorder()
	srv.handlePortfolioPositions(w, httptest.NewRequest("GET", "/api/portfolio/positions", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["source"] != "broker" {
		t.Errorf("source: want broker, got %v", resp["source"])
	}
	positions := resp["positions"].([]any)
	if len(positions) != 1 {
		t.Fatalf("expected 1 position, got %d", len(positions))
	}
	pos := positions[0].(map[string]any)
	if pos["Ticker"] != "AAPL" {
		t.Errorf("Ticker: want AAPL, got %v", pos["Ticker"])
	}
	if pos["Direction"] != "long" {
		t.Errorf("Direction: want long, got %v", pos["Direction"])
	}
}

func TestHandlePortfolioPositions_BrokerError_FallbackNoDB(t *testing.T) {
	bv := &stubBrokerView{positionsErr: execution.ErrBrokerUnavailable}
	srv := NewServer(Config{}, nil, nil)
	srv.SetBrokerView(stubCache(bv))

	w := httptest.NewRecorder()
	srv.handlePortfolioPositions(w, httptest.NewRequest("GET", "/api/portfolio/positions", nil))

	// No tracker, no DB → 500.
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// ── handlePortfolioHistory ────────────────────────────────────────────────────

func TestHandlePortfolioHistory_NoBroker_NoDB(t *testing.T) {
	srv := NewServer(Config{}, nil, nil)

	w := httptest.NewRecorder()
	srv.handlePortfolioHistory(w, httptest.NewRequest("GET", "/api/portfolio/history", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["alpaca"] != nil {
		t.Errorf("alpaca: want nil, got %v", resp["alpaca"])
	}
}

func TestHandlePortfolioHistory_BrokerEquityCurve(t *testing.T) {
	bv := &stubBrokerView{
		history: execution.EquityHistory{
			Period:    "1M",
			Timeframe: "1D",
			BaseValue: 100_000,
			Points: []execution.EquityPoint{
				{Time: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), Equity: 101_000},
				{Time: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), Equity: 102_000},
			},
		},
	}
	srv := NewServer(Config{}, nil, nil)
	srv.SetBrokerView(stubCache(bv))

	w := httptest.NewRecorder()
	srv.handlePortfolioHistory(w, httptest.NewRequest("GET", "/api/portfolio/history", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["source"] != "broker" {
		t.Errorf("source: want broker, got %v", resp["source"])
	}
	alpaca := resp["alpaca"].([]any)
	if len(alpaca) != 2 {
		t.Errorf("alpaca points: want 2, got %d", len(alpaca))
	}
}

// ── brokercache singleflight / TTL ───────────────────────────────────────────

func TestBrokerCache_TTLExpiry(t *testing.T) {
	calls := 0
	bv := &stubBrokerView{
		account: execution.BrokerAccount{Equity: 100},
	}
	// Override AccountSnapshot to count calls.
	countBv := &callCountingBV{inner: bv, count: &calls}
	cache := NewBrokerCache(countBv, 50*time.Millisecond, 0, 0, 0)
	ctx := context.Background()

	// First call — miss → fetch.
	if _, err := cache.AccountSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	// Second call within TTL — hit, no extra fetch.
	if _, err := cache.AccountSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("expected 1 fetch within TTL, got %d", calls)
	}

	// Wait for TTL to expire then fetch again.
	time.Sleep(60 * time.Millisecond)
	if _, err := cache.AccountSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("expected 2 fetches after TTL expiry, got %d", calls)
	}
}

type callCountingBV struct {
	inner execution.BrokerView
	count *int
}

func (c *callCountingBV) AccountSnapshot(ctx context.Context) (execution.BrokerAccount, error) {
	(*c.count)++
	return c.inner.AccountSnapshot(ctx)
}
func (c *callCountingBV) PositionsSnapshot(ctx context.Context) ([]execution.BrokerPosition, error) {
	return c.inner.PositionsSnapshot(ctx)
}
func (c *callCountingBV) EquityCurve(ctx context.Context, p, tf string) (execution.EquityHistory, error) {
	return c.inner.EquityCurve(ctx, p, tf)
}
func (c *callCountingBV) RecentFills(ctx context.Context) ([]execution.BrokerOrder, error) {
	return c.inner.RecentFills(ctx)
}
