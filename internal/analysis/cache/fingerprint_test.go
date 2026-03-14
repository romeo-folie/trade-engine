package cache_test

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis/cache"
)

func baseInput() cache.FingerprintInput {
	return cache.FingerprintInput{
		Ticker:          "AAPL",
		AssetClass:      "stock",
		Timeframe:       "1d",
		StrategyName:    "trend_following",
		StrategyVersion: "v1",
		ModelProfile:    "balanced",
		Analysts:        []string{"openai/gpt-4o", "google/gemini-1.5-pro"},
		Arbiter:         "anthropic/claude-3-5-sonnet",
		RSI:             52.3,
		EMAGapPct:       1.2,
		MACDSign:        1,
		MACDBucket:      2,
		ADXBucket:       1,
		ATRPctBucket:    1,
		PriceRegimeBucket: 1,
		NewsHash:        "abc123",
		OpenPositionsBucket: 1,
		ExposurePctBucket:   2,
	}
}

func TestFingerprintDeterministic(t *testing.T) {
	in := baseInput()
	f1 := cache.Build(in)
	f2 := cache.Build(in)
	if f1 != f2 {
		t.Errorf("fingerprint not deterministic: %q vs %q", f1, f2)
	}
}

func TestFingerprintRSIBucketingAbsorbsNoise(t *testing.T) {
	in1 := baseInput()
	in2 := baseInput()
	// 51.0 and 52.0 are both in the bucket-50 range:
	//   round(51/5) = round(10.2) = 10 → bucket 50
	//   round(52/5) = round(10.4) = 10 → bucket 50
	in1.RSI = 51.0
	in2.RSI = 52.0
	f1 := cache.Build(in1)
	f2 := cache.Build(in2)
	if f1 != f2 {
		t.Errorf("RSI noise within bucket should not change fingerprint: %q vs %q", f1, f2)
	}
}

func TestFingerprintRSIBucketChangesAcrossBoundary(t *testing.T) {
	in1 := baseInput()
	in2 := baseInput()
	in1.RSI = 47.4 // bucket 45
	in2.RSI = 52.6 // bucket 55
	f1 := cache.Build(in1)
	f2 := cache.Build(in2)
	if f1 == f2 {
		t.Error("RSI values in different buckets should produce different fingerprints")
	}
}

func TestFingerprintEMAGapBucketingAbsorbsNoise(t *testing.T) {
	in1 := baseInput()
	in2 := baseInput()
	// 1.1% and 1.2% both round to bucket 2 (nearest 0.5 step)
	in1.EMAGapPct = 1.1
	in2.EMAGapPct = 1.2
	if cache.Build(in1) != cache.Build(in2) {
		t.Error("EMA gap noise within 0.5% bucket should not bust fingerprint")
	}
}

func TestFingerprintProfileChange(t *testing.T) {
	in1 := baseInput()
	in2 := baseInput()
	in2.ModelProfile = "pro"
	if cache.Build(in1) == cache.Build(in2) {
		t.Error("different model profiles should produce different fingerprints")
	}
}

func TestFingerprintNewsHashChange(t *testing.T) {
	in1 := baseInput()
	in2 := baseInput()
	in2.NewsHash = "different"
	if cache.Build(in1) == cache.Build(in2) {
		t.Error("different news hash should produce different fingerprints")
	}
}

func TestFingerprintStrategyVersionChange(t *testing.T) {
	in1 := baseInput()
	in2 := baseInput()
	in2.StrategyVersion = "v2"
	if cache.Build(in1) == cache.Build(in2) {
		t.Error("prompt template version bump should change fingerprint")
	}
}

func TestFingerprintAnalystOrderIndependent(t *testing.T) {
	// Build sorts analysts internally — same set in different order must produce
	// the same fingerprint so callers don't need to pre-sort.
	in1 := baseInput()
	in2 := baseInput()
	in2.Analysts = []string{"google/gemini-1.5-pro", "openai/gpt-4o"} // reversed
	if cache.Build(in1) != cache.Build(in2) {
		t.Error("same analyst set in different order should produce same fingerprint (Build sorts internally)")
	}
}

func TestFingerprintLength(t *testing.T) {
	f := cache.Build(baseInput())
	if len(f) != 64 {
		t.Errorf("expected 64-char hex SHA-256, got len=%d: %q", len(f), f)
	}
}
