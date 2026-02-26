package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
)

// captureServer returns a test HTTP server that captures sent messages.
func captureServer(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var messages []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			mu.Lock()
			messages = append(messages, body)
			mu.Unlock()
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
	return srv, &messages
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
	if len(*msgs) < 1 {
		t.Fatal("expected startup message")
	}
	text := (*msgs)[0]["text"].(string)
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
	startCount := len(*msgs)

	bus.Publish(events.TradeExecuted, &events.TradeExecutedPayload{
		TradeID:    "t1",
		Ticker:     "AAPL",
		Direction:  "buy",
		EntryPrice: 150.00,
		Quantity:   10,
		Strategy:   "momentum",
	})

	time.Sleep(200 * time.Millisecond)
	if len(*msgs) <= startCount {
		t.Fatal("expected trade executed message")
	}
	text := (*msgs)[startCount]["text"].(string)
	if !strings.Contains(text, "AAPL") || !strings.Contains(text, "Trade Executed") {
		t.Errorf("trade msg = %q", text)
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
	startCount := len(*msgs)

	bus.Publish(events.TradeClosed, &events.TradeClosedPayload{
		TradeID:    "t1",
		Ticker:     "TSLA",
		Direction:  "buy",
		EntryPrice: 200.0,
		ExitPrice:  220.0,
		PnL:        200.0,
		PnLPct:     10.0,
		Reason:     "take-profit hit",
	})

	time.Sleep(200 * time.Millisecond)
	if len(*msgs) <= startCount {
		t.Fatal("expected trade closed message")
	}
	text := (*msgs)[startCount]["text"].(string)
	if !strings.Contains(text, "Trade Closed") || !strings.Contains(text, "TSLA") {
		t.Errorf("trade closed msg = %q", text)
	}
	if !strings.Contains(text, "🟢") {
		t.Error("positive P&L should show green emoji")
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
	startCount := len(*msgs)

	bus.Publish(events.SignalQueued, &events.SignalQueuedPayload{
		SignalID:   "sig1",
		Ticker:     "GOOG",
		Action:     "BUY",
		Confidence: 0.85,
		ExpiresIn:  25 * time.Minute,
	})

	time.Sleep(200 * time.Millisecond)
	if len(*msgs) <= startCount {
		t.Fatal("expected signal queued message")
	}
	msg := (*msgs)[startCount]
	text := msg["text"].(string)
	if !strings.Contains(text, "Awaiting Approval") || !strings.Contains(text, "GOOG") {
		t.Errorf("queued msg = %q", text)
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
	startCount := len(*msgs)

	bus.Publish(events.ErrorOccurred, &events.ErrorPayload{
		Component: "data",
		Ticker:    "MSFT",
		Message:   "API rate limit exceeded",
	})

	time.Sleep(200 * time.Millisecond)
	if len(*msgs) <= startCount {
		t.Fatal("expected error message")
	}
	text := (*msgs)[startCount]["text"].(string)
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
	startCount := len(*msgs)

	// Signal event should be filtered out.
	bus.Publish(events.SignalGenerated, &events.SignalGeneratedPayload{
		SignalID: "sig1",
		Ticker:   "NVDA",
		Action:   "BUY",
	})

	time.Sleep(200 * time.Millisecond)
	if len(*msgs) != startCount {
		t.Errorf("expected no signal notification when trade-only, got %d new messages", len(*msgs)-startCount)
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
		TradesOpened:      3,
		TradesClosed:      2,
		Wins:              2,
		Losses:            0,
		SignalsGenerated:  5,
		SignalsBlocked:    1,
	})

	if len(*msgs) < 1 {
		t.Fatal("expected daily summary message")
	}
	text := (*msgs)[0]["text"].(string)
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

	if len(*msgs) < 1 {
		t.Fatal("expected drawdown warning")
	}
	text := (*msgs)[0]["text"].(string)
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
