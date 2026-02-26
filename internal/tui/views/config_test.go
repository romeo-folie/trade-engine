package views

import (
	"strings"
	"testing"
	"time"
)

func TestRenderConfig_ShowsItems(t *testing.T) {
	data := ConfigData{
		Items: []ConfigItem{
			{Key: "engine.mode", Label: "Trading Mode", Value: "paper", Kind: "select", Options: []string{"manual", "auto", "paper"}, Description: "Controls signal handling"},
			{Key: "risk.confidence_threshold", Label: "Min Confidence", Value: "0.65", Kind: "number", Description: "Min confidence"},
		},
		Selected:   0,
		LastUpdate: time.Now(),
	}
	out := RenderConfig(data, 120)
	if !strings.Contains(out, "Trading Mode") {
		t.Fatal("expected Trading Mode label")
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
	out := RenderConfig(data, 120)
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
