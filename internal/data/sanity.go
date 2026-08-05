package data

import (
	"fmt"
	"math"
	"sort"
	"sync"
)

// Candle sanity guards (issue #519). Providers occasionally serve a corrupt
// aggregate — the trigger for this code was an Alpaca 1h bar for ANET on
// 2026-08-04T20:00Z reporting high 224.98 / close 211.47 while the same
// session's daily bar showed high 194.30 and the last trade was 190.53. Every
// other endpoint contradicted it.
//
// An unvalidated bar is not merely cosmetic: ATR, the trailing manager's
// high-water mark, and therefore the resting broker stop are all derived from
// this series. Under the ATR trailing rule that bar would have ratcheted the
// HWM to 224.98, computed a stop above the real market, and closed a healthy
// position at market on fabricated data.
//
// Two independent gates run here:
//
//   - Structural (every timeframe): prices must be finite, positive, and
//     internally ordered — high >= low, high >= max(open,close),
//     low <= min(open,close). These cannot occur in a well-formed bar at any
//     volatility, so they are always rejected.
//
//   - Range plausibility (intraday only): the bar's OWN high-low span measured
//     against the recent median range and against price. See excursive.
//
// The design was chosen by measuring the live candle table (109,946 bars), and
// two earlier approaches were discarded because that measurement showed them
// destroying real data:
//
//  1. Gating on distance from the previous close. Real overnight gaps beat the
//     corrupt bar outright — the 2026-08-04 opening hour shows VOYG +30.6% and
//     CLPT +19.7% against the corrupt bar's 18.2% — so no threshold separates
//     them. Measuring the bar's own width instead does: a gap shifts the level
//     without widening the bar (those two score 6.88x and 5.62x, versus ~17.4x
//     for the corrupt bar, against a legitimate intraday maximum of 10.13x).
//
//  2. Applying any continuity gate to daily bars. Their excursion distribution
//     is far too heavy-tailed to threshold (p99 29.8%, p99.9 110.3%, max
//     6003%), and an early build rejected ANET's real -24% DeepSeek crash,
//     INTC's real +30%, and SBET's real +70%. Daily bars are structurally
//     validated and otherwise trusted.
//
// The guard is deliberately biased toward admitting a doubtful bar. A dropped
// real bar leaves a gap that indicators tolerate; a retained corrupt bar can
// place a resting stop above the market and liquidate a healthy position.
// Everything rejected is logged, so provider degradation stays visible instead
// of becoming silent data loss.
const (
	// DefaultMaxRangeRatio is how many multiples of the recent median range a
	// single intraday bar's own high-low span may reach. Widest legitimate
	// intraday bar measured was 10.13x (KO, opening hour); the corrupt ANET bar
	// scored ~17.4x.
	DefaultMaxRangeRatio = 15.0
	// DefaultMaxRangePct is that same span as a percentage of the prior close.
	// It floors the ratio so a pathologically quiet reference window cannot
	// turn an ordinary bar into a large multiple. The corrupt bar was 18.3%.
	DefaultMaxRangePct = 10.0
	// DefaultScaleWindow is how many prior bars form the median-range scale.
	DefaultScaleWindow = 20
	// DefaultMaxConsecutiveRejections is how many bars in a row may breach the
	// continuity check before the run is reinterpreted as a genuine re-rating
	// and restored. A corrupt print is typically isolated (the ANET bar was a
	// single bar, with the next bar back at the real price); a security that
	// holds a new level for this many bars is indistinguishable from one that
	// actually moved, so the guard defers rather than discarding real history.
	DefaultMaxConsecutiveRejections = 3
)

// CandleSanityConfig tunes the guards. Thresholds are deliberately operator
// -visible: a provider that starts emitting a different corruption signature
// should be answerable without a code change.
type CandleSanityConfig struct {
	Enabled                  bool    `yaml:"enabled"`
	MaxRangeRatio            float64 `yaml:"max_range_ratio"`
	MaxRangePct              float64 `yaml:"max_range_pct"`
	ScaleWindow              int     `yaml:"scale_window"`
	MaxConsecutiveRejections int     `yaml:"max_consecutive_rejections"`
}

