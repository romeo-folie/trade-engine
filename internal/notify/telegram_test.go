package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// capturedMsgs is a thread-safe container for messages captured by captureServer.
// The HTTP handler goroutine appends via add(); the test goroutine reads via Len()/At().
type capturedMsgs struct {
	mu   sync.Mutex
	msgs []map[string]any
}

func (c *capturedMsgs) add(m map[string]any) {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
}

// Len returns the current number of captured messages.
func (c *capturedMsgs) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

// At returns the message at index i.  The caller must ensure i < Len().
func (c *capturedMsgs) At(i int) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.msgs[i]
}

// captureServer returns a test HTTP server that captures sent messages in a
// thread-safe container.  Using *capturedMsgs (instead of *[]map[string]any)
// prevents data races when the HTTP handler goroutine appends while the test
// goroutine reads.
func captureServer(t *testing.T) (*httptest.Server, *capturedMsgs) {
	t.Helper()
	msgs := &capturedMsgs{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			msgs.add(body)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"ok":true}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			// Return empty updates to prevent blocking.
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	return srv, msgs
}

func testBot(t *testing.T, srv *httptest.Server, notifyOn []string) (*TelegramBot, *events.Bus) {
	t.Helper()
	bus := events.New()
	cfg := config.TelegramConfig{
		Enabled:  true,
		BotToken: "test-token",
		ChatID:   "123",
		NotifyOn: notifyOn,
	}
	bot := NewTelegramBot(cfg, bus)
	bot.apiBase = srv.URL + "/bot"
	return bot, bus
}

