package views

import (
	"strings"
	"testing"
	"time"
)

func TestRenderORBPanelEmpty(t *testing.T) {
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "pending",
	}
	out := RenderORBPanel(data, 80)
	if !strings.Contains(out, "Spec Session") {
		t.Error("expected 'Spec Session' title")
	}
	if !strings.Contains(out, "2026-03-20") {
		t.Error("expected date in output")
	}
}

func TestRenderORBPanelWithCandidates(t *testing.T) {
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "candidate_ready",
		Candidates: []ORBCandidateRow{
			{Ticker: "AAPL", GapPct: 5.2, Conviction: 0.85, Action: "trade", Reasoning: "earnings beat"},
			{Ticker: "TSLA", GapPct: -4.1, Conviction: 0.60, Action: "skip", Reasoning: "no clear catalyst"},
		},
		Selected: []string{"AAPL"},
	}
	out := RenderORBPanel(data, 80)
	if !strings.Contains(out, "AAPL") {
		t.Error("expected AAPL in output")
	}
	if !strings.Contains(out, "candidate ready") {
		t.Error("expected 'candidate ready' status")
	}
}

func TestRenderORBPanelWithTrades(t *testing.T) {
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "done",
		Trades: []ORBTradeRow{
			{Ticker: "AAPL", Side: "buy", EntryStop: 150.15, StopLoss: 139.86, TakeProfit: 170.73, Qty: 9, Status: "closed", FillPrice: 150.20, ExitPrice: 170.73, ExitType: "tp", PnL: 125.50},
		},
	}
	out := RenderORBPanel(data, 80)
	if !strings.Contains(out, "TP hit") {
		t.Error("expected 'TP hit' exit label in trade row")
	}
	if !strings.Contains(out, "PnL") {
		t.Error("expected PnL in output")
	}
}

func TestRenderORBPanelCostBasis(t *testing.T) {
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "done",
		Trades: []ORBTradeRow{
			{Ticker: "AAPL", Side: "buy", EntryStop: 150.00, StopLoss: 148.00, TakeProfit: 154.00, Qty: 10, Status: "filled", FillPrice: 150.20},
		},
	}
	out := RenderORBPanel(data, 120)
	// cost = 150.20 * 10 = 1502.00; per-trade row no longer shows cost, only summary footer does
	if !strings.Contains(out, "notional $1502.00") {
		t.Errorf("expected 'notional $1502.00' in footer, got:\n%s", out)
	}
	// no closed trades → no W/L in footer
	if strings.Contains(out, "W:") {
		t.Errorf("expected no W/L count for unfinalised trade, got:\n%s", out)
	}
}

func TestRenderORBPanelSessionSummary(t *testing.T) {
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "done",
		Trades: []ORBTradeRow{
			{Ticker: "AAPL", Side: "buy", EntryStop: 150.00, StopLoss: 148.00, TakeProfit: 154.00, Qty: 10, Status: "closed", FillPrice: 150.20, ExitPrice: 154.00, ExitType: "tp", PnL: 38.00},
			{Ticker: "NVDA", Side: "sell", EntryStop: 820.00, StopLoss: 825.00, TakeProfit: 808.00, Qty: 5, Status: "closed", FillPrice: 819.80, ExitPrice: 825.00, ExitType: "stop", PnL: -26.00},
		},
	}
	out := RenderORBPanel(data, 160)
	if !strings.Contains(out, "W:1 L:1") {
		t.Errorf("expected 'W:1 L:1' in footer, got:\n%s", out)
	}
	if !strings.Contains(out, "net PnL") {
		t.Errorf("expected 'net PnL' in footer, got:\n%s", out)
	}
	if !strings.Contains(out, "notional") {
		t.Errorf("expected 'notional' in footer, got:\n%s", out)
	}
}

func TestRenderORBPanelBreakevenTrade(t *testing.T) {
	// A closed trade with exactly PnL == 0 should still show the footer.
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "done",
		Trades: []ORBTradeRow{
			{Ticker: "AAPL", Side: "buy", EntryStop: 150.00, StopLoss: 148.00, TakeProfit: 154.00, Qty: 10, Status: "closed", FillPrice: 150.00, ExitPrice: 150.00, ExitType: "exit", PnL: 0},
		},
	}
	out := RenderORBPanel(data, 120)
	if !strings.Contains(out, "net PnL") {
		t.Errorf("expected 'net PnL' for breakeven closed trade, got:\n%s", out)
	}
	// breakeven counts as neither win nor loss
	if !strings.Contains(out, "W:0 L:0") {
		t.Errorf("expected 'W:0 L:0' for breakeven, got:\n%s", out)
	}
}

