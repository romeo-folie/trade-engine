package views

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestRenderConfig_ShowsItems(t *testing.T) {
	data := ConfigData{
		Items: []ConfigItem{
			{Key: "engine.mode", Label: "ENGINE · Mode", Value: "paper", Kind: "select", Options: []string{"manual", "auto", "paper"}, Description: "Controls signal handling", Source: configSourceOverride},
			{Key: "risk.confidence_threshold", Label: "RISK · Confidence Floor", Value: "0.65", Kind: "readonly", Description: "Min confidence", Source: configSourceFile},
		},
		Selected:   0,
		LastUpdate: time.Now(),
	}
	out := RenderConfig(data, 120, 30)
	if !strings.Contains(out, "ENGINE · Mode") {
		t.Fatal("expected engine mode label")
	}
	if !strings.Contains(out, "paper") {
		t.Fatal("expected paper value")
	}
	if !strings.Contains(out, "manual | auto | paper") {
		t.Fatal("expected mode options")
	}
	// Selected item should show description
	if !strings.Contains(out, "Controls signal handling") {
		t.Fatal("expected description for selected item")
	}
	if !strings.Contains(out, "[DB override]") ||
		!strings.Contains(out, "[resolved]") {
		t.Fatal("expected accurate value sources")
	}
}

func TestCycleMode(t *testing.T) {
	tests := []struct {
		current string
		want    string
	}{
		{"manual", "auto"},
		{"auto", "paper"},
		{"paper", "manual"},
		{"unknown", "manual"},
	}
	for _, tt := range tests {
		got := CycleMode(tt.current)
		if got != tt.want {
			t.Errorf("CycleMode(%q) = %q, want %q", tt.current, got, tt.want)
		}
	}
}

func TestRenderConfig_SelectionMarker(t *testing.T) {
	data := ConfigData{
		Items: []ConfigItem{
			{Label: "A", Value: "1"},
			{Label: "B", Value: "2"},
		},
		Selected:   1,
		LastUpdate: time.Now(),
	}
	out := RenderConfig(data, 120, 30)
	lines := strings.Split(out, "\n")
	foundMarker := false
	for _, line := range lines {
		if strings.Contains(line, "▸") && strings.Contains(line, "B") {
			foundMarker = true
		}
	}
	if !foundMarker {
		t.Fatal("expected selection marker on item B")
	}
}

func TestBuildConfigItemsUsesResolvedValues(t *testing.T) {
	cfg := config.Defaults()
	cfg.Engine.TickIntervalSec = 900
	cfg.Risk.ConfidenceThreshold = 0.71
	cfg.Risk.ConfidenceTiers = []config.ConfidenceTier{
		{RRMin: 2, ConfMin: 0.52},
		{RRMin: 1.4, ConfMin: 0.58},
	}
	cfg.Risk.MinRiskReward = 1.8
	cfg.Risk.SignalExpiryMin = 120
	cfg.Risk.StalePriceMovePct = 2
	cfg.Risk.TrailingStop.Enabled = true
	cfg.Risk.TrailingStop.Rule = "ema9"
	cfg.Risk.PyramidEnabled = true
	cfg.Risk.PyramidMinGainPct = 6
	cfg.Risk.EntryGates.TrendAlignmentEnabled = true
	cfg.Risk.EntryGates.PullbackStrengthEnabled = true
	cfg.Risk.EntryGates.PullbackMinMomentumReturnPct = 30
	cfg.Risk.EntryGates.PullbackConsolidationSessions = 40
	cfg.Risk.EntryGates.PullbackSupportTolerancePct = 1.5
	cfg.Risk.EntryGates.PullbackMaxStopWidthPct = 4
	cfg.Risk.EntryGates.FirstTickDefer = true
	cfg.Risk.EntryGates.FirstTickHoldTolerancePct = 1

	items := buildConfigItems(cfg, "paper", configSourceOverride)
	values := make(map[string]ConfigItem, len(items))
	for _, item := range items {
		values[item.Key] = item
	}

	if got := values["engine.mode"]; got.Value != "paper" ||
		got.Source != configSourceOverride || got.Kind != "select" {
		t.Fatalf("mode item = %+v", got)
	}
	if got := values["engine.tick_interval_sec"].Value; got != "15m" {
		t.Fatalf("tick interval = %q, want 15m", got)
	}
	if got := values["risk.confidence_threshold"].Value; got != "0.71" {
		t.Fatalf("confidence = %q, want 0.71", got)
	}
	if got := values["risk.confidence_tiers"].Value; got != "2→0.52 · 1.4→0.58" {
		t.Fatalf("confidence tiers = %q", got)
	}
	if got := values["risk.signal_safety"].Value; got != "120m expiry · 2% stale move" {
		t.Fatalf("signal safety = %q", got)
	}
	if got := values["risk.trailing_stop.rule"].Value; got != "ema9" {
		t.Fatalf("trail rule = %q, want ema9", got)
	}
	if got := values["risk.pyramid_enabled"].Value; got != "enabled at 6% gain" {
		t.Fatalf("pyramiding = %q", got)
	}
	if got := values["risk.entry_gates.trend_alignment_enabled"].Value; got != "enabled" {
		t.Fatalf("trend alignment gate = %q, want enabled", got)
	}
	if got := values["risk.entry_gates.pullback_strength_enabled"].Value; got != "enabled" {
		t.Fatalf("pullback gate = %q, want enabled", got)
	}
	if got := values["risk.entry_gates.pullback_setup"].Value; got != "30% mom · 40d · 1.5% sup · 4% stop" {
		t.Fatalf("pullback setup = %q", got)
	}
	if got := values["risk.entry_gates.first_tick_defer"].Value; got != "enabled · 1% tolerance" {
		t.Fatalf("first-tick deferral = %q", got)
	}
}

func TestBuildConfigItemsShowsEffectivePullbackDefaults(t *testing.T) {
	cfg := config.Defaults()
	cfg.Risk.EntryGates.PullbackMinMomentumReturnPct = 0
	cfg.Risk.EntryGates.PullbackConsolidationSessions = 0
	cfg.Risk.EntryGates.PullbackSupportTolerancePct = 0
	cfg.Risk.EntryGates.PullbackMaxStopWidthPct = 0

	items := buildConfigItems(cfg, "paper", configSourceFile)
	for _, item := range items {
		if item.Key == "risk.entry_gates.pullback_setup" {
			want := "30% mom · 40d · 1.5% sup · 4% stop"
			if item.Value != want {
				t.Fatalf("effective pullback defaults = %q, want %q", item.Value, want)
			}
			return
		}
	}
	t.Fatal("pullback setup item missing")
}

func TestRenderConfigWindowsAroundSelection(t *testing.T) {
	items := make([]ConfigItem, 20)
	for i := range items {
		items[i] = ConfigItem{
			Label: "Setting " + decimal(float64(i)),
			Value: decimal(float64(i)), Source: configSourceFile,
		}
	}
	data := ConfigData{Items: items, Selected: 15, LastUpdate: time.Now()}
	out := RenderConfig(data, 100, 12)
	if !strings.Contains(out, "Setting 15") || !strings.Contains(out, "settings above") ||
		!strings.Contains(out, "settings below") {
		t.Fatalf("windowed config did not keep selection and overflow markers:\n%s", out)
	}
}

func TestSetConfigValueRejectsUnsupportedRuntimeOverride(t *testing.T) {
	err := SetConfigValue(context.Background(), "risk.max_position_pct", "9")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("unsupported override error = %v", err)
	}
}
