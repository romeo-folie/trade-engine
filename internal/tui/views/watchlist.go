package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// WatchlistRow is a single row in the watchlist management view.
type WatchlistRow struct {
	Ticker     string
	Platform   string
	Active     bool
	Timeframes []string
	Strategies []string
	AddedAt    time.Time
}

// WatchlistData holds the watchlist for the TUI view.
type WatchlistData struct {
	Items      []WatchlistRow
	Selected   int
	LastUpdate time.Time
}

// FetchWatchlistData loads all watchlist items (active and inactive).
func FetchWatchlistData(ctx context.Context) WatchlistData {
	if !store.IsConnected() {
		return WatchlistData{LastUpdate: time.Now()}
	}
	items, err := store.GetWatchlistItems(ctx, false) // include inactive
	if err != nil {
		return WatchlistData{LastUpdate: time.Now()}
	}

	rows := make([]WatchlistRow, 0, len(items))
	for _, item := range items {
		rows = append(rows, WatchlistRow{
			Ticker:     item.Ticker,
			Platform:   item.Platform,
			Active:     item.Active,
			Timeframes: item.Timeframes,
			Strategies: item.Strategies,
			AddedAt:    item.CreatedAt,
		})
	}
	return WatchlistData{Items: rows, LastUpdate: time.Now()}
}

// ToggleWatchlistItem enables or disables a watchlist item by ticker.
func ToggleWatchlistItem(ctx context.Context, ticker string, active bool) error {
	if active {
		// Re-enable: update active=true
		_, err := store.GetDB().Exec(ctx,
			`UPDATE watchlist SET active=true, updated_at=NOW() WHERE ticker=$1`, ticker)
		return err
	}
	// Disable (soft delete) — default to stock platform
	return store.RemoveWatchlistItem(ctx, ticker, "stock")
}

// ─── Rendering ────────────────────────────────────────────────────────────────

var (
	wlHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	activeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	inactiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// RenderWatchlist renders the watchlist management view. When height > 0 the
// title, column header and separator stay fixed while the rows are windowed
// around the selection so a long watchlist never overflows the viewport
// (height <= 0 renders every row — used by tests).
func RenderWatchlist(data WatchlistData, width, height int) string {
	var b strings.Builder

	b.WriteString(wlHeaderStyle.Render("Watchlist"))
	b.WriteString("\n\n")

	if len(data.Items) == 0 {
		b.WriteString("  Watchlist is empty. Use `te watchlist add <ticker>` to add items.\n")
		return b.String()
	}

	b.WriteString(fmt.Sprintf("  %-8s  %-8s  %-8s  %-20s  %-20s  %s\n",
		"TICKER", "PLATFORM", "STATUS", "TIMEFRAMES", "STRATEGIES", "ADDED"))
	b.WriteString(fmt.Sprintf("  %s\n", strings.Repeat("─", 85)))

	// activeCount counts across the whole list (not just the visible window) so
	// the footer stays accurate while scrolling.
	activeCount := 0
	for _, item := range data.Items {
		if item.Active {
			activeCount++
		}
	}

	// Fixed chrome: title + blank + column header + separator (4) and
	// blank + footer (2).
	start, end := tableWindow(len(data.Items), data.Selected, height, 6)

	for i := start; i < end; i++ {
		item := data.Items[i]
		status := "active"
		style := activeStyle
		if !item.Active {
			status = "inactive"
			style = inactiveStyle
		}

		prefix := "  "
		if i == data.Selected {
			prefix = "▸ "
		}

		tfs := strings.Join(item.Timeframes, ",")
		if tfs == "" {
			tfs = "1d"
		}
		strats := strings.Join(item.Strategies, ",")
		if strats == "" {
			strats = "default"
		}
		if len(tfs) > 18 {
			tfs = tfs[:15] + "..."
		}
		if len(strats) > 18 {
			strats = strats[:15] + "..."
		}

		line := fmt.Sprintf("%s%-8s  %-8s  %-8s  %-20s  %-20s  %s",
			prefix, item.Ticker, item.Platform, status,
			tfs, strats, item.AddedAt.Format("2006-01-02"))
		b.WriteString(style.Render(line))
		b.WriteString("\n")
	}

	if ind := scrollIndicator(start, end, len(data.Items)); ind != "" {
		b.WriteString(ind + "\n")
	}

	b.WriteString(fmt.Sprintf("\n  %d items (%d active)  |  Updated %s  |  ↑↓ navigate  Space toggle  Esc back",
		len(data.Items), activeCount, data.LastUpdate.Format("15:04:05")))
	return b.String()
}
