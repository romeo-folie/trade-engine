package notify

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
)

// scannerPromotionOutcomeBatcher preserves Telegram's configured batching for
// the source-neutral promotion contract. Unlike TickBatcher, it never drops a
// selected attempt: outcomes are kept together for capacity-state decisions,
// while Telegram messages are split at the configured item limit.
type scannerPromotionOutcomeBatcher struct {
	cfg    config.TelegramConfig
	sendFn func(context.Context, string)
	ctxFn  func() context.Context

	sendMu             sync.Mutex
	orderMu            sync.Mutex
	orderCond          *sync.Cond
	nextBatchSeq       uint64
	nextSendSeq        uint64
	mu                 sync.Mutex
	outcomes           []events.ScannerPromotionOutcomePayload
	timer              *time.Timer
	timerDone          chan struct{}
	timerActive        map[chan struct{}]struct{}
	deferrals          map[deferralThrottleKey]deferralThrottleState
	nowFn              func() time.Time
	lastDeferralAlert  time.Time
	pendingRecovery    map[string]struct{}
	recoveryTimer      *time.Timer
	recoveryGeneration uint64
}

type deferredKey struct {
	runID, bucket, reason string
}

type deferralThrottleKey struct {
	bucket, reason string
}

type deferralThrottleState struct {
	suppressed int
}

const scannerDeferralReminderInterval = time.Hour

func newScannerPromotionOutcomeBatcher(
	cfg config.TelegramConfig, sendFn func(context.Context, string), ctxFn func() context.Context,
) *scannerPromotionOutcomeBatcher {
	b := &scannerPromotionOutcomeBatcher{
		cfg: cfg, sendFn: sendFn, ctxFn: ctxFn,
		deferrals: make(map[deferralThrottleKey]deferralThrottleState), nowFn: time.Now,
		pendingRecovery: make(map[string]struct{}),
	}
	b.nextSendSeq = 1
	b.orderCond = sync.NewCond(&b.orderMu)
	return b
}

func (b *scannerPromotionOutcomeBatcher) Add(outcome events.ScannerPromotionOutcomePayload) {
	var full []events.ScannerPromotionOutcomePayload
	var seq uint64
	b.mu.Lock()
	if scannerPromotionDeferred(outcome) && b.recoveryTimer != nil {
		b.recoveryGeneration++
		b.recoveryTimer.Stop()
		b.recoveryTimer = nil
	}
	maxItems := b.cfg.MaxItemsPerBatch
	if maxItems <= 0 {
		maxItems = 20
	}
	b.outcomes = append(b.outcomes, outcome)
	if len(b.outcomes) >= maxItems || (!b.cfg.BatchEnabled && scannerPromotionDeferred(outcome)) {
		full = b.takeLocked()
		seq = b.reserveBatchLocked()
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
	if seq > 0 {
		b.sendBatch(b.notificationContext(), full, seq)
	}
}

// ObserveAdmission tracks capacity recovery when regular notifications bypass
// batching, without delaying their immediate delivery.
func (b *scannerPromotionOutcomeBatcher) ObserveAdmission(outcome events.ScannerPromotionOutcomePayload) {
	if outcome.Outcome != "admitted" || strings.TrimSpace(outcome.QueueBucket) == "" {
		return
	}
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	b.mu.Lock()
	deferred := false
	for key := range b.deferrals {
		if key.bucket == outcome.QueueBucket {
			deferred = true
			break
		}
	}
	if deferred {
		b.pendingRecovery[outcome.QueueBucket] = struct{}{}
		if b.recoveryTimer == nil {
			window := time.Duration(b.cfg.BatchWindowSec) * time.Second
			if window <= 0 {
				window = time.Minute
			}
			generation := b.recoveryGeneration
			b.recoveryTimer = time.AfterFunc(window, func() { b.flushPendingRecoveries(generation) })
		}
	}
	b.mu.Unlock()
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
	seq := b.reserveBatchLocked()
	b.mu.Unlock()
	b.sendBatch(b.notificationContext(), outcomes, seq)
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
	seq := uint64(0)
	if len(outcomes) > 0 {
		seq = b.reserveBatchLocked()
	}
	b.mu.Unlock()
	if seq > 0 {
		b.sendBatch(ctx, outcomes, seq)
	}
}

func (b *scannerPromotionOutcomeBatcher) reserveBatchLocked() uint64 {
	b.nextBatchSeq++
	return b.nextBatchSeq
}

func (b *scannerPromotionOutcomeBatcher) sendBatch(ctx context.Context, outcomes []events.ScannerPromotionOutcomePayload, seq uint64) {
	b.orderMu.Lock()
	for seq != b.nextSendSeq {
		b.orderCond.Wait()
	}
	b.orderMu.Unlock()
	b.send(ctx, outcomes)
	b.orderMu.Lock()
	b.nextSendSeq++
	b.orderCond.Broadcast()
	b.orderMu.Unlock()
}

func (b *scannerPromotionOutcomeBatcher) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recoveryGeneration++
	b.outcomes = nil
	if b.timer != nil {
		if b.timer.Stop() {
			b.timer = nil
			b.timerDone = nil
		}
	}
	if b.recoveryTimer != nil {
		b.recoveryTimer.Stop()
		b.recoveryTimer = nil
	}
	b.deferrals = make(map[deferralThrottleKey]deferralThrottleState)
	b.pendingRecovery = make(map[string]struct{})
	b.lastDeferralAlert = time.Time{}
}

