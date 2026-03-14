// Package notify provides notification delivery via Telegram.
// The bot runs as a background goroutine within the engine process,
// subscribing to event bus events and delivering formatted messages.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

const (
	telegramAPIBase = "https://api.telegram.org/bot"
	// pollTimeout is the long-poll timeout for getUpdates (seconds).
	pollTimeout = 30
	// maxRetries for transient send failures.
	maxRetries = 3
	// retryDelay between retries.
	retryDelay = 2 * time.Second
)

// CallbackHandler processes inline button callbacks.
// Returns a response text to send back to the user.
type CallbackHandler func(ctx context.Context, signalID, action string) (string, error)

// TelegramBot manages the Telegram notification lifecycle.
type TelegramBot struct {
	token   string
	chatID  string
	apiBase string
	cfg     config.TelegramConfig

	notifyOn map[string]bool
	bus      *events.Bus
	subs     []*events.Subscription

	batcher         *TickBatcher
	callbackHandler CallbackHandler

	client *http.Client
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewTelegramBot creates a bot from config. Does not start polling or subscribing.
func NewTelegramBot(cfg config.TelegramConfig, bus *events.Bus) *TelegramBot {
	notifyOn := make(map[string]bool, len(cfg.NotifyOn))
	for _, n := range cfg.NotifyOn {
		notifyOn[n] = true
	}
	bot := &TelegramBot{
		token:    cfg.BotToken,
		chatID:   cfg.ChatID,
		apiBase:  telegramAPIBase,
		cfg:      cfg,
		notifyOn: notifyOn,
		bus:      bus,
		client:   &http.Client{Timeout: 60 * time.Second},
	}
	if cfg.BatchEnabled {
		bot.batcher = NewTickBatcher(cfg, func(text string) error {
			return bot.sendMessage(text, nil)
		})
	}
	return bot
}

// FlushBatch flushes any pending batched notifications immediately.
// Called by the engine at the end of each tick.
func (b *TelegramBot) FlushBatch() {
	if b.batcher != nil {
		b.batcher.Flush()
	}
}

// SetCallbackHandler registers the handler for inline button callbacks.
func (b *TelegramBot) SetCallbackHandler(h CallbackHandler) {
	b.callbackHandler = h
}

// Start subscribes to events and begins the polling loop for callbacks.
func (b *TelegramBot) Start(ctx context.Context) error {
	ctx, b.cancel = context.WithCancel(ctx)
	b.ctx = ctx

	// Send startup message.
	if err := b.sendMessage("🟢 *TradeEngine online*\nMode: notifications active", nil); err != nil {
		logger.Warn("telegram: failed to send startup message", "error", err)
	}

	// Subscribe to relevant event types.
	b.subscribeEvents(ctx)

	// Start polling for inline button callbacks.
	b.wg.Add(1)
	go b.pollUpdates(ctx)

	return nil
}

// Stop gracefully shuts down the bot.
func (b *TelegramBot) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
	for _, sub := range b.subs {
		b.bus.Unsubscribe(sub)
	}
	logger.Info("telegram: bot stopped")
}

// ── Event subscription ────────────────────────────────────────────────────────

func (b *TelegramBot) subscribeEvents(ctx context.Context) {
	type binding struct {
		evType  events.Type
		handler func(events.Event)
	}

	bindings := []binding{
		{events.TradeExecuted, b.handleTradeExecuted},
		{events.TradeClosed, b.handleTradeClosed},
		{events.SignalGenerated, b.handleSignalGenerated},
		{events.SignalQueued, b.handleSignalQueued},
		{events.SignalBlocked, b.handleSignalBlocked},
		{events.ErrorOccurred, b.handleError},
	}

	for _, bind := range bindings {
		sub := b.bus.Subscribe(bind.evType)
		b.subs = append(b.subs, sub)
		handler := bind.handler
		b.wg.Add(1)
		go func(s *events.Subscription, h func(events.Event)) {
			defer b.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-s.C:
					if !ok {
						return
					}
					h(ev)
				}
			}
		}(sub, handler)
	}
}

// ── Event handlers ────────────────────────────────────────────────────────────

