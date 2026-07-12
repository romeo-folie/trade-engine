package optionsreplay

import (
	"context"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/execution"
)

// HistoricalOptionData is the historical-market-data seam the runnable backtest
// needs but production never did (live uses real-time snapshots). It is the
// concrete I/O that was deferred through the epic: a real implementation
// (internal/data.AlpacaOptionsHist) fetches OPRA option bars/quotes + the equity
// underlying bars, and tests supply fakes.
//
// Every method is strictly AS-OF: it must not return data timestamped after the
// requested instant/range, or the backtest would peek the future and overstate
// performance. Implementations enforce this at the API-parameter level (Alpaca's
// end/asof bounds) rather than filtering after the fact.
type HistoricalOptionData interface {
	// ListContracts returns the option contracts for underlying whose expiry falls in
	// [expiryGTE, expiryLTE] for the given right ("call"/"put"). Contract identity is
	// static, so this is not itself time-sensitive; the as-of discipline is enforced
	// by the quote/bar reads that price them.
	ListContracts(ctx context.Context, underlying, right string, expiryGTE, expiryLTE time.Time) ([]execution.OptionContract, error)

	// QuoteAt returns the NBBO bid/ask for one OCC symbol at the most recent quote
	// at or before asOf. ok is false when no quote exists in the lookback window
	// (the chain gate then never sees this contract — fail-closed, like live).
	QuoteAt(ctx context.Context, occSymbol string, asOf time.Time) (bid, ask float64, ok bool)

	// OptionBars returns 1-minute OHLC bars for one OCC symbol over [from, to]. Used
	// to build the exit-walk option-mid path once a contract is chosen.
	OptionBars(ctx context.Context, occSymbol string, from, to time.Time) ([]data.Candle, error)

	// UnderlyingBars returns 1-minute OHLC bars for the equity underlying over
	// [from, to]. These drive opening-range + breakout assembly (reusing the live
	// ORB range/bracket functions) and supply the exit-walk underlying prices.
	UnderlyingBars(ctx context.Context, underlying string, from, to time.Time) ([]data.Candle, error)
}

// asOfContext carries the point-in-time the gate's chain provider must price at.
// The gate's ChainProvider.GetOptionChain signature has no as-of parameter (live is
// always "now"), so the backtest threads it through the context the gate already
// forwards to the provider — no interface change, no lookahead.
type asOfContext struct {
	at   time.Time
	spot float64 // underlying price at `at`, for Black-Scholes IV/delta derivation
}

type asOfKey struct{}

// WithAsOf stamps the context with the backtest evaluation instant and the
// underlying spot at that instant. The runner sets this before every gate call.
func WithAsOf(ctx context.Context, at time.Time, spot float64) context.Context {
	return context.WithValue(ctx, asOfKey{}, asOfContext{at: at, spot: spot})
}

func asOfFrom(ctx context.Context) (asOfContext, bool) {
	v, ok := ctx.Value(asOfKey{}).(asOfContext)
	return v, ok
}
