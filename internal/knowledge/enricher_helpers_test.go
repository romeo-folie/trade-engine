package knowledge

import (
	"fmt"
	"strings"
	"testing"
)

// ── sanitizeJSONPayload ───────────────────────────────────────────────────────

func TestSanitizeJSONPayload_Empty(t *testing.T) {
	if got := sanitizeJSONPayload(""); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestSanitizeJSONPayload_PlainJSON(t *testing.T) {
	raw := `[{"from_entity":"A","to_entity":"B"}]`
	got := sanitizeJSONPayload(raw)
	if got != raw {
		t.Errorf("plain JSON should be unchanged, got %q", got)
	}
}

func TestSanitizeJSONPayload_FencedJSON(t *testing.T) {
	raw := "```json\n[{\"from_entity\":\"A\"}]\n```"
	got := sanitizeJSONPayload(raw)
	if got != `[{"from_entity":"A"}]` {
		t.Errorf("expected stripped fence, got %q", got)
	}
}

func TestSanitizeJSONPayload_PlainFence(t *testing.T) {
	raw := "```\n{\"key\":\"val\"}\n```"
	got := sanitizeJSONPayload(raw)
	if got != `{"key":"val"}` {
		t.Errorf("expected stripped plain fence, got %q", got)
	}
}

func TestSanitizeJSONPayload_ProsePrefix(t *testing.T) {
	raw := "Here are the relationships:\n[{\"from_entity\":\"A\"}]"
	got := sanitizeJSONPayload(raw)
	if got != `[{"from_entity":"A"}]` {
		t.Errorf("expected prose stripped, got %q", got)
	}
}

func TestSanitizeJSONPayload_ObjectProsePrefix(t *testing.T) {
	raw := `Sure, here you go: {"from_entity":"A"}`
	got := sanitizeJSONPayload(raw)
	if got != `{"from_entity":"A"}` {
		t.Errorf("expected prose stripped before object, got %q", got)
	}
}

func TestSanitizeJSONPayload_Whitespace(t *testing.T) {
	raw := "  \n  [{}]  \n  "
	got := sanitizeJSONPayload(raw)
	if got != "[{}]" {
		t.Errorf("expected trimmed whitespace, got %q", got)
	}
}

// ── isRateLimitErr ────────────────────────────────────────────────────────────

func TestIsRateLimitErr_Nil(t *testing.T) {
	if isRateLimitErr(nil) {
		t.Error("nil should not be rate limit error")
	}
}

func TestIsRateLimitErr_429(t *testing.T) {
	err := fmt.Errorf("HTTP 429 too many requests")
	if !isRateLimitErr(err) {
		t.Error("HTTP 429 should be detected as rate limit error")
	}
}

func TestIsRateLimitErr_RateLimitText(t *testing.T) {
	err := fmt.Errorf("rate limit exceeded")
	if !isRateLimitErr(err) {
		t.Error("'rate limit' text should be detected")
	}
}

func TestIsRateLimitErr_RateLimitUnderscore(t *testing.T) {
	err := fmt.Errorf("error: rate_limit_exceeded")
	if !isRateLimitErr(err) {
		t.Error("'rate_limit' text should be detected")
	}
}

func TestIsRateLimitErr_NonRateLimit(t *testing.T) {
	err := fmt.Errorf("connection refused")
	if isRateLimitErr(err) {
		t.Error("connection refused should not be rate limit error")
	}
}

// ── normalizeTicker ───────────────────────────────────────────────────────────

func TestNormalizeTicker_UppercaseAndTrim(t *testing.T) {
	cases := []struct{ in, want string }{
		{"aapl", "AAPL"},
		{"  tsla  ", "TSLA"},
		{"goog", "GOOG"},
		{"", ""},
	}
	for _, c := range cases {
		got := normalizeTicker(c.in)
		if got != c.want {
			t.Errorf("normalizeTicker(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── normalizeEntityName ───────────────────────────────────────────────────────

func TestNormalizeEntityName_TrimAndSuffixStrip(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Apple Inc.", "Apple"},
		{"  Google LLC  ", "Google LLC"}, // LLC not in suffix list — passes through
		{"Meta Platforms, Inc.", "Meta Platforms"},
		{"Tesla Corp", "Tesla"},
		{"NVIDIA Corporation", "NVIDIA"},
		{"  Hims &amp; Hers Health, Inc.  ", "Hims & Hers Health"},
	}
	for _, c := range cases {
		got := normalizeEntityName(c.in)
		if got != c.want {
			t.Errorf("normalizeEntityName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeEntityName_NoSuffix(t *testing.T) {
	got := normalizeEntityName("Apple")
	if got != "Apple" {
		t.Errorf("no suffix should not strip anything, got %q", got)
	}
}

// ── buildExtractionPrompt ─────────────────────────────────────────────────────

func TestBuildExtractionPrompt_WithSummary(t *testing.T) {
	_, user := buildExtractionPrompt("Headline text", "Summary text")
	if !strings.Contains(user, "Headline: Headline text") {
		t.Errorf("expected headline in user prompt, got %q", user)
	}
	if !strings.Contains(user, "Summary: Summary text") {
		t.Errorf("expected summary in user prompt, got %q", user)
	}
}

func TestBuildExtractionPrompt_NoSummary(t *testing.T) {
	_, user := buildExtractionPrompt("Only headline", "")
	if !strings.Contains(user, "Headline: Only headline") {
		t.Errorf("expected headline in user prompt, got %q", user)
	}
	if strings.Contains(user, "Summary:") {
		t.Errorf("empty summary should not appear in prompt, got %q", user)
	}
}

func TestBuildExtractionPrompt_SystemNotEmpty(t *testing.T) {
	system, _ := buildExtractionPrompt("H", "S")
	if system == "" {
		t.Error("system prompt should not be empty")
	}
}
