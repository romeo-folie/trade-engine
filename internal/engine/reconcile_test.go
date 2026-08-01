package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

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
	trades    []store.Trade
	ownership []store.DurableExitOrderOwnership
}

func (f *fakeStore) GetOpenTrades(ctx context.Context) ([]store.Trade, error) {
	return f.trades, nil
}

func (f *fakeStore) GetDurableExitOrderOwnership(context.Context) ([]store.DurableExitOrderOwnership, error) {
	return f.ownership, nil
}

func TestClassifyPendingExitDriftsRequiresFreshDurableOwnership(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	base := []BrokerDrift{{Ticker: "PSN", BrokerQty: 0, DBQty: 10, Source: "db_only"}}
	owner := store.DurableExitOrderOwnership{
		TradeID: "t1", Ticker: "PSN", TradeStatus: "exiting", TradeQuantity: 10,
		ClaimOwned: true,
	}

	freshAt := now.Add(-19 * time.Minute)
	owner.ExitClaimedAt = &freshAt
	got := classifyPendingExitDrifts(append([]BrokerDrift(nil), base...), []store.DurableExitOrderOwnership{owner}, now)
	if got[0].Source != "exit_pending" || len(AlertableBrokerDrifts(got)) != 0 {
		t.Fatalf("fresh claimed reduction = %+v, want non-alerting exit_pending", got)
	}

	agedAt := now.Add(-20 * time.Minute)
	owner.ExitClaimedAt = &agedAt
	got = classifyPendingExitDrifts(append([]BrokerDrift(nil), base...), []store.DurableExitOrderOwnership{owner}, now)
	if got[0].Source != "db_only" || len(AlertableBrokerDrifts(got)) != 1 {
		t.Fatalf("aged claimed reduction = %+v, want alertable db_only", got)
	}
}

func TestClassifyPendingExitDriftsDoesNotSuppressUnexplainedReduction(t *testing.T) {
	now := time.Now().UTC()
	claimed := now.Add(-time.Minute)
	got := classifyPendingExitDrifts(
		[]BrokerDrift{{Ticker: "MPLT", BrokerQty: 0, DBQty: 20, Source: "db_only"}},
		[]store.DurableExitOrderOwnership{{
			TradeID: "t1", Ticker: "MPLT", TradeStatus: "exiting", TradeQuantity: 5,
			ClaimOwned: true, ExitClaimedAt: &claimed,
		}}, now,
	)
	if got[0].Source != "db_only" {
		t.Fatalf("unexplained reduction source = %q, want db_only", got[0].Source)
	}
}

func TestClassifyPendingExitDriftsCountsOnlyFreshSameTickerClaims(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	oldClaim := now.Add(-30 * time.Minute)
	freshClaim := now.Add(-5 * time.Minute)
	ownership := []store.DurableExitOrderOwnership{
		{TradeID: "old", Ticker: "PSN", TradeStatus: "exiting", TradeQuantity: 8, ClaimOwned: true, ExitClaimedAt: &oldClaim},
		{TradeID: "fresh", Ticker: "PSN", TradeStatus: "exiting", TradeQuantity: 2, ClaimOwned: true, ExitClaimedAt: &freshClaim},
	}

	fullyFresh := classifyPendingExitDrifts(
		[]BrokerDrift{{Ticker: "PSN", BrokerQty: 8, DBQty: 10, Source: "mismatch"}}, ownership, now,
	)
	if fullyFresh[0].Source != "exit_pending" {
		t.Fatalf("freshly explainable reduction = %+v, want exit_pending", fullyFresh[0])
	}
	includesOld := classifyPendingExitDrifts(
		[]BrokerDrift{{Ticker: "PSN", BrokerQty: 0, DBQty: 10, Source: "db_only"}}, ownership, now,
	)
	if includesOld[0].Source == "exit_pending" {
		t.Fatalf("old+fresh reduction was fully suppressed: %+v", includesOld[0])
	}
}

func TestClassifyPendingExitDriftsRejectsUnsafeGraceShapes(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Minute)
	future := now.Add(time.Minute)
	cases := []struct {
		name  string
		drift BrokerDrift
		owner store.DurableExitOrderOwnership
	}{
		{"broker surplus", BrokerDrift{Ticker: "X", BrokerQty: 12, DBQty: 10, Source: "mismatch"}, store.DurableExitOrderOwnership{TradeQuantity: 10, ClaimOwned: true, ExitClaimedAt: &fresh}},
		{"side flip", BrokerDrift{Ticker: "X", BrokerQty: -5, DBQty: 10, Source: "mismatch"}, store.DurableExitOrderOwnership{TradeQuantity: 10, ClaimOwned: true, ExitClaimedAt: &fresh}},
		{"not claim owned", BrokerDrift{Ticker: "X", BrokerQty: 5, DBQty: 10, Source: "mismatch"}, store.DurableExitOrderOwnership{TradeQuantity: 10, ExitClaimedAt: &fresh}},
		{"future claim", BrokerDrift{Ticker: "X", BrokerQty: 5, DBQty: 10, Source: "mismatch"}, store.DurableExitOrderOwnership{TradeQuantity: 10, ClaimOwned: true, ExitClaimedAt: &future}},
		{"open owner", BrokerDrift{Ticker: "X", BrokerQty: 5, DBQty: 10, Source: "mismatch"}, store.DurableExitOrderOwnership{TradeQuantity: 10, TradeStatus: "open", ClaimOwned: true, ExitClaimedAt: &fresh}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.owner.TradeID, tc.owner.Ticker = "t1", "X"
			if tc.owner.TradeStatus == "" {
				tc.owner.TradeStatus = "exiting"
			}
			got := classifyPendingExitDrifts([]BrokerDrift{tc.drift}, []store.DurableExitOrderOwnership{tc.owner}, now)
			if got[0].Source == "exit_pending" {
				t.Fatalf("unsafe drift received grace: %+v owner=%+v", got[0], tc.owner)
			}
		})
	}
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

func TestCompareBrokerVsDBSnapshotsIsSilent(t *testing.T) {
	collect, restore := captureLogs(t)
	defer restore()

	drifts := CompareBrokerVsDBSnapshots(
		[]execution.BrokerPosition{{Symbol: "AAPL", Quantity: 10, Side: "long"}},
		[]store.Trade{{Ticker: "AAPL", Quantity: 5, Direction: "buy", Status: "open"}},
		0,
	)
	if len(drifts) != 1 || drifts[0].Source != "mismatch" {
		t.Fatalf("drifts = %+v, want one mismatch", drifts)
	}
	if got := len(collect()); got != 0 {
		t.Fatalf("silent snapshot comparison emitted %d log records", got)
	}
}

func TestCompareBrokerVsDBSnapshotsNormalizesTickerCase(t *testing.T) {
	drifts := CompareBrokerVsDBSnapshots(
		[]execution.BrokerPosition{{Symbol: "aapl", Quantity: 10, Side: "long"}},
		[]store.Trade{{Ticker: "AAPL", Quantity: 10, Direction: "buy", Status: "open"}},
		0,
	)
	if len(drifts) != 0 {
		t.Fatalf("drifts = %+v, want case-insensitive match", drifts)
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
