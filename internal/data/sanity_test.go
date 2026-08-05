package data

import (
	"math"
	"testing"
	"time"
)

// series builds a well-behaved run of bars with a constant range, so tests can
// establish a known median scale and then append the bar under test.
func series(ticker string, tf Timeframe, n int, price, rng float64) []Candle {
	out := make([]Candle, 0, n)
	base := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	for i := range n {
		out = append(out, Candle{
			Ticker: ticker, Timeframe: tf,
			Timestamp: base.Add(time.Duration(i) * time.Hour),
			Open:      price, High: price + rng/2, Low: price - rng/2, Close: price,
			Volume: 1000,
		})
	}
	return out
}

func at(c []Candle, i int) time.Time { return c[len(c)-1].Timestamp.Add(time.Duration(i) * time.Hour) }

// ── The bar that motivated the guard ─────────────────────────────────────────

// Replays the real corrupt bar from issue #519: Alpaca served ANET 1h at
// 2026-08-04T20:00Z with high 224.98 / close 211.47 while the session's daily
// high was 194.30 and the last trade was 190.53.
func TestSanitize_RejectsRealANETCorruptBar(t *testing.T) {
	// ~190 with a ~2.0 median hourly range, matching the observed tape.
	bars := series("ANET", Timeframe1Hour, 25, 190.34, 2.0)
	bars = append(bars, Candle{
		Ticker: "ANET", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
		Open: 190.40, High: 224.98, Low: 190.20, Close: 211.47, Volume: 5000,
	})

	kept, rejected := SanitizeCandlesWith(bars, DefaultCandleSanity())
	if len(rejected) != 1 {
		t.Fatalf("expected the corrupt bar rejected, got %d rejections", len(rejected))
	}
	if rejected[0].Reason != "implausible_range" {
		t.Errorf("reason = %q, want implausible_range", rejected[0].Reason)
	}
	if rejected[0].Candle.High != 224.98 {
		t.Errorf("wrong bar rejected: high %.2f", rejected[0].Candle.High)
	}
	if len(kept) != len(bars)-1 {
		t.Errorf("kept %d bars, want %d", len(kept), len(bars)-1)
	}
}

// ── The conjunction: neither axis alone may reject ───────────────────────────

// The decisive case. A real overnight gap moves further from the prior close
// than the corrupt bar did (VOYG +30.6% on 2026-08-04 vs the corrupt bar's
// 18.2%), so any distance-from-prior-close rule would discard it. Measured by
// its own width the gap is unremarkable, because a gap shifts the level without
// widening the bar.
func TestSanitize_KeepsLargeOvernightGap(t *testing.T) {
	// Median range 1.0 at ~21, then the session opens 30% higher and trades a
	// normal-width bar up there: excursion ~6.4 from the prior close (30%),
	// own span only 1.6 (1.6x).
	bars := series("VOYG", Timeframe1Hour, 25, 21.00, 1.0)
	bars = append(bars, Candle{
		Ticker: "VOYG", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
		Open: 27.00, High: 27.40, Low: 25.80, Close: 27.20, Volume: 900,
	})
	if _, rejected := SanitizeCandlesWith(bars, DefaultCandleSanity()); len(rejected) != 0 {
		t.Fatalf("a real overnight gap must survive, got %v", rejected[0])
	}
}

// A wide bar in already-volatile tape is normal: the ratio stays low because
// the reference window is itself wide.
func TestSanitize_KeepsWideBarInVolatileTape(t *testing.T) {
	bars := series("CRDO", Timeframe1Hour, 25, 100.00, 8.0)
	bars = append(bars, Candle{
		Ticker: "CRDO", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
		Open: 100.00, High: 112.30, Low: 99.50, Close: 111.80, Volume: 900,
	})
	if _, rejected := SanitizeCandlesWith(bars, DefaultCandleSanity()); len(rejected) != 0 {
		t.Fatalf("wide bar in volatile tape must be kept, got %v", rejected[0])
	}
}

// A quiet reference window makes almost any bar a large multiple of the median
// range; the percentage floor is what stops that becoming a rejection.
func TestSanitize_KeepsHighRatioTinyAbsoluteMove(t *testing.T) {
	// 0.02 median range at ~61: a 0.45 span is 22x the median but only 0.7%.
	bars := series("IBKR", Timeframe1Hour, 25, 61.00, 0.02)
	bars = append(bars, Candle{
		Ticker: "IBKR", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
		Open: 61.00, High: 61.30, Low: 60.85, Close: 61.20, Volume: 900,
	})
	if _, rejected := SanitizeCandlesWith(bars, DefaultCandleSanity()); len(rejected) != 0 {
		t.Fatalf("tiny absolute move must be kept regardless of ratio, got %v", rejected[0])
	}
}

