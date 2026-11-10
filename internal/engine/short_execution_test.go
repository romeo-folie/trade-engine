package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/cost"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
	"github.com/romeo-folie/trade-engine/internal/stockorders"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// ─── Fakes ───────────────────────────────────────────────────────────────────

// shortBroker is an Alpaca-shaped broker for the short entry path. Placed
// bracket parents fill immediately (unless placeResult overrides), with both
// child legs working at the broker.
type shortBroker struct {
	mu          sync.Mutex
	account     execution.BrokerAccount
	asset       execution.StockAsset
	positions   []execution.BrokerPosition
	working     []execution.WorkingOrder
	placed      []execution.OrderRequest
	cancelled   []string
	byClient    map[string]execution.OrderResult
	byID        map[string]execution.OrderResult
	placeResult func(req execution.OrderRequest, n int) (execution.OrderResult, error)
}

func newShortBroker() *shortBroker {
	now := time.Now().UTC()
	account := validAdmissionAccount(now)
	account.Equity, account.LastEquity, account.Cash = 10_000, 10_000, 10_000
	account.BuyingPower, account.RegTBuyingPower = 20_000, 20_000
	account.Multiplier, account.ShortingEnabled = 4, true
	return &shortBroker{
		account: account,
		asset: execution.StockAsset{
			Status: "active", AssetClass: "us_equity", Tradable: true,
			TradableAvailable: true, Marginable: true, MarginableAvailable: true,
			Fractionable: true, FractionableAvailable: true,
			Shortable: true, ShortableAvailable: true, EasyToBorrow: true, EasyToBorrowAvailable: true,
		},
		byClient: map[string]execution.OrderResult{},
		byID:     map[string]execution.OrderResult{},
	}
}

func (*shortBroker) BrokerName() string { return "alpaca" }
func (b *shortBroker) AccountSnapshot(context.Context) (execution.BrokerAccount, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.account
	a.FetchedAt = time.Now().UTC()
	return a, nil
}
func (b *shortBroker) PositionsSnapshot(context.Context) ([]execution.BrokerPosition, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]execution.BrokerPosition(nil), b.positions...), nil
}
func (*shortBroker) EquityCurve(context.Context, string, string) (execution.EquityHistory, error) {
	return execution.EquityHistory{}, nil
}
func (*shortBroker) RecentFills(context.Context) ([]execution.BrokerOrder, error) { return nil, nil }
func (b *shortBroker) StockAssetSnapshot(_ context.Context, symbol string) (execution.StockAsset, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.asset
	a.Symbol, a.FetchedAt = symbol, time.Now().UTC()
	return a, nil
}
func (b *shortBroker) GetWorkingOrders(context.Context) ([]execution.WorkingOrder, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]execution.WorkingOrder(nil), b.working...), nil
}
func (b *shortBroker) GetOrderByClientID(_ context.Context, clientID string) (execution.OrderResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.byClient[clientID]; ok {
		return r, nil
	}
	return execution.OrderResult{}, execution.ErrOrderNotFound
}
func (b *shortBroker) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.placed = append(b.placed, req)
	n := len(b.placed)
	var res execution.OrderResult
	var err error
	if b.placeResult != nil {
		res, err = b.placeResult(req, n)
	} else {
		res = filledBracket(req, n)
	}
	if res.BrokerOrderID != "" {
		b.byClient[req.ClientOrderID] = res
		b.byID[res.BrokerOrderID] = res
	}
	return res, err
}
func (b *shortBroker) CancelOrder(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelled = append(b.cancelled, id)
	if r, ok := b.byID[id]; ok && !execution.IsTerminalOrderStatus(r.Status) {
		r.Status = "cancelled"
		b.byID[id] = r
	}
	return nil
}
func (b *shortBroker) GetOrderStatus(_ context.Context, id string) (execution.OrderResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.byID[id]; ok {
		return r, nil
	}
	return execution.OrderResult{}, execution.ErrOrderNotFound
}
func (*shortBroker) GetPositions(context.Context) ([]execution.Position, error) { return nil, nil }
func (*shortBroker) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}

func (b *shortBroker) placedOrders() []execution.OrderRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]execution.OrderRequest(nil), b.placed...)
}

// filledBracket is a fully filled bracket parent whose two children are
// working at the broker, which is verified broker protection.
func filledBracket(req execution.OrderRequest, n int) execution.OrderResult {
	if req.OrderClass != "bracket" {
		return execution.OrderResult{
			BrokerOrderID: fmt.Sprintf("order-%d", n), Status: "filled",
			Quantity: req.Quantity, FilledQty: req.Quantity, FilledPrice: req.ReferencePrice,
			FilledAt: time.Now().UTC(),
		}
	}
	parent := fmt.Sprintf("parent-%d", n)
	tp, sl := parent+"-tp", parent+"-sl"
	return execution.OrderResult{
		BrokerOrderID: parent, Status: "filled", Quantity: req.Quantity,
		FilledQty: req.Quantity, FilledPrice: req.ReferencePrice, FilledAt: time.Now().UTC(),
		ChildOrderIDs: []string{tp, sl},
		ChildOrders: []execution.OrderLeg{
			{BrokerOrderID: tp, Type: "limit", Status: "new", Quantity: req.Quantity, RemainingQty: req.Quantity, LimitPrice: req.TakeProfitPrice},
			{BrokerOrderID: sl, Type: "stop", Status: "new", Quantity: req.Quantity, RemainingQty: req.Quantity, StopPrice: req.StopLossPrice},
		},
	}
}

// shortMarket serves a fresh two-sided quote and daily bars: 25 completed
// sessions closing at 100 on 200k shares ($20M/day) and today's partial bar.
type shortMarket struct {
	bid, ask     float64
	todayLow     float64 // consolidated (SIP) low through the entitlement cutoff
	iexTodayLow  float64 // real-time primary-feed low; 0 = same as todayLow
	prevLow      float64 // low of the last completed session (SSR carry-over)
	noToday      bool
	historyCalls int
}

// FetchDailyBars is the consolidated (SIP) daily-bar source.
func (m *shortMarket) FetchDailyBars(_ context.Context, symbols []string, _, _ time.Time, _ int) (data.DailyBarsBatchResult, error) {
	res := data.DailyBarsBatchResult{Bars: map[string][]data.Candle{}, Failures: map[string]error{}}
	for _, s := range symbols {
		res.Bars[s] = shortDailyBars(s, time.Now(), m.todayLow, m.prevLow, m.noToday)
	}
	return res, nil
}

func (m *shortMarket) FetchQuote(_ context.Context, ticker string) (*data.Quote, error) {
	return &data.Quote{Ticker: ticker, Bid: m.bid, Ask: m.ask, Last: m.bid, Timestamp: time.Now().UTC()}, nil
}
func (*shortMarket) FetchCandles(context.Context, string, data.Timeframe, time.Time, time.Time) ([]data.Candle, error) {
	return nil, nil
}
func (m *shortMarket) FetchHistory(_ context.Context, ticker string, tf data.Timeframe, _ int) ([]data.Candle, error) {
	m.historyCalls++
	if tf != data.Timeframe1Day {
		return nil, nil
	}
	low := m.todayLow
	if m.iexTodayLow > 0 {
		low = m.iexTodayLow
	}
	return shortDailyBars(ticker, time.Now(), low, m.prevLow, m.noToday), nil
}

