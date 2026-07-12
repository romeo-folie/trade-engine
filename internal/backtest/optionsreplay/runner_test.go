package optionsreplay

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/options"
)

// fakeHist is a deterministic synthetic options market. It prices every contract via
// Black-Scholes at a per-day vol (so ImpliedVol recovers it and the IV-rank window
// has spread), and generates an underlying day with a flat opening range then an
// upward breakout.
type fakeHist struct {
	volAt    func(time.Time) float64 // vol used to price option quotes at a time (nil → 0.25)
	optClose float64                 // option bar close used along the exit path (mid proxy)
}

func (f fakeHist) vol(at time.Time) float64 {
	if f.volAt == nil {
		return 0.25
	}
	return f.volAt(at)
}

const fhSpot = 100.0    // range-session underlying price
const fhBreakout = 102.0 // post-breakout underlying price (crosses the buy entry stop)

// priceAt is the fake's underlying price at any instant: flat at 100 through the
// 09:30–09:45 opening range, then 102 for the rest of the session. QuoteAt prices
// options at THIS spot so the chain provider's BS inversion (which is told the
// breakout spot via context) is self-consistent.
func priceAt(ts time.Time) float64 {
	et := ts.In(etLoc)
	if et.Hour()*60+et.Minute() >= 9*60+46 { // 09:46 = minute 16 after the open
		return fhBreakout
	}
	return fhSpot
}

// underlying day: 09:30–09:45 ET flat ~100 (the range), a step up to 102 that crosses
// the buy entry stop, then holds to the session-end flatten (bars run past 15:50 so
// the exit walk reaches the EOD session-ending bar).
func (f fakeHist) UnderlyingBars(_ context.Context, _ string, from, to time.Time) ([]data.Candle, error) {
	var out []data.Candle
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		if wd := day.In(etLoc).Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		d := day.In(etLoc)
		base := time.Date(d.Year(), d.Month(), d.Day(), 9, 30, 0, 0, etLoc)
		for m := range 390 { // 09:30 → ~15:59, past the 15:50 flatten
			ts := base.Add(time.Duration(m) * time.Minute)
			// Respect the requested [from,to] window like the real Alpaca client does —
			// so a caller that asks for an out-of-session window (e.g. a UTC-midnight
			// IV sample) correctly gets nothing, exercising the as-of anchoring.
			if ts.Before(from) || ts.After(to) {
				continue
			}
			px := priceAt(ts)
			out = append(out, data.Candle{Timestamp: ts, Open: px, High: px + 0.5, Low: px - 0.5, Close: px})
		}
	}
	return out, nil
}

func (f fakeHist) ListContracts(_ context.Context, underlying, right string, expiryGTE, _ time.Time) ([]execution.OptionContract, error) {
	rt := "C"
	if right == "put" {
		rt = "P"
	}
	exp := dayFloorLoc(expiryGTE).AddDate(0, 0, 2) // ~3 DTE
	var out []execution.OptionContract
	for _, k := range []float64{98, 99, 100, 101, 102} {
		oc := execution.OptionContract{Underlying: underlying, Expiry: exp, Strike: k, Right: rt, Multiplier: 100}
		oc.OCCSymbol = execution.OCCSymbol(oc)
		out = append(out, oc)
	}
	return out, nil
}

func (f fakeHist) QuoteAt(_ context.Context, occSymbol string, asOf time.Time) (float64, float64, bool) {
	oc, err := execution.ParseOCCSymbol(occSymbol)
	if err != nil {
		return 0, 0, false
	}
	t := yearsToExpiry(asOf, oc.Expiry)
	mid := options.BSPrice(oc.Right, priceAt(asOf), oc.Strike, t, 0, 0, f.vol(asOf))
	if mid <= 0 {
		return 0, 0, false
	}
	half := mid * 0.01 // 2% spread → under the 5% ceiling
	return mid - half, mid + half, true
}

func (f fakeHist) OptionBars(_ context.Context, occSymbol string, from, to time.Time) ([]data.Candle, error) {
	var out []data.Candle
	for ts := from; !ts.After(to); ts = ts.Add(time.Minute) {
		out = append(out, data.Candle{Timestamp: ts, Open: f.optClose, High: f.optClose, Low: f.optClose, Close: f.optClose})
	}
	return out, nil
}