// Daily bars are exempt from the range gate: their distribution is too
// heavy-tailed to threshold, and an early build rejected ANET's real -24%
// DeepSeek crash this way.
func TestSanitize_DailyBarsExemptFromRangeGate(t *testing.T) {
	bars := series("ANET", Timeframe1Day, 25, 129.15, 2.4)
	bars = append(bars, Candle{ // the real 2025-01-27 crash session
		Ticker: "ANET", Timeframe: Timeframe1Day, Timestamp: at(bars, 24),
		Open: 120.00, High: 121.00, Low: 97.69, Close: 100.50, Volume: 900,
	})
	if _, rejected := SanitizeCandlesWith(bars, DefaultCandleSanity()); len(rejected) != 0 {
		t.Fatalf("a real daily catalyst bar must survive, got %v", rejected[0])
	}
}

// ── Structural checks ────────────────────────────────────────────────────────

func TestSanitize_StructuralRejections(t *testing.T) {
	base := Candle{Ticker: "T", Timeframe: Timeframe1Hour, Timestamp: time.Now(), Open: 10, High: 11, Low: 9, Close: 10}
	cases := []struct {
		name   string
		mutate func(*Candle)
		reason string
	}{
		{"high below low", func(c *Candle) { c.High, c.Low = 9, 11 }, "high_below_low"},
		{"high below body", func(c *Candle) { c.High = 9.5 }, "high_below_body"},
		{"low above body", func(c *Candle) { c.Low = 10.5 }, "low_above_body"},
		{"zero close", func(c *Candle) { c.Close = 0 }, "non_positive_price"},
		{"negative low", func(c *Candle) { c.Low = -1 }, "non_positive_price"},
		{"NaN high", func(c *Candle) { c.High = math.NaN() }, "non_finite_price"},
		{"Inf open", func(c *Candle) { c.Open = math.Inf(1) }, "non_finite_price"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.mutate(&c)
			kept, rejected := SanitizeCandlesWith([]Candle{c}, DefaultCandleSanity())
			if len(rejected) != 1 {
				t.Fatalf("expected rejection, kept=%d", len(kept))
			}
			if rejected[0].Reason != tc.reason {
				t.Errorf("reason = %q, want %q", rejected[0].Reason, tc.reason)
			}
		})
	}
}

// Structural corruption is never legitimate, so it must be dropped even when
// the operator has switched the continuity check off.
func TestSanitize_StructuralAppliesWhenContinuityDisabled(t *testing.T) {
	cfg := DefaultCandleSanity()
	cfg.Enabled = false
	bad := Candle{Ticker: "T", Timeframe: Timeframe1Hour, Timestamp: time.Now(), Open: 10, High: 9, Low: 11, Close: 10}
	if _, rejected := SanitizeCandlesWith([]Candle{bad}, cfg); len(rejected) != 1 {
		t.Fatal("structural checks must run even with continuity disabled")
	}
}

func TestSanitize_DisabledKeepsDiscontinuousBar(t *testing.T) {
	cfg := DefaultCandleSanity()
	cfg.Enabled = false
	bars := series("ANET", Timeframe1Hour, 25, 190.34, 2.0)
	bars = append(bars, Candle{
		Ticker: "ANET", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
		Open: 190.40, High: 224.98, Low: 190.20, Close: 211.47,
	})
	if _, rejected := SanitizeCandlesWith(bars, cfg); len(rejected) != 0 {
		t.Fatalf("continuity disabled must keep the bar, got %v", rejected[0])
	}
}

// ── Cascade guard ────────────────────────────────────────────────────────────

// A rejected bar must not become the continuity reference for its successor,
// or one bad print would drag a legitimate follow-on bar out with it.
func TestSanitize_RejectedBarIsNotTheNextReference(t *testing.T) {
	bars := series("ANET", Timeframe1Hour, 25, 190.34, 2.0)
	bars = append(bars,
		Candle{ // corrupt spike
			Ticker: "ANET", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
			Open: 190.40, High: 224.98, Low: 190.20, Close: 211.47,
		},
		Candle{ // ordinary bar back at the real price
			Ticker: "ANET", Timeframe: Timeframe1Hour, Timestamp: at(bars, 2),
			Open: 190.50, High: 191.20, Low: 189.90, Close: 190.60,
		},
	)
	kept, rejected := SanitizeCandlesWith(bars, DefaultCandleSanity())
	if len(rejected) != 1 {
		t.Fatalf("only the spike should be rejected, got %d", len(rejected))
	}
	last := kept[len(kept)-1]
	if last.Close != 190.60 {
		t.Errorf("the normal follow-on bar must survive; last close %.2f", last.Close)
	}
}

// ── Batch handling ───────────────────────────────────────────────────────────

