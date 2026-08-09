package disclosures

import "testing"

func TestTickerFromAssetRejectsAccountDescriptors(t *testing.T) {
	for _, asset := range []string{"Example Fund (IRA)", "Index Fund (ETF)", "Diversified Holding (EIF)"} {
		if got := tickerFromAsset(asset); got != "" {
			t.Fatalf("tickerFromAsset(%q) = %q", asset, got)
		}
	}
	if got := tickerFromAsset("Apple Inc. (AAPL)"); got != "AAPL" {
		t.Fatalf("valid ticker = %q", got)
	}
}
