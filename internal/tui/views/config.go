package views

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const (
	configSourceFile     = "resolved"
	configSourceOverride = "DB override"
)

// ConfigItem represents one operational setting shown in the dashboard.
// Only engine.mode is currently consumed from DB overrides; all other values
// are accurate, read-only snapshots of the resolved YAML configuration.
type ConfigItem struct {
	Key         string
	Label       string
	Value       string
	Kind        string   // "select" | "readonly"
	Options     []string // for select kind
	Description string
	Source      string
}

// ConfigData holds the configuration snapshot rendered by the TUI.
type ConfigData struct {
	Items      []ConfigItem
	Selected   int
	LastUpdate time.Time
	LoadError  string
}

// FetchConfigData loads the resolved YAML configuration and overlays the one
// runtime override the engine currently honors: engine.mode.
func FetchConfigData(ctx context.Context, currentMode string) ConfigData {
	cfg, err := config.Load()
	if err != nil {
		return ConfigData{
			Items: []ConfigItem{{
				Key: "engine.mode", Label: "Engine Mode", Value: currentMode,
				Kind: "readonly", Source: "dashboard startup",
				Description: "Configuration could not be reloaded.",
			}},
			LastUpdate: time.Now(),
			LoadError:  err.Error(),
		}
	}

	mode := cfg.Engine.Mode
	modeSource := configSourceFile
	var overrideErr error
	if store.IsConnected() {
		if overridden, ok, err := store.GetOverride(ctx, "engine.mode"); err != nil {
			overrideErr = err
		} else if ok {
			mode = overridden
			modeSource = configSourceOverride
		}
	}

	data := ConfigData{
		Items:      buildConfigItems(cfg, mode, modeSource),
		LastUpdate: time.Now(),
	}
	if store.IsConnected() {
		if control, err := store.GetBuyControl(ctx, store.BuyControlStocks); err != nil {
			data.LoadError = "buy control unavailable: " + err.Error()
		} else {
			value := "enabled"
			if !control.Enabled {
				value = "paused"
			}
			source := "default"
			if !control.UpdatedAt.IsZero() {
				source = fmt.Sprintf("%s · %s", control.UpdatedBy, control.UpdatedAt.UTC().Format(time.RFC3339))
			}
			data.Items = append(data.Items, ConfigItem{
				Key: "runtime.buy_control", Label: "RUNTIME · New Stock Buys", Value: value,
				Kind: "readonly", Source: source,
				Description: "Persistent operator control. Exits and protective stops remain active when paused.",
			})
		}
	}
	if overrideErr != nil {
		message := "mode override unavailable: " + overrideErr.Error()
		if data.LoadError != "" {
			data.LoadError += "; " + message
		} else {
			data.LoadError = message
		}
	}
	return data
}

