package views

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// ORBViewData holds data for the full ORB tab (one or more recent sessions).
type ORBViewData struct {
	Sessions   []ORBSessionData
	LastUpdate time.Time
}

// ORBSessionData holds all data needed to render one ORB session panel.
type ORBSessionData struct {
	Date        time.Time
	SessionName string
	Status      string
	Candidates  []ORBCandidateRow
	Selected    []string
	Trades      []ORBTradeRow
}

// ORBCandidateRow represents a single scored candidate.
type ORBCandidateRow struct {
	Ticker     string
	GapPct     float64
	Conviction float64
	Action     string
	Reasoning  string
	// AssetClass labels the instrument kind for display: "equity" | "option" |
	// "metal" | "forex" | "futures". Populated from the stored candidate, or
	// derived (see candidateAssetClass) for rows persisted before it was stamped.
	AssetClass string
}

// ORBTradeRow represents a single bracket trade in the ORB panel.
type ORBTradeRow struct {
	Ticker     string
	Side       string
	EntryStop  float64
	StopLoss   float64
	TakeProfit float64
	Qty        int
	Status     string
	FillPrice  float64
	ExitPrice  float64
	ExitType   string
	PnL        float64
	// Options-ORB identity (empty/"equity" for non-options rows).
	AssetClass string
	OCCSymbol  string
	Right      string // "C" | "P"
	Strike     float64
}

// IsOption reports whether the row is an options-ORB trade.
func (r ORBTradeRow) IsOption() bool { return r.AssetClass == "option" }

// CostBasis is the capital deployed by the entry fill: premium × contracts × 100 for
// options (premium is quoted per share, 100 shares/contract), price × shares
// otherwise. Without the ×100, an options row understates deployed capital 100×.
func (r ORBTradeRow) CostBasis() float64 {
	mult := 1.0
	if r.IsOption() {
		mult = 100
	}
	return r.FillPrice * float64(r.Qty) * mult
}

// Label is the instrument label for display: the OCC contract for options
// (e.g. "SPY C551"), otherwise the plain ticker.
func (r ORBTradeRow) Label() string {
	if r.IsOption() && r.Right != "" {
		return fmt.Sprintf("%s %s%g", r.Ticker, r.Right, r.Strike)
	}
	return r.Ticker
}

// candidateAssetClass resolves an ORB candidate's asset class for display. It
// prefers the value stamped on the persisted candidate; for rows written before
// that field existed it derives one from the session and ticker. The options
// force-watch sentinel reasoning is our own constant (not free-form LLM prose),
// so keying on it for legacy rows is safe.
func candidateAssetClass(stored, sessionName, ticker, reasoning string) string {
	if stored != "" {
		return stored
	}
	if reasoning == "options-router force-watch" {
		return "option"
	}
	switch sessionName {
	case "futures":
		return "futures"
	case "london", "newyork", "both":
		if isSpotMetal(ticker) {
			return "metal"
		}
		return "forex"
	default: // "" — the equities scheduler
		return "equity"
	}
}

// isSpotMetal reports whether a forex-path ticker is a spot precious metal
// (XAU/XAG/XPT/XPD vs a currency), which trades on the forex scheduler but is a
// metal, not an FX pair.
func isSpotMetal(ticker string) bool {
	base, _, _ := strings.Cut(ticker, "/")
	switch strings.ToUpper(base) {
	case "XAU", "XAG", "XPT", "XPD":
		return true
	default:
		return false
	}
}

// sessionDisplayLabel turns the raw session name (also the DB uniqueness key)
// into a user-facing label. Forex sessions carry a time-window name ("newyork")
// rather than an asset class, so the instrument kind is prepended from the
// candidates it holds (e.g. "metals · newyork"). Equities/futures already name
// their asset class.
func sessionDisplayLabel(sessionName string, candidates []ORBCandidateRow) string {
	switch sessionName {
	case "":
		// The equities scheduler also hosts the options-ORB force-watch, so this
		// panel can hold equity gappers, options underlyings, or both. Name it by
		// what's actually shown rather than by the scheduler — so an options-only
		// panel reads "options", not "equities".
		return equitiesSessionLabel(candidates)
	case "london", "newyork", "both":
		return forexAssetWord(candidates) + " · " + sessionName
	default: // "futures" and any future scheduler name
		return sessionName
	}
}

