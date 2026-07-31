package views

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func sampleData() DashboardData {
	return DashboardData{
		Mode:               "paper",
		PaperMode:          true,
		TotalValue:         100000,
		Cash:               60000,
		Invested:           40000,
		TodayPnL:           250.50,
		TodayPnLAvailable:  true,
		UnrealizedPnL:      -120.30,
		PortfolioAvailable: true,
		Positions: []PositionRow{
			{Ticker: "AAPL", EntryPrice: 180, CurrentPrice: 185, Quantity: 10, PnL: 50, PnLPct: 2.78, StopLoss: 175, TakeProfit: 195, Strategy: "default", MarkAvailable: true, MarkAsOf: time.Now(), MarkSource: "broker"},
			{Ticker: "TSLA", EntryPrice: 250, CurrentPrice: 245, Quantity: 5, PnL: -25, PnLPct: -1.0, StopLoss: 240, TakeProfit: 270, Strategy: "default", MarkAvailable: true, MarkAsOf: time.Now(), MarkSource: "broker"},
		},
		PendingSignals: []SignalRow{
			{ID: "sig-12345678-abcd", Ticker: "MSFT", Action: "BUY", Confidence: 0.82, Status: "pending", CreatedAt: time.Now()},
		},
		RecentSignals: []SignalRow{
			{ID: "sig-aaaabbbb-1234", Ticker: "AAPL", Action: "BUY", Confidence: 0.75, Status: "executed", CreatedAt: time.Now()},
			{ID: "sig-ccccdddd-5678", Ticker: "TSLA", Action: "SELL", Confidence: 0.60, Status: "blocked", Reasoning: "High risk environment detected", CreatedAt: time.Now()},
		},
		OpenPositionCnt:     2,
		LargestPosPct:       46.25,
		LargestPosAvailable: true,
		Watchlist:           []string{"AAPL", "TSLA", "MSFT"},
		LastUpdate:          time.Now(),
	}
}

func TestRenderDashboard_ContainsAllSections(t *testing.T) {
	d := sampleData()
	out := RenderDashboard(d, 120, 40, 0, 0)

	sections := []string{
		"PAPER",              // mode header
		"Portfolio",          // portfolio section
		"Open Positions (2)", // positions header
		"AAPL",               // position ticker
		"TSLA",
		"Watchlist (3)",     // watchlist section
		"Pending Approvals", // pending section
		"MSFT",
		"Signal Feed", // signal feed
		"Risk:",       // risk bar
	}

	for _, s := range sections {
		if !strings.Contains(out, s) {
			t.Errorf("expected dashboard to contain %q", s)
		}
	}
}

func TestRenderDashboard_EmptyData(t *testing.T) {
	d := DashboardData{Mode: "manual", LastUpdate: time.Now()}
	out := RenderDashboard(d, 120, 40, 0, 0)

	if !strings.Contains(out, "No open positions") {
		t.Error("expected 'No open positions' for empty data")
	}
	if !strings.Contains(out, "No recent signals") {
		t.Error("expected 'No recent signals' for empty data")
	}
}

func TestRenderPortfolioSummary_ClosedTodayUnavailable(t *testing.T) {
	d := DashboardData{
		PortfolioAvailable: true,
		TotalValue:         100000,
		Cash:               90000,
		Invested:           10000,
	}
	out := ansi.Strip(renderPortfolioSummary(d))
	if !strings.Contains(out, "Closed today (all ledgers): —") {
		t.Fatalf("unavailable closed-today P&L rendered as a value: %q", out)
	}
}

func TestRenderDashboard_AutoMode(t *testing.T) {
	d := DashboardData{Mode: "auto", LastUpdate: time.Now()}
	out := RenderDashboard(d, 120, 40, 0, 0)
	if !strings.Contains(out, "AUTO") {
		t.Error("expected AUTO mode in header")
	}
}

func TestApplyEvent_Relevant(t *testing.T) {
	d := &DashboardData{}
	relevant := []events.Type{
		events.TradeExecuted, events.TradeClosed,
		events.SignalGenerated, events.SignalBlocked,
	}
	for _, et := range relevant {
		if !ApplyEvent(d, events.Event{Type: et}) {
			t.Errorf("expected %s to be relevant", et)
		}
	}
}

