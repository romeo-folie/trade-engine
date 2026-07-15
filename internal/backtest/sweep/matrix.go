// Package sweep drives the ORB strategy-discovery parameter search: it generates a parameter
// grid, resolves each parameter set into a concrete orbreplay configuration + variant, runs the
// backtest over the walk-forward period splits, and ranks candidates by robustness (not net
// profit). The methodology is coarse-grid-then-refine: a broad low-resolution sweep first, then
// local refinement of the survivors, then out-of-sample walk-forward validation.
//
// The engine is a HARNESS over the knobs the backtest already supports. New strategy primitives
// (extra sessions, retest/body-size entries, macro blackout, failed-breakout exits) land as
// separate features that each add a dimension here.
package sweep

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/romeo-folie/trade-engine/internal/backtest/orbreplay"
	"github.com/romeo-folie/trade-engine/internal/config"
)

// ParamSet is one concrete point in the search space: a full ORB rule configuration. Only knobs
// the backtest engine already supports are represented; every field maps to an orbreplay Variant
// flag or a config value in Resolve.
type ParamSet struct {
	Session        string  `json:"session"`          // "london" | "newyork" | "newyork_et" (DST-aware US RTH, futures)
	RangeMinutes   int     `json:"range_minutes"`    // opening-range width: 5 | 15 | 30 | 60
	Confirmation   string  `json:"confirmation"`     // "stop_touch" | "close_1m" | "close_3m"
	RiskReward     float64 `json:"risk_reward"`      // take-profit multiple of risk: 1.0 | 1.5 | 2.0
	MaxHoldMinutes int     `json:"max_hold_minutes"` // time exit: 0 (hold to bracket) | 120 | 240
	DirectionElig  bool    `json:"direction_elig"`   // trade only the trend-aligned side
	RangeATRFilter bool    `json:"range_atr_filter"` // skip sessions whose opening range is outside the coarse ATR band
	AdaptiveExits  bool    `json:"adaptive_exits"`   // break-even + ATR trailing preset
	CloseFrac      float64 `json:"close_frac"`       // close-position guard threshold (0 = off; arm buy only when range closed in top frac, sell in bottom)
}

// Coarse ATR-band and adaptive presets applied when the corresponding boolean knob is on. Kept
// as fixed coarse values here; the refine stage sweeps around a survivor's neighbourhood.
const (
	coarseMinRangeATR = 0.5
	coarseMaxRangeATR = 3.0

	coarseBreakEvenR   = 1.0
	coarseTrailATRMult = 1.5
	coarseTrailAfterR  = 1.0
)

// CoarseGrid defines the per-dimension levels for the coarse sweep. Data-driven so the resolution
// is easy to tune; the full cartesian product is GenerateCoarse.
type CoarseGrid struct {
	Sessions       []string
	RangeMinutes   []int
	Confirmations  []string
	RiskRewards    []float64
	MaxHoldMinutes []int
	DirectionElig  []bool
	RangeATRFilter []bool
	AdaptiveExits  []bool
	CloseFrac      []float64
}

// DefaultCoarseGrid is the broad, low-resolution first pass (2·4·5·3·3·2·2·2·1 = 2880 sets).
// CloseFrac defaults to {0} (guard off) so the broad sweep keeps its existing size — the
// close-position guard multiplies an already-large product, so it is studied in a focused
// sweep (set CloseFrac to e.g. {0, 0.55, 0.6, 0.65, 0.7}) rather than in every coarse run.
func DefaultCoarseGrid() CoarseGrid {
	return CoarseGrid{
		Sessions:       []string{"london", "newyork"},
		RangeMinutes:   []int{5, 15, 30, 60},
		Confirmations:  []string{"stop_touch", "close_1m", "close_3m", "retest_swing", "retest_range"},
		RiskRewards:    []float64{1.0, 1.5, 2.0},
		MaxHoldMinutes: []int{0, 120, 240},
		DirectionElig:  []bool{false, true},
		RangeATRFilter: []bool{false, true},
		AdaptiveExits:  []bool{false, true},
		CloseFrac:      []float64{0},
	}
}

// Size is the number of parameter sets the grid's cartesian product yields. An unset
// CloseFrac counts as one level (guard off), matching GenerateCoarse's defaulting.
func (g CoarseGrid) Size() int {
	closeFracLevels := len(g.CloseFrac)
	if closeFracLevels == 0 {
		closeFracLevels = 1
	}
	return len(g.Sessions) * len(g.RangeMinutes) * len(g.Confirmations) * len(g.RiskRewards) *
		len(g.MaxHoldMinutes) * len(g.DirectionElig) * len(g.RangeATRFilter) * len(g.AdaptiveExits) *
		closeFracLevels
}

