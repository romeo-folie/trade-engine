package analysis

import "testing"

func TestExtractAnalysisJSON_BareJSON(t *testing.T) {
	raw := `{"action":"BUY","confidence":0.82,"size_pct":2.0,"stop_loss":128.5,"take_profit":145.0,"reasoning":"breakout"}`
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.Action != ActionBuy {
		t.Fatalf("action = %q, want BUY", got.Action)
	}
	if got.Confidence != 0.82 {
		t.Fatalf("confidence = %v, want 0.82", got.Confidence)
	}
}

func TestExtractAnalysisJSON_MarkdownFence(t *testing.T) {
	raw := "```json\n{\n  \"action\": \"HOLD\",\n  \"confidence\": 0.22,\n  \"size_pct\": 0,\n  \"stop_loss\": 0,\n  \"take_profit\": 0,\n  \"reasoning\": \"insufficient confirmation\"\n}\n```"
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.Action != ActionHold {
		t.Fatalf("action = %q, want HOLD", got.Action)
	}
}

func TestExtractAnalysisJSON_UnclosedFence(t *testing.T) {
	raw := "```json\n{\n  \"action\": \"BUY\",\n  \"confidence\": 0.65,\n  \"size_pct\": 2.5,\n  \"stop_loss\": 179.00,\n  \"take_profit\": 195.00,\n  \"reasoning\": \"x\"\n}"
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.Action != ActionBuy {
		t.Fatalf("action = %q, want BUY", got.Action)
	}
}

func TestExtractAnalysisJSON_NormalizesLowercaseAction(t *testing.T) {
	raw := `{"action":"hold","confidence":0.5,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"x"}`
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.Action != ActionHold {
		t.Fatalf("action = %q, want HOLD", got.Action)
	}
}

func TestExtractAnalysisJSON_RejectsInvalidAction(t *testing.T) {
	raw := `{"action":"WATCH","confidence":0.5,"size_pct":1.0,"stop_loss":1,"take_profit":2,"reasoning":"x"}`
	if _, err := extractAnalysisJSON(raw); err == nil {
		t.Fatal("expected error for invalid action")
	}
}

func TestExtractAnalysisJSON_AcceptsWaitAction(t *testing.T) {
	raw := `{"action":"WAIT","confidence":0.75,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"real catalyst, gap too extreme for entry"}`
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.Action != ActionWait {
		t.Fatalf("action = %q, want WAIT", got.Action)
	}
}

func TestExtractAnalysisJSON_WaitZerosSizePct(t *testing.T) {
	// Model non-compliance: WAIT with non-zero size_pct — must be sanitised before persistence.
	raw := `{"action":"WAIT","confidence":0.80,"size_pct":3.0,"stop_loss":150.0,"take_profit":160.0,"reasoning":"gap too large"}`
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.SizePct != 0 {
		t.Fatalf("size_pct = %f, want 0 for WAIT action", got.SizePct)
	}
	if got.StopLoss != 0 {
		t.Fatalf("stop_loss = %f, want 0 for WAIT action", got.StopLoss)
	}
	if got.TakeProfit != 0 {
		t.Fatalf("take_profit = %f, want 0 for WAIT action", got.TakeProfit)
	}
}

func TestExtractAnalysisJSON_HoldZerosSizePct(t *testing.T) {
	// Same invariant for HOLD — must never carry non-zero sizing fields.
	raw := `{"action":"HOLD","confidence":0.40,"size_pct":1.5,"stop_loss":100.0,"take_profit":110.0,"reasoning":"no catalyst"}`
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.SizePct != 0 {
		t.Fatalf("size_pct = %f, want 0 for HOLD action", got.SizePct)
	}
}

// ORB mode returns "trade" or "skip" instead of BUY/SELL/HOLD.
// Both must be accepted so the ORB scanner does not log "invalid action" errors.
func TestExtractAnalysisJSON_AcceptsORBTradeAction(t *testing.T) {
	raw := `{"action":"trade","conviction":0.85,"catalyst_type":"news","catalyst_quality":"substantive","reasoning":"FDA catalyst"}`
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.Action != ActionTrade {
		t.Fatalf("action = %q, want TRADE", got.Action)
	}
}

