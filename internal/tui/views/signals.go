package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// SignalFeedData holds the expanded signal feed for the TUI view.
type SignalFeedData struct {
	Signals    []SignalFeedRow
	Selected   int
	LastUpdate time.Time
}

// SignalFeedRow is a single row in the expanded signal feed.
type SignalFeedRow struct {
	ID          string
	Ticker      string
	Action      string
	Confidence  float64
	SignalPrice float64
	StopLoss    float64
	TakeProfit  float64
	Status      string // pending | approved | blocked | executed | expired | stale | rejected
	BlockReason string
	Strategy    string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// FetchSignalFeed loads recent signals from the database (all statuses).
func FetchSignalFeed(ctx context.Context, limit int) SignalFeedData {
	if limit == 0 {
		limit = 50
	}

	// Fetch signals across multiple tickers. We'll query per-status to get a good mix.
	var allSignals []SignalFeedRow

	// Pending/approved first.
	pending, _ := store.GetPendingSignals(ctx)
	for _, s := range pending {
		allSignals = append(allSignals, toFeedRow(s))
	}

	// Then recent signals for watchlist tickers (get from recent trades for tickers).
	// Simplified: query signals by recent tickers. For now, use a broad approach.
	items, _ := store.GetWatchlistItems(ctx, false)
	seen := make(map[string]bool)
	for _, s := range allSignals {
		seen[s.ID] = true
	}

	remaining := limit - len(allSignals)
	for _, item := range items {
		if remaining <= 0 {
			break
		}
		signals, _ := store.GetSignalsByTicker(ctx, item.Ticker, 10)
		for _, s := range signals {
			if seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			allSignals = append(allSignals, toFeedRow(s))
			remaining--
			if remaining <= 0 {
				break
			}
		}
	}

	return SignalFeedData{
		Signals:    allSignals,
		LastUpdate: time.Now(),
	}
}

func toFeedRow(s store.Signal) SignalFeedRow {
	return SignalFeedRow{
		ID:          s.ID,
		Ticker:      s.Ticker,
		Action:      s.Action,
		Confidence:  s.Confidence,
		SignalPrice: s.SignalPrice,
		StopLoss:    s.StopLoss,
		TakeProfit:  s.TakeProfit,
		Status:      s.Status,
		BlockReason: s.BlockReason,
		Strategy:    s.Strategy,
		CreatedAt:   s.CreatedAt,
		ExpiresAt:   s.ExpiresAt,
	}
}

// ─── Rendering ────────────────────────────────────────────────────────────────

var (
	signalHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	pendingStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // yellow
	executedStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // green
	blockedStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // red
	expiredStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))  // gray
	staleStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	rejectedStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("13")) // magenta
)

func signalStatusStyle(status string) lipgloss.Style {
	switch status {
	case "pending", "approved":
		return pendingStyle
	case "executed":
		return executedStyle
	case "blocked":
		return blockedStyle
	case "expired":
		return expiredStyle
	case "stale":
		return staleStyle
	case "rejected":
		return rejectedStyle
	default:
		return lipgloss.NewStyle()
	}
}

// RenderSignalFeed renders the expanded signal feed view.
func RenderSignalFeed(data SignalFeedData, width int) string {
	var b strings.Builder

	b.WriteString(signalHeaderStyle.Render("Signal Feed"))
	b.WriteString("\n\n")

	if len(data.Signals) == 0 {
		b.WriteString("  No signals yet.\n")
		return b.String()
	}

	// Header
	b.WriteString(fmt.Sprintf("  %-8s  %-5s  %-9s  %-6s  %-10s  %-10s  %s\n",
		"TICKER", "ACT", "STATUS", "CONF", "PRICE", "AGE", "DETAIL"))
	b.WriteString(fmt.Sprintf("  %s\n", strings.Repeat("─", 80)))

	for i, s := range data.Signals {
		age := time.Since(s.CreatedAt).Round(time.Second)
		ageStr := formatDuration(age)

		detail := ""
		switch s.Status {
		case "blocked":
			detail = s.BlockReason
		case "pending", "approved":
			remaining := time.Until(s.ExpiresAt).Round(time.Second)
			if remaining > 0 {
				detail = fmt.Sprintf("expires in %s", formatDuration(remaining))
			} else {
				detail = "expiring"
			}
		case "executed":
			detail = fmt.Sprintf("SL=%.2f TP=%.2f", s.StopLoss, s.TakeProfit)
		}
		if len(detail) > 35 {
			detail = detail[:32] + "..."
		}

		prefix := "  "
		if i == data.Selected {
			prefix = "▸ "
		}

		style := signalStatusStyle(s.Status)
		line := fmt.Sprintf("%s%-8s  %-5s  %-9s  %-6.2f  %-10.4f  %-10s  %s",
			prefix, s.Ticker, s.Action, s.Status,
			s.Confidence, s.SignalPrice, ageStr, detail)
		b.WriteString(style.Render(line))
		b.WriteString("\n")
	}

	counts := countStatuses(data.Signals)
	b.WriteString(fmt.Sprintf("\n  %d signals  |  %s  |  Updated %s",
		len(data.Signals), counts, data.LastUpdate.Format("15:04:05")))
	return b.String()
}

func countStatuses(signals []SignalFeedRow) string {
	m := make(map[string]int)
	for _, s := range signals {
		m[s.Status]++
	}
	var parts []string
	for _, status := range []string{"pending", "executed", "blocked", "expired", "stale", "rejected"} {
		if n, ok := m[status]; ok && n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, status))
		}
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, ", ")
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