// equitiesSessionLabel names the equities-scheduler panel from its trade-eligible
// candidates (the only ones RenderORBPanel shows): "options" when the panel holds
// only options force-watch, "equities · options" when it holds both, else
// "equities" (the default, including an empty panel).
func equitiesSessionLabel(candidates []ORBCandidateRow) string {
	var hasEquity, hasOption bool
	for _, c := range candidates {
		if c.Action != "trade" { // mirror RenderORBPanel's eligible filter
			continue
		}
		switch c.AssetClass {
		case "option":
			hasOption = true
		case "equity":
			hasEquity = true
		}
	}
	switch {
	case hasOption && !hasEquity:
		return "options"
	case hasOption && hasEquity:
		return "equities · options"
	default:
		return "equities"
	}
}

// forexAssetWord summarises a forex session's instrument kind for its header:
// "metals" when every candidate is a spot metal, else "forex".
func forexAssetWord(candidates []ORBCandidateRow) string {
	if len(candidates) == 0 {
		return "forex"
	}
	for _, c := range candidates {
		if c.AssetClass != "metal" {
			return "forex"
		}
	}
	return "metals"
}

// assetBadge is the short display tag for a candidate's asset class, padded so
// the columns after it stay aligned. Empty class → blank (but padded).
func assetBadge(assetClass string) string {
	tag := ""
	switch assetClass {
	case "option":
		tag = "[opt]"
	case "metal":
		tag = "[metal]"
	case "forex":
		tag = "[fx]"
	case "futures":
		tag = "[fut]"
	case "equity":
		tag = "[eq]"
	}
	return fmt.Sprintf("%-7s", tag)
}

// FetchORBViewData loads the most recent ORB sessions (newest first — a trading day has
// several concurrent sessions: equities/forex/futures, plus a morning london session) and
// their trades. Safe to call when DB is unavailable — returns an empty result.
func FetchORBViewData(ctx context.Context) (d ORBViewData) {
	d.LastUpdate = time.Now()
	if !store.IsConnected() {
		return d
	}
	db := store.GetDB()

	// A single trading day has up to 4 concurrent sessions (equities + forex + futures for
	// orb-newyork, plus a morning london forex session), so a limit of 3 truncated a day and
	// hid the futures panel. 6 covers a full day plus a little history; the view scrolls
	// (j/k) so panels past the first screenful stay reachable. created_at-DESC ordering
	// puts the newest sessions (today's, incl. futures) FIRST.
	sessions, err := store.GetRecentORBSessions(ctx, db, 6)
	if err != nil || len(sessions) == 0 {
		return d
	}

	for _, s := range sessions {
		sd := ORBSessionData{
			Date:     s.Date,
			Status:   s.Status,
			Selected: s.Selected,
		}

		// Unmarshal candidates JSON into display rows.
		if len(s.Candidates) > 0 {
			var raw []struct {
				AssetClass string `json:"asset_class"`
				Gap        struct {
					Ticker string  `json:"Ticker"`
					GapPct float64 `json:"GapPct"`
				} `json:"Gap"`
				Score struct {
					Action     string  `json:"Action"`
					Conviction float64 `json:"Conviction"`
					Reasoning  string  `json:"Reasoning"`
				} `json:"Score"`
			}
			if json.Unmarshal(s.Candidates, &raw) == nil {
				for _, c := range raw {
					sd.Candidates = append(sd.Candidates, ORBCandidateRow{
						Ticker:     c.Gap.Ticker,
						GapPct:     c.Gap.GapPct,
						Conviction: c.Score.Conviction,
						Action:     c.Score.Action,
						Reasoning:  truncate(c.Score.Reasoning, 50),
						// Derive from the RAW session name + untruncated reasoning.
						AssetClass: candidateAssetClass(c.AssetClass, s.SessionName, c.Gap.Ticker, c.Score.Reasoning),
					})
				}
			}
		}

		// Header label is derived AFTER candidates are parsed: a forex session's
		// asset word ("metals" vs "forex") depends on what it holds.
		sd.SessionName = sessionDisplayLabel(s.SessionName, sd.Candidates)

		// Fetch trades for this session.
		trades, err := store.GetORBTrades(ctx, db, s.ID)
		if err == nil {
			for _, t := range trades {
				sd.Trades = append(sd.Trades, ORBTradeRow{
					Ticker:     t.Ticker,
					Side:       t.Side,
					EntryStop:  t.EntryStop,
					StopLoss:   t.StopLoss,
					TakeProfit: t.TakeProfit,
					Qty:        t.Qty,
					Status:     t.Status,
					FillPrice:  t.FillPrice,
					ExitPrice:  t.ExitPrice,
					ExitType:   t.ExitType,
					PnL:        t.PnL,
					AssetClass: t.AssetClass,
					OCCSymbol:  t.OCCSymbol,
					Right:      t.Right,
					Strike:     t.Strike,
				})
			}
		}

		d.Sessions = append(d.Sessions, sd)
	}
	return d
}

