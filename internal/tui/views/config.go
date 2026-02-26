package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// ConfigItem represents a single toggleable/editable config item.
type ConfigItem struct {
	Key         string
	Label       string
	Value       string
	Kind        string   // "toggle" | "select" | "number"
	Options     []string // for select kind
	Description string
}

// ConfigData holds all config items for the TUI view.
type ConfigData struct {
	Items      []ConfigItem
	Selected   int
	LastUpdate time.Time
}

// FetchConfigData loads current config state from DB overrides.
func FetchConfigData(ctx context.Context, currentMode string) ConfigData {
	items := []ConfigItem{
		{
			Key: "engine.mode", Label: "Trading Mode", Kind: "select",
			Options:     []string{"manual", "auto", "paper"},
			Description: "Controls how signals are handled (manual=queue, auto/paper=execute)",
		},
		{
			Key: "risk.confidence_threshold", Label: "Min Confidence", Kind: "number",
			Description: "Minimum model confidence to pass risk gate (0.0–1.0)",
		},
		{
			Key: "risk.max_open_positions", Label: "Max Open Positions", Kind: "number",
			Description: "Maximum concurrent open trades",
		},
		{
			Key: "risk.daily_loss_limit_pct", Label: "Daily Loss Limit %", Kind: "number",
			Description: "Circuit breaker: max daily loss as % of portfolio",
		},
		{
			Key: "risk.max_drawdown_pct", Label: "Max Drawdown %", Kind: "number",
			Description: "Circuit breaker: max drawdown from peak",
		},
		{
			Key: "risk.max_position_pct", Label: "Max Position Size %", Kind: "number",
			Description: "Max single position as % of portfolio",
		},
		{
			Key: "risk.cooldown_after_loss_min", Label: "Loss Cooldown (min)", Kind: "number",
			Description: "Minutes to wait after a losing trade before new entries",
		},
	}

	// Load current values from DB overrides, fall back to defaults.
	defaults := map[string]string{
		"engine.mode":                  currentMode,
		"risk.confidence_threshold":    "0.65",
		"risk.max_open_positions":      "10",
		"risk.daily_loss_limit_pct":    "3.0",
		"risk.max_drawdown_pct":        "15.0",
		"risk.max_position_pct":        "5.0",
		"risk.cooldown_after_loss_min": "60",
	}

	for i := range items {
		val, ok, _ := store.GetOverride(ctx, items[i].Key)
		if ok {
			items[i].Value = val
		} else if d, exists := defaults[items[i].Key]; exists {
			items[i].Value = d
		}
	}

	return ConfigData{Items: items, LastUpdate: time.Now()}
}

// SetConfigValue writes a config override to the DB. Takes effect immediately.
func SetConfigValue(ctx context.Context, key, value string) error {
	return store.SetOverride(ctx, key, value)
}

// CycleMode returns the next mode in the rotation.
func CycleMode(current string) string {
	modes := []string{"manual", "auto", "paper"}
	for i, m := range modes {
		if m == current {
			return modes[(i+1)%len(modes)]
		}
	}
	return "manual"
}

// ─── Rendering ────────────────────────────────────────────────────────────────

var (
	cfgHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	cfgKeyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	cfgValStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	cfgDescStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// RenderConfig renders the config toggle view.
func RenderConfig(data ConfigData, width int) string {
	var b strings.Builder

	b.WriteString(cfgHeaderStyle.Render("Configuration"))
	b.WriteString("\n")
	b.WriteString(cfgDescStyle.Render("  Changes write to DB overrides and take effect on next engine tick."))
	b.WriteString("\n\n")

	for i, item := range data.Items {
		prefix := "  "
		if i == data.Selected {
			prefix = "▸ "
		}

		label := cfgKeyStyle.Render(item.Label)
		value := cfgValStyle.Render(item.Value)

		hint := ""
		if item.Kind == "select" {
			hint = fmt.Sprintf("  [%s]", strings.Join(item.Options, " | "))
		}

		b.WriteString(fmt.Sprintf("%s%-25s  %s%s\n", prefix, label, value, hint))
		if i == data.Selected {
			b.WriteString(fmt.Sprintf("    %s\n", cfgDescStyle.Render(item.Description)))
		}
	}

	b.WriteString(fmt.Sprintf("\n  ↑↓ navigate  Enter toggle/edit  Esc back  |  Updated %s",
		data.LastUpdate.Format("15:04:05")))
	return b.String()
}