func (b *TelegramBot) handleTradeExecuted(ev events.Event) {
	if !b.notifyOn["trade"] {
		return
	}
	p, ok := ev.Payload.(events.TradeExecutedPayload)
	if !ok {
		return
	}
	if b.batcher != nil {
		b.batcher.Add(BatchItem{
			Kind:   "trade",
			Ticker: p.Ticker,
			Action: strings.ToUpper(p.Direction),
			Note:   fmt.Sprintf("$%.2f qty=%.4f", p.EntryPrice, p.Quantity),
		})
		return
	}
	msg := fmt.Sprintf(
		"📈 *Trade Executed*\n"+
			"Ticker: `%s` | %s\n"+
			"Entry: $%.2f | Qty: %.4f\n"+
			"Strategy: %s",
		p.Ticker, strings.ToUpper(p.Direction),
		p.EntryPrice, p.Quantity,
		p.Strategy,
	)
	if err := b.sendMessage(msg, nil); err != nil {
		logger.Warn("telegram: send trade alert failed", "error", err)
	}
}

func (b *TelegramBot) handleTradeClosed(ev events.Event) {
	if !b.notifyOn["trade"] {
		return
	}
	p, ok := ev.Payload.(events.TradeClosedPayload)
	if !ok {
		return
	}
	pnlEmoji := "🟢"
	if p.PnL < 0 {
		pnlEmoji = "🔴"
	}
	msg := fmt.Sprintf(
		"%s *Trade Closed*\n"+
			"Ticker: `%s` | %s\n"+
			"Entry: $%.2f → Exit: $%.2f\n"+
			"P&L: $%.2f (%.2f%%)\n"+
			"Reason: %s",
		pnlEmoji, p.Ticker, strings.ToUpper(p.Direction),
		p.EntryPrice, p.ExitPrice,
		p.PnL, p.PnLPct,
		p.Reason,
	)
	if err := b.sendMessage(msg, nil); err != nil {
		logger.Warn("telegram: send trade closed failed", "error", err)
	}
}

func (b *TelegramBot) handleSignalGenerated(ev events.Event) {
	if !b.notifyOn["signal"] {
		return
	}
	p, ok := ev.Payload.(events.SignalGeneratedPayload)
	if !ok {
		return
	}
	if b.batcher != nil {
		b.batcher.Add(BatchItem{
			Kind:       "signal",
			Ticker:     p.Ticker,
			Action:     p.Action,
			Confidence: p.Confidence,
		})
		return
	}
	msg := fmt.Sprintf(
		"🔔 *New Signal*\n"+
			"Ticker: `%s` | %s\n"+
			"Confidence: %.0f%%\n"+
			"Price: $%.2f | SL: $%.2f | TP: $%.2f\n"+
			"Mode: %s",
		p.Ticker, p.Action,
		p.Confidence*100,
		p.SignalPrice, p.StopLoss, p.TakeProfit,
		p.Mode,
	)
	if err := b.sendMessage(msg, nil); err != nil {
		logger.Warn("telegram: send signal alert failed", "error", err)
	}
}

func (b *TelegramBot) handleSignalQueued(ev events.Event) {
	if !b.notifyOn["signal"] {
		return
	}
	p, ok := ev.Payload.(events.SignalQueuedPayload)
	if !ok {
		return
	}
	msg := fmt.Sprintf(
		"⏳ *Signal Awaiting Approval*\n"+
			"Ticker: `%s` | %s\n"+
			"Confidence: %.0f%%\n"+
			"Expires in: %s",
		p.Ticker, p.Action,
		p.Confidence*100,
		formatDuration(p.ExpiresIn),
	)
	keyboard := inlineKeyboard(p.SignalID)
	if err := b.sendMessage(msg, keyboard); err != nil {
		logger.Warn("telegram: send queued signal failed", "error", err)
	}
}

func (b *TelegramBot) handleSignalBlocked(ev events.Event) {
	if !b.notifyOn["signal"] {
		return
	}
	p, ok := ev.Payload.(events.SignalBlockedPayload)
	if !ok {
		return
	}
	if b.batcher != nil {
		b.batcher.Add(BatchItem{
			Kind:   "blocked",
			Ticker: p.Ticker,
			Action: p.Action,
			Note:   p.BlockReason,
		})
		return
	}
	msg := fmt.Sprintf(
		"🚫 *Signal Blocked*\n"+
			"Ticker: `%s` | %s\n"+
			"Reason: %s\n"+
			"Failed check: %s",
		p.Ticker, p.Action,
		p.BlockReason,
		p.FailedCheck,
	)
	if err := b.sendMessage(msg, nil); err != nil {
		logger.Warn("telegram: send blocked signal failed", "error", err)
	}
}