func shortDailyBars(ticker string, now time.Time, todayLow, prevLow float64, noToday bool) []data.Candle {
	et := now.In(nyLoc)
	today := time.Date(et.Year(), et.Month(), et.Day(), 0, 0, 0, 0, nyLoc)
	var bars []data.Candle
	for i := 25; i >= 1; i-- {
		low := 99.0
		if i == 1 && prevLow > 0 {
			low = prevLow
		}
		bars = append(bars, data.Candle{
			Ticker: ticker, Timeframe: data.Timeframe1Day, Timestamp: today.AddDate(0, 0, -i),
			Open: 100, High: 101, Low: low, Close: 100, Volume: 200_000,
		})
	}
	if !noToday {
		bars = append(bars, data.Candle{
			Ticker: ticker, Timeframe: data.Timeframe1Day, Timestamp: today,
			Open: 100, High: 100.5, Low: todayLow, Close: 98, Volume: 50_000,
		})
	}
	return bars
}

// ─── Fixture ─────────────────────────────────────────────────────────────────

type shortFixture struct {
	t      *testing.T
	ctx    context.Context
	ticker string
	broker *shortBroker
	market *shortMarket
	e      *Engine
}

func newShortFixture(t *testing.T, prefix string) *shortFixture {
	t.Helper()
	requireEngineDB(t)
	prevOpen := shortEntryMarketOpen
	shortEntryMarketOpen = func(time.Time) bool { return true }
	prevDelay := brokerProtectionVerificationRetryDelay
	brokerProtectionVerificationRetryDelay = 0
	// An accepted-but-unfilled entry starts pollUnfilledStockIntent, whose
	// Coordinator.Recover sweeps every intent in the shared test DB. Park it
	// past the test run so it cannot race other tests' intents through this
	// fixture's fake broker (recovery itself is exercised explicitly).
	prevPoll := unfilledEntryPollDelay
	unfilledEntryPollDelay = 24 * time.Hour
	t.Cleanup(func() {
		shortEntryMarketOpen = prevOpen
		brokerProtectionVerificationRetryDelay = prevDelay
		unfilledEntryPollDelay = prevPoll
	})
	ctx := context.Background()
	ticker := uniqueTicker(prefix)
	// The earnings calendar covers the ticker with a date far outside the
	// blackout, so the calendar is known and quiet.
	if err := store.UpsertEarnings(ctx, store.GetDB(), ticker, time.Now().AddDate(0, 2, 0), "amc", "test"); err != nil {
		t.Fatalf("seed earnings: %v", err)
	}
	broker := newShortBroker()
	market := &shortMarket{bid: 98, ask: 98.05, todayLow: 97.5}
	cfg := &config.Config{}
	cfg.Markets.Stocks = config.StocksConfig{Enabled: true, Paper: true, ExpectedAccountNumber: "PA123"}
	cfg.Risk = config.RiskConfig{
		ConfidenceThreshold: 0.5, MinRiskReward: 1.5, MaxPositionPct: 10,
		// The shared test DB holds other tests' open trades, so the position
		// count is not under test here.
		MaxOpenPositions: 1_000_000, MaxDollarRiskPct: 2, DailyLossLimitPct: 5, MaxDrawdownPct: 20,
		BracketExitsEnabled: true,
		Shorts: config.ShortsConfig{
			Enabled: true, SizePct: 5, MaxGrossShortPct: 20, MaxTickerShortPct: 10,
			MinPrice: 5, MinAvgDollarVolume: 1_000_000, EarningsBlackoutDays: 3, SSRDropPct: 10,
		},
	}
	e := &Engine{
		cfg: cfg, executor: stockorders.New(broker), marketData: market,
		portfolio: portfolio.NewTracker(time.Hour), bus: events.New(), costTracker: cost.NewTracker(),
		mode: "paper",
	}
	e.SetShortEvidenceBars(market, true)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = store.GetDB().Exec(c, `DELETE FROM orders WHERE ticker=$1`, ticker)
		_, _ = store.GetDB().Exec(c, `UPDATE trades SET stock_intent_id=NULL WHERE ticker=$1`, ticker)
		_, _ = store.GetDB().Exec(c, `DELETE FROM stock_order_intents WHERE ticker=$1`, ticker)
		_, _ = store.GetDB().Exec(c, `DELETE FROM trades WHERE ticker=$1`, ticker)
		_, _ = store.GetDB().Exec(c, `DELETE FROM earnings_calendar WHERE symbol=$1`, ticker)
	})
	return &shortFixture{t: t, ctx: ctx, ticker: ticker, broker: broker, market: market, e: e}
}

// signal inserts a pending SHORT at the bid with a stop 3 above and a target
// 6 below (R:R 2.0).
func (f *shortFixture) signal() (string, store.Signal) {
	f.t.Helper()
	sig := store.Signal{
		Ticker: f.ticker, Action: "SHORT", Strategy: "momentum", SignalPrice: 98,
		AcceptedReferencePrice: 98, StopLoss: 101, TakeProfit: 92, Confidence: 0.8,
		SizePct: 5, StalePriceMovePct: 2, Status: "pending", EntrySetup: store.EntrySetupStandard,
		ExpiresAt: time.Now().Add(30 * time.Minute), CreatedAt: time.Now(),
	}
	id, err := store.InsertSignal(f.ctx, sig)
	if err != nil {
		f.t.Fatalf("InsertSignal: %v", err)
	}
	sig.ID = id
	return id, sig
}

func (f *shortFixture) signalStatus(id string) (string, string) {
	f.t.Helper()
	s, err := store.GetSignalByID(f.ctx, id)
	if err != nil || s == nil {
		f.t.Fatalf("GetSignalByID: %v", err)
	}
	return s.Status, s.BlockReason
}

func (f *shortFixture) openTrades() []store.Trade {
	f.t.Helper()
	trades, err := store.GetOwnedPositionTrades(f.ctx)
	if err != nil {
		f.t.Fatalf("GetOwnedPositionTrades: %v", err)
	}
	var out []store.Trade
	for _, tr := range trades {
		if tr.Ticker == f.ticker {
			out = append(out, tr)
		}
	}
	return out
}

func (f *shortFixture) ordersForTicker() []store.Order {
	f.t.Helper()
	rows, err := store.GetDB().Query(f.ctx, `
		SELECT COALESCE(broker_order_id,''), side::text, order_type::text, quantity,
		       COALESCE(is_stop_loss,false), COALESCE(is_take_profit,false),
		       COALESCE(stop_price,0), COALESCE(limit_price,0)
		FROM orders WHERE ticker=$1 ORDER BY created_at, broker_order_id`, f.ticker)
	if err != nil {
		f.t.Fatalf("orders: %v", err)
	}
	defer rows.Close()
	var out []store.Order
	for rows.Next() {
		var o store.Order
		if err := rows.Scan(&o.BrokerOrderID, &o.Side, &o.OrderType, &o.Quantity, &o.IsStopLoss, &o.IsTakeProfit, &o.StopPrice, &o.LimitPrice); err != nil {
			f.t.Fatalf("scan order: %v", err)
		}
		out = append(out, o)
	}
	return out
}

// ─── Short entry: accepted and filled ────────────────────────────────────────