func TestRenderORBPanelPendingNoFooter(t *testing.T) {
	// Pending trades with no fill should produce no footer at all.
	data := ORBSessionData{
		Date:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Status: "executing",
		Trades: []ORBTradeRow{
			{Ticker: "AAPL", Side: "buy", EntryStop: 150.00, StopLoss: 148.00, TakeProfit: 154.00, Qty: 10, Status: "pending"},
		},
	}
	out := RenderORBPanel(data, 120)
	if strings.Contains(out, "notional") {
		t.Errorf("expected no footer for all-pending trades, got:\n%s", out)
	}
	if strings.Contains(out, "net PnL") {
		t.Errorf("expected no footer for all-pending trades, got:\n%s", out)
	}
}

func TestORBTradeRowCostBasis(t *testing.T) {
	// Equity: price × shares.
	eq := ORBTradeRow{FillPrice: 150, Qty: 10}
	if got := eq.CostBasis(); got != 1500 {
		t.Errorf("equity cost basis = %v; want 1500", got)
	}
	// Option: premium × contracts × 100 (was 100× understated without the multiplier).
	opt := ORBTradeRow{FillPrice: 2.00, Qty: 3, AssetClass: "option"}
	if got := opt.CostBasis(); got != 600 {
		t.Errorf("option cost basis = %v; want 600 (2.00 × 3 × 100)", got)
	}
}

// manySessionsView builds an ORB view tall enough to need windowing.
func manySessionsView(n int) ORBViewData {
	d := ORBViewData{LastUpdate: time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)}
	for i := 0; i < n; i++ {
		d.Sessions = append(d.Sessions, ORBSessionData{
			Date:        time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
			SessionName: "newyork",
			Status:      "done",
			Trades: []ORBTradeRow{
				{Ticker: "XAUUSD", Side: "buy", Qty: 10, Status: "closed", FillPrice: 2400, ExitPrice: 2410, ExitType: "tp", PnL: 100},
				{Ticker: "EURUSD", Side: "sell", Qty: 5000, Status: "closed", FillPrice: 1.08, ExitPrice: 1.09, ExitType: "stop", PnL: -50},
			},
		})
	}
	return d
}

func TestRenderORBViewWindowedFitsHeight(t *testing.T) {
	d := manySessionsView(6)

	const height = 20
	out := RenderORBView(d, 120, height, 0)
	if n := strings.Count(out, "\n") + 1; n > height {
		t.Errorf("rendered %d lines, exceeds height %d", n, height)
	}
	if !strings.Contains(out, "Spec Flow") {
		t.Error("expected the view title on the first page")
	}
	if !strings.Contains(out, "below") {
		t.Error("expected a scroll indicator for a windowed view")
	}
}

func TestRenderORBViewScrollReachesBottom(t *testing.T) {
	d := manySessionsView(6)

	const width, height = 120, 20
	maxScroll := ORBMaxScroll(d, width, height)
	if maxScroll <= 0 {
		t.Fatalf("expected a positive max scroll for 6 sessions at height %d, got %d", height, maxScroll)
	}
	out := RenderORBView(d, width, height, maxScroll)
	if n := strings.Count(out, "\n") + 1; n > height {
		t.Errorf("rendered %d lines at max scroll, exceeds height %d", n, height)
	}
	// The last content line of the full render must be visible at max scroll.
	full := RenderORBView(d, width, 0, 0)
	fullLines := strings.Split(full, "\n")
	last := strings.TrimRight(fullLines[len(fullLines)-1], " ")
	if last == "" { // full render has no trailing pad; find last non-empty
		for i := len(fullLines) - 1; i >= 0; i-- {
			if s := strings.TrimRight(fullLines[i], " "); s != "" {
				last = s
				break
			}
		}
	}
	if !strings.Contains(out, last) {
		t.Errorf("expected last content line %q visible at max scroll, got:\n%s", last, out)
	}
	if !strings.Contains(out, "above") {
		t.Error("expected an above-indicator at max scroll")
	}
}

func TestRenderORBViewScrollClamped(t *testing.T) {
	d := manySessionsView(6)

	const width, height = 120, 20
	atMax := RenderORBView(d, width, height, ORBMaxScroll(d, width, height))
	beyond := RenderORBView(d, width, height, 10_000)
	if atMax != beyond {
		t.Error("scroll offset past the end must clamp to the bottom page")
	}
	negative := RenderORBView(d, width, height, -5)
	top := RenderORBView(d, width, height, 0)
	if negative != top {
		t.Error("negative scroll offset must clamp to the top page")
	}
}

func TestRenderORBViewFitsWithoutScrolling(t *testing.T) {
	d := manySessionsView(1)
	out := RenderORBView(d, 120, 200, 0)
	if strings.Contains(out, "rows ") {
		t.Errorf("expected no scroll indicator when content fits, got:\n%s", out)
	}
	if ORBMaxScroll(d, 120, 200) != 0 {
		t.Error("expected zero max scroll when content fits")
	}
}
