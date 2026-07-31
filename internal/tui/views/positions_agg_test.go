package views

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// flatMark returns a markPrice func that gives every ticker the same price at a
// fixed timestamp.
func flatMark(price float64, ts time.Time) func(string) (float64, time.Time) {
	return func(string) (float64, time.Time) { return price, ts }
}

func TestAggregateOpenPositions_WeightedAverageLong(t *testing.T) {
	// Two long lots of AAA: 10 @ $100 and 30 @ $120 → avg entry $115, qty 40.
	// Marked at $130 → PnL = (130-115)*40 = 600.
	trades := []store.Trade{
		{Ticker: "AAA", Direction: "long", EntryPrice: 100, Quantity: 10},
		{Ticker: "AAA", Direction: "long", EntryPrice: 120, Quantity: 30},
	}
	rows, lots, malformed, _, _ := aggregateOpenPositions(trades, flatMark(130, time.Now()))
	if len(rows) != 1 || lots != 2 || malformed != 0 {
		t.Fatalf("rows=%d lots=%d malformed=%d, want 1/2/0", len(rows), lots, malformed)
	}
	r := rows[0]
	if !approx(r.EntryPrice, 115) {
		t.Errorf("weighted entry = %.4f, want 115", r.EntryPrice)
	}
	if !approx(r.Quantity, 40) {
		t.Errorf("qty = %.4f, want 40", r.Quantity)
	}
	if !approx(r.PnL, 600) {
		t.Errorf("PnL = %.4f, want 600", r.PnL)
	}
	if r.Lots != 2 {
		t.Errorf("Lots = %d, want 2", r.Lots)
	}
}

func TestAggregateOpenPositions_ShortPnLSign(t *testing.T) {
	// Short 10 @ $50, marked at $40 → profit (50-40)*10 = 100.
	trades := []store.Trade{{Ticker: "SSS", Direction: "short", EntryPrice: 50, Quantity: 10}}
	rows, _, _, _, _ := aggregateOpenPositions(trades, flatMark(40, time.Now()))
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if !approx(rows[0].PnL, 100) {
		t.Errorf("short PnL = %.4f, want +100", rows[0].PnL)
	}
	if !approx(rows[0].PnLPct, 20) {
		t.Errorf("short PnL%% = %.4f, want 20", rows[0].PnLPct)
	}
}

func TestAggregateOpenPositions_LongAndShortSameTickerStaySeparate(t *testing.T) {
	trades := []store.Trade{
		{Ticker: "AMC", Direction: "long", EntryPrice: 2, Quantity: 100},
		{Ticker: "AMC", Direction: "short", EntryPrice: 3, Quantity: 100},
	}
	rows, _, _, _, _ := aggregateOpenPositions(trades, flatMark(2.5, time.Now()))
	if len(rows) != 2 {
		t.Fatalf("long+short same ticker must be 2 rows, got %d", len(rows))
	}
}

func TestAggregateOpenPositions_MalformedExcluded(t *testing.T) {
	trades := []store.Trade{
		{Ticker: "GOOD", Direction: "long", EntryPrice: 10, Quantity: 5},
		{Ticker: "ZERO_ENTRY", Direction: "long", EntryPrice: 0, Quantity: 5},
		{Ticker: "ZERO_QTY", Direction: "long", EntryPrice: 10, Quantity: 0},
	}
	rows, lots, malformed, _, _ := aggregateOpenPositions(trades, flatMark(11, time.Now()))
	if len(rows) != 1 || lots != 1 || malformed != 2 {
		t.Fatalf("rows=%d lots=%d malformed=%d, want 1/1/2", len(rows), lots, malformed)
	}
	if rows[0].Ticker != "GOOD" {
		t.Errorf("kept the wrong row: %s", rows[0].Ticker)
	}
}

