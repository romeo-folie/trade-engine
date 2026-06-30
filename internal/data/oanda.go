package data

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// oandaGranularities maps our internal Timeframe constants to OANDA v20 granularities.
// OANDA serves deep intraday forex/metals history (years of M1), which IBKR's Client
// Portal does not — this is the data source for the ORB backtest engine.
var oandaGranularities = map[Timeframe]string{
	Timeframe1Min:  "M1",
	Timeframe5Min:  "M5",
	Timeframe15Min: "M15",
	Timeframe1Hour: "H1",
	Timeframe4Hour: "H4",
	Timeframe1Day:  "D",
}

// oandaGranularityDuration is the wall-clock span of one bar per timeframe, used to
// page through ranges larger than OANDA's 5000-candle response cap.
var oandaGranularityDuration = map[Timeframe]time.Duration{
	Timeframe1Min:  time.Minute,
	Timeframe5Min:  5 * time.Minute,
	Timeframe15Min: 15 * time.Minute,
	Timeframe1Hour: time.Hour,
	Timeframe4Hour: 4 * time.Hour,
	Timeframe1Day:  24 * time.Hour,
}

// oandaMaxCandles is the maximum number of candles OANDA returns per request.
const oandaMaxCandles = 5000

// OANDAPracticeURL and OANDALiveURL are the v20 REST base URLs. The practice
// (demo) environment is free, needs no funding, and — on a non-US OANDA entity —
// serves metals (XAU/USD, XAG/USD) alongside spot forex.
const (
	OANDAPracticeURL = "https://api-fxpractice.oanda.com"
	OANDALiveURL     = "https://api-fxtrade.oanda.com"
)

// OANDAClient implements MarketDataProvider using the OANDA v20 REST API. It is used
// by the ORB backtest engine as a free, metals-capable source of deep 1-minute history.
// Rate limit: OANDA allows ~120 req/s; we throttle conservatively.
type OANDAClient struct {
	token     string
	accountID string // required only for FetchQuote (pricing endpoint)
	baseURL   string

	mu          sync.Mutex
	lastRequest time.Time
	minInterval time.Duration

	httpClient *http.Client
}

