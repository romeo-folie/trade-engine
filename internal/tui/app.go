// Package tui implements the Bubble Tea terminal dashboard for TradeEngine.
// It subscribes to the engine event bus for real-time updates and falls back
// to DB polling at 3-second intervals when events are not flowing.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
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
	ViewORB
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
	case ViewORB:
		return "ORB"
	default:
		return "Unknown"
	}
}

// allViews lists every view in tab order.
var allViews = []View{ViewDashboard, ViewTrades, ViewWatchlist, ViewSignals, ViewConfig, ViewORB}

// viewKeys maps key presses to views.
var viewKeys = map[string]View{
	"1": ViewDashboard,
	"2": ViewTrades,
	"3": ViewWatchlist,
	"4": ViewSignals,
	"5": ViewConfig,
	"6": ViewORB,
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

	// nextCycleAt is the engine's next scheduled run-cycle time, published to the
	// shared engine_state table by the (separate) engine process. Zero if unknown.
	nextCycleAt time.Time

	// dashData is the cached dashboard data (refreshed on poll ticks).
	dashData views.DashboardData
	mode     string // engine mode for dashboard header

	// Position detail overlay
	showDetail  bool
	selectedPos int // index into dashData.Positions

	// Pending signal navigation
	selectedPending int

	// Signal approval callback (set by caller if engine is available)
	OnApprove func(ctx context.Context, signalID string) error
	OnReject  func(ctx context.Context, signalID string) error

	// View data (refreshed on poll ticks)
	tradeData     views.TradeHistoryData
	signalData    views.SignalFeedData
	watchlistData views.WatchlistData
	configData    views.ConfigData
	orbData       views.ORBViewData

	// Selection state for sub-views
	selectedTrade     int
	selectedSignal    int
	selectedWatchlist int
	selectedConfig    int
	showTradeDetail   bool
	tradeDetail       views.TradeDetailData // cached on open; avoids per-frame DB query
	configEditing     bool
	configInput       string

	// Status message (briefly shown after actions)
	statusMsg     string
	statusMsgTime time.Time
}

const maxRecentEvents = 50

// New creates a new TUI Model. The event bus may be nil (DB-only fallback).
// mode is the current engine mode (manual/auto/paper).
func New(bus *events.Bus, mode string) Model {
	if mode == "" {
		mode = "paper"
	}
	m := Model{
		activeView: ViewDashboard,
		bus:        bus,
		mode:       mode,
	}
	// Subscribe eagerly in New() so the subscription is part of the model value
	// returned to Bubble Tea — Init() has a value receiver, so setting fields
	// there would be lost.
	if bus != nil {
		m.sub = bus.SubscribeAll()
	}
	return m
}

