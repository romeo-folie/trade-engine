package data

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
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
	baseURL     string
	cacheDir    string             // on-disk cache root ("" disables); see readDiskCache/writeDiskCache
	pointScale  map[string]float64 // ticker → price divisor override (see priceDivisor)
	httpClient  *http.Client
	maxAttempts int // per-request try budget (default dukascopyMaxAttempts; lowered in tests)

	concurrency int           // in-flight hour-fetch bound (default dukascopyConcurrency)
	minInterval time.Duration // global spacing between request STARTS (0 = none); see waitTurn

	paceMu        sync.Mutex
	nextRequestAt time.Time // earliest time the next request may start (under paceMu)

	mu    sync.Mutex
	cache map[string][]dukaTick // hour key → parsed ticks (an empty slice is cached for a no-data hour)

	// Per-instrument lifetime coverage counters (under mu): if an instrument's fetches exceed
	// a full weekend of hours and NONE return ticks, its feed path is misconfigured (wrong
	// symbol or base URL) rather than just closed — a legitimate weekend gap is at most ~48
	// hours, and any longer span includes weekday trading hours with data. Keyed PER INSTRUMENT
	// (not client-wide) so a valid earlier symbol can't mask a later broken one; tracked across
	// calls because the ORB backtest fetches in many small per-session windows.
	hoursFetched  map[string]int // instrument → hours fetched
	hoursWithData map[string]int // instrument → hours that returned ticks
	fetchFailures map[string]int // instrument → hours skipped after exhausting retries
}

// dukascopyEmptyHoursError is the lifetime empty-hour count past which the client concludes
// the feed is broken rather than closed (must exceed a ~48h weekend so it never false-fires).
const dukascopyEmptyHoursError = 48

// dukascopyMaxAttempts is the per-request try budget. The free feed emits rare, transient
// 503s under sustained fetching; combined with exponential backoff this rides out a short
// block (~15s total). A 503 that outlasts the budget doesn't abort the run (see fetchHour).
const dukascopyMaxAttempts = 6

// dukascopyConcurrency bounds in-flight hour fetches. It doubles as the rate limiter (no
// per-request throttle): a modest bound keeps the aggregate rate near what the free feed
// tolerated serially while cutting wall-clock ~Nx over a high-latency link.
const dukascopyConcurrency = 6

// errDukaTransient marks a fetch error that exhausted the retry budget on a RETRYABLE cause
// (5xx / network). fetchHour skips such an hour as a data gap; every other error (a terminal
// non-404 4xx, a context cancellation/deadline) is propagated so the run fails fast.
var errDukaTransient = errors.New("dukascopy: transient fetch failure (retries exhausted)")

// dukascopyMaxFetchFailures bounds how many hours an instrument may FAIL to fetch (after
// exhausting retries) before the run aborts. A rare transient 503 that outlasts the retry
// budget is skipped as a one-hour data gap (a multi-thousand-fetch backtest tolerates a few);
// but a high failure count means the feed is genuinely unstable and the results untrustworthy.
const dukascopyMaxFetchFailures = 25

// DukascopyOptions tunes the client's politeness against the free feed's (varying) burst
// tolerance. Zero values keep the historical defaults, so existing callers are unchanged.
type DukascopyOptions struct {
	FetchConcurrency   int           // in-flight hour fetches (0 → 6)
	MinRequestInterval time.Duration // global spacing between request starts (0 → none)
	RequestTimeout     time.Duration // per-request HTTP timeout (0 → default 20s)
}

