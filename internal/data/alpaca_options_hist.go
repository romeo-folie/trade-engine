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
// OPRA snapshots; the backtest reconstructs the past from Alpaca endpoints:
//
//   - contract listing  → trading API  /v2/options/contracts   (metadata)
//   - historical bars   → data API     /v1beta1/options/bars    (price + exit path)
//
// FREE-DATA MODE (no OPRA subscription): Alpaca serves historical option BARS and
// TRADES on the basic plan but NOT historical NBBO quotes (/options/quotes 404s
// without Algo Trader Plus). So QuoteAt/QuotesAt reconstruct a synthetic bid/ask from
// the option's last-trade price (the 1-minute bar close) bracketed by a configurable
// MODELED spread (modeledSpreadPct, full width as a fraction of the mid). The mid is
// therefore a last-trade proxy, and the spread gate operates on a modeled — not real —
// spread. This is the deliberate free-data trade-off (see docs/options-orb-backtest.md);
// the exit models a market-sell receiving the bid via the same spread.
//
// Underlying equity bars come from the existing AlpacaClient. Every read is bounded by
// its request's start/end so it cannot return post-as-of data. Alpaca option history
// only reaches back to ~Feb 2024; earlier windows return empty and HTTP errors are
// logged, never fabricated.
type AlpacaOptionsHist struct {
	apiKey           string
	secret           string
	dataURL          string // e.g. https://data.alpaca.markets
	tradingURL       string // e.g. https://api.alpaca.markets (or paper-api for paper keys)
	modeledSpreadPct float64 // full modeled bid/ask width as a fraction of the mid (0 = none)
	equity           *AlpacaClient
}

// NewAlpacaOptionsHist builds the client. dataURL/tradingURL default to the live
// hosts when empty. modeledSpreadPct is the synthetic bid/ask width (fraction of mid)
// used to reconstruct quotes from bars; 0 means bid=ask=last-trade (no modeled spread).
func NewAlpacaOptionsHist(apiKey, secret, dataURL, tradingURL string, modeledSpreadPct float64) *AlpacaOptionsHist {
	if dataURL == "" {
		dataURL = "https://data.alpaca.markets"
	}
	if tradingURL == "" {
		tradingURL = "https://api.alpaca.markets"
	}
	if modeledSpreadPct < 0 {
		modeledSpreadPct = 0
	}
	return &AlpacaOptionsHist{
		apiKey:           apiKey,
		secret:           secret,
		dataURL:          dataURL,
		tradingURL:       tradingURL,
		modeledSpreadPct: modeledSpreadPct,
		equity:           NewAlpacaClient(apiKey, secret, dataURL),
	}
}