// DefaultCandleSanity returns the empirically calibrated defaults.
func DefaultCandleSanity() CandleSanityConfig {
	return CandleSanityConfig{
		Enabled:                  true,
		MaxRangeRatio:            DefaultMaxRangeRatio,
		MaxRangePct:              DefaultMaxRangePct,
		ScaleWindow:              DefaultScaleWindow,
		MaxConsecutiveRejections: DefaultMaxConsecutiveRejections,
	}
}

// withDefaults fills unset fields so a partially specified config (or a zero
// value from a test) still applies meaningful thresholds instead of rejecting
// or admitting everything.
func (c CandleSanityConfig) withDefaults() CandleSanityConfig {
	if c.MaxRangeRatio <= 0 {
		c.MaxRangeRatio = DefaultMaxRangeRatio
	}
	if c.MaxRangePct <= 0 {
		c.MaxRangePct = DefaultMaxRangePct
	}
	if c.ScaleWindow <= 0 {
		c.ScaleWindow = DefaultScaleWindow
	}
	if c.MaxConsecutiveRejections <= 0 {
		c.MaxConsecutiveRejections = DefaultMaxConsecutiveRejections
	}
	return c
}

// CandleRejection records a dropped bar and why. Callers are expected to log
// these — a bar vanishing silently would turn provider degradation into an
// invisible failure, which is the same class of problem as trusting the bar.
type CandleRejection struct {
	Candle Candle
	Reason string // short machine-ish label, e.g. "high_below_low"
	Detail string // human-readable specifics with the offending numbers
}

func (r CandleRejection) String() string {
	return fmt.Sprintf("%s %s @ %s: %s (%s)",
		r.Candle.Ticker, r.Candle.Timeframe, r.Candle.Timestamp.UTC().Format("2006-01-02T15:04:05Z"),
		r.Reason, r.Detail)
}

var (
	sanityMu  sync.RWMutex
	sanityCfg = DefaultCandleSanity()
)

// SetCandleSanity installs the process-wide guard configuration. Called once
// during startup from the loaded config. store.UpsertCandles cannot reach the
// config package, so the setting lives here where Candle itself is defined.
func SetCandleSanity(cfg CandleSanityConfig) {
	sanityMu.Lock()
	defer sanityMu.Unlock()
	sanityCfg = cfg.withDefaults()
}

// CandleSanity returns the active guard configuration.
func CandleSanity() CandleSanityConfig {
	sanityMu.RLock()
	defer sanityMu.RUnlock()
	return sanityCfg
}

// SanitizeCandles filters a batch using the process-wide configuration.
func SanitizeCandles(candles []Candle) ([]Candle, []CandleRejection) {
	return SanitizeCandlesWith(candles, CandleSanity())
}

