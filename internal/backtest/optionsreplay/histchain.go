package optionsreplay

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
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

	mu      sync.Mutex
	clCache map[string][]execution.OptionContract // month-bucketed contract listings
}

// NewHistChainProvider builds the provider. r and q feed Black-Scholes; for the
// short DTEs options-ORB trades they barely move the result, so 0/0 is a fine default.
func NewHistChainProvider(hd HistoricalOptionData, r, q float64) *HistChainProvider {
	return &HistChainProvider{data: hd, r: r, q: q, clCache: map[string][]execution.OptionContract{}}
}

// listContracts wraps the data source's ListContracts with a MONTH-bucketed cache.
// The gate's DTE window shifts every day, so caching by the exact [gte,lte] would
// never hit across days; instead it fetches the whole month(s) spanning the window
// once (contract listings are static, so over-fetching is cheap) and filters locally.
// This collapses ~252 per-day list calls per underlying into ~12 per-month calls.
func (p *HistChainProvider) listContracts(ctx context.Context, underlying, right string, gte, lte time.Time) ([]execution.OptionContract, error) {
	wideLo := monthStart(gte)
	wideHi := monthStart(lte).AddDate(0, 1, 0) // first day of the month after lte
	key := strings.ToUpper(underlying) + "|" + right + "|" + wideLo.Format("2006-01") + "|" + wideHi.Format("2006-01")

	p.mu.Lock()
	wide, cached := p.clCache[key]
	p.mu.Unlock()
	if !cached {
		var err error
		wide, err = p.data.ListContracts(ctx, underlying, right, wideLo, wideHi)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		p.clCache[key] = wide
		p.mu.Unlock()
	}

	out := make([]execution.OptionContract, 0, len(wide))
	for _, c := range wide {
		if c.Expiry.Before(gte) || c.Expiry.After(lte) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
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
	contracts, err := p.listContracts(ctx, underlying, filt.Right, filt.ExpiryGTE, filt.ExpiryLTE)
	if err != nil {
		return nil, fmt.Errorf("optionsreplay: list contracts: %w", err)
	}
	// Batch every contract's NBBO in ONE call rather than N per-contract fetches.
	symbols := make([]string, len(contracts))
	for i, oc := range contracts {
		symbols[i] = oc.OCCSymbol
	}
	quotes, err := p.data.QuotesAt(ctx, symbols, ao.at)
	if err != nil {
		return nil, fmt.Errorf("optionsreplay: batch quotes: %w", err)
	}
	snaps := make([]execution.OptionSnapshot, 0, len(contracts))
	for _, oc := range contracts {
		q, ok := quotes[oc.OCCSymbol]
		if !ok {
			continue // no quote at this instant → contract is invisible to the gate
		}
		snap := execution.OptionSnapshot{OCCSymbol: oc.OCCSymbol, Bid: q.Bid, Ask: q.Ask}
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

// SeedIVHistory WRITE-THROUGH-caches the daily ATM implied volatility for underlying
// over [from, to] into ivstore. For each ET trading day NOT already stored it derives
// the ATM IV (from the same as-of bar/quote reads the gate uses) and upserts it as a
// 'derived' point stamped at the sample INSTANT (ref). The first run seeds the window;
// subsequent runs find the days present and fetch nothing — turning a ~250-day
// re-fetch every run into a one-time cost, shared with the live gate (both read
// option_iv_history). To produce a valid rank on the first backtest day, `from` must
// reach back ≥ the IV service's MinSamples trading days (the caller's lookback).
//
// A day with no invertible ATM quote is skipped (the series stays sparse and the rank
// gate fails closed when the sample count is short — the correct, conservative behavior).
func (p *HistChainProvider) SeedIVHistory(ctx context.Context, ivstore options.IVStore, underlying string, from, to time.Time, refMinuteET int) error {
	underlying = strings.ToUpper(underlying)
	existing, err := ivstore.GetIVHistory(ctx, underlying, from, to)
	if err != nil {
		return fmt.Errorf("read seeded IV history for %s: %w", underlying, err)
	}
	have := make(map[string]bool, len(existing))
	for _, d := range existing {
		have[d.Date.In(etLoc).Format("2006-01-02")] = true
	}
	// Iterate ET CALENDAR days regardless of the input timestamps' own location (the
	// CLI parses --from/--to in UTC): a UTC-midnight day-floor would sample the ATM
	// quote in the prior ET evening, when the market is closed.
	etFrom := from.In(etLoc)
	day := time.Date(etFrom.Year(), etFrom.Month(), etFrom.Day(), 0, 0, 0, 0, etLoc)
	for ; !day.After(to); day = day.AddDate(0, 0, 1) {
		if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		if have[day.Format("2006-01-02")] {
			continue // already seeded — the whole point: no re-fetch
		}
		// Sample refMinuteET minutes after the 09:30 ET open (not midnight) so the series
		// is comparable day to day and lands in real trading hours (DST-safe via etLoc).
		ref := time.Date(day.Year(), day.Month(), day.Day(), 9, 30, 0, 0, etLoc).
			Add(time.Duration(refMinuteET) * time.Minute)
		iv, ok := p.dailyATMIVAt(ctx, underlying, ref)
		if !ok {
			continue
		}
		if err := ivstore.UpsertOptionIV(ctx, underlying, ref, iv, "derived"); err != nil {
			return fmt.Errorf("persist IV %s %s: %w", underlying, ref.Format("2006-01-02"), err)
		}
	}
	return nil
}

// dailyATMIVAt reads the underlying spot at ref (the first bar close in a 30-minute
// window) and derives the ATM IV, or false when the day has no usable data.
func (p *HistChainProvider) dailyATMIVAt(ctx context.Context, underlying string, ref time.Time) (float64, bool) {
	bars, err := p.data.UnderlyingBars(ctx, underlying, ref, ref.Add(30*time.Minute))
	if err != nil {
		// A systemic failure (e.g. 403 no entitlement) must be visible, not silently
		// indistinguishable from a thin-data day.
		slog.Warn("optionsreplay: IV seed underlying bars failed", "underlying", underlying, "at", ref, "err", err)
		return 0, false
	}
	if len(bars) == 0 {
		return 0, false
	}
	spot := bars[0].Close
	if spot <= 0 {
		return 0, false
	}
	return p.atmIV(ctx, underlying, ref, spot)
}

// atmIV finds the ATM call nearest spot in the front DTE window and inverts its mid.
func (p *HistChainProvider) atmIV(ctx context.Context, underlying string, at time.Time, spot float64) (float64, bool) {
	lo := dayFloorLoc(at).AddDate(0, 0, 1)
	hi := dayFloorLoc(at).AddDate(0, 0, 7)
	contracts, err := p.listContracts(ctx, underlying, "call", lo, hi)
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

// memIVStore is an in-memory options.IVStore for DB-free runs and tests: SeedIVHistory
// upserts into it and IVRankService reads from it, with no option_iv_history rows. It
// keeps one entry per ET calendar day (mirroring the store's PK) and filters
// GetIVHistory on the sample instant like the DB reader.
type memIVStore struct {
	mu           sync.Mutex
	byUnderlying map[string][]options.DailyIV
}

// NewMemIVStore builds an empty in-memory IV store.
func NewMemIVStore() *memIVStore {
	return &memIVStore{byUnderlying: map[string][]options.DailyIV{}}
}

func (m *memIVStore) UpsertOptionIV(_ context.Context, underlying string, asOf time.Time, atmIV float64, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := strings.ToUpper(underlying)
	day := asOf.In(etLoc).Format("2006-01-02")
	series := m.byUnderlying[u]
	for i := range series { // one row per ET day — replace if present
		if series[i].Date.In(etLoc).Format("2006-01-02") == day {
			series[i] = options.DailyIV{Date: asOf, ATMIV: atmIV}
			return nil
		}
	}
	series = append(series, options.DailyIV{Date: asOf, ATMIV: atmIV})
	sort.Slice(series, func(i, j int) bool { return series[i].Date.Before(series[j].Date) })
	m.byUnderlying[u] = series
	return nil
}

func (m *memIVStore) GetIVHistory(_ context.Context, underlying string, from, to time.Time) ([]options.DailyIV, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
