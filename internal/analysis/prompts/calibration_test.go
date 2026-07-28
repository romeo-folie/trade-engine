package prompts

import (
	"strings"
	"testing"
	"time"
)

// anchorSentinel is a distinctive line from the ConfidenceAnchors block; if it
// is present the whole block is (the const is a single string).
const anchorSentinel = "0.70 = clear edge"

// TestConfidenceAnchors_InAllSystemPrompts is the regression guard for the
// calibration bundle: every system prompt — generalist, momentum, and all four
// specialist roles — must carry the shared confidence anchors.
func TestConfidenceAnchors_InAllSystemPrompts(t *testing.T) {
	if sys := buildSystem([]string{"trend_following"}); !strings.Contains(sys, anchorSentinel) {
		t.Error("generalist system prompt is missing the confidence anchors")
	}
	if sys := buildMomentumSystem([]string{"momentum"}); !strings.Contains(sys, anchorSentinel) {
		t.Error("momentum system prompt is missing the confidence anchors")
	}
	for _, role := range []string{"fundamental", "technical", "sentiment", "risk_manager"} {
		sys, _ := BuildRolePrompt(role, "AAPL", []string{"trend_following"}, "1h", nil, Indicators{}, nil, Portfolio{}, "", nil, "")
		if !strings.Contains(sys, anchorSentinel) {
			t.Errorf("role %q prompt is missing the confidence anchors", role)
		}
	}
}

// TestStrategyGuidance_NewsSentimentOnly asserts the anti-chase block is
// attached to news_sentiment and not leaked into other strategies.
func TestStrategyGuidance_NewsSentimentOnly(t *testing.T) {
	const marker = "STRATEGY GUIDANCE — news_sentiment"
	if sys := buildSystem([]string{"news_sentiment"}); !strings.Contains(sys, marker) {
		t.Error("news_sentiment system prompt is missing its strategy guidance block")
	}
	for _, s := range []string{"trend_following", "mean_reversion", "momentum"} {
		if sys := buildSystem([]string{s}); strings.Contains(sys, marker) {
			t.Errorf("strategy %q unexpectedly contains the news_sentiment guidance block", s)
		}
	}
}

// TestBuildUser_CurrentPriceLine asserts the signal_price definition line: the
// mandatory R:R self-filter computes against signal_price, so the user message
// must define it from the latest close (and omit the line when no candles).
func TestBuildUser_CurrentPriceLine(t *testing.T) {
	candles := []Candle{{Timestamp: time.Now(), Open: 10, High: 11, Low: 9, Close: 10.55, Volume: 1000, VWAP: 10.4}}
	user := buildUser("AAPL", "1d", candles, Indicators{}, nil, Portfolio{}, "", nil, "")
	if !strings.Contains(user, "CURRENT PRICE (latest close): 10.55") {
		t.Error("user message is missing the CURRENT PRICE / signal_price line")
	}
	empty := buildUser("AAPL", "1d", nil, Indicators{}, nil, Portfolio{}, "", nil, "")
	if strings.Contains(empty, "CURRENT PRICE") {
		t.Error("CURRENT PRICE line must be omitted when there are no candles")
	}
}

// TestPrompts_NoLeakedFormatVerbs guards against the substitution bug where
// blocks passed as %s arguments carried double-escaped percents: the specialist
// models literally saw "5%% of portfolio". No rendered prompt may contain "%%"
// or a raw "%.2f" verb.
func TestPrompts_NoLeakedFormatVerbs(t *testing.T) {
	rendered := []struct {
		name string
		text string
	}{
		{"generalist", buildSystem([]string{"news_sentiment"})},
		{"momentum", buildMomentumSystem([]string{"momentum"})},
	}
	for _, role := range []string{"fundamental", "technical", "sentiment", "risk_manager"} {
		sys, _ := BuildRolePrompt(role, "AAPL", []string{"trend_following"}, "1h", nil, Indicators{}, nil, Portfolio{}, "", nil, "")
		rendered = append(rendered, struct {
			name string
			text string
		}{"role:" + role, sys})
	}
	for _, r := range rendered {
		if strings.Contains(r.text, "%%") {
			t.Errorf("%s prompt leaks a double-escaped percent (%%%%)", r.name)
		}
		if strings.Contains(r.text, "%.2f") {
			t.Errorf("%s prompt leaks a raw %%.2f format verb", r.name)
		}
	}
}