// NewDukascopyClient creates a client against the historical feed. baseURL defaults to the
// public feed when empty. cacheDir, when non-empty, enables the on-disk cache (each hour is
// fetched from the feed at most once, ever). pointScale optionally overrides the divisor.
// An optional DukascopyOptions tunes fetch pacing — the 2026 feed throttles cold-IP bursts
// to roughly one request per 3-5s, which the default concurrency-6/no-spacing profile
// drowns in 503s; concurrency 1 + 3-5s spacing fills a cache slowly but reliably.
func NewDukascopyClient(baseURL, cacheDir string, pointScale map[string]float64, opts ...DukascopyOptions) *DukascopyClient {
	if baseURL == "" {
		baseURL = DukascopyBaseURL
	}
	var opt DukascopyOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.FetchConcurrency <= 0 {
		opt.FetchConcurrency = dukascopyConcurrency
	}
	// Default 20s fails fast when a throttled feed hangs the connection. But the 2026 feed
	// TARPITS bulk fetchers — successful responses slow to ~15-20s, right at the 20s edge, so
	// they time out and get discarded as gaps (zero cache progress). A caller doing a paced,
	// patient fill raises this (via RequestTimeout) so tarpitted-but-successful responses land.
	reqTimeout := opt.RequestTimeout
	if reqTimeout <= 0 {
		reqTimeout = 20 * time.Second
	}
	return &DukascopyClient{
		baseURL:       strings.TrimRight(baseURL, "/"),
		cacheDir:      cacheDir,
		pointScale:    pointScale,
		httpClient:    &http.Client{Timeout: reqTimeout},
		maxAttempts:   dukascopyMaxAttempts,
		concurrency:   opt.FetchConcurrency,
		minInterval:   opt.MinRequestInterval,
		cache:         make(map[string][]dukaTick),
		hoursFetched:  make(map[string]int),
		hoursWithData: make(map[string]int),
	}
}