func TestApplyEvent_Irrelevant(t *testing.T) {
	d := &DashboardData{}
	if ApplyEvent(d, events.Event{Type: events.DataFetched}) {
		t.Error("expected DataFetched to be irrelevant")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("short", 10) != "short" {
		t.Error("short string should not be truncated")
	}
	long := "this is a very long string that should be truncated"
	result := truncate(long, 20)
	if len(result) != 20 {
		t.Errorf("expected length 20, got %d", len(result))
	}
	if !strings.HasSuffix(result, "...") {
		t.Error("expected truncated string to end with ...")
	}
}

// ── deriveCashAndTotal tests ──────────────────────────────────────────────────

func TestDeriveCashAndTotal_LongProfit(t *testing.T) {
	// Long position: bought 10 shares @ $100 (cost basis $1000).
	// Price rose to $120 → invested $1200, unrealized PnL $200.
	// initialCapital = $10000 → cash = $10000 - $1000 = $9000, total = $9000 + $1200 = $10200.
	cash, total := deriveCashAndTotal(10000, 1000, 1200)
	if cash != 9000 {
		t.Errorf("expected cash=9000, got %.2f", cash)
	}
	if total != 10200 {
		t.Errorf("expected total=10200, got %.2f", total)
	}
}

func TestDeriveCashAndTotal_LongLoss(t *testing.T) {
	// Long position: bought 10 shares @ $100 (cost basis $1000).
	// Price fell to $80 → invested $800.
	// initialCapital = $10000 → cash = $10000 - $1000 = $9000, total = $9000 + $800 = $9800.
	cash, total := deriveCashAndTotal(10000, 1000, 800)
	if cash != 9000 {
		t.Errorf("expected cash=9000, got %.2f", cash)
	}
	if total != 9800 {
		t.Errorf("expected total=9800, got %.2f", total)
	}
}

func TestDeriveCashAndTotal_CashClamp(t *testing.T) {
	// Cost basis exceeds initial capital (e.g. margin/over-allocation edge case).
	// Cash must be clamped to 0, not go negative.
	cash, total := deriveCashAndTotal(500, 1000, 1100)
	if cash != 0 {
		t.Errorf("expected cash clamped to 0, got %.2f", cash)
	}
	if total != 1100 {
		t.Errorf("expected total=1100 (0+invested), got %.2f", total)
	}
}

func TestDeriveCashAndTotal_NoPositions(t *testing.T) {
	// No open positions: cash == initialCapital, total == initialCapital.
	cash, total := deriveCashAndTotal(100000, 0, 0)
	if cash != 100000 {
		t.Errorf("expected cash=100000, got %.2f", cash)
	}
	if total != 100000 {
		t.Errorf("expected total=100000, got %.2f", total)
	}
}

func TestColorPnL(t *testing.T) {
	pos := colorPnL(100)
	neg := colorPnL(-50)
	zero := colorPnL(0)

	if !strings.Contains(pos, "+") {
		t.Error("positive P&L should have +")
	}
	if !strings.Contains(neg, "-") {
		t.Error("negative P&L should have -")
	}
	_ = zero // just shouldn't panic
}

func TestRenderPositionsTable_UsesTruthfulTargetLabels(t *testing.T) {
	d := DashboardData{Positions: []PositionRow{
		{Ticker: "UNCAP", EntryPrice: 100, CurrentPrice: 101, Quantity: 1, StopLoss: 95, TakeProfit: 0, MarkAvailable: true, MarkAsOf: time.Now()},
		{Ticker: "FIXED", EntryPrice: 100, CurrentPrice: 101, Quantity: 1, StopLoss: 95, TakeProfit: 120, MarkAvailable: true, MarkAsOf: time.Now()},
		{Ticker: "UNKNOWN", EntryPrice: 100, Quantity: 1},
	}}
	out := ansi.Strip(renderPositionsTable(d, 140, 0, 0))
	var uncapped, fixed, unknown string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "UNCAP"):
			uncapped = line
		case strings.Contains(line, "FIXED"):
			fixed = line
		case strings.Contains(line, "UNKNOWN"):
			unknown = line
		}
	}
	if !strings.Contains(uncapped, "UNCAPPED") {
		t.Fatalf("uncapped row is not explicit: %q", uncapped)
	}
	if !strings.Contains(fixed, "120.00") {
		t.Fatalf("fixed target must remain numeric: %q", fixed)
	}
	if !strings.Contains(unknown, "—") {
		t.Fatalf("unmarked position must show unavailable values: %q", unknown)
	}
	detail := ansi.Strip(RenderPositionDetailStatic(d.Positions[0], 120, 30))
	if !strings.Contains(detail, "Target: UNCAPPED") {
		t.Fatalf("detail target is not truthful: %q", detail)
	}
}

