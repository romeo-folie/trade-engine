package options

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// IVStore is the persistence the IV pipeline needs: read the history (for ranking)
// and upsert a day's ATM IV (from the forward snapshot or the bounded derivation).
// It composes the IVHistoryReader the gate provider consumes.
type IVStore interface {
	IVHistoryReader
	UpsertOptionIV(ctx context.Context, underlying string, asOf time.Time, atmIV float64, source string) error
}

// storeIVStore adapts the store package to IVStore, keeping store free of any
// options-package types.
type storeIVStore struct{ db *pgxpool.Pool }

// NewStoreIVStore wires the IV pipeline to the real database.
func NewStoreIVStore(db *pgxpool.Pool) IVStore { return storeIVStore{db: db} }

func (s storeIVStore) UpsertOptionIV(ctx context.Context, underlying string, asOf time.Time, atmIV float64, source string) error {
	return store.UpsertOptionIV(ctx, s.db, underlying, asOf, atmIV, source)
}

func (s storeIVStore) GetIVHistory(ctx context.Context, underlying string, from, to time.Time) ([]DailyIV, error) {
	rows, err := store.GetOptionIVHistory(ctx, s.db, underlying, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]DailyIV, len(rows))
	for i, r := range rows {
		out[i] = DailyIV{Date: r.Date, ATMIV: r.ATMIV}
	}
	return out, nil
}

// atmIVFromChain returns the implied volatility of the nearest-to-the-money
// contract that actually carries an IV, and ok=false when no snapshot has one
// (so the caller records nothing rather than a bogus zero).
func atmIVFromChain(snaps []execution.OptionSnapshot, spot float64) (float64, bool) {
	var best float64
	var bestDist = math.Inf(1)
	found := false
	for _, s := range snaps {
		if s.IV == nil {
			continue
		}
		oc, err := execution.ParseOCCSymbol(s.OCCSymbol)
		if err != nil {
			continue
		}
		dist := math.Abs(oc.Strike - spot)
		if dist < bestDist {
			bestDist = dist
			best = *s.IV
			found = true
		}
	}
	return best, found
}

// RecordLiveATMIV is the forward-accumulation path: it reads the ATM IV straight
// from a live chain snapshot (no Black-Scholes needed — Alpaca serves IV live) and
// persists it as that day's point. Run daily so the IV-Rank window keeps growing.
func RecordLiveATMIV(ctx context.Context, ivstore IVStore, underlying string, asOf time.Time, spot float64, snaps []execution.OptionSnapshot) error {
	iv, ok := atmIVFromChain(snaps, spot)
	if !ok {
		return fmt.Errorf("options: no ATM IV in live chain for %s", underlying)
	}
	// Persist at the precise snapshot instant (not day-floored) so the as-of filter is
	// lookahead-safe: a same-day intraday breakout before this snapshot won't see it.
	return ivstore.UpsertOptionIV(ctx, underlying, asOf, iv, "live")
}

// ─── Bounded up-front derivation ────────────────────────────────────────────

// HistoricalATMQuote is one historical near-ATM option observation, enough to
// invert a Black-Scholes IV: the option's mid price plus the pricing inputs.
type HistoricalATMQuote struct {
	Date      time.Time
	Right     string  // "C" | "P"
	Spot      float64 // underlying close that day
	Strike    float64
	OptionMid float64 // option mid/close price that day
	DTEYears  float64 // time to expiry in years at that date
}

// HistoricalQuoteProvider yields historical near-ATM option observations for an
// underlying over a date range. The concrete Alpaca implementation (fetching
// historical option bars + underlying closes and selecting the nearest strike per
// day) is the remaining I/O integration; the derivation orchestration below is
// provider-agnostic and fully tested against a fake.
type HistoricalQuoteProvider interface {
	ATMQuotes(ctx context.Context, underlying string, from, to time.Time) ([]HistoricalATMQuote, error)
}

// BackfillIVHistory is the bounded up-front derivation that seeds the IV-Rank
// series so the fail-closed gate has a real value on day one (resolving the
// cold-start conflict). For each historical near-ATM quote it inverts a
// Black-Scholes IV and upserts it as a 'derived' point. Quotes whose price is not
// invertible (outside the no-arbitrage band) are skipped, not faked. Returns the
// number of days recorded. r and q are the risk-free rate and dividend yield.
func BackfillIVHistory(ctx context.Context, ivstore IVStore, hp HistoricalQuoteProvider, underlying string, from, to time.Time, r, q float64) (int, error) {
	quotes, err := hp.ATMQuotes(ctx, underlying, from, to)
	if err != nil {
		return 0, fmt.Errorf("options: backfill %s: %w", underlying, err)
	}
	recorded := 0
	for _, qt := range quotes {
		iv, ok := ImpliedVol(qt.Right, qt.OptionMid, qt.Spot, qt.Strike, qt.DTEYears, r, q)
		if !ok {
			continue // non-invertible price — skip rather than store a bogus IV
		}
		if err := ivstore.UpsertOptionIV(ctx, underlying, qt.Date, iv, "derived"); err != nil {
			return recorded, fmt.Errorf("options: backfill %s upsert %s: %w", underlying, qt.Date.Format("2006-01-02"), err)
		}
		recorded++
	}
	return recorded, nil
}
