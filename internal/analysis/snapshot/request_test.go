package snapshot

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestFrozenInputsRequestRoundTrip(t *testing.T) {
	// Request reconstruction must preserve decision identity and context.
	in := Inputs{Ticker: "TEST", Timeframe: "1d", Strategy: "momentum", Strategies: []string{"momentum"}, DecisionCutoff: time.Now().UTC(), KnowledgeContext: "knowledge", PerformanceContext: "performance", MomentumPlay: true}
	if err := json.Unmarshal([]byte(`{"risk_policy":{"Configured":true},"timeframe_role":"primary_thesis","recent_candles":[{"Close":100}],"indicators":{"ATR":2},"timeframe_contexts":[{"Role":"entry_timing","Timeframe":"1h"}],"news":[{"Headline":"test"}],"portfolio":{"Cash":100},"similar_analyses":[{"Ticker":"TEST"}],"discovery_context":{"version":1},"execution_profile":{"version":1},"pullback_context":{},"primary_thesis_context":{}}`), &in); err != nil {
		t.Fatal(err)
	}
	got := InputsFromRequest(in.Request())
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("round trip: want %+v got %+v", in, got)
	}
}
