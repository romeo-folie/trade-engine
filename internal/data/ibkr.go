package data

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/ibkrconids"
)

// ibkrTimeframes maps our internal Timeframe constants to IBKR's bar size format.
var ibkrTimeframes = map[Timeframe]string{
	Timeframe1Min:  "1min",
	Timeframe5Min:  "5min",
	Timeframe15Min: "15min",
	Timeframe1Hour: "1h",
	Timeframe1Day:  "1d",
}

// ibkrTimeframeDurations maps timeframes to their duration for period calculation.
var ibkrTimeframeDurations = map[Timeframe]time.Duration{
	Timeframe1Min:  time.Minute,
	Timeframe5Min:  5 * time.Minute,
	Timeframe15Min: 15 * time.Minute,
	Timeframe1Hour: time.Hour,
	Timeframe1Day:  24 * time.Hour,
}

// Well-known IBKR contract IDs for major forex pairs and CFD instruments.
// The authoritative map lives in internal/ibkrconids and is shared with the
// execution package to avoid duplication.
var ibkrConids = ibkrconids.Conids

// IBKRClient implements MarketDataProvider using the IBKR Client Portal REST API.
// The Client Portal gateway uses cookie-based SSO — no API key headers are needed.
// IBKR rate limit: 10 req/sec → 1 req per 100ms.
type IBKRClient struct {
	gatewayURL  string
	httpClient  *http.Client
	mu          sync.Mutex
	lastRequest time.Time
	minInterval time.Duration // 100ms for 10 req/sec

	futConidMu    sync.Mutex
	futConidCache map[string]string // "FUT:SYMBOL" → conid, for dynamically-resolved futures

	fxConidMu    sync.Mutex
	fxConidCache map[string]string // "FX:BASE/QUOTE" → conid, for dynamically-resolved IDEALPRO pairs
}

// NewIBKRClient creates a new IBKR Client Portal market data client.
// Set insecureTLS=true when the gateway uses a self-signed certificate (default IBKR setup).
func NewIBKRClient(gatewayURL string, insecureTLS bool) *IBKRClient {
	transport := &http.Transport{}
	if insecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return &IBKRClient{
		gatewayURL:    strings.TrimRight(gatewayURL, "/"),
		httpClient:    &http.Client{Timeout: 30 * time.Second, Transport: transport},
		minInterval:   100 * time.Millisecond,
		futConidCache: make(map[string]string),
		fxConidCache:  make(map[string]string),
	}
}

// ── Rate limiter ─────────────────────────────────────────────────────────────