// StopAndFlush prevents a pending timer from racing a graceful shutdown. If a
// timer callback already owns its batch, wait for that delivery before sending
// the remainder with the caller's bounded shutdown context.
func (b *scannerPromotionOutcomeBatcher) StopAndFlush(ctx context.Context) {
	b.mu.Lock()
	var outcomes []events.ScannerPromotionOutcomePayload
	var seq uint64
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
			// The shutdown deadline can expire while another timer owns an
			// earlier sequence. Return this unsent batch to the queue without
			// reserving a sequence that would permanently block later sends.
			if len(outcomes) > 0 {
				b.mu.Lock()
				b.outcomes = append(outcomes, b.outcomes...)
				if b.timer == nil {
					window := time.Duration(b.cfg.BatchWindowSec) * time.Second
					if window <= 0 {
						window = time.Minute
					}
					done := make(chan struct{})
					b.timerDone = done
					b.timer = time.AfterFunc(window, func() { b.flushTimer(done) })
				}
				b.mu.Unlock()
			}
			return
		}
	}
	if len(outcomes) > 0 {
		b.mu.Lock()
		outcomes = append(outcomes, b.takeLocked()...)
		seq = b.reserveBatchLocked()
		b.mu.Unlock()
		b.sendBatch(ctx, outcomes, seq)
	}
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
	// Keep throttle-state transitions in the same order as delivery. In
	// particular, an older admitted batch must not finish its blocked Telegram
	// send after a newer deferral batch and clear that newer capacity state.
	b.sendMu.Lock()
	defer b.sendMu.Unlock()

	nowFn := b.nowFn
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	regular := make([]events.ScannerPromotionOutcomePayload, 0, len(outcomes))
	counts := make(map[deferralThrottleKey]int)
	admittedBuckets := make(map[string]bool)
	for _, outcome := range outcomes {
		if scannerPromotionDeferred(outcome) {
			counts[deferralThrottleKey{bucket: outcome.QueueBucket, reason: outcome.ReasonCode}]++
			continue
		}
		regular = append(regular, outcome)
		if outcome.Outcome == "admitted" && strings.TrimSpace(outcome.QueueBucket) != "" {
			admittedBuckets[outcome.QueueBucket] = true
		}
	}
	for _, message := range formatScannerPromotionOutcomeBatches(regular, b.cfg.MaxItemsPerBatch) {
		b.sendFn(ctx, message)
	}

	b.mu.Lock()
	deferred := make(map[deferredKey]int)
	deferredBuckets := make(map[string]bool)
	for key, count := range counts {
		deferredBuckets[key.bucket] = true
		state := b.deferrals[key]
		state.suppressed += count
		b.deferrals[key] = state
	}
	if len(counts) > 0 && (b.lastDeferralAlert.IsZero() || now.Sub(b.lastDeferralAlert) >= scannerDeferralReminderInterval) {
		for key, state := range b.deferrals {
			if state.suppressed == 0 {
				continue
			}
			deferred[deferredKey{runID: "hourly", bucket: key.bucket, reason: key.reason}] = state.suppressed
			state.suppressed = 0
			b.deferrals[key] = state
		}
		b.lastDeferralAlert = now
	}
	for bucket := range deferredBuckets {
		delete(b.pendingRecovery, bucket)
	}
	for key := range b.deferrals {
		if !admittedBuckets[key.bucket] || deferredBuckets[key.bucket] {
			continue
		}
		b.pendingRecovery[key.bucket] = struct{}{}
	}
	if len(b.pendingRecovery) > 0 && b.recoveryTimer == nil {
		window := time.Duration(b.cfg.BatchWindowSec) * time.Second
		if window <= 0 {
			window = time.Minute
		}
		generation := b.recoveryGeneration
		b.recoveryTimer = time.AfterFunc(window, func() { b.flushPendingRecoveries(generation) })
	}
	b.mu.Unlock()

	if len(deferred) > 0 {
		keys := make([]deferredKey, 0, len(deferred))
		for key := range deferred {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].bucket != keys[j].bucket {
				return keys[i].bucket < keys[j].bucket
			}
			return keys[i].reason < keys[j].reason
		})
		batches := make([]string, 0, 1)
		appendDeferredSummaries(&batches, keys, deferred)
		for _, message := range batches {
			b.sendFn(ctx, message)
		}
	}
}