// RenderORBView renders the full ORB tab with all recent sessions, windowed to
// `height` terminal rows starting `scroll` rows into the content. The ORB view
// scrolls by line rather than by selected row (like the table views do)
// because its session panels have variable heights and no uniform row unit.
// height <= 0 disables windowing and returns the full content.
func RenderORBView(d ORBViewData, width, height, scroll int) string {
	lines := orbViewLines(d, width)
	if height <= 0 || len(lines) <= height {
		return strings.Join(lines, "\n")
	}
	// Reserve one line for the scroll indicator.
	visible := max(height-1, 1)
	scroll = min(max(scroll, 0), len(lines)-visible)
	return strings.Join(lines[scroll:scroll+visible], "\n") + "\n" +
		scrollIndicator(scroll, scroll+visible, len(lines))
}

// ORBMaxScroll returns the largest useful scroll offset for the ORB view at
// the given size — the key handler clamps to this so the offset can't run
// past the bottom of the content.
func ORBMaxScroll(d ORBViewData, width, height int) int {
	lines := orbViewLines(d, width)
	if height <= 0 || len(lines) <= height {
		return 0
	}
	return len(lines) - max(height-1, 1)
}

// orbViewLines renders the full ORB content and splits it into physical
// terminal rows. Wrapping to width BEFORE splitting matters: a logical line
// longer than the terminal occupies several physical rows, so windowing
// unwrapped lines would overflow the viewport (same reason app.go wraps
// before clipping).
func orbViewLines(d ORBViewData, width int) []string {
	var content string
	if len(d.Sessions) == 0 {
		content = sectionTitle.Render("ORB Flow") + "\n" +
			dimText.Render("  No ORB sessions found")
	} else {
		var b strings.Builder
		b.WriteString(sectionTitle.Render("ORB Flow") +
			dimText.Render(fmt.Sprintf("  updated %s", d.LastUpdate.Format("15:04:05"))) + "\n\n")
		for i, s := range d.Sessions {
			b.WriteString(RenderORBPanel(s, width))
			if i < len(d.Sessions)-1 {
				b.WriteString("\n" + dimText.Render(strings.Repeat("─", max(0, min(width-2, 60)))) + "\n\n")
			}
		}
		content = b.String()
	}
	if width > 0 {
		content = lipgloss.NewStyle().Width(width).Render(content)
	}
	return strings.Split(content, "\n")
}

