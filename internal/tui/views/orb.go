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

	// Candidates
	if len(data.Candidates) > 0 {
		sb.WriteString("\n")
		sb.WriteString(labelStyle.Render("Candidates:") + "\n")
		for _, c := range data.Candidates {
			selected := " "
			if slices.Contains(data.Selected, c.Ticker) {
				selected = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("*")
			}
			actionStr := dimText.Render(c.Action)
			if c.Action == "trade" {
				actionStr = greenText.Render("trade")
			}
			fmt.Fprintf(&sb, "  %s %-6s  gap %+5.1f%%  conv %.0f%%  %s  %s\n",
				selected, c.Ticker, c.GapPct, c.Conviction*100, actionStr,
				dimText.Render(c.Reasoning))
		}
	}

	// Trades
	if len(data.Trades) > 0 {
		sb.WriteString("\n")
		sb.WriteString(labelStyle.Render("Trades:") + "\n")

		colFmt := "  %-6s %-4s  entry=%7.4f  stop=%7.4f  target=%7.4f  qty=%-4d  [%-9s]%s\n"
		var totalCost, totalPnL float64
		var wins, losses int
		for _, t := range data.Trades {
			extra := ""
			if t.FillPrice != 0 {
				cost := t.FillPrice * float64(t.Qty)
				totalCost += cost
				extra = fmt.Sprintf("  fill=%.4f  cost=$%.2f", t.FillPrice, cost)
			}
			if t.Status == "closed" && t.ExitPrice != 0 {
				exitLabel := t.ExitType
				if exitLabel == "" {
					exitLabel = "exit"
				}
				extra += fmt.Sprintf("  %s=%.4f", exitLabel, t.ExitPrice)
			}
			if t.PnL != 0 {
				totalPnL += t.PnL
				if t.PnL > 0 {
					wins++
				} else {
					losses++
				}
				extra += fmt.Sprintf("  PnL:%s", colorPnL(t.PnL))
			}
			fmt.Fprintf(&sb, colFmt,
				t.Ticker, strings.ToUpper(t.Side),
				t.EntryStop, t.StopLoss, t.TakeProfit, t.Qty,
				renderTradeStatus(t.Status), extra)
		}

		// Session summary line
		if totalCost > 0 || totalPnL != 0 {
			summaryParts := []string{}
			if totalCost > 0 {
				summaryParts = append(summaryParts, fmt.Sprintf("deployed $%.2f", totalCost))
			}
			if wins+losses > 0 {
				summaryParts = append(summaryParts,
					fmt.Sprintf("W:%d L:%d", wins, losses))
				summaryParts = append(summaryParts,
					fmt.Sprintf("net PnL: %s", colorPnL(totalPnL)))
			}
			sb.WriteString("  " + labelStyle.Render(strings.Join(summaryParts, "  │  ")) + "\n")
		}
	} else if data.Status != "idle" && data.Status != "" {
		sb.WriteString("\n" + dimText.Render("  No trades placed this session") + "\n")
	}

	return sb.String()
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

func renderTradeStatus(status string) string {
	switch status {
	case "filled":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render("filled")
	case "closed":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("46")).Render("closed")
	case "cancelled":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render("cancelled")
	case "pending":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Render("pending")
	default:
		return status
	}
}

