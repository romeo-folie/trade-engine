package notify

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
)

// scannerPromotionOutcomeBatcher preserves Telegram's configured batching for
// the source-neutral promotion contract. Unlike TickBatcher, it never drops a
// selected attempt: full batches flush immediately and remaining outcomes wait
// only for the configured window.
type scannerPromotionOutcomeBatcher struct {
	cfg    config.TelegramConfig
	sendFn func(context.Context, string)
	ctxFn  func() context.Context

	mu          sync.Mutex
	outcomes    []events.ScannerPromotionOutcomePayload
	timer       *time.Timer
	timerDone   chan struct{}
	timerActive map[chan struct{}]struct{}
}

func newScannerPromotionOutcomeBatcher(
	cfg config.TelegramConfig, sendFn func(context.Context, string), ctxFn func() context.Context,
) *scannerPromotionOutcomeBatcher {
	return &scannerPromotionOutcomeBatcher{cfg: cfg, sendFn: sendFn, ctxFn: ctxFn}
}

func (b *scannerPromotionOutcomeBatcher) Add(outcome events.ScannerPromotionOutcomePayload) {
	var full []events.ScannerPromotionOutcomePayload
	b.mu.Lock()
	maxItems := b.cfg.MaxItemsPerBatch
	if maxItems <= 0 {
		maxItems = 20
	}
	b.outcomes = append(b.outcomes, outcome)
	if len(b.outcomes) >= maxItems {
		full = b.takeLocked()
	} else if b.timer == nil {
		window := time.Duration(b.cfg.BatchWindowSec) * time.Second
		if window <= 0 {
			window = time.Minute
		}
		done := make(chan struct{})
		b.timerDone = done
		b.timer = time.AfterFunc(window, func() { b.flushTimer(done) })
	}
	b.mu.Unlock()
	if len(full) > 0 {
		b.send(b.notificationContext(), full)
	}
}

func (b *scannerPromotionOutcomeBatcher) flushTimer(done chan struct{}) {
	b.mu.Lock()
	if b.timerDone == done {
		b.timer, b.timerDone = nil, nil
	}
	if b.timerActive == nil {
		b.timerActive = make(map[chan struct{}]struct{})
	}
	b.timerActive[done] = struct{}{}
	outcomes := b.outcomes
	b.outcomes = nil
	b.mu.Unlock()
	b.send(b.notificationContext(), outcomes)
	b.mu.Lock()
	delete(b.timerActive, done)
	b.mu.Unlock()
	close(done)
}

func (b *scannerPromotionOutcomeBatcher) Flush() {
	b.FlushWithContext(b.notificationContext())
}

func (b *scannerPromotionOutcomeBatcher) notificationContext() context.Context {
	if b.ctxFn != nil {
		if ctx := b.ctxFn(); ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

func (b *scannerPromotionOutcomeBatcher) FlushWithContext(ctx context.Context) {
	b.mu.Lock()
	outcomes := b.takeLocked()
	b.mu.Unlock()
	b.send(ctx, outcomes)
}

func (b *scannerPromotionOutcomeBatcher) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.outcomes = nil
	if b.timer != nil {
		if b.timer.Stop() {
			b.timer = nil
			b.timerDone = nil
		}
	}
}

// StopAndFlush prevents a pending timer from racing a graceful shutdown. If a
// timer callback already owns its batch, wait for that delivery before sending
// the remainder with the caller's bounded shutdown context.
func (b *scannerPromotionOutcomeBatcher) StopAndFlush(ctx context.Context) {
	b.mu.Lock()
	var outcomes []events.ScannerPromotionOutcomePayload
	waits := make([]chan struct{}, 0, len(b.timerActive)+1)
	for done := range b.timerActive {
		waits = append(waits, done)
	}
	if b.timer != nil && b.timer.Stop() {
		b.timer, b.timerDone = nil, nil
		outcomes = b.outcomes
		b.outcomes = nil
	} else if b.timerDone != nil {
		waits = append(waits, b.timerDone)
	}
	b.mu.Unlock()
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
	b.send(ctx, outcomes)
	b.FlushWithContext(ctx)
}

func (b *scannerPromotionOutcomeBatcher) takeLocked() []events.ScannerPromotionOutcomePayload {
	outcomes := b.outcomes
	b.outcomes = nil
	if b.timer != nil {
		if b.timer.Stop() {
			b.timer = nil
			b.timerDone = nil
		}
	}
	return outcomes
}

func (b *scannerPromotionOutcomeBatcher) send(ctx context.Context, outcomes []events.ScannerPromotionOutcomePayload) {
	for _, message := range formatScannerPromotionOutcomeBatches(outcomes, b.cfg.MaxItemsPerBatch) {
		b.sendFn(ctx, message)
	}
}

func formatScannerPromotionOutcomeBatches(outcomes []events.ScannerPromotionOutcomePayload, maxItems int) []string {
	if maxItems <= 0 {
		maxItems = 20
	}
	var batches []string
	var current []string
	flush := func() {
		if len(current) == 0 {
			return
		}
		batches = append(batches, strings.Join(current, "\n"))
		current = nil
	}
	for _, outcome := range outcomes {
		parts := splitScannerPromotionOutcome(formatScannerPromotionOutcome(outcome))
		if len(parts) > 1 {
			flush()
			batches = append(batches, parts...)
			continue
		}
		message := parts[0]
		prospective := append(append([]string(nil), current...), message)
		if len(current) >= maxItems || (len(current) > 0 && len(strings.Join(prospective, "\n")) > telegramMessageMaxLength) {
			flush()
		}
		current = append(current, message)
	}
	flush()
	return batches
}

func splitScannerPromotionOutcome(message string) []string {
	if len(message) <= telegramMessageMaxLength {
		return []string{message}
	}
	parts := make([]string, 0, (len(message)+telegramMessageMaxLength-1)/telegramMessageMaxLength)
	for len(message) > telegramMessageMaxLength {
		limit := telegramMessageMaxLength
		if newline := strings.LastIndex(message[:limit], "\n"); newline > 0 {
			limit = newline + 1
		} else {
			for limit > 0 && !utf8.RuneStart(message[limit]) {
				limit--
			}
		}
		parts = append(parts, message[:limit])
		message = message[limit:]
	}
	return append(parts, message)
}
