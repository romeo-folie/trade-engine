package orb

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/indicators"
)

// Entry-quality filters (Phase 2): the live counterparts to the backtest's
// `range-filter` variant. They gate a forex/CFD session BEFORE brackets are placed,
// rejecting opening ranges that are too narrow/wide relative to volatility, or sessions
// whose spread is too large relative to the planned risk.
//
// Both filters FAIL OPEN: if the supporting data (ATR history, a live quote) can't be
// fetched, the trade is allowed (with a warning) rather than silently suppressed. A
// flaky data feed must never quietly halt all trading — the opposite failure (an
// unfiltered trade) is the lesser harm and is visible in the logs.

// passesRangeQuality checks the ATR-normalised opening-range width against the
// instrument's [MinRangeATR, MaxRangeATR]. Returns (ok, ratio, reason). The filter is
// disabled (always ok) when both bounds are 0; a single zero bound leaves that side open.
func (s *ORBScheduler) passesRangeQuality(ctx context.Context, ticker string, r *OpeningRange, inst config.Instrument) (bool, float64, string) {
	if inst.MinRangeATR <= 0 && inst.MaxRangeATR <= 0 {
		return true, 0, ""
	}
	atr := s.instrumentATR(ctx, ticker)
	if atr <= 0 {
		slog.Warn("orb: range-quality filter: ATR unavailable — allowing trade (fail-open)", "ticker", ticker)
		return true, 0, ""
	}
	ratio := (r.High - r.Low) / atr
	if inst.MinRangeATR > 0 && ratio < inst.MinRangeATR {
		return false, ratio, fmt.Sprintf("range_atr_ratio %.2f < min %.2f", ratio, inst.MinRangeATR)
	}
	if inst.MaxRangeATR > 0 && ratio > inst.MaxRangeATR {
		return false, ratio, fmt.Sprintf("range_atr_ratio %.2f > max %.2f", ratio, inst.MaxRangeATR)
	}
	return true, ratio, ""
}

// CloseFractionEligibility reports which breakout sides survive the close-position guard
// (config.Instrument.CloseFracThreshold): the buy side is eligible only when the range
// locked with price near its top (CloseFrac >= threshold), the sell side only when it
// locked near its bottom (CloseFrac <= 1 - threshold). With a threshold in (0.5, 1) a
// mid-range close disables BOTH sides (directionless chop → skip the session). applied is
// false when the guard is off (threshold <= 0), so the caller leaves both sides untouched
// rather than treating the always-true bounds as a decision.
//
// Unlike the ATR/spread gates there is no market-data fetch and thus no fail-open path:
// CloseFrac is computed from the same formation bars that defined the range, so it is
// always available whenever a range exists. A degenerate range already yields CloseFrac
// 0.5 (see RangeBuffer.Add), which keeps both sides for any threshold <= 0.5.
func CloseFractionEligibility(r *OpeningRange, inst config.Instrument) (buyOK, sellOK, applied bool) {
	if inst.CloseFracThreshold <= 0 || r == nil {
		return true, true, false
	}
	t := inst.CloseFracThreshold
	return r.CloseFrac >= t, r.CloseFrac <= 1-t, true
}

// instrumentATR fetches a thin lookback of the configured ATR timeframe and returns the
// ATR, or 0 when market data is unavailable or too shallow (ComputeATR's own convention).
func (s *ORBScheduler) instrumentATR(ctx context.Context, ticker string) float64 {
	if s.marketData == nil || (!s.isForexMode() && !s.isFuturesMode()) {
		return 0
	}
	cfg := s.effectiveForexCfg() // forex config, or the futures config mapped onto it
	period := cfg.ATRPeriod
	if period <= 0 {
		period = 14
	}
	tf := data.Timeframe(cfg.ATRTimeframe)
	if tf == "" {
		tf = data.Timeframe1Hour
	}
	to := s.now()
	from := to.Add(-atrLookback(period, tf))
	candles, err := s.marketData.FetchCandles(ctx, ticker, tf, from, to)
	if err != nil {
		return 0
	}
	return indicators.ComputeATR(candles, period)
}