// SHORT → sell-side durable intent → broker bracket (stop above, target
// below) → one short trade → entry order sell, both legs buy.
func TestShortEntryFilledEndToEnd(t *testing.T) {
	f := newShortFixture(t, "SHF")
	id, sig := f.signal()
	f.e.executeShortEntry(f.ctx, id, sig, false)

	placed := f.broker.placedOrders()
	if len(placed) != 1 {
		t.Fatalf("placed %d orders, want 1 (status %v)", len(placed), firstNonEmpty(f.signalStatus(id)))
	}
	req := placed[0]
	if req.Side != execution.OrderSideSell || req.OrderClass != "bracket" || !req.RequireBrokerProtection ||
		req.TimeInForce != "gtc" || req.IntentKey != "entry:"+id || req.ClientOrderID == "" {
		t.Fatalf("short request = %+v", req)
	}
	if req.StopLossPrice <= 98 || req.TakeProfitPrice >= 98 || req.StopLossPrice != 101 || req.TakeProfitPrice != 92 {
		t.Fatalf("short bracket stop=%v target=%v around bid 98", req.StopLossPrice, req.TakeProfitPrice)
	}
	// 5%% of $10k equity at the $98 bid = 5.10 → 5 whole shares (never fractional).
	if req.Quantity != 5 || req.ReferencePrice != 98 {
		t.Fatalf("short qty=%v ref=%v, want 5 @ 98", req.Quantity, req.ReferencePrice)
	}

	if status, reason := f.signalStatus(id); status != "executed" {
		t.Fatalf("signal status %s (%s), want executed", status, reason)
	}
	trades := f.openTrades()
	if len(trades) != 1 || trades[0].Direction != "short" || trades[0].Quantity != 5 ||
		trades[0].EntryPrice != 98 || trades[0].StopLoss != 101 || trades[0].TakeProfit != 92 {
		t.Fatalf("trades = %+v, want one 5-share short @98 SL101 TP92", trades)
	}
	intent, err := store.GetStockOrderIntentByKey(f.ctx, "entry:"+id)
	if err != nil || intent == nil || intent.Side != "sell" || intent.TradeID != trades[0].ID {
		t.Fatalf("intent = %+v (%v), want side sell linked to the trade", intent, err)
	}

	var entry, tp, sl int
	for _, o := range f.ordersForTicker() {
		switch {
		case o.IsStopLoss:
			sl++
			if o.Side != "buy" || o.StopPrice != 101 {
				t.Fatalf("stop leg = %+v, want buy stop @101", o)
			}
		case o.IsTakeProfit:
			tp++
			if o.Side != "buy" || o.LimitPrice != 92 {
				t.Fatalf("target leg = %+v, want buy limit @92", o)
			}
		default:
			entry++
			if o.Side != "sell" {
				t.Fatalf("entry order = %+v, want sell", o)
			}
		}
	}
	if entry != 1 || tp != 1 || sl != 1 {
		t.Fatalf("orders entry=%d tp=%d sl=%d, want 1/1/1", entry, tp, sl)
	}
	var childSides []string
	rows, err := store.GetDB().Query(f.ctx, `SELECT side::text FROM stock_order_intents WHERE parent_intent_id=$1::uuid AND idempotency_key LIKE 'bracket-child:%' ORDER BY idempotency_key`, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		childSides = append(childSides, s)
	}
	rows.Close()
	if len(childSides) != 2 || childSides[0] != "buy" || childSides[1] != "buy" {
		t.Fatalf("bracket child intent sides = %v, want [buy buy]", childSides)
	}
	if required, _, err := store.StockEntryBrokerProtection(f.ctx, trades[0].ID); err != nil || !required {
		t.Fatalf("StockEntryBrokerProtection(short) = %v, %v; want required", required, err)
	}

	// Duplicate execution of the same signal never places a second order.
	f.e.executeShortEntry(f.ctx, id, sig, false)
	if n := len(f.broker.placedOrders()); n != 1 {
		t.Fatalf("duplicate SHORT placed %d orders, want 1", n)
	}
	if n := len(f.openTrades()); n != 1 {
		t.Fatalf("duplicate SHORT produced %d trades, want 1", n)
	}
}

// setGrossRoom sets the gross short cap so that exactly room dollars remain
// above brokerShort, after any open shorts other tests left in the shared
// test DB (shortExposure counts the larger of broker and local truth).
func (f *shortFixture) setGrossRoom(brokerShort, pendingShort, room float64) {
	f.t.Helper()
	owned, err := store.GetOwnedPositionTrades(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	local, _, err := shortExposure(nil, owned, nil, "", f.ticker)
	if err != nil {
		f.t.Fatal(err)
	}
	gross := local
	if brokerShort > gross {
		gross = brokerShort
	}
	gross += pendingShort
	f.e.cfg.Risk.Shorts.MaxGrossShortPct = (gross + room) / f.broker.account.Equity * 100
}

func firstNonEmpty(a, b string) string {
	if b != "" {
		return a + ": " + b
	}
	return a
}

// ─── Eligibility at order time ───────────────────────────────────────────────

func TestShortEntryEligibilityBlocksAtOrderTime(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *shortFixture)
		want   string
	}{
		{"account shorting disabled", func(f *shortFixture) { f.broker.account.ShortingEnabled = false }, "account_shorting_disabled"},
		{"cash account", func(f *shortFixture) { f.broker.account.Multiplier = 1 }, "account_not_margin"},
		{"trading blocked", func(f *shortFixture) { f.broker.account.TradingBlocked = true }, "not active, unrestricted"},
		{"not shortable", func(f *shortFixture) { f.broker.asset.Shortable = false }, "asset_not_shortable"},
		{"hard to borrow", func(f *shortFixture) { f.broker.asset.EasyToBorrow = false }, "asset_not_easy_to_borrow"},
		{"borrow unknown", func(f *shortFixture) { f.broker.asset.EasyToBorrowAvailable = false }, "asset_borrow_status_unknown"},
		{"ssr today", func(f *shortFixture) { f.market.todayLow = 89 }, "short_sale_restriction_active"},
		{"ssr carried over", func(f *shortFixture) { f.market.prevLow = 89 }, "short_sale_restriction_active"},
		{"session low unknown", func(f *shortFixture) { f.market.noToday = true }, "ssr_status_unknown"},
		// Codex: SSR needs consolidated evidence. No SIP source, or an IEX-only
		// source, leaves SSR and liquidity unknown.
		{"no consolidated bars", func(f *shortFixture) { f.e.SetShortEvidenceBars(nil, true) }, "ssr_status_unknown"},
		{"iex-only bars refused", func(f *shortFixture) { f.e.SetShortEvidenceBars(f.market, false) }, "liquidity_unknown"},
		// A primary-feed print after the SIP cutoff still triggers SSR.
		{"ssr after the sip cutoff", func(f *shortFixture) { f.market.iexTodayLow = 89 }, "short_sale_restriction_active"},
		{"below min price", func(f *shortFixture) { f.e.cfg.Risk.Shorts.MinPrice = 150 }, "below_min_price"},
		{"illiquid", func(f *shortFixture) { f.e.cfg.Risk.Shorts.MinAvgDollarVolume = 1e9 }, "below_min_dollar_volume"},
		{"earnings blackout", func(f *shortFixture) {
			_, _ = store.GetDB().Exec(f.ctx, `DELETE FROM earnings_calendar WHERE symbol=$1`, f.ticker)
			if err := store.UpsertEarnings(f.ctx, store.GetDB(), f.ticker, time.Now().AddDate(0, 0, 1), "amc", "test"); err != nil {
				f.t.Fatal(err)
			}
		}, "earnings_blackout"},
		{"earnings unknown", func(f *shortFixture) {
			_, _ = store.GetDB().Exec(f.ctx, `DELETE FROM earnings_calendar WHERE symbol=$1`, f.ticker)
		}, "earnings_calendar_unknown"},
		{"live account not allowed", func(f *shortFixture) {
			f.e.cfg.Markets.Stocks.Paper = false
		}, "shorts_live_not_allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newShortFixture(t, "SHE")
			tc.mutate(f)
			id, sig := f.signal()
			f.e.executeShortEntry(f.ctx, id, sig, false)
			status, reason := f.signalStatus(id)
			if status != "blocked" || !strings.Contains(reason, tc.want) {
				t.Fatalf("status=%s reason=%q, want blocked with %s", status, reason, tc.want)
			}
			if n := len(f.broker.placedOrders()); n != 0 {
				t.Fatalf("ineligible short placed %d orders", n)
			}
			if n := len(f.openTrades()); n != 0 {
				t.Fatalf("ineligible short produced %d trades", n)
			}
		})
	}
}