// RenderORBPanel renders a single ORB session panel.
func RenderORBPanel(data ORBSessionData, width int) string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

	var sb strings.Builder

	// Session header
	sessionLabel := data.Date.Format("2006-01-02")
	if data.SessionName != "" {
		sessionLabel += "  " + labelStyle.Render(data.SessionName)
	}
	sb.WriteString(titleStyle.Render("ORB Session") + "  " + sessionLabel + "\n")
	sb.WriteString(labelStyle.Render("Status: ") + renderORBStatus(data.Status) + "\n")

	// Candidates — show only those eligible for trading (Action == "trade")
	if len(data.Candidates) > 0 {
		var eligible []ORBCandidateRow
		for _, c := range data.Candidates {
			if c.Action == "trade" {
				eligible = append(eligible, c)
			}
		}
		sb.WriteString("\n")
		sb.WriteString(labelStyle.Render(fmt.Sprintf("Candidates (%d trade-eligible of %d scanned):", len(eligible), len(data.Candidates))) + "\n")
		if len(eligible) == 0 {
			sb.WriteString(dimText.Render("  No trade-eligible candidates") + "\n")
		}
		for _, c := range eligible {
			selected := " "
			if slices.Contains(data.Selected, c.Ticker) {
				selected = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("*")
			}
			// Badge is pre-padded to a fixed width BEFORE styling, so the ANSI
			// color codes don't throw off the %s column alignment.
			badge := dimText.Render(assetBadge(c.AssetClass))
			// %-8s fits 7-char forex/metal pairs (e.g. "XAU/USD") so the badge and
			// downstream columns stay aligned across equity/forex/futures panels.
			fmt.Fprintf(&sb, "  %s %-8s %s gap %+5.1f%%  conv %.0f%%  %s\n",
				selected, c.Ticker, badge, c.GapPct, c.Conviction*100,
				dimText.Render(c.Reasoning))
		}
	}

	// Trades
	if len(data.Trades) > 0 {
		sb.WriteString("\n")
		sb.WriteString(labelStyle.Render("Trades:") + "\n")

		var totalCost, totalPnL float64
		var wins, losses, closedCount int
		for _, t := range data.Trades {
			switch t.Status {
			case "closed":
				closedCount++
				totalPnL += t.PnL
				if t.PnL > 0 {
					wins++
				} else if t.PnL < 0 {
					losses++
				}
				if t.FillPrice != 0 {
					totalCost += t.CostBasis()
				}
				exitStr := ""
				if t.ExitPrice != 0 {
					exitStr = fmt.Sprintf(" → %.4f", t.ExitPrice)
				}
				fillStr := "—"
				if t.FillPrice != 0 {
					fillStr = fmt.Sprintf("%.4f", t.FillPrice)
				}
				fmt.Fprintf(&sb, "  %-6s %-4s  [%-8s]  qty=%-4d  %s%s   PnL:%s\n",
					t.Label(), strings.ToUpper(t.Side),
					orbExitLabel(t.ExitType),
					t.Qty, fillStr, exitStr,
					colorPnL(t.PnL))
			case "filled":
				if t.FillPrice != 0 {
					totalCost += t.CostBasis()
				}
				fmt.Fprintf(&sb, "  %-6s %-4s  [open    ]  qty=%-4d  fill=%.4f  stop=%.4f  target=%.4f\n",
					t.Label(), strings.ToUpper(t.Side),
					t.Qty, t.FillPrice, t.StopLoss, t.TakeProfit)
			default:
				statusLabel := t.Status
				if len(statusLabel) > 8 {
					statusLabel = statusLabel[:8]
				}
				fmt.Fprintf(&sb, "  %-6s %-4s  [%-8s]  qty=%-4d  entry=%.4f  stop=%.4f  target=%.4f\n",
					t.Label(), strings.ToUpper(t.Side),
					statusLabel,
					t.Qty, t.EntryStop, t.StopLoss, t.TakeProfit)
			}
		}

		// Session summary line
		if totalCost > 0 || closedCount > 0 {
			summaryParts := []string{}
			if totalCost > 0 {
				summaryParts = append(summaryParts, fmt.Sprintf("notional $%.2f", totalCost))
			}
			if closedCount > 0 {
				summaryParts = append(summaryParts, fmt.Sprintf("W:%d L:%d", wins, losses))
				summaryParts = append(summaryParts, fmt.Sprintf("net PnL: %s", colorPnL(totalPnL)))
			}
			sb.WriteString("  " + strings.Join(summaryParts, "  │  ") + "\n")
		}
	} else if data.Status != "idle" && data.Status != "" {
		sb.WriteString("\n" + dimText.Render("  No trades placed this session") + "\n")
	}

	return sb.String()
}

func orbExitLabel(exitType string) string {
	switch exitType {
	case "tp":
		return "TP hit"
	case "stop":
		return "stopped"
	case "expired":
		return "expired"
	default:
		return "closed"
	}
}

func renderORBStatus(status string) string {
	style := lipgloss.NewStyle()
	switch status {
	case "scanning":
		return style.Foreground(lipgloss.Color("33")).Render(status)
	case "candidate_ready":
		return style.Foreground(lipgloss.Color("214")).Render("candidate ready")
	case "range_watch":
		return style.Foreground(lipgloss.Color("220")).Render("range watch")
	case "executing":
		return style.Foreground(lipgloss.Color("196")).Bold(true).Render(status)
	case "done":
		return style.Foreground(lipgloss.Color("46")).Render(status)
	case "skipped":
		return style.Foreground(lipgloss.Color("245")).Render(status)
	default:
		return style.Foreground(lipgloss.Color("255")).Render(status)
	}
}
