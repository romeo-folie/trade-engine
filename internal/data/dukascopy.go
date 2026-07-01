package data

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ulikunitz/xz/lzma"
)

// DukascopyBaseURL is Dukascopy's free historical tick-data feed. Unlike OANDA it needs NO
// account and NO API key: hourly LZMA-compressed .bi5 tick files are served as static
// objects, which makes it the zero-credential source for the ORB backtest engine.
const DukascopyBaseURL = "https://datafeed.dukascopy.com/datafeed"

// dukascopyTimeframeStep maps our internal timeframes to the aggregation bar width. Ticks
// are bucketed by truncating their UTC timestamp to this step.
var dukascopyTimeframeStep = map[Timeframe]time.Duration{
	Timeframe1Min:  time.Minute,
	Timeframe5Min:  5 * time.Minute,
	Timeframe15Min: 15 * time.Minute,
	Timeframe1Hour: time.Hour,
	Timeframe4Hour: 4 * time.Hour,
	Timeframe1Day:  24 * time.Hour,
}

// dukaTickBytes is the fixed size of one .bi5 tick record.
const dukaTickBytes = 20

// DukascopyClient implements MarketDataProvider against Dukascopy's free historical tick
// feed. FetchCandles fetches the hourly .bi5 files spanning the range, LZMA-decompresses
// them, parses the fixed-width big-endian tick records, and aggregates ticks into OHLC bars
// on the MID price (bid/ask spread is modelled separately by the backtest cost layer, not
// baked into the bar — matching the OANDA provider). Fetched hours are cached so the ORB
// backtest's overlapping session lookbacks don't re-download.
//
// Only FetchCandles is meaningful: the historical feed has no live quote, so FetchQuote and
// FetchHistory return errors. This is a BACKTEST data source, not a live feed.
type DukascopyClient struct {
	baseURL    string
	pointScale map[string]float64 // ticker → price divisor override (see priceDivisor)
	httpClient *http.Client

	mu    sync.Mutex
	cache map[string][]dukaTick // hour key → parsed ticks (an empty slice is cached for a no-data hour)

	// Lifetime coverage counters (under mu): if the client fetches more than a full weekend
	// of hours and NONE ever return ticks, the feed is misconfigured (wrong base URL or
	// instrument) rather than just closed — a legitimate weekend gap is at most ~48 hours, and
	// any longer span necessarily includes weekday trading hours with data. Tracked at the
	// client level (not per FetchCandles call) because the ORB backtest fetches in many small
	// per-session windows, so a per-call check would never accumulate enough hours to fire.
	hoursFetched  int
	hoursWithData int

	minInterval time.Duration
	lastRequest time.Time
}

// dukascopyEmptyHoursError is the lifetime empty-hour count past which the client concludes
// the feed is broken rather than closed (must exceed a ~48h weekend so it never false-fires).
const dukascopyEmptyHoursError = 48

// NewDukascopyClient creates a client against the historical feed. baseURL defaults to the
// public feed when empty. pointScale optionally overrides the per-ticker price divisor.
func NewDukascopyClient(baseURL string, pointScale map[string]float64) *DukascopyClient {
	if baseURL == "" {
		baseURL = DukascopyBaseURL
	}
	return &DukascopyClient{
		baseURL:     strings.TrimRight(baseURL, "/"),
		pointScale:  pointScale,
		httpClient:  &http.Client{Timeout: 60 * time.Second},
		cache:       make(map[string][]dukaTick),
		minInterval: 50 * time.Millisecond,
	}
}

// dukaTick is one parsed tick: absolute UTC time, mid price, and combined volume.
type dukaTick struct {
	t   time.Time
	mid float64
	vol float64
}

// dukascopyInstrument converts our "EUR/USD" ticker form to Dukascopy's "EURUSD".
func dukascopyInstrument(ticker string) string {
	return strings.ReplaceAll(strings.ToUpper(ticker), "/", "")
}

// dukascopyHourPath builds the feed-relative path for one instrument-hour (UTC). The MONTH
// is ZERO-INDEXED (00 = January) — a Dukascopy quirk and an easy off-by-one.
func dukascopyHourPath(inst string, hour time.Time) string {
	hour = hour.UTC()
	return fmt.Sprintf("%s/%04d/%02d/%02d/%02dh_ticks.bi5",
		inst, hour.Year(), int(hour.Month())-1, hour.Day(), hour.Hour())
}

