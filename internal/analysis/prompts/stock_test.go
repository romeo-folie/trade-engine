package prompts

import (
	"strings"
	"testing"
	"time"
)

func TestBuildStockPrompt_IncludesRequiredSections(t *testing.T) {
	sys, user := BuildStockPrompt(
		"AAPL",
		"momentum",
		"1d",
		[]Candle{{Timestamp: time.Now().UTC(), Open: 100, High: 101, Low: 99, Close: 100.5, Volume: 12345, VWAP: 100.2}},
		Indicators{RSI: 55.2, EMA20: 99.1, VolSMA20: 12000, VolRatio: 1.2},
		[]NewsItem{{Headline: "Apple launches new product", Source: "Reuters", PublishedAt: time.Now().Add(-2 * time.Hour), Sentiment: "bullish"}},
		Portfolio{},
		"",    // knowledgeContext — empty for unit test
		nil,   // similarSetups — empty for unit test
		"",    // performanceContext — empty for unit test
		false, // momentumPlay — standard prompt for this test
	)

	checks := []string{
		"ONLY a valid JSON object",
		"RECENT OHLCV",
		"TECHNICAL INDICATORS",
		"PORTFOLIO CONTEXT",
		"RECENT NEWS (last 48h)",
		"KNOWLEDGE GRAPH",
		"SIMILAR PAST ANALYSES",
	}
	for _, c := range checks {
		if !strings.Contains(sys+"\n"+user, c) {
			t.Fatalf("prompt missing section: %q", c)
		}
	}
}