func buildConfigItems(cfg *config.Config, mode, modeSource string) []ConfigItem {
	risk := cfg.Risk
	entry := risk.EntryGates
	return []ConfigItem{
		{
			Key: "engine.mode", Label: "ENGINE · Mode", Value: mode,
			Kind: "select", Options: []string{"manual", "auto", "paper"},
			Source:      modeSource,
			Description: "DB startup override. Restart the engine and dashboard after changing it.",
		},
		{
			Key: "engine.tick_interval_sec", Label: "ENGINE · Tick Interval",
			Value: secondsLabel(cfg.Engine.TickIntervalSec), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Time between standard engine cycles while the market is open.",
		},
		{
			Key: "risk.confidence_threshold", Label: "RISK · Confidence Floor",
			Value: decimal(risk.ConfidenceThreshold), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Fallback confidence floor when no R:R confidence tier applies.",
		},
		{
			Key: "risk.confidence_tiers", Label: "RISK · Confidence Tiers",
			Value: confidenceTiersLabel(risk.ConfidenceTiers), Kind: "readonly",
			Source:      configSourceFile,
			Description: "R:R floor to minimum confidence; first matching tier wins.",
		},
		{
			Key: "risk.min_risk_reward", Label: "RISK · Minimum R:R",
			Value: decimal(risk.MinRiskReward), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Minimum reward-to-risk ratio required for a signal.",
		},
		{
			Key: "risk.max_dollar_risk_pct", Label: "RISK · Max Dollar Risk",
			Value: disabledPercent(risk.MaxDollarRiskPct), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Maximum portfolio equity at risk between entry and stop.",
		},
		{
			Key: "risk.max_position_pct", Label: "RISK · Max Position",
			Value: percent(risk.MaxPositionPct), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Maximum allocation to one position.",
		},
		{
			Key: "risk.max_open_positions", Label: "RISK · Open Positions",
			Value: strconv.Itoa(risk.MaxOpenPositions), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Maximum number of distinct concurrent positions.",
		},
		{
			Key: "risk.max_per_ticker", Label: "RISK · Lots Per Ticker",
			Value: disabledInt(risk.MaxPerTicker), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Same-direction open-lot cap per ticker; disabled when zero.",
		},
		{
			Key: "risk.pyramid_enabled", Label: "RISK · Pyramiding",
			Value: pyramidLabel(risk.PyramidEnabled, risk.PyramidMinGainPct), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Adds to winners only after the configured minimum gain.",
		},
		{
			Key: "risk.daily_loss_limit_pct", Label: "RISK · Daily Loss Limit",
			Value: percent(risk.DailyLossLimitPct), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Daily-loss circuit breaker as a percentage of equity.",
		},
		{
			Key: "risk.max_drawdown_pct", Label: "RISK · Max Drawdown",
			Value: percent(risk.MaxDrawdownPct), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Maximum portfolio drawdown from the tracked peak.",
		},
		{
			Key: "risk.cooldown_after_loss_min", Label: "RISK · Loss Cooldown",
			Value: minutesLabel(risk.CooldownAfterLoss), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Minimum delay before a new entry after a losing trade.",
		},
		{
			Key: "risk.signal_safety", Label: "RISK · Signal Safety",
			Value: fmt.Sprintf("%s expiry · %s stale move",
				minutesLabel(risk.SignalExpiryMin), disabledPercent(risk.StalePriceMovePct)),
			Kind: "readonly", Source: configSourceFile,
			Description: "Rejects signals that are too old or whose price has moved too far.",
		},
		{
			Key: "risk.default_exits", Label: "EXITS · Default Stop / TP",
			Value: fmt.Sprintf("%s / %s",
				percent(risk.DefaultStopLossPct), percent(risk.DefaultTakeProfitPct)),
			Kind: "readonly", Source: configSourceFile,
			Description: "Fallback exit levels when a signal does not provide its own.",
		},
		{
			Key: "risk.trailing_stop.enabled", Label: "EXITS · Trailing Stop",
			Value: enabled(risk.TrailingStop.Enabled), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Server-side protective-stop manager.",
		},
		{
			Key: "risk.trailing_stop.rule", Label: "EXITS · Trail Rule",
			Value: defaultText(risk.TrailingStop.Rule, "atr"), Kind: "readonly",
			Source:      configSourceFile,
			Description: "ATR ratchet or daily EMA9 close exit.",
		},
		{
			Key: "risk.uncap_take_profit", Label: "EXITS · Uncap Take Profit",
			Value: enabled(risk.UncapTakeProfit), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Lets the active trailing rule own winning exits.",
		},
		{
			Key: "risk.exit_conf_min", Label: "EXITS · SELL Confidence",
			Value: disabledDecimal(risk.ExitConfMin), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Minimum analyst SELL confidence; disabled when zero.",
		},
		{
			Key: "risk.bracket_exits_enabled", Label: "EXITS · Broker Brackets",
			Value: enabled(risk.BracketExitsEnabled), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Places eligible stock exits at the broker.",
		},
		{
			Key:   "risk.entry_gates.trend_alignment_enabled",
			Label: "ENTRY · Trend Alignment",
			Value: enabled(entry.TrendAlignmentEnabled), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Blocks stock BUYs in a confirmed daily EMA9/EMA21 downtrend.",
		},
		{
			Key:   "risk.entry_gates.pullback_strength_enabled",
			Label: "ENTRY · Pullback Strength", Value: enabled(entry.PullbackStrengthEnabled),
			Kind: "readonly", Source: configSourceFile,
			Description: "Adds the momentum, consolidation, AVWAP, flush, and reclaim entry path.",
		},
		{
			Key: "risk.entry_gates.pullback_setup", Label: "ENTRY · Pullback Setup",
			Value: fmt.Sprintf("%s mom · %dd · %s sup · %s stop",
				percent(defaultFloat(entry.PullbackMinMomentumReturnPct, 30)),
				defaultInt(entry.PullbackConsolidationSessions, 40),
				percent(defaultFloat(entry.PullbackSupportTolerancePct, 1.5)),
				percent(defaultFloat(entry.PullbackMaxStopWidthPct, 4)),
			),
			Kind: "readonly", Source: configSourceFile,
			Description: "Momentum floor, consolidation window, support tolerance, and stop cap.",
		},
		{
			Key: "risk.entry_gates.news_min_runup_pct", Label: "ENTRY · News Confirmation",
			Value: disabledPercent(entry.NewsMinRunupPct), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Minimum positive tape response for news-driven BUYs.",
		},
		{
			Key: "risk.entry_gates.momentum_max_runup_pct", Label: "ENTRY · Momentum Chase Cap",
			Value: disabledPercent(entry.MomentumMaxRunupPct), Kind: "readonly",
			Source:      configSourceFile,
			Description: "Maximum run-up allowed before a momentum BUY is considered late.",
		},
		{
			Key: "risk.entry_gates.first_tick_defer", Label: "ENTRY · First-Tick Deferral",
			Value:       firstTickLabel(entry.FirstTickDefer, entry.FirstTickHoldTolerancePct),
			Kind:        "readonly",
			Source:      configSourceFile,
			Description: "Requires a BUY to hold through the next engine tick.",
		},
	}
}

