package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// fakeBrokerView returns a fixed slice of broker positions or an error.
type fakeBrokerView struct {
	positions []execution.BrokerPosition
	err       error
}

func (f *fakeBrokerView) PositionsSnapshot(ctx context.Context) ([]execution.BrokerPosition, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.positions, nil
}

// fakeStore returns a fixed slice of open trades.
type fakeStore struct {
	trades []store.Trade
}

func (f *fakeStore) GetOpenTrades(ctx context.Context) ([]store.Trade, error) {
	return f.trades, nil
}

// captureLogs swaps the package logger for a JSON handler writing to a buffer
// and returns (records, restore). The returned slice is populated on each call
// to records().
func captureLogs(t *testing.T) (collect func() []map[string]any, restore func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	restore = logger.SetForTesting(slog.New(h))
	collect = func() []map[string]any {
		out := []map[string]any{}
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("captureLogs: unmarshal failed: %v (line: %q)", err, line)
			}
			out = append(out, rec)
		}
		return out
	}
	return collect, restore
}

func countByLevelMsg(records []map[string]any, level, msg string) int {
	n := 0
	for _, r := range records {
		if r["level"] == level && r["msg"] == msg {
			n++
		}
	}
	return n
}

func findRecord(records []map[string]any, level, msg string, ticker string) map[string]any {
	for _, r := range records {
		if r["level"] != level || r["msg"] != msg {
			continue
		}
		if ticker == "" || r["ticker"] == ticker {
			return r
		}
	}
	return nil
}

func TestReconcile_BothEmpty(t *testing.T) {
	collect, restore := captureLogs(t)
	defer restore()

	ReconcileBrokerVsDB(context.Background(),
		&fakeBrokerView{},
		&fakeStore{},
		0, // use default tolerance
	)
	recs := collect()
	if got := countByLevelMsg(recs, "WARN", "reconciler: broker/DB drift"); got != 0 {
		t.Fatalf("expected 0 drift warnings, got %d", got)
	}
}

func TestReconcile_MatchWithinTolerance(t *testing.T) {
	collect, restore := captureLogs(t)
	defer restore()

	ReconcileBrokerVsDB(context.Background(),
		&fakeBrokerView{positions: []execution.BrokerPosition{
			{Symbol: "AAPL", Quantity: 10.005, Side: "long"},
		}},
		&fakeStore{trades: []store.Trade{
			{Ticker: "AAPL", Quantity: 10.000, Status: "open", Direction: "buy"},
		}},
		0.01,
	)
	if got := countByLevelMsg(collect(), "WARN", "reconciler: broker/DB drift"); got != 0 {
		t.Fatalf("expected 0 warnings within tolerance, got %d", got)
	}
}

func TestReconcile_BrokerOnly(t *testing.T) {
	collect, restore := captureLogs(t)
	defer restore()

	ReconcileBrokerVsDB(context.Background(),
		&fakeBrokerView{positions: []execution.BrokerPosition{
			{Symbol: "JMIA", Quantity: 50, Side: "long"},
		}},
		&fakeStore{trades: nil},
		0.01,
	)
	recs := collect()
	if got := countByLevelMsg(recs, "WARN", "reconciler: broker/DB drift"); got != 1 {
		t.Fatalf("expected 1 drift warning, got %d", got)
	}
	rec := findRecord(recs, "WARN", "reconciler: broker/DB drift", "JMIA")
	if rec == nil {
		t.Fatal("expected JMIA WARN record not found")
	}
	if rec["source"] != "broker_only" {
		t.Fatalf("expected source=broker_only, got %v", rec["source"])
	}
	if rec["broker_qty"].(float64) != 50 {
		t.Fatalf("expected broker_qty=50, got %v", rec["broker_qty"])
	}
	if rec["db_qty"].(float64) != 0 {
		t.Fatalf("expected db_qty=0, got %v", rec["db_qty"])
	}
	if int(rec["db_row_count"].(float64)) != 0 {
		t.Fatalf("expected db_row_count=0, got %v", rec["db_row_count"])
	}
}

