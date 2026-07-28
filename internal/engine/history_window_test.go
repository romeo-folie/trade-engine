package engine

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// TestWidestHistoryWindow: with every admissible lens now selectable by the
// analyst, the candle budget must satisfy the most demanding one. Capping at
// the primary's window would hand a momentum-primary row only 14 candles while
// asking it to confirm or reject a trend_following read that needs 20.
func TestWidestHistoryWindow(t *testing.T) {
	windows := map[string]int{
		"trend_following": 20,
		"mean_reversion":  20,
		"momentum":        14,
	}
	cases := []struct {
		name       string
		primary    string
		strategies []string
		want       int
	}{
		{
			name:       "narrow primary widens to the other candidate",
			primary:    "momentum",
			strategies: []string{"momentum", "trend_following"},
			want:       20,
		},
		{
			name:       "single lens keeps its own window",
			primary:    "momentum",
			strategies: []string{"momentum"},
			want:       14,
		},
		{
			name:       "entry-gate tag contributes nothing",
			primary:    "momentum",
			strategies: []string{"momentum", "pullback_strength"},
			want:       14,
		},
		{
			name:       "lens without a configured window is skipped",
			primary:    "news_sentiment",
			strategies: []string{"news_sentiment"},
			want:       0,
		},
		{
			name:       "news_sentiment paired with a windowed lens takes that window",
			primary:    "news_sentiment",
			strategies: []string{"news_sentiment", "mean_reversion"},
			want:       20,
		},
		{
			name:       "empty set falls back to the default lens window",
			primary:    "",
			strategies: nil,
			want:       20, // defaultLens is trend_following
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := widestHistoryWindow(windows, tc.primary, tc.strategies); got != tc.want {
				t.Errorf("widestHistoryWindow(%q, %v) = %d, want %d",
					tc.primary, tc.strategies, got, tc.want)
			}
		})
	}
}

// TestWidestHistoryWindow_DoesNotMutateInput guards the append() in the helper:
// it builds its candidate slice from the caller's watchlist strategies, which
// must not be clobbered — the same slice drives hasEntryStrategy downstream.
func TestWidestHistoryWindow_DoesNotMutateInput(t *testing.T) {
	strategies := []string{"momentum", "pullback_strength"}
	original := append([]string(nil), strategies...)
	_ = widestHistoryWindow(map[string]int{"momentum": 14}, "momentum", strategies)
	for i := range original {
		if strategies[i] != original[i] {
			t.Fatalf("input mutated: %v became %v", original, strategies)
		}
	}
}

// TestEffectiveGateStrategy: the entry-quality run-up gates are keyed by
// strategy, and before the candidate-lens change the configured primary was
// necessarily the lens the analyst reasoned under. Now that a row can admit
// several lenses, keying on the primary would let a news_sentiment BUY on a
// trend_following-primary row skip the tape-confirmation gate entirely.
func TestEffectiveGateStrategy(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		selected   string
		want       string
	}{
		{
			name:       "analyst selection wins over configured primary",
			configured: "trend_following",
			selected:   "news_sentiment",
			want:       "news_sentiment",
		},
		{
			name:       "momentum selected on a non-momentum row still arms anti-chase",
			configured: "mean_reversion",
			selected:   "momentum",
			want:       "momentum",
		},
		{"empty selection falls back", "news_sentiment", "", "news_sentiment"},
		{"regime-mismatch none falls back", "news_sentiment", "none", "news_sentiment"},
		{"unknown label falls back", "news_sentiment", "vibes", "news_sentiment"},
		{"entry-gate tag is not a lens", "momentum", "pullback_strength", "momentum"},
		{"case and space normalized", "momentum", " News_Sentiment ", "news_sentiment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveGateStrategy(tc.configured, tc.selected); got != tc.want {
				t.Errorf("effectiveGateStrategy(%q, %q) = %q, want %q",
					tc.configured, tc.selected, got, tc.want)
			}
		})
	}
}

// TestRunupGates_FireOnSelectedLens is the end-to-end statement of the same
// invariant: a BUY reasoned under news_sentiment must be blocked by the tape
// -confirmation gate even when the watchlist row's primary is trend_following.
func TestRunupGates_FireOnSelectedLens(t *testing.T) {
	cfg := config.EntryGatesConfig{NewsMinRunupPct: 2.0, MomentumMaxRunupPct: 2.0}
	// Price flat vs prior close: the tape never confirmed the headline.
	reason := evaluateRunupGates(
		effectiveGateStrategy("trend_following", "news_sentiment"),
		"BUY", 100.0, 100.0, cfg,
	)
	if reason == "" {
		t.Error("news tape-confirmation gate did not fire for a news_sentiment-selected BUY")
	}
	// Same inputs keyed off the stale primary: the gate would have stayed silent.
	if stale := evaluateRunupGates("trend_following", "BUY", 100.0, 100.0, cfg); stale != "" {
		t.Errorf("expected the stale-primary path to be silent, got %q", stale)
	}
}

// TestLensDivergenceSignal pins the semantics the phase-1 measurement depends
// on (issue #509): "divergent" must be true whenever the analyst reasoned under
// something other than the configured strategy — including "none", the
// regime-mismatch answer, which is the strongest evidence a row is mislabelled.
func TestLensDivergenceSignal(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		selected   string
		want       bool
	}{
		{"agreement is not divergence", "mean_reversion", "mean_reversion", false},
		{"different lens is divergence", "mean_reversion", "trend_following", true},
		{"regime mismatch is divergence", "mean_reversion", "none", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.selected != tc.configured; got != tc.want {
				t.Errorf("divergent(%q, %q) = %v, want %v",
					tc.configured, tc.selected, got, tc.want)
			}
		})
	}
}