func TestExtractAnalysisJSON_AcceptsORBSkipAction(t *testing.T) {
	raw := `{"action":"skip","conviction":0.2,"catalyst_type":"unknown","catalyst_quality":"none","reasoning":"no catalyst"}`
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.Action != ActionSkip {
		t.Fatalf("action = %q, want SKIP", got.Action)
	}
}

// Swing scanner mode returns "add" or "skip". "add" must be accepted so the
// market scanner does not drop every candidate with an "invalid action" error.
func TestExtractAnalysisJSON_AcceptsScannerAddAction(t *testing.T) {
	raw := `{"action":"add","conviction":0.65,"setup_type":"news_catalyst","strategy":"news_sentiment","timeframes":["1D"],"reasoning":"earnings beat"}`
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.Action != ActionAdd {
		t.Fatalf("action = %q, want ADD", got.Action)
	}
}

// ADD is non-directional; any sizing/stop/target fields must be zeroed, matching
// the invariant already enforced for HOLD and WAIT.
func TestExtractAnalysisJSON_AddZerosSizePct(t *testing.T) {
	raw := `{"action":"add","confidence":0.70,"size_pct":2.0,"stop_loss":150.0,"take_profit":160.0,"reasoning":"watchlist candidate"}`
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.SizePct != 0 || got.StopLoss != 0 || got.TakeProfit != 0 {
		t.Fatalf("ADD must zero size/stop/target, got size=%v stop=%v tp=%v", got.SizePct, got.StopLoss, got.TakeProfit)
	}
}

func TestExtractAnalysisJSON_RecoversTruncatedJSON(t *testing.T) {
	raw := "{\n \"action\": \"buy\",\n \"confidence\": 0.75,\n \"size_pct\": 2.0,\n \"stop_loss\": 100,\n \"take_profit\": 110,\n \"reasoning\": \"truncated"
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.Action != ActionBuy {
		t.Fatalf("action = %q, want BUY", got.Action)
	}
	if got.Confidence != 0.75 {
		t.Fatalf("confidence = %v, want 0.75", got.Confidence)
	}
}

// ── JSON-extraction hardening (gpt-5-nano malformed output) ───────────────────

func TestSanitizeJSONControlChars(t *testing.T) {
	if got := sanitizeJSONControlChars(`{"a":"b"}`); got != `{"a":"b"}` {
		t.Errorf("well-formed JSON should round-trip unchanged, got %q", got)
	}
	// A literal newline INSIDE a string value must be escaped; whitespace outside
	// strings is left alone.
	got := sanitizeJSONControlChars("{\n  \"r\":\"a\nb\"}")
	want := "{\n  \"r\":\"a\\nb\"}"
	if got != want {
		t.Errorf("sanitize = %q, want %q", got, want)
	}
}

func TestExtractAnalysisJSON_LiteralNewlineInString(t *testing.T) {
	// gpt-5-nano emits reasoning with real newlines inside the string value —
	// invalid JSON. Sanitizing recovers the FULL structured response.
	raw := "{\n  \"action\": \"BUY\",\n  \"confidence\": 0.70,\n  \"size_pct\": 2.5,\n  \"stop_loss\": 580.0,\n  \"take_profit\": 644.47,\n  \"reasoning\": \"Signal at 601\nsupport near 580\"\n}"
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.Action != ActionBuy || got.Confidence != 0.70 {
		t.Fatalf("action/confidence = %q/%v, want BUY/0.70", got.Action, got.Confidence)
	}
	if got.StopLoss != 580.0 || got.TakeProfit != 644.47 {
		t.Fatalf("SL/TP = %v/%v, want full parse 580/644.47", got.StopLoss, got.TakeProfit)
	}
}

func TestExtractAnalysisJSON_RecoversMalformed(t *testing.T) {
	// A stray JS-style comment breaks strict parsing and can't be sanitized —
	// the regex fallback must still recover action + confidence (previously this
	// only fired on truncation, so the vote was dropped).
	raw := "{\"action\": \"SELL\", \"confidence\": 0.75, // note\n \"size_pct\": 1.5, \"stop_loss\": 100, \"take_profit\": 90, \"reasoning\": \"bearish\"}"
	got, err := extractAnalysisJSON(raw)
	if err != nil {
		t.Fatalf("extractAnalysisJSON error: %v", err)
	}
	if got.Action != ActionSell || got.Confidence != 0.75 {
		t.Fatalf("action/confidence = %q/%v, want SELL/0.75", got.Action, got.Confidence)
	}
}