// throttle enforces the per-client rate limit. It returns ctx.Err() immediately
// if the context is cancelled while waiting.
func (c *IBKRClient) throttle(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	since := time.Since(c.lastRequest)
	if since < c.minInterval {
		remaining := c.minInterval - since
		select {
		case <-time.After(remaining):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.lastRequest = time.Now()
	return nil
}

// ── HTTP helper ───────────────────────────────────────────────────────────────

// get performs a throttled HTTP GET against the IBKR gateway. Authentication is
// handled by the gateway's cookie-based SSO — no API key headers are required.
func (c *IBKRClient) get(ctx context.Context, path string, params url.Values) (*http.Response, error) {
	if err := c.throttle(ctx); err != nil {
		return nil, err
	}
	u := c.gatewayURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	return withRetry(ctx, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return nil, &httpError{
				status: resp.StatusCode,
				msg:    fmt.Sprintf("ibkr API %s %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body))),
			}
		}
		return resp, nil
	})
}

// ── Contract resolution ───────────────────────────────────────────────────────

// resolveConid returns the IBKR contract ID (conid) for the given symbol.
//
// Resolution order mirrors the broker-side resolver (execution.IBKRBroker) so the
// market-data and order paths agree on which contract a symbol maps to:
//  1. Forex/CMDTY static map (ibkrConids) — spot forex + London metals.
//  2. Futures static map (FuturesConids) — front-month, quarterly then monthly.
//  3. Session cache of dynamically-resolved IDEALPRO forex conids.
//  4. Dynamic /iserver/secdef/search lookup for slash-delimited CASH pairs.
//  5. Session cache of dynamically-resolved futures conids.
//  6. Dynamic /trsrv/futures lookup for futures symbols missing from the map
//     (every micro before this map is refreshed, and any symbol after it goes
//     stale) — without this, FetchCandles fails with "unknown symbol" and the
//     ORB range watch can never collect candles for the instrument.
func (c *IBKRClient) resolveConid(ctx context.Context, pair string) (string, error) {
	if conid, ok := ibkrConids[pair]; ok {
		return conid, nil
	}
	if strings.Contains(pair, "/") {
		return c.resolveConidForexDynamic(ctx, pair)
	}
	for _, cycle := range []string{"quarterly", "monthly"} {
		if conid, ok := ibkrconids.FuturesConids[ibkrconids.FrontMonthKey(pair, cycle, time.Now())]; ok {
			return conid, nil
		}
	}

	cacheKey := "FUT:" + pair
	c.futConidMu.Lock()
	if conid, ok := c.futConidCache[cacheKey]; ok {
		c.futConidMu.Unlock()
		return conid, nil
	}
	c.futConidMu.Unlock()

	return c.resolveConidFuturesDynamic(ctx, pair, cacheKey)
}

type ibkrSecdefSearchResult struct {
	Conid       ibkrconids.Conid `json:"conid"`
	CompanyName string           `json:"companyName"`
	Description string           `json:"description"`
	Exchange    string           `json:"exchange"`
}

func (c *IBKRClient) resolveConidForexDynamic(ctx context.Context, pair string) (string, error) {
	cacheKey := "FX:" + pair
	c.fxConidMu.Lock()
	if conid, ok := c.fxConidCache[cacheKey]; ok {
		c.fxConidMu.Unlock()
		return conid, nil
	}
	c.fxConidMu.Unlock()

	parts := strings.SplitN(pair, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("ibkr: invalid forex pair %q: expected BASE/QUOTE", pair)
	}

	resp, err := c.get(ctx, "/v1/api/iserver/secdef/search", url.Values{
		"symbol":  {parts[0]},
		"secType": {"CASH"},
	})
	if err != nil {
		return "", fmt.Errorf("ibkr: resolve forex conid for %q: %w", pair, err)
	}
	defer resp.Body.Close()

	var rawResults []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&rawResults); err != nil {
		return "", fmt.Errorf("ibkr: decode secdef/search for %q: %w", pair, err)
	}

	companyName := strings.Replace(pair, "/", ".", 1)
	for _, raw := range rawResults {
		var r ibkrSecdefSearchResult
		if err := json.Unmarshal(raw, &r); err != nil {
			// IBKR may include unrelated or partially populated contracts. Ignore
			// malformed rows and continue looking for the requested IDEALPRO pair.
			continue
		}
		if r.Conid <= 0 {
			continue
		}
		if r.Exchange == "IDEALPRO" && (r.Description == pair || r.CompanyName == companyName) {
			conid := strconv.Itoa(int(r.Conid))
			c.fxConidMu.Lock()
			c.fxConidCache[cacheKey] = conid
			c.fxConidMu.Unlock()
			return conid, nil
		}
	}

	return "", fmt.Errorf("ibkr: forex pair %q not found on IDEALPRO", pair)
}

// ibkrTrsrvFuture is one contract from /trsrv/futures. expirationDate and ltd are
// integer YYYYMMDD dates, not strings.
type ibkrTrsrvFuture struct {
	Conid          int `json:"conid"`
	ExpirationDate int `json:"expirationDate"`
	LTD            int `json:"ltd"`
}