// The gross cap sizes a short down to the remaining room, and blocks it when
// no whole share fits.
func TestShortEntryExposureCapsSizeAndBlock(t *testing.T) {
	t.Run("sized down to the remaining gross cap", func(t *testing.T) {
		f := newShortFixture(t, "SHC")
		// $1,700 of other shorts against a $2,000 (20%) gross cap leaves
		// $300: three shares at $98, not the five size_pct asks for.
		f.broker.positions = []execution.BrokerPosition{{Symbol: "OTHERSHORT", Quantity: -17, CurrentPrice: 100, MarketValue: -1700, AvgEntryPrice: 100, Side: "short"}}
		f.setGrossRoom(1700, 0, 300)
		id, sig := f.signal()
		f.e.executeShortEntry(f.ctx, id, sig, false)
		placed := f.broker.placedOrders()
		if len(placed) != 1 || placed[0].Quantity != 3 {
			t.Fatalf("placed = %+v (status %v), want one 3-share short", placed, firstNonEmpty(f.signalStatus(id)))
		}
	})
	t.Run("no room blocks", func(t *testing.T) {
		f := newShortFixture(t, "SHC")
		f.broker.positions = []execution.BrokerPosition{{Symbol: "OTHERSHORT", Quantity: -20, CurrentPrice: 100, MarketValue: -2000, AvgEntryPrice: 100, Side: "short"}}
		f.setGrossRoom(2000, 0, 50) // less than one $98 share
		id, sig := f.signal()
		f.e.executeShortEntry(f.ctx, id, sig, false)
		if status, _ := f.signalStatus(id); status != "blocked" || len(f.broker.placedOrders()) != 0 {
			t.Fatalf("status=%s placed=%d, want blocked with no order", status, len(f.broker.placedOrders()))
		}
	})
}

// ─── Opposite positions ──────────────────────────────────────────────────────

func TestShortEntryRequiresFlatTicker(t *testing.T) {
	t.Run("broker long", func(t *testing.T) {
		f := newShortFixture(t, "SHO")
		f.broker.positions = []execution.BrokerPosition{{Symbol: f.ticker, Quantity: 4, CurrentPrice: 98, MarketValue: 392, AvgEntryPrice: 95, Side: "long"}}
		id, sig := f.signal()
		f.e.executeShortEntry(f.ctx, id, sig, false)
		if status, reason := f.signalStatus(id); status != "blocked" || !strings.Contains(reason, "long position") {
			t.Fatalf("status=%s reason=%q", status, reason)
		}
		if len(f.broker.placedOrders()) != 0 {
			t.Fatal("SHORT on a broker-held long placed an order")
		}
	})
	t.Run("local long trade", func(t *testing.T) {
		f := newShortFixture(t, "SHO")
		if _, err := store.InsertTrade(f.ctx, store.Trade{Ticker: f.ticker, Direction: "long", Strategy: "t", EntryPrice: 95, Quantity: 4, StopLoss: 90, TakeProfit: 110, Status: "open"}); err != nil {
			t.Fatal(err)
		}
		id, sig := f.signal()
		f.e.executeShortEntry(f.ctx, id, sig, false)
		if status, reason := f.signalStatus(id); status != "blocked" || !strings.Contains(reason, "cannot short") {
			t.Fatalf("status=%s reason=%q", status, reason)
		}
		if len(f.broker.placedOrders()) != 0 {
			t.Fatal("SHORT on a held long placed an order")
		}
	})
	t.Run("existing short", func(t *testing.T) {
		f := newShortFixture(t, "SHO")
		if _, err := store.InsertTrade(f.ctx, store.Trade{Ticker: f.ticker, Direction: "short", Strategy: "t", EntryPrice: 99, Quantity: 2, StopLoss: 102, TakeProfit: 93, Status: "open"}); err != nil {
			t.Fatal(err)
		}
		id, sig := f.signal()
		f.e.executeShortEntry(f.ctx, id, sig, false)
		if status, reason := f.signalStatus(id); status != "blocked" || !strings.Contains(reason, "cannot add to a short") {
			t.Fatalf("status=%s reason=%q", status, reason)
		}
	})
}

// ─── Broker outcomes ─────────────────────────────────────────────────────────

