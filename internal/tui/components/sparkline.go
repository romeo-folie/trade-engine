package components

import "strings"

// Sparkline renders an ASCII sparkline from a series of float64 values.
// Uses Unicode block characters for 8 vertical levels.
var sparkBlocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// Sparkline generates a sparkline string from the given values.
// maxWidth limits the number of data points shown (0 = no limit).
func Sparkline(values []float64, maxWidth int) string {
	if len(values) == 0 {
		return ""
	}

	// Trim to maxWidth (take most recent).
	if maxWidth > 0 && len(values) > maxWidth {
		values = values[len(values)-maxWidth:]
	}

	// Find min/max.
	minVal, maxVal := values[0], values[0]
	for _, v := range values {
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
	}

	span := maxVal - minVal
	if span == 0 {
		// Flat line — render all as mid-level.
		return strings.Repeat(string(sparkBlocks[3]), len(values))
	}

	var b strings.Builder
	for _, v := range values {
		normalized := (v - minVal) / span // 0.0–1.0
		idx := int(normalized * float64(len(sparkBlocks)-1))
		if idx >= len(sparkBlocks) {
			idx = len(sparkBlocks) - 1
		}
		b.WriteRune(sparkBlocks[idx])
	}
	return b.String()
}