// waitTurn enforces the global minimum spacing between request starts across all workers.
// Returns false if the context is cancelled while waiting. Each caller reserves the next
// slot under paceMu, so N concurrent workers still start requests minInterval apart.
func (c *DukascopyClient) waitTurn(ctx context.Context) bool {
	if c.minInterval <= 0 {
		return true
	}
	c.paceMu.Lock()
	now := time.Now()
	start := c.nextRequestAt
	if start.Before(now) {
		start = now
	}
	c.nextRequestAt = start.Add(c.minInterval)
	c.paceMu.Unlock()
	if wait := time.Until(start); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return false
		}
	}
	return true
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
// scaled integers: 5-decimal FX ÷1e5; JPY pairs, metals (XAU/XAG) and index CFDs
// (…IDXUSD: USA30IDXUSD, USA500IDXUSD, USATECHIDXUSD, USSC2000IDXUSD, …) ÷1e3. The index
// scaling was verified empirically against 2024-03-14 ticks: USA30IDXUSD raw 38897625 ÷1e3
// = 38,897.6 (Dow) and USSC2000IDXUSD raw 2038126 ÷1e3 = 2,038.1 (Russell 2000), both
// matching that day's levels. A per-ticker config override (markets.dukascopy.point_scale)
// wins — set it if a symbol's scaling differs from these rules.
func (c *DukascopyClient) priceDivisor(ticker string) float64 {
	if c.pointScale != nil {
		if d, ok := c.pointScale[ticker]; ok && d > 0 {
			return d
		}
	}
	u := dukascopyInstrument(ticker)
	if strings.Contains(u, "JPY") || strings.HasPrefix(u, "XAU") || strings.HasPrefix(u, "XAG") || strings.HasSuffix(u, "IDXUSD") {
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

	// Enumerate the hours overlapping [from, to).
	var hours []time.Time
	for hour := from.Truncate(time.Hour); hour.Before(to); hour = hour.Add(time.Hour) {
		hours = append(hours, hour)
	}

	// Fetch the hours CONCURRENTLY (bounded): sequential per-hour fetches over the feed's
	// high-latency link dominate a multi-month backtest's wall-clock. The per-hour cache still
	// de-dups repeated/overlapping hours across calls; each hour here is distinct so workers
	// never contend on the same key. The concurrency bound is also the rate limiter (there is
	// no per-request throttle) — occasional 503s from the extra load are absorbed by the
	// retry + skip-on-exhaust path.
	// A fatal fetch error (terminal 4xx, the >25 failure cap, a parse error, or caller
	// cancellation) cancels the shared context and stops scheduling further hours, so the
	// per-instrument cap acts as a TIMELY safety net rather than letting every remaining hour
	// run its full retry/backoff first.
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()

	perHour := make([][]dukaTick, len(hours))
	concurrency := c.concurrency
	if concurrency <= 0 { // struct-literal construction (tests) leaves it unset
		concurrency = dukascopyConcurrency
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for i := range hours {
		if fctx.Err() != nil {
			break // a fatal error already occurred — don't schedule more work
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			t, err := c.fetchHour(fctx, ticker, hours[i])
			if err != nil {
				errOnce.Do(func() { firstErr = err; cancel() })
				return
			}
			perHour[i] = t
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	// The caller's context may have been cancelled before/while scheduling (so no worker
	// recorded an error) — surface it rather than returning silently-empty bars.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var ticks []dukaTick
	for i := range hours {
		ticks = append(ticks, perHour[i]...)
	}
	return aggregateTicks(ticker, tf, step, from, to, ticks), nil
}

// fetchHour returns the parsed ticks for one instrument-hour, caching the result (including
// an empty slice for a no-data hour) so repeated/overlapping range requests don't re-fetch.
func (c *DukascopyClient) fetchHour(ctx context.Context, ticker string, hour time.Time) ([]dukaTick, error) {
	inst := dukascopyInstrument(ticker)
	path := dukascopyHourPath(inst, hour)

	c.mu.Lock()
	if ticks, cached := c.cache[path]; cached {
		c.mu.Unlock()
		return ticks, nil
	}
	c.mu.Unlock()

	// On-disk cache: a previously-fetched (or confirmed-empty) hour is served from disk and
	// NEVER re-fetched — the way to use a rate-limited free feed for repeated backtests. Only
	// confirmed outcomes are cached (see below), so a transient-skipped hour is left uncached
	// and re-fetched on a later run — that is what makes a throttled fetch resumable.
	if c.cacheDir != "" {
		if ticks, ok := c.readDiskCache(path, hour, c.priceDivisor(ticker)); ok {
			c.mu.Lock()
			c.cache[path] = ticks
			c.mu.Unlock()
			return ticks, nil
		}
	}

	body, found, err := c.getBI5(ctx, c.baseURL+"/"+path)
	confirmed := err == nil // a real 200 or 404 — safe to persist; a transient skip is not
	if err != nil {
		// Terminal errors (a non-404 4xx, or context cancellation/deadline) are propagated so
		// the run fails fast — NOT converted into a silent data gap.
		if !errors.Is(err, errDukaTransient) {
			return nil, err
		}
		// A TRANSIENT failure (a 503 / network error that outlasted the retry budget) must NOT
		// abort the whole backtest: skip this one hour as a data gap and carry on — but abort if
		// an instrument accumulates too many, which means the feed is genuinely unstable and the
		// results can't be trusted.
		c.mu.Lock()
		if c.fetchFailures == nil {
			c.fetchFailures = make(map[string]int)
		}
		c.fetchFailures[inst]++
		failures := c.fetchFailures[inst]
		c.mu.Unlock()
		if failures > dukascopyMaxFetchFailures {
			return nil, fmt.Errorf("dukascopy: %s: aborting after %d fetch failures for %s (feed unstable): %w",
				path, failures, inst, err)
		}
		slog.Warn("dukascopy: hour fetch failed after retries — skipping as a data gap",
			"path", path, "instrument_failures", failures, "err", err)
		found, body = false, nil // treat as no-data for this hour
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

	// Persist a CONFIRMED outcome (data or no-data) to disk so it is never re-fetched.
	if c.cacheDir != "" && confirmed {
		if werr := c.writeDiskCache(path, body, found); werr != nil {
			slog.Warn("dukascopy: disk cache write failed", "path", path, "err", werr)
		}
	}
	c.mu.Lock()
	c.cache[path] = ticks
	if c.hoursFetched == nil { // defensive for a directly-constructed client (e.g. in tests)
		c.hoursFetched = make(map[string]int)
		c.hoursWithData = make(map[string]int)
	}
	c.hoursFetched[inst]++
	if len(ticks) > 0 {
		c.hoursWithData[inst]++
	}
	fetched := c.hoursFetched[inst]
	brokenFeed := c.hoursWithData[inst] == 0 && fetched > dukascopyEmptyHoursError
	c.mu.Unlock()

	if brokenFeed {
		return nil, fmt.Errorf("dukascopy: no ticks for %s across %d fetched hours — verify the "+
			"instrument name and base URL (every request 404'd, including weekday trading hours)", ticker, fetched)
	}
	return ticks, nil
}

// readDiskCache serves an hour from the on-disk cache. The cache mirrors the feed path under
// cacheDir; a data hour is the raw .bi5 bytes, a confirmed no-data hour is a 0-byte file, and
// an absent file means "not cached yet". A corrupt cache file is treated as a miss (re-fetch)
// rather than an error, so a partial write from a killed run self-heals.
func (c *DukascopyClient) readDiskCache(path string, hour time.Time, divisor float64) ([]dukaTick, bool) {
	full := filepath.Join(c.cacheDir, filepath.FromSlash(path))
	info, err := os.Stat(full)
	if err != nil {
		return nil, false // not cached (or unreadable) → fetch
	}
	if info.Size() == 0 {
		return []dukaTick{}, true // confirmed no-data hour
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		slog.Warn("dukascopy: disk cache read failed — re-fetching", "path", path, "err", err)
		return nil, false
	}
	ticks, err := parseBI5(raw, hour, divisor)
	if err != nil {
		slog.Warn("dukascopy: corrupt disk cache entry — re-fetching", "path", path, "err", err)
		return nil, false
	}
	return ticks, true
}

// writeDiskCache persists a confirmed hour: the raw .bi5 bytes for a data hour, or a 0-byte
// file for a confirmed no-data (404) hour. The write is atomic (temp file + rename) so an
// interrupted run never leaves a partially-written entry that a later run would misread.
func (c *DukascopyClient) writeDiskCache(path string, body []byte, hasData bool) error {
	full := filepath.Join(c.cacheDir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	var data []byte
	if hasData {
		data = body
	}
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, full)
}

// getBI5 GETs a .bi5 file. A 404 (weekend / holiday / future hour — the market was closed)
// is not an error: it returns found=false. Network and 5xx errors are retried; a 4xx other
// than 404 fails immediately.
func (c *DukascopyClient) getBI5(ctx context.Context, u string) (body []byte, found bool, err error) {
	var lastErr error
	attempts := c.maxAttempts
	if attempts <= 0 {
		attempts = dukascopyMaxAttempts
	}
	for attempt := range attempts {
		// Back off BEFORE each retry (not after each failure) so a persistently-dead URL
		// doesn't sleep a final, pointless interval before giving up.
		if attempt > 0 && !backoff(ctx, attempt-1) {
			return nil, false, ctx.Err()
		}
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if reqErr != nil {
			return nil, false, reqErr
		}
		if !c.waitTurn(ctx) { // global politeness spacing (no-op unless configured)
			return nil, false, ctx.Err()
		}
		resp, doErr := c.httpClient.Do(req)
		if doErr != nil {
			lastErr = doErr
			continue // network error — retry
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
				continue // read error — retry
			}
			return data, true, nil
		case resp.StatusCode < 500:
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			resp.Body.Close()
			return nil, false, fmt.Errorf("dukascopy: %s -> %d: %s", u, resp.StatusCode, strings.TrimSpace(string(b)))
		default: // 5xx — retry
			resp.Body.Close()
			lastErr = fmt.Errorf("dukascopy: %s -> %d", u, resp.StatusCode)
		}
	}
	// Retry budget exhausted on a retryable (5xx / network) cause — mark it transient so
	// fetchHour can skip the hour rather than abort the whole backtest.
	return nil, false, fmt.Errorf("%w: %s -> %w", errDukaTransient, u, lastErr)
}

// backoff sleeps an EXPONENTIALLY growing interval (0.5s, 1s, 2s, 4s, 8s… capped at 8s) so a
// transient 503 from the free feed's rate-limiter gets seconds to clear across the retry
// budget — a multi-thousand-fetch backtest hits intermittent 503s, and too-short a backoff
// aborts the whole run on one of them. Returns false if the context is cancelled.
func backoff(ctx context.Context, attempt int) bool {
	d := min(500*time.Millisecond<<attempt, 8*time.Second)
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
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
