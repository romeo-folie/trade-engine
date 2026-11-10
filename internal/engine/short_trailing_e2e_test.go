package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/execution/trailing"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// ReplaceStopOrder and ReplaceStopOrderWithClientID make the fake an
// Alpaca-shaped PATCH-capable broker; the durable coordinator uses the
// client-ID form so a replacement is idempotent across restarts.
func (b *shortBroker) ReplaceStopOrder(ctx context.Context, brokerOrderID string, px float64) (execution.OrderResult, error) {
	return b.ReplaceStopOrderWithClientID(ctx, brokerOrderID, px, "")
}

func (b *shortBroker) ReplaceStopOrderWithClientID(_ context.Context, brokerOrderID string, px float64, clientID string) (execution.OrderResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.replaced = append(b.replaced, shortReplace{brokerOrderID, px})
	res := execution.OrderResult{BrokerOrderID: fmt.Sprintf("%s-r%d", brokerOrderID, len(b.replaced)), Status: "new"}
	b.byID[res.BrokerOrderID] = res
	if clientID != "" {
		b.byClient[clientID] = res
	}
	return res, nil
}

type shortReplace struct {
	ID    string
	Price float64
}

// durableTrailingStore is the live adapter's full method set: TradeStore plus
// the optional durable-intent methods the manager discovers by assertion.
type durableTrailingStore interface {
	trailing.TradeStore
	StockIntentIDForKey(context.Context, string) (string, error)
	ResolveStockOrderIntentRetryGeneration(context.Context, string) (string, error)
	LinkReplacementOrderIntent(context.Context, string, string) error
	UpdateOrderStopReplacementAndLinkIntent(context.Context, string, string, string, float64, float64, float64, float64, string, float64, float64, time.Time, store.ExitIntent) error
}

// tickerScopedStore limits the manager to one ticker. The shared test DB
// holds other tests' open trades, and the manager trails every open trade.
type tickerScopedStore struct {
	durableTrailingStore
	ticker string
}

func (s tickerScopedStore) GetOpenTrades(ctx context.Context) ([]store.Trade, error) {
	all, err := s.durableTrailingStore.GetOpenTrades(ctx)
	var out []store.Trade
	for _, t := range all {
		if t.Ticker == s.ticker {
			out = append(out, t)
		}
	}
	return out, err
}

// hourlyPrices feeds the trailing manager 1h candles ending in tail.
type hourlyPrices struct{ candles []data.Candle }

func (h hourlyPrices) FetchHistory(context.Context, string, data.Timeframe, int) ([]data.Candle, error) {
	return h.candles, nil
}

// End to end (#599 PR 4b): a real short opened through the engine (durable
// sell-side intent, broker bracket, buy-side legs in Postgres) is trailed by
// the production trailing manager over the live store and the durable
// coordinator: the BUY stop leg is PATCHed down, the low-water mark and stop
// persist, and nothing is ever sold.
func TestShortTrailingEndToEndPatchesBracketStopDown(t *testing.T) {
	f := newShortFixture(t, "SHTR")
	id, sig := f.signal()
	f.e.executeShortEntry(f.ctx, id, sig, false)
	trades := f.openTrades()
	if len(trades) != 1 || trades[0].StopLoss != 101 {
		t.Fatalf("setup: trades=%+v status=%v", trades, firstNonEmpty(f.signalStatus(id)))
	}
	// The broker now holds the 5-share short.
	f.broker.mu.Lock()
	f.broker.positions = []execution.BrokerPosition{{Symbol: f.ticker, Side: "short", Quantity: -5, CurrentPrice: 90, MarketValue: -450, AvgEntryPrice: 98}}
	f.broker.mu.Unlock()

	var candles []data.Candle
	for i := 0; i < 30; i++ {
		candles = append(candles, data.Candle{Ticker: f.ticker, Open: 97.5, High: 98.5, Low: 97.5, Close: 98, Volume: 1000,
			Timestamp: time.Now().Add(time.Duration(i-31) * time.Hour)})
	}
	candles = append(candles, data.Candle{Ticker: f.ticker, Open: 92, High: 92.5, Low: 90, Close: 90, Volume: 1000, Timestamp: time.Now()})

	live, ok := trailing.LiveStore().(durableTrailingStore)
	if !ok {
		t.Fatal("live trailing store lost its durable-intent methods")
	}
	scoped := tickerScopedStore{durableTrailingStore: live, ticker: f.ticker}
	m := trailing.NewManager(trailing.Config{Enabled: true, ATRMultiple: 1.5}, scoped, hourlyPrices{candles}, f.e.executor)
	m.UpdateAll(f.ctx)

	after, err := store.GetTradeByID(f.ctx, trades[0].ID)
	if err != nil || after == nil {
		t.Fatalf("reload: %v", err)
	}
	if after.HighWaterMark != 90 || after.StopLoss >= 101 || after.StopLoss <= 90 {
		t.Fatalf("after trailing: low=%v stop=%v, want low 90 and the stop ratcheted below 101 but above 90", after.HighWaterMark, after.StopLoss)
	}
	f.broker.mu.Lock()
	replaced := append([]shortReplace(nil), f.broker.replaced...)
	placed := append([]execution.OrderRequest(nil), f.broker.placed...)
	f.broker.mu.Unlock()
	if len(replaced) != 1 || replaced[0].ID != f.id("parent-1-sl") || replaced[0].Price != after.StopLoss {
		t.Fatalf("replaced = %+v, want one PATCH of the short's stop leg to %v", replaced, after.StopLoss)
	}
	for _, p := range placed[1:] {
		if p.Side == execution.OrderSideSell {
			t.Fatalf("trailing a short placed a SELL: %+v", p)
		}
	}
	// The replacement intent is a BUY stop, linked to the short trade.
	var side string
	if err := store.GetDB().QueryRow(f.ctx, `SELECT side::text FROM stock_order_intents WHERE idempotency_key LIKE 'replace:%' AND trade_id=$1::uuid`, after.ID).Scan(&side); err != nil || side != "buy" {
		t.Fatalf("replacement intent side = %q (%v), want buy", side, err)
	}
}