// resolveConidFuturesDynamic resolves a futures symbol to its nearest front-month
// conid via /trsrv/futures (which accepts a bare symbol), caching the result.
func (c *IBKRClient) resolveConidFuturesDynamic(ctx context.Context, symbol, cacheKey string) (string, error) {
	resp, err := c.get(ctx, "/v1/api/trsrv/futures", url.Values{"symbols": {symbol}})
	if err != nil {
		return "", fmt.Errorf("ibkr: resolve futures conid for %q: %w", symbol, err)
	}
	defer resp.Body.Close()

	var chains map[string][]ibkrTrsrvFuture
	if err := json.NewDecoder(resp.Body).Decode(&chains); err != nil {
		return "", fmt.Errorf("ibkr: decode trsrv/futures for %q: %w", symbol, err)
	}
	contracts := chains[symbol]
	if len(contracts) == 0 {
		for _, v := range chains {
			if len(v) > 0 {
				contracts = v
				break
			}
		}
	}

	today, _ := strconv.Atoi(time.Now().Format("20060102"))
	var bestConid, bestExp int
	for _, ct := range contracts {
		exp := ct.LTD
		if exp == 0 {
			exp = ct.ExpirationDate
		}
		if ct.Conid <= 0 || exp < today {
			continue
		}
		if bestExp == 0 || exp < bestExp {
			bestExp, bestConid = exp, ct.Conid
		}
	}
	if bestConid == 0 {
		return "", fmt.Errorf("ibkr: unknown symbol %q (not in static conid map, no futures contract found)", symbol)
	}

	conid := strconv.Itoa(bestConid)
	c.futConidMu.Lock()
	c.futConidCache[cacheKey] = conid
	c.futConidMu.Unlock()
	return conid, nil
}

// ── Period helper ─────────────────────────────────────────────────────────────

// computePeriod returns the IBKR history period string that covers the given
// time range. IBKR accepts: 1d, 2d, 3d, 5d, 1w, 1m, 1y.
func computePeriod(from, to time.Time) string {
	hours := to.Sub(from).Hours()
	switch {
	case hours <= 24:
		return "1d"
	case hours <= 48:
		return "2d"
	case hours <= 72:
		return "3d"
	case hours <= 120:
		return "5d"
	case hours <= 168:
		return "1w"
	case hours <= 720:
		return "1m"
	default:
		return "1y"
	}
}

// ── IBKR response types ───────────────────────────────────────────────────────

// ibkrHistoryResponse is the top-level response from /iserver/marketdata/history.
type ibkrHistoryResponse struct {
	Data []ibkrBar `json:"data"`
}

// ibkrBar is a single OHLCV bar as returned by the IBKR history endpoint.
// The timestamp field "t" is a millisecond epoch integer.
type ibkrBar struct {
	Open      float64 `json:"o"`
	High      float64 `json:"h"`
	Low       float64 `json:"l"`
	Close     float64 `json:"c"`
	Volume    float64 `json:"v"`
	Timestamp int64   `json:"t"`
}

// ibkrSnapshotEntry is one entry in the snapshot array returned by
// /iserver/marketdata/snapshot. Fields 84/85/86 are string-typed price values.
type ibkrSnapshotEntry struct {
	Conid int    `json:"conid"`
	Bid   string `json:"84"`
	Ask   string `json:"85"`
	Last  string `json:"86"`
}

// ── MarketDataProvider implementation ────────────────────────────────────────

// FetchCandles retrieves OHLCV bars for the given forex pair and time range via
// the IBKR history endpoint. Volume is set to 0 — IBKR forex volume is unreliable.
func (c *IBKRClient) FetchCandles(ctx context.Context, ticker string, tf Timeframe, from, to time.Time) ([]Candle, error) {
	conid, err := c.resolveConid(ctx, ticker)
	if err != nil {
		return nil, err
	}

	barSize, ok := ibkrTimeframes[tf]
	if !ok {
		return nil, fmt.Errorf("ibkr: unsupported timeframe: %s", tf)
	}

	period := computePeriod(from, to)

	params := url.Values{}
	params.Set("conid", conid)
	params.Set("period", period)
	params.Set("bar", barSize)
	params.Set("outsideRth", "true")
	// startTime is IBKR's most-recent anchor: the returned window is `period` of bars
	// extending BACKWARD from startTime, i.e. [startTime − period, startTime]. We pass
	// startTime=to and a period sized by computePeriod to cover the span, which yields
	// [to − period, to] ⊇ [from, to] (period ≥ to−from, so the far edge reaches `from`).
	// Without startTime the endpoint anchors at "now", so a request for an OLD [from,to]
	// window returns the most-recent `period` of bars, which the range filter below then
	// discards entirely (they all fall after `to`) — the bug that left old paper trades
	// unresolvable. The format is IBKR's yyyyMMdd-HH:mm:ss in UTC. Existing callers pass
	// to≈now, so for them this is behaviour-preserving (anchor "now" was already implicit).
	params.Set("startTime", to.UTC().Format("20060102-15:04:05"))

	resp, err := c.get(ctx, "/v1/api/iserver/marketdata/history", params)
	if err != nil {
		return nil, fmt.Errorf("fetch candles %s: %w", ticker, err)
	}
	defer resp.Body.Close()

	var data ibkrHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode history response: %w", err)
	}

	candles := make([]Candle, 0, len(data.Data))
	for _, bar := range data.Data {
		// t is millisecond epoch
		ts := time.UnixMilli(bar.Timestamp).UTC()
		// Filter to requested range
		if ts.Before(from) || ts.After(to) {
			continue
		}
		candles = append(candles, Candle{
			Ticker:    ticker,
			Timeframe: tf,
			Timestamp: ts,
			Open:      bar.Open,
			High:      bar.High,
			Low:       bar.Low,
			Close:     bar.Close,
			Volume:    0, // IBKR forex volume is unreliable
			VWAP:      0,
		})
	}
	return candles, nil
}