// TestReconcile_SkipsOptionPosition verifies the #416 scope-out: an option
// position (reported as an OCC symbol) is skipped by this ticker-keyed reconciler
// — no drift warning, no adoption — because it's owned by the OCC-keyed options
// orphan detector instead.
func TestReconcile_SkipsOptionPosition(t *testing.T) {
	collect, restore := captureLogs(t)
	defer restore()

	ReconcileBrokerVsDB(context.Background(),
		&fakeBrokerView{positions: []execution.BrokerPosition{
			{Symbol: "SPY260717C00551000", Quantity: 4, Side: "long"}, // an option OCC symbol
			{Symbol: "JMIA", Quantity: 50, Side: "long"},              // a normal equity → still drifts
		}},
		&fakeStore{trades: nil},
		0.01,
	)
	recs := collect()
	// Exactly one drift warning — the equity — the option is skipped.
	if got := countByLevelMsg(recs, "WARN", "reconciler: broker/DB drift"); got != 1 {
		t.Fatalf("expected 1 drift warning (equity only), got %d", got)
	}
	if findRecord(recs, "WARN", "reconciler: broker/DB drift", "SPY260717C00551000") != nil {
		t.Fatal("option OCC position must NOT produce a drift warning")
	}
}

func TestReconcile_DBOnly(t *testing.T) {
	collect, restore := captureLogs(t)
	defer restore()

	ReconcileBrokerVsDB(context.Background(),
		&fakeBrokerView{positions: nil},
		&fakeStore{trades: []store.Trade{
			{Ticker: "JMIA", Quantity: 25, Status: "open", Direction: "buy"},
			{Ticker: "JMIA", Quantity: 5, Status: "open", Direction: "buy"},
		}},
		0.01,
	)
	recs := collect()
	if got := countByLevelMsg(recs, "WARN", "reconciler: broker/DB drift"); got != 1 {
		t.Fatalf("expected 1 drift warning, got %d", got)
	}
	rec := findRecord(recs, "WARN", "reconciler: broker/DB drift", "JMIA")
	if rec == nil {
		t.Fatal("expected JMIA WARN record not found")
	}
	if rec["source"] != "db_only" {
		t.Fatalf("expected source=db_only, got %v", rec["source"])
	}
	if rec["db_qty"].(float64) != 30 {
		t.Fatalf("expected db_qty=30, got %v", rec["db_qty"])
	}
	if int(rec["db_row_count"].(float64)) != 2 {
		t.Fatalf("expected db_row_count=2, got %v", rec["db_row_count"])
	}
}

func TestReconcile_MismatchOverTolerance(t *testing.T) {
	collect, restore := captureLogs(t)
	defer restore()

	ReconcileBrokerVsDB(context.Background(),
		&fakeBrokerView{positions: []execution.BrokerPosition{
			{Symbol: "TSLA", Quantity: 12, Side: "long"},
		}},
		&fakeStore{trades: []store.Trade{
			{Ticker: "TSLA", Quantity: 5, Status: "open", Direction: "buy"},
		}},
		0.01,
	)
	recs := collect()
	if got := countByLevelMsg(recs, "WARN", "reconciler: broker/DB drift"); got != 1 {
		t.Fatalf("expected 1 drift warning, got %d", got)
	}
	rec := findRecord(recs, "WARN", "reconciler: broker/DB drift", "TSLA")
	if rec == nil {
		t.Fatal("expected TSLA WARN record not found")
	}
	if rec["source"] != "mismatch" {
		t.Fatalf("expected source=mismatch, got %v", rec["source"])
	}
	if rec["broker_qty"].(float64) != 12 {
		t.Fatalf("expected broker_qty=12, got %v", rec["broker_qty"])
	}
	if rec["db_qty"].(float64) != 5 {
		t.Fatalf("expected db_qty=5, got %v", rec["db_qty"])
	}
}

func TestReconcile_NilBrokerSkipsCleanly(t *testing.T) {
	collect, restore := captureLogs(t)
	defer restore()

	ReconcileBrokerVsDB(context.Background(), nil, &fakeStore{}, 0.01)
	recs := collect()
	if got := countByLevelMsg(recs, "INFO", "reconciler skipped — broker unavailable"); got != 1 {
		t.Fatalf("expected 1 skip INFO, got %d", got)
	}
	if got := countByLevelMsg(recs, "WARN", "reconciler: broker/DB drift"); got != 0 {
		t.Fatalf("expected 0 WARNs when broker is nil, got %d", got)
	}
}

func TestReconcile_BrokerUnavailableErrorSkipsCleanly(t *testing.T) {
	collect, restore := captureLogs(t)
	defer restore()

	ReconcileBrokerVsDB(context.Background(),
		&fakeBrokerView{err: execution.ErrBrokerUnavailable},
		&fakeStore{},
		0.01,
	)
	if got := countByLevelMsg(collect(), "INFO", "reconciler skipped — broker unavailable"); got != 1 {
		t.Fatalf("expected 1 skip INFO, got %d", got)
	}
}

