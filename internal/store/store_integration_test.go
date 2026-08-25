package store_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// testAnalysisResp is a minimal AnalysisResponse used across analysis tests.
func testAnalysisResp(action analysis.Action) analysis.AnalysisResponse {
	return analysis.AnalysisResponse{
		Action:     action,
		Confidence: 0.80,
		SizePct:    5.0,
		StopLoss:   395.0,
		TakeProfit: 415.0,
		Reasoning:  "integration test reasoning",
	}
}

// testModelMeta returns a minimal ModelCallMeta.
func testModelMeta() analysis.ModelCallMeta {
	return analysis.ModelCallMeta{
		Provider:     "anthropic",
		Model:        "claude-test",
		PromptHash:   "abc123",
		RawPrompt:    "test prompt",
		RawResponse:  `{"action":"BUY"}`,
		InputTokens:  100,
		OutputTokens: 50,
		LatencyMs:    200,
	}
}

func TestGetRealizedPnLByDateRange_AggregatesUSDCompatibleStrategyExits(t *testing.T) {
	ctx := context.Background()
	db := store.GetDB()
	from := time.Date(2040, time.January, 15, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	const ticker = "TEST_DASH_PNL"
	const sessionName = "test-dashboard-pnl"
	const accountID = "test-dashboard-pnl"

	defer func() {
		_, _ = db.Exec(ctx, `DELETE FROM currency_strength_runs WHERE account_id LIKE $1`, accountID+"%")
		_, _ = db.Exec(ctx, `DELETE FROM orb_sessions WHERE date=$1 AND session_name=$2`, from, sessionName)
		_, _ = db.Exec(ctx, `DELETE FROM trades WHERE ticker=$1`, ticker)
	}()

	if _, err := db.Exec(ctx, `
		INSERT INTO trades (
			ticker, direction, strategy, entry_price, exit_price, quantity,
			pnl, pnl_pct, status, opened_at, closed_at, created_at
		) VALUES
			($1, 'long', 'test', 100, 101, 10, 10, 1, 'closed', $2, $3, $2),
			($1, 'long', 'test', 100, 199, 10, 990, 99, 'closed', $2, $4, $2),
			($1, 'long', 'test', 100, NULL, 10, 100, 10, 'open', $2, $5, $2)`,
		ticker, from.Add(time.Hour), from.Add(2*time.Hour), to, from.Add(5*time.Hour)); err != nil {
		t.Fatalf("insert stock P&L fixtures: %v", err)
	}

	var sessionID string
	if err := db.QueryRow(ctx, `
		INSERT INTO orb_sessions (date, status, session_name)
		VALUES ($1, 'done', $2)
		RETURNING id`, from, sessionName).Scan(&sessionID); err != nil {
		t.Fatalf("insert ORB session fixture: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO orb_trades (
			session_id, ticker, side, qty, status, fill_price, pnl, closed_at
		) VALUES
			($1, $2, 'buy', 1, 'closed', 100, -3, $3),
			($1, $2, 'buy', 1, 'closed', 100, 2, $4),
			($1, $2, 'buy', 1, 'closed', 100, NULL, $4),
			($1, $2, 'buy', 1, 'filled', 100, 100, $3),
			($1, 'USD/JPY', 'buy', 1, 'closed', 150, 10000, $3)`,
		sessionID, ticker, from.Add(3*time.Hour), from); err != nil {
		t.Fatalf("insert ORB P&L fixture: %v", err)
	}

	if _, err := db.Exec(ctx, `
		INSERT INTO currency_strength_runs (
			account_id, session_date, status, timezone, strength_start,
			strength_end, exit_time, units, exited_at, realized_pnl
		) VALUES
			($1, $2, 'exited', 'UTC', '06:00', '09:00', '18:00', 100000, $3, 5),
			($4, $2, 'exit_submitted', 'UTC', '06:00', '09:00', '18:00', 100000, $3, 100)`,
		accountID, from, from.Add(4*time.Hour), accountID+"-pending"); err != nil {
		t.Fatalf("insert currency-strength P&L fixture: %v", err)
	}

	got, err := store.GetRealizedPnLByDateRange(ctx, from, to)
	if err != nil {
		t.Fatalf("GetRealizedPnLByDateRange: %v", err)
	}
	if got != 14 {
		t.Fatalf("terminal-exit P&L = %v, want 14", got)
	}
}

func TestPortfolioSnapshotReads_AreScopedByMode(t *testing.T) {
	ctx := context.Background()
	db := store.GetDB()
	at := time.Date(2041, time.February, 3, 12, 0, 0, 0, time.UTC)
	const source = "test-dashboard-scope"

	defer func() {
		_, _ = db.Exec(ctx, `DELETE FROM portfolio_snapshots WHERE source=$1`, source)
	}()
	if _, err := db.Exec(ctx, `
		INSERT INTO portfolio_snapshots (
			total_value, cash, invested, unrealized_pnl, open_positions,
			source, mode, broker, created_at
		) VALUES
			(101000, 90000, 11000, 1000, 1, $1, 'live', 'alpaca', $2),
			(55000, 50000, 5000, 0, 2, $1, 'paper', 'alpaca', $3),
			(777000, 700000, 77000, 0, 3, $1, 'live', 'ibkr', $3),
			(333000, 300000, 33000, 0, 4, $1, 'live', NULL, $4),
			(202000, 180000, 22000, 2000, 2, $1, 'live', 'alpaca', $5)`,
		source, at, at.Add(time.Minute), at.Add(2*time.Minute), at.Add(3*time.Minute)); err != nil {
		t.Fatalf("insert scoped portfolio fixtures: %v", err)
	}
	var backfillID string
	if err := db.QueryRow(ctx, `
		INSERT INTO portfolio_snapshots (
			total_value, cash, invested, unrealized_pnl, open_positions,
			source, mode, broker, created_at
		) VALUES (250000, 0, 0, 0, 0, 'backfill', 'live', 'alpaca', $1)
		RETURNING id`, at.Add(4*time.Minute)).Scan(&backfillID); err != nil {
		t.Fatalf("insert backfill peak fixture: %v", err)
	}
	defer func() {
		_, _ = db.Exec(ctx, `DELETE FROM portfolio_snapshots WHERE id=$1`, backfillID)
	}()

	live, err := store.GetLatestPortfolioSnapshotByScope(ctx, "live", "alpaca")
	if err != nil {
		t.Fatalf("GetLatestPortfolioSnapshotByMode(live): %v", err)
	}
	if live == nil || live.TotalValue != 202000 || live.Mode != "live" {
		t.Fatalf("live snapshot = %+v, want latest Alpaca live 202000", live)
	}
	paper, err := store.GetLatestPortfolioSnapshotByMode(ctx, "paper")
	if err != nil {
		t.Fatalf("GetLatestPortfolioSnapshotByMode(paper): %v", err)
	}
	if paper == nil || paper.TotalValue != 55000 || paper.Mode != "paper" {
		t.Fatalf("paper snapshot = %+v, want paper 55000", paper)
	}
	local, err := store.GetLatestPortfolioSnapshotByScope(ctx, "live", "")
	if err != nil {
		t.Fatalf("GetLatestPortfolioSnapshotByScope(local): %v", err)
	}
	if local == nil || local.TotalValue != 333000 || local.Broker != "" {
		t.Fatalf("local snapshot = %+v, want untagged 333000", local)
	}

	peak, err := store.GetPeakPortfolioValueByScope(ctx, at.Add(-time.Hour), "live", "alpaca")
	if err != nil {
		t.Fatalf("GetPeakPortfolioValueByMode(live): %v", err)
	}
	if peak != 250000 {
		t.Fatalf("live peak = %v, want backfilled broker equity peak 250000", peak)
	}
}

func TestPortfolioSnapshot_PositionStatsAvailabilityRoundTrips(t *testing.T) {
	ctx := context.Background()
	db := store.GetDB()
	const source = "test-position-stats-availability"
	defer func() {
		_, _ = db.Exec(ctx, `DELETE FROM portfolio_snapshots WHERE source=$1`, source)
	}()

	for _, tt := range []struct {
		broker    string
		available bool
	}{
		{broker: "test-unavailable", available: false},
		{broker: "test-available", available: true},
	} {
		if err := store.InsertPortfolioSnapshot(
			ctx, 100000, 90000, 10000, 0, 0, 0,
			tt.available, source, "paper", tt.broker,
		); err != nil {
			t.Fatalf("insert %s snapshot: %v", tt.broker, err)
		}
		got, err := store.GetLatestPortfolioSnapshotByScope(ctx, "paper", tt.broker)
		if err != nil {
			t.Fatalf("read %s snapshot: %v", tt.broker, err)
		}
		if got == nil || got.PositionStatsAvailable != tt.available {
			t.Fatalf("%s availability = %+v, want %v", tt.broker, got, tt.available)
		}
	}
}

func TestCandles_UpsertRefreshesSameTimestampBar(t *testing.T) {
	ctx := context.Background()
	ts := time.Date(2026, time.July, 25, 13, 0, 0, 0, time.UTC)
	first := data.Candle{
		Ticker: "TEST_CANDLE_REFRESH", Timeframe: data.Timeframe1Day,
		Timestamp: ts, Open: 100, High: 101, Low: 99, Close: 100,
		Volume: 1000, VWAP: 100,
	}
	revised := data.Candle{
		Ticker: "TEST_CANDLE_REFRESH", Timeframe: data.Timeframe1Day,
		Timestamp: ts, Open: 100, High: 105, Low: 98, Close: 104,
		Volume: 2500, VWAP: 102.5,
	}
	if err := store.UpsertCandles(ctx, []data.Candle{first}); err != nil {
		t.Fatalf("first UpsertCandles: %v", err)
	}
	if err := store.UpsertCandles(ctx, []data.Candle{first, revised}); err != nil {
		t.Fatalf("revised UpsertCandles: %v", err)
	}
	got, err := store.GetCandles(
		ctx, revised.Ticker, revised.Timeframe, ts, ts,
	)
	if err != nil {
		t.Fatalf("GetCandles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("candles = %d, want 1", len(got))
	}
	if got[0].Open != revised.Open || got[0].High != revised.High ||
		got[0].Low != revised.Low || got[0].Close != revised.Close ||
		got[0].Volume != revised.Volume || got[0].VWAP != revised.VWAP {
		t.Fatalf("stored candle = %+v, want revised %+v", got[0], revised)
	}
}

// ─── Analyses ─────────────────────────────────────────────────────────────────

func TestAnalysis_InsertAndQuery(t *testing.T) {
	ctx := context.Background()

	id, err := store.InsertAnalysis(ctx, "TEST_AN_AAPL", "momentum",
		testAnalysisResp(analysis.ActionBuy), testModelMeta())
	if err != nil {
		t.Fatalf("InsertAnalysis: %v", err)
	}
	if id == "" {
		t.Fatal("InsertAnalysis: empty ID")
	}

	got, err := store.GetAnalysisByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAnalysisByID: %v", err)
	}
	if got == nil {
		t.Fatal("GetAnalysisByID: nil")
	}
	if got.Ticker != "TEST_AN_AAPL" {
		t.Errorf("Ticker: got %q, want TEST_AN_AAPL", got.Ticker)
	}
	if string(got.Action) != "BUY" {
		t.Errorf("Action: got %q, want BUY", got.Action)
	}

	list, err := store.GetAnalysesByTicker(ctx, "TEST_AN_AAPL", 10)
	if err != nil {
		t.Fatalf("GetAnalysesByTicker: %v", err)
	}
	found := false
	for _, a := range list {
		if a.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GetAnalysesByTicker: inserted ID %q not found", id)
	}
}

func TestAnalysis_TimeframeWorkflowRoundTrip(t *testing.T) {
	ctx := context.Background()
	var hasSignalAnalysisIndex bool
	if err := store.GetDB().QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = current_schema()
			  AND indexname = 'idx_signals_analysis_ids_gin'
		)`).Scan(&hasSignalAnalysisIndex); err != nil {
		t.Fatalf("inspect signal analysis index: %v", err)
	}
	if !hasSignalAnalysisIndex {
		t.Fatal("idx_signals_analysis_ids_gin is missing")
	}
	wf := store.TimeframeWorkflowAudit{
		Version: 1, RoutingSource: "profile", Mode: "enforce",
		Primary:      store.TimeframePassAudit{Timeframe: "1d", Action: "BUY", Confidence: .82, SelectedLens: "momentum"},
		Confirmation: &store.TimeframePassAudit{Timeframe: "4h", Status: "confirmed", Action: "BUY", Confidence: .76, SelectedLens: "momentum"},
		FinalAction:  "BUY",
	}
	meta := testModelMeta()
	meta.IndicatorJSON = []byte(`{"RSI":62.5,"EMA20":101.25}`)
	id, err := store.InsertAnalysisWithScannerRunAudit(ctx, "TEST_TF_AUDIT", "momentum", testAnalysisResp(analysis.ActionBuy), meta, "", store.AnalysisAudit{
		SelectionStatus:   analysis.LensSelectionSelected,
		SelectedLens:      "momentum",
		RejectedLenses:    map[string]string{"trend_following": "trend was not established"},
		Regime:            "expansion",
		TimeframeWorkflow: &wf,
	})
	if err != nil {
		t.Fatalf("insert workflow audit: %v", err)
	}
	got, err := store.GetAnalysisByID(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get workflow audit: %v", err)
	}
	var decoded store.TimeframeWorkflowAudit
	if err := json.Unmarshal(got.TimeframeWorkflow, &decoded); err != nil {
		t.Fatalf("decode workflow audit: %v", err)
	}
	if decoded.RoutingSource != "profile" || decoded.Confirmation == nil || decoded.Confirmation.Status != "confirmed" {
		t.Fatalf("workflow audit = %+v", decoded)
	}

	gateJSON, err := json.Marshal([]store.EntryGateDecision{{Name: "momentum_anti_chase", Status: "passed", Reason: "extension acceptable"}})
	if err != nil {
		t.Fatalf("marshal entry gates: %v", err)
	}
	if _, err := store.InsertSignal(ctx, store.Signal{
		Ticker: "TEST_TF_AUDIT", Action: "BUY", Strategy: "momentum",
		SignalPrice: 400, StopLoss: 395, TakeProfit: 415, Confidence: .8, SizePct: 5,
		Status: "pending", ExpiresAt: time.Now().Add(time.Hour), StalePriceMovePct: 2,
		AnalysisIDs: []string{id}, EntrySetup: store.EntrySetupStandard,
		EntrySetupEvidence: "price reclaimed the configured entry level",
		EntryGateDecisions: gateJSON,
	}); err != nil {
		t.Fatalf("insert linked signal: %v", err)
	}
	detail, err := store.GetAnalysisDetail(ctx, id)
	if err != nil || detail == nil {
		t.Fatalf("get analysis detail: %v", err)
	}
	if detail.SelectedLens != "momentum" || detail.Regime != "expansion" || detail.RelatedSignalAction != "BUY" {
		t.Fatalf("detail audit fields = %+v", detail)
	}
	if !strings.Contains(string(detail.IndicatorsRaw), `"RSI": 62.5`) && !strings.Contains(string(detail.IndicatorsRaw), `"RSI":62.5`) {
		t.Fatalf("detail indicators = %s", detail.IndicatorsRaw)
	}
	if !strings.Contains(string(detail.EntryGateDecisionsRaw), "momentum_anti_chase") {
		t.Fatalf("detail entry gates = %s", detail.EntryGateDecisionsRaw)
	}
}

func TestAnalysis_EnsembleInsertAndQuery(t *testing.T) {
	ctx := context.Background()

	ensResults, _ := json.Marshal([]map[string]any{
		{"provider": "anthropic", "model": "claude-test", "action": "BUY", "confidence": 0.85},
	})
	arbResult, _ := json.Marshal(map[string]any{"action": "BUY", "reasoning": "test"})

	rec := store.EnsembleRecord{
		EnsembleResults: ensResults,
		ArbiterResponse: arbResult,
		FastPathed:      false,
		DissentSummary:  "no dissent",
	}

	id, err := store.InsertEnsembleAnalysis(ctx, "TEST_AN_MSFT", "momentum",
		testAnalysisResp(analysis.ActionBuy), testModelMeta(), rec)
	if err != nil {
		t.Fatalf("InsertEnsembleAnalysis: %v", err)
	}

	got, err := store.GetAnalysisByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAnalysisByID after ensemble insert: %v", err)
	}
	if got == nil {
		t.Fatal("GetAnalysisByID: nil")
	}
	if string(got.Action) != "BUY" {
		t.Errorf("Action: got %q, want BUY", got.Action)
	}
}

