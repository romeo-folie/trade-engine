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

func TestExtractAnalysisJSON_RejectsInvalidAction(t *testing.T) {
	raw := `{"action":"WAIT","confidence":0.5,"size_pct":1.0,"stop_loss":1,"take_profit":2,"reasoning":"x"}`
	if _, err := extractAnalysisJSON(raw); err == nil {
		t.Fatal("expected error for invalid action")
	}
}
