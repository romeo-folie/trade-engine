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
	if sys := buildSystem("trend_following"); !strings.Contains(sys, anchorSentinel) {
		t.Error("generalist system prompt is missing the confidence anchors")
	}
	if sys := buildMomentumSystem("momentum"); !strings.Contains(sys, anchorSentinel) {
		t.Error("momentum system prompt is missing the confidence anchors")
	}
	for _, role := range []string{"fundamental", "technical", "sentiment", "risk_manager"} {
		sys, _ := BuildRolePrompt(role, "AAPL", "swing", "1h", nil, Indicators{}, nil, Portfolio{}, "", nil, "")
		if !strings.Contains(sys, anchorSentinel) {
			t.Errorf("role %q prompt is missing the confidence anchors", role)
		}
	}
}

// TestStrategyGuidance_NewsSentimentOnly asserts the anti-chase block is
// attached to news_sentiment and not leaked into other strategies.
func TestStrategyGuidance_NewsSentimentOnly(t *testing.T) {
	const marker = "STRATEGY GUIDANCE — news_sentiment"
	if sys := buildSystem("news_sentiment"); !strings.Contains(sys, marker) {
		t.Error("news_sentiment system prompt is missing its strategy guidance block")
	}
	for _, s := range []string{"trend_following", "mean_reversion", "momentum"} {
		if sys := buildSystem(s); strings.Contains(sys, marker) {
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
		{"generalist", buildSystem("news_sentiment")},
		{"momentum", buildMomentumSystem("momentum")},
	}
	for _, role := range []string{"fundamental", "technical", "sentiment", "risk_manager"} {
		sys, _ := BuildRolePrompt(role, "AAPL", "swing", "1h", nil, Indicators{}, nil, Portfolio{}, "", nil, "")
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