// UpsertCandles accepts any mix of tickers/timeframes, so continuity must be
// judged per series — otherwise one ticker's price level would be compared
// against another's.
func TestSanitize_GroupsByTickerAndTimeframe(t *testing.T) {
	anet := series("ANET", Timeframe1Hour, 25, 190.0, 2.0)
	jmia := series("JMIA", Timeframe1Hour, 25, 6.0, 0.08)
	daily := series("ANET", Timeframe1Day, 25, 190.0, 6.0)

	mixed := make([]Candle, 0, 75)
	// Interleave so a grouping bug cannot pass by accident.
	for i := range 25 {
		mixed = append(mixed, anet[i], jmia[i], daily[i])
	}
	if _, rejected := SanitizeCandlesWith(mixed, DefaultCandleSanity()); len(rejected) != 0 {
		t.Fatalf("clean interleaved series must all survive, got %v", rejected[0])
	}
}

// The guard must not disturb the caller's slice: provider code may still hold
// a reference to it.
func TestSanitize_DoesNotMutateOrReorderInput(t *testing.T) {
	bars := series("ANET", Timeframe1Hour, 25, 190.0, 2.0)
	// Present them newest-first so any in-place sort would be visible.
	for i, j := 0, len(bars)-1; i < j; i, j = i+1, j-1 {
		bars[i], bars[j] = bars[j], bars[i]
	}
	snapshot := make([]Candle, len(bars))
	copy(snapshot, bars)

	SanitizeCandlesWith(bars, DefaultCandleSanity())

	for i := range bars {
		if bars[i] != snapshot[i] {
			t.Fatalf("input mutated at %d: %+v != %+v", i, bars[i], snapshot[i])
		}
	}
}

// Kept bars come back in the caller's original order, not sorted order.
func TestSanitize_PreservesInputOrder(t *testing.T) {
	bars := series("ANET", Timeframe1Hour, 25, 190.0, 2.0)
	bars = append(bars, Candle{
		Ticker: "ANET", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
		Open: 190.40, High: 224.98, Low: 190.20, Close: 211.47,
	})
	// Reverse: newest first, corrupt bar at index 0.
	for i, j := 0, len(bars)-1; i < j; i, j = i+1, j-1 {
		bars[i], bars[j] = bars[j], bars[i]
	}
	kept, rejected := SanitizeCandlesWith(bars, DefaultCandleSanity())
	if len(rejected) != 1 {
		t.Fatalf("expected 1 rejection, got %d", len(rejected))
	}
	for i := 1; i < len(kept); i++ {
		if kept[i].Timestamp.After(kept[i-1].Timestamp) {
			t.Fatal("output order changed; input order must be preserved")
		}
	}
}

// ── Insufficient history ─────────────────────────────────────────────────────

// Discarding the leading bars of every fetch would be worse than admitting
// them; they are structurally validated regardless.
func TestSanitize_ShortSeriesSkipsContinuityCheck(t *testing.T) {
	bars := series("ANET", Timeframe1Hour, 5, 190.0, 2.0)
	bars = append(bars, Candle{
		Ticker: "ANET", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
		Open: 190.40, High: 224.98, Low: 190.20, Close: 211.47,
	})
	if _, rejected := SanitizeCandlesWith(bars, DefaultCandleSanity()); len(rejected) != 0 {
		t.Fatalf("cannot judge continuity without history; got %v", rejected[0])
	}
}

// A perfectly flat window carries no volatility information, so the ratio is
// undefined and the bar must not be rejected on that basis.
func TestSanitize_FlatWindowSkipsContinuityCheck(t *testing.T) {
	bars := series("FLAT", Timeframe1Hour, 25, 50.0, 0)
	bars = append(bars, Candle{
		Ticker: "FLAT", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
		Open: 50, High: 80, Low: 50, Close: 79,
	})
	if _, rejected := SanitizeCandlesWith(bars, DefaultCandleSanity()); len(rejected) != 0 {
		t.Fatalf("flat window gives no scale to judge against; got %v", rejected[0])
	}
}

// ── Config plumbing ──────────────────────────────────────────────────────────

// A zero-valued config must apply calibrated defaults, not admit everything —
// otherwise a partially specified YAML block would silently disable the guard.
func TestSanitize_ZeroConfigFallsBackToDefaults(t *testing.T) {
	bars := series("ANET", Timeframe1Hour, 25, 190.34, 2.0)
	bars = append(bars, Candle{
		Ticker: "ANET", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
		Open: 190.40, High: 224.98, Low: 190.20, Close: 211.47,
	})
	cfg := CandleSanityConfig{Enabled: true} // thresholds all zero
	if _, rejected := SanitizeCandlesWith(bars, cfg); len(rejected) != 1 {
		t.Fatal("zero thresholds must fall back to defaults and still reject")
	}
}