// priceDivisor returns the integer→price divisor for a ticker. Dukascopy stores prices as
// scaled integers: 5-decimal FX ÷1e5, JPY pairs and metals (XAU/XAG) ÷1e3. A per-ticker
// config override (markets.dukascopy.point_scale) wins — set it if a metal's scaling differs
// (metals scaling is the one worth spot-checking against a known bar).
func (c *DukascopyClient) priceDivisor(ticker string) float64 {
	if c.pointScale != nil {
		if d, ok := c.pointScale[ticker]; ok && d > 0 {
			return d
		}
	}
	u := dukascopyInstrument(ticker)
	if strings.Contains(u, "JPY") || strings.HasPrefix(u, "XAU") || strings.HasPrefix(u, "XAG") {
		return 1000
	}
	return 100000
}

// FetchCandles fetches every hourly .bi5 tick file overlapping [from, to), then aggregates
// the ticks into OHLC bars of the requested timeframe. The range is half-open (a tick at
// exactly `to` is excluded), matching the OANDA provider.
func (c *DukascopyClient) FetchCandles(ctx context.Context, ticker string, tf Timeframe, from, to time.Time) ([]Candle, error) {
	step, ok := dukascopyTimeframeStep[tf]
	if !ok {
		return nil, fmt.Errorf("dukascopy: unsupported timeframe %q", tf)
	}
	from, to = from.UTC(), to.UTC()
	if !to.After(from) {
		return nil, nil
	}

	var ticks []dukaTick
	for hour := from.Truncate(time.Hour); hour.Before(to); hour = hour.Add(time.Hour) {
		hourTicks, err := c.fetchHour(ctx, ticker, hour)
		if err != nil {
			return nil, err
		}
		ticks = append(ticks, hourTicks...)
	}
	return aggregateTicks(ticker, tf, step, from, to, ticks), nil
}

// fetchHour returns the parsed ticks for one instrument-hour, caching the result (including
// an empty slice for a no-data hour) so repeated/overlapping range requests don't re-fetch.
func (c *DukascopyClient) fetchHour(ctx context.Context, ticker string, hour time.Time) ([]dukaTick, error) {
	path := dukascopyHourPath(dukascopyInstrument(ticker), hour)

	c.mu.Lock()
	if ticks, cached := c.cache[path]; cached {
		c.mu.Unlock()
		return ticks, nil
	}
	c.mu.Unlock()

	body, found, err := c.getBI5(ctx, c.baseURL+"/"+path)
	if err != nil {
		return nil, err
	}
	var ticks []dukaTick
	if found && len(body) > 0 {
		ticks, err = parseBI5(body, hour, c.priceDivisor(ticker))
		if err != nil {
			return nil, fmt.Errorf("dukascopy: parse %s: %w", path, err)
		}
	}
	if ticks == nil {
		ticks = []dukaTick{} // cache non-nil so the "cached" check above hits for no-data hours
	}
	c.mu.Lock()
	c.cache[path] = ticks
	c.hoursFetched++
	if len(ticks) > 0 {
		c.hoursWithData++
	}
	brokenFeed := c.hoursWithData == 0 && c.hoursFetched > dukascopyEmptyHoursError
	fetched := c.hoursFetched
	c.mu.Unlock()

	if brokenFeed {
		return nil, fmt.Errorf("dukascopy: no ticks for %s across %d fetched hours — verify the "+
			"instrument name and base URL (every request 404'd, including weekday trading hours)", ticker, fetched)
	}
	return ticks, nil
}

// getBI5 GETs a .bi5 file. A 404 (weekend / holiday / future hour — the market was closed)
// is not an error: it returns found=false. Network and 5xx errors are retried; a 4xx other
// than 404 fails immediately.
func (c *DukascopyClient) getBI5(ctx context.Context, u string) (body []byte, found bool, err error) {
	if err := c.throttle(ctx); err != nil {
		return nil, false, err
	}
	var lastErr error
	for attempt := range 3 {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if reqErr != nil {
			return nil, false, reqErr
		}
		resp, doErr := c.httpClient.Do(req)
		if doErr != nil {
			lastErr = doErr
			if !backoff(ctx, attempt) {
				return nil, false, ctx.Err()
			}
			continue
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			resp.Body.Close()
			return nil, false, nil // no data for this hour — market closed
		case resp.StatusCode == http.StatusOK:
			data, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				lastErr = readErr
				if !backoff(ctx, attempt) {
					return nil, false, ctx.Err()
				}
				continue
			}
			return data, true, nil
		case resp.StatusCode < 500:
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			resp.Body.Close()
			return nil, false, fmt.Errorf("dukascopy: %s -> %d: %s", u, resp.StatusCode, strings.TrimSpace(string(b)))
		default: // 5xx — retry
			resp.Body.Close()
			lastErr = fmt.Errorf("dukascopy: %s -> %d", u, resp.StatusCode)
			if !backoff(ctx, attempt) {
				return nil, false, ctx.Err()
			}
		}
	}
	return nil, false, lastErr
}