func TestAggregateOpenPositions_FallbackToEntryWhenNoMark(t *testing.T) {
	// markPrice returns 0 → the row remains explicitly unmarked.
	trades := []store.Trade{{Ticker: "NOCANDLE", Direction: "long", EntryPrice: 42, Quantity: 3}}
	noMark := func(string) (float64, time.Time) { return 0, time.Time{} }
	rows, _, _, oldest, unmarked := aggregateOpenPositions(trades, noMark)
	if rows[0].MarkAvailable {
		t.Fatal("missing candle must not masquerade as an available mark")
	}
	if rows[0].CurrentPrice != 0 {
		t.Errorf("current = %.4f, want zero-value unavailable mark", rows[0].CurrentPrice)
	}
	if !oldest.IsZero() || unmarked != 1 {
		t.Errorf("oldest=%v unmarked=%d, want zero/1", oldest, unmarked)
	}
}

func TestAggregateOpenPositions_MultiLotNoMarkRemainsUnavailable(t *testing.T) {
	// Two lots, no candle: 10@$100 + 30@$120 → weighted avg $115, qty 40.
	// With no mark, the aggregate must not fabricate a current value or P&L.
	trades := []store.Trade{
		{Ticker: "AAA", Direction: "long", EntryPrice: 100, Quantity: 10},
		{Ticker: "AAA", Direction: "long", EntryPrice: 120, Quantity: 30},
	}
	noMark := func(string) (float64, time.Time) { return 0, time.Time{} }
	rows, _, _, _, unmarked := aggregateOpenPositions(trades, noMark)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].MarkAvailable || rows[0].CurrentPrice != 0 || unmarked != 1 {
		t.Errorf("unexpected unmarked row: %+v unmarked=%d", rows[0], unmarked)
	}
}

func TestAggregateOpenPositions_MixedLotMetadataIsExplicit(t *testing.T) {
	trades := []store.Trade{
		{Ticker: "MIX", Direction: "long", EntryPrice: 100, Quantity: 1, StopLoss: 95, TakeProfit: 120, Strategy: "momentum", Status: "open"},
		{Ticker: "MIX", Direction: "long", EntryPrice: 101, Quantity: 1, StopLoss: 90, TakeProfit: 0, Strategy: "mean_reversion", Status: "exiting"},
	}
	rows, _, _, _, _ := aggregateOpenPositions(trades, flatMark(105, time.Now()))
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	p := rows[0]
	if !p.ExitPolicyMixed || !p.StrategyMixed || !p.StatusMixed || p.Status != "mixed" {
		t.Fatalf("mixed metadata was not retained: %+v", p)
	}
	out := renderPositionsTable(DashboardData{Positions: rows}, 160, 0, 0)
	if strings.Count(out, "MIXED") < 4 {
		t.Fatalf("mixed stop/target/state/strategy not explicit: %q", out)
	}
}

func TestAggregateOpenPositions_OldestMarkIsMin(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	i := 0
	marks := []struct {
		px float64
		ts time.Time
	}{{10, older}, {10, newer}}
	mark := func(string) (float64, time.Time) { m := marks[i]; i++; return m.px, m.ts }
	trades := []store.Trade{
		{Ticker: "A", Direction: "long", EntryPrice: 9, Quantity: 1},
		{Ticker: "B", Direction: "long", EntryPrice: 9, Quantity: 1},
	}
	rows, _, _, oldest, unmarked := aggregateOpenPositions(trades, mark)
	if !oldest.Equal(older) {
		t.Errorf("oldest = %v, want %v", oldest, older)
	}
	if !rows[0].MarkAsOf.Equal(older) || !rows[1].MarkAsOf.Equal(newer) || unmarked != 0 {
		t.Fatalf("per-row mark timestamps not retained: %+v", rows)
	}
}

func TestPositionsNote(t *testing.T) {
	d := DashboardData{
		Positions:       make([]PositionRow, 25),
		PositionLots:    39,
		MalformedTrades: 3,
		OldestMarkAt:    time.Now().Add(-40 * time.Hour), // stale
	}
	note := positionsNote(d)
	for _, want := range []string{"39 lots", "3 malformed hidden", "oldest mark", "stale"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q missing %q", note, want)
		}
	}

	// Fresh, no lots/malformed → no note.
	empty := positionsNote(DashboardData{Positions: make([]PositionRow, 2), PositionLots: 2})
	if empty != "" {
		t.Errorf("expected empty note, got %q", empty)
	}
}
