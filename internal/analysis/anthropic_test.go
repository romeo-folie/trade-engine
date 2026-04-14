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
