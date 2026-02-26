// Package tui implements the Bubble Tea terminal dashboard for TradeEngine.
// It subscribes to the engine event bus for real-time updates and falls back
// to DB polling at 3-second intervals when events are not flowing.
package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/tui/views"
)

// View represents the active screen in the TUI.
type View int

const (
	ViewDashboard View = iota
	ViewTrades
	ViewWatchlist
	ViewSignals
	ViewConfig
)

// viewLabel returns the display name for a view.
func viewLabel(v View) string {
	switch v {
	case ViewDashboard:
		return "Dashboard"
	case ViewTrades:
		return "Trades"
	case ViewWatchlist:
		return "Watchlist"
	case ViewSignals:
		return "Signals"
	case ViewConfig:
		return "Config"
	default:
		return "Unknown"
	}
}

// allViews lists every view in tab order.
var allViews = []View{ViewDashboard, ViewTrades, ViewWatchlist, ViewSignals, ViewConfig}

// viewKeys maps key presses to views.
var viewKeys = map[string]View{
	"1": ViewDashboard,
	"2": ViewTrades,
	"3": ViewWatchlist,
	"4": ViewSignals,
	"5": ViewConfig,
}

// ── Messages ──────────────────────────────────────────────────────────────────

// eventMsg wraps an engine event delivered via the bus subscription.
type eventMsg events.Event

// pollTickMsg triggers a DB poll fallback.
type pollTickMsg time.Time

// ── Model ─────────────────────────────────────────────────────────────────────

// Model is the top-level Bubble Tea model for the dashboard.
type Model struct {
	activeView View
	width      int
	height     int
	bus        *events.Bus
	sub        *events.Subscription

	// recentEvents holds the last N events for the status line.
	recentEvents []events.Event
	lastPoll     time.Time

	// dashData is the cached dashboard data (refreshed on poll ticks).
	dashData views.DashboardData
	mode     string // engine mode for dashboard header
}

const maxRecentEvents = 50

// New creates a new TUI Model. The event bus may be nil (DB-only fallback).
// mode is the current engine mode (manual/auto/paper).
func New(bus *events.Bus, mode string) Model {
	if mode == "" {
		mode = "paper"
	}
	return Model{
		activeView: ViewDashboard,
		bus:        bus,
		mode:       mode,
	}
}

// Init subscribes to the event bus and starts the poll ticker.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{pollTicker()}
	if m.bus != nil {
		m.sub = m.bus.SubscribeAll()
		cmds = append(cmds, waitForEvent(m.sub))
	}
	return tea.Batch(cmds...)
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.cleanup()
			return m, tea.Quit
		case "tab":
			m.activeView = (m.activeView + 1) % View(len(allViews))
			return m, nil
		case "shift+tab":
			m.activeView = (m.activeView - 1 + View(len(allViews))) % View(len(allViews))
			return m, nil
		default:
			if v, ok := viewKeys[msg.String()]; ok {
				m.activeView = v
				return m, nil
			}
		}

	case eventMsg:
		evt := events.Event(msg)
		m.recentEvents = append(m.recentEvents, evt)
		if len(m.recentEvents) > maxRecentEvents {
			m.recentEvents = m.recentEvents[len(m.recentEvents)-maxRecentEvents:]
		}
		// Continue listening for events.
		if m.sub != nil {
			return m, waitForEvent(m.sub)
		}

	case pollTickMsg:
		m.lastPoll = time.Time(msg)
		m.dashData = views.FetchDashboardData(context.Background(), m.mode)
		return m, pollTicker()
	}

	return m, nil
}

// View renders the current screen.
func (m Model) View() string {
	if m.width == 0 {
		return "loading..."
	}

	header := m.renderHeader()
	body := m.renderBody()
	statusBar := m.renderStatusBar()

	// Calculate body height: total - header (1) - status bar (1) - borders (2).
	bodyHeight := m.height - 4
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	bodyStyle := lipgloss.NewStyle().
		Width(m.width).
		Height(bodyHeight)

	return fmt.Sprintf("%s\n%s\n%s", header, bodyStyle.Render(body), statusBar)
}

// cleanup unsubscribes from the bus.
func (m *Model) cleanup() {
	if m.bus != nil && m.sub != nil {
		m.bus.Unsubscribe(m.sub)
		m.sub = nil
	}
}

// ── Rendering helpers ─────────────────────────────────────────────────────────

var (
	tabActiveStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("229")).
			Background(lipgloss.Color("57")).
			Padding(0, 1)

	tabInactiveStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("250")).
				Background(lipgloss.Color("236")).
				Padding(0, 1)

	headerStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("236"))

	statusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("250")).
			Background(lipgloss.Color("235"))
)

func (m Model) renderHeader() string {
	var tabs string
	for i, v := range allViews {
		label := fmt.Sprintf("%d:%s", i+1, viewLabel(v))
		if v == m.activeView {
			tabs += tabActiveStyle.Render(label)
		} else {
			tabs += tabInactiveStyle.Render(label)
		}
		if i < len(allViews)-1 {
			tabs += " "
		}
	}

	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("117")).
		Render("TradeEngine")

	return headerStyle.Width(m.width).Render(title + "  " + tabs)
}

func (m Model) renderBody() string {
	switch m.activeView {
	case ViewDashboard:
		return views.RenderDashboard(m.dashData, m.width, m.height)
	case ViewTrades:
		return m.renderPlaceholder("Trade History", "All trades with P&L, strategy, timing")
	case ViewWatchlist:
		return m.renderPlaceholder("Watchlist", "Active tickers, indicators, last analysis")
	case ViewSignals:
		return m.renderPlaceholder("Signal Feed", "Pending, executed, blocked, expired signals")
	case ViewConfig:
		return m.renderPlaceholder("Configuration", "Engine mode, risk params, model config")
	default:
		return "unknown view"
	}
}

func (m Model) renderPlaceholder(title, desc string) string {
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("117")).
		MarginTop(1).
		MarginLeft(2)

	descStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("245")).
		MarginLeft(2)

	eventCount := fmt.Sprintf("Events received: %d", len(m.recentEvents))
	eventStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("243")).
		MarginLeft(2).
		MarginTop(1)

	return titleStyle.Render(title) + "\n" +
		descStyle.Render(desc) + "\n" +
		eventStyle.Render(eventCount) + "\n" +
		descStyle.Render("(placeholder — implementation coming in next sub-issues)")
}

func (m Model) renderStatusBar() string {
	left := " [1-5] switch view  [tab] next  [q] quit"

	lastEvent := "no events"
	if len(m.recentEvents) > 0 {
		last := m.recentEvents[len(m.recentEvents)-1]
		lastEvent = fmt.Sprintf("last: %s @ %s", last.Type, last.Timestamp.Format("15:04:05"))
	}

	right := fmt.Sprintf("poll: %s  %s ", m.lastPoll.Format("15:04:05"), lastEvent)

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}
	padding := ""
	for i := 0; i < gap; i++ {
		padding += " "
	}

	return statusBarStyle.Width(m.width).Render(left + padding + right)
}

// ── Commands ──────────────────────────────────────────────────────────────────

// pollTicker returns a command that fires a pollTickMsg every 3 seconds.
func pollTicker() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return pollTickMsg(t)
	})
}

// waitForEvent blocks until the next event arrives on the bus subscription.
func waitForEvent(sub *events.Subscription) tea.Cmd {
	return func() tea.Msg {
		if sub == nil {
			return nil
		}
		evt, ok := <-sub.C
		if !ok {
			return nil
		}
		return eventMsg(evt)
	}
}