func TestRenderPositionsTable_AlignsHeadersWithColoredValues(t *testing.T) {
	d := DashboardData{Positions: []PositionRow{{
		Ticker: "ALIGN", EntryPrice: 168.86, CurrentPrice: 170.36,
		Quantity: 10, PnL: 15, PnLPct: 0.9, StopLoss: 156.87,
		Status: "open", Strategy: "momentum", MarkAvailable: true,
		MarkAsOf: time.Now().Add(time.Minute), MarkSource: "broker",
	}}}
	lines := strings.Split(ansi.Strip(renderPositionsTable(d, 160, 0, 0)), "\n")
	if len(lines) < 3 {
		t.Fatalf("rendered table has %d lines, want at least 3", len(lines))
	}
	header, row := lines[1], lines[2]

	// Text fields share a left edge. Numeric fields are right-aligned, so their
	// right edge must match the corresponding heading's right edge.
	for _, pair := range []struct {
		heading, value string
		rightAligned   bool
	}{
		{"Ticker", "ALIGN", false},
		{"Entry", "168.86", true},
		{"Mark", "170.36", true},
		{"P&L", "+$15.00", true},
		{"P&L%", "+0.9%", true},
		{"Stop", "156.87", true},
		{"Target", "UNCAPPED", true},
		{"Age", "B 0s", false},
		{"State", "OPEN", false},
		{"Strategy", "momentum", false},
	} {
		headerAt := strings.Index(header, pair.heading)
		valueAt := strings.Index(row, pair.value)
		if headerAt < 0 || valueAt < 0 {
			t.Fatalf("missing alignment pair %q/%q\nheader: %q\nrow:    %q",
				pair.heading, pair.value, header, row)
		}
		headerAt = ansi.StringWidth(header[:headerAt])
		valueAt = ansi.StringWidth(row[:valueAt])
		if pair.rightAligned {
			headerAt += ansi.StringWidth(pair.heading)
			valueAt += ansi.StringWidth(pair.value)
		}
		if headerAt != valueAt {
			t.Errorf("%s column misaligned: heading edge=%d value edge=%d\nheader: %q\nrow:    %q",
				pair.heading, headerAt, valueAt, header, row)
		}
	}
}

func TestRenderPositionsTable_AdaptsToTerminalWidth(t *testing.T) {
	d := DashboardData{Positions: []PositionRow{{
		Ticker: "LONGTICKER", EntryPrice: 168.86, CurrentPrice: 170.36,
		PnL: -123.45, PnLPct: -7.2, StopLoss: 156.87, Status: "exiting-too-long",
		Strategy: "mean_reversion", MarkAvailable: true, MarkSource: "broker",
		MarkAsOf: time.Now().Add(-744 * time.Hour),
	}}, PositionLots: 12, MalformedTrades: 3, UnmarkedCount: 2,
		OldestMarkAt: time.Now().Add(-744 * time.Hour)}
	for _, width := range []int{80, 100, 120} {
		lines := strings.Split(ansi.Strip(renderPositionsTable(d, width, 0, 0)), "\n")
		if len(lines) < 3 {
			t.Fatalf("width %d: rendered table has %d lines", width, len(lines))
		}
		for _, line := range lines[:3] {
			if got := ansi.StringWidth(line); got > width {
				t.Errorf("width %d: table line is %d cells: %q", width, got, line)
			}
		}
	}
}

func TestRenderPortfolioSummary_ExposesSnapshotSourceAndAge(t *testing.T) {
	asOf := time.Now().Add(-2 * time.Hour)
	out := ansi.Strip(renderPortfolioSummary(DashboardData{
		PortfolioAvailable: true,
		TotalValue:         100000,
		PortfolioSource:    "poller",
		PortfolioAsOf:      asOf,
	}))
	for _, want := range []string{"poller as of", asOf.Local().Format("15:04:05"), "ago"} {
		if !strings.Contains(out, want) {
			t.Fatalf("portfolio summary %q missing %q", out, want)
		}
	}
}