// GenerateCoarse enumerates every parameter set in the grid's cartesian product. Order is stable
// (nested loops in field order) so a run is reproducible.
func (g CoarseGrid) GenerateCoarse() []ParamSet {
	// A grid built without CloseFrac levels (e.g. older callers) would collapse the innermost
	// loop to zero sets; default it to {0} (guard off) so the dimension is purely additive.
	closeFracs := g.CloseFrac
	if len(closeFracs) == 0 {
		closeFracs = []float64{0}
	}
	out := make([]ParamSet, 0, g.Size())
	for _, s := range g.Sessions {
		for _, rm := range g.RangeMinutes {
			for _, cf := range g.Confirmations {
				for _, rr := range g.RiskRewards {
					for _, mh := range g.MaxHoldMinutes {
						for _, de := range g.DirectionElig {
							for _, raf := range g.RangeATRFilter {
								for _, ae := range g.AdaptiveExits {
									for _, cfrac := range closeFracs {
										out = append(out, ParamSet{
											Session: s, RangeMinutes: rm, Confirmation: cf,
											RiskReward: rr, MaxHoldMinutes: mh, DirectionElig: de,
											RangeATRFilter: raf, AdaptiveExits: ae, CloseFrac: cfrac,
										})
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return out
}

// Hash is the parameter set's stable identity — the SHA-256 of its canonical JSON, truncated to
// 16 hex chars. Struct field order is fixed, so the encoding is deterministic.
func (p ParamSet) Hash() string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:8])
}

// JSON returns the parameter set as a JSON object for storage in sweep_results.resolved_parameters.
func (p ParamSet) JSON() json.RawMessage {
	b, _ := json.Marshal(p)
	return b
}

// Label is a short human-readable descriptor for logs and the variant name.
func (p ParamSet) Label() string {
	de := "de0"
	if p.DirectionElig {
		de = "de1"
	}
	extra := ""
	if p.RangeATRFilter {
		extra += "+atr"
	}
	if p.AdaptiveExits {
		extra += "+adpt"
	}
	if p.CloseFrac > 0 {
		extra += fmt.Sprintf("+cf%.2f", p.CloseFrac)
	}
	return fmt.Sprintf("%s/or%d/%s/rr%.1f/mh%d/%s%s", p.Session, p.RangeMinutes, p.Confirmation, p.RiskReward, p.MaxHoldMinutes, de, extra)
}

// Resolve turns a parameter set into the concrete inputs orbreplay.Engine.Run needs: a
// ForexORBConfig (session/range/risk-reward), the per-instrument config with the swept gates
// applied, and the Variant whose Apply* flags switch those gates on. Most features gate on
// BOTH a variant flag AND the per-instrument config, so both are set together. The exception
// is the close-position guard: like the live scheduler it has no variant flag and gates purely
// on inst.CloseFracThreshold (0 = off), so only the config value is set here.
func (p ParamSet) Resolve(base config.ForexORBConfig, instruments []config.Instrument) (config.ForexORBConfig, []config.Instrument, orbreplay.Variant) {
	cfg := base
	cfg.Session = p.Session
	cfg.RangeMinutes = p.RangeMinutes
	cfg.RiskReward = p.RiskReward

	out := make([]config.Instrument, len(instruments))
	for i, inst := range instruments {
		inst.MaxHoldMinutes = p.MaxHoldMinutes
		if p.Confirmation == "" || p.Confirmation == "stop_touch" {
			inst.BreakoutConfirmation = "stop_touch"
		} else {
			inst.BreakoutConfirmation = p.Confirmation
		}
		inst.DirectionEligibility = p.DirectionElig
		if p.RangeATRFilter {
			inst.MinRangeATR = coarseMinRangeATR
			inst.MaxRangeATR = coarseMaxRangeATR
		} else {
			inst.MinRangeATR, inst.MaxRangeATR = 0, 0
		}
		if p.AdaptiveExits {
			inst.BreakEvenR = coarseBreakEvenR
			inst.TrailATRMult = coarseTrailATRMult
			inst.TrailAfterR = coarseTrailAfterR
		} else {
			inst.BreakEvenR, inst.TrailATRMult, inst.TrailAfterR = 0, 0, 0
		}
		inst.CloseFracThreshold = p.CloseFrac
		out[i] = inst
	}

	isRetest := p.Confirmation == "retest_swing" || p.Confirmation == "retest_range"
	v := orbreplay.Variant{
		Name:              p.Hash(),
		MaxHoldMinutes:    p.MaxHoldMinutes,
		ApplyEntryFilters: p.RangeATRFilter,
		ApplyConfirmation: !isRetest && p.Confirmation != "" && p.Confirmation != "stop_touch",
		ApplyDirection:    p.DirectionElig,
		ApplyAdaptiveExit: p.AdaptiveExits,
		ApplyRetest:       isRetest,
	}
	return cfg, out, v
}