// SanitizeCandlesWith filters a batch using an explicit configuration.
//
// The input slice is never reordered or mutated — callers pass provider output
// that other code may still be holding. Kept bars are returned in their
// original order.
//
// A batch may legitimately mix tickers and timeframes (UpsertCandles accepts
// any mix), so continuity is evaluated per (ticker, timeframe) group in
// timestamp order. Structural checks need no history and apply to every bar.
func SanitizeCandlesWith(candles []Candle, cfg CandleSanityConfig) ([]Candle, []CandleRejection) {
	if len(candles) == 0 {
		return candles, nil
	}
	cfg = cfg.withDefaults()

	type key struct {
		ticker    string
		timeframe Timeframe
	}
	// Group indices by series, ordered by timestamp, without touching the input.
	groups := make(map[key][]int)
	for i, c := range candles {
		k := key{c.Ticker, c.Timeframe}
		groups[k] = append(groups[k], i)
	}

	rejected := make(map[int]CandleRejection)
	for _, idxs := range groups {
		sort.SliceStable(idxs, func(a, b int) bool {
			return candles[idxs[a]].Timestamp.Before(candles[idxs[b]].Timestamp)
		})

		// kept holds the accepted bars of this series in time order. The
		// continuity reference is always the last KEPT bar: letting a rejected
		// bar seed the next comparison would let one bad print cascade into its
		// successor.
		//
		// That alone is not sufficient. When a security genuinely re-rates and
		// STAYS at the new level (ABVX ran ~$10 → ~$70 on 2025-07-23; a reverse
		// split looks identical in raw stored data), the first bar trips the
		// check, the reference then never advances, and every later bar is
		// judged against a stale price forever. Validating an early build
		// against the live candle table rejected 18,465 bars this way — almost
		// all of them real.
		//
		// So consecutive rejections are held as PENDING rather than settled. An
		// isolated spike is followed by a normal bar, which settles the run as
		// genuine corruption. A sustained shift instead accumulates, and once it
		// reaches MaxConsecutiveRejections the run is reinterpreted as a real
		// re-rating: the held bars are restored and the reference re-anchors.
		// Nothing is lost in that case — the bars are put back, not skipped.
		kept := make([]Candle, 0, len(idxs))
		pending := make([]int, 0, 4)
		pendingRej := make(map[int]CandleRejection, 4)

		settle := func() { // the run was isolated corruption
			for i, r := range pendingRej {
				rejected[i] = r
			}
			pending = pending[:0]
			clear(pendingRej)
		}
		restore := func() { // the run was a real level shift
			for _, i := range pending {
				kept = append(kept, candles[i])
			}
			pending = pending[:0]
			clear(pendingRej)
		}

		for _, i := range idxs {
			c := candles[i]
			// Structural corruption is never restorable and never counts toward
			// a re-rating run — such a bar is malformed regardless of level.
			if reason, detail, bad := structurallyInvalid(c); bad {
				rejected[i] = CandleRejection{Candle: c, Reason: reason, Detail: detail}
				continue
			}
			if !cfg.Enabled {
				kept = append(kept, c)
				continue
			}
			if reason, detail, bad := excursive(c, kept, cfg); bad {
				pending = append(pending, i)
				pendingRej[i] = CandleRejection{Candle: c, Reason: reason, Detail: detail}
				if len(pending) >= cfg.MaxConsecutiveRejections {
					restore()
				}
				continue
			}
			// A bar that reconciles with the old reference proves the held run
			// was an excursion, not a new level.
			settle()
			kept = append(kept, c)
		}
		settle() // series ended while still holding a short run
	}

	if len(rejected) == 0 {
		return candles, nil
	}
	out := make([]Candle, 0, len(candles)-len(rejected))
	drops := make([]CandleRejection, 0, len(rejected))
	for i, c := range candles {
		if r, ok := rejected[i]; ok {
			drops = append(drops, r)
			continue
		}
		out = append(out, c)
	}
	return out, drops
}

// structurallyInvalid catches corruption that needs no historical context.
// These conditions cannot occur in a well-formed OHLC bar at any volatility.
func structurallyInvalid(c Candle) (reason, detail string, bad bool) {
	for name, v := range map[string]float64{"open": c.Open, "high": c.High, "low": c.Low, "close": c.Close} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "non_finite_price", fmt.Sprintf("%s=%v", name, v), true
		}
	}
	// Volume/VWAP are not load-bearing for the stop path, so a bad one is not
	// worth discarding an otherwise sound price bar over.
	if c.Open <= 0 || c.High <= 0 || c.Low <= 0 || c.Close <= 0 {
		return "non_positive_price", fmt.Sprintf("O=%.4f H=%.4f L=%.4f C=%.4f", c.Open, c.High, c.Low, c.Close), true
	}
	if c.High < c.Low {
		return "high_below_low", fmt.Sprintf("H=%.4f < L=%.4f", c.High, c.Low), true
	}
	if c.High < math.Max(c.Open, c.Close) {
		return "high_below_body", fmt.Sprintf("H=%.4f < max(O=%.4f,C=%.4f)", c.High, c.Open, c.Close), true
	}
	if c.Low > math.Min(c.Open, c.Close) {
		return "low_above_body", fmt.Sprintf("L=%.4f > min(O=%.4f,C=%.4f)", c.Low, c.Open, c.Close), true
	}
	return "", "", false
}