func TestApplyBrokerDashboardSnapshot_IsAuthoritativeAndPreservesMetadata(t *testing.T) {
	d := DashboardData{
		TotalValue: 90000, Cash: 50000, Invested: 40000, UnrealizedPnL: 999,
		DrawdownPeak: 110000,
		Positions: []PositionRow{{
			Ticker: "AAPL", Direction: "long", EntryPrice: 90, CurrentPrice: 95,
			Quantity: 8, PnL: 40, PnLPct: 5.5, StopLoss: 80, Strategy: "momentum",
			MarkAvailable: true, MarkSource: "candle",
		}},
	}
	at := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	account := execution.BrokerAccount{Equity: 100500, LastEquity: 101000, Cash: 80000}
	ApplyBrokerDashboardSnapshot(&d, BrokerDashboardSnapshot{
		Account: &account,
		Positions: []execution.BrokerPosition{{
			Symbol: "AAPL", Side: "long", Quantity: 10, AvgEntryPrice: 100,
			CurrentPrice: 102, MarketValue: 1020, UnrealizedPL: 20,
		}},
		FetchedAt: at,
	}, 3)

	if d.TotalValue != 100500 || d.Cash != 80000 || d.Invested != 1020 || d.UnrealizedPnL != 20 {
		t.Fatalf("broker summary not authoritative: %+v", d)
	}
	if !d.PortfolioAvailable || d.PortfolioEstimated || !d.LargestPosAvailable {
		t.Fatalf("broker availability flags are wrong: %+v", d)
	}
	if len(d.Positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(d.Positions))
	}
	p := d.Positions[0]
	if p.EntryPrice != 100 || p.CurrentPrice != 102 || p.Quantity != 10 || p.PnL != 20 || p.PnLPct != 2 {
		t.Fatalf("broker row economics not authoritative: %+v", p)
	}
	if p.StopLoss != 80 || p.Strategy != "momentum" {
		t.Fatalf("DB metadata was not preserved: %+v", p)
	}
	if p.MarkSource != "broker" || !p.MarkAsOf.Equal(at) {
		t.Fatalf("broker freshness missing: %+v", p)
	}
	wantDrawdown := (110000.0 - 100500.0) / 110000.0 * 100
	if !approx(d.DrawdownPct, wantDrawdown) {
		t.Fatalf("drawdown = %v, want %v after broker equity overlay", d.DrawdownPct, wantDrawdown)
	}
	wantBudget := ((101000.0 - 100500.0) / 101000.0 * 100) / 3 * 100
	if !d.DailyLossAvailable || !approx(d.DailyLossUsed, wantBudget) {
		t.Fatalf("daily loss budget = %v available=%v, want %v/true",
			d.DailyLossUsed, d.DailyLossAvailable, wantBudget)
	}
}

func TestApplyBrokerDashboardSnapshot_RetainsBrokerFlatExitingLifecycle(t *testing.T) {
	d := DashboardData{Positions: []PositionRow{{
		Ticker: "FLAT", Direction: "long", EntryPrice: 10, Quantity: 5,
		StopLoss: 9, Strategy: "momentum", Status: "exiting",
		MarkAvailable: true, CurrentPrice: 11,
	}}}
	account := execution.BrokerAccount{Equity: 100000, Cash: 100000}
	ApplyBrokerDashboardSnapshot(&d, BrokerDashboardSnapshot{
		Account: &account, FetchedAt: time.Now(),
	}, 3)
	if d.OpenPositionCnt != 0 || !d.BrokerAuthoritative {
		t.Fatalf("broker-flat count not authoritative: %+v", d)
	}
	if len(d.Positions) != 1 {
		t.Fatalf("DB lifecycle rows = %d, want 1", len(d.Positions))
	}
	p := d.Positions[0]
	if !p.ReconcileOnly || p.Status != "awaiting" || p.MarkAvailable || p.Quantity != 0 {
		t.Fatalf("broker-flat exiting lifecycle is misleading: %+v", p)
	}
	out := ansi.Strip(renderPositionsTable(d, 140, 0, 0))
	if !strings.Contains(out, "Open Positions (0)") || !strings.Contains(out, "AWAITING") {
		t.Fatalf("broker-flat lifecycle not explicit: %q", out)
	}
}

func TestApplyBrokerDashboardSnapshot_AllUnmarkedHasNoFreshnessTimestamp(t *testing.T) {
	account := execution.BrokerAccount{Equity: 100000, Cash: 90000}
	d := DashboardData{}
	ApplyBrokerDashboardSnapshot(&d, BrokerDashboardSnapshot{
		Account: &account,
		Positions: []execution.BrokerPosition{{
			Symbol: "NOMARK", Side: "long", Quantity: 2, AvgEntryPrice: 10,
		}},
		FetchedAt: time.Now(),
	}, 3)
	if d.UnmarkedCount != 1 || !d.OldestMarkAt.IsZero() {
		t.Fatalf("all-unmarked freshness = count %d at %v, want 1/zero", d.UnmarkedCount, d.OldestMarkAt)
	}
}