// TestReconcile_SideMismatch covers the canonical side-flip drift the abs-only
// aggregation used to hide: broker reports a -50 short, DB has a 50 long. The
// absolute quantities are equal, but the positions point in opposite
// directions — that's a real, dangerous mismatch.
func TestReconcile_SideMismatch(t *testing.T) {
	collect, restore := captureLogs(t)
	defer restore()

	ReconcileBrokerVsDB(context.Background(),
		&fakeBrokerView{positions: []execution.BrokerPosition{
			{Symbol: "JMIA", Quantity: -50, Side: "short"},
		}},
		&fakeStore{trades: []store.Trade{
			{Ticker: "JMIA", Quantity: 50, Status: "open", Direction: "buy"},
		}},
		0.01,
	)
	recs := collect()
	if got := countByLevelMsg(recs, "WARN", "reconciler: broker/DB drift"); got != 1 {
		t.Fatalf("expected 1 drift warning, got %d", got)
	}
	rec := findRecord(recs, "WARN", "reconciler: broker/DB drift", "JMIA")
	if rec == nil {
		t.Fatal("expected JMIA WARN record not found")
	}
	if rec["source"] != "side_mismatch" {
		t.Fatalf("expected source=side_mismatch, got %v", rec["source"])
	}
	if rec["broker_qty"].(float64) != -50 {
		t.Fatalf("expected broker_qty=-50 (signed), got %v", rec["broker_qty"])
	}
	if rec["db_qty"].(float64) != 50 {
		t.Fatalf("expected db_qty=50 (signed long), got %v", rec["db_qty"])
	}
}

// TestReconcile_BrokerShortPositiveQtyWithSide covers the non-Alpaca shape
// where a broker reports a positive Quantity with Side="short". The reconciler
// must still treat it as a short and flag the side mismatch against a long DB
// row.
func TestReconcile_BrokerShortPositiveQtyWithSide(t *testing.T) {
	collect, restore := captureLogs(t)
	defer restore()

	ReconcileBrokerVsDB(context.Background(),
		&fakeBrokerView{positions: []execution.BrokerPosition{
			{Symbol: "JMIA", Quantity: 50, Side: "short"},
		}},
		&fakeStore{trades: []store.Trade{
			{Ticker: "JMIA", Quantity: 50, Status: "open", Direction: "buy"},
		}},
		0.01,
	)
	recs := collect()
	rec := findRecord(recs, "WARN", "reconciler: broker/DB drift", "JMIA")
	if rec == nil {
		t.Fatal("expected JMIA WARN record not found")
	}
	if rec["source"] != "side_mismatch" {
		t.Fatalf("expected source=side_mismatch, got %v", rec["source"])
	}
}

// TestReconcile_BothShortMatch ensures the signed convention does not
// accidentally flag a legitimate match where both sides are short.
func TestReconcile_BothShortMatch(t *testing.T) {
	collect, restore := captureLogs(t)
	defer restore()

	ReconcileBrokerVsDB(context.Background(),
		&fakeBrokerView{positions: []execution.BrokerPosition{
			{Symbol: "JMIA", Quantity: -50, Side: "short"},
		}},
		&fakeStore{trades: []store.Trade{
			{Ticker: "JMIA", Quantity: 50, Status: "open", Direction: "sell"},
		}},
		0.01,
	)
	if got := countByLevelMsg(collect(), "WARN", "reconciler: broker/DB drift"); got != 0 {
		t.Fatalf("expected 0 warnings when both sides are short and quantities match, got %d", got)
	}
}

func TestReconcile_MultipleTickersDeterministicOrder(t *testing.T) {
	collect, restore := captureLogs(t)
	defer restore()

	ReconcileBrokerVsDB(context.Background(),
		&fakeBrokerView{positions: []execution.BrokerPosition{
			{Symbol: "ZBRA", Quantity: 1},
			{Symbol: "AAPL", Quantity: 9},
		}},
		&fakeStore{trades: []store.Trade{
			{Ticker: "ZBRA", Quantity: 0, Status: "open", Direction: "buy"},
			{Ticker: "MSFT", Quantity: 4, Status: "open", Direction: "buy"},
		}},
		0.01,
	)
	recs := collect()
	if got := countByLevelMsg(recs, "WARN", "reconciler: broker/DB drift"); got != 3 {
		t.Fatalf("expected 3 drift warnings, got %d", got)
	}
	// Ensure the WARNs appear in alphabetical ticker order.
	want := []string{"AAPL", "MSFT", "ZBRA"}
	got := []string{}
	for _, r := range recs {
		if r["msg"] == "reconciler: broker/DB drift" {
			got = append(got, r["ticker"].(string))
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("expected WARN tickers in order %v, got %v", want, got)
	}
}
