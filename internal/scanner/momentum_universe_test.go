package scanner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

type fakeFundamentals struct{}

func (fakeFundamentals) FetchCompanyFundamentals(context.Context, string) (data.CompanyFundamentals, error) {
	return data.CompanyFundamentals{Name: "Test", MarketCapitalization: 500}, nil
}

func TestScannerHorizonReturnsAndRanking(t *testing.T) {
	history := make([]data.Candle, 130)
	for i := range history {
		history[i].Close = 50 + float64(i)
	}
	price := history[len(history)-1].Close
	r1 := scannerHorizonReturn(price, history, 21)
	r3 := scannerHorizonReturn(price, history, 63)
	r6 := scannerHorizonReturn(price, history, 126)
	if !(r1 > 0 && r3 > r1 && r6 > r3) {
		t.Fatalf("unexpected horizon returns: %.2f %.2f %.2f", r1, r3, r6)
	}
	strong := SwingCandidate{Return1M: r1, Return3M: r3, Return6M: r6}
	weak := SwingCandidate{Return1M: 1, Return3M: 2, Return6M: 3}
	if maxScannerReturn(strong) < 30 {
		t.Fatalf("strong candidate should clear 30%%: %+v", strong)
	}
	if momentumComposite(strong) <= momentumComposite(weak) {
		t.Fatal("momentum composite did not preserve ranking")
	}
}

func TestQualifyPullbackUniverseKeepsTopQuartile(t *testing.T) {
	bases := map[string]float64{"AAA": 50, "BBB": 75, "CCC": 80, "DDD": 90}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		ticker := parts[len(parts)-2]
		base := bases[ticker]
		bars := make([]map[string]any, 130)
		start := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
		for i := range bars {
			bars[i] = map[string]any{
				"t": start.AddDate(0, 0, i).Format(time.RFC3339),
				"o": base, "h": base + 1, "l": base - 1, "c": base,
				"v": 10_000, "vw": base,
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"bars": bars})
	}))
	defer server.Close()

	scan := &MarketScanner{
		AlpacaClient: data.NewAlpacaClient("key", "secret", server.URL),
		Fundamentals: fakeFundamentals{},
	}
	got := scan.qualifyPullbackUniverse(context.Background(), []SwingCandidate{
		{Ticker: "AAA", Price: 200, Volume: 10_000},
		{Ticker: "BBB", Price: 150, Volume: 10_000},
		{Ticker: "CCC", Price: 120, Volume: 10_000},
		{Ticker: "DDD", Price: 100, Volume: 10_000}, // below 30% on every horizon
	})
	if len(got) != 1 || got[0].Ticker != "AAA" {
		t.Fatalf("top quartile = %+v, want AAA", got)
	}
	if got[0].MarketCapMM != 500 || got[0].DollarVolume != 2_000_000 {
		t.Fatalf("universe metrics missing: %+v", got[0])
	}

	scan.PullbackMinMomentumReturnPct = 400
	if got := scan.qualifyPullbackUniverse(context.Background(), []SwingCandidate{
		{Ticker: "AAA", Price: 200, Volume: 10_000},
	}); len(got) != 0 {
		t.Fatalf("configured 400%% floor ignored: %+v", got)
	}
}

func TestFilterActiveCandidatesDoesNotPromoteLowerQuartiles(t *testing.T) {
	// Universe qualification has already selected AAA as the sole top-quartile
	// name. Removing active AAA must leave the scan empty; BBB must not be
	// promoted from the unselected lower quartiles on the next scan.
	selectedTopQuartile := []SwingCandidate{{Ticker: "AAA"}}
	got := filterActiveCandidates(
		selectedTopQuartile,
		map[string]struct{}{"AAA": {}},
	)
	if len(got) != 0 {
		t.Fatalf("active top quartile should leave no candidates: %+v", got)
	}
}