func TestApplyBrokerDashboardSnapshot_NonPositiveEquityRemainsAuthoritative(t *testing.T) {
	for _, equity := range []float64{0, -500} {
		d := DashboardData{}
		account := execution.BrokerAccount{Equity: equity, Cash: equity}
		ApplyBrokerDashboardSnapshot(&d, BrokerDashboardSnapshot{
			Account: &account, FetchedAt: time.Now(),
		}, 3)
		if !d.BrokerAuthoritative || !d.PortfolioAvailable || d.TotalValue != equity {
			t.Fatalf("equity %v broker state not authoritative: %+v", equity, d)
		}
	}
}

func TestApplyBrokerDashboardSnapshot_PreservesDBOnlyRowOrder(t *testing.T) {
	d := DashboardData{Positions: []PositionRow{
		{Ticker: "ZZZ", Direction: "long", Quantity: 3, Status: "open"},
		{Ticker: "AAA", Direction: "long", Quantity: 2, Status: "exiting"},
	}}
	account := execution.BrokerAccount{Equity: 100000, Cash: 100000}

	for i := 0; i < 5; i++ {
		ApplyBrokerDashboardSnapshot(&d, BrokerDashboardSnapshot{
			Account: &account, FetchedAt: time.Now(),
		}, 3)
		if got := []string{d.Positions[0].Ticker, d.Positions[1].Ticker}; got[0] != "ZZZ" || got[1] != "AAA" {
			t.Fatalf("refresh %d row order = %v, want [ZZZ AAA]", i, got)
		}
	}
}

func TestSetLargestPositionRiskRequiresCompleteMarks(t *testing.T) {
	d := DashboardData{TotalValue: 100000}
	setLargestPositionRisk(&d, 25000, false)
	if d.LargestPosAvailable || d.LargestPosPct != 0 {
		t.Fatalf("incomplete marks published largest-position risk: %+v", d)
	}

	setLargestPositionRisk(&d, 25000, true)
	if !d.LargestPosAvailable || d.LargestPosPct != 25 {
		t.Fatalf("complete marks did not publish largest-position risk: %+v", d)
	}
}

func TestDailyLossBudgetUsed(t *testing.T) {
	for _, tt := range []struct {
		name                      string
		equity, last, limit, want float64
	}{
		{"half used", 98500, 100000, 3, 50},
		{"breached", 96000, 100000, 3, 133.3333333333},
		{"gain", 101000, 100000, 3, 0},
		{"disabled", 99000, 100000, 0, 0},
		{"no prior equity", 99000, 0, 3, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := dailyLossBudgetUsed(tt.equity, tt.last, tt.limit); !approx(got, tt.want) {
				t.Fatalf("dailyLossBudgetUsed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolvedSnapshotScopePinsWildcardToSelectedBook(t *testing.T) {
	mode, broker := resolvedSnapshotScope("live", "*", &store.PortfolioSnapshot{
		Mode: "live", Broker: "alpaca",
	})
	if mode != "live" || broker != "alpaca" {
		t.Fatalf("resolved scope = (%q, %q), want (live, alpaca)", mode, broker)
	}

	mode, broker = resolvedSnapshotScope("*", "*", &store.PortfolioSnapshot{
		Mode: "paper", Broker: "ibkr",
	})
	if mode != "paper" || broker != "ibkr" {
		t.Fatalf("fully resolved scope = (%q, %q), want (paper, ibkr)", mode, broker)
	}
}

func TestDBExposureSignatureIsOrderIndependentAndLifecycleSensitive(t *testing.T) {
	a := store.Trade{ID: "a", Ticker: "AAPL", Direction: "long", Status: "open", Quantity: 2}
	b := store.Trade{ID: "b", Ticker: "MSFT", Direction: "long", Status: "exiting", Quantity: 3}
	if DBExposureSignature([]store.Trade{a, b}) != DBExposureSignature([]store.Trade{b, a}) {
		t.Fatal("signature changed with query order")
	}
	b.Status = "open"
	if DBExposureSignature([]store.Trade{a, b}) == DBExposureSignature([]store.Trade{
		a, {ID: "b", Ticker: "MSFT", Direction: "long", Status: "exiting", Quantity: 3},
	}) {
		t.Fatal("signature did not change with lifecycle state")
	}
}