// intradayTimeframe reports whether the continuity check applies.
//
// Daily bars are exempt, and that is a measured decision rather than an
// oversight. Across 95,351 daily bars in the live candle table the excursion
// distribution is far too heavy-tailed to gate: p99 is 29.8%, p99.9 is 110.3%,
// and the maximum is 6003%. Every threshold that catches corruption also
// discards real catalyst sessions — an early build rejected ANET's genuine
// -24% DeepSeek crash (2025-01-27), INTC's +30% (2025-09-18) and SBET's +70%
// (2026-02-06). Those are the highest-information bars in the series and
// dropping them would do more damage than the corruption being guarded against.
// Daily bars still receive full structural validation.
func intradayTimeframe(tf Timeframe) bool {
	switch tf {
	case Timeframe1Min, Timeframe5Min, Timeframe15Min, Timeframe1Hour, Timeframe4Hour:
		return true
	default:
		return false
	}
}

// excursive rejects an intraday bar whose OWN high-low range is implausibly
// wide relative to the recent median range.
//
// Measuring the bar's own width rather than its distance from the previous
// close is what makes this work, and the difference is not cosmetic. An
// overnight gap moves the price LEVEL without widening the bar: the opening
// hour of 2026-08-04 shows VOYG +30.6%, CLPT +19.7% and SNAP +13.5% away from
// the prior close — all legitimate, and all LARGER than the 18.2% corrupt ANET
// bar. Any distance-from-prior-close threshold that catches the corrupt bar
// therefore also discards real opening gaps. Measured by own range instead,
// those same gaps score 6.88x, 5.62x and 8.30x while the corrupt bar scores
// ~17.4x, because inventing a spurious high inflates the range and a gap
// does not.
//
// Both a range multiple and a percentage-of-price floor must be breached. The
// floor stops a pathologically quiet reference window (median range near zero,
// common in illiquid tape) from turning an ordinary bar into a large multiple.
//
// Returns false (accept) when there is not enough history to judge: silently
// discarding the leading bars of every fetch would be worse than admitting
// them, and they are structurally validated regardless.
func excursive(c Candle, prior []Candle, cfg CandleSanityConfig) (reason, detail string, bad bool) {
	if !intradayTimeframe(c.Timeframe) {
		return "", "", false
	}
	if len(prior) < cfg.ScaleWindow+1 {
		return "", "", false
	}
	scale := medianRange(prior[len(prior)-cfg.ScaleWindow:])
	prevClose := prior[len(prior)-1].Close
	if scale <= 0 || prevClose <= 0 {
		// A flat window carries no volatility information to judge against.
		return "", "", false
	}

	span := c.High - c.Low
	ratio := span / scale
	pct := span / prevClose * 100

	if ratio > cfg.MaxRangeRatio && pct > cfg.MaxRangePct {
		return "implausible_range", fmt.Sprintf(
			"bar range %.4f = %.1fx median range %.4f and %.2f%% of prev close %.4f (limits %.1fx / %.2f%%)",
			span, ratio, scale, pct, prevClose, cfg.MaxRangeRatio, cfg.MaxRangePct), true
	}
	return "", "", false
}

// medianRange is the median high-low range of the window. The median rather
// than the mean is essential: a mean would be dragged upward by the very
// outlier being hunted, deflating its ratio and hiding it.
func medianRange(window []Candle) float64 {
	if len(window) == 0 {
		return 0
	}
	ranges := make([]float64, 0, len(window))
	for _, c := range window {
		ranges = append(ranges, c.High-c.Low)
	}
	sort.Float64s(ranges)
	n := len(ranges)
	if n%2 == 1 {
		return ranges[n/2]
	}
	return (ranges[n/2-1] + ranges[n/2]) / 2
}
