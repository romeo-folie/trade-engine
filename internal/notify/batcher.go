package notify

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

// BatchItem represents a single collected tick event for the summary.
type BatchItem struct {
	Kind       string // signal | trade | blocked
	Ticker     string
	Action     string
	Confidence float64
	Note       string // optional brief note (e.g. block reason, direction)
}

// TickBatcher collects events during a batch window and flushes a single
// per-tick summary message. Error/critical events bypass the batcher entirely.
//
// Usage:
//
//	b := NewTickBatcher(cfg, sendFn)
//	b.Add(item)
//	b.Flush() // explicit flush at end of tick; auto-flush fires on window expiry
type TickBatcher struct {
	cfg    config.TelegramConfig
	sendFn func(text string) error

	mu    sync.Mutex
	items []BatchItem
	timer *time.Timer
}

// NewTickBatcher creates a batcher. sendFn is called with the formatted summary text.
func NewTickBatcher(cfg config.TelegramConfig, sendFn func(text string) error) *TickBatcher {
	return &TickBatcher{
		cfg:    cfg,
		sendFn: sendFn,
	}
}

// Add enqueues a BatchItem. Drops the item if it does not meet the confidence
// threshold or is a HOLD/WAIT when include_holds=false. Starts the auto-flush
// timer on the first item added.
func (b *TickBatcher) Add(item BatchItem) {
	if !b.cfg.BatchEnabled {
		return
	}
	if (item.Action == "HOLD" || item.Action == "WAIT") && !b.cfg.IncludeHolds {
		return
	}
	if item.Kind == "signal" && item.Confidence < b.cfg.MinConfidenceForAlert {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	maxItems := b.cfg.MaxItemsPerBatch
	if maxItems <= 0 {
		maxItems = 20 // safe default if not configured
	}
	if len(b.items) >= maxItems {
		return // cap enforced
	}

	b.items = append(b.items, item)

	// Start window timer on first item.
	if len(b.items) == 1 && b.timer == nil {
		window := time.Duration(b.cfg.BatchWindowSec) * time.Second
		if window <= 0 {
			window = 60 * time.Second
		}
		b.timer = time.AfterFunc(window, func() {
			b.Flush()
		})
	}
}

// Flush sends the accumulated batch summary and resets state.
// Safe to call multiple times — only flushes once per batch window.
func (b *TickBatcher) Flush() {
	b.mu.Lock()
	items := b.items
	b.items = nil
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.mu.Unlock()

	if len(items) == 0 {
		return
	}

	msg := formatBatchSummary(items)
	if err := b.sendFn(msg); err != nil {
		logger.Warn("telegram: batch flush failed", "items", len(items), "err", err)
	}
}

// Reset discards any accumulated items and cancels the pending timer.
// Useful at the start of each tick to ensure a clean slate.
func (b *TickBatcher) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = nil
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
}

// formatBatchSummary builds the human-readable batch summary message.
func formatBatchSummary(items []BatchItem) string {
	var signals, trades, blocked []BatchItem
	for _, it := range items {
		switch it.Kind {
		case "signal":
			signals = append(signals, it)
		case "trade":
			trades = append(trades, it)
		case "blocked":
			blocked = append(blocked, it)
		}
	}

	ts := time.Now().UTC().Format("15:04 UTC")
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📊 *Tick summary — %s*\n", ts))
	sb.WriteString(fmt.Sprintf("Signals: %d | Trades: %d | Blocked: %d\n", len(signals), len(trades), len(blocked)))

	if len(signals) > 0 {
		sb.WriteString("\n*Signals:*\n")
		for _, s := range signals {
			note := ""
			if s.Note != "" {
				note = " — " + s.Note
			}
			sb.WriteString(fmt.Sprintf(
				"• `%s` — %s %.0f%%%s\n",
				s.Ticker, s.Action, s.Confidence*100, note,
			))
		}
	}
	if len(trades) > 0 {
		sb.WriteString("\n*Trades:*\n")
		for _, t := range trades {
			note := ""
			if t.Note != "" {
				note = " — " + t.Note
			}
			sb.WriteString(fmt.Sprintf("• `%s` %s%s\n", t.Ticker, t.Action, note))
		}
	}
	if len(blocked) > 0 {
		sb.WriteString("\n*Blocked:*\n")
		for _, bl := range blocked {
			note := ""
			if bl.Note != "" {
				note = ": " + bl.Note
			}
			sb.WriteString(fmt.Sprintf("• `%s`%s\n", bl.Ticker, note))
		}
	}

	return sb.String()
}
