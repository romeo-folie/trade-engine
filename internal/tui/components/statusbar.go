package components

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// StatusBarItem is a single key-label pair in the status bar.
type StatusBarItem struct {
	Key   string // e.g. "q"
	Label string // e.g. "Quit"
}

var (
	barStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("236")).
			Foreground(lipgloss.Color("252")).
			Padding(0, 1)
	keyStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("11"))
)

// RenderStatusBar renders a bottom status bar with keyboard shortcuts.
func RenderStatusBar(items []StatusBarItem, width int) string {
	var parts []string
	for _, item := range items {
		parts = append(parts, keyStyle.Render(item.Key)+" "+item.Label)
	}
	content := strings.Join(parts, "  │  ")
	// Pad to full width.
	if len(content) < width {
		content += strings.Repeat(" ", width-len(content))
	}
	return barStyle.Width(width).Render(content)
}
