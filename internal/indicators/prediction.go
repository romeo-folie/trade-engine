package indicators

import (
	"math"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// PredictionIndicatorSet holds all computed indicators for a prediction market
// contract, derived from its historical price snapshots.
type PredictionIndicatorSet struct {
	// PriceTrend is the linear regression slope of the Yes-price over the
	// observation window, expressed as change-per-hour.  Positive = rising
	// probability, negative = falling.
	PriceTrend float64

	// PriceVelocity is the percentage change between the oldest and most recent
	// price in the window (not annualised).
	PriceVelocity float64

	// VolumeTrend is the slope of 24h-volume observations (change per snapshot).
	VolumeTrend float64

	// SpreadTrend is the slope of (ask-bid)/mid across snapshots.  A rising
	// spread suggests growing uncertainty / reduced liquidity.
	SpreadTrend float64

	// DaysToResolution is the calendar days remaining until the contract's
	// resolution deadline, computed from the most recent snapshot.
	DaysToResolution float64

	// ProbabilityVelocity is the per-day rate of probability change estimated
	// over the observation window.  Used by the prompt to quantify momentum.
	ProbabilityVelocity float64

	// CurrentPrice is the most recent Yes-price (implied probability, 0–1).
	CurrentPrice float64

	// Spread is the most recent (ask-bid) value.
	Spread float64

	// SampleCount is the number of snapshots used for computation.
	SampleCount int
}

// ComputePredictionIndicators derives all prediction indicators from a slice of
// snapshots ordered chronologically (oldest first).  Returns nil if fewer than
// 2 snapshots are provided.
func ComputePredictionIndicators(snaps []store.PredictionSnapshot) *PredictionIndicatorSet {
	n := len(snaps)
	if n < 2 {
		return nil
	}

	ind := &PredictionIndicatorSet{SampleCount: n}

	// Most recent snapshot.
	latest := snaps[n-1]
	oldest := snaps[0]

	ind.CurrentPrice = latest.Price
	ind.Spread = latest.Ask - latest.Bid

	// Days to resolution.
	if !latest.ResolutionDeadline.IsZero() {
		ind.DaysToResolution = latest.ResolutionDeadline.Sub(time.Now()).Hours() / 24.0
		if ind.DaysToResolution < 0 {
			ind.DaysToResolution = 0
		}
	}

	// ── Price trend (linear regression slope in hours) ────────────────────────
	// x = elapsed hours from oldest snapshot, y = price
	prices := make([]float64, n)
	times := make([]float64, n)
	for i, s := range snaps {
		prices[i] = s.Price
		times[i] = s.CreatedAt.Sub(oldest.CreatedAt).Hours()
	}
	ind.PriceTrend = linearSlope(times, prices)

	// Price velocity: % change over full window.
	if oldest.Price > 0 {
		ind.PriceVelocity = (latest.Price - oldest.Price) / oldest.Price * 100
	}

	// ── Volume trend (slope per snapshot index) ───────────────────────────────
	volumes := make([]float64, n)
	indices := make([]float64, n)
	for i, s := range snaps {
		volumes[i] = s.Volume24h
		indices[i] = float64(i)
	}
	ind.VolumeTrend = linearSlope(indices, volumes)

	// ── Spread trend ──────────────────────────────────────────────────────────
	spreads := make([]float64, n)
	for i, s := range snaps {
		mid := (s.Ask + s.Bid) / 2.0
		if mid > 0 {
			spreads[i] = (s.Ask - s.Bid) / mid
		}
	}
	ind.SpreadTrend = linearSlope(indices, spreads)

	// ── Per-day probability velocity ──────────────────────────────────────────
	windowDays := snaps[n-1].CreatedAt.Sub(snaps[0].CreatedAt).Hours() / 24.0
	if windowDays > 0 {
		ind.ProbabilityVelocity = (latest.Price - oldest.Price) / windowDays
	}

	return ind
}

// linearSlope returns the OLS slope (Σ(x-x̄)(y-ȳ) / Σ(x-x̄)²) for paired
// x and y slices.  Returns 0 if x has zero variance.
func linearSlope(x, y []float64) float64 {
	n := float64(len(x))
	if n < 2 {
		return 0
	}
	var sumX, sumY float64
	for i := range x {
		sumX += x[i]
		sumY += y[i]
	}
	meanX := sumX / n
	meanY := sumY / n

	var num, den float64
	for i := range x {
		dx := x[i] - meanX
		dy := y[i] - meanY
		num += dx * dy
		den += dx * dx
	}
	if math.Abs(den) < 1e-12 {
		return 0
	}
	return num / den
}
