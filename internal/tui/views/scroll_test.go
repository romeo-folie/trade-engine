package views

import (
	"strings"
	"testing"
	"time"
)

func TestWindowRange(t *testing.T) {
	cases := []struct {
		name                     string
		total, selected, visible int
		wantStart, wantEnd       int
	}{
		{"fits: no windowing", 5, 3, 10, 0, 5},
		{"zero visible: full range", 5, 3, 0, 0, 5},
		{"top of long list", 100, 0, 10, 0, 10},
		{"middle centered", 100, 50, 10, 45, 55},
		{"bottom clamps to end", 100, 99, 10, 90, 100},
		{"selected past end is clamped", 100, 200, 10, 90, 100},
		{"negative selected clamps to 0", 100, -5, 10, 0, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end := windowRange(tc.total, tc.selected, tc.visible)
			if start != tc.wantStart || end != tc.wantEnd {
				t.Errorf("windowRange(%d,%d,%d) = (%d,%d), want (%d,%d)",
					tc.total, tc.selected, tc.visible, start, end, tc.wantStart, tc.wantEnd)
			}
			// The selected row must always be within the returned window.
			sel := tc.selected
			if sel >= 0 && sel < tc.total && (sel < start || sel >= end) {
				t.Errorf("selected %d not within window [%d,%d)", sel, start, end)
			}
		})
	}
}

func TestTableWindowReservesScrollLine(t *testing.T) {
	// 50 rows, height 20, chrome 6 → base visible 14, but windowing kicks in so
	// one line is reserved for the indicator → 13 rows shown.
	start, end := tableWindow(50, 0, 20, 6)
	if got := end - start; got != 13 {
		t.Errorf("expected 13 visible rows (14 - scroll line), got %d", got)
	}
}

func TestScrollIndicator(t *testing.T) {
	if s := scrollIndicator(0, 10, 10); s != "" {
		t.Errorf("expected empty indicator when everything visible, got %q", s)
	}
	s := scrollIndicator(20, 40, 100)
	for _, want := range []string{"rows 21–40 of 100", "20 above", "60 below"} {
		if !strings.Contains(s, want) {
			t.Errorf("indicator %q missing %q", s, want)
		}
	}
}

// A long trade list rendered at a fixed height must stay within that height and
// keep the "TICKER" column header on screen — the bug this fixes.
func TestRenderTradeHistory_WindowedFitsHeight(t *testing.T) {
	rows := make([]TradeRow, 60)
	for i := range rows {
		rows[i] = TradeRow{Ticker: "AAA", Direction: "long", Status: "open",
			EntryPrice: 1, OpenedAt: time.Now()}
	}
	data := TradeHistoryData{Trades: rows, Selected: 55, LastUpdate: time.Now()}

	const height = 20
	out := RenderTradeHistory(data, 120, height)
	if n := strings.Count(out, "\n") + 1; n > height {
		t.Errorf("rendered %d lines, exceeds height %d", n, height)
	}
	if !strings.Contains(out, "TICKER") {
		t.Error("column header row must remain visible")
	}
	if !strings.Contains(out, "of 60") {
		t.Error("expected a scroll indicator for a windowed list")
	}
}

// The dashboard positions table must stay bounded so the sections below it
// (watchlist, signal feed, risk bar) remain on screen with many positions.
func TestRenderDashboard_PositionsWindowedKeepsFooter(t *testing.T) {
	d := sampleData()
	d.Positions = make([]PositionRow, 80)
	for i := range d.Positions {
		d.Positions[i] = PositionRow{Ticker: "ZZZ", EntryPrice: 10, CurrentPrice: 11, Quantity: 1, Strategy: "x"}
	}
	d.OpenPositionCnt = 80

	const height = 40
	out := RenderDashboard(d, 120, height, 70, 0)
	if n := strings.Count(out, "\n") + 1; n > height {
		t.Errorf("dashboard rendered %d lines, exceeds height %d", n, height)
	}
	for _, want := range []string{"Open Positions (80)", "Signal Feed", "Risk:"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected dashboard to still contain %q with many positions", want)
		}
	}
}