// NewOANDAClient creates an OANDA v20 client. baseURL defaults to the practice
// environment when empty (the expected setup for backtesting).
func NewOANDAClient(token, accountID, baseURL string) *OANDAClient {
	if baseURL == "" {
		baseURL = OANDAPracticeURL
	}
	return &OANDAClient{
		token:       token,
		accountID:   accountID,
		baseURL:     strings.TrimRight(baseURL, "/"),
		minInterval: 100 * time.Millisecond,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

// oandaInstrument converts our "EUR/USD" ticker form to OANDA's "EUR_USD".
func oandaInstrument(ticker string) string {
	return strings.ReplaceAll(ticker, "/", "_")
}

// throttle enforces the per-client rate limit, honouring context cancellation.
func (c *OANDAClient) throttle(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if since := time.Since(c.lastRequest); since < c.minInterval {
		select {
		case <-time.After(c.minInterval - since):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.lastRequest = time.Now()
	return nil
}

// get performs an authenticated GET and decodes the JSON body into out.
func (c *OANDAClient) get(ctx context.Context, path string, params url.Values, out any) error {
	if err := c.throttle(ctx); err != nil {
		return err
	}
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept-Datetime-Format", "RFC3339")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oanda: %s -> %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("oanda: decode %s: %w", path, err)
	}
	return nil
}

// oandaCandle is one row of the candles response. Prices are JSON strings.
type oandaCandle struct {
	Time     time.Time `json:"time"`
	Volume   int64     `json:"volume"`
	Complete bool      `json:"complete"`
	Mid      struct {
		O string `json:"o"`
		H string `json:"h"`
		L string `json:"l"`
		C string `json:"c"`
	} `json:"mid"`
}

// FetchCandles retrieves OHLCV bars for [from, to], paging through OANDA's 5000-candle
// response cap. Incomplete (still-forming) candles are dropped so a backtest never sees
// a partial bar. Mid prices are used (bid/ask spread is modelled separately by the
// backtest's commission/spread layer, not baked into the candle).
func (c *OANDAClient) FetchCandles(ctx context.Context, ticker string, tf Timeframe, from, to time.Time) ([]Candle, error) {
	gran, ok := oandaGranularities[tf]
	if !ok {
		return nil, fmt.Errorf("oanda: unsupported timeframe %q", tf)
	}
	step := oandaGranularityDuration[tf]
	instrument := oandaInstrument(ticker)

	var out []Candle
	cursor := from.UTC()
	to = to.UTC()
	for cursor.Before(to) {
		params := url.Values{}
		params.Set("granularity", gran)
		params.Set("price", "M")
		params.Set("from", cursor.Format(time.RFC3339))
		params.Set("to", to.Format(time.RFC3339))
		params.Set("count", strconv.Itoa(oandaMaxCandles))

		var resp struct {
			Candles []oandaCandle `json:"candles"`
		}
		if err := c.get(ctx, "/v3/instruments/"+instrument+"/candles", params, &resp); err != nil {
			return nil, err
		}
		if len(resp.Candles) == 0 {
			break
		}

		var last time.Time
		for _, oc := range resp.Candles {
			if !oc.Complete {
				continue // skip the still-forming bar
			}
			if !oc.Time.Before(to) {
				continue // OANDA's range is inclusive of `to`; keep it strictly half-open
			}
			cd, err := oc.toCandle(ticker, tf)
			if err != nil {
				return nil, err
			}
			out = append(out, cd)
			last = oc.Time
		}

		// Advance past the last bar we saw. If the page returned fewer than the cap,
		// or we made no time progress, we've reached the end of the range.
		if len(resp.Candles) < oandaMaxCandles || last.IsZero() {
			break
		}
		next := last.Add(step)
		if !next.After(cursor) {
			break // guard against a non-advancing cursor
		}
		cursor = next
	}
	return out, nil
}

// toCandle parses the string-encoded OHLC into a typed Candle.
func (oc oandaCandle) toCandle(ticker string, tf Timeframe) (Candle, error) {
	o, err1 := strconv.ParseFloat(oc.Mid.O, 64)
	h, err2 := strconv.ParseFloat(oc.Mid.H, 64)
	l, err3 := strconv.ParseFloat(oc.Mid.L, 64)
	cl, err4 := strconv.ParseFloat(oc.Mid.C, 64)
	for _, err := range []error{err1, err2, err3, err4} {
		if err != nil {
			return Candle{}, fmt.Errorf("oanda: parse candle price at %s: %w", oc.Time.Format(time.RFC3339), err)
		}
	}
	return Candle{
		Ticker:    ticker,
		Timeframe: tf,
		Timestamp: oc.Time.UTC(),
		Open:      o,
		High:      h,
		Low:       l,
		Close:     cl,
		Volume:    oc.Volume,
	}, nil
}

// FetchHistory retrieves the most recent N candles for a ticker.
func (c *OANDAClient) FetchHistory(ctx context.Context, ticker string, tf Timeframe, limit int) ([]Candle, error) {
	gran, ok := oandaGranularities[tf]
	if !ok {
		return nil, fmt.Errorf("oanda: unsupported timeframe %q", tf)
	}
	if limit <= 0 || limit > oandaMaxCandles {
		limit = oandaMaxCandles
	}
	params := url.Values{}
	params.Set("granularity", gran)
	params.Set("price", "M")
	params.Set("count", strconv.Itoa(limit))

	var resp struct {
		Candles []oandaCandle `json:"candles"`
	}
	if err := c.get(ctx, "/v3/instruments/"+oandaInstrument(ticker)+"/candles", params, &resp); err != nil {
		return nil, err
	}
	out := make([]Candle, 0, len(resp.Candles))
	for _, oc := range resp.Candles {
		if !oc.Complete {
			continue
		}
		cd, err := oc.toCandle(ticker, tf)
		if err != nil {
			return nil, err
		}
		out = append(out, cd)
	}
	return out, nil
}

// FetchQuote retrieves the latest bid/ask for a ticker via the pricing endpoint.
// Requires an account ID (set on the client); returns an error if unset.
func (c *OANDAClient) FetchQuote(ctx context.Context, ticker string) (*Quote, error) {
	if c.accountID == "" {
		return nil, fmt.Errorf("oanda: FetchQuote requires an account ID")
	}
	params := url.Values{}
	params.Set("instruments", oandaInstrument(ticker))

	var resp struct {
		Prices []struct {
			Instrument string    `json:"instrument"`
			Time       time.Time `json:"time"`
			Bids       []struct {
				Price string `json:"price"`
			} `json:"bids"`
			Asks []struct {
				Price string `json:"price"`
			} `json:"asks"`
		} `json:"prices"`
	}
	if err := c.get(ctx, "/v3/accounts/"+c.accountID+"/pricing", params, &resp); err != nil {
		return nil, err
	}
	if len(resp.Prices) == 0 {
		return nil, fmt.Errorf("oanda: no price for %q", ticker)
	}
	p := resp.Prices[0]
	q := &Quote{Ticker: ticker, Timestamp: p.Time.UTC()}
	if len(p.Bids) > 0 {
		q.Bid, _ = strconv.ParseFloat(p.Bids[0].Price, 64)
	}
	if len(p.Asks) > 0 {
		q.Ask, _ = strconv.ParseFloat(p.Asks[0].Price, 64)
	}
	q.Last = (q.Bid + q.Ask) / 2
	return q, nil
}

// compile-time assertion that OANDAClient satisfies the provider interface.
var _ MarketDataProvider = (*OANDAClient)(nil)