func TestSetCandleSanity_RoundTrips(t *testing.T) {
	original := CandleSanity()
	t.Cleanup(func() { SetCandleSanity(original) })

	SetCandleSanity(CandleSanityConfig{Enabled: true, MaxRangeRatio: 3, MaxRangePct: 4, ScaleWindow: 5})
	got := CandleSanity()
	if got.MaxRangeRatio != 3 || got.MaxRangePct != 4 || got.ScaleWindow != 5 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestSanitize_EmptyInput(t *testing.T) {
	kept, rejected := SanitizeCandlesWith(nil, DefaultCandleSanity())
	if len(kept) != 0 || len(rejected) != 0 {
		t.Errorf("empty input should yield empty output, got %d/%d", len(kept), len(rejected))
	}
}

// ── Genuine re-rating restoration ────────────────────────────────────────────

// The highest-risk branch in the filter. When a security genuinely re-rates and
// holds the new level, the run of rejections must be reinterpreted and the bars
// RESTORED — not discarded. An early build without this lost 18,465 real bars
// from the live candle table, because the reference never advanced past the
// first rejected bar.
func TestSanitize_SustainedReRatingIsRestored(t *testing.T) {
	cfg := DefaultCandleSanity() // MaxConsecutiveRejections = 3
	bars := series("ABVX", Timeframe1Hour, 25, 10.00, 0.20)

	// Three consecutive wide bars carrying price to a new level, each of which
	// trips the range gate on its own.
	for i := 1; i <= 3; i++ {
		bars = append(bars, Candle{
			Ticker: "ABVX", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
			Open: 10.0, High: 16.0, Low: 9.8, Close: 15.5, Volume: 900,
		})
	}
	// Then ordinary trade up at the new level.
	bars = append(bars, Candle{
		Ticker: "ABVX", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
		Open: 15.5, High: 15.7, Low: 15.3, Close: 15.6, Volume: 900,
	})

	kept, rejected := SanitizeCandlesWith(bars, cfg)
	if len(rejected) != 0 {
		t.Fatalf("a sustained re-rating must be restored, got %d rejections: %v", len(rejected), rejected[0])
	}
	if len(kept) != len(bars) {
		t.Errorf("kept %d bars, want all %d", len(kept), len(bars))
	}
}

// The counterpart: a run SHORTER than the limit, followed by a bar that
// reconciles with the old level, stays rejected. This is what keeps an isolated
// corrupt print from being restored just because it was followed by another.
func TestSanitize_ShortRunFollowedByNormalBarStaysRejected(t *testing.T) {
	cfg := DefaultCandleSanity()
	bars := series("ANET", Timeframe1Hour, 25, 190.34, 2.0)
	bars = append(bars,
		Candle{ // corrupt spike 1
			Ticker: "ANET", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
			Open: 190.40, High: 224.98, Low: 190.20, Close: 211.47,
		},
		Candle{ // corrupt spike 2 — still below the 3-bar limit
			Ticker: "ANET", Timeframe: Timeframe1Hour, Timestamp: at(bars, 2),
			Open: 190.50, High: 226.10, Low: 190.10, Close: 210.90,
		},
		Candle{ // back at the real level, settling the run as corruption
			Ticker: "ANET", Timeframe: Timeframe1Hour, Timestamp: at(bars, 3),
			Open: 190.50, High: 191.20, Low: 189.90, Close: 190.60,
		},
	)
	_, rejected := SanitizeCandlesWith(bars, cfg)
	if len(rejected) != 2 {
		t.Fatalf("expected both spikes rejected, got %d", len(rejected))
	}
	for _, r := range rejected {
		if r.Candle.High < 200 {
			t.Errorf("rejected a legitimate bar: high %.2f", r.Candle.High)
		}
	}
}

// A run still pending when the series ends must be flushed as rejections, not
// silently dropped from both outputs.
func TestSanitize_PendingRunAtSeriesEndIsFlushed(t *testing.T) {
	cfg := DefaultCandleSanity()
	bars := series("ANET", Timeframe1Hour, 25, 190.34, 2.0)
	bars = append(bars, Candle{
		Ticker: "ANET", Timeframe: Timeframe1Hour, Timestamp: at(bars, 1),
		Open: 190.40, High: 224.98, Low: 190.20, Close: 211.47,
	})
	kept, rejected := SanitizeCandlesWith(bars, cfg)
	if len(rejected) != 1 {
		t.Fatalf("trailing pending run must be flushed as a rejection, got %d", len(rejected))
	}
	if len(kept)+len(rejected) != len(bars) {
		t.Errorf("bars lost: kept %d + rejected %d != %d input", len(kept), len(rejected), len(bars))
	}
}