func (b *scannerPromotionOutcomeBatcher) flushPendingRecoveries(generation uint64) {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	b.mu.Lock()
	if generation != b.recoveryGeneration {
		b.mu.Unlock()
		return
	}
	b.recoveryTimer = nil
	var recovered []string
	for bucket := range b.pendingRecovery {
		active := false
		for key := range b.deferrals {
			if key.bucket == bucket {
				active = true
				delete(b.deferrals, key)
			}
		}
		if active {
			recovered = append(recovered, bucket)
		}
		delete(b.pendingRecovery, bucket)
	}
	if len(b.deferrals) == 0 {
		b.lastDeferralAlert = time.Time{}
	}
	b.mu.Unlock()
	sort.Strings(recovered)
	for _, bucket := range recovered {
		label := strings.ReplaceAll(bucket, "_", " ")
		b.sendFn(b.notificationContext(), "✅ *Scanner admissions resumed*\n• Capacity available · "+label)
	}
}

func formatScannerPromotionOutcomeBatches(outcomes []events.ScannerPromotionOutcomePayload, maxItems int) []string {
	if maxItems <= 0 {
		maxItems = 20
	}
	var batches []string
	var current []string
	deferred := make(map[deferredKey]int)
	flush := func() {
		if len(current) == 0 {
			return
		}
		batches = append(batches, strings.Join(current, "\n"))
		current = nil
	}
	for _, outcome := range outcomes {
		if scannerPromotionDeferred(outcome) {
			key := deferredKey{runID: outcome.RunID, bucket: outcome.QueueBucket, reason: outcome.ReasonCode}
			deferred[key]++
			continue
		}
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
	keys := make([]deferredKey, 0, len(deferred))
	for key := range deferred {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].runID != keys[j].runID {
			return keys[i].runID < keys[j].runID
		}
		if keys[i].bucket != keys[j].bucket {
			return keys[i].bucket < keys[j].bucket
		}
		return keys[i].reason < keys[j].reason
	})
	appendDeferredSummaries(&batches, keys, deferred)
	return batches
}

// scannerPromotionDeferred identifies expected, retryable admission pressure.
// These outcomes must never produce one Telegram alert per ticker: operators
// receive one compact batch summary instead.
func scannerPromotionDeferred(outcome events.ScannerPromotionOutcomePayload) bool {
	return outcome.Outcome == "not_admitted" &&
		(outcome.ReasonCode == "staging_capacity" || outcome.ReasonCode == "queue_capacity" || outcome.ReasonCode == "watchlist_capacity")
}

func appendDeferredSummaries(batches *[]string, keys []deferredKey, deferred map[deferredKey]int) {
	const header = "⏳ *Scanner promotion deferrals*"
	current := header
	for _, key := range keys {
		line := formatScannerPromotionDeferredLine(key.bucket, key.reason, deferred[key])
		if len(current)+1+len(line) > telegramMessageMaxLength {
			*batches = append(*batches, current)
			current = header
		}
		current += "\n" + line
	}
	if current != header {
		*batches = append(*batches, current)
	}
}

func formatScannerPromotionDeferredLine(bucket, reason string, count int) string {
	label := "scanner admission allocation capacity"
	if reason == "staging_capacity" {
		label = "research staging capacity"
	} else if reason == "watchlist_capacity" {
		label = "main watchlist capacity"
	}
	line := fmt.Sprintf("• %d candidate", count)
	if count != 1 {
		line += "s"
	}
	line += " deferred: " + label
	if bucket != "" {
		line += " · " + scannerNotificationField(strings.ReplaceAll(bucket, "_", " "), 128)
	}
	return line
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
