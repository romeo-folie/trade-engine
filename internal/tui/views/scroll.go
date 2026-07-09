package views

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// scrollHintStyle dims the "more rows" scroll indicator so it reads as chrome,
// not data.
var scrollHintStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

// windowRange returns the [start, end) slice of a list of `total` rows that
// should be visible so the `selected` row stays on screen within at most
// `visible` rows. The window is centered on the selection and clamped to the
// list bounds, so the first and last pages fill the viewport without leaving
// blank space. When visible <= 0 or the whole list already fits, the full
// range is returned.
//
// The result is a pure function of (total, selected, visible): there is no
// persisted scroll offset, so it stays correct across data refreshes and the
// modulo-wraparound the navigation keys use.
func windowRange(total, selected, visible int) (start, end int) {
	if visible <= 0 || total <= visible {
		return 0, total
	}
	if selected < 0 {
		selected = 0
	}
	if selected >= total {
		selected = total - 1
	}
	start = max(selected-visible/2, 0)
	end = start + visible
	if end > total {
		end = total
		start = end - visible
	}
	start = max(start, 0)
	return start, end
}

// tableWindow computes the [start, end) row slice for a single-table view.
// `chrome` is the number of fixed lines the view renders around the rows
// (title, blank, column header, separator, footer, …). When windowing kicks
// in, one extra line is reserved for the scroll indicator. height <= 0 returns
// the full range (no windowing), which the render tests rely on.
func tableWindow(total, selected, height, chrome int) (start, end int) {
	if height <= 0 {
		return 0, total
	}
	visible := max(height-chrome, 1)
	if total > visible {
		// Reserve one line for the scroll indicator.
		visible = max(height-chrome-1, 1)
	}
	return windowRange(total, selected, visible)
}

// scrollIndicator renders a single dim line describing the visible slice of a
// windowed list, e.g. "  rows 13–24 of 42   ↑ 12 above · ↓ 18 below". It
// returns "" when the whole list is on screen.
func scrollIndicator(start, end, total int) string {
	if start <= 0 && end >= total {
		return ""
	}
	var arrows []string
	if start > 0 {
		arrows = append(arrows, fmt.Sprintf("↑ %d above", start))
	}
	if end < total {
		arrows = append(arrows, fmt.Sprintf("↓ %d below", total-end))
	}
	return scrollHintStyle.Render(fmt.Sprintf("  rows %d–%d of %d   %s",
		start+1, end, total, strings.Join(arrows, " · ")))
}
