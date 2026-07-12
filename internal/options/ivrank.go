package options

import (
	"context"
	"time"
)

// DailyIV is one underlying's at-the-money implied volatility on one date.
type DailyIV struct {
	Date   time.Time
	ATMIV  float64 // annualized IV as a fraction (0.18 = 18%)
	Source string  // "derived" | "live" ("" when unknown); the write-through seeder
	// treats a "live" row as an authoritative cache hit rather than re-deriving over it.
}

// IVHistoryReader supplies stored daily ATM IV over a date range, oldest first.
// Implemented by the store (issue #413, migration 040).
type IVHistoryReader interface {
	GetIVHistory(ctx context.Context, underlying string, from, to time.Time) ([]DailyIV, error)
}

// ivRankOf ranks current within [min,max] of series, as a 0–100 percent. It
// returns ok=false when the series has no spread (max<=min) — a degenerate window
// where rank is undefined — so the fail-closed gate does not trade on it.
func ivRankOf(series []float64, current float64) (float64, bool) {
	if len(series) == 0 {
		return 0, false
	}
	lo, hi := series[0], series[0]
	for _, v := range series {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	if hi <= lo {
		return 0, false
	}
	rank := (current - lo) / (hi - lo) * 100
	if rank < 0 {
		rank = 0
	} else if rank > 100 {
		rank = 100
	}
	return rank, true
}

// IVRankService computes IV rank from persisted history and implements the
// IVRankProvider interface the gate engine consumes.
//
// It is the concrete resolution of the cold-start-vs-fail-closed conflict: below
// MinSamples stored days it returns ok=false, so a brand-new series aborts trades
// (fail closed) until the bounded up-front derivation (BackfillIVHistory) plus
// forward accumulation have filled the window. That is why the backfill must run
// before live trading, not after.
type IVRankService struct {
	reader       IVHistoryReader
	lookbackDays int // trailing window (e.g. 252 for ~1y; a shorter seed works too)
	minSamples   int // fail closed below this many stored days
}

// NewIVRankService builds the provider. lookbackDays<=0 defaults to 252 (~1y);
// minSamples<=0 defaults to 40 (roughly two trading months — the bounded seed).
func NewIVRankService(reader IVHistoryReader, lookbackDays, minSamples int) *IVRankService {
	if lookbackDays <= 0 {
		lookbackDays = 252
	}
	if minSamples <= 0 {
		minSamples = 40
	}
	return &IVRankService{reader: reader, lookbackDays: lookbackDays, minSamples: minSamples}
}

// IVRank returns the IV rank (0–100) for underlying as of asOf. ok is false when
// the stored series is too short (cold start) or the read fails or the window is
// flat — every such case aborts the trade in the fail-closed gate.
func (s *IVRankService) IVRank(ctx context.Context, underlying string, asOf time.Time) (float64, bool) {
	from := asOf.AddDate(0, 0, -s.lookbackDays)
	hist, err := s.reader.GetIVHistory(ctx, underlying, from, asOf)
	if err != nil || len(hist) < s.minSamples {
		return 0, false
	}
	series := make([]float64, len(hist))
	for i, d := range hist {
		series[i] = d.ATMIV
	}
	// Current IV is the most recent stored day (history is oldest-first).
	current := series[len(series)-1]
	return ivRankOf(series, current)
}