// backoff sleeps a short, attempt-scaled interval; returns false if the context is cancelled.
func backoff(ctx context.Context, attempt int) bool {
	select {
	case <-time.After(time.Duration(attempt+1) * 250 * time.Millisecond):
		return true
	case <-ctx.Done():
		return false
	}
}

// throttle enforces the per-client minimum request interval, honouring context cancellation.
func (c *DukascopyClient) throttle(ctx context.Context) error {
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

// parseBI5 LZMA-decompresses a .bi5 payload (classic LZMA format) and decodes its ticks.
func parseBI5(compressed []byte, hour time.Time, divisor float64) ([]dukaTick, error) {
	lr, err := lzma.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("lzma header: %w", err)
	}
	raw, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("lzma decompress: %w", err)
	}
	return decodeTicks(raw, hour, divisor)
}

// decodeTicks parses the fixed 20-byte big-endian tick records of a decompressed .bi5:
//
//	uint32 ms-offset within the hour | uint32 ask | uint32 bid | float32 ask-vol | float32 bid-vol
//
// Prices are scaled integers divided by the instrument's point divisor; the mid is used.
func decodeTicks(raw []byte, hour time.Time, divisor float64) ([]dukaTick, error) {
	if divisor <= 0 {
		return nil, fmt.Errorf("invalid price divisor %v", divisor)
	}
	if len(raw)%dukaTickBytes != 0 {
		return nil, fmt.Errorf("tick stream length %d is not a multiple of %d", len(raw), dukaTickBytes)
	}
	n := len(raw) / dukaTickBytes
	out := make([]dukaTick, 0, n)
	for i := range n {
		rec := raw[i*dukaTickBytes : (i+1)*dukaTickBytes]
		ms := binary.BigEndian.Uint32(rec[0:4])
		ask := binary.BigEndian.Uint32(rec[4:8])
		bid := binary.BigEndian.Uint32(rec[8:12])
		askVol := math.Float32frombits(binary.BigEndian.Uint32(rec[12:16]))
		bidVol := math.Float32frombits(binary.BigEndian.Uint32(rec[16:20]))
		out = append(out, dukaTick{
			t:   hour.Add(time.Duration(ms) * time.Millisecond),
			mid: (float64(ask) + float64(bid)) / 2 / divisor,
			vol: float64(askVol) + float64(bidVol),
		})
	}
	return out, nil
}

// aggBar accumulates the OHLC of one bar as ticks arrive.
type aggBar struct {
	open, high, low, close, vol float64
	openT, closeT               time.Time
}

// aggregateTicks buckets ticks (within the half-open [from, to)) into OHLC bars of the given
// step, on the mid price. Pure — no network — so it is unit-testable. Ticks arrive in
// chronological order per file, but open/close are chosen by timestamp defensively.
func aggregateTicks(ticker string, tf Timeframe, step time.Duration, from, to time.Time, ticks []dukaTick) []Candle {
	bars := make(map[time.Time]*aggBar)
	for _, tk := range ticks {
		if tk.t.Before(from) || !tk.t.Before(to) {
			continue
		}
		bucket := tk.t.UTC().Truncate(step)
		a := bars[bucket]
		if a == nil {
			a = &aggBar{open: tk.mid, high: tk.mid, low: tk.mid, close: tk.mid, openT: tk.t, closeT: tk.t}
			bars[bucket] = a
		}
		if tk.mid > a.high {
			a.high = tk.mid
		}
		if tk.mid < a.low {
			a.low = tk.mid
		}
		if tk.t.Before(a.openT) {
			a.openT, a.open = tk.t, tk.mid
		}
		if !tk.t.Before(a.closeT) {
			a.closeT, a.close = tk.t, tk.mid
		}
		a.vol += tk.vol
	}

	out := make([]Candle, 0, len(bars))
	for bucket, a := range bars {
		out = append(out, Candle{
			Ticker:    ticker,
			Timeframe: tf,
			Timestamp: bucket,
			Open:      a.open,
			High:      a.high,
			Low:       a.low,
			Close:     a.close,
			Volume:    int64(math.Round(a.vol)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out
}

// FetchQuote is not supported by the historical tick feed (no live pricing).
func (c *DukascopyClient) FetchQuote(_ context.Context, ticker string) (*Quote, error) {
	return nil, fmt.Errorf("dukascopy: FetchQuote not supported for %q (historical tick feed only)", ticker)
}

// FetchHistory is not supported: the caller should use FetchCandles with an explicit range.
func (c *DukascopyClient) FetchHistory(_ context.Context, ticker string, _ Timeframe, _ int) ([]Candle, error) {
	return nil, fmt.Errorf("dukascopy: FetchHistory not supported for %q (use FetchCandles with a range)", ticker)
}

// compile-time assertion that DukascopyClient satisfies the provider interface.
var _ MarketDataProvider = (*DukascopyClient)(nil)