// Init starts the poll ticker and event listener.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{pollTicker()}
	if m.sub != nil {
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
		if m.activeView == ViewConfig && m.configEditing {
			switch msg.String() {
			case "esc":
				m.configEditing = false
				m.configInput = ""
				return m, nil
			case "enter":
				if m.selectedConfig < 0 || m.selectedConfig >= len(m.configData.Items) {
					m.configEditing = false
					m.configInput = ""
					return m, nil
				}
				item := m.configData.Items[m.selectedConfig]
				v := strings.TrimSpace(m.configInput)
				if v == "" {
					m.statusMsg = "Config edit cancelled (empty value)"
					m.statusMsgTime = time.Now()
					m.configEditing = false
					m.configInput = ""
					return m, nil
				}
				if !store.IsConnected() {
					m.statusMsg = "Config update unavailable: database not connected"
				} else if err := views.SetConfigValue(context.Background(), item.Key, v); err != nil {
					m.statusMsg = fmt.Sprintf("Config update failed: %s", err)
				} else {
					m.configData.Items[m.selectedConfig].Value = v
					m.statusMsg = fmt.Sprintf("✓ Updated %s=%s", item.Key, v)
				}
				m.statusMsgTime = time.Now()
				m.configEditing = false
				m.configInput = ""
				return m, nil
			case "backspace", "ctrl+h":
				if len(m.configInput) > 0 {
					m.configInput = m.configInput[:len(m.configInput)-1]
				}
				return m, nil
			default:
				// Accept raw numeric/text input for number fields.
				r := msg.String()
				if len(r) == 1 {
					ch := r[0]
					if (ch >= '0' && ch <= '9') || ch == '.' || ch == '-' {
						m.configInput += r
						return m, nil
					}
				}
				return m, nil
			}
		}

		switch msg.String() {
		case "q", "ctrl+c":
			if m.showDetail || m.showTradeDetail {
				m.showDetail = false
				m.showTradeDetail = false
				return m, nil
			}
			m.cleanup()
			return m, tea.Quit
		case "esc":
			if m.showDetail || m.showTradeDetail {
				m.showDetail = false
				m.showTradeDetail = false
				return m, nil
			}
		case "enter":
			if m.activeView == ViewConfig && len(m.configData.Items) > 0 {
				item := m.configData.Items[m.selectedConfig]
				switch item.Kind {
				case "select", "toggle":
					var next string
					if len(item.Options) > 0 {
						idx := 0
						for i, o := range item.Options {
							if o == item.Value {
								idx = i
								break
							}
						}
						next = item.Options[(idx+1)%len(item.Options)]
					} else {
						next = views.CycleMode(item.Value)
					}
					if !store.IsConnected() {
						m.statusMsg = "Config update unavailable: database not connected"
					} else if err := views.SetConfigValue(context.Background(), item.Key, next); err != nil {
						m.statusMsg = fmt.Sprintf("Config update failed: %s", err)
					} else {
						m.configData.Items[m.selectedConfig].Value = next
						if item.Key == "engine.mode" {
							m.mode = next
						}
						m.statusMsg = fmt.Sprintf("✓ Updated %s=%s", item.Key, next)
					}
					m.statusMsgTime = time.Now()
					return m, nil
				case "number":
					m.configEditing = true
					m.configInput = item.Value
					m.statusMsg = fmt.Sprintf("Editing %s (Enter to save, Esc to cancel)", item.Key)
					m.statusMsgTime = time.Now()
					return m, nil
				}
			}
			if m.activeView == ViewDashboard && !m.showDetail && len(m.dashData.Positions) > 0 {
				m.showDetail = true
				return m, nil
			}
			if m.activeView == ViewTrades && !m.showTradeDetail && len(m.tradeData.Trades) > 0 {
				if m.selectedTrade >= len(m.tradeData.Trades) {
					m.selectedTrade = len(m.tradeData.Trades) - 1
				}
				tradeID := m.tradeData.Trades[m.selectedTrade].ID
				if detail, ok := views.FetchTradeDetail(context.Background(), tradeID); ok {
					m.tradeDetail = detail
					m.showTradeDetail = true
				}
				return m, nil
			}
		case "j", "down":
			if m.showDetail || m.showTradeDetail {
				return m, nil
			}
			switch m.activeView {
			case ViewConfig:
				if len(m.configData.Items) > 0 {
					m.selectedConfig = (m.selectedConfig + 1) % len(m.configData.Items)
					m.configData.Selected = m.selectedConfig
				}
			case ViewTrades:
				if len(m.tradeData.Trades) > 0 {
					m.selectedTrade = (m.selectedTrade + 1) % len(m.tradeData.Trades)
					m.tradeData.Selected = m.selectedTrade
				}
			case ViewSignals:
				if len(m.signalData.Signals) > 0 {
					m.selectedSignal = (m.selectedSignal + 1) % len(m.signalData.Signals)
					m.signalData.Selected = m.selectedSignal
				}
			case ViewWatchlist:
				if len(m.watchlistData.Items) > 0 {
					m.selectedWatchlist = (m.selectedWatchlist + 1) % len(m.watchlistData.Items)
					m.watchlistData.Selected = m.selectedWatchlist
				}
			case ViewDashboard:
				if m.dashData.Mode == "manual" && len(m.dashData.PendingSignals) > 0 {
					m.selectedPending = (m.selectedPending + 1) % len(m.dashData.PendingSignals)
				} else if len(m.dashData.Positions) > 0 {
					m.selectedPos = (m.selectedPos + 1) % len(m.dashData.Positions)
				}
			}
			return m, nil
		case "k", "up":
			if m.showDetail || m.showTradeDetail {
				return m, nil
			}
			switch m.activeView {
			case ViewConfig:
				if len(m.configData.Items) > 0 {
					m.selectedConfig = (m.selectedConfig - 1 + len(m.configData.Items)) % len(m.configData.Items)
					m.configData.Selected = m.selectedConfig
				}
			case ViewTrades:
				if len(m.tradeData.Trades) > 0 {
					m.selectedTrade = (m.selectedTrade - 1 + len(m.tradeData.Trades)) % len(m.tradeData.Trades)
					m.tradeData.Selected = m.selectedTrade
				}
			case ViewSignals:
				if len(m.signalData.Signals) > 0 {
					m.selectedSignal = (m.selectedSignal - 1 + len(m.signalData.Signals)) % len(m.signalData.Signals)
					m.signalData.Selected = m.selectedSignal
				}
			case ViewWatchlist:
				if len(m.watchlistData.Items) > 0 {
					m.selectedWatchlist = (m.selectedWatchlist - 1 + len(m.watchlistData.Items)) % len(m.watchlistData.Items)
					m.watchlistData.Selected = m.selectedWatchlist
				}
			case ViewDashboard:
				if m.dashData.Mode == "manual" && len(m.dashData.PendingSignals) > 0 {
					m.selectedPending = (m.selectedPending - 1 + len(m.dashData.PendingSignals)) % len(m.dashData.PendingSignals)
				} else if len(m.dashData.Positions) > 0 {
					m.selectedPos = (m.selectedPos - 1 + len(m.dashData.Positions)) % len(m.dashData.Positions)
				}
			}
			return m, nil
		case "a":
			// Approve pending signal
			if m.activeView == ViewDashboard && m.dashData.Mode == "manual" && !m.showDetail && len(m.dashData.PendingSignals) > 0 {
				idx := m.selectedPending % len(m.dashData.PendingSignals)
				sig := m.dashData.PendingSignals[idx]
				if m.OnApprove != nil {
					if err := m.OnApprove(context.Background(), sig.ID); err != nil {
						m.statusMsg = fmt.Sprintf("Approve failed: %s", err)
					} else {
						m.statusMsg = fmt.Sprintf("✓ Approved %s %s", sig.Action, sig.Ticker)
					}
				} else {
					// Fallback: update store directly
					if err := store.UpdateSignalStatus(context.Background(), sig.ID, "approved", ""); err != nil {
						m.statusMsg = fmt.Sprintf("Approve failed: %s", err)
					} else {
						m.statusMsg = fmt.Sprintf("✓ Approved %s %s (execution requires running engine)", sig.Action, sig.Ticker)
					}
				}
				m.statusMsgTime = time.Now()
				if m.selectedPending > 0 {
					m.selectedPending--
				}
				return m, nil
			}
		case "r":
			// Reject pending signal
			if m.activeView == ViewDashboard && m.dashData.Mode == "manual" && !m.showDetail && len(m.dashData.PendingSignals) > 0 {
				idx := m.selectedPending % len(m.dashData.PendingSignals)
				sig := m.dashData.PendingSignals[idx]
				if m.OnReject != nil {
					if err := m.OnReject(context.Background(), sig.ID); err != nil {
						m.statusMsg = fmt.Sprintf("Reject failed: %s", err)
					} else {
						m.statusMsg = fmt.Sprintf("✗ Rejected %s %s", sig.Action, sig.Ticker)
					}
				} else {
					if err := store.UpdateSignalStatus(context.Background(), sig.ID, "rejected", "manually rejected via TUI"); err != nil {
						m.statusMsg = fmt.Sprintf("Reject failed: %s", err)
					} else {
						m.statusMsg = fmt.Sprintf("✗ Rejected %s %s", sig.Action, sig.Ticker)
					}
				}
				m.statusMsgTime = time.Now()
				if m.selectedPending > 0 {
					m.selectedPending--
				}
				return m, nil
			}
		case "d":
			// Show detail for pending signal — handled like position detail
			if m.activeView == ViewDashboard && m.dashData.Mode == "manual" && !m.showDetail && len(m.dashData.PendingSignals) > 0 {
				m.showDetail = true
				return m, nil
			}
		case "tab":
			if !m.showDetail && !m.showTradeDetail {
				m.activeView = (m.activeView + 1) % View(len(allViews))
			}
			return m, nil
		case "shift+tab":
			if !m.showDetail && !m.showTradeDetail {
				m.activeView = (m.activeView - 1 + View(len(allViews))) % View(len(allViews))
			}
			return m, nil
		default:
			if !m.showDetail && !m.showTradeDetail {
				if v, ok := viewKeys[msg.String()]; ok {
					m.activeView = v
					return m, nil
				}
			}
		}

	case eventMsg:
		evt := events.Event(msg)
		m.recentEvents = append(m.recentEvents, evt)
		if len(m.recentEvents) > maxRecentEvents {
			m.recentEvents = m.recentEvents[len(m.recentEvents)-maxRecentEvents:]
		}
		ctx := context.Background()
		// Refresh dashboard data on trade/portfolio/signal events.
		if views.ApplyEvent(&m.dashData, evt) {
			dc := loadDisplayConfig()
			m.dashData = views.FetchDashboardData(ctx, m.mode, dc.initialCapital)
		}
		// Refresh ORB view on any ORB-related event.
		switch evt.Type {
		case events.ORBStateChanged, events.ORBCandidatesReady, events.ORBTradeUpdate:
			m.orbData = views.FetchORBViewData(ctx)
		}
		// Continue listening for events.
		if m.sub != nil {
			return m, waitForEvent(m.sub)
		}

	case pollTickMsg:
		m.lastPoll = time.Time(msg)
		ctx := context.Background()
		dc := loadDisplayConfig()
		m.dashData = views.FetchDashboardData(ctx, m.mode, dc.initialCapital)
		m.tradeData = views.FetchTradeHistory(ctx, 50)
		m.signalData = views.FetchSignalFeed(ctx, 50)
		m.watchlistData = views.FetchWatchlistData(ctx)
		m.configData = views.FetchConfigData(ctx, m.mode)
		m.orbData = views.FetchORBViewData(ctx)
		// Next engine run-cycle time, published by the engine process to engine_state.
		m.nextCycleAt = time.Time{}
		if v, ok, err := store.GetEngineState(ctx, store.EngineStateNextCycleAt); err == nil && ok {
			if t, perr := time.Parse(time.RFC3339, v); perr == nil {
				m.nextCycleAt = t
			}
		}
		// Clamp selection indices so they stay in-bounds after data shrinks.
		if n := len(m.tradeData.Trades); m.selectedTrade >= n && n > 0 {
			m.selectedTrade = n - 1
		}
		if n := len(m.signalData.Signals); m.selectedSignal >= n && n > 0 {
			m.selectedSignal = n - 1
		}
		if n := len(m.watchlistData.Items); m.selectedWatchlist >= n && n > 0 {
			m.selectedWatchlist = n - 1
		}
		if n := len(m.configData.Items); m.selectedConfig >= n && n > 0 {
			m.selectedConfig = n - 1
		}
		if n := len(m.dashData.PendingSignals); m.selectedPending >= n && n > 0 {
			m.selectedPending = n - 1
		}
		m.tradeData.Selected = m.selectedTrade
		m.signalData.Selected = m.selectedSignal
		m.watchlistData.Selected = m.selectedWatchlist
		m.configData.Selected = m.selectedConfig
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
	statusBar := m.renderStatusBar()

	// Calculate body height: total - header (1) - status bar (1) - borders (2).
	bodyHeight := m.height - 4
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	// Clip the body to the available height. lipgloss's Height() only pads a
	// short block up to the target; it does NOT truncate a tall one. Without
	// this clip an over-long view (e.g. many open positions) overflows and the
	// terminal scrolls the tab bar and column headers off the top of the screen.
	body := clipToHeight(m.renderBody(bodyHeight), bodyHeight)

	bodyStyle := lipgloss.NewStyle().
		Width(m.width).
		Height(bodyHeight)

	return fmt.Sprintf("%s\n%s\n%s", header, bodyStyle.Render(body), statusBar)
}

