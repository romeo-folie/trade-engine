package notify

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
)

func TestScannerPromotionOutcomeBatcherFlushesAtConfiguredItemLimit(t *testing.T) {
	cfg := config.TelegramConfig{BatchEnabled: true, BatchWindowSec: 60, MaxItemsPerBatch: 2}
	var messages []string
	batcher := newScannerPromotionOutcomeBatcher(cfg, func(_ context.Context, message string) { messages = append(messages, message) }, nil)
	batcher.Add(scannerOutcome("AAA"))
	batcher.Add(scannerOutcome("BBB"))
	if len(messages) != 1 {
		t.Fatalf("messages=%d, want one full batch", len(messages))
	}
	if message := messages[0]; !containsScannerOutcomeText(message, "AAA", "BBB") {
		t.Fatalf("batched message=%q", message)
	}
	batcher.Add(scannerOutcome("CCC"))
	batcher.Flush()
	if len(messages) != 2 || !containsScannerOutcomeText(messages[1], "CCC") {
		t.Fatalf("messages=%v, want flushed remainder", messages)
	}
}

func TestScannerPromotionOutcomeBatcherDoesNotDeclarePartialRunRecovered(t *testing.T) {
	cfg := config.TelegramConfig{BatchEnabled: true, BatchWindowSec: 60, MaxItemsPerBatch: 1}
	var messages []string
	batcher := newScannerPromotionOutcomeBatcher(cfg, func(_ context.Context, message string) { messages = append(messages, message) }, nil)
	batcher.send(context.Background(), []events.ScannerPromotionOutcomePayload{{
		RunID: "prior", Ticker: "OLD", Outcome: "not_admitted", QueueBucket: "earnings_scheduled", ReasonCode: "queue_capacity",
	}})
	batcher.Add(events.ScannerPromotionOutcomePayload{RunID: "same-run", Ticker: "NEW", Outcome: "admitted", QueueBucket: "earnings_scheduled"})
	batcher.Add(events.ScannerPromotionOutcomePayload{RunID: "same-run", Ticker: "LATER", Outcome: "not_admitted", QueueBucket: "earnings_scheduled", ReasonCode: "queue_capacity"})
	batcher.Flush()
	for _, message := range messages {
		if strings.Contains(message, "admissions resumed") {
			t.Fatalf("partial run incorrectly declared capacity recovered: %v", messages)
		}
	}
}

func TestScannerPromotionOutcomeBatcherResetClearsDeferralState(t *testing.T) {
	batcher := newScannerPromotionOutcomeBatcher(
		config.TelegramConfig{BatchWindowSec: 60, MaxItemsPerBatch: 20}, func(context.Context, string) {}, nil,
	)
	batcher.send(context.Background(), []events.ScannerPromotionOutcomePayload{{
		RunID: "deferred", Ticker: "AAA", Outcome: "not_admitted",
		QueueBucket: "earnings_scheduled", ReasonCode: "queue_capacity",
	}})
	batcher.send(context.Background(), []events.ScannerPromotionOutcomePayload{{
		RunID: "admitted", Ticker: "BBB", Outcome: "admitted", QueueBucket: "earnings_scheduled",
	}})
	if len(batcher.deferrals) == 0 || len(batcher.pendingRecovery) == 0 || batcher.lastDeferralAlert.IsZero() {
		t.Fatal("test setup did not create deferral and recovery state")
	}
	batcher.Reset()
	if len(batcher.deferrals) != 0 || len(batcher.pendingRecovery) != 0 || !batcher.lastDeferralAlert.IsZero() || batcher.recoveryTimer != nil {
		t.Fatalf("Reset retained capacity state: deferrals=%v pending=%v last=%v timer=%v",
			batcher.deferrals, batcher.pendingRecovery, batcher.lastDeferralAlert, batcher.recoveryTimer)
	}
}

func TestUnbatchedAdmissionCanResolveCapacityDeferral(t *testing.T) {
	var messages []string
	batcher := newScannerPromotionOutcomeBatcher(
		config.TelegramConfig{BatchEnabled: false, BatchWindowSec: 60, MaxItemsPerBatch: 20},
		func(_ context.Context, message string) { messages = append(messages, message) }, nil,
	)
	batcher.Add(events.ScannerPromotionOutcomePayload{
		RunID: "deferred", Ticker: "AAA", Outcome: "not_admitted",
		QueueBucket: "earnings_scheduled", ReasonCode: "queue_capacity",
	})
	if len(messages) != 1 || !strings.Contains(messages[0], "candidate deferred") {
		t.Fatalf("unbatched deferral messages = %v, want immediate capacity summary", messages)
	}
	batcher.ObserveAdmission(events.ScannerPromotionOutcomePayload{
		RunID: "admitted", Ticker: "BBB", Outcome: "admitted", QueueBucket: "earnings_scheduled",
	})
	batcher.mu.Lock()
	if batcher.recoveryTimer != nil {
		batcher.recoveryTimer.Stop()
		batcher.recoveryTimer = nil
	}
	generation := batcher.recoveryGeneration
	batcher.mu.Unlock()
	batcher.flushPendingRecoveries(generation)
	if len(messages) != 2 || !strings.Contains(messages[1], "Scanner admissions resumed") {
		t.Fatalf("unbatched recovery messages = %v, want immediate admission plus recovery notice", messages)
	}
}

