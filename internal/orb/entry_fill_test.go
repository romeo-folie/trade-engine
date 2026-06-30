package orb

import "testing"

func TestSanitizeEntryFill(t *testing.T) {
	cases := []struct {
		name      string
		side      string
		entryStop float64
		reported  float64
		want      float64
		corrected bool
	}{
		// The real XAU bug: a buy "filled" below its buy-stop, a sell above its sell-stop.
		{"buy filled below stop (bug)", "buy", 4738.16, 4723.73, 4738.16, true},
		{"sell filled above stop (bug)", "sell", 4724.27, 4739.00, 4724.27, true},

		// Valid fills: buy at/above stop, sell at/below stop (slippage worsens, never improves).
		{"buy at stop", "buy", 4812.47, 4812.80, 4812.80, false},
		{"sell at stop", "sell", 1.3000, 1.2990, 1.2990, false},
		{"buy exactly at stop", "buy", 100.0, 100.0, 100.0, false},

		// Tiny rounding within tolerance is accepted, not corrected.
		{"buy a hair below stop within tol", "buy", 1000.0, 999.95, 999.95, false},

		// Guards: unset values pass through unchanged.
		{"zero entry stop", "buy", 0, 1.23, 1.23, false},
		{"zero reported", "sell", 1.23, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, corrected := sanitizeEntryFill(tc.side, tc.entryStop, tc.reported)
			if got != tc.want || corrected != tc.corrected {
				t.Errorf("sanitizeEntryFill(%s, %v, %v) = (%v, %v); want (%v, %v)",
					tc.side, tc.entryStop, tc.reported, got, corrected, tc.want, tc.corrected)
			}
		})
	}
}
