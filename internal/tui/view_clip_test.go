package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/romeo-folie/trade-engine/internal/tui/views"
)

// Reproduces Codex's P2: on a terminal narrower than the fixed table rows,
// lipgloss Width() wraps long lines into extra physical rows. The body handed
// to bodyStyle.Render must still fit bodyHeight after that wrapping.
func TestView_NarrowTerminalDoesNotOverflow(t *testing.T) {
	rows := make([]views.TradeRow, 60)
	for i := range rows {
		rows[i] = views.TradeRow{Ticker: "AAAAAAAA", Direction: "long", Status: "open", EntryPrice: 1, OpenedAt: time.Now()}
	}
	m := Model{width: 80, height: 24, activeView: ViewTrades, mode: "paper"}
	m.tradeData = views.TradeHistoryData{Trades: rows, Selected: 55, LastUpdate: time.Now()}

	bodyHeight := m.height - 4
	// Mirror View(): wrap to width, then clip.
	body := lipgloss.NewStyle().Width(m.width).Render(m.renderBody(bodyHeight))
	body = clipToHeight(body, bodyHeight)

	// After wrapping to width, the physical line count must not exceed bodyHeight.
	rewrapped := lipgloss.NewStyle().Width(m.width).Render(body)
	if got := strings.Count(rewrapped, "\n") + 1; got > bodyHeight {
		t.Fatalf("body wraps to %d physical lines at width 80, exceeds bodyHeight %d", got, bodyHeight)
	}
}
