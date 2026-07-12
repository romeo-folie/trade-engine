package data

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
)

// AlpacaOptionsHist is the concrete historical-options data source the runnable
// options-ORB backtest needs (internal/backtest/optionsreplay.HistoricalOptionData).
// It is the I/O that production never used — live strike selection reads real-time
// OPRA snapshots; the backtest reconstructs the past from three Alpaca endpoints:
//
//   - contract listing  → trading API  /v2/options/contracts        (metadata)
//   - historical quotes → data API     /v1beta1/options/quotes       (NBBO as-of)
//   - historical bars   → data API     /v1beta1/options/bars         (exit path)
//
// Underlying equity bars come from the existing AlpacaClient. Every read is bounded
// by the request's own start/end so it cannot return post-as-of data.
//
// CONSTRAINTS (documented, not silently swallowed): Alpaca option history requires
// an OPRA-entitled data subscription and only reaches back to ~Feb 2024. A missing
// entitlement surfaces as an HTTP error from the underlying calls, which the runner
// logs — it does not fabricate quotes.
type AlpacaOptionsHist struct {
	apiKey     string
	secret     string
	dataURL    string // e.g. https://data.alpaca.markets
	tradingURL string // e.g. https://api.alpaca.markets (or paper-api for paper keys)
	equity     *AlpacaClient
}

// NewAlpacaOptionsHist builds the client. dataURL/tradingURL default to the live
// hosts when empty. The same key/secret authenticate both hosts.
func NewAlpacaOptionsHist(apiKey, secret, dataURL, tradingURL string) *AlpacaOptionsHist {
	if dataURL == "" {
		dataURL = "https://data.alpaca.markets"
	}
	if tradingURL == "" {
		tradingURL = "https://api.alpaca.markets"
	}
	return &AlpacaOptionsHist{
		apiKey:     apiKey,
		secret:     secret,
		dataURL:    dataURL,
		tradingURL: tradingURL,
		equity:     NewAlpacaClient(apiKey, secret, dataURL),
	}
}

// UnderlyingBars returns 1-minute equity bars via the existing equity client.
func (a *AlpacaOptionsHist) UnderlyingBars(ctx context.Context, underlying string, from, to time.Time) ([]Candle, error) {
	return a.equity.FetchCandles(ctx, underlying, Timeframe1Min, from, to)
}

// ── contract listing (trading API) ───────────────────────────────────────────

type alpacaContract struct {
	Symbol         string `json:"symbol"`
	UnderlyingSym  string `json:"underlying_symbol"`
	ExpirationDate string `json:"expiration_date"` // YYYY-MM-DD
	StrikePrice    string `json:"strike_price"`
	Type           string `json:"type"` // "call" | "put"
}

type alpacaContractsResponse struct {
	OptionContracts []alpacaContract `json:"option_contracts"`
	NextPageToken   string           `json:"next_page_token"`
}

// ListContracts pages the trading API for contracts on underlying with expiry in
// [expiryGTE, expiryLTE] and the given right.
//
// It queries BOTH status=active AND status=inactive and merges (dedup by symbol).
// The endpoint defaults to status=active, which Alpaca limits to contracts expiring
// before the coming weekend — so a backtest over a PAST window (whose contracts have
// since expired → inactive) would get an empty list and silently degenerate into
// fail-closed iv_rank/expiry aborts. Requesting both makes the historical universe
// visible regardless of when the backtest is run.
func (a *AlpacaOptionsHist) ListContracts(ctx context.Context, underlying, right string, expiryGTE, expiryLTE time.Time) ([]execution.OptionContract, error) {
	seen := make(map[string]bool)
	var out []execution.OptionContract
	for _, status := range []string{"active", "inactive"} {
		cs, err := a.listContractsStatus(ctx, underlying, right, status, expiryGTE, expiryLTE)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if seen[c.OCCSymbol] {
				continue
			}
			seen[c.OCCSymbol] = true
			out = append(out, c)
		}
	}
	return out, nil
}