func newTestRunner(t *testing.T, hd HistoricalOptionData) *Runner {
	t.Helper()
	cfg := RunnerConfig{
		Gate:     options.GateConfig{ETFUnderlyings: []string{"SPY"}}, // ETF → skip earnings gate
		Replay:   Config{PremiumBudget: 1000, TimeExitMinutes: 0},
		Assemble: AssembleConfig{RangeMinutes: 15, EntryWindowMins: 60, RiskReward: 2},
		IVLookbackDays: 20,
		IVMinSamples:   5,
		IVRefMinuteET:  15,
	}
	from := time.Date(2026, 3, 2, 0, 0, 0, 0, etLoc)
	to := time.Date(2026, 3, 6, 0, 0, 0, 0, etLoc)
	r, err := NewRunner(context.Background(), hd, []string{"SPY"}, from, to, cfg, nil)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

func TestRunner_EndToEnd(t *testing.T) {
	// Vol falls as the date advances, so the breakout day sits near the BOTTOM of its
	// trailing IV window → a low, sub-ceiling IV rank (a flat series would be rank-
	// undefined and fail closed — see the seeding note). Option rises to $3 → a win.
	hd := fakeHist{volAt: func(ts time.Time) float64 { return 0.40 - 0.003*float64(ts.YearDay()) }, optClose: 3.0}
	r := newTestRunner(t, hd)

	from := time.Date(2026, 3, 4, 0, 0, 0, 0, etLoc)
	to := time.Date(2026, 3, 4, 23, 0, 0, 0, etLoc)
	trades, err := r.RunUnderlying(context.Background(), "SPY", from, to)
	if err != nil {
		t.Fatalf("RunUnderlying: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("expected 1 breakout signal, got %d", len(trades))
	}
	tr := trades[0]
	if !tr.Accepted {
		t.Fatalf("expected accepted trade; aborted at gate %q (%s)", tr.Gate, tr.Reason)
	}
	if tr.Contracts < 1 {
		t.Errorf("expected ≥1 contract, got %d", tr.Contracts)
	}
	// Entry mid ~ BS ATM call price (~$1); option rose to $3 → a real, priced win.
	if !tr.PriceKnown {
		t.Errorf("expected a priced exit (option bars supplied)")
	}
	if tr.PnL <= 0 {
		t.Errorf("expected positive P&L on a rising option, got %.2f", tr.PnL)
	}
	rep := Aggregate(trades)
	if rep.Accepted != 1 || rep.Wins != 1 {
		t.Errorf("report = %+v; want 1 accepted, 1 win", rep)
	}
}

func TestHistChainProvider_DerivesGreeks(t *testing.T) {
	p := NewHistChainProvider(fakeHist{}, 0, 0) // default vol 0.25
	asOf := time.Date(2026, 3, 4, 9, 45, 0, 0, etLoc)
	ctx := WithAsOf(context.Background(), asOf, fhSpot)
	snaps, err := p.GetOptionChain(ctx, "SPY", execution.OptionChainFilter{
		Right: "call", ExpiryGTE: asOf, ExpiryLTE: asOf.AddDate(0, 0, 5),
	})
	if err != nil {
		t.Fatalf("GetOptionChain: %v", err)
	}
	if len(snaps) == 0 {
		t.Fatal("expected snapshots")
	}
	for _, s := range snaps {
		if s.Delta == nil || s.IV == nil {
			t.Errorf("%s: expected derived delta+IV, got delta=%v iv=%v", s.OCCSymbol, s.Delta, s.IV)
			continue
		}
		if *s.IV < 0.2 || *s.IV > 0.3 {
			t.Errorf("%s: IV %.3f not near seeded 0.25", s.OCCSymbol, *s.IV)
		}
	}
}

func TestGetOptionChain_RequiresAsOf(t *testing.T) {
	p := NewHistChainProvider(fakeHist{}, 0, 0)
	// No WithAsOf on the context → must error rather than silently price at zero time.
	_, err := p.GetOptionChain(context.Background(), "SPY", execution.OptionChainFilter{Right: "call"})
	if err == nil {
		t.Fatal("expected error when as-of is missing from context")
	}
}

func TestBuildIVSeries(t *testing.T) {
	p := NewHistChainProvider(fakeHist{volAt: func(time.Time) float64 { return 0.30 }}, 0, 0)
	from := time.Date(2026, 3, 2, 0, 0, 0, 0, etLoc)
	to := time.Date(2026, 3, 13, 0, 0, 0, 0, etLoc) // ~2 weeks → ~10 weekdays
	series, err := p.BuildIVSeries(context.Background(), "SPY", from, to, 15)
	if err != nil {
		t.Fatalf("BuildIVSeries: %v", err)
	}
	if len(series) < 8 {
		t.Fatalf("expected ~10 weekday IV samples, got %d", len(series))
	}
	for _, d := range series {
		if d.ATMIV < 0.25 || d.ATMIV > 0.35 {
			t.Errorf("%s ATM IV %.3f not near seeded 0.30", d.Date.Format("2006-01-02"), d.ATMIV)
		}
	}
}

// TestBuildIVSeries_UTCDates guards the anchoring regression: the CLI parses
// --from/--to with time.Parse, which yields UTC. If BuildIVSeries floored the day in
// that UTC location and sampled at midnight+offset, the ATM quote would land in the
// prior ET evening (market closed) and seed an EMPTY series → every signal fails the
// IV-rank gate. The fake now respects the request window, so this fails if that
// anchoring regresses.
func TestBuildIVSeries_UTCDates(t *testing.T) {
	p := NewHistChainProvider(fakeHist{volAt: func(time.Time) float64 { return 0.30 }}, 0, 0)
	from, _ := time.Parse("2006-01-02", "2026-03-02") // UTC, exactly like the CLI
	to, _ := time.Parse("2006-01-02", "2026-03-13")
	series, err := p.BuildIVSeries(context.Background(), "SPY", from, to, 15)
	if err != nil {
		t.Fatalf("BuildIVSeries: %v", err)
	}
	if len(series) < 8 {
		t.Fatalf("UTC-parsed dates seeded only %d IV days; expected ~10 — as-of anchoring regressed to a closed-market sample window", len(series))
	}
}

func TestAssembleSignals_UpBreakout(t *testing.T) {
	// One ET day: flat 100 for 15m, then breakout up to 102.
	day := time.Date(2026, 3, 4, 0, 0, 0, 0, etLoc)
	base := time.Date(2026, 3, 4, 9, 30, 0, 0, etLoc)
	var candles []data.Candle
	for m := range 120 {
		px := 100.0
		if m >= 16 {
			px = 102
		}
		candles = append(candles, data.Candle{Timestamp: base.Add(time.Duration(m) * time.Minute),
			Open: px, High: px + 0.3, Low: px - 0.3, Close: px})
	}
	sigs := AssembleSignals("SPY", candles, AssembleConfig{RangeMinutes: 15, EntryWindowMins: 60, RiskReward: 2})
	if len(sigs) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(sigs))
	}
	s := sigs[0]
	if s.Direction != "buy" {
		t.Errorf("direction = %q; want buy", s.Direction)
	}
	// Range high ≈ 100.3 → entry ≈ 100.4; stop just below range low ≈ 99.6.
	if s.UnderlyingStop <= 0 || s.UnderlyingStop >= 100 {
		t.Errorf("stop %.2f should sit just below the range low", s.UnderlyingStop)
	}
	if s.UnderlyingTarget <= s.UnderlyingStop {
		t.Errorf("target %.2f must exceed stop %.2f for a long", s.UnderlyingTarget, s.UnderlyingStop)
	}
	if !s.AsOf.After(base) || len(s.Path) == 0 {
		t.Errorf("expected a breakout after the range with a non-empty path")
	}
	_ = day
}

// TestAssembleSignals_EntryDeadlineFromOpen: a breakout that fires AFTER session
// open + EntryWindowMins must be rejected — the live scheduler cancels unfilled
// entries at open+60m, so replaying a later move would trade something production
// never held. The deadline is measured from 09:30, NOT from the range lock.
func TestAssembleSignals_EntryDeadlineFromOpen(t *testing.T) {
	base := time.Date(2026, 3, 4, 9, 30, 0, 0, etLoc)
	var candles []data.Candle
	for m := range 130 { // 09:30 → 11:39
		px := 100.0
		if m >= 70 { // 10:40 ET — past the 10:30 (open+60m) cutoff
			px = 103
		}
		candles = append(candles, data.Candle{Timestamp: base.Add(time.Duration(m) * time.Minute),
			Open: px, High: px + 0.3, Low: px - 0.3, Close: px})
	}
	sigs := AssembleSignals("SPY", candles, AssembleConfig{RangeMinutes: 15, EntryWindowMins: 60, RiskReward: 2})
	if len(sigs) != 0 {
		t.Fatalf("expected 0 signals (breakout at 10:40 is past the open+60m deadline), got %d", len(sigs))
	}
}
