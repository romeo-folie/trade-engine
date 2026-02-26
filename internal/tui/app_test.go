package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/romeo-folie/trade-engine/internal/events"
)

func TestNew_DefaultView(t *testing.T) {
	m := New(nil)
	if m.activeView != ViewDashboard {
		t.Errorf("expected ViewDashboard, got %d", m.activeView)
	}
}

func TestViewSwitching_NumberKeys(t *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 40

	tests := []struct {
		key  string
		want View
	}{
		{"2", ViewTrades},
		{"3", ViewWatchlist},
		{"4", ViewSignals},
		{"5", ViewConfig},
		{"1", ViewDashboard},
	}

	for _, tt := range tests {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tt.key)})
		m = updated.(Model)
		if m.activeView != tt.want {
			t.Errorf("key %q: expected view %d, got %d", tt.key, tt.want, m.activeView)
		}
	}
}

func TestViewSwitching_Tab(t *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 40

	// Tab through all views and wrap around.
	for i := 0; i < len(allViews)+1; i++ {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = updated.(Model)
	}
	// After 6 tabs from Dashboard(0): 1,2,3,4,0,1 → should be ViewTrades.
	if m.activeView != ViewTrades {
		t.Errorf("expected ViewTrades after wrapping, got %d", m.activeView)
	}
}

func TestEventMsgAccumulation(t *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 40

	evt := events.Event{
		Type:      events.DataFetched,
		Timestamp: time.Now(),
		Payload:   events.DataFetchedPayload{Ticker: "AAPL", Count: 100},
	}

	updated, _ := m.Update(eventMsg(evt))
	m = updated.(Model)

	if len(m.recentEvents) != 1 {
		t.Errorf("expected 1 event, got %d", len(m.recentEvents))
	}
	if m.recentEvents[0].Type != events.DataFetched {
		t.Errorf("expected DataFetched, got %s", m.recentEvents[0].Type)
	}
}

func TestEventMsgCap(t *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 40

	for i := 0; i < maxRecentEvents+10; i++ {
		evt := events.Event{Type: events.DataFetched, Timestamp: time.Now()}
		updated, _ := m.Update(eventMsg(evt))
		m = updated.(Model)
	}

	if len(m.recentEvents) != maxRecentEvents {
		t.Errorf("expected cap at %d, got %d", maxRecentEvents, len(m.recentEvents))
	}
}

func TestWindowSizeMsg(t *testing.T) {
	m := New(nil)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	m = updated.(Model)
	if m.width != 200 || m.height != 50 {
		t.Errorf("expected 200x50, got %dx%d", m.width, m.height)
	}
}

func TestViewRender_NoWidth(t *testing.T) {
	m := New(nil)
	out := m.View()
	if out != "loading..." {
		t.Errorf("expected loading screen, got %q", out)
	}
}

func TestViewRender_WithSize(t *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 40
	out := m.View()
	if len(out) == 0 {
		t.Error("expected non-empty render")
	}
}

func TestPollTickMsg(t *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 40

	now := time.Now()
	updated, cmd := m.Update(pollTickMsg(now))
	m = updated.(Model)

	if !m.lastPoll.Equal(now) {
		t.Error("lastPoll not updated")
	}
	if cmd == nil {
		t.Error("expected poll ticker command")
	}
}

func TestCleanup_NilBus(t *testing.T) {
	m := New(nil)
	m.cleanup() // should not panic
}

func TestCleanup_WithBus(t *testing.T) {
	bus := events.New()
	m := New(bus)
	m.sub = bus.SubscribeAll()

	before := bus.SubscriberCount()
	m.cleanup()
	after := bus.SubscriberCount()

	if after >= before {
		t.Errorf("expected subscriber count to decrease: before=%d, after=%d", before, after)
	}
}

func TestViewLabels(t *testing.T) {
	labels := map[View]string{
		ViewDashboard: "Dashboard",
		ViewTrades:    "Trades",
		ViewWatchlist: "Watchlist",
		ViewSignals:   "Signals",
		ViewConfig:    "Config",
	}
	for v, want := range labels {
		got := viewLabel(v)
		if got != want {
			t.Errorf("viewLabel(%d) = %q, want %q", v, got, want)
		}
	}
}

func TestQuitKey(t *testing.T) {
	m := New(nil)
	m.width = 120
	m.height = 40

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Error("expected quit command")
	}
}
