package components

import (
	"strings"
	"testing"
)

// ── Table ─────────────────────────────────────────────────────────────────────

func TestTable_Render(t *testing.T) {
	tbl := NewTable([]Column{
		{Header: "NAME", Width: 8},
		{Header: "VALUE", Width: 10, Align: AlignRight},
	})
	tbl.Rows = [][]string{
		{"AAPL", "150.00"},
		{"TSLA", "200.00"},
	}
	tbl.Selected = 1

	out := tbl.Render()
	if !strings.Contains(out, "NAME") {
		t.Fatal("expected header")
	}
	if !strings.Contains(out, "AAPL") || !strings.Contains(out, "TSLA") {
		t.Fatal("expected row data")
	}
	if !strings.Contains(out, "▸") {
		t.Fatal("expected selection marker")
	}
}

func TestTable_ScrollTo(t *testing.T) {
	tbl := NewTable([]Column{{Header: "X", Width: 5}})
	tbl.Height = 3
	for i := 0; i < 10; i++ {
		tbl.Rows = append(tbl.Rows, []string{"row"})
	}

	tbl.ScrollTo(7)
	if tbl.Offset != 5 { // 7 - 3 + 1
		t.Fatalf("offset = %d, want 5", tbl.Offset)
	}

	tbl.ScrollTo(2)
	if tbl.Offset != 2 {
		t.Fatalf("offset = %d, want 2", tbl.Offset)
	}
}

func TestAlignText(t *testing.T) {
	tests := []struct {
		s     string
		width int
		align Align
		want  string
	}{
		{"hi", 5, AlignLeft, "hi   "},
		{"hi", 5, AlignRight, "   hi"},
		{"hi", 5, AlignCenter, " hi  "},
		{"toolong", 4, AlignLeft, "tool"},
	}
	for _, tt := range tests {
		got := alignText(tt.s, tt.width, tt.align)
		if got != tt.want {
			t.Errorf("alignText(%q, %d, %d) = %q, want %q", tt.s, tt.width, tt.align, got, tt.want)
		}
	}
}

// ── Sparkline ─────────────────────────────────────────────────────────────────

func TestSparkline_Basic(t *testing.T) {
	out := Sparkline([]float64{1, 2, 3, 4, 5, 6, 7, 8}, 0)
	if len([]rune(out)) != 8 {
		t.Fatalf("expected 8 chars, got %d", len([]rune(out)))
	}
	runes := []rune(out)
	if runes[0] != '▁' {
		t.Fatalf("first char should be ▁, got %c", runes[0])
	}
	if runes[7] != '█' {
		t.Fatalf("last char should be █, got %c", runes[7])
	}
}

func TestSparkline_Empty(t *testing.T) {
	if out := Sparkline(nil, 0); out != "" {
		t.Fatalf("expected empty, got %q", out)
	}
}

func TestSparkline_Flat(t *testing.T) {
	out := Sparkline([]float64{5, 5, 5}, 0)
	for _, r := range out {
		if r != '▄' {
			t.Fatalf("flat line should be ▄, got %c", r)
		}
	}
}

func TestSparkline_MaxWidth(t *testing.T) {
	out := Sparkline([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 5)
	if len([]rune(out)) != 5 {
		t.Fatalf("expected 5 chars, got %d", len([]rune(out)))
	}
}

// ── StatusBar ─────────────────────────────────────────────────────────────────

func TestRenderStatusBar(t *testing.T) {
	items := []StatusBarItem{
		{Key: "q", Label: "Quit"},
		{Key: "1", Label: "Dashboard"},
	}
	out := RenderStatusBar(items, 80)
	if !strings.Contains(out, "q") || !strings.Contains(out, "Quit") {
		t.Fatal("expected quit shortcut")
	}
	if !strings.Contains(out, "Dashboard") {
		t.Fatal("expected dashboard label")
	}
}
