package main

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
)

// metricsJSON must always produce valid, non-nil JSON — even for an all-winners run whose
// ProfitFactor is +Inf. encoding/json rejects ±Inf/NaN, so a naive marshal returns nil, which
// becomes a NULL that violates metrics JSONB NOT NULL and aborts the whole sweep.
func TestMetricsJSON_HandlesInfProfitFactor(t *testing.T) {
	for _, pf := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		raw := metricsJSON(orbreplay.Metrics{Trades: 10, ExpectancyR: 0.4, ProfitFactor: pf})
		if len(raw) == 0 {
			t.Fatalf("metricsJSON returned nil/empty for ProfitFactor %v", pf)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("metricsJSON produced invalid JSON for %v: %v", pf, err)
		}
		if got["profit_factor"].(float64) != pfSentinel {
			t.Errorf("profit_factor = %v; want sentinel %v", got["profit_factor"], pfSentinel)
		}
	}
}

func TestMetricsJSON_FinitePassesThrough(t *testing.T) {
	raw := metricsJSON(orbreplay.Metrics{Trades: 10, ProfitFactor: 1.42, ExpectancyR: 0.1})
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["profit_factor"].(float64) != 1.42 {
		t.Errorf("profit_factor = %v; want 1.42", got["profit_factor"])
	}
}