// passesSpread checks the current bid/ask spread against the planned stop distance:
// spread / |entry_stop - stop_loss|. Returns (ok, ratio, reason). Disabled when
// maxSpreadToRisk is 0; fails open when no live quote is available.
func (s *ORBScheduler) passesSpread(ctx context.Context, ticker string, entryStop, stopLoss, maxSpreadToRisk float64) (bool, float64, string) {
	if maxSpreadToRisk <= 0 {
		return true, 0, ""
	}
	if s.marketData == nil {
		return true, 0, ""
	}
	q, err := s.marketData.FetchQuote(ctx, ticker)
	if err != nil || q == nil || q.Bid <= 0 || q.Ask <= 0 {
		slog.Warn("orb: spread filter: quote unavailable — allowing trade (fail-open)", "ticker", ticker)
		return true, 0, ""
	}
	risk := math.Abs(entryStop - stopLoss)
	if risk <= 0 {
		return true, 0, ""
	}
	ratio := (q.Ask - q.Bid) / risk
	if ratio > maxSpreadToRisk {
		return false, ratio, fmt.Sprintf("spread/risk %.2f > max %.2f", ratio, maxSpreadToRisk)
	}
	return true, ratio, ""
}

// passesCommission checks the estimated round-trip commission against the planned DOLLAR
// risk: estimated fills / (|entry_stop − stop_loss| × qty). The spread gate can't catch
// this failure mode — spread cost scales with position size while commission may have a
// broker minimum or a bps-on-notional charge. A split-bracket instrument
// (partial_take_profit_r + partial_fraction) places runner and partial as SEPARATE broker
// orders — two entry fills and two exit fills — so its round trip pays 4 sides, not 2.
// Returns (ok, ratio, reason). Disabled when MaxCommissionToRisk or no commission estimate is set;
// unlike the ATR and spread filters it needs no market data, so there is no fail-open path.
func passesCommission(qty int, entryStop, stopLoss float64, inst config.Instrument) (bool, float64, string) {
	if inst.MaxCommissionToRisk <= 0 || (inst.CommissionPerSide <= 0 && inst.CommissionBpsPerSide <= 0) {
		return true, 0, ""
	}
	// Dollar risk = price distance × qty × point value. Forex/CFD units are $1/point (pv
	// defaults to 1), so existing instruments are unchanged; a futures contract's point
	// value (MNQ $2, MES $5) must be applied or the ratio is inflated by that multiple and
	// the gate wrongly disables valid sides. Matches the backtest's commissionRiskOK.
	pv := inst.PointValue
	if pv <= 0 {
		pv = 1
	}
	riskDollars := math.Abs(entryStop-stopLoss) * float64(qty) * pv
	if riskDollars <= 0 {
		return true, 0, ""
	}
	fills := 2.0
	if inst.PartialTakeProfitR > 0 && inst.PartialFraction > 0 {
		fills = 4.0 // ScaleOutBrackets splits into two orders: 2 entry + 2 exit fills
	}
	commission := fills * estimatedFillCommission(qty, entryStop, pv, inst)
	ratio := commission / riskDollars
	if ratio > inst.MaxCommissionToRisk {
		return false, ratio, fmt.Sprintf("commission/risk %.2f > max %.2f (round-trip $%.2f on $%.2f risk)",
			ratio, inst.MaxCommissionToRisk, commission, riskDollars)
	}
	return true, ratio, ""
}

func estimatedFillCommission(qty int, fillPrice, pointValue float64, inst config.Instrument) float64 {
	commission := inst.CommissionPerSide
	if inst.CommissionBpsPerSide > 0 {
		notional := math.Abs(fillPrice) * float64(qty) * pointValue
		bpsCommission := notional * inst.CommissionBpsPerSide / 10000.0
		if bpsCommission < inst.CommissionMinPerSide {
			bpsCommission = inst.CommissionMinPerSide
		}
		commission += bpsCommission
	}
	return commission
}

// atrLookback returns how far back to fetch for an ATR of `period` bars on timeframe
// `tf`. For intraday timeframes it adds a 72h pad so the window clears the ~48h forex
// weekend gap — otherwise a Monday-morning session's lookback lands inside the closed
// weekend, returns too few bars, and ComputeATR silently yields 0 (filter fails open).
// ComputeATR uses every bar passed, so over-fetching only makes the ATR more robust.
func atrLookback(period int, tf data.Timeframe) time.Duration {
	barDur := timeframeDuration(tf)
	lookback := time.Duration(period+2) * barDur
	if barDur < 24*time.Hour {
		lookback += 72 * time.Hour
	}
	return lookback
}

// timeframeDuration is the wall-clock span of one bar for the timeframes the ATR
// filter supports. Unknown timeframes fall back to one hour.
func timeframeDuration(tf data.Timeframe) time.Duration {
	switch tf {
	case data.Timeframe1Min:
		return time.Minute
	case data.Timeframe5Min:
		return 5 * time.Minute
	case data.Timeframe15Min:
		return 15 * time.Minute
	case data.Timeframe1Hour:
		return time.Hour
	case data.Timeframe4Hour:
		return 4 * time.Hour
	case data.Timeframe1Day:
		return 24 * time.Hour
	default:
		return time.Hour
	}
}
