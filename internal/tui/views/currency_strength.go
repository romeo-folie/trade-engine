package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const CurrencyStrengthPageSize = 8

// CurrencyStrengthViewData is one page of durable London-session strategy
// history. Pagination is independent from the ORB page.
type CurrencyStrengthViewData struct {
	Runs       []store.CurrencyStrengthRun
	LastUpdate time.Time
	Page       int
	HasNewer   bool
	HasOlder   bool
	Err        string
}

func FetchCurrencyStrengthViewData(ctx context.Context, page int) (d CurrencyStrengthViewData) {
	if page < 0 {
		page = 0
	}
	d.LastUpdate = time.Now()
	d.Page = page
	d.HasNewer = page > 0
	if !store.IsConnected() {
		d.Err = "database not connected"
		return d
	}
	runs, err := store.ListCurrencyStrengthRunsPage(
		ctx, store.GetDB(), CurrencyStrengthPageSize+1, page*CurrencyStrengthPageSize,
	)
	if err != nil {
		d.Err = err.Error()
		return d
	}
	if len(runs) > CurrencyStrengthPageSize {
		d.HasOlder = true
		runs = runs[:CurrencyStrengthPageSize]
	}
	d.Runs = runs
	return d
}

func RenderCurrencyStrengthView(
	d CurrencyStrengthViewData,
	width, height, scroll int,
) string {
	lines := currencyStrengthViewLines(d, width)
	if height <= 0 || len(lines) <= height {
		return strings.Join(lines, "\n")
	}
	visible := max(height-1, 1)
	scroll = min(max(scroll, 0), len(lines)-visible)
	return strings.Join(lines[scroll:scroll+visible], "\n") + "\n" +
		scrollIndicator(scroll, scroll+visible, len(lines))
}

func CurrencyStrengthMaxScroll(d CurrencyStrengthViewData, width, height int) int {
	lines := currencyStrengthViewLines(d, width)
	if height <= 0 || len(lines) <= height {
		return 0
	}
	return len(lines) - max(height-1, 1)
}

func currencyStrengthViewLines(d CurrencyStrengthViewData, width int) []string {
	var b strings.Builder
	b.WriteString(renderIntradayTabs("currency"))
	b.WriteString(dimText.Render(fmt.Sprintf(
		"  page %d · updated %s", d.Page+1, d.LastUpdate.Format("15:04:05"),
	)))
	b.WriteString("\n\n")
	if d.Err != "" {
		b.WriteString(
			lipgloss.NewStyle().Foreground(lipgloss.Color("196")).
				Render("  Currency-strength history unavailable: " + d.Err),
		)
	} else if len(d.Runs) == 0 {
		b.WriteString(dimText.Render("  No currency-strength sessions recorded on this page"))
	} else {
		for i, run := range d.Runs {
			b.WriteString(renderCurrencyStrengthRun(run))
			if i < len(d.Runs)-1 {
				b.WriteString("\n")
				b.WriteString(dimText.Render(strings.Repeat("─", max(0, min(width-2, 60)))))
				b.WriteString("\n\n")
			}
		}
	}
	content := b.String()
	if width > 0 {
		content = lipgloss.NewStyle().Width(width).Render(content)
	}
	return strings.Split(content, "\n")
}

func renderCurrencyStrengthRun(run store.CurrencyStrengthRun) string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

	var b strings.Builder
	fmt.Fprintf(
		&b, "%s  %s  %s\n",
		titleStyle.Render("Currency Strength"),
		run.SessionDate.Format("2006-01-02"),
		renderCurrencyStrengthStatus(run.Status),
	)
	fmt.Fprintf(
		&b, "%s%s–%s %s  %s%d units\n",
		labelStyle.Render("Window: "),
		run.StrengthStart, run.StrengthEnd, run.Timezone,
		labelStyle.Render("Size: "), run.Units,
	)
	if len(run.TradeInstruments) > 0 {
		fmt.Fprintf(
			&b, "%s%s\n",
			labelStyle.Render("Tradeable: "),
			strings.Join(run.TradeInstruments, ", "),
		)
	}

	switch run.Status {
	case "no_signal":
		fmt.Fprintf(&b, "%s%s\n", labelStyle.Render("Outcome: "), displayText(run.SkipReason, "no qualifying signal"))
	case "entry_failed":
		fmt.Fprintf(
			&b, "%s%s %s  order %s · %s%s\n",
			labelStyle.Render("Entry failed: "),
			strings.ToUpper(run.SignalSide),
			displayText(run.SignalSymbol, "unknown instrument"),
			displayText(run.EntryOrderID, "unknown"),
			displayText(run.EntryStatus, "rejected"),
			formatFill(run.EntryFillPrice, run.EntryFilledQty),
		)
		if run.EntryFilledQty > 0 {
			fmt.Fprintf(
				&b, "%s%.0f units filled; broker exposure requires reconciliation\n",
				labelStyle.Render("Exposure: "), run.EntryFilledQty,
			)
		}
	case "failed":
		fmt.Fprintf(&b, "%s%s\n", labelStyle.Render("Failure: "), displayText(run.Error, "unknown error"))
	case "entry_submitted", "entered", "exit_submitted", "exit_failed", "exited":
		if run.StrongestCurrency != "" || run.WeakestCurrency != "" {
			fmt.Fprintf(
				&b, "%s%s strongest · %s weakest · gap %.4f\n",
				labelStyle.Render("Decision: "),
				run.StrongestCurrency, run.WeakestCurrency, run.StrengthGap,
			)
		}
		fmt.Fprintf(
			&b, "%s%s %s  order %s · %s%s\n",
			labelStyle.Render("Entry: "),
			strings.ToUpper(run.SignalSide), run.SignalSymbol,
			displayText(run.EntryOrderID, "pending"),
			displayText(run.EntryStatus, "unknown"),
			formatFill(run.EntryFillPrice, run.EntryFilledQty),
		)
		if run.Status == "exit_submitted" || run.Status == "exit_failed" || run.Status == "exited" {
			fmt.Fprintf(
				&b, "%sorder %s · %s%s",
				labelStyle.Render("Exit: "),
				displayText(run.ExitOrderID, "pending"),
				displayText(run.ExitStatus, "unknown"),
				formatFill(run.ExitFillPrice, run.ExitFilledQty),
			)
			if run.RealizedPnL != nil {
				fmt.Fprintf(&b, "  PnL:%s", colorPnL(*run.RealizedPnL))
			}
			b.WriteString("\n")
		}
	default:
		b.WriteString(dimText.Render("Awaiting the London strength decision window") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderCurrencyStrengthStatus(status string) string {
	label := strings.ReplaceAll(status, "_", " ")
	switch status {
	case "entry_submitted", "entered", "exit_submitted":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render(label)
	case "exited", "no_signal":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render(label)
	case "entry_failed", "exit_failed", "failed":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(label)
	default:
		return dimText.Render(label)
	}
}

func displayText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func formatFill(price, qty float64) string {
	if price <= 0 {
		return ""
	}
	if qty > 0 {
		return fmt.Sprintf("  fill %.5f × %.0f", price, qty)
	}
	return fmt.Sprintf("  fill %.5f", price)
}