func (b *TelegramBot) handleError(ev events.Event) {
	if !b.notifyOn["error"] {
		return
	}
	p, ok := ev.Payload.(events.ErrorPayload)
	if !ok {
		return
	}
	msg := fmt.Sprintf(
		"⚠️ *Error*\n"+
			"Component: %s\n"+
			"Ticker: %s\n"+
			"Message: %s",
		p.Component, p.Ticker, p.Message,
	)
	if err := b.sendMessage(msg, nil); err != nil {
		logger.Warn("telegram: send error alert failed", "error", err)
	}
}

// ── Daily summary ─────────────────────────────────────────────────────────────

// DailySummaryData holds the data needed for a daily summary notification.
type DailySummaryData struct {
	TotalValue    float64
	DailyChange   float64
	DailyChangePct float64
	TradesOpened  int
	TradesClosed  int
	Wins          int
	Losses        int
	SignalsGenerated int
	SignalsBlocked   int
}

// SendDailySummary sends the daily portfolio summary. Called from the engine scheduler.
func (b *TelegramBot) SendDailySummary(data DailySummaryData) {
	if !b.notifyOn["daily_summary"] {
		return
	}
	changeEmoji := "🟢"
	if data.DailyChange < 0 {
		changeEmoji = "🔴"
	}
	msg := fmt.Sprintf(
		"📊 *Daily Summary*\n\n"+
			"Portfolio: $%.2f\n"+
			"%s Daily: $%.2f (%.2f%%)\n\n"+
			"Trades opened: %d\n"+
			"Trades closed: %d\n"+
			"Wins: %d | Losses: %d\n\n"+
			"Signals generated: %d\n"+
			"Signals blocked: %d",
		data.TotalValue,
		changeEmoji, data.DailyChange, data.DailyChangePct,
		data.TradesOpened,
		data.TradesClosed,
		data.Wins, data.Losses,
		data.SignalsGenerated,
		data.SignalsBlocked,
	)
	if err := b.sendMessage(msg, nil); err != nil {
		logger.Warn("telegram: send daily summary failed", "error", err)
	}
}

// SendDrawdownWarning sends a drawdown breach alert.
func (b *TelegramBot) SendDrawdownWarning(currentDrawdown, maxAllowed float64) {
	if !b.notifyOn["error"] {
		return
	}
	msg := fmt.Sprintf(
		"🚨 *Drawdown Warning*\n"+
			"Current: %.2f%%\n"+
			"Max allowed: %.2f%%\n\n"+
			"Consider reducing exposure.",
		currentDrawdown, maxAllowed,
	)
	if err := b.sendMessage(msg, nil); err != nil {
		logger.Warn("telegram: send drawdown warning failed", "error", err)
	}
}

// SendImmediate delivers message directly to Telegram, bypassing any batch
// queue. It satisfies cost.DegradeNotifier for guardrail auto-degrade alerts.
// ctx is honoured for cancellation — the underlying HTTP call inherits it.
func (b *TelegramBot) SendImmediate(ctx context.Context, message string) error {
	url := fmt.Sprintf("%s%s/sendMessage", b.apiBase, b.token)
	body := map[string]any{
		"chat_id":    b.chatID,
		"text":       message,
		"parse_mode": "Markdown",
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("telegram SendImmediate: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram SendImmediate: %s", sanitizeError(err))
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram SendImmediate: API status %d", resp.StatusCode)
	}
	return nil
}

// ── Inline keyboard for approvals ─────────────────────────────────────────────

func inlineKeyboard(signalID string) []byte {
	kb := map[string]any{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "✅ Approve", "callback_data": "approve:" + signalID},
				{"text": "❌ Reject", "callback_data": "reject:" + signalID},
			},
			{
				{"text": "📋 Full Analysis", "callback_data": "detail:" + signalID},
			},
		},
	}
	data, _ := json.Marshal(kb)
	return data
}

// ── Polling for inline button callbacks ───────────────────────────────────────