// modeledNBBO brackets a last-trade price with the configured modeled spread: the mid
// is the price, and bid/ask sit a half-spread either side. A price ≤ 0 yields (0,0).
func (a *AlpacaOptionsHist) modeledNBBO(price float64) execution.OptionNBBO {
	if price <= 0 {
		return execution.OptionNBBO{}
	}
	half := price * a.modeledSpreadPct / 2
	return execution.OptionNBBO{Bid: price - half, Ask: price + half}
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

// ── historical quotes reconstructed from bars (data API) ─────────────────────

// priceLookback is how far before asOf QuoteAt/QuotesAt search for the last-trade
// price (the most recent 1-minute bar close).
const priceLookback = 15 * time.Minute

// maxOptionBarPages bounds the batched bar pagination so a busy chain (many symbols ×
// many 1-minute bars in the lookback window) can't spin forever.
const maxOptionBarPages = 50

// maxSymbolsPerBarRequest is Alpaca's cap on the multi-symbol `symbols` list for the
// option bars endpoint. Larger requests 400.
const maxSymbolsPerBarRequest = 100

// QuoteAt returns a modeled bid/ask at or before asOf for one OCC symbol. Because the
// account has no historical NBBO quotes, it takes the most recent 1-minute bar close
// in [asOf-priceLookback, asOf] as the last-trade price and brackets it with the
// modeled spread. ok is false when no bar exists in the window.
func (a *AlpacaOptionsHist) QuoteAt(ctx context.Context, occSymbol string, asOf time.Time) (float64, float64, bool) {
	out := make(map[string]execution.OptionNBBO, 1)
	if err := a.barsToNBBOChunk(ctx, []string{occSymbol}, asOf, out); err != nil {
		slog.Warn("alpaca options: historical bar fetch failed", "occ", occSymbol, "as_of", asOf, "err", err)
		return 0, 0, false
	}
	q, ok := out[occSymbol]
	if !ok || q.Bid <= 0 || q.Ask <= 0 {
		return 0, 0, false
	}
	return q.Bid, q.Ask, true
}

// QuotesAt returns a modeled bid/ask at or before asOf for MANY OCC symbols by
// batch-fetching each contract's most recent 1-minute bar close (its last-trade price)
// and bracketing it with the modeled spread. It chunks to Alpaca's 100-symbol cap and
// pages each chunk (the total-row `limit` is shared across symbols, so one busy
// contract can push others onto later pages). A symbol with no bar in the window is
// excluded — fail-closed, matching the gate's treatment of a missing contract.
func (a *AlpacaOptionsHist) QuotesAt(ctx context.Context, occSymbols []string, asOf time.Time) (map[string]execution.OptionNBBO, error) {
	out := make(map[string]execution.OptionNBBO)
	if len(occSymbols) == 0 {
		return out, nil
	}
	for i := 0; i < len(occSymbols); i += maxSymbolsPerBarRequest {
		end := min(i+maxSymbolsPerBarRequest, len(occSymbols))
		if err := a.barsToNBBOChunk(ctx, occSymbols[i:end], asOf, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// barsToNBBOChunk fetches 1-minute bars for one ≤100-symbol chunk over
// [asOf-priceLookback, asOf] (newest-first, paged), takes each symbol's newest bar
// close as its last-trade price, and writes the modeled bid/ask into out.
func (a *AlpacaOptionsHist) barsToNBBOChunk(ctx context.Context, chunk []string, asOf time.Time, out map[string]execution.OptionNBBO) error {
	remaining := make(map[string]bool, len(chunk))
	for _, s := range chunk {
		remaining[s] = true
	}
	params := url.Values{}
	params.Set("symbols", strings.Join(chunk, ","))
	params.Set("timeframe", "1Min")
	params.Set("start", asOf.Add(-priceLookback).UTC().Format(time.RFC3339))
	params.Set("end", asOf.UTC().Format(time.RFC3339))
	params.Set("sort", "desc")
	params.Set("limit", "1000")
	for range maxOptionBarPages {
		var resp alpacaOptBarsResponse
		if err := a.getJSON(ctx, a.dataURL, "/v1beta1/options/bars", params, &resp); err != nil {
			slog.Warn("alpaca options: batch historical bars failed", "n", len(chunk), "as_of", asOf, "err", err)
			return err
		}
		for sym, bars := range resp.Bars {
			if !remaining[sym] || len(bars) == 0 {
				continue
			}
			// Bars are stamped at their START and desc-sorted, so bars[0] may be the
			// in-progress minute [asOf, asOf+1m) whose close reflects trades AFTER asOf —
			// a lookahead. Take the newest COMPLETED bar (ts+1m ≤ asOf) instead. Only
			// decide (delete) the symbol once such a bar is found, so if this page held
			// only the in-progress bar, a later page's older bars can still resolve it.
			for _, b := range bars {
				ts, err := time.Parse(time.RFC3339, b.Timestamp)
				if err != nil {
					continue
				}
				if ts.Add(time.Minute).After(asOf) {
					continue // in-progress / future bar — its close peeks past asOf
				}
				delete(remaining, sym) // newest completed bar for this symbol — decide here
				if b.Close > 0 {
					out[sym] = a.modeledNBBO(b.Close)
				}
				break
			}
		}
		if resp.NextPageToken == "" || len(remaining) == 0 {
			break
		}
		params.Set("page_token", resp.NextPageToken)
	}
	return nil
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