// listContractsStatus pages one contract-status universe.
func (a *AlpacaOptionsHist) listContractsStatus(ctx context.Context, underlying, right, status string, expiryGTE, expiryLTE time.Time) ([]execution.OptionContract, error) {
	var out []execution.OptionContract
	pageToken := ""
	for {
		params := url.Values{}
		params.Set("underlying_symbols", strings.ToUpper(underlying))
		params.Set("expiration_date_gte", expiryGTE.Format("2006-01-02"))
		params.Set("expiration_date_lte", expiryLTE.Format("2006-01-02"))
		params.Set("type", right)
		params.Set("status", status)
		params.Set("limit", "1000")
		if pageToken != "" {
			params.Set("page_token", pageToken)
		}
		var resp alpacaContractsResponse
		if err := a.getJSON(ctx, a.tradingURL, "/v2/options/contracts", params, &resp); err != nil {
			return nil, fmt.Errorf("list option contracts %s (%s): %w", underlying, status, err)
		}
		for _, c := range resp.OptionContracts {
			exp, err := time.Parse("2006-01-02", c.ExpirationDate)
			if err != nil {
				continue
			}
			strike, err := strconv.ParseFloat(c.StrikePrice, 64)
			if err != nil {
				continue
			}
			rt := "C"
			if strings.EqualFold(c.Type, "put") {
				rt = "P"
			}
			out = append(out, execution.OptionContract{
				OCCSymbol:  c.Symbol,
				Underlying: strings.ToUpper(underlying),
				Expiry:     exp,
				Strike:     strike,
				Right:      rt,
				Multiplier: 100,
			})
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return out, nil
}

// ── historical quotes (data API) ─────────────────────────────────────────────

type alpacaOptQuote struct {
	Timestamp string  `json:"t"`
	BidPrice  float64 `json:"bp"`
	AskPrice  float64 `json:"ap"`
}

type alpacaOptQuotesResponse struct {
	Quotes        map[string][]alpacaOptQuote `json:"quotes"`
	NextPageToken string                      `json:"next_page_token"`
}

// quoteLookback is how far before asOf QuoteAt searches for the last NBBO.
const quoteLookback = 15 * time.Minute

// maxOptionQuotePages bounds the batched QuotesAt pagination so a busy chain (many
// symbols × many ticks in the lookback window) can't spin forever. 50 pages ×
// limit 1000 = 50k quotes, ample for a whole DTE window over a 15-minute window.
const maxOptionQuotePages = 50

// QuoteAt returns the most recent NBBO at or before asOf for one OCC symbol. It asks
// for the [asOf-quoteLookback, asOf] window sorted DESCENDING with limit=1, so the
// single returned quote is the newest at or before the evaluation instant — never a
// stale first-ascending-page quote (a liquid 15-minute window can exceed one page of
// NBBO updates) and never a quote stamped after asOf (bounded by end=asOf).
func (a *AlpacaOptionsHist) QuoteAt(ctx context.Context, occSymbol string, asOf time.Time) (float64, float64, bool) {
	params := url.Values{}
	params.Set("symbols", occSymbol)
	params.Set("start", asOf.Add(-quoteLookback).UTC().Format(time.RFC3339))
	params.Set("end", asOf.UTC().Format(time.RFC3339))
	params.Set("sort", "desc")
	params.Set("limit", "1")
	var resp alpacaOptQuotesResponse
	if err := a.getJSON(ctx, a.dataURL, "/v1beta1/options/quotes", params, &resp); err != nil {
		// Surface systemic failures (e.g. 403 no OPRA entitlement, 429) — otherwise a
		// blanket auth error is indistinguishable from "no quote in the window" and the
		// backtest silently reports zero candidates with no cause. ok=false either way;
		// we never fabricate a quote.
		slog.Warn("alpaca options: historical quote fetch failed", "occ", occSymbol, "as_of", asOf, "err", err)
		return 0, 0, false
	}
	quotes := resp.Quotes[occSymbol]
	if len(quotes) == 0 {
		return 0, 0, false
	}
	// Newest first with the end bound at asOf; defensively re-check the timestamp so a
	// server that ignores sort/end can never hand us a future quote.
	newest := quotes[0]
	if ts, err := time.Parse(time.RFC3339, newest.Timestamp); err == nil && ts.After(asOf) {
		return 0, 0, false
	}
	if newest.BidPrice <= 0 || newest.AskPrice <= 0 {
		return 0, 0, false
	}
	return newest.BidPrice, newest.AskPrice, true
}

// QuotesAt returns the newest NBBO at or before asOf for MANY OCC symbols, replacing
// an N+1 per-contract fetch with a batched, PAGED request over the window
// [asOf-quoteLookback, asOf] sorted DESCENDING.
//
// Alpaca's multi-symbol quotes endpoint groups results by symbol then orders each
// group by time, and `limit` caps the TOTAL rows across all symbols — so one busy
// at-the-money contract can fill a whole page and push other requested symbols onto
// later pages. A single page would therefore silently drop most of the chain. We
// page (bounded by maxOptionQuotePages) and record each symbol's newest quote the first
// time it appears (its quotes are newest-first within a group, so the first
// occurrence across pages is the global newest), stopping early once every requested
// symbol is resolved. A symbol absent from all pages is excluded — fail-closed, like
// the single QuoteAt.
func (a *AlpacaOptionsHist) QuotesAt(ctx context.Context, occSymbols []string, asOf time.Time) (map[string]execution.OptionNBBO, error) {
	out := make(map[string]execution.OptionNBBO)
	if len(occSymbols) == 0 {
		return out, nil
	}
	remaining := make(map[string]bool, len(occSymbols))
	for _, s := range occSymbols {
		remaining[s] = true
	}
	params := url.Values{}
	params.Set("symbols", strings.Join(occSymbols, ","))
	params.Set("start", asOf.Add(-quoteLookback).UTC().Format(time.RFC3339))
	params.Set("end", asOf.UTC().Format(time.RFC3339))
	params.Set("sort", "desc")
	params.Set("limit", "1000")
	for range maxOptionQuotePages {
		var resp alpacaOptQuotesResponse
		if err := a.getJSON(ctx, a.dataURL, "/v1beta1/options/quotes", params, &resp); err != nil {
			slog.Warn("alpaca options: batch historical quotes failed", "n", len(occSymbols), "as_of", asOf, "err", err)
			return nil, err
		}
		for sym, quotes := range resp.Quotes {
			if !remaining[sym] || len(quotes) == 0 {
				continue
			}
			// First appearance of this symbol across pages = its global newest (desc).
			// Decide the symbol here (newest-or-nothing, matching single QuoteAt), so a
			// later page's older quotes for it are ignored.
			delete(remaining, sym)
			newest := quotes[0]
			if ts, err := time.Parse(time.RFC3339, newest.Timestamp); err == nil && ts.After(asOf) {
				continue
			}
			if newest.BidPrice <= 0 || newest.AskPrice <= 0 {
				continue
			}
			out[sym] = execution.OptionNBBO{Bid: newest.BidPrice, Ask: newest.AskPrice}
		}
		if resp.NextPageToken == "" || len(remaining) == 0 {
			break
		}
		params.Set("page_token", resp.NextPageToken)
	}
	return out, nil
}

// ── historical bars (data API) ───────────────────────────────────────────────

type alpacaOptBarsResponse struct {
	Bars          map[string][]alpacaBar `json:"bars"`
	NextPageToken string                 `json:"next_page_token"`
}

// OptionBars returns 1-minute OHLC bars for one OCC symbol over [from, to], paging
// as needed.
func (a *AlpacaOptionsHist) OptionBars(ctx context.Context, occSymbol string, from, to time.Time) ([]Candle, error) {
	var all []Candle
	pageToken := ""
	for {
		params := url.Values{}
		params.Set("symbols", occSymbol)
		params.Set("timeframe", "1Min")
		params.Set("start", from.UTC().Format(time.RFC3339))
		params.Set("end", to.UTC().Format(time.RFC3339))
		params.Set("limit", "1000")
		if pageToken != "" {
			params.Set("page_token", pageToken)
		}
		var resp alpacaOptBarsResponse
		if err := a.getJSON(ctx, a.dataURL, "/v1beta1/options/bars", params, &resp); err != nil {
			return nil, fmt.Errorf("option bars %s: %w", occSymbol, err)
		}
		for _, b := range resp.Bars[occSymbol] {
			ts, err := time.Parse(time.RFC3339, b.Timestamp)
			if err != nil {
				continue
			}
			all = append(all, Candle{
				Ticker: occSymbol, Timeframe: Timeframe1Min, Timestamp: ts,
				Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume, VWAP: b.VWAP,
			})
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Timestamp.Before(all[j].Timestamp) })
	return all, nil
}

// getJSON issues an authenticated GET against host+path and decodes into out,
// reusing the equity client's shared getFrom (throttle + auth + retry + error
// envelope) rather than re-implementing them.
func (a *AlpacaOptionsHist) getJSON(ctx context.Context, host, path string, params url.Values, out any) error {
	resp, err := a.equity.getFrom(ctx, host, path, params)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}
