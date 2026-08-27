package notify

import (
	"context"
	"fmt"
	"strings"
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
