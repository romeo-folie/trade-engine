package optionsreplay

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/options"
)

// yearsToExpiry is whole-day-granular time to expiry in years (ACT/365), floored at
// a tiny positive so a same-day expiry is still invertible rather than degenerate.
func yearsToExpiry(asOf, expiry time.Time) float64 {
	d := expiry.Sub(asOf).Hours() / 24 / 365
	if d < 1.0/365/24 { // < 1 hour → clamp to a sliver so BS is well-defined
		d = 1.0 / 365 / 24
	}
	return d
}

// HistChainProvider reconstructs an as-of option chain from historical quotes for
// the gate engine's ChainProvider seam. For each contract in the requested window
// it reads the NBBO at the evaluation instant (from the context) and derives the
// missing greeks — Alpaca serves delta/IV on LIVE snapshots only — by inverting the
// quote mid through Black-Scholes (options.ImpliedVol) and computing options.BSDelta
// at that vol. A contract whose IV can't be inverted gets a nil Delta, so the delta
// gate skips it exactly as it would a live snapshot with missing greeks (fail closed).
type HistChainProvider struct {
	data HistoricalOptionData
	r    float64 // risk-free rate (annualized)
	q    float64 // dividend yield (annualized)
}

// NewHistChainProvider builds the provider. r and q feed Black-Scholes; for the
// short DTEs options-ORB trades they barely move the result, so 0/0 is a fine default.
func NewHistChainProvider(hd HistoricalOptionData, r, q float64) *HistChainProvider {
	return &HistChainProvider{data: hd, r: r, q: q}
}

// GetOptionChain implements options.ChainProvider. The evaluation instant + spot
// come from the context (WithAsOf), which the gate forwards unchanged — so no
// lookahead and no interface change from the live path.
func (p *HistChainProvider) GetOptionChain(ctx context.Context, underlying string, filt execution.OptionChainFilter) ([]execution.OptionSnapshot, error) {
	ao, ok := asOfFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("optionsreplay: no as-of in context (call WithAsOf before Select)")
	}
	if ao.spot <= 0 {
		return nil, fmt.Errorf("optionsreplay: non-positive underlying spot %.2f at %s", ao.spot, ao.at.Format(time.RFC3339))
	}
	contracts, err := p.data.ListContracts(ctx, underlying, filt.Right, filt.ExpiryGTE, filt.ExpiryLTE)
	if err != nil {
		return nil, fmt.Errorf("optionsreplay: list contracts: %w", err)
	}
	snaps := make([]execution.OptionSnapshot, 0, len(contracts))
	for _, oc := range contracts {
		bid, ask, ok := p.data.QuoteAt(ctx, oc.OCCSymbol, ao.at)
		if !ok {
			continue // no quote at this instant → contract is invisible to the gate
		}
		snap := execution.OptionSnapshot{OCCSymbol: oc.OCCSymbol, Bid: bid, Ask: ask}
		mid := snap.Mid()
		if mid > 0 {
			t := yearsToExpiry(ao.at, oc.Expiry)
			if iv, ok := options.ImpliedVol(oc.Right, mid, ao.spot, oc.Strike, t, p.r, p.q); ok {
				d := options.BSDelta(oc.Right, ao.spot, oc.Strike, t, p.r, p.q, iv)
				ivC, dC := iv, d
				snap.IV = &ivC
				snap.Delta = &dC
			}
		}
		snaps = append(snaps, snap)
	}
	return snaps, nil
}

