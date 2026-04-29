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
}

// FetchORBViewData loads the most recent 3 ORB sessions (today + prior days)
// and their trades. Safe to call when DB is unavailable — returns an empty result.
func FetchORBViewData(ctx context.Context) (d ORBViewData) {
	d.LastUpdate = time.Now()
	if !store.IsConnected() {
		return d
	}
	db := store.GetDB()

	sessions, err := store.GetRecentORBSessions(ctx, db, 3)
	if err != nil || len(sessions) == 0 {
		return d
	}

	for _, s := range sessions {
		sd := ORBSessionData{
			Date:        s.Date,
			SessionName: s.SessionName,
			Status:      s.Status,
			Selected:    s.Selected,
		}

		// Unmarshal candidates JSON into display rows.
		if len(s.Candidates) > 0 {
			var raw []struct {
				Gap struct {
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
					})
				}
			}
		}

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
				})
			}
		}

		d.Sessions = append(d.Sessions, sd)
	}
	return d
}

// RenderORBView renders the full ORB tab with all recent sessions.
func RenderORBView(d ORBViewData, width int) string {
	if len(d.Sessions) == 0 {
		return sectionTitle.Render("ORB Flow") + "\n" +
			dimText.Render("  No ORB sessions found")
	}

	var b strings.Builder
	b.WriteString(sectionTitle.Render("ORB Flow") +
		dimText.Render(fmt.Sprintf("  updated %s", d.LastUpdate.Format("15:04:05"))) + "\n\n")

	for i, s := range d.Sessions {
		b.WriteString(RenderORBPanel(s, width))
		if i < len(d.Sessions)-1 {
			b.WriteString("\n" + dimText.Render(strings.Repeat("─", max(0, min(width-2, 60)))) + "\n\n")
		}
	}
	return b.String()
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
		sb.WriteString(labelStyle.Render(fmt.Sprintf("Candidates (%d of %d eligible):", len(eligible), len(data.Candidates))) + "\n")
		for _, c := range eligible {
			selected := " "
			if slices.Contains(data.Selected, c.Ticker) {
				selected = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("*")
			}
			fmt.Fprintf(&sb, "  %s %-6s  gap %+5.1f%%  conv %.0f%%  %s\n",
				selected, c.Ticker, c.GapPct, c.Conviction*100,
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
					totalCost += t.FillPrice * float64(t.Qty)
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
					t.Ticker, strings.ToUpper(t.Side),
					orbExitLabel(t.ExitType),
					t.Qty, fillStr, exitStr,
					colorPnL(t.PnL))
			case "filled":
				if t.FillPrice != 0 {
					totalCost += t.FillPrice * float64(t.Qty)
				}
				fmt.Fprintf(&sb, "  %-6s %-4s  [open    ]  qty=%-4d  fill=%.4f  stop=%.4f  target=%.4f\n",
					t.Ticker, strings.ToUpper(t.Side),
					t.Qty, t.FillPrice, t.StopLoss, t.TakeProfit)
			default:
				statusLabel := t.Status
				if len(statusLabel) > 8 {
					statusLabel = statusLabel[:8]
				}
				fmt.Fprintf(&sb, "  %-6s %-4s  [%-8s]  qty=%-4d  entry=%.4f  stop=%.4f  target=%.4f\n",
					t.Ticker, strings.ToUpper(t.Side),
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