// TestAnalysis_SummaryEntityJoinOneToOne guards the CompanyName join in
// GetRecentAnalysesSummary / GetAnalysisDetail against fan-out. The entities
// schema only has a case-SENSITIVE UNIQUE(ticker), so two rows whose tickers
// differ only by case can coexist; a naive case-insensitive join would then
// duplicate the analysis row in the list (and push others out of the LIMIT).
func TestAnalysis_SummaryEntityJoinOneToOne(t *testing.T) {
	ctx := context.Background()

	for _, e := range []store.Entity{
		{EntityType: "company", Name: "Case Join Test Co", Ticker: "TEST_CJ_DUP"},
		{EntityType: "commodity", Name: "case join test metal", Ticker: "test_cj_dup"},
	} {
		if _, err := store.UpsertEntity(ctx, e); err != nil {
			t.Fatalf("UpsertEntity %q: %v", e.Ticker, err)
		}
	}

	id, err := store.InsertAnalysis(ctx, "TEST_CJ_DUP", "momentum",
		testAnalysisResp(analysis.ActionBuy), testModelMeta())
	if err != nil {
		t.Fatalf("InsertAnalysis: %v", err)
	}

	sums, err := store.GetRecentAnalysesSummary(ctx, 200)
	if err != nil {
		t.Fatalf("GetRecentAnalysesSummary: %v", err)
	}
	var matches []store.AnalysisSummary
	for _, s := range sums {
		if s.ID == id {
			matches = append(matches, s)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("analysis %s appeared %d times in summary, want exactly 1 (entity join fan-out)", id, len(matches))
	}
	if matches[0].CompanyName != "Case Join Test Co" {
		t.Errorf("CompanyName: got %q, want company entity preferred", matches[0].CompanyName)
	}

	det, err := store.GetAnalysisDetail(ctx, id)
	if err != nil {
		t.Fatalf("GetAnalysisDetail: %v", err)
	}
	if det == nil {
		t.Fatal("GetAnalysisDetail: nil")
	}
	if det.CompanyName != "Case Join Test Co" {
		t.Errorf("detail CompanyName: got %q, want company entity preferred", det.CompanyName)
	}
}

func TestAnalysis_GetModelPerformance(t *testing.T) {
	ctx := context.Background()
	// Just verifies the query executes without error (may return empty rows).
	rows, err := store.GetModelPerformance(ctx, 0)
	if err != nil {
		t.Fatalf("GetModelPerformance: %v", err)
	}
	_ = rows // may be empty in test DB
}

// ─── Backtests ────────────────────────────────────────────────────────────────

func TestSignal_PullbackStrengthRoundTrip(t *testing.T) {
	ctx := context.Background()
	observedAt := time.Now().Add(-5 * time.Minute).Truncate(time.Microsecond)
	id, err := store.InsertSignal(ctx, store.Signal{
		Ticker: "TEST_PB", Action: "BUY", Strategy: "momentum",
		SignalPrice: 100, StopLoss: 98, Confidence: 0.8, SizePct: 1,
		Status: "pending", ExpiresAt: time.Now().Add(time.Hour),
		StalePriceMovePct: 2, PullbackStrength: true, CreatedAt: observedAt,
		EntrySetup:         store.EntrySetupPullbackStrength,
		EntrySetupEvidence: "reclaimed $99.00 support",
	})
	if err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}
	got, err := store.GetSignalByID(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("GetSignalByID: %v", err)
	}
	if !got.PullbackStrength {
		t.Fatal("pullback-strength execution marker was not persisted")
	}
	if got.EntrySetup != store.EntrySetupPullbackStrength ||
		got.EntrySetupEvidence != "reclaimed $99.00 support" {
		t.Fatalf("signal entry attribution = %q / %q",
			got.EntrySetup, got.EntrySetupEvidence)
	}
	if !got.CreatedAt.Equal(observedAt) {
		t.Fatalf("pullback observation boundary = %s, want %s", got.CreatedAt, observedAt)
	}

	tradeID, err := store.InsertTrade(ctx, store.Trade{
		SignalID: id, Ticker: "TEST_PB", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, Quantity: 1, StopLoss: 98,
		EntrySetup:         store.EntrySetupPullbackStrength,
		EntrySetupEvidence: "reclaimed $99.00 support",
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	trade, err := store.GetTradeByID(ctx, tradeID)
	if err != nil || trade == nil {
		t.Fatalf("GetTradeByID: %v", err)
	}
	if trade.EntrySetup != store.EntrySetupPullbackStrength ||
		trade.EntrySetupEvidence != "reclaimed $99.00 support" {
		t.Fatalf("trade entry attribution = %q / %q",
			trade.EntrySetup, trade.EntrySetupEvidence)
	}
}

func TestSignal_EntryGateDecisionsRoundTrip(t *testing.T) {
	ctx := context.Background()
	decisions, _ := json.Marshal([]store.EntryGateDecision{{Name: "trend_alignment", Status: "passed"}, {Name: "runup_anti_chase", Status: "blocked", Reason: "extended"}})
	id, err := store.InsertSignal(ctx, store.Signal{Ticker: "TEST_GATE_AUDIT", Action: "BUY", Strategy: "momentum", SignalPrice: 100, Confidence: .8, SizePct: 1, Status: "blocked", BlockReason: "extended", ExpiresAt: time.Now().Add(time.Hour), EntryGateDecisions: decisions})
	if err != nil {
		t.Fatalf("insert gate audit: %v", err)
	}
	got, err := store.GetSignalByID(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get gate audit: %v", err)
	}
	var decoded []store.EntryGateDecision
	if err := json.Unmarshal(got.EntryGateDecisions, &decoded); err != nil {
		t.Fatalf("decode gate decisions: %v", err)
	}
	if len(decoded) != 2 || decoded[0].Status != "passed" || decoded[1].Reason != "extended" {
		t.Fatalf("gate decisions = %+v", decoded)
	}
}

func TestSignal_QueueAdmissionRetryPreservesSignalAndDedupesMarker(t *testing.T) {
	ctx := context.Background()
	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	id, err := store.InsertSignal(ctx, store.Signal{
		Ticker: "TEST_ADMISSION_RETRY", Action: "BUY", Strategy: "momentum",
		SignalPrice: 100, Confidence: .8, SizePct: 1, Status: "approved", ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("insert signal: %v", err)
	}
	if err := store.QueueSignalForAdmissionRetry(ctx, id, "broker admission aged during anti-chase evidence refresh"); err != nil {
		t.Fatalf("queue admission retry: %v", err)
	}
	if err := store.QueueSignalForAdmissionRetry(ctx, id, "broker admission aged during anti-chase evidence refresh"); err != nil {
		t.Fatalf("dedupe admission retry: %v", err)
	}
	got, err := store.GetSignalByID(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get signal: %v", err)
	}
	if got.Status != "pending" || got.BlockReason == "" {
		t.Fatalf("retry state = status=%q reason=%q", got.Status, got.BlockReason)
	}
	if got.SignalPrice != 100 || !got.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("retry changed signal data: price=%v expiry=%s", got.SignalPrice, got.ExpiresAt)
	}
	var decisions []store.EntryGateDecision
	if err := json.Unmarshal(got.EntryGateDecisions, &decisions); err != nil {
		t.Fatalf("decode decisions: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Name != store.AdmissionRetryGateName || decisions[0].Status != "retry_pending" {
		t.Fatalf("retry decisions = %+v", decisions)
	}
	deferred, err := store.GetDeferredSignals(ctx)
	if err != nil {
		t.Fatalf("get deferred signals: %v", err)
	}
	for _, signal := range deferred {
		if signal.ID == id && signal.Deferred {
			return
		}
	}
	t.Fatal("admission retry was not eligible for the deferred sweep")
}

func TestBacktest_FullLifecycle(t *testing.T) {
	ctx := context.Background()

	label := "INTG-BACKTEST-" + time.Now().Format("150405")
	run := store.BacktestRun{
		Label:            label,
		Ticker:           "TEST_BT_SPY",
		Strategy:         "momentum",
		ModelProvider:    "anthropic",
		ModelName:        "claude-test",
		FromDate:         time.Now().Add(-30 * 24 * time.Hour),
		ToDate:           time.Now(),
		ConfigSnapshot:   json.RawMessage(`{}`),
		RunOptions:       json.RawMessage(`{"pullback_strength":true}`),
		EntryDiagnostics: json.RawMessage(`{"candidates":3,"qualified":1,"blocks":{"gap_invalidation":1}}`),
	}

	runLabel, err := store.InsertRun(ctx, run)
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	if runLabel == "" {
		t.Fatal("InsertRun: empty label")
	}

	got, err := store.GetRunByLabel(ctx, runLabel)
	if err != nil {
		t.Fatalf("GetRunByLabel: %v", err)
	}
	if got == nil {
		t.Fatal("GetRunByLabel: nil")
	}
	if got.Ticker != "TEST_BT_SPY" {
		t.Errorf("Ticker: got %q, want TEST_BT_SPY", got.Ticker)
	}
	if !strings.Contains(string(got.RunOptions), "pullback_strength") ||
		!strings.Contains(string(got.EntryDiagnostics), "gap_invalidation") {
		t.Errorf("replay metadata not persisted: options=%s diagnostics=%s",
			got.RunOptions, got.EntryDiagnostics)
	}

	// Insert a trade for this run.
	exitTS := time.Now()
	exitPrice := 505.0
	pnl := 5.0
	pnlPct := 1.0
	bt := store.BacktestTrade{
		RunID:      got.ID,
		Ticker:     "TEST_BT_SPY",
		Direction:  "long",
		EntryTS:    time.Now().Add(-10 * time.Hour),
		ExitTS:     &exitTS,
		EntryPrice: 500.0,
		ExitPrice:  &exitPrice,
		Quantity:   10.0,
		PnL:        &pnl,
		PnLPct:     &pnlPct,
		ExitReason: "take_profit",
	}
	if err := store.InsertBacktestTrade(ctx, bt); err != nil {
		t.Fatalf("InsertBacktestTrade: %v", err)
	}

	trades, err := store.ListTradesForRun(ctx, got.ID)
	if err != nil {
		t.Fatalf("ListTradesForRun: %v", err)
	}
	if len(trades) == 0 {
		t.Error("ListTradesForRun: expected at least 1 trade")
	}

	// ListRuns — should include this run.
	runs, err := store.ListRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	found := false
	for _, r := range runs {
		if r.ID == got.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListRuns: run %q not found", got.ID)
	}
}

// ─── Entities ─────────────────────────────────────────────────────────────────

func TestEntity_UpsertAndQuery(t *testing.T) {
	ctx := context.Background()

	e := store.Entity{
		EntityType:  "company",
		Name:        "INTG Test Corp",
		Ticker:      "INTG_TST",
		Description: "integration test entity",
		Sector:      "technology",
		Country:     "US",
	}

	id, err := store.UpsertEntity(ctx, e)
	if err != nil {
		t.Fatalf("UpsertEntity: %v", err)
	}
	if id == "" {
		t.Fatal("UpsertEntity: empty ID")
	}

	byTicker, err := store.GetEntityByTicker(ctx, "INTG_TST")
	if err != nil {
		t.Fatalf("GetEntityByTicker: %v", err)
	}
	if byTicker == nil {
		t.Fatal("GetEntityByTicker: nil")
	}
	if byTicker.Name != "INTG Test Corp" {
		t.Errorf("Name: got %q, want INTG Test Corp", byTicker.Name)
	}

	byID, err := store.GetEntityByID(ctx, id)
	if err != nil {
		t.Fatalf("GetEntityByID: %v", err)
	}
	if byID == nil {
		t.Fatal("GetEntityByID: nil")
	}
	if byID.ID != id {
		t.Errorf("ID: got %q, want %q", byID.ID, id)
	}

	list, err := store.ListEntities(ctx, "company")
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	found := false
	for _, en := range list {
		if en.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListEntities: entity %q not found", id)
	}
}

// ─── Relationships ─────────────────────────────────────────────────────────────

func TestRelationship_LifecycleAndMentions(t *testing.T) {
	ctx := context.Background()

	// Need two entities to relate.
	fromID, err := store.UpsertEntity(ctx, store.Entity{
		EntityType: "company", Name: "INTG From Corp", Ticker: "INTG_FROM",
	})
	if err != nil {
		t.Fatalf("UpsertEntity (from): %v", err)
	}
	toID, err := store.UpsertEntity(ctx, store.Entity{
		EntityType: "company", Name: "INTG To Corp", Ticker: "INTG_TO",
	})
	if err != nil {
		t.Fatalf("UpsertEntity (to): %v", err)
	}

	conf := 0.75
	relID, err := store.UpsertRelationship(ctx, store.Relationship{
		FromEntityID:     fromID,
		ToEntityID:       toID,
		RelationshipType: "competes_with",
		Strength:         0.8,
		Description:      "integration test relationship",
		AutoGenerated:    true,
		Confidence:       &conf,
	})
	if err != nil {
		t.Fatalf("UpsertRelationship: %v", err)
	}
	if relID == "" {
		t.Fatal("UpsertRelationship: empty ID")
	}

	rels, err := store.GetRelationshipsForEntity(ctx, fromID)
	if err != nil {
		t.Fatalf("GetRelationshipsForEntity: %v", err)
	}
	found := false
	for _, r := range rels {
		if r.ID == relID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GetRelationshipsForEntity: relationship %q not found", relID)
	}

	// 2-hop relationships.
	hops, err := store.Get2HopRelationships(ctx, fromID)
	if err != nil {
		t.Fatalf("Get2HopRelationships: %v", err)
	}
	_ = hops

	// Pending auto-generated relationships.
	pending, err := store.GetPendingAutoRelationships(ctx)
	if err != nil {
		t.Fatalf("GetPendingAutoRelationships: %v", err)
	}
	_ = pending

	// UpsertEntityMention — requires a real news item DB ID.
	newsExtID := "intg-news-rel-" + time.Now().Format("150405")
	if _, err := store.UpsertNewsItems(ctx, []data.NewsItem{
		{
			ExternalID:  newsExtID,
			Headline:    "Test news for entity mention",
			URL:         "https://example.com/intg/mention/" + newsExtID,
			Source:      "test",
			PublishedAt: time.Now(),
			Tickers:     []string{"INTG_FROM"},
		},
	}); err != nil {
		t.Fatalf("UpsertNewsItems for mention: %v", err)
	}

	db := store.GetDB()
	var newsDBID string
	if err := db.QueryRow(ctx, `SELECT id FROM news_items WHERE external_id=$1`, newsExtID).Scan(&newsDBID); err != nil {
		t.Fatalf("news ID lookup: %v", err)
	}

	relevance := 0.9
	if err := store.UpsertEntityMention(ctx, fromID, newsDBID, &relevance); err != nil {
		t.Fatalf("UpsertEntityMention: %v", err)
	}

	newsIDs, err := store.GetNewsIDsForEntity(ctx, fromID, 10)
	if err != nil {
		t.Fatalf("GetNewsIDsForEntity: %v", err)
	}
	foundNews := false
	for _, nid := range newsIDs {
		if nid == newsDBID {
			foundNews = true
			break
		}
	}
	if !foundNews {
		t.Errorf("GetNewsIDsForEntity: news item %q not found", newsDBID)
	}

	// DeleteRelationship.
	if err := store.DeleteRelationship(ctx, relID); err != nil {
		t.Fatalf("DeleteRelationship: %v", err)
	}

	afterDel, err := store.GetRelationshipsForEntity(ctx, fromID)
	if err != nil {
		t.Fatalf("GetRelationshipsForEntity after delete: %v", err)
	}
	for _, r := range afterDel {
		if r.ID == relID {
			t.Errorf("relationship %q still present after DeleteRelationship", relID)
		}
	}
}

// ─── News ─────────────────────────────────────────────────────────────────────

func TestNews_UpsertAndQuery(t *testing.T) {
	ctx := context.Background()
	extID := "intg-news-" + time.Now().Format("150405.000")
	published := time.Now().Add(-1 * time.Hour)

	items := []data.NewsItem{
		{
			ExternalID:     extID,
			Headline:       "Integration test headline",
			Summary:        "Integration test summary",
			Source:         "finnhub",
			URL:            "https://example.com/intg/" + extID,
			PublishedAt:    published,
			Tickers:        []string{"TEST_NEWS_AAPL"},
			SentimentScore: 0.5,
		},
	}
	if _, err := store.UpsertNewsItems(ctx, items); err != nil {
		t.Fatalf("UpsertNewsItems: %v", err)
	}

	// Idempotency — re-upsert should not error.
	if _, err := store.UpsertNewsItems(ctx, items); err != nil {
		t.Fatalf("UpsertNewsItems (idempotent): %v", err)
	}

	since := published.Add(-1 * time.Minute)
	got, err := store.GetRecentNews(ctx, "TEST_NEWS_AAPL", since)
	if err != nil {
		t.Fatalf("GetRecentNews: %v", err)
	}
	found := false
	for _, n := range got {
		if n.ExternalID == extID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GetRecentNews: item %q not found", extID)
	}
}

// ─── Orders ───────────────────────────────────────────────────────────────────

func TestNews_UpsertMergesTickerIntoNullAssociation(t *testing.T) {
	ctx := context.Background()
	extID := "intg-news-null-tickers-" + time.Now().Format("150405.000")
	url := "https://example.com/" + extID
	published := time.Now().Add(-time.Hour)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(ctx, "DELETE FROM news_items WHERE external_id=$1", extID)
	})

	if _, err := store.GetDB().Exec(ctx, `
		INSERT INTO news_items (external_id, headline, source, url, published_at, tickers)
		VALUES ($1, 'nullable ticker association', 'test', $2, $3, NULL)
	`, extID, url, published); err != nil {
		t.Fatalf("seed null ticker association: %v", err)
	}
	if _, err := store.UpsertNewsItems(ctx, []data.NewsItem{{
		ExternalID: extID, Headline: "nullable ticker association", Source: "test", URL: url,
		PublishedAt: published, Tickers: []string{"MSFT"},
	}}); err != nil {
		t.Fatalf("upsert ticker association: %v", err)
	}

	var tickers []string
	if err := store.GetDB().QueryRow(ctx, "SELECT tickers FROM news_items WHERE external_id=$1", extID).Scan(&tickers); err != nil {
		t.Fatalf("read merged ticker association: %v", err)
	}
	if len(tickers) != 1 || tickers[0] != "MSFT" {
		t.Fatalf("merged tickers = %v, want [MSFT]", tickers)
	}
	items, err := store.GetRecentNews(ctx, "MSFT", published.Add(-time.Minute))
	if err != nil {
		t.Fatalf("query merged ticker association: %v", err)
	}
	if len(items) != 1 || items[0].ExternalID != extID {
		t.Fatalf("MSFT news = %v, want %q", items, extID)
	}
}

func TestOrder_InsertAndUpdate(t *testing.T) {
	ctx := context.Background()

	// Insert a trade to attach the order to.
	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker:     "TEST_ORD_NVDA",
		Direction:  "long",
		Strategy:   "momentum",
		EntryPrice: 800.0,
		Quantity:   5.0,
	})
	if err != nil {
		t.Fatalf("InsertTrade for order test: %v", err)
	}

	orderID, err := store.InsertOrder(ctx, store.Order{
		TradeID:      tradeID,
		Ticker:       "TEST_ORD_NVDA",
		Side:         "buy",
		OrderType:    "market",
		Quantity:     5.0,
		Status:       "pending",
		ExitReason:   store.ExitReasonTrailingATR,
		ExitEvidence: json.RawMessage(`{"trailing_stop":790}`),
	})
	if err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if orderID == "" {
		t.Fatal("InsertOrder: empty ID")
	}

	orders, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("GetOrdersByTrade: %v", err)
	}
	found := false
	for _, o := range orders {
		if o.ID == orderID {
			found = true
			var evidence map[string]any
			if err := json.Unmarshal(o.ExitEvidence, &evidence); err != nil || o.ExitReason != store.ExitReasonTrailingATR || evidence["trailing_stop"] != 790.0 {
				t.Errorf("order exit provenance = %q %s", o.ExitReason, o.ExitEvidence)
			}
			break
		}
	}
	if !found {
		t.Errorf("GetOrdersByTrade: order %q not found", orderID)
	}

	if err := store.UpdateOrderFill(ctx, orderID, 802.50, 5.0, "filled"); err != nil {
		t.Fatalf("UpdateOrderFill: %v", err)
	}

	afterFill, err := store.GetOrdersByTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("GetOrdersByTrade after fill: %v", err)
	}
	for _, o := range afterFill {
		if o.ID == orderID {
			if o.Status != "filled" {
				t.Errorf("Status after fill: got %q, want filled", o.Status)
			}
			if o.FilledPrice != 802.50 {
				t.Errorf("FilledPrice: got %v, want 802.50", o.FilledPrice)
			}
			break
		}
	}
}

