// Package components provides reusable TUI building blocks.
package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Column defines a table column with alignment and width.
type Column struct {
	Header string
	Width  int
	Align  Align // Left (default), Right, Center
}

// Align controls column text alignment.
type Align int

const (
	AlignLeft Align = iota
	AlignRight
	AlignCenter
)

// Table renders a scrollable table with column alignment and row selection.
type Table struct {
	Columns  []Column
	Rows     [][]string
	Selected int
	Offset   int // scroll offset (first visible row)
	Height   int // visible rows (0 = no scroll limit)

	HeaderStyle   lipgloss.Style
	RowStyle      lipgloss.Style
	SelectedStyle lipgloss.Style
	SepStyle      lipgloss.Style
}

// NewTable creates a table with default styles.
func NewTable(cols []Column) *Table {
	return &Table{
		Columns:       cols,
		HeaderStyle:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")),
		RowStyle:      lipgloss.NewStyle(),
		SelectedStyle: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11")),
		SepStyle:      lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
	}
}

// Render outputs the table as a string.
func (t *Table) Render() string {
	var b strings.Builder

	// Header
	var header strings.Builder
	for _, col := range t.Columns {
		header.WriteString(alignText(col.Header, col.Width, col.Align))
		header.WriteString("  ")
	}
	b.WriteString(t.HeaderStyle.Render(header.String()))
	b.WriteString("\n")

	// Separator
	totalWidth := 0
	for _, col := range t.Columns {
		totalWidth += col.Width + 2
	}
	b.WriteString(t.SepStyle.Render(strings.Repeat("─", totalWidth)))
	b.WriteString("\n")

	// Rows with scroll
	start := t.Offset
	end := len(t.Rows)
	if t.Height > 0 && end-start > t.Height {
		end = start + t.Height
	}
	if end > len(t.Rows) {
		end = len(t.Rows)
	}

	for i := start; i < end; i++ {
		row := t.Rows[i]
		var line strings.Builder

		prefix := "  "
		if i == t.Selected {
			prefix = "▸ "
		}
		line.WriteString(prefix)

		for j, col := range t.Columns {
			cell := ""
			if j < len(row) {
				cell = row[j]
			}
			line.WriteString(alignText(cell, col.Width, col.Align))
			line.WriteString("  ")
		}

		style := t.RowStyle
		if i == t.Selected {
			style = t.SelectedStyle
		}
		b.WriteString(style.Render(line.String()))
		b.WriteString("\n")
	}

	// Scroll indicator
	if t.Height > 0 && len(t.Rows) > t.Height {
		b.WriteString(t.SepStyle.Render(fmt.Sprintf("  ↕ %d/%d rows", end-start, len(t.Rows))))
		b.WriteString("\n")
	}

	return b.String()
}

// ScrollTo adjusts the offset so the selected row is visible.
func (t *Table) ScrollTo(selected int) {
	t.Selected = selected
	if t.Height <= 0 {
		return
	}
	if selected < t.Offset {
		t.Offset = selected
	}
	if selected >= t.Offset+t.Height {
		t.Offset = selected - t.Height + 1
	}
}

func alignText(s string, width int, align Align) string {
	if len(s) > width {
		s = s[:width]
	}
	pad := width - len(s)
	if pad <= 0 {
		return s
	}
	switch align {
	case AlignRight:
		return strings.Repeat(" ", pad) + s
	case AlignCenter:
		left := pad / 2
		right := pad - left
		return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
	default:
		return s + strings.Repeat(" ", pad)
	}
}