func TestStartupMessage(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	bot, _ := testBot(t, srv, []string{"trade", "signal"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bot.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bot.Stop()

	// Wait for startup message.
	time.Sleep(100 * time.Millisecond)
	if msgs.Len() < 1 {
		t.Fatal("expected startup message")
	}
	text := msgs.At(0)["text"].(string)
	if !strings.Contains(text, "TradeEngine online") {
		t.Errorf("startup message = %q, want TradeEngine online", text)
	}
}

func TestTradeExecutedNotification(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	bot, bus := testBot(t, srv, []string{"trade"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bot.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bot.Stop()

	// Clear startup message.
	time.Sleep(100 * time.Millisecond)
	startCount := msgs.Len()

	bus.Publish(events.TradeExecuted, events.TradeExecutedPayload{
		TradeID:       "t1",
		Ticker:        "AAPL",
		Direction:     "buy",
		EntryPrice:    150.00,
		Quantity:      10,
		Strategy:      "momentum",
		EntrySetup:    "pullback_strength",
		SetupEvidence: "Momentum 35.0/50.0/80.0%; reclaimed $149.00 support",
	})

	time.Sleep(200 * time.Millisecond)
	if msgs.Len() <= startCount {
		t.Fatal("expected trade executed message")
	}
	text := msgs.At(startCount)["text"].(string)
	if !strings.Contains(text, "AAPL") || !strings.Contains(text, "Trade Executed") {
		t.Errorf("trade msg = %q", text)
	}
	if !strings.Contains(text, "Setup: Pullback in Strength") ||
		!strings.Contains(text, "Outcome: Momentum 35.0/50.0/80.0%") {
		t.Errorf("trade setup attribution missing: %q", text)
	}
}

func TestEntrySetupNotificationHidesStandardPath(t *testing.T) {
	if got := entrySetupNotification("standard", "unused"); got != "" {
		t.Fatalf("standard entry detail = %q, want empty", got)
	}
}

func TestSignalDetailIncludesPullbackAttribution(t *testing.T) {
	var detail strings.Builder
	writeSignalDetailSummary(&detail, &store.Signal{
		Ticker: "AMD", Action: "BUY", Confidence: 0.8,
		SignalPrice: 200, StopLoss: 195, TakeProfit: 220,
		Status: "pending", Strategy: "momentum",
		EntrySetup:         store.EntrySetupPullbackStrength,
		EntrySetupEvidence: "reclaimed $198.00 support",
	})
	got := detail.String()
	if !strings.Contains(got, "Setup: Pullback in Strength") ||
		!strings.Contains(got, "Outcome: reclaimed $198.00 support") {
		t.Fatalf("signal detail setup attribution missing: %q", got)
	}
}

func TestBatchedSignalIncludesPullbackAttribution(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	bus := events.New()
	cfg := config.TelegramConfig{
		Enabled: true, BotToken: "test-token", ChatID: "123",
		NotifyOn: []string{"signal"}, BatchEnabled: true,
		BatchWindowSec: 60, MaxItemsPerBatch: 10,
		MinConfidenceForAlert: 0.65,
	}
	bot := NewTelegramBot(cfg, bus)
	bot.apiBase = srv.URL + "/bot"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bot.Stop()
	time.Sleep(100 * time.Millisecond)
	startCount := msgs.Len()

	bus.Publish(events.SignalGenerated, events.SignalGeneratedPayload{
		Ticker: "NVDA", Action: "BUY", Confidence: 0.8,
		EntrySetup:    "pullback_strength",
		SetupEvidence: "reclaimed $170.00 support",
	})
	time.Sleep(100 * time.Millisecond)
	bot.FlushBatch()
	time.Sleep(200 * time.Millisecond)

	if msgs.Len() <= startCount {
		t.Fatal("expected batched signal message")
	}
	text := msgs.At(startCount)["text"].(string)
	if !strings.Contains(text, "Pullback in Strength") ||
		!strings.Contains(text, "reclaimed $170.00 support") {
		t.Fatalf("batched signal setup attribution missing: %q", text)
	}
}

func TestTradeClosedNotification(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	bot, bus := testBot(t, srv, []string{"trade"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bot.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bot.Stop()

	time.Sleep(100 * time.Millisecond)
	startCount := msgs.Len()

	bus.Publish(events.TradeClosed, events.TradeClosedPayload{
		TradeID:    "t1",
		Ticker:     "TSLA",
		Direction:  "buy",
		EntryPrice: 200.0,
		ExitPrice:  220.0,
		PnL:        200.0,
		PnLPct:     10.0,
		Reason:     "take_profit",
	})

	time.Sleep(200 * time.Millisecond)
	if msgs.Len() <= startCount {
		t.Fatal("expected trade closed message")
	}
	text := msgs.At(startCount)["text"].(string)
	if !strings.Contains(text, "Trade Closed") || !strings.Contains(text, "TSLA") {
		t.Errorf("trade closed msg = %q", text)
	}
	if !strings.Contains(text, "🟢") {
		t.Error("positive P&L should show green emoji")
	}
	if !strings.Contains(text, "take-profit hit") {
		t.Errorf("canonical reason was not rendered for operators: %q", text)
	}
}

func TestScannerCycleNotificationBatchesAdmissionsAndRediscoveries(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	bus := events.New()
	bot := NewTelegramBot(config.TelegramConfig{
		Enabled: true, BotToken: "test-token", ChatID: "123",
		NotifyOn: []string{"scanner"}, BatchEnabled: true,
	}, bus)
	bot.apiBase = srv.URL + "/bot"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bot.Stop()
	time.Sleep(100 * time.Millisecond)
	startCount := msgs.Len()

	bus.Publish(events.ScanStarted, events.ScanStartedPayload{ScanID: "scan-1"})
	bus.Publish(events.ScannerItemAdded, events.ScannerItemAddedPayload{
		ScanID: "scan-1", Ticker: "NVDA", Conviction: 0.82, SetupType: "momentum",
		Sources: []string{"mover", "news_sympathy"}, GraphContext: "Supplier sympathy",
	})
	bus.Publish(events.ScannerItemRediscovered, events.ScannerItemRediscoveredPayload{
		ScanID: "scan-1", Ticker: "AMD", ChangedSources: []events.ScannerSourceLeaseChangePayload{
			{Source: "political_disclosure"}, {Source: "weekly_leader"},
		},
	})
	bus.Publish(events.ScanCompleted, events.ScanCompletedPayload{ScanID: "scan-1"})

	time.Sleep(200 * time.Millisecond)
	if got := msgs.Len(); got != startCount+1 {
		t.Fatalf("scanner messages = %d, want %d", got-startCount, 1)
	}
	text := msgs.At(startCount)["text"].(string)
	for _, want := range []string{"Added (1)", "NVDA", "Mover, News sympathy", "Rediscovered (1)", "AMD", "Political disclosure, Weekly leader"} {
		if !strings.Contains(text, want) {
			t.Fatalf("scanner batch missing %q:\n%s", want, text)
		}
	}
}

func TestScannerCycleBatchesHonorItemAndTelegramMessageLimits(t *testing.T) {
	state := &scannerCycleNotifications{}
	for _, ticker := range []string{"AAA", "BBB", "CCC"} {
		state.added = append(state.added, events.ScannerItemAddedPayload{
			Ticker: ticker, Sources: []string{"mover"}, Reasoning: strings.Repeat("evidence ", 30),
		})
	}
	state.rediscovered = append(state.rediscovered, events.ScannerItemRediscoveredPayload{
		Ticker: "DDD", ChangedSources: []events.ScannerSourceLeaseChangePayload{{Source: "weekly_leader"}},
	})
	batches := formatScannerCycleBatches(state, 2)
	if len(batches) != 2 {
		t.Fatalf("batches = %d, want 2", len(batches))
	}
	combined := strings.Join(batches, "\n")
	for _, ticker := range []string{"AAA", "BBB", "CCC", "DDD"} {
		if count := strings.Count(combined, ticker); count != 1 {
			t.Errorf("ticker %s appeared %d times across batches", ticker, count)
		}
	}
	for _, batch := range batches {
		if len(batch) > telegramMessageMaxLength {
			t.Fatalf("scanner batch is %d bytes, exceeds Telegram limit", len(batch))
		}
	}

	longState := &scannerCycleNotifications{}
	for i := 0; i < 25; i++ {
		longState.added = append(longState.added, events.ScannerItemAddedPayload{
			Ticker: fmt.Sprintf("T%02d", i), Sources: []string{"mover"}, Reasoning: strings.Repeat("evidence ", 30),
		})
	}
	longBatches := formatScannerCycleBatches(longState, 25)
	if len(longBatches) < 2 {
		t.Fatal("verbose scanner cycle was not split for Telegram's message limit")
	}
	for _, batch := range longBatches {
		if len(batch) > telegramMessageMaxLength {
			t.Fatalf("verbose scanner batch is %d bytes, exceeds Telegram limit", len(batch))
		}
	}
}

func TestScannerNotificationsRespectCategory(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	bot, bus := testBot(t, srv, []string{"trade"})
	configured := true
	bot.cfg.ScannerNotificationsConfigured = &configured
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bot.Stop()
	time.Sleep(100 * time.Millisecond)
	startCount := msgs.Len()

	bus.Publish(events.ScanStarted, events.ScanStartedPayload{ScanID: "scan-1"})
	bus.Publish(events.ScannerItemAdded, events.ScannerItemAddedPayload{ScanID: "scan-1", Ticker: "NVDA", Sources: []string{"mover"}})
	bus.Publish(events.ScanCompleted, events.ScanCompletedPayload{ScanID: "scan-1"})
	time.Sleep(150 * time.Millisecond)
	if got := msgs.Len(); got != startCount {
		t.Fatalf("disabled scanner category sent %d message(s)", got-startCount)
	}
}

func TestLegacyTelegramNotifyOnKeepsScannerAdmissions(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	bot, bus := testBot(t, srv, []string{"signal", "trade", "error", "daily_summary"})
	bot.cfg.BatchEnabled = false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bot.Stop()
	time.Sleep(100 * time.Millisecond)
	startCount := msgs.Len()

	bus.Publish(events.ScannerItemAdded, events.ScannerItemAddedPayload{
		Ticker: "NVDA", Sources: []string{"mover"},
	})
	time.Sleep(200 * time.Millisecond)
	if got := msgs.Len(); got != startCount+1 {
		t.Fatalf("legacy notify_on sent %d scanner messages, want 1", got-startCount)
	}
}

func TestLegacyCustomizedNotifyOnKeepsScannerAdmissions(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	bot, bus := testBot(t, srv, []string{"trade"})
	bot.cfg.BatchEnabled = false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bot.Stop()
	time.Sleep(100 * time.Millisecond)
	startCount := msgs.Len()

	bus.Publish(events.ScannerItemAdded, events.ScannerItemAddedPayload{
		Ticker: "NVDA", Sources: []string{"mover"},
	})
	time.Sleep(200 * time.Millisecond)
	if got := msgs.Len(); got != startCount+1 {
		t.Fatalf("custom legacy notify_on sent %d scanner messages, want 1", got-startCount)
	}
}

func TestScannerCategoryExplicitOptOutOverridesLegacyCompatibility(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	configured := true
	bus := events.New()
	bot := NewTelegramBot(config.TelegramConfig{
		Enabled: true, BotToken: "test-token", ChatID: "123", BatchEnabled: false,
		NotifyOn:                       []string{"signal", "trade", "error", "daily_summary"},
		ScannerNotificationsConfigured: &configured,
	}, bus)
	bot.apiBase = srv.URL + "/bot"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bot.Stop()
	time.Sleep(100 * time.Millisecond)
	startCount := msgs.Len()

	bus.Publish(events.ScannerItemAdded, events.ScannerItemAddedPayload{
		Ticker: "NVDA", Sources: []string{"mover"},
	})
	time.Sleep(150 * time.Millisecond)
	if got := msgs.Len(); got != startCount {
		t.Fatalf("explicit scanner opt-out sent %d message(s)", got-startCount)
	}
}

func TestScannerNotificationsSendEachEventWhenBatchingDisabled(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	bus := events.New()
	bot := NewTelegramBot(config.TelegramConfig{
		Enabled: true, BotToken: "test-token", ChatID: "123",
		NotifyOn: []string{"scanner"}, BatchEnabled: false,
	}, bus)
	bot.apiBase = srv.URL + "/bot"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bot.Stop()
	time.Sleep(100 * time.Millisecond)
	startCount := msgs.Len()

	bus.Publish(events.ScannerItemAdded, events.ScannerItemAddedPayload{Ticker: "NVDA", Sources: []string{"mover"}})
	bus.Publish(events.ScannerItemRediscovered, events.ScannerItemRediscoveredPayload{
		Ticker: "AMD", ChangedSources: []events.ScannerSourceLeaseChangePayload{{Source: "graph_neighbor"}},
	})
	time.Sleep(200 * time.Millisecond)
	if got := msgs.Len(); got != startCount+2 {
		t.Fatalf("unbatched scanner messages = %d, want 2", got-startCount)
	}
}

func TestScannerNotificationsBoundIndividualMessages(t *testing.T) {
	admission := formatScannerAdmission(events.ScannerItemAddedPayload{
		Ticker:    strings.Repeat("T", 100),
		Sources:   []string{strings.Repeat("source_", 100)},
		SetupType: strings.Repeat("setup_", 1000),
		Reasoning: strings.Repeat("evidence ", 1000),
	})
	if len(admission) > telegramMessageMaxLength {
		t.Fatalf("admission is %d bytes, exceeds Telegram limit", len(admission))
	}
	rediscovery := formatScannerRediscovery(events.ScannerItemRediscoveredPayload{
		Ticker: strings.Repeat("T", 100),
		ChangedSources: []events.ScannerSourceLeaseChangePayload{
			{Source: strings.Repeat("source_", 100)},
		},
	})
	if len(rediscovery) > telegramMessageMaxLength {
		t.Fatalf("rediscovery is %d bytes, exceeds Telegram limit", len(rediscovery))
	}
}

func TestScannerNotificationFieldsEscapeMarkdownAndPreserveRunes(t *testing.T) {
	message := formatScannerAdmission(events.ScannerItemAddedPayload{
		Ticker: "A_B", Sources: []string{"mover"},
		SetupType: "[breakout]", Reasoning: "café *momentum*",
	})
	for _, want := range []string{"A\\_B", "\\[breakout\\]", "café \\*momentum\\*"} {
		if !strings.Contains(message, want) {
			t.Fatalf("escaped scanner message missing %q: %s", want, message)
		}
	}
	if got := truncateScannerNotification("ééé", 2); got != "é…" {
		t.Fatalf("rune-safe truncation = %q, want %q", got, "é…")
	}
}

func TestScannerSourceLabelsCoverEveryDiscoverySource(t *testing.T) {
	want := map[string]string{
		"mover":                "Mover",
		"active":               "Most active",
		"weekly_leader":        "Weekly leader",
		"potent_industry":      "Potent industry",
		"premarket_episodic":   "Premarket",
		"graph_neighbor":       "Graph neighbor",
		"news_sympathy":        "News sympathy",
		"political_disclosure": "Political disclosure",
	}
	for source, label := range want {
		if got := scannerSourceLabel(source); got != label {
			t.Errorf("scannerSourceLabel(%q) = %q, want %q", source, got, label)
		}
	}
}

func TestExitReasonLabel(t *testing.T) {
	cases := map[string]string{
		"stop_loss":       "stop-loss hit",
		"take_profit":     "take-profit hit",
		"sell_signal":     "sell signal",
		"trailing_atr":    "ATR trailing stop",
		"ema9_close":      "daily close below EMA9",
		"broker_recovery": "broker recovery",
		"":                "unknown",
	}
	for input, want := range cases {
		if got := exitReasonLabel(input); got != want {
			t.Errorf("exitReasonLabel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSignalQueuedWithInlineButtons(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	bot, bus := testBot(t, srv, []string{"signal"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bot.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bot.Stop()

	time.Sleep(100 * time.Millisecond)
	startCount := msgs.Len()

	bus.Publish(events.SignalQueued, events.SignalQueuedPayload{
		SignalID:      "sig1",
		Ticker:        "GOOG",
		Action:        "BUY",
		Confidence:    0.85,
		ExpiresIn:     25 * time.Minute,
		EntrySetup:    "pullback_strength",
		SetupEvidence: "reclaimed $200.00 support",
	})

	time.Sleep(200 * time.Millisecond)
	if msgs.Len() <= startCount {
		t.Fatal("expected signal queued message")
	}
	msg := msgs.At(startCount)
	text := msg["text"].(string)
	if !strings.Contains(text, "Awaiting Approval") || !strings.Contains(text, "GOOG") {
		t.Errorf("queued msg = %q", text)
	}
	if !strings.Contains(text, "Setup: Pullback in Strength") ||
		!strings.Contains(text, "Outcome: reclaimed $200.00 support") {
		t.Errorf("queued setup attribution missing: %q", text)
	}
	// Should have inline keyboard.
	if msg["reply_markup"] == nil {
		t.Error("expected inline keyboard for queued signal")
	}
}

func TestErrorNotification(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	bot, bus := testBot(t, srv, []string{"error"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bot.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bot.Stop()

	time.Sleep(100 * time.Millisecond)
	startCount := msgs.Len()

	bus.Publish(events.ErrorOccurred, events.ErrorPayload{
		Component: "data",
		Ticker:    "MSFT",
		Message:   "API rate limit exceeded",
	})

	time.Sleep(200 * time.Millisecond)
	if msgs.Len() <= startCount {
		t.Fatal("expected error message")
	}
	text := msgs.At(startCount)["text"].(string)
	if !strings.Contains(text, "Error") || !strings.Contains(text, "rate limit") {
		t.Errorf("error msg = %q", text)
	}
}

func TestNotifyOnFiltering(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	// Only enable "trade" — signal notifications should be suppressed.
	bot, bus := testBot(t, srv, []string{"trade"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bot.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bot.Stop()

	time.Sleep(100 * time.Millisecond)
	startCount := msgs.Len()

	// Signal event should be filtered out.
	bus.Publish(events.SignalGenerated, events.SignalGeneratedPayload{
		SignalID: "sig1",
		Ticker:   "NVDA",
		Action:   "BUY",
	})

	time.Sleep(200 * time.Millisecond)
	if msgs.Len() != startCount {
		t.Errorf("expected no signal notification when trade-only, got %d new messages", msgs.Len()-startCount)
	}
}

func TestDailySummary(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	bot, _ := testBot(t, srv, []string{"daily_summary"})
	// Don't need to start the full bot for this — just call SendDailySummary directly.
	bot.apiBase = srv.URL + "/bot"

	bot.SendDailySummary(DailySummaryData{
		TotalValue:       105000,
		DailyChange:      5000,
		DailyChangePct:   5.0,
		TradesOpened:     3,
		TradesClosed:     2,
		Wins:             2,
		Losses:           0,
		SignalsGenerated: 5,
		SignalsBlocked:   1,
	})

	if msgs.Len() < 1 {
		t.Fatal("expected daily summary message")
	}
	text := msgs.At(0)["text"].(string)
	if !strings.Contains(text, "Daily Summary") {
		t.Errorf("daily summary msg = %q", text)
	}
	if !strings.Contains(text, "105000") {
		t.Errorf("should contain portfolio value")
	}
}

func TestDrawdownWarning(t *testing.T) {
	srv, msgs := captureServer(t)
	defer srv.Close()

	bot, _ := testBot(t, srv, []string{"error"})
	bot.apiBase = srv.URL + "/bot"

	bot.SendDrawdownWarning(8.5, 10.0)

	if msgs.Len() < 1 {
		t.Fatal("expected drawdown warning")
	}
	text := msgs.At(0)["text"].(string)
	if !strings.Contains(text, "Drawdown Warning") || !strings.Contains(text, "8.50") {
		t.Errorf("drawdown msg = %q", text)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h 30m"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