// FetchQuote retrieves the latest bid/ask/last snapshot for a forex pair.
// IBKR snapshot API has a "priming" quirk: the first request may return empty
// fields. If bid and ask are both 0, the method waits 300ms and retries once.
func (c *IBKRClient) FetchQuote(ctx context.Context, ticker string) (*Quote, error) {
	conid, err := c.resolveConid(ctx, ticker)
	if err != nil {
		return nil, err
	}

	fetchSnapshot := func() (*Quote, error) {
		params := url.Values{}
		params.Set("conids", conid)
		params.Set("fields", "84,85,86")

		resp, err := c.get(ctx, "/v1/api/iserver/marketdata/snapshot", params)
		if err != nil {
			return nil, fmt.Errorf("fetch quote %s: %w", ticker, err)
		}
		defer resp.Body.Close()

		var entries []ibkrSnapshotEntry
		if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
			return nil, fmt.Errorf("decode snapshot response: %w", err)
		}
		if len(entries) == 0 {
			return nil, fmt.Errorf("ibkr: empty snapshot response for %s", ticker)
		}

		entry := entries[0]
		bid, _ := strconv.ParseFloat(entry.Bid, 64)
		ask, _ := strconv.ParseFloat(entry.Ask, 64)
		last, _ := strconv.ParseFloat(entry.Last, 64)

		return &Quote{
			Ticker:    ticker,
			Bid:       bid,
			Ask:       ask,
			Last:      last,
			Timestamp: time.Now(),
		}, nil
	}

	quote, err := fetchSnapshot()
	if err != nil {
		return nil, err
	}

	// Retry once if bid and ask are both 0 — this is the IBKR "priming" behaviour
	// where the first snapshot request subscribes to the feed but returns no data.
	if quote.Bid == 0 && quote.Ask == 0 {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		quote, err = fetchSnapshot()
		if err != nil {
			return nil, err
		}
	}

	return quote, nil
}

// FetchHistory retrieves the most recent N candles for a forex pair.
func (c *IBKRClient) FetchHistory(ctx context.Context, ticker string, tf Timeframe, limit int) ([]Candle, error) {
	dur, ok := ibkrTimeframeDurations[tf]
	if !ok {
		return nil, fmt.Errorf("ibkr: unsupported timeframe: %s", tf)
	}

	now := time.Now()
	from := now.Add(-time.Duration(limit) * dur)

	// Add a small buffer so computePeriod covers the full window even with
	// floating-point rounding near boundary conditions (e.g. exactly 24h).
	buffered := from.Add(-dur)
	candles, err := c.FetchCandles(ctx, ticker, tf, buffered, now)
	if err != nil {
		return nil, fmt.Errorf("fetch history %s: %w", ticker, err)
	}

	// Re-filter to drop the buffer candles that fall outside [from, now].
	var filtered []Candle
	for _, c := range candles {
		if !c.Timestamp.Before(from) {
			filtered = append(filtered, c)
		}
	}

	// Trim to the last `limit` entries if the provider returned more.
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	return filtered, nil
}

// Ensure IBKRClient satisfies the MarketDataProvider interface at compile time.
var _ MarketDataProvider = (*IBKRClient)(nil)