func TestScannerPromotionOutcomeBatcherFlushesAfterWindow(t *testing.T) {
	cfg := config.TelegramConfig{BatchEnabled: true, BatchWindowSec: 1, MaxItemsPerBatch: 2}
	delivered := make(chan string, 1)
	batcher := newScannerPromotionOutcomeBatcher(cfg, func(_ context.Context, message string) { delivered <- message }, nil)
	batcher.Add(scannerOutcome("AAA"))

	select {
	case message := <-delivered:
		if !containsScannerOutcomeText(message, "AAA") {
			t.Fatalf("batched message=%q", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scanner promotion batch")
	}
}

func TestScannerPromotionOutcomeBatcherStopWaitsForTimerDelivery(t *testing.T) {
	cfg := config.TelegramConfig{BatchEnabled: true, BatchWindowSec: 60, MaxItemsPerBatch: 2}
	started := make(chan struct{})
	release := make(chan struct{})
	var messages []string
	batcher := newScannerPromotionOutcomeBatcher(cfg, func(_ context.Context, message string) {
		close(started)
		<-release
		messages = append(messages, message)
	}, nil)
	batcher.Add(scannerOutcome("AAA"))

	batcher.mu.Lock()
	done := batcher.timerDone
	if batcher.timer == nil || !batcher.timer.Stop() {
		batcher.mu.Unlock()
		t.Fatal("failed to take control of pending timer")
	}
	batcher.timer, batcher.timerDone = nil, nil
	batcher.mu.Unlock()
	go batcher.flushTimer(done)
	<-started

	stopDone := make(chan struct{})
	go func() {
		batcher.StopAndFlush(context.Background())
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("stop returned before timer delivery completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopDone:
		if len(messages) != 1 || !containsScannerOutcomeText(messages[0], "AAA") {
			t.Fatalf("messages=%v, want delivered timer outcome", messages)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not finish after timer delivery")
	}
}

func TestScannerPromotionOutcomeBatcherTimeoutDoesNotLeaveSequenceGap(t *testing.T) {
	cfg := config.TelegramConfig{BatchEnabled: true, BatchWindowSec: 60, MaxItemsPerBatch: 10}
	started := make(chan string, 2)
	release := make(chan struct{})
	firstFinished := make(chan struct{})
	delivered := make(chan string, 1)
	batcher := newScannerPromotionOutcomeBatcher(cfg, func(_ context.Context, message string) {
		started <- message
		if containsScannerOutcomeText(message, "AAA") {
			<-release
			close(firstFinished)
			return
		}
		delivered <- message
	}, nil)
	batcher.Add(scannerOutcome("AAA"))

	batcher.mu.Lock()
	done := batcher.timerDone
	if batcher.timer == nil || !batcher.timer.Stop() {
		batcher.mu.Unlock()
		t.Fatal("failed to take control of first pending timer")
	}
	batcher.timer, batcher.timerDone = nil, nil
	batcher.mu.Unlock()
	go batcher.flushTimer(done)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first batch did not begin delivery")
	}

	batcher.Add(scannerOutcome("BBB"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	batcher.StopAndFlush(ctx)
	batcher.Reset()
	close(release)

	select {
	case <-firstFinished:
	case <-time.After(time.Second):
		t.Fatal("first in-flight batch did not finish")
	}
	batcher.Add(scannerOutcome("CCC"))
	batcher.Flush()
	select {
	case message := <-delivered:
		if !containsScannerOutcomeText(message, "CCC") {
			t.Fatalf("post-reset batch=%q, want CCC", message)
		}
	case <-time.After(time.Second):
		t.Fatal("post-reset batch was blocked by a missing sequence")
	}
}

func TestScannerPromotionOutcomeBatcherSplitsOversizedOutcome(t *testing.T) {
	payload := scannerOutcome("AAA")
	payload.Sources = nil
	for i := 0; i < 30; i++ {
		payload.Sources = append(payload.Sources, events.ScannerPromotionSourcePayload{
			Source: fmt.Sprintf("source_%02d", i), Rank: i + 1,
			Evidence: fmt.Sprintf("evidence-%02d %s", i, strings.Repeat("x", 180)),
		})
	}
	batches := formatScannerPromotionOutcomeBatches([]events.ScannerPromotionOutcomePayload{payload}, 20)
	if len(batches) < 2 {
		t.Fatalf("batches=%d, want oversized outcome to be split", len(batches))
	}
	combined := strings.Join(batches, "")
	for _, want := range []string{"evidence-00", "evidence-29"} {
		if !strings.Contains(combined, want) {
			t.Fatalf("split outcome lost %q: %q", want, combined)
		}
	}
	for _, batch := range batches {
		if len(batch) > telegramMessageMaxLength {
			t.Fatalf("batch length=%d, exceeds Telegram limit", len(batch))
		}
	}
}

func TestFormatScannerPromotionOutcomeBatchesSummarizesCapacityDeferrals(t *testing.T) {
	outcomes := []events.ScannerPromotionOutcomePayload{
		{RunID: "run-1", Ticker: "AAA", Outcome: "not_admitted", QueueBucket: "market_activity", ReasonCode: "queue_capacity"},
		{RunID: "run-1", Ticker: "BBB", Outcome: "not_admitted", QueueBucket: "market_activity", ReasonCode: "queue_capacity"},
		{RunID: "run-1", Ticker: "CCC", Outcome: "admitted", Sources: []events.ScannerPromotionSourcePayload{{Source: "mover", Rank: 1}}},
	}
	batches := formatScannerPromotionOutcomeBatches(outcomes, 20)
	if len(batches) != 2 {
		t.Fatalf("batch count = %d, want 2", len(batches))
	}
	if !strings.Contains(strings.Join(batches, "\n"), "2 candidates deferred: scanner admission allocation capacity · market activity") {
		t.Fatalf("missing aggregate capacity summary: %q", batches)
	}
	if strings.Contains(strings.Join(batches, "\n"), "AAA") || strings.Contains(strings.Join(batches, "\n"), "BBB") {
		t.Fatalf("capacity summary leaked individual tickers: %q", batches)
	}
}

func TestFormatScannerPromotionOutcomeBatchesSeparatesStagingDeferrals(t *testing.T) {
	batches := formatScannerPromotionOutcomeBatches([]events.ScannerPromotionOutcomePayload{
		{RunID: "run-1", Ticker: "AAA", Outcome: "not_admitted", QueueBucket: "market_activity", ReasonCode: "staging_capacity"},
	}, 20)
	if len(batches) != 1 || !strings.Contains(batches[0], "research staging capacity") {
		t.Fatalf("staging deferral summary = %q, want research staging capacity", batches)
	}
}

func TestFormatScannerPromotionOutcomeBatchesCombinesDeferredSummaries(t *testing.T) {
	batches := formatScannerPromotionOutcomeBatches([]events.ScannerPromotionOutcomePayload{
		{RunID: "run-1", Ticker: "AAA", Outcome: "not_admitted", QueueBucket: "market_activity", ReasonCode: "staging_capacity"},
		{RunID: "run-2", Ticker: "BBB", Outcome: "not_admitted", QueueBucket: "earnings_scheduled", ReasonCode: "queue_capacity"},
		{RunID: "run-3", Ticker: "CCC", Outcome: "not_admitted", QueueBucket: "weekly_leader", ReasonCode: "watchlist_capacity"},
	}, 20)
	if len(batches) != 1 || strings.Count(batches[0], "deferred:") != 3 {
		t.Fatalf("deferred summaries=%q, want one message with three lines", batches)
	}
}

func TestScannerPromotionDeferralsAreHourlyAndEmitRecovery(t *testing.T) {
	cfg := config.TelegramConfig{BatchEnabled: true, BatchWindowSec: 60, MaxItemsPerBatch: 20}
	var messages []string
	batcher := newScannerPromotionOutcomeBatcher(cfg, func(_ context.Context, message string) { messages = append(messages, message) }, nil)
	now := time.Date(2026, time.September, 29, 13, 0, 0, 0, time.UTC)
	batcher.nowFn = func() time.Time { return now }
	deferred := func(runID, ticker string) events.ScannerPromotionOutcomePayload {
		return events.ScannerPromotionOutcomePayload{
			RunID: runID, Ticker: ticker, Outcome: "not_admitted",
			QueueBucket: "earnings_scheduled", ReasonCode: "queue_capacity",
		}
	}
	batcher.send(context.Background(), []events.ScannerPromotionOutcomePayload{
		deferred("attempt-1", "AAA"), deferred("attempt-1", "BBB"),
	})
	if len(messages) != 1 || !strings.Contains(messages[0], "2 candidates deferred") {
		t.Fatalf("first deferral alert = %v", messages)
	}
	now = now.Add(5 * time.Minute)
	batcher.send(context.Background(), []events.ScannerPromotionOutcomePayload{
		deferred("attempt-2", "AAA"), deferred("attempt-2", "BBB"), deferred("attempt-2", "CCC"),
	})
	if len(messages) != 1 {
		t.Fatalf("retry generated early reminder: %v", messages)
	}
	political := deferred("political-attempt", "CCC")
	political.QueueBucket = "political_disclosure"
	batcher.send(context.Background(), []events.ScannerPromotionOutcomePayload{political})
	if len(messages) != 1 {
		t.Fatalf("second bucket bypassed global hourly throttle: %v", messages)
	}
	now = now.Add(55 * time.Minute)
	batcher.send(context.Background(), []events.ScannerPromotionOutcomePayload{deferred("attempt-3", "AAA")})
	if len(messages) != 2 || !strings.Contains(messages[1], "4 candidates deferred") || !strings.Contains(messages[1], "1 candidate deferred: scanner admission allocation capacity · political disclosure") {
		t.Fatalf("hourly reminder should aggregate suppressed retries: %v", messages)
	}
	batcher.send(context.Background(), []events.ScannerPromotionOutcomePayload{{
		Ticker: "AAA", Outcome: "rediscovered", QueueBucket: "earnings_scheduled",
	}})
	if len(messages) != 3 || strings.Contains(messages[2], "admissions resumed") {
		t.Fatalf("rediscovery incorrectly cleared capacity deferral: %v", messages)
	}
	batcher.send(context.Background(), []events.ScannerPromotionOutcomePayload{{
		Ticker: "DDD", Outcome: "admitted", QueueBucket: "earnings_scheduled",
	}})
	batcher.mu.Lock()
	generation := batcher.recoveryGeneration
	batcher.mu.Unlock()
	batcher.flushPendingRecoveries(generation)
	if len(messages) != 5 || !strings.Contains(messages[4], "Scanner admissions resumed") {
		t.Fatalf("admission recovery notice = %v", messages)
	}
}

func TestScannerPromotionOutcomeBatcherSerializesOverlappingDeferralAndAdmission(t *testing.T) {
	cfg := config.TelegramConfig{BatchEnabled: true, BatchWindowSec: 60, MaxItemsPerBatch: 20}
	started := make(chan struct{})
	release := make(chan struct{})
	var messages []string
	var messagesMu sync.Mutex
	var blockOnce sync.Once
	batcher := newScannerPromotionOutcomeBatcher(cfg, func(_ context.Context, message string) {
		blockOnce.Do(func() {
			close(started)
			<-release
		})
		messagesMu.Lock()
		messages = append(messages, message)
		messagesMu.Unlock()
	}, nil)
	batcher.nowFn = func() time.Time { return time.Date(2026, time.October, 1, 10, 0, 0, 0, time.UTC) }

	admissionDone := make(chan struct{})
	go func() {
		batcher.send(context.Background(), []events.ScannerPromotionOutcomePayload{{
			Ticker: "AAA", Outcome: "admitted", QueueBucket: "earnings_scheduled",
		}})
		close(admissionDone)
	}()
	<-started
	deferralDone := make(chan struct{})
	go func() {
		batcher.send(context.Background(), []events.ScannerPromotionOutcomePayload{{
			Ticker: "BBB", Outcome: "not_admitted", QueueBucket: "earnings_scheduled",
			ReasonCode: "queue_capacity",
		}})
		close(deferralDone)
	}()

	select {
	case <-deferralDone:
		t.Fatal("newer deferral completed while the older admission delivery was blocked")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for _, done := range []<-chan struct{}{admissionDone, deferralDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("overlapping outcome send did not complete")
		}
	}

	messagesMu.Lock()
	defer messagesMu.Unlock()
	if len(messages) != 2 || !strings.Contains(messages[1], "1 candidate deferred") {
		t.Fatalf("serialized messages = %v, want admission then deferral alert", messages)
	}
	for _, message := range messages {
		if strings.Contains(message, "admissions resumed") {
			t.Fatalf("older admission falsely cleared newer deferral state: %v", messages)
		}
	}
}

func containsScannerOutcomeText(message string, values ...string) bool {
	for _, value := range values {
		if !strings.Contains(message, value) {
			return false
		}
	}
	return true
}

func scannerOutcome(ticker string) events.ScannerPromotionOutcomePayload {
	return events.ScannerPromotionOutcomePayload{
		Ticker: ticker, Outcome: "admitted",
		Sources: []events.ScannerPromotionSourcePayload{{Source: "mover", Rank: 1, Evidence: "ranked mover"}},
	}
}