// clipToHeight truncates s to at most height terminal lines. Splitting on "\n"
// is ANSI-safe because lipgloss colour codes never contain newlines.
func clipToHeight(s string, height int) string {
	if height < 1 {
		height = 1
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= height {
		return s
	}
	return strings.Join(lines[:height], "\n")
}

// cleanup unsubscribes from the bus. Uses value receiver for consistency
// with Bubble Tea's Model interface (Update returns value, not pointer).
func (m Model) cleanup() {
	if m.bus != nil && m.sub != nil {
		m.bus.Unsubscribe(m.sub)
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

func (m Model) renderBody(height int) string {
	// Position detail overlay
	if m.showDetail && m.activeView == ViewDashboard && m.selectedPos < len(m.dashData.Positions) {
		return views.RenderPositionDetailStatic(m.dashData.Positions[m.selectedPos], m.width, m.height)
	}

	switch m.activeView {
	case ViewDashboard:
		return views.RenderDashboard(m.dashData, m.width, height, m.selectedPos, m.selectedPending)
	case ViewTrades:
		if m.showTradeDetail {
			return views.RenderTradeDetail(m.tradeDetail)
		}
		return views.RenderTradeHistory(m.tradeData, m.width, height)
	case ViewWatchlist:
		return views.RenderWatchlist(m.watchlistData, m.width, height)
	case ViewSignals:
		return views.RenderSignalFeed(m.signalData, m.width, height)
	case ViewConfig:
		return views.RenderConfig(m.configData, m.width)
	case ViewORB:
		return views.RenderORBView(m.orbData, m.width)
	default:
		return "unknown view"
	}
}

func (m Model) renderStatusBar() string {
	left := " [1-6] switch view  [tab] next  [q] quit"
	if m.activeView == ViewConfig {
		if m.configEditing {
			left = fmt.Sprintf(" editing value: %s", m.configInput)
		} else {
			left = " [j/k] nav  [enter] edit/toggle  [q] quit"
		}
	}
	if m.activeView == ViewDashboard && m.dashData.Mode == "manual" && len(m.dashData.PendingSignals) > 0 && !m.showDetail {
		left = " [a]pprove [r]eject [d]etail  [j/k] nav  [enter] position  [q] quit"
	}
	// Show status message for 5 seconds
	if m.statusMsg != "" && time.Since(m.statusMsgTime) < 5*time.Second {
		left = " " + m.statusMsg
	}

	lastEvent := "no events"
	if len(m.recentEvents) > 0 {
		last := m.recentEvents[len(m.recentEvents)-1]
		lastEvent = fmt.Sprintf("last: %s @ %s", last.Type, last.Timestamp.Format("15:04:05"))
	}

	right := fmt.Sprintf("next: %s  poll: %s  %s ", fmtNextCycle(m.nextCycleAt), m.lastPoll.Format("15:04:05"), lastEvent)

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

// fmtNextCycle renders the engine's next run-cycle time for the status bar.
// Shows the local clock time plus a relative countdown; "—" when unknown and
// "due" once the scheduled time has passed (engine tick imminent or stalled).
func fmtNextCycle(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Until(t)
	if d <= 0 {
		return "due"
	}
	var rel string
	switch {
	case d >= time.Hour:
		rel = fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		rel = fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		rel = fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%s (%s)", t.Local().Format("15:04"), rel)
}

// displayConfig holds per-tick config values needed by the TUI poll loop.
type displayConfig struct {
	initialCapital float64
}

// loadDisplayConfig loads the config once and returns all TUI display settings.
// On error, safe defaults are returned: initial capital 0.
func loadDisplayConfig() displayConfig {
	cfg, err := config.Load()
	if err != nil {
		return displayConfig{initialCapital: 0}
	}
	return displayConfig{
		initialCapital: cfg.Engine.InitialCapital,
	}
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