// SetConfigValue writes the only supported dashboard override. Other settings
// remain read-only until the engine has a real runtime reload path for them.
func SetConfigValue(ctx context.Context, key, value string) error {
	if key != "engine.mode" {
		return fmt.Errorf("%s is read-only; edit config.yaml and restart the engine", key)
	}
	switch value {
	case "manual", "auto", "paper":
	default:
		return fmt.Errorf("invalid engine mode %q", value)
	}
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

func decimal(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func percent(v float64) string {
	return decimal(v) + "%"
}

func enabled(v bool) string {
	if v {
		return "enabled"
	}
	return "disabled"
}

func disabledInt(v int) string {
	if v == 0 {
		return "disabled"
	}
	return strconv.Itoa(v)
}

func disabledDecimal(v float64) string {
	if v == 0 {
		return "disabled"
	}
	return decimal(v)
}

func disabledPercent(v float64) string {
	if v == 0 {
		return "disabled"
	}
	return percent(v)
}

func pyramidLabel(on bool, minGain float64) string {
	if !on {
		return "disabled"
	}
	return "enabled at " + percent(defaultFloat(minGain, 5)) + " gain"
}

func confidenceTiersLabel(tiers []config.ConfidenceTier) string {
	if len(tiers) == 0 {
		return "disabled"
	}
	parts := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		parts = append(parts, fmt.Sprintf("%s→%s",
			decimal(tier.RRMin), decimal(tier.ConfMin)))
	}
	return strings.Join(parts, " · ")
}

func firstTickLabel(on bool, tolerance float64) string {
	if !on {
		return "disabled"
	}
	return "enabled · " + percent(defaultFloat(tolerance, 1)) + " tolerance"
}

func defaultFloat(v, fallback float64) float64 {
	if v <= 0 {
		return fallback
	}
	return v
}

func defaultInt(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

func defaultText(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func minutesLabel(minutes int) string {
	if minutes <= 0 {
		return "disabled"
	}
	return fmt.Sprintf("%dm", minutes)
}

func secondsLabel(seconds int) string {
	if seconds > 0 && seconds%60 == 0 {
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%ds", seconds)
}

// ─── Rendering ────────────────────────────────────────────────────────────────

var (
	cfgHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	cfgKeyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	cfgValStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	cfgDescStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	cfgSourceStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	cfgErrorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

// RenderConfig renders an accurate, windowed configuration snapshot.
func RenderConfig(data ConfigData, width, height int) string {
	var b strings.Builder

	b.WriteString(cfgHeaderStyle.Render("Configuration Snapshot"))
	b.WriteString("\n")
	b.WriteString(cfgDescStyle.Render(
		"  Resolved file values; restart after file changes. Mode is the only applied DB override.",
	))
	b.WriteString("\n")
	if data.LoadError != "" {
		b.WriteString(cfgErrorStyle.Render("  Warning: " + data.LoadError))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	start, end := configWindow(len(data.Items), data.Selected, height)
	if start > 0 {
		b.WriteString(cfgSourceStyle.Render(fmt.Sprintf("  ↑ %d settings above", start)))
		b.WriteString("\n")
	}
	for i := start; i < end; i++ {
		item := data.Items[i]
		prefix := "  "
		if i == data.Selected {
			prefix = "▸ "
		}

		label := cfgKeyStyle.Render(item.Label)
		value := cfgValStyle.Render(item.Value)
		source := cfgSourceStyle.Render("[" + item.Source + "]")

		hint := ""
		if item.Kind == "select" {
			hint = fmt.Sprintf("  [%s]", strings.Join(item.Options, " | "))
		}
		if width >= 100 {
			label = lipgloss.NewStyle().Width(30).Render(label)
			b.WriteString(fmt.Sprintf("%s%s  %s %s%s\n",
				prefix, label, value, source, hint))
		} else {
			b.WriteString(fmt.Sprintf("%s%s: %s  %s%s\n",
				prefix, label, value, source, hint))
		}
		if i == data.Selected {
			b.WriteString(fmt.Sprintf("    %s\n", cfgDescStyle.Render(item.Description)))
		}
	}
	if end < len(data.Items) {
		b.WriteString(cfgSourceStyle.Render(
			fmt.Sprintf("  ↓ %d settings below", len(data.Items)-end),
		))
		b.WriteString("\n")
	}

	b.WriteString(fmt.Sprintf("\n  j/k navigate · Enter changes Mode only · Updated %s",
		data.LastUpdate.Format("15:04:05")))
	return b.String()
}

func configWindow(total, selected, height int) (int, int) {
	if total == 0 {
		return 0, 0
	}
	if selected < 0 {
		selected = 0
	}
	if selected >= total {
		selected = total - 1
	}
	// Header, selected description, overflow markers, and footer consume up to
	// eight lines. Keep a useful minimum for small terminals.
	visible := max(height-8, 4)
	if visible >= total {
		return 0, total
	}
	start := selected - visible/2
	if start < 0 {
		start = 0
	}
	if start+visible > total {
		start = total - visible
	}
	return start, start + visible
}
