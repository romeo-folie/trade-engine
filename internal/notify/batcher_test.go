package notify

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func testCfg(batchEnabled bool) config.TelegramConfig {
	return config.TelegramConfig{
		BatchEnabled:          batchEnabled,
		BatchWindowSec:        2,
		MaxItemsPerBatch:      25,
		IncludeHolds:          false,
		MinConfidenceForAlert: 0.65,
	}
}

func TestBatcherAccumulatesAndFlushes(t *testing.T) {
	var sent []string
	b := NewTickBatcher(testCfg(true), func(text string) error {
		sent = append(sent, text)
		return nil
	})

	b.Add(BatchItem{Kind: "signal", Ticker: "AAPL", Action: "BUY", Confidence: 0.80})
	b.Add(BatchItem{Kind: "trade", Ticker: "TSLA", Action: "LONG", Note: "$200 qty=1"})
	b.Flush()

	if len(sent) != 1 {
		t.Fatalf("expected 1 batch send, got %d", len(sent))
	}
	if !strings.Contains(sent[0], "AAPL") || !strings.Contains(sent[0], "TSLA") {
		t.Errorf("batch message missing expected tickers: %s", sent[0])
	}
}

func TestBatcherFiltersLowConfidence(t *testing.T) {
	var callCount int32
	b := NewTickBatcher(testCfg(true), func(_ string) error {
		atomic.AddInt32(&callCount, 1)
		return nil
	})

	b.Add(BatchItem{Kind: "signal", Ticker: "AAPL", Action: "BUY", Confidence: 0.50})
	b.Flush()

	if atomic.LoadInt32(&callCount) != 0 {
		t.Error("expected no send for low-confidence signal")
	}
}

func TestBatcherFiltersHoldsWhenDisabled(t *testing.T) {
	var sent []string
	b := NewTickBatcher(testCfg(true), func(text string) error {
		sent = append(sent, text)
		return nil
	})

	b.Add(BatchItem{Kind: "signal", Ticker: "AAPL", Action: "HOLD", Confidence: 0.90})
	b.Flush()

	if len(sent) != 0 {
		t.Errorf("expected no send for HOLD when include_holds=false, got: %v", sent)
	}
}

func TestBatcherIncludesHoldsWhenEnabled(t *testing.T) {
	cfg := testCfg(true)
	cfg.IncludeHolds = true
	var sent []string
	b := NewTickBatcher(cfg, func(text string) error {
		sent = append(sent, text)
		return nil
	})

	b.Add(BatchItem{Kind: "signal", Ticker: "AAPL", Action: "HOLD", Confidence: 0.90})
	b.Flush()

	if len(sent) != 1 {
		t.Fatalf("expected 1 send with include_holds=true, got %d", len(sent))
	}
}

func TestBatcherNoSendWhenEmpty(t *testing.T) {
	var callCount int32
	b := NewTickBatcher(testCfg(true), func(_ string) error {
		atomic.AddInt32(&callCount, 1)
		return nil
	})
	b.Flush()
	if atomic.LoadInt32(&callCount) != 0 {
		t.Error("expected no send for empty batch")
	}
}

func TestBatcherDisabledAddNoOp(t *testing.T) {
	var callCount int32
	b := NewTickBatcher(testCfg(false), func(_ string) error {
		atomic.AddInt32(&callCount, 1)
		return nil
	})
	b.Add(BatchItem{Kind: "signal", Ticker: "AAPL", Action: "BUY", Confidence: 0.90})
	b.Flush()
	if atomic.LoadInt32(&callCount) != 0 {
		t.Error("expected no send when batching disabled")
	}
}

func TestBatcherMaxItemsEnforced(t *testing.T) {
	cfg := testCfg(true)
	cfg.MaxItemsPerBatch = 3
	var sent []string
	b := NewTickBatcher(cfg, func(text string) error {
		sent = append(sent, text)
		return nil
	})
	for i := range 5 {
		b.Add(BatchItem{Kind: "signal", Ticker: "T" + string(rune('A'+i)), Action: "BUY", Confidence: 0.90})
	}
	b.Flush()
	if len(sent) != 1 {
		t.Fatalf("expected 1 batch send, got %d", len(sent))
	}
	// Only 3 tickers should appear
	count := strings.Count(sent[0], "BUY")
	if count != 3 {
		t.Errorf("expected 3 BUY items in batch, got %d: %s", count, sent[0])
	}
}

func TestBatcherAutoFlushOnTimer(t *testing.T) {
	cfg := testCfg(true)
	cfg.BatchWindowSec = 1
	var callCount int32
	b := NewTickBatcher(cfg, func(_ string) error {
		atomic.AddInt32(&callCount, 1)
		return nil
	})

	b.Add(BatchItem{Kind: "signal", Ticker: "AAPL", Action: "BUY", Confidence: 0.90})
	// Wait for timer auto-flush.
	time.Sleep(1500 * time.Millisecond)

	if atomic.LoadInt32(&callCount) != 1 {
		t.Errorf("expected auto-flush after window, got %d sends", atomic.LoadInt32(&callCount))
	}
}

func TestBatcherResetClearsItems(t *testing.T) {
	var callCount int32
	b := NewTickBatcher(testCfg(true), func(_ string) error {
		atomic.AddInt32(&callCount, 1)
		return nil
	})
	b.Add(BatchItem{Kind: "signal", Ticker: "AAPL", Action: "BUY", Confidence: 0.90})
	b.Reset()
	b.Flush()
	if atomic.LoadInt32(&callCount) != 0 {
		t.Error("expected no send after Reset()")
	}
}