// BuildIVSeries derives a daily ATM implied-volatility series for underlying over
// [from, to] by inverting the at-the-money option quote on each trading day. It is
// the historical seed the IV-Rank gate needs: to produce a valid rank on the first
// backtest day, `from` must reach back at least the IV service's MinSamples of
// trading days before the window (the caller is responsible for that lookback).
//
// It reuses the same as-of quote reads as the chain provider, so the seed is
// consistent with what the gate sees intraday. A day with no invertible ATM quote
// is skipped (the series is sparse, and the rank gate fails closed when the sample
// count is short — the correct, conservative behavior).
func (p *HistChainProvider) BuildIVSeries(ctx context.Context, underlying string, from, to time.Time, refMinuteET int) ([]options.DailyIV, error) {
	underlying = strings.ToUpper(underlying)
	var out []options.DailyIV
	// Iterate ET CALENDAR days regardless of the input timestamps' own location (the
	// CLI parses --from/--to in UTC): a UTC-midnight day-floor would sample the ATM
	// quote in the prior ET evening, when the market is closed, and seed an empty
	// series that fails the IV-rank gate for every signal.
	etFrom := from.In(etLoc)
	day := time.Date(etFrom.Year(), etFrom.Month(), etFrom.Day(), 0, 0, 0, 0, etLoc)
	for ; !day.After(to); day = day.AddDate(0, 0, 1) {
		if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		// Sample the ATM quote refMinuteET minutes after the 09:30 ET regular-session
		// open (not after midnight) so the series is comparable day to day and lands
		// inside real trading hours. time.Date in etLoc resolves DST correctly.
		ref := time.Date(day.Year(), day.Month(), day.Day(), 9, 30, 0, 0, etLoc).
			Add(time.Duration(refMinuteET) * time.Minute)
		bars, err := p.data.UnderlyingBars(ctx, underlying, ref, ref.Add(30*time.Minute))
		if err != nil {
			// A systemic failure (e.g. 403 no OPRA entitlement) must be visible, not
			// silently indistinguishable from a thin-data day.
			slog.Warn("optionsreplay: IV seed underlying bars failed", "underlying", underlying, "at", ref, "err", err)
			continue
		}
		if len(bars) == 0 {
			continue
		}
		spot := bars[0].Close
		if spot <= 0 {
			continue
		}
		// Nearest listed expiry within a short window, ATM strike nearest spot.
		iv, ok := p.atmIV(ctx, underlying, ref, spot)
		if !ok {
			continue
		}
		out = append(out, options.DailyIV{Date: dayFloorLoc(day), ATMIV: iv})
	}
	return out, nil
}

// atmIV finds the ATM call nearest spot in the front DTE window and inverts its mid.
func (p *HistChainProvider) atmIV(ctx context.Context, underlying string, at time.Time, spot float64) (float64, bool) {
	lo := dayFloorLoc(at).AddDate(0, 0, 1)
	hi := dayFloorLoc(at).AddDate(0, 0, 7)
	contracts, err := p.data.ListContracts(ctx, underlying, "call", lo, hi)
	if err != nil {
		slog.Warn("optionsreplay: IV seed contract listing failed", "underlying", underlying, "at", at, "err", err)
		return 0, false
	}
	if len(contracts) == 0 {
		return 0, false
	}
	// Nearest strike to spot.
	sort.Slice(contracts, func(i, j int) bool {
		return absf(contracts[i].Strike-spot) < absf(contracts[j].Strike-spot)
	})
	for _, oc := range contracts {
		bid, ask, ok := p.data.QuoteAt(ctx, oc.OCCSymbol, at)
		if !ok {
			continue
		}
		mid := (bid + ask) / 2
		if mid <= 0 {
			continue
		}
		t := yearsToExpiry(at, oc.Expiry)
		if iv, ok := options.ImpliedVol(oc.Right, mid, spot, oc.Strike, t, p.r, p.q); ok {
			return iv, true
		}
	}
	return 0, false
}

// memIVReader is an in-memory options.IVHistoryReader over a precomputed series, so
// a backtest run needs no option_iv_history DB rows. GetIVHistory returns the
// oldest-first slice within [from, to], matching the store reader's contract.
type memIVReader struct {
	byUnderlying map[string][]options.DailyIV
}

// NewMemIVReader wraps a per-underlying daily-IV series (as from BuildIVSeries).
func NewMemIVReader(series map[string][]options.DailyIV) *memIVReader {
	m := &memIVReader{byUnderlying: make(map[string][]options.DailyIV, len(series))}
	for u, s := range series {
		cp := append([]options.DailyIV(nil), s...)
		sort.Slice(cp, func(i, j int) bool { return cp[i].Date.Before(cp[j].Date) })
		m.byUnderlying[strings.ToUpper(u)] = cp
	}
	return m
}

func (m *memIVReader) GetIVHistory(_ context.Context, underlying string, from, to time.Time) ([]options.DailyIV, error) {
	all := m.byUnderlying[strings.ToUpper(underlying)]
	var out []options.DailyIV
	for _, d := range all {
		if d.Date.Before(from) || d.Date.After(to) {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// dayFloorLoc truncates to the day in the timestamp's own location.
func dayFloorLoc(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