// TestLatestStopLossOrderForTrade_OrderTypeFilter pins the order_type filter
// on LatestStopLossOrderForTrade. The codebase records market exit orders
// with is_stop_loss=TRUE (it tags the order's purpose, not its type), so a
// trade can have BOTH a live STOP and a market exit row that both satisfy
// is_stop_loss + status. Without the order_type IN ('stop','stop_limit')
// guard the query could return the market order and the trailing-stop
// manager would try to PATCH/cancel it — racing the broker fill.
func TestLatestStopLossOrderForTrade_OrderTypeFilter(t *testing.T) {
	ctx := context.Background()

	tradeID, err := store.InsertTrade(ctx, store.Trade{
		Ticker:     "TEST_LSL_AAPL",
		Direction:  "long",
		Strategy:   "momentum",
		EntryPrice: 100.0,
		Quantity:   10,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	// Pre-existing live STOP — what the manager should find.
	stopID, err := store.InsertOrder(ctx, store.Order{
		TradeID:    tradeID,
		Ticker:     "TEST_LSL_AAPL",
		Side:       "sell",
		OrderType:  "stop",
		Quantity:   10,
		StopPrice:  95.0,
		Status:     "pending",
		IsStopLoss: true,
	})
	if err != nil {
		t.Fatalf("InsertOrder stop: %v", err)
	}

	// Trail-driven market exit recorded with is_stop_loss=true — the bug
	// would surface this row instead of the STOP above.
	if _, err := store.InsertOrder(ctx, store.Order{
		TradeID:    tradeID,
		Ticker:     "TEST_LSL_AAPL",
		Side:       "sell",
		OrderType:  "market",
		Quantity:   10,
		Status:     "pending",
		IsStopLoss: true,
	}); err != nil {
		t.Fatalf("InsertOrder market: %v", err)
	}

	got, err := store.LatestStopLossOrderForTrade(ctx, tradeID)
	if err != nil {
		t.Fatalf("LatestStopLossOrderForTrade: %v", err)
	}
	if got == nil {
		t.Fatal("LatestStopLossOrderForTrade: nil result")
	}
	if got.ID != stopID {
		t.Errorf("returned wrong order: got id=%s (type=%s); want STOP id=%s",
			got.ID, got.OrderType, stopID)
	}
	if got.OrderType != "stop" {
		t.Errorf("returned non-stop order_type: %q", got.OrderType)
	}
}

// ─── Overrides ────────────────────────────────────────────────────────────────

func TestOverride_CRUD(t *testing.T) {
	ctx := context.Background()
	const key = "intg_test_override_key"

	t.Cleanup(func() {
		_ = store.DeleteOverride(ctx, key)
	})

	// GetOverride — key not set yet.
	_, ok, err := store.GetOverride(ctx, key)
	if err != nil {
		t.Fatalf("GetOverride (missing): %v", err)
	}
	if ok {
		t.Error("GetOverride: expected not-found before set")
	}

	// Set.
	if err := store.SetOverride(ctx, key, "test_value"); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}

	// Get.
	val, ok, err := store.GetOverride(ctx, key)
	if err != nil {
		t.Fatalf("GetOverride after set: %v", err)
	}
	if !ok {
		t.Fatal("GetOverride: not found after set")
	}
	if val != "test_value" {
		t.Errorf("Value: got %q, want test_value", val)
	}

	// ListOverrides.
	list, err := store.ListOverrides(ctx)
	if err != nil {
		t.Fatalf("ListOverrides: %v", err)
	}
	found := false
	for _, o := range list {
		if o.Key == key {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListOverrides: key %q not found", key)
	}

	// Update via SetOverride (upsert).
	if err := store.SetOverride(ctx, key, "updated_value"); err != nil {
		t.Fatalf("SetOverride (update): %v", err)
	}
	val2, _, _ := store.GetOverride(ctx, key)
	if val2 != "updated_value" {
		t.Errorf("Value after update: got %q, want updated_value", val2)
	}

	// Delete.
	if err := store.DeleteOverride(ctx, key); err != nil {
		t.Fatalf("DeleteOverride: %v", err)
	}
	_, ok2, _ := store.GetOverride(ctx, key)
	if ok2 {
		t.Error("GetOverride: key still present after delete")
	}
}

// ─── Trades ───────────────────────────────────────────────────────────────────

func TestTrade_LifecycleAndQuery(t *testing.T) {
	ctx := context.Background()

	id, err := store.InsertTrade(ctx, store.Trade{
		Ticker:     "TEST_TR_GOOG",
		Direction:  "long",
		Strategy:   "momentum",
		EntryPrice: 175.0,
		Quantity:   20.0,
		StopLoss:   170.0,
		TakeProfit: 185.0,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	byID, err := store.GetTradeByID(ctx, id)
	if err != nil {
		t.Fatalf("GetTradeByID: %v", err)
	}
	if byID == nil {
		t.Fatal("GetTradeByID: nil")
	}
	if byID.Status != "open" {
		t.Errorf("Status: got %q, want open", byID.Status)
	}

	open, err := store.GetOpenTrades(ctx)
	if err != nil {
		t.Fatalf("GetOpenTrades: %v", err)
	}
	foundOpen := false
	for _, tr := range open {
		if tr.ID == id {
			foundOpen = true
			break
		}
	}
	if !foundOpen {
		t.Errorf("GetOpenTrades: trade %q not found", id)
	}

	if err := store.CloseTrade(ctx, id, 182.0, 140.0, 8.0); err != nil {
		t.Fatalf("CloseTrade: %v", err)
	}

	closed, err := store.GetTradeByID(ctx, id)
	if err != nil {
		t.Fatalf("GetTradeByID after close: %v", err)
	}
	if closed.Status != "closed" {
		t.Errorf("Status after close: got %q, want closed", closed.Status)
	}

	recent, err := store.GetRecentTrades(ctx, 10)
	if err != nil {
		t.Fatalf("GetRecentTrades: %v", err)
	}
	foundRecent := false
	for _, tr := range recent {
		if tr.ID == id {
			foundRecent = true
			break
		}
	}
	if !foundRecent {
		t.Errorf("GetRecentTrades: closed trade %q not found", id)
	}
}

// TestTrade_ConsolidatedExcludedFromOpen verifies that the migration that
// added the `consolidated` trade_status enum value (issue #311) is wired
// correctly: GetOpenTrades must NOT return rows whose status was set to
// 'consolidated' by the JMIA collapse migration.
//
// The test inserts two open rows for the same ticker, marks one
// 'consolidated' with consolidated_into pointing at the other, and asserts
// only the canonical row appears in GetOpenTrades.
func TestTrade_ConsolidatedExcludedFromOpen(t *testing.T) {
	ctx := context.Background()

	canonicalID, err := store.InsertTrade(ctx, store.Trade{
		Ticker:     "TEST_TR_CONSOL",
		Direction:  "long",
		Strategy:   "momentum",
		EntryPrice: 10.0,
		Quantity:   100.0,
	})
	if err != nil {
		t.Fatalf("InsertTrade canonical: %v", err)
	}
	staleID, err := store.InsertTrade(ctx, store.Trade{
		Ticker:     "TEST_TR_CONSOL",
		Direction:  "long",
		Strategy:   "momentum",
		EntryPrice: 11.0,
		Quantity:   50.0,
	})
	if err != nil {
		t.Fatalf("InsertTrade stale: %v", err)
	}

	// Apply the same mutation the consolidate command performs.
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE trades
		SET status='consolidated', consolidated_into=$1
		WHERE id=$2`, canonicalID, staleID); err != nil {
		t.Fatalf("mark consolidated: %v", err)
	}

	open, err := store.GetOpenTrades(ctx)
	if err != nil {
		t.Fatalf("GetOpenTrades: %v", err)
	}
	for _, tr := range open {
		if tr.ID == staleID {
			t.Errorf("GetOpenTrades returned consolidated row %q (must be excluded)", staleID)
		}
	}

	// Sanity: the canonical row IS still open.
	foundCanonical := false
	for _, tr := range open {
		if tr.ID == canonicalID {
			foundCanonical = true
			break
		}
	}
	if !foundCanonical {
		t.Errorf("GetOpenTrades: canonical row %q missing", canonicalID)
	}

	// Sanity: the consolidated row is still in the table (preserved, not deleted)
	// and carries the expected back-pointer.
	got, err := store.GetTradeByID(ctx, staleID)
	if err != nil {
		t.Fatalf("GetTradeByID stale: %v", err)
	}
	if got == nil {
		t.Fatalf("consolidated row %q was deleted; should be preserved", staleID)
	}
	if got.Status != "consolidated" {
		t.Errorf("status: got %q, want consolidated", got.Status)
	}

	// Verify the consolidated_into pointer via direct SELECT (column not on
	// the Trade struct because it's collapse-only metadata).
	var pointsAt string
	if err := store.GetDB().QueryRow(ctx,
		`SELECT consolidated_into::text FROM trades WHERE id=$1`, staleID,
	).Scan(&pointsAt); err != nil {
		t.Fatalf("query consolidated_into: %v", err)
	}
	if pointsAt != canonicalID {
		t.Errorf("consolidated_into: got %q, want %q", pointsAt, canonicalID)
	}
}

// TestCountOpenTrades_AgreesWithGetOpenTrades pins the contract that
// CountOpenTrades and len(GetOpenTrades) return the same value at every
// step. Both run independent SQL today; if a future filter is added to
// one and forgotten on the other, the canvas would silently disagree
// with the count badge. This test catches that drift.
func TestCountOpenTrades_AgreesWithGetOpenTrades(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()

	// Capture the baseline so the assertions are robust to any rows
	// other tests left behind in the shared schema.
	baselineCount, err := store.CountOpenTrades(ctx)
	if err != nil {
		t.Fatalf("CountOpenTrades baseline: %v", err)
	}
	baselineList, err := store.GetOpenTrades(ctx)
	if err != nil {
		t.Fatalf("GetOpenTrades baseline: %v", err)
	}
	if baselineCount != len(baselineList) {
		t.Fatalf("baseline disagreement: CountOpenTrades=%d, len(GetOpenTrades)=%d",
			baselineCount, len(baselineList))
	}

	// Insert three open trades. Using a TEST_COA_ prefix and unique
	// suffix keeps the rows isolated even when the suite is re-run
	// against a non-truncated schema.
	suffix := time.Now().Format("150405.000")
	tickers := []string{
		"TEST_COA_AAA_" + suffix,
		"TEST_COA_BBB_" + suffix,
		"TEST_COA_CCC_" + suffix,
	}
	openIDs := make([]string, 0, len(tickers))
	for _, tk := range tickers {
		id, err := store.InsertTrade(ctx, store.Trade{
			Ticker:     tk,
			Direction:  "long",
			Strategy:   "momentum",
			EntryPrice: 100.0,
			Quantity:   1.0,
		})
		if err != nil {
			t.Fatalf("InsertTrade %s: %v", tk, err)
		}
		openIDs = append(openIDs, id)
		assertCountAgrees(t, ctx, "after open insert "+tk)
	}

	// Cleanup at end: hard-delete every row this test created.
	t.Cleanup(func() {
		db := store.GetDB()
		for _, id := range openIDs {
			_, _ = db.Exec(ctx, `DELETE FROM trades WHERE id=$1`, id)
		}
	})

	// Insert and immediately close two trades.
	closedIDs := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		tk := "TEST_COA_CLOSED_" + suffix + "_" + time.Now().Format("000")
		id, err := store.InsertTrade(ctx, store.Trade{
			Ticker:     tk,
			Direction:  "long",
			Strategy:   "momentum",
			EntryPrice: 100.0,
			Quantity:   1.0,
		})
		if err != nil {
			t.Fatalf("InsertTrade closed %d: %v", i, err)
		}
		closedIDs = append(closedIDs, id)
		assertCountAgrees(t, ctx, "after closed insert (still open)")

		if err := store.CloseTrade(ctx, id, 110.0, 10.0, 10.0); err != nil {
			t.Fatalf("CloseTrade %d: %v", i, err)
		}
		assertCountAgrees(t, ctx, "after CloseTrade")
	}
	t.Cleanup(func() {
		db := store.GetDB()
		for _, id := range closedIDs {
			_, _ = db.Exec(ctx, `DELETE FROM trades WHERE id=$1`, id)
		}
	})

	// Insert a row and mark it consolidated — exercises the same
	// status-filter contract that PR #311 introduced.
	consID, err := store.InsertTrade(ctx, store.Trade{
		Ticker:     "TEST_COA_CONS_" + suffix,
		Direction:  "long",
		Strategy:   "momentum",
		EntryPrice: 100.0,
		Quantity:   1.0,
	})
	if err != nil {
		t.Fatalf("InsertTrade consolidated: %v", err)
	}
	assertCountAgrees(t, ctx, "after consolidated insert (still open)")

	// Need a canonical row for the FK back-pointer.
	canonicalID, err := store.InsertTrade(ctx, store.Trade{
		Ticker:     "TEST_COA_CONS_CANON_" + suffix,
		Direction:  "long",
		Strategy:   "momentum",
		EntryPrice: 100.0,
		Quantity:   1.0,
	})
	if err != nil {
		t.Fatalf("InsertTrade canonical: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE trades
		SET status='consolidated', consolidated_into=$1
		WHERE id=$2`, canonicalID, consID); err != nil {
		t.Fatalf("mark consolidated: %v", err)
	}
	assertCountAgrees(t, ctx, "after mark consolidated")

	t.Cleanup(func() {
		db := store.GetDB()
		// Clear back-pointer first so the canonical row can be deleted.
		_, _ = db.Exec(ctx, `UPDATE trades SET consolidated_into=NULL WHERE id=$1`, consID)
		_, _ = db.Exec(ctx, `DELETE FROM trades WHERE id=$1`, consID)
		_, _ = db.Exec(ctx, `DELETE FROM trades WHERE id=$1`, canonicalID)
	})

	// Final delta sanity-check: 3 inserted opens + 1 canonical (still open).
	finalCount, err := store.CountOpenTrades(ctx)
	if err != nil {
		t.Fatalf("CountOpenTrades final: %v", err)
	}
	wantDelta := len(openIDs) + 1
	if got := finalCount - baselineCount; got != wantDelta {
		t.Errorf("open delta vs baseline: want %d, got %d (baseline=%d, final=%d)",
			wantDelta, got, baselineCount, finalCount)
	}
}

// assertCountAgrees fails the test if CountOpenTrades and
// len(GetOpenTrades) disagree at the named step.
func assertCountAgrees(t *testing.T, ctx context.Context, step string) {
	t.Helper()
	c, err := store.CountOpenTrades(ctx)
	if err != nil {
		t.Fatalf("%s: CountOpenTrades: %v", step, err)
	}
	list, err := store.GetOpenTrades(ctx)
	if err != nil {
		t.Fatalf("%s: GetOpenTrades: %v", step, err)
	}
	if c != len(list) {
		t.Fatalf("%s: CountOpenTrades=%d disagrees with len(GetOpenTrades)=%d",
			step, c, len(list))
	}
}

// ─── Portfolio ────────────────────────────────────────────────────────────────

func TestPortfolio_EmptyRangeNoError(t *testing.T) {
	ctx := context.Background()
	farPast := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	farFuture := time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)

	snaps, err := store.GetPortfolioSnapshots(ctx, farPast, farFuture)
	if err != nil {
		t.Fatalf("GetPortfolioSnapshots: %v", err)
	}
	_ = snaps // may be empty

	latest, err := store.GetLatestPortfolioSnapshot(ctx)
	if err != nil {
		t.Fatalf("GetLatestPortfolioSnapshot: %v", err)
	}
	_ = latest // may be nil

	summaries, err := store.GetDailySummaries(ctx, farPast, farFuture)
	if err != nil {
		t.Fatalf("GetDailySummaries: %v", err)
	}
	_ = summaries

	daily, err := store.GetDailySummaryByDate(ctx, farPast)
	if err != nil {
		t.Fatalf("GetDailySummaryByDate: %v", err)
	}
	_ = daily // may be nil
}

func TestPortfolio_GetClosedTradesByDateRange(t *testing.T) {
	ctx := context.Background()

	// Insert a trade and close it.
	id, err := store.InsertTrade(ctx, store.Trade{
		Ticker:     "TEST_PF_AMZN",
		Direction:  "long",
		Strategy:   "momentum",
		EntryPrice: 200.0,
		Quantity:   10.0,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if err := store.CloseTrade(ctx, id, 210.0, 100.0, 5.0); err != nil {
		t.Fatalf("CloseTrade: %v", err)
	}

	from := time.Now().Add(-1 * time.Minute)
	to := time.Now().Add(1 * time.Minute)
	trades, err := store.GetClosedTradesByDateRange(ctx, from, to)
	if err != nil {
		t.Fatalf("GetClosedTradesByDateRange: %v", err)
	}
	found := false
	for _, tr := range trades {
		if tr.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GetClosedTradesByDateRange: trade %q not found", id)
	}
}

// TestPortfolio_GetClosedTradesByDateRange_MidnightBoundary asserts the
// half-open [from, to) semantics: a trade whose closed_at is exactly the
// boundary instant counts in the day STARTING at that instant, never in
// the day ending at it. Matches AggregateDailyCounters' window so the two
// rollup paths agree on which day owns a midnight-close trade.
func TestPortfolio_GetClosedTradesByDateRange_MidnightBoundary(t *testing.T) {
	ctx := context.Background()

	// Pick a midnight far enough in the past that we won't collide with
	// other tests' "now" inserts but stable across runs.
	midnight := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	id, err := store.InsertTrade(ctx, store.Trade{
		Ticker:     "TEST_BOUNDARY",
		Direction:  "long",
		Strategy:   "momentum",
		EntryPrice: 100,
		Quantity:   1,
	})
	if err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE trades SET status='closed', exit_price=$1, pnl=$2, closed_at=$3
		WHERE id=$4`, 110.0, 10.0, midnight, id); err != nil {
		t.Fatalf("close trade at midnight: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(ctx, "DELETE FROM trades WHERE id=$1", id)
	})

	// PREVIOUS day window [midnight - 24h, midnight) — must NOT contain it.
	prevDay, err := store.GetClosedTradesByDateRange(ctx, midnight.Add(-24*time.Hour), midnight)
	if err != nil {
		t.Fatalf("prev-day query: %v", err)
	}
	for _, tr := range prevDay {
		if tr.ID == id {
			t.Fatalf("midnight trade leaked into previous day's window")
		}
	}

	// DAY-OF window [midnight, midnight + 24h) — must contain it.
	dayOf, err := store.GetClosedTradesByDateRange(ctx, midnight, midnight.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("day-of query: %v", err)
	}
	found := false
	for _, tr := range dayOf {
		if tr.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("midnight trade missing from its own day's window")
	}
}

// ─── Retention ────────────────────────────────────────────────────────────────

func TestRetention_CleanupNoError(t *testing.T) {
	ctx := context.Background()

	n, err := store.CleanupIntradayCandles(ctx)
	if err != nil {
		t.Fatalf("CleanupIntradayCandles: %v", err)
	}
	_ = n // may be 0

	n2, err := store.CleanupPromptCache(ctx)
	if err != nil {
		t.Fatalf("CleanupPromptCache: %v", err)
	}
	_ = n2
}

// ─── Embeddings ───────────────────────────────────────────────────────────────

func TestEmbedding_VectorOperations(t *testing.T) {
	ctx := context.Background()

	// Insert an analysis so we have something to embed.
	id, err := store.InsertAnalysis(ctx, "TEST_EMB_TSLA", "momentum",
		testAnalysisResp(analysis.ActionBuy), testModelMeta())
	if err != nil {
		t.Fatalf("InsertAnalysis for embedding test: %v", err)
	}

	// GetAnalysesWithoutEmbedding should include our freshly inserted row.
	pending, err := store.GetAnalysesWithoutEmbedding(ctx, 50)
	if err != nil {
		t.Fatalf("GetAnalysesWithoutEmbedding: %v", err)
	}
	found := false
	for _, a := range pending {
		if a.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GetAnalysesWithoutEmbedding: analysis %q not found", id)
	}

	// UpdateAnalysisEmbedding — supply a 768-dim vector (matches DB schema).
	vec := make([]float32, 768)
	for i := range vec {
		vec[i] = float32(i) * 0.001
	}
	if err := store.UpdateAnalysisEmbedding(ctx, id, vec); err != nil {
		t.Fatalf("UpdateAnalysisEmbedding: %v", err)
	}

	// After update, should not appear in "without embedding" query.
	afterUpdate, err := store.GetAnalysesWithoutEmbedding(ctx, 50)
	if err != nil {
		t.Fatalf("GetAnalysesWithoutEmbedding after update: %v", err)
	}
	for _, a := range afterUpdate {
		if a.ID == id {
			t.Errorf("analysis %q still in GetAnalysesWithoutEmbedding after embedding set", id)
		}
	}

	// EmbeddingCounts.
	counts, err := store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("EmbeddingCounts: %v", err)
	}
	if _, ok := counts["analyses"]; !ok {
		t.Error("EmbeddingCounts: missing 'analyses' key")
	}

	// FindSimilarAnalyses — use the same vector; should find our record.
	similar, err := store.FindSimilarAnalyses(ctx, vec, 0.0, 5)
	if err != nil {
		t.Fatalf("FindSimilarAnalyses: %v", err)
	}
	foundSimilar := false
	for _, s := range similar {
		if s.ID == id {
			foundSimilar = true
			break
		}
	}
	if !foundSimilar {
		t.Errorf("FindSimilarAnalyses: analysis %q not found in results", id)
	}

	// GetNewsWithoutEmbedding and GetEntitiesWithoutEmbedding — just verify no error.
	_, err = store.GetNewsWithoutEmbedding(ctx, 10)
	if err != nil {
		t.Fatalf("GetNewsWithoutEmbedding: %v", err)
	}

	_, err = store.GetEntitiesWithoutEmbedding(ctx, 10)
	if err != nil {
		t.Fatalf("GetEntitiesWithoutEmbedding: %v", err)
	}

	// ClearAllEmbeddings.
	if err := store.ClearAllEmbeddings(ctx); err != nil {
		t.Fatalf("ClearAllEmbeddings: %v", err)
	}

	// After clear, our row should be back in without-embedding list.
	afterClear, err := store.GetAnalysesWithoutEmbedding(ctx, 50)
	if err != nil {
		t.Fatalf("GetAnalysesWithoutEmbedding after clear: %v", err)
	}
	foundAfterClear := false
	for _, a := range afterClear {
		if a.ID == id {
			foundAfterClear = true
			break
		}
	}
	if !foundAfterClear {
		t.Errorf("GetAnalysesWithoutEmbedding after ClearAllEmbeddings: analysis %q not found", id)
	}
}
