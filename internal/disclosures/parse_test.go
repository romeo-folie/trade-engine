package disclosures

import "testing"

func TestTickerFromAssetRequiresOriginalUppercaseSymbol(t *testing.T) {
	tests := []struct {
		asset string
		want  string
	}{
		{asset: "Apple Inc. (AAPL)", want: "AAPL"},
		{asset: "Example Fund (IRA)", want: ""},
		{asset: "Index Fund (ETF)", want: ""},
		{asset: "Diversified Holding (EIF)", want: ""},
		{asset: "Acme Fund (common)", want: ""},
		{asset: "Acme Fund (MiXeD)", want: ""},
		{asset: "Acme Fund (AAPL) (retirement account)", want: "AAPL"},
	}
	for _, test := range tests {
		if got := tickerFromAsset(test.asset); got != test.want {
			t.Errorf("tickerFromAsset(%q) = %q, want %q", test.asset, got, test.want)
		}
	}
}