// TestPortfolioRules_InAllSystemPrompts: the portfolio-usage rules (SELL only
// what is held, pyramid awareness, correlation, no anchoring on other losses)
// must ride along wherever the anchors do.
func TestPortfolioRules_InAllSystemPrompts(t *testing.T) {
	const sentinel = "USING PORTFOLIO CONTEXT:"
	if sys := buildSystem([]string{"trend_following"}); !strings.Contains(sys, sentinel) {
		t.Error("generalist system prompt is missing the portfolio rules")
	}
	if sys := buildMomentumSystem([]string{"momentum"}); !strings.Contains(sys, sentinel) {
		t.Error("momentum system prompt is missing the portfolio rules")
	}
	for _, role := range []string{"fundamental", "technical", "sentiment", "risk_manager"} {
		sys, _ := BuildRolePrompt(role, "AAPL", []string{"trend_following"}, "1h", nil, Indicators{}, nil, Portfolio{}, "", nil, "")
		if !strings.Contains(sys, sentinel) {
			t.Errorf("role %q prompt is missing the portfolio rules", role)
		}
	}
}

// TestBuildUser_PortfolioRendering_CacheStable asserts the position block only
// contains stable or bucketed figures: exact qty/avg-cost, unrealized P&L
// bucketed to 5 points, cash rounded to $100, no raw CurrentValue, and an
// explicit "+N more" line when the conversion cap dropped positions.
func TestBuildUser_PortfolioRendering_CacheStable(t *testing.T) {
	p := Portfolio{
		CashAvailable: 12345.67,
		OpenPositions: []Position{
			// +7.3% unrealized → bucket ≈+5%
			{Ticker: "HIMS", Side: "long", Quantity: 10, AvgCostBasis: 100, CurrentValue: 1073.10, UnrealizedPL: 73.10},
			// -12.4% unrealized → bucket ≈-10%
			{Ticker: "SOFI", Quantity: 20, AvgCostBasis: 10, CurrentValue: 175.20, UnrealizedPL: -24.80},
		},
		OmittedPositions: 3,
	}
	user := buildUser("HIMS", "1d", nil, Indicators{}, nil, p, "", nil, "")
	for _, want := range []string{
		"Cash available: ≈$12300",
		"HIMS: long 10.0000 @ 100.00  unrealized≈+5%",
		"SOFI: long 20.0000 @ 10.00  unrealized≈-10%", // empty side renders long
		"(+3 more open positions not listed)",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("portfolio block missing %q\ngot:\n%s", want, user)
		}
	}
	for _, forbidden := range []string{"1073.10", "175.20", "73.10", "12345.67"} {
		if strings.Contains(user, forbidden) {
			t.Errorf("portfolio block leaks volatile figure %q (cache churn)", forbidden)
		}
	}
}

// TestBuildUser_CashKnownStates: known $0 renders as fully deployed (not the
// "not provided" marker), unknown renders the marker, and fallback positions
// without mark-to-market omit the P&L bucket (Codex #392).
func TestBuildUser_CashKnownStates(t *testing.T) {
	deployed := buildUser("HIMS", "1d", nil, Indicators{}, nil,
		Portfolio{CashAvailable: 0, CashKnown: true, OpenPositions: []Position{
			{Ticker: "HIMS", Side: "long", Quantity: 10, AvgCostBasis: 40}, // no mark-to-market
		}}, "", nil, "")
	if !strings.Contains(deployed, "≈$0 (fully deployed") {
		t.Error("known $0 must render as fully deployed")
	}
	if strings.Contains(deployed, "not provided") {
		t.Error("known $0 must not render the unknown-cash marker")
	}
	if strings.Contains(deployed, "unrealized≈") {
		t.Error("position without mark-to-market must omit the P&L bucket")
	}
	unknown := buildUser("HIMS", "1d", nil, Indicators{}, nil, Portfolio{}, "", nil, "")
	if !strings.Contains(unknown, "not provided — do not assume zero") {
		t.Error("unknown cash must render the explicit marker")
	}
}
