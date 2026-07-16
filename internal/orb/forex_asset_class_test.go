package orb

import "testing"

func TestForexAssetClass(t *testing.T) {
	cases := map[string]string{
		"XAU/USD": "metal",
		"XAG/USD": "metal",
		"XPT/USD": "metal",
		"XPD/USD": "metal",
		"xau/usd": "metal", // case-insensitive
		"EUR/USD": "forex",
		"GBP/JPY": "forex",
		"USD/CAD": "forex",
	}
	for sym, want := range cases {
		if got := forexAssetClass(sym); got != want {
			t.Errorf("forexAssetClass(%q) = %q, want %q", sym, got, want)
		}
	}
}