func TestShortEntryBrokerRejectAndUnfilled(t *testing.T) {
	t.Run("rejected at the broker", func(t *testing.T) {
		f := newShortFixture(t, "SHR")
		f.broker.placeResult = func(req execution.OrderRequest, n int) (execution.OrderResult, error) {
			return execution.OrderResult{BrokerOrderID: fmt.Sprintf("rej-%d", n), Status: "rejected", Quantity: req.Quantity}, nil
		}
		id, sig := f.signal()
		f.e.executeShortEntry(f.ctx, id, sig, false)
		if n := len(f.openTrades()); n != 0 {
			t.Fatalf("rejected short left %d open trades", n)
		}
		if status, _ := f.signalStatus(id); status == "executed" {
			// A synchronous terminal rejection must not look like a fill.
			var trades int
			_ = store.GetDB().QueryRow(f.ctx, `SELECT count(*) FROM trades WHERE ticker=$1 AND status IN ('open','exiting')`, f.ticker).Scan(&trades)
			if trades != 0 {
				t.Fatalf("rejected short is executed with %d live trades", trades)
			}
		}
	})
	t.Run("accepted but not yet filled", func(t *testing.T) {
		f := newShortFixture(t, "SHU")
		f.broker.placeResult = func(req execution.OrderRequest, n int) (execution.OrderResult, error) {
			return execution.OrderResult{BrokerOrderID: fmt.Sprintf("acc-%d", n), Status: "accepted", Quantity: req.Quantity, RemainingQty: req.Quantity}, nil
		}
		id, sig := f.signal()
		f.e.executeShortEntry(f.ctx, id, sig, false)
		// A working entry is not a position: no trade until a fill is reported.
		if n := len(f.openTrades()); n != 0 {
			t.Fatalf("unfilled short produced %d trades", n)
		}
		intent, err := store.GetStockOrderIntentByKey(f.ctx, "entry:"+id)
		if err != nil || intent == nil || intent.Side != "sell" || intent.BrokerOrderID == "" {
			t.Fatalf("unfilled short intent = %+v (%v)", intent, err)
		}
		// Its reservation is active and carries the short direction.
		res, err := store.ListActiveStockEntryReservations(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range res {
			if r.IdempotencyKey == "entry:"+id {
				found = true
				if r.Direction() != "short" {
					t.Fatalf("reservation direction = %s", r.Direction())
				}
			}
		}
		if !found {
			t.Fatal("unfilled short holds no reservation")
		}
		// A second SHORT on the same ticker while the first is working is
		// refused (the pending short counts as a held short).
		id2, sig2 := f.signal()
		f.e.executeShortEntry(f.ctx, id2, sig2, false)
		if status, reason := f.signalStatus(id2); status != "blocked" {
			t.Fatalf("second SHORT behind a working short: status=%s reason=%q", status, reason)
		}
	})
}

// Restart after submit: the coordinator recovers the accepted sell-side
// intent from broker truth into exactly one short trade with buy-side legs.
func TestShortEntryRecoveryAfterFill(t *testing.T) {
	f := newShortFixture(t, "SHV")
	f.broker.placeResult = func(req execution.OrderRequest, n int) (execution.OrderResult, error) {
		return execution.OrderResult{BrokerOrderID: fmt.Sprintf("rec-%d", n), Status: "accepted", Quantity: req.Quantity, RemainingQty: req.Quantity}, nil
	}
	id, sig := f.signal()
	f.e.executeShortEntry(f.ctx, id, sig, false)
	placed := f.broker.placedOrders()
	if len(placed) != 1 {
		t.Fatalf("placed %d", len(placed))
	}
	// The broker fills while the engine is down.
	f.broker.mu.Lock()
	filled := filledBracket(placed[0], 1)
	filled.BrokerOrderID = "rec-1"
	f.broker.byClient[placed[0].ClientOrderID] = filled
	f.broker.byID["rec-1"] = filled
	f.broker.mu.Unlock()

	coordinator := f.e.executor.(*stockorders.Coordinator)
	for i := 0; i < 2; i++ { // recovery is idempotent
		// Recover sweeps every intent in the shared test DB; only an error
		// about this short's own intent is a failure here.
		if err := coordinator.Recover(f.ctx); err != nil && strings.Contains(err.Error(), placed[0].ClientOrderID) {
			t.Fatalf("Recover: %v", err)
		}
	}
	trades := f.openTrades()
	if len(trades) != 1 || trades[0].Direction != "short" || trades[0].Quantity != placed[0].Quantity {
		t.Fatalf("recovered trades = %+v", trades)
	}
	for _, o := range f.ordersForTicker() {
		if (o.IsStopLoss || o.IsTakeProfit) && o.Side != "buy" {
			t.Fatalf("recovered leg %+v must be buy", o)
		}
	}
}

// ─── Disabled means today ────────────────────────────────────────────────────

func TestShortsDisabledPlaceNothing(t *testing.T) {
	f := newShortFixture(t, "SHD")
	f.e.cfg.Risk.Shorts.Enabled = false
	id, sig := f.signal()
	f.e.executeShortEntry(f.ctx, id, sig, false)
	if status, _ := f.signalStatus(id); status != "blocked" || len(f.broker.placedOrders()) != 0 {
		t.Fatalf("disabled shorts: status=%s placed=%d", status, len(f.broker.placedOrders()))
	}
	if executableSignalAction("SHORT", false) || executableSignalAction("COVER", false) {
		t.Fatal("SHORT/COVER must not be executable with shorts disabled")
	}
	if !executableSignalAction("BUY", false) || !executableSignalAction("SELL", false) ||
		!executableSignalAction("SHORT", true) || !executableSignalAction("COVER", true) || executableSignalAction("HOLD", true) {
		t.Fatal("executableSignalAction table wrong")
	}
}

// ─── Cover ───────────────────────────────────────────────────────────────────

func coverSignal(t *testing.T, f *shortFixture) (string, store.Signal) {
	t.Helper()
	sig := store.Signal{Ticker: f.ticker, Action: "COVER", Strategy: "momentum", SignalPrice: 90, Confidence: 0.8, Status: "pending"}
	id, err := store.InsertSignal(f.ctx, sig)
	if err != nil {
		t.Fatal(err)
	}
	sig.ID = id
	return id, sig
}

// COVER buys back exactly the held short and closes it with short P&L. It
// never touches a long in another ticker and never buys more than is short.
func TestCoverClosesShortExactly(t *testing.T) {
	f := newShortFixture(t, "SHX")
	shortID, err := store.InsertTrade(f.ctx, store.Trade{Ticker: f.ticker, Direction: "short", Strategy: "t", EntryPrice: 100, Quantity: 3, StopLoss: 103, TakeProfit: 94, Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	id, sig := coverSignal(t, f)
	f.e.executeCoverExit(f.ctx, id, sig)
	placed := f.broker.placedOrders()
	if len(placed) != 1 || placed[0].Side != execution.OrderSideBuy || placed[0].Quantity != 3 ||
		!strings.HasPrefix(placed[0].IntentKey, "analyst-exit:") {
		t.Fatalf("cover request = %+v", placed)
	}
	closed, err := store.GetTradeByID(f.ctx, shortID)
	if err != nil || closed == nil || closed.Status != "closed" {
		t.Fatalf("short after cover = %+v (%v)", closed, err)
	}
	// Short P&L: (100 - 90) * 3 = +30.
	if closed.PnL < 29.99 || closed.PnL > 30.01 || closed.ExitReason != store.ExitReasonCoverSignal {
		t.Fatalf("cover pnl=%v reason=%q, want +30 cover_signal", closed.PnL, closed.ExitReason)
	}
	if status, _ := f.signalStatus(id); status != "executed" {
		t.Fatalf("cover signal status %s", status)
	}
}

func TestCoverWithoutShortIsBlockedAndLongUntouched(t *testing.T) {
	f := newShortFixture(t, "SHY")
	longID, err := store.InsertTrade(f.ctx, store.Trade{Ticker: f.ticker, Direction: "long", Strategy: "t", EntryPrice: 90, Quantity: 4, StopLoss: 85, TakeProfit: 100, Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	id, sig := coverSignal(t, f)
	f.e.executeCoverExit(f.ctx, id, sig)
	if status, reason := f.signalStatus(id); status != "blocked" || !strings.Contains(reason, "no open short position") {
		t.Fatalf("cover without short: status=%s reason=%q", status, reason)
	}
	if len(f.broker.placedOrders()) != 0 {
		t.Fatal("cover without a short placed an order")
	}
	long, _ := store.GetTradeByID(f.ctx, longID)
	if long == nil || long.Status != "open" || long.Quantity != 4 {
		t.Fatalf("long touched by cover: %+v", long)
	}

	// A SELL with only a short held is blocked (a long exit never opens a short).
	_, _ = store.GetDB().Exec(f.ctx, `UPDATE trades SET status='closed', closed_at=now() WHERE id=$1::uuid`, longID)
	if _, err := store.InsertTrade(f.ctx, store.Trade{Ticker: f.ticker, Direction: "short", Strategy: "t", EntryPrice: 100, Quantity: 2, StopLoss: 103, TakeProfit: 94, Status: "open"}); err != nil {
		t.Fatal(err)
	}
	sell := store.Signal{Ticker: f.ticker, Action: "SELL", Strategy: "momentum", SignalPrice: 99, Confidence: 0.8, Status: "pending"}
	sellID, err := store.InsertSignal(f.ctx, sell)
	if err != nil {
		t.Fatal(err)
	}
	f.e.executeSellExit(f.ctx, sellID, sell)
	if status, reason := f.signalStatus(sellID); status != "blocked" || !strings.Contains(reason, "no open long position") {
		t.Fatalf("SELL with only a short: status=%s reason=%q", status, reason)
	}
	if len(f.broker.placedOrders()) != 0 {
		t.Fatal("SELL with only a short placed an order")
	}
}

// A stop-leg (buy) fill on a short bracket closes the short with short P&L.
func TestShortBracketStopFillClosesShort(t *testing.T) {
	f := newShortFixture(t, "SHS")
	id, sig := f.signal()
	f.e.executeShortEntry(f.ctx, id, sig, false)
	trades := f.openTrades()
	if len(trades) != 1 {
		t.Fatalf("setup: trades=%+v status=%v", trades, firstNonEmpty(f.signalStatus(id)))
	}
	slID := "parent-1-sl"
	f.broker.mu.Lock()
	f.broker.byID[slID] = execution.OrderResult{BrokerOrderID: slID, Status: "filled", Quantity: 5, FilledQty: 5, FilledPrice: 101, FilledAt: time.Now().UTC()}
	f.broker.byID["parent-1-tp"] = execution.OrderResult{BrokerOrderID: "parent-1-tp", Status: "cancelled", Quantity: 5}
	f.broker.mu.Unlock()
	if handled := f.e.reconcileBracketTrade(f.ctx, trades[0]); !handled {
		t.Fatal("short bracket trade was not broker-managed")
	}
	closed, err := store.GetTradeByID(f.ctx, trades[0].ID)
	if err != nil || closed == nil || closed.Status != "closed" {
		t.Fatalf("short after stop fill = %+v (%v)", closed, err)
	}
	// (98 - 101) * 5 = -15.
	if closed.PnL > -14.99 || closed.PnL < -15.01 {
		t.Fatalf("short stop pnl = %v, want -15", closed.PnL)
	}
}

// ─── Pure helpers ────────────────────────────────────────────────────────────

func TestNormalizedShortBracketPrices(t *testing.T) {
	if sl, tp, ok := normalizedShortBracketPrices(98, 101.004, 91.996); !ok || sl != 101 || tp != 92 {
		t.Fatalf("got %v %v %v", sl, tp, ok)
	}
	for _, c := range [][3]float64{{98, 97, 92}, {98, 101, 99}, {98, 101, 0}, {98, 98.001, 92}} {
		if _, _, ok := normalizedShortBracketPrices(c[0], c[1], c[2]); ok {
			t.Fatalf("incoherent short bracket %v accepted", c)
		}
	}
}

func TestShortFactsFromDailyCandles(t *testing.T) {
	now := time.Now()
	f := shortFactsFromDailyCandles(shortDailyBars("X", now, 97.5, 0, false), now, 10)
	if f.PrevClose != 100 || f.SessionLow != 97.5 || f.SSRCarriedOver || f.AvgDollarVolume != 20_000_000 {
		t.Fatalf("facts = %+v", f)
	}
	if g := shortFactsFromDailyCandles(shortDailyBars("X", now, 97.5, 89, false), now, 10); !g.SSRCarriedOver {
		t.Fatal("yesterday's 11% drop must carry the SSR over")
	}
	if g := shortFactsFromDailyCandles(shortDailyBars("X", now, 97.5, 0, true), now, 10); g.SessionLow != 0 || g.PrevClose != 0 {
		t.Fatalf("no today bar must leave SSR unknown: %+v", g)
	}
}

func TestCapShortQuantity(t *testing.T) {
	if q := capShortQuantity(5, 98, 10_000, 1_700, 0, 20, 10); q != 3 {
		t.Fatalf("gross cap: %v", q)
	}
	if q := capShortQuantity(5, 98, 10_000, 0, 900, 20, 10); q != 1 {
		t.Fatalf("ticker cap: %v", q)
	}
	if q := capShortQuantity(5, 98, 10_000, 2_000, 0, 20, 10); q != 0 {
		t.Fatalf("no room: %v", q)
	}
	if q := capShortQuantity(2, 98, 10_000, 0, 0, 20, 10); q != 2 {
		t.Fatalf("never grows: %v", q)
	}
}

// Reviewer finding: an engine-owned working BUY exit (cover, software exit,
// containment flatten, resting buy stop) is not an entry. It must not wedge
// admission as "unattributed", and must not count as a long position.
func TestWithoutEngineBuyExits(t *testing.T) {
	intents := map[string]*store.StockOrderIntent{
		"te-stock-cover": {IdempotencyKey: "analyst-exit:sig:XYZ"},
		"te-stock-stop":  {IdempotencyKey: "protective-stop:trade"},
		"te-stock-entry": {IdempotencyKey: "entry:sig"},
	}
	lookup := func(_ context.Context, id string) (*store.StockOrderIntent, error) {
		if id == "te-stock-broken" {
			return nil, fmt.Errorf("db down")
		}
		return intents[id], nil
	}
	byBroker := func(_ context.Context, id string) (*store.StockOrderIntent, error) {
		if id == "child-by-broker-id" {
			return &store.StockOrderIntent{IdempotencyKey: "bracket-child:parent:stop-loss"}, nil
		}
		return nil, nil
	}
	working := []execution.WorkingOrder{
		{OrderID: "7", TradeID: "alpaca-generated", ParentOrderID: "short-parent", Side: "buy"},
		{OrderID: "child-by-broker-id", TradeID: "alpaca-generated-2", Side: "buy"},
		{OrderID: "1", TradeID: "te-stock-cover", Side: "buy"},
		{OrderID: "2", TradeID: "te-stock-stop", Side: "buy"},
		{OrderID: "3", TradeID: "te-stock-entry", Side: "buy"},
		{OrderID: "4", TradeID: "te-stock-unknown", Side: "buy"},
		{OrderID: "5", TradeID: "manual-1", Side: "buy"},
		{OrderID: "6", TradeID: "te-stock-cover", Side: "sell"},
	}
	got, err := withoutEngineBuyExits(context.Background(), working, lookup, byBroker)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, o := range got {
		ids = append(ids, o.OrderID)
	}
	if strings.Join(ids, ",") != "3,4,5,6" {
		t.Fatalf("kept %v, want entry, unknown, manual, and the SELL", ids)
	}
	if _, err := withoutEngineBuyExits(context.Background(), []execution.WorkingOrder{{OrderID: "x", TradeID: "te-stock-broken", Side: "buy"}}, lookup, byBroker); err == nil {
		t.Fatal("an unreadable intent must fail closed")
	}
}

func TestShortEntryAdmitsBesideWorkingEngineCover(t *testing.T) {
	f := newShortFixture(t, "SHW")
	other := uniqueTicker("SHWO")
	key := "analyst-exit:" + other + "-sig:" + other
	clientID := "te-stock-cover-" + other
	intent := store.StockOrderIntent{
		IdempotencyKey: key, ClientOrderID: clientID, Ticker: other, Side: "buy", OrderType: "market",
		RequestedQty: 3, RequestPayload: []byte(`{}`), PayloadHash: clientID, Status: "pending",
	}
	if err := store.GetDB().QueryRow(f.ctx, `SELECT uuid_generate_v4()::text`).Scan(&intent.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateStockOrderIntent(f.ctx, intent); err != nil {
		t.Fatalf("CreateStockOrderIntent: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM stock_order_intents WHERE id=$1::uuid`, intent.ID)
	})
	f.broker.working = []execution.WorkingOrder{{
		OrderID: "cover-working", TradeID: clientID, Ticker: other, Side: "buy",
		Status: "pending", RemainingQty: 3, OrderType: "market",
	}}
	id, sig := f.signal()
	f.e.executeShortEntry(f.ctx, id, sig, false)
	if status, reason := f.signalStatus(id); status != "executed" || len(f.broker.placedOrders()) != 1 {
		t.Fatalf("status=%s reason=%q placed=%d; a working engine cover must not block a new short", status, reason, len(f.broker.placedOrders()))
	}
}

// Codex P2: a filled short's bracket children are working BUYs with
// broker-generated client IDs. They are exits, never a phantom long.
func TestShortBracketChildrenAreNotCountedAsLongs(t *testing.T) {
	f := newShortFixture(t, "SHK")
	id, sig := f.signal()
	f.e.executeShortEntry(f.ctx, id, sig, false)
	if len(f.openTrades()) != 1 {
		t.Fatalf("setup: status %v", firstNonEmpty(f.signalStatus(id)))
	}
	// The broker now lists the bracket's two BUY legs as working orders.
	f.broker.mu.Lock()
	f.broker.working = []execution.WorkingOrder{
		{OrderID: "parent-1-tp", TradeID: "alpaca-tp", ParentOrderID: "parent-1", Ticker: f.ticker, Side: "buy", Status: "pending", RemainingQty: 5, Price: 92, OrderType: "limit"},
		{OrderID: "parent-1-sl", TradeID: "alpaca-sl", Ticker: f.ticker, Side: "buy", Status: "pending", RemainingQty: 5, StopPrice: 101, OrderType: "stop"},
	}
	f.broker.positions = []execution.BrokerPosition{{Symbol: f.ticker, Quantity: -5, CurrentPrice: 98, MarketValue: -490, AvgEntryPrice: 98, Side: "short"}}
	f.broker.mu.Unlock()
	other := uniqueTicker("SHKO")
	admission, _, _, release, err := f.e.beginStockEntryAdmissionSide(f.ctx, execution.OrderSideBuy, "entry:k-"+other, other, 1)
	if err != nil {
		t.Fatalf("admission beside a protected short: %v", err)
	}
	release()
	if len(admission.WorkingOrders) != 0 {
		t.Fatalf("bracket children kept as entry inventory: %+v", admission.WorkingOrders)
	}
	if phantom := manualWorkingEntryTrades(admission.WorkingOrders, admission.Reservations, 98); len(phantom) != 0 {
		t.Fatalf("bracket children projected as long positions: %+v", phantom)
	}
}

// Codex P1: a working manual SELL on a flat ticker is a pending short.
func TestShortEntryCountsPendingManualShorts(t *testing.T) {
	t.Run("same ticker blocks", func(t *testing.T) {
		f := newShortFixture(t, "SHM")
		f.broker.working = []execution.WorkingOrder{{OrderID: "manual-sell", TradeID: "manual-1", Ticker: f.ticker, Side: "sell", Status: "pending", RemainingQty: 2, Price: 99, OrderType: "limit"}}
		id, sig := f.signal()
		f.e.executeShortEntry(f.ctx, id, sig, false)
		if status, reason := f.signalStatus(id); status != "blocked" || !strings.Contains(reason, "cannot add to a short") || len(f.broker.placedOrders()) != 0 {
			t.Fatalf("status=%s reason=%q placed=%d", status, reason, len(f.broker.placedOrders()))
		}
	})
	t.Run("other ticker counts toward the gross cap", func(t *testing.T) {
		f := newShortFixture(t, "SHM")
		// $1,700 pending manual short elsewhere leaves $300 of the $2,000 cap.
		f.broker.working = []execution.WorkingOrder{{OrderID: "manual-sell", TradeID: "manual-1", Ticker: "ELSEWHERE", Side: "sell", Status: "pending", RemainingQty: 17, Price: 100, OrderType: "limit"}}
		f.setGrossRoom(0, 1700, 300)
		id, sig := f.signal()
		f.e.executeShortEntry(f.ctx, id, sig, false)
		if placed := f.broker.placedOrders(); len(placed) != 1 || placed[0].Quantity != 3 {
			t.Fatalf("placed=%+v status=%v; want a 3-share short", placed, firstNonEmpty(f.signalStatus(id)))
		}
	})
	t.Run("unpriced manual short fails closed", func(t *testing.T) {
		f := newShortFixture(t, "SHM")
		f.broker.working = []execution.WorkingOrder{{OrderID: "manual-mkt", TradeID: "manual-2", Ticker: "ELSEWHERE", Side: "sell", Status: "pending", RemainingQty: 5, OrderType: "market"}}
		id, sig := f.signal()
		f.e.executeShortEntry(f.ctx, id, sig, false)
		if status, reason := f.signalStatus(id); status != "blocked" || !strings.Contains(reason, "exposure_unknown") {
			t.Fatalf("status=%s reason=%q", status, reason)
		}
	})
	t.Run("sell closing a held long is not a short", func(t *testing.T) {
		f := newShortFixture(t, "SHM")
		f.broker.positions = []execution.BrokerPosition{{Symbol: "LONGHELD", Quantity: 17, CurrentPrice: 100, MarketValue: 1700, AvgEntryPrice: 90, Side: "long"}}
		f.broker.working = []execution.WorkingOrder{{OrderID: "manual-exit", TradeID: "manual-3", Ticker: "LONGHELD", Side: "sell", Status: "pending", RemainingQty: 17, Price: 100, OrderType: "limit"}}
		f.setGrossRoom(0, 0, 2000) // room for the full 5 shares only if the exit is not counted
		id, sig := f.signal()
		f.e.executeShortEntry(f.ctx, id, sig, false)
		if placed := f.broker.placedOrders(); len(placed) != 1 || placed[0].Quantity != 5 {
			t.Fatalf("placed=%+v status=%v; a long exit must not consume short room", placed, firstNonEmpty(f.signalStatus(id)))
		}
	})
}

// Codex P2: an aged short admission and a prepared-short broker miss must be
// durably queued and then picked up by the deferred sweep.
func TestShortSignalsAreRequeuedAndSwept(t *testing.T) {
	f := newShortFixture(t, "SHQ")
	for _, queue := range []struct {
		name string
		fn   func(id string) error
	}{
		{"admission retry", func(id string) error {
			return store.QueueSignalForAdmissionRetry(f.ctx, id, "broker admission aged", true)
		}},
		{"deferred execution", func(id string) error { return store.QueueSignalForDeferredExecution(f.ctx, id) }},
	} {
		t.Run(queue.name, func(t *testing.T) {
			id, _ := f.signal()
			if err := queue.fn(id); err != nil {
				t.Fatal(err)
			}
			var deferred bool
			var status string
			if err := store.GetDB().QueryRow(f.ctx, `SELECT deferred, status::text FROM signals WHERE id=$1::uuid`, id).Scan(&deferred, &status); err != nil || !deferred || status != "pending" {
				t.Fatalf("queued SHORT deferred=%v status=%s (%v); want deferred pending", deferred, status, err)
			}
		})
	}
	// The sweep dispatches an admission-retry SHORT (it is no longer left
	// pending): with the market open it executes, otherwise the release
	// guard blocks it, but it is never skipped.
	id, _ := f.signal()
	if err := store.QueueSignalForAdmissionRetry(f.ctx, id, "broker admission aged", true); err != nil {
		t.Fatal(err)
	}
	f.e.processDeferredSignals(f.ctx)
	got, _ := store.GetSignalByID(f.ctx, id)
	if got == nil || got.Status == "pending" {
		t.Fatalf("admission-retry SHORT was not swept: %+v", got)
	}
}

// Codex round 3: containment cancels the unprotected parent before any
// database read, so a trade that cannot be loaded never leaves the entry
// filling. Only the flatten is withheld.
func TestContainmentCancelsParentEvenWithoutTradeRow(t *testing.T) {
	requireEngineDB(t)
	exec := &fakeBracketExec{statuses: map[string]execution.OrderResult{
		"orphan-parent": {Status: "cancelled", ChildOrders: nil},
	}}
	e := newBracketTestEngine(exec)
	missingTrade := "00000000-0000-0000-0000-000000000000"
	e.containUnprotectedBrokerEntry(context.Background(), missingTrade, "orphan-parent", "NOTRADE", "missing stop")
	if len(exec.cancelled) == 0 || exec.cancelled[0] != "orphan-parent" {
		t.Fatalf("cancelled = %v; the parent must be cancelled before any trade lookup", exec.cancelled)
	}
	if len(exec.placed) != 0 {
		t.Fatalf("flattened without a known trade: %+v", exec.placed)
	}
}

// Containing an unprotected short flattens with a BUY, never a SELL.
func TestContainmentFlattensShortWithBuy(t *testing.T) {
	requireEngineDB(t)
	ctx := context.Background()
	ticker := uniqueTicker("SHCT")
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker: ticker, Direction: "short", Strategy: "test",
		EntryPrice: 100, Quantity: 2, StopLoss: 105, TakeProfit: 90, Status: "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM orders WHERE trade_id=$1::uuid`, tradeID)
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM trades WHERE id=$1::uuid`, tradeID)
	})
	parentID := "short-unprotected-" + tradeID[:8]
	exec := &fakeBracketExec{
		statuses: map[string]execution.OrderResult{
			parentID: {Status: "filled", ChildOrders: []execution.OrderLeg{
				{BrokerOrderID: parentID + "-tp", Type: "limit", Status: "accepted", Quantity: 2, RemainingQty: 2},
				{BrokerOrderID: parentID + "-sl", Type: "stop", Status: "accepted", Quantity: 2, RemainingQty: 2},
			}},
			parentID + "-tp": {Status: "cancelled"},
			parentID + "-sl": {Status: "cancelled"},
		},
		placeRes: execution.OrderResult{BrokerOrderID: "short-containment-exit", Status: "accepted"},
	}
	e := newBracketTestEngine(exec)
	e.containUnprotectedBrokerEntry(ctx, tradeID, parentID, ticker, "missing stop")
	if len(exec.placed) != 1 || exec.placed[0].Side != execution.OrderSideBuy || exec.placed[0].Quantity != 2 {
		t.Fatalf("short containment flatten = %+v; want a 2-share BUY", exec.placed)
	}
}

// Codex round 4: a short tracked by a durable trade takes exactly one
// position slot in BUY admission; an untracked broker short still takes one.
func TestTrackedShortConsumesOneSlotInBuyAdmission(t *testing.T) {
	owned := []store.Trade{{Ticker: "TSLA", Direction: "short", Quantity: 3, EntryPrice: 200}}
	in := buildRiskInput(config.RiskConfig{}, "BUY", "NVDA", .9, 1, 95, 120, 110, portfolio.State{TotalValue: 1_000}, owned)
	if err := mergeBrokerLongOccupancy(&in, []execution.BrokerPosition{
		{Symbol: "TSLA", Side: "short", Quantity: -3, AvgEntryPrice: 200},
		{Symbol: "AMD", Side: "short", Quantity: -1, AvgEntryPrice: 100},
	}, "NVDA", owned); err != nil {
		t.Fatal(err)
	}
	if in.OpenPositions != 2 {
		t.Fatalf("open positions = %d, want 2 (tracked TSLA short once, untracked AMD short once)", in.OpenPositions)
	}
}

// Codex round 4: a stale local long does not exempt a working SELL. Only the
// fresh broker snapshot shows a long that the SELL could be closing.
func TestManualPendingShortsTrustOnlyBrokerLongs(t *testing.T) {
	sell := execution.WorkingOrder{OrderID: "m", TradeID: "manual", Ticker: "STALE", Side: "sell", Status: "pending", RemainingQty: 4, Price: 50}
	if got := manualPendingShorts([]execution.WorkingOrder{sell}, nil, nil); len(got) != 1 {
		t.Fatalf("flat broker: pending shorts = %+v, want the SELL", got)
	}
	brokerLong := []execution.BrokerPosition{{Symbol: "STALE", Side: "long", Quantity: 4}}
	if got := manualPendingShorts([]execution.WorkingOrder{sell}, brokerLong, nil); len(got) != 0 {
		t.Fatalf("broker long: pending shorts = %+v, want none", got)
	}
}

// Codex round 5: a pending manual short takes a position slot once per
// ticker, never double-counted with a broker position or a tracked short.
func TestPendingShortSlots(t *testing.T) {
	pending := []execution.WorkingOrder{
		{Ticker: "NEW1"}, {Ticker: "new1"}, // one ticker, two orders → one slot
		{Ticker: "BROKER"},  // already a broker position
		{Ticker: "TRACKED"}, // already a tracked short
	}
	positions := []execution.BrokerPosition{{Symbol: "BROKER", Side: "short", Quantity: -1}}
	counted := []store.Trade{{Ticker: "TRACKED", Direction: "short", Quantity: 1}}
	if got := pendingShortSlots(pending, positions, counted); got != 1 {
		t.Fatalf("slots = %d, want 1", got)
	}
}

func TestShortEntryPendingManualShortUsesPositionSlot(t *testing.T) {
	f := newShortFixture(t, "SHP")
	f.broker.working = []execution.WorkingOrder{{OrderID: "manual-sell", TradeID: "manual-1", Ticker: "ELSEWHERE", Side: "sell", Status: "pending", RemainingQty: 1, Price: 50, OrderType: "limit"}}
	owned, err := store.GetOwnedPositionTrades(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the positions the shared DB already holds, plus one: with
	// the pending manual short counted, the limit is reached.
	in := buildRiskInput(f.e.cfg.Risk, "SHORT", f.ticker, .8, 5, 101, 92, 98, portfolio.State{}, owned)
	f.e.cfg.Risk.MaxOpenPositions = in.OpenPositions + 1
	id, sig := f.signal()
	f.e.executeShortEntry(f.ctx, id, sig, false)
	if status, reason := f.signalStatus(id); status != "blocked" || !strings.Contains(reason, "open positions") || len(f.broker.placedOrders()) != 0 {
		t.Fatalf("status=%s reason=%q placed=%d; the pending short must take the last slot", status, reason, len(f.broker.placedOrders()))
	}
}
