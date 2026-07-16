package views

import (
	"strings"
	"testing"
)

func TestCandidateAssetClassStoredWins(t *testing.T) {
	// A stored asset class always takes precedence over any derivation.
	if got := candidateAssetClass("option", "newyork", "XAU/USD", "whatever"); got != "option" {
		t.Fatalf("stored class should win, got %q", got)
	}
}

func TestCandidateAssetClassDerivation(t *testing.T) {
	cases := []struct {
		name        string
		sessionName string
		ticker      string
		reasoning   string
		want        string
	}{
		{"options force-watch sentinel", "", "SPY", "options-router force-watch", "option"},
		{"equity gapper", "", "AAPL", "strong catalyst", "equity"},
		{"futures session", "futures", "MNQ", "Futures ORB", "futures"},
		{"forex session metal (gold)", "newyork", "XAU/USD", "Forex ORB", "metal"},
		{"forex session metal (silver)", "newyork", "XAG/USD", "Forex ORB", "metal"},
		{"forex session FX pair", "london", "EUR/USD", "Forex ORB", "forex"},
		{"both-session FX pair", "both", "GBP/USD", "Forex ORB", "forex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidateAssetClass("", tc.sessionName, tc.ticker, tc.reasoning); got != tc.want {
				t.Fatalf("candidateAssetClass(%q,%q,%q) = %q, want %q",
					tc.sessionName, tc.ticker, tc.reasoning, got, tc.want)
			}
		})
	}
}

func TestSessionDisplayLabel(t *testing.T) {
	metals := []ORBCandidateRow{
		{Ticker: "XAU/USD", AssetClass: "metal"},
		{Ticker: "XAG/USD", AssetClass: "metal"},
	}
	mixed := []ORBCandidateRow{
		{Ticker: "XAU/USD", AssetClass: "metal"},
		{Ticker: "EUR/USD", AssetClass: "forex"},
	}
	optionsOnly := []ORBCandidateRow{
		{Ticker: "SPY", Action: "trade", AssetClass: "option"},
		{Ticker: "IWM", Action: "trade", AssetClass: "option"},
	}
	// An options panel that also has a real equity gapper that cleared the gate,
	// plus skipped equity gappers that must NOT affect the label.
	equityAndOptions := []ORBCandidateRow{
		{Ticker: "SPY", Action: "trade", AssetClass: "option"},
		{Ticker: "AAPL", Action: "trade", AssetClass: "equity"},
		{Ticker: "TSLA", Action: "skip", AssetClass: "equity"},
	}
	skippedEquitiesOnly := []ORBCandidateRow{
		{Ticker: "AAPL", Action: "skip", AssetClass: "equity"},
		{Ticker: "TSLA", Action: "skip", AssetClass: "equity"},
	}
	cases := []struct {
		name       string
		session    string
		candidates []ORBCandidateRow
		want       string
	}{
		{"equities empty name", "", nil, "equities"},
		{"equities options-only → options", "", optionsOnly, "options"},
		{"equities mixed → equities · options", "", equityAndOptions, "equities · options"},
		{"equities all skipped → equities", "", skippedEquitiesOnly, "equities"},
		{"futures", "futures", nil, "futures"},
		{"newyork all metals", "newyork", metals, "metals · newyork"},
		{"london all metals", "london", metals, "metals · london"},
		{"newyork mixed → forex", "newyork", mixed, "forex · newyork"},
		{"newyork no candidates → forex", "newyork", nil, "forex · newyork"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionDisplayLabel(tc.session, tc.candidates); got != tc.want {
				t.Fatalf("sessionDisplayLabel(%q) = %q, want %q", tc.session, got, tc.want)
			}
		})
	}
}

func TestAssetBadge(t *testing.T) {
	// Every non-empty class produces a bracketed tag; width is fixed for alignment.
	for class, want := range map[string]string{
		"option":  "[opt]",
		"metal":   "[metal]",
		"forex":   "[fx]",
		"futures": "[fut]",
		"equity":  "[eq]",
	} {
		got := assetBadge(class)
		if !strings.HasPrefix(got, want) {
			t.Errorf("assetBadge(%q) = %q, want prefix %q", class, got, want)
		}
		if len(got) != 7 {
			t.Errorf("assetBadge(%q) width = %d, want 7 (column alignment)", class, len(got))
		}
	}
	// Unknown/empty class → blank but still padded.
	if got := assetBadge(""); strings.TrimSpace(got) != "" {
		t.Errorf("assetBadge(\"\") = %q, want blank", got)
	}
}

func TestRenderORBPanelShowsAssetBadge(t *testing.T) {
	data := ORBSessionData{
		SessionName: "metals · newyork",
		Status:      "done",
		Selected:    []string{"XAU/USD"},
		Candidates: []ORBCandidateRow{
			{Ticker: "XAU/USD", Action: "trade", Conviction: 1.0, AssetClass: "metal", Reasoning: "Forex ORB"},
		},
	}
	out := RenderORBPanel(data, 120)
	if !strings.Contains(out, "[metal]") {
		t.Fatalf("panel should badge the metal candidate, got:\n%s", out)
	}
	if !strings.Contains(out, "metals · newyork") {
		t.Fatalf("panel header should show the metals label, got:\n%s", out)
	}
}