func (b *TelegramBot) pollUpdates(ctx context.Context) {
	defer b.wg.Done()
	offset := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		updates, err := b.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Warn("telegram: getUpdates failed", "error", sanitizeError(err))
			if !b.retrySleep(5 * time.Second) {
				return
			}
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.CallbackQuery != nil {
				b.handleCallback(ctx, u.CallbackQuery)
			}
		}
	}
}

type telegramUpdate struct {
	UpdateID      int                    `json:"update_id"`
	CallbackQuery *telegramCallbackQuery `json:"callback_query"`
}

type telegramCallbackQuery struct {
	ID   string `json:"id"`
	Data string `json:"data"`
}

func (b *TelegramBot) getUpdates(ctx context.Context, offset int) ([]telegramUpdate, error) {
	url := fmt.Sprintf("%s%s/getUpdates?offset=%d&timeout=%d&allowed_updates=[\"callback_query\"]",
		b.apiBase, b.token, offset, pollTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		OK     bool             `json:"ok"`
		Result []telegramUpdate `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Result, nil
}

func (b *TelegramBot) handleCallback(ctx context.Context, cb *telegramCallbackQuery) {
	parts := strings.SplitN(cb.Data, ":", 2)
	if len(parts) != 2 {
		b.answerCallback(cb.ID, "Invalid action")
		return
	}
	action, signalID := parts[0], parts[1]
	if b.callbackHandler == nil {
		b.answerCallback(cb.ID, "No handler registered")
		return
	}
	responseText, err := b.callbackHandler(ctx, signalID, action)
	if err != nil {
		logger.Warn("telegram: callback handler error", "error", err, "action", action, "signal", signalID)
		b.answerCallback(cb.ID, "Error: "+err.Error())
		return
	}
	b.answerCallback(cb.ID, responseText)
	if action == "detail" {
		// Detail responses are longer — send as a message.
		if err := b.sendMessage(responseText, nil); err != nil {
			logger.Warn("telegram: send detail failed", "error", err)
		}
	}
}

func (b *TelegramBot) answerCallback(callbackID, text string) {
	url := fmt.Sprintf("%s%s/answerCallbackQuery", b.apiBase, b.token)
	body := map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
	}
	data, _ := json.Marshal(body)
	resp, err := b.client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		logger.Warn("telegram: answerCallbackQuery failed", "error", sanitizeError(err))
		return
	}
	resp.Body.Close()
}

// ── Telegram API helpers ──────────────────────────────────────────────────────

func (b *TelegramBot) sendMessage(text string, replyMarkup []byte) error {
	url := fmt.Sprintf("%s%s/sendMessage", b.apiBase, b.token)
	body := map[string]any{
		"chat_id":    b.chatID,
		"text":       text,
		"parse_mode": "Markdown",
	}
	if replyMarkup != nil {
		body["reply_markup"] = json.RawMessage(replyMarkup)
	}
	data, _ := json.Marshal(body)

	var lastErr error
	for attempt := range maxRetries {
		resp, err := b.client.Post(url, "application/json", bytes.NewReader(data))
		if err != nil {
			lastErr = fmt.Errorf("send failed: %v", sanitizeError(err))
			if !b.retrySleep(retryDelay * time.Duration(attempt+1)) {
				return lastErr
			}
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("rate limited (status %d)", resp.StatusCode)
			if !b.retrySleep(retryDelay * time.Duration(attempt+1)) {
				return lastErr
			}
			continue
		}
		return fmt.Errorf("telegram API error %d", resp.StatusCode)
	}
	return fmt.Errorf("telegram: max retries exceeded: %w", lastErr)
}

// retrySleep waits for d or until the bot's context is cancelled.
// Returns true if the sleep completed, false if cancelled.
func (b *TelegramBot) retrySleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-b.ctx.Done():
		return false
	}
}

// sanitizeError strips URL query parameters (which may contain tokens) from error messages.
func sanitizeError(err error) string {
	s := err.Error()
	// Remove anything after '?' in URLs to avoid leaking tokens.
	if idx := strings.Index(s, "?"); idx != -1 {
		if end := strings.IndexAny(s[idx:], " \"'"); end != -1 {
			s = s[:idx] + "<redacted>" + s[idx+end:]
		} else {
			s = s[:idx] + "<redacted>"
		}
	}
	return s
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}
