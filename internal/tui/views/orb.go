package views

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// ORBSessionData holds all data needed to render the ORB status panel.
type ORBSessionData struct {
	Date       time.Time
	Status     string
	Candidates []ORBCandidateRow
	Selected   []string
	Trades     []ORBTradeRow
}

// ORBCandidateRow represents a single candidate in the ORB panel.
type ORBCandidateRow struct {
	Ticker     string
	GapPct     float64
	Conviction float64
	Reasoning  string
}

// ORBTradeRow represents a single trade in the ORB panel.
type ORBTradeRow struct {
	Ticker     string
	Side       string
	EntryStop  float64
	StopLoss   float64
	TakeProfit float64
	Qty        int
	Status     string
	FillPrice  float64
	PnL        float64
}

// RenderORBPanel renders the ORB session status for the dashboard.
func RenderORBPanel(data ORBSessionData, width int) string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	valueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("255"))

	var sb strings.Builder

	sb.WriteString(titleStyle.Render("ORB Session"))
	sb.WriteString("\n")
	sb.WriteString(labelStyle.Render("Date:   ") + valueStyle.Render(data.Date.Format("2006-01-02")))
	sb.WriteString("\n")
	sb.WriteString(labelStyle.Render("Status: ") + renderORBStatus(data.Status))
	sb.WriteString("\n")

	if len(data.Candidates) > 0 {
		sb.WriteString("\n")
		sb.WriteString(labelStyle.Render("Candidates:"))
		sb.WriteString("\n")
		for _, c := range data.Candidates {
			marker := " "
			for _, sel := range data.Selected {
				if sel == c.Ticker {
					marker = "*"
					break
				}
			}
			sb.WriteString(fmt.Sprintf("  %s %-5s  gap %+5.1f%%  conviction %.0f%%  %s\n",
				marker, c.Ticker, c.GapPct, c.Conviction*100, c.Reasoning))
		}
	}

	if len(data.Trades) > 0 {
		sb.WriteString("\n")
		sb.WriteString(labelStyle.Render("Trades:"))
		sb.WriteString("\n")
		for _, t := range data.Trades {
			pnlStr := ""
			if t.PnL != 0 {
				pnlStr = fmt.Sprintf(" PnL: $%.2f", t.PnL)
			}
			sb.WriteString(fmt.Sprintf("  %-5s %4s  entry=%.2f  stop=%.2f  target=%.2f  qty=%d  [%s]%s\n",
				t.Ticker, strings.ToUpper(t.Side), t.EntryStop, t.StopLoss, t.TakeProfit, t.Qty, t.Status, pnlStr))
		}
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
		return style.Foreground(lipgloss.Color("196")).Render(status)
	case "done":
		return style.Foreground(lipgloss.Color("46")).Render(status)
	case "skipped":
		return style.Foreground(lipgloss.Color("245")).Render(status)
	default:
		return style.Foreground(lipgloss.Color("255")).Render(status)
	}
}
