package store_test

import (
	"context"
	"encoding/json"
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
		Provider:    "anthropic",
		Model:       "claude-test",
		PromptHash:  "abc123",
		RawPrompt:   "test prompt",
		RawResponse: `{"action":"BUY"}`,
		InputTokens: 100,
		OutputTokens: 50,
		LatencyMs:   200,
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

func TestBacktest_FullLifecycle(t *testing.T) {
	ctx := context.Background()

	label := "INTG-BACKTEST-" + time.Now().Format("150405")
	run := store.BacktestRun{
		Label:          label,
		Ticker:         "TEST_BT_SPY",
		Strategy:       "momentum",
		ModelProvider:  "anthropic",
		ModelName:      "claude-test",
		FromDate:       time.Now().Add(-30 * 24 * time.Hour),
		ToDate:         time.Now(),
		ConfigSnapshot: json.RawMessage(`{}`),
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
	if err := store.UpsertNewsItems(ctx, []data.NewsItem{
		{
			ExternalID:  newsExtID,
			Headline:    "Test news for entity mention",
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
			URL:            "https://example.com/intg",
			PublishedAt:    published,
			Tickers:        []string{"TEST_NEWS_AAPL"},
			SentimentScore: 0.5,
		},
	}
	if err := store.UpsertNewsItems(ctx, items); err != nil {
		t.Fatalf("UpsertNewsItems: %v", err)
	}

	// Idempotency — re-upsert should not error.
	if err := store.UpsertNewsItems(ctx, items); err != nil {
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
		TradeID:   tradeID,
		Ticker:    "TEST_ORD_NVDA",
		Side:      "buy",
		OrderType: "market",
		Quantity:  5.0,
		Status:    "pending",
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

// ─── Predictions ─────────────────────────────────────────────────────────────

func TestPrediction_SnapshotLifecycle(t *testing.T) {
	ctx := context.Background()
	const platform = "kalshi"
	const contractID = "INTG-PRED-TEST-" + "001"

	snap := data.ContractSnapshot{
		Platform:      platform,
		ContractID:    contractID,
		ContractTitle: "Integration test contract",
		Price:         0.65,
		YesPrice:      0.65,
		NoPrice:       0.35,
		Volume24h:     10000.0,
		OpenInterest:  50000.0,
		Bid:           0.64,
		Ask:           0.66,
		ResolutionDeadline: time.Now().Add(30 * 24 * time.Hour),
		Categories:    []string{"politics"},
		CreatedAt:     time.Now(),
	}

	if err := store.InsertPredictionSnapshot(ctx, snap); err != nil {
		t.Fatalf("InsertPredictionSnapshot: %v", err)
	}

	recent, err := store.GetRecentSnapshots(ctx, platform, contractID, 5)
	if err != nil {
		t.Fatalf("GetRecentSnapshots: %v", err)
	}
	if len(recent) == 0 {
		t.Fatal("GetRecentSnapshots: expected at least 1 snapshot")
	}

	since := time.Now().Add(-1 * time.Minute)
	sincSnaps, err := store.GetSnapshotsSince(ctx, platform, contractID, since)
	if err != nil {
		t.Fatalf("GetSnapshotsSince: %v", err)
	}
	if len(sincSnaps) == 0 {
		t.Error("GetSnapshotsSince: expected at least 1 snapshot")
	}

	latest, err := store.GetLatestSnapshot(ctx, platform, contractID)
	if err != nil {
		t.Fatalf("GetLatestSnapshot: %v", err)
	}
	if latest == nil {
		t.Fatal("GetLatestSnapshot: nil")
	}
	if latest.ContractID != contractID {
		t.Errorf("ContractID: got %q, want %q", latest.ContractID, contractID)
	}

	resVal := true
	if err := store.MarkContractResolved(ctx, platform, contractID, resVal); err != nil {
		t.Fatalf("MarkContractResolved: %v", err)
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

	// UpdateAnalysisEmbedding — supply a 1536-dim vector.
	vec := make([]float32, 1536)
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
