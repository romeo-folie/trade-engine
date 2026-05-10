package engine

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestCheckMaterialChange(t *testing.T) {
	now := time.Now()
	cfg := config.AnalysisConfig{
		MaterialChangeOnly:    true,
		HoldCooldownMin:       120,
		RSIDeltaThreshold:     3.0,
		EMAGapDeltaPct:        0.5,
		PriceMoveThresholdPct: 0.75,
		NewsNoveltyEnabled:    true,
	}

	tests := []struct {
		name       string
		prev       *TickerState
		in         MaterialChangeInput
		wantSkip   bool
		wantReason SkipReason
	}{
		{
			name: "First run always analyzes",
			prev: nil,
			in: MaterialChangeInput{
				RSI: 50, EMA20: 100, Price: 105, NewsHash: "abc", Now: now,
			},
			wantSkip:   false,
			wantReason: SkipReasonNone,
		},
		{
			name: "HOLD cooldown active",
			prev: &TickerState{
				LastDecision:   "HOLD",
				LastAnalysedAt: now.Add(-60 * time.Minute),
			},
			in: MaterialChangeInput{
				Now: now,
			},
			wantSkip:   true,
			wantReason: SkipReasonHoldCooldown,
		},
		{
			name: "HOLD cooldown expired",
			prev: &TickerState{
				LastDecision:   "HOLD",
				LastAnalysedAt: now.Add(-130 * time.Minute),
				LastRSI:        50,
				LastEMAGapPct:  5.0,
				LastPrice:      105,
				LastNewsHash:   "abc",
			},
			in: MaterialChangeInput{
				RSI: 50, EMA20: 100, Price: 105, NewsHash: "abc", Now: now,
			},
			wantSkip:   true, // Still skips if no material change and not first run
			wantReason: SkipReasonNoChange,
		},
		{
			name: "No material change",
			prev: &TickerState{
				LastRSI:       50,
				LastEMAGapPct: 5.0,
				LastPrice:     105,
				LastNewsHash:  "abc",
			},
			in: MaterialChangeInput{
				RSI: 51, EMA20: 100, Price: 105.4, NewsHash: "abc", Now: now,
			},
			wantSkip:   true,
			wantReason: SkipReasonNoChange,
		},
		{
			name: "RSI material change",
			prev: &TickerState{
				LastRSI:       50,
				LastEMAGapPct: 5.0,
				LastPrice:     105,
				LastNewsHash:  "abc",
			},
			in: MaterialChangeInput{
				RSI: 54, EMA20: 100, Price: 105, NewsHash: "abc", Now: now,
			},
			wantSkip:   false,
			wantReason: SkipReasonNone,
		},
		{
			name: "Price move material change",
			prev: &TickerState{
				LastRSI:       50,
				LastEMAGapPct: 5.0,
				LastPrice:     100,
				LastNewsHash:  "abc",
			},
			in: MaterialChangeInput{
				RSI: 50, EMA20: 100, Price: 101, NewsHash: "abc", Now: now,
			},
			wantSkip:   false,
			wantReason: SkipReasonNone,
		},
		{
			name: "News hash change",
			prev: &TickerState{
				LastRSI:       50,
				LastEMAGapPct: 5.0,
				LastPrice:     100,
				LastNewsHash:  "abc",
			},
			in: MaterialChangeInput{
				RSI: 50, EMA20: 100, Price: 100, NewsHash: "def", Now: now,
			},
			wantSkip:   false,
			wantReason: SkipReasonNone,
		},
		{
			name: "Disabled gate never skips",
			prev: &TickerState{
				LastRSI:       50,
				LastEMAGapPct: 5.0,
				LastPrice:     105,
				LastNewsHash:  "abc",
			},
			in: MaterialChangeInput{
				RSI: 50, EMA20: 100, Price: 105, NewsHash: "abc", Now: now,
			},
			wantSkip:   false,
			wantReason: SkipReasonNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testCfg := cfg
			if tt.name == "Disabled gate never skips" {
				testCfg.MaterialChangeOnly = false
			}
			skip, reason, _ := CheckMaterialChange(tt.prev, tt.in, testCfg)
			if skip != tt.wantSkip {
				t.Errorf("skip = %v, want %v", skip, tt.wantSkip)
			}
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

// ── Scanner overrides (#317) ──────────────────────────────────────────────────

func TestApplyScannerOverrides_NonScannerUntouched(t *testing.T) {
	base := config.AnalysisConfig{
		HoldCooldownMin:   120,
		RSIDeltaThreshold: 3.0,
	}
	out := applyScannerOverrides(base, "manual", 0.95)
	if out.HoldCooldownMin != 120 || out.RSIDeltaThreshold != 3.0 {
		t.Fatalf("manual source must be untouched, got %+v", out)
	}
}

func TestApplyScannerOverrides_ScannerLowConvictionOnlyCooldown(t *testing.T) {
	base := config.AnalysisConfig{
		HoldCooldownMin:   120,
		RSIDeltaThreshold: 3.0,
	}
	out := applyScannerOverrides(base, "scanner", 0.60)
	// Cooldown still drops (this isn't gated by conviction).
	if out.HoldCooldownMin != 30 {
		t.Fatalf("cooldown should drop to 30, got %d", out.HoldCooldownMin)
	}
	// RSI threshold must NOT drop for conviction below the 0.75 floor.
	if out.RSIDeltaThreshold != 3.0 {
		t.Fatalf("low-conviction scanner must keep RSI threshold at 3.0, got %.1f", out.RSIDeltaThreshold)
	}
}

func TestApplyScannerOverrides_ScannerHighConvictionLoosensRSI(t *testing.T) {
	base := config.AnalysisConfig{
		HoldCooldownMin:   120,
		RSIDeltaThreshold: 3.0,
	}
	out := applyScannerOverrides(base, "scanner", 0.80)
	if out.RSIDeltaThreshold != 2.0 {
		t.Fatalf("high-conviction scanner must drop RSI threshold to 2.0, got %.1f", out.RSIDeltaThreshold)
	}
}

// If a config already sets RSI threshold below the 2.0 floor, the override
// must not raise it. The override is a one-way relaxation.
func TestApplyScannerOverrides_DoesNotRaiseRSIThreshold(t *testing.T) {
	base := config.AnalysisConfig{
		HoldCooldownMin:   120,
		RSIDeltaThreshold: 1.5,
	}
	out := applyScannerOverrides(base, "scanner", 0.90)
	if out.RSIDeltaThreshold != 1.5 {
		t.Fatalf("override must not raise RSI threshold, got %.1f", out.RSIDeltaThreshold)
	}
}

func TestNewsDigestHash(t *testing.T) {
	h1 := NewsDigestHash([]string{"A", "B"})
	h2 := NewsDigestHash([]string{"A", "B"})
	h3 := NewsDigestHash([]string{"B", "A"}) // same set, different order
	h4 := NewsDigestHash([]string{"ab", "c"})
	h5 := NewsDigestHash([]string{"a", "bc"}) // different split, same bytes without delimiter

	// Identical input → identical hash.
	if h1 != h2 {
		t.Errorf("identical headlines produced different hashes")
	}
	// Order-independent: ["A","B"] == ["B","A"].
	if h1 != h3 {
		t.Errorf("same headline set in different order should produce same hash (sort-stable)")
	}
	// NUL delimiter prevents hash collisions between different splits.
	if h4 == h5 {
		t.Errorf("different headline splits should produce different hashes (NUL delimiter)")
	}
	// Empty list should not panic.
	_ = NewsDigestHash(nil)
	_ = NewsDigestHash([]string{})
}
