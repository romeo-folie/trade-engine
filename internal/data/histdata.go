package data

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HistDataClient is a BACKTEST-only market-data provider backed by HistData.com's free
// historical tick archive. Unlike Dukascopy's per-hour throttled feed ([[DukascopyClient]]),
// HistData serves one LZIP-free ZIP per instrument-month with NO account, API key, or rate
// limit — so a multi-year backtest history is a handful of bulk downloads rather than a
// throttled hour-by-hour grind.
//
// The tick archive was verified equivalent to Dukascopy to the fifth decimal on real data
// (EUR/USD May 2025 and Nov 2024), so bars from this provider match the Dukascopy path. Two
// things differ from Dukascopy and are handled here:
//   - Timestamps are US Eastern WITH daylight saving (EDT in summer, EST in winter), so they
//     are parsed in the America/New_York location and converted to UTC. A FIXED offset would
//     drift by an hour across the DST boundaries.
//   - Prices are plain decimals (gold quoted as 3229.035, not a scaled integer), so there is
//     no per-instrument divisor — unlike Dukascopy's ÷1e5 / ÷1e3.
//
// Only FetchCandles is meaningful (this is a historical source, not a live feed).
type HistDataClient struct {
	baseURL    string
	cacheDir   string // on-disk ZIP cache root ("" disables); a month is downloaded at most once
	httpClient *http.Client
	nyLoc      *time.Location

	mu sync.Mutex
	// parseMu serializes the expensive tick-parse+aggregate step so only ONE month's ticks
	// (~96 MB) are resident at a time, regardless of how many parameter sets run concurrently.
	// Bar-cache reads stay concurrent under mu; only the one-time population is serialized.
	parseMu sync.Mutex
	// monthBars caches AGGREGATED 1-minute bars per "PAIR/YYYYMM", NOT raw ticks. Ticks are the
	// memory sink (a month is ~2.4M ticks ≈ 100 MB; a multi-year, multi-instrument sweep holding
	// them all resident blows out RAM and thrashes swap). 1-minute bars are ~1000× smaller and
	// carry everything the ORB engine needs — coarser timeframes are re-derived from them in
	// FetchCandles (cheap), and the finest tick detail is never used. Ticks are parsed, aggregated
	// to 1-minute bars, and immediately discarded (see monthMinuteBarsFor).
	monthBars map[string][]Candle
}

// HistDataBaseURL is the public site root. The download flow scrapes a per-page token from the
// pair/year page, then POSTs it to get.php (which requires a matching Referer header).
const HistDataBaseURL = "https://www.histdata.com"

// histDataMaxAttempts bounds retries on a transient network / non-200 download failure.
const histDataMaxAttempts = 3

// histDataUserAgent is sent on every request; the site rejects an empty UA.
const histDataUserAgent = "Mozilla/5.0 (compatible; trade-engine-backtest/1)"

// histDataTokenRE extracts the 32-hex download token from the pair/year page.
var histDataTokenRE = regexp.MustCompile(`id="tk"[^>]*value="([0-9a-f]{32})"`)

// NewHistDataClient creates a client against the HistData archive. baseURL defaults to the
// public site when empty. cacheDir, when non-empty, enables the on-disk per-month ZIP cache
// (each instrument-month is downloaded at most once, ever, then served from disk).
func NewHistDataClient(baseURL, cacheDir string) (*HistDataClient, error) {
	if baseURL == "" {
		baseURL = HistDataBaseURL
	}
	// America/New_York is the crux: HistData timestamps follow US Eastern WITH DST, so a
	// DST-aware location is required — a fixed EST offset misaligns every summer bar by an hour.
	//
	// NOTE: HistData's own FAQ claims the timestamps are fixed EST (UTC-5, no DST). That is
	// WRONG — verified empirically against Dukascopy (an independent UTC-anchored feed): EUR/USD
	// bars align at UTC-4 in May 2025 and UTC-5 in Nov 2024, matching to the 5th decimal. Two
	// different offsets across the DST boundary is impossible under a fixed offset — the data
	// is DST-aware Eastern. Do NOT "fix" this to a fixed -5h. See TestHistData_ParseTime_DST.
	nyLoc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, fmt.Errorf("histdata: load America/New_York: %w", err)
	}
	return &HistDataClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		cacheDir:   cacheDir,
		httpClient: &http.Client{Timeout: 120 * time.Second}, // month ZIPs are ~15 MB
		nyLoc:      nyLoc,
		monthBars:  make(map[string][]Candle),
	}, nil
}

// histDataPair converts our "EUR/USD" ticker into the two forms the site uses: the lowercase
// URL segment ("eurusd") and the uppercase fxpair form field ("EURUSD").
func histDataPair(ticker string) (urlSeg, fxPair string) {
	up := strings.ReplaceAll(strings.ToUpper(ticker), "/", "")
	return strings.ToLower(up), up
}

// FetchCandles downloads every instrument-month overlapping [from, to), parses its ticks, and
// aggregates them into OHLC bars of the requested timeframe on the MID price. The range is
// half-open (a tick at exactly `to` is excluded), matching the Dukascopy and OANDA providers.
func (c *HistDataClient) FetchCandles(ctx context.Context, ticker string, tf Timeframe, from, to time.Time) ([]Candle, error) {
	step, ok := dukascopyTimeframeStep[tf] // shared timeframe→bar-width map (same aggregation)
	if !ok {
		return nil, fmt.Errorf("histdata: unsupported timeframe %q", tf)
	}
	if !to.After(from) {
		return nil, nil
	}

	// This provider is 1-minute-granular: it caches 1-minute bars, so sub-minute precision in the
	// range is not representable. Floor both bounds to the minute so behaviour is DETERMINISTIC and
	// matches tick-level [from, to) filtering exactly (a no-op for every real caller, which is
	// already minute-aligned; without it a sub-minute bound would silently drop/keep an edge bar).
	from = from.Truncate(time.Minute)
	to = to.Truncate(time.Minute)
	if !to.After(from) {
		return nil, nil // a sub-minute range collapsed to empty
	}

	// The archive is keyed by ET month. A tick's ET time is monotonic in its UTC time, so every
	// tick whose UTC time is in [from, to) has an ET month between from-in-ET and to-in-ET
	// inclusive — enumerate exactly those, no padding needed.
	months := enumerateHistDataMonths(from.In(c.nyLoc), to.In(c.nyLoc))

	// Gather the cached 1-minute bars for the needed months (each month is aggregated once, then
	// reused). Per-month 1-minute slices are individually sorted and the months are in order, so
	// the concatenation is globally sorted ascending — no month's 1-minute bucket can straddle the
	// ET-month seam because the seam lands on a whole UTC hour (ET offset is ±whole hours).
	var minBars []Candle
	for _, ym := range months {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		bars, err := c.monthMinuteBarsFor(ctx, ticker, ym.year, ym.month)
		if err != nil {
			return nil, err
		}
		minBars = append(minBars, bars...)
	}

	// Slice to the in-range 1-minute window ONCE (binary search over the sorted bars) so a
	// per-session query costs O(range), not O(month). 1-minute is the cache granularity, so it is
	// served directly; coarser timeframes are re-bucketed from the window — which correctly handles
	// buckets spanning the ET-month seam (they draw from both months' 1-minute bars) and matches
	// aggregating the raw ticks, because 1-minute bars preserve OHLC and every query is minute-aligned.
	window := sliceBars(minBars, from, to)
	if tf == Timeframe1Min {
		return window, nil
	}
	return aggregateBars(ticker, tf, step, window), nil
}

type histDataYM struct{ year, month int }

// enumerateHistDataMonths lists the calendar months from `from` to `to` inclusive (both already
// in ET). Endpoints are truncated to the first of their month.
func enumerateHistDataMonths(from, to time.Time) []histDataYM {
	var out []histDataYM
	y, m := from.Year(), int(from.Month())
	endY, endM := to.Year(), int(to.Month())
	for y < endY || (y == endY && m <= endM) {
		out = append(out, histDataYM{y, m})
		m++
		if m > 12 {
			m = 1
			y++
		}
	}
	return out
}

// monthMinuteBarsFor returns the 1-minute bars for one instrument-ET-month, served from the
// in-memory bar cache, then the on-disk ZIP cache, then a network download. In every miss path
// the ticks are parsed, aggregated to 1-minute bars, and DISCARDED — only the bars are retained,
// which is what keeps a multi-year, multi-instrument sweep inside RAM. The ZIP is cached on disk
// only AFTER it parses cleanly, and a disk-cached ZIP that fails to parse is treated as a miss and
// re-downloaded — so a truncated/corrupt cache entry self-heals instead of wedging the run.
func (c *HistDataClient) monthMinuteBarsFor(ctx context.Context, ticker string, year, month int) ([]Candle, error) {
	urlSeg, fxPair := histDataPair(ticker)
	key := fmt.Sprintf("%s/%04d%02d", fxPair, year, month)

	c.mu.Lock()
	if bars, ok := c.monthBars[key]; ok {
		c.mu.Unlock()
		return bars, nil
	}
	c.mu.Unlock()

	// Serialize the parse+aggregate so at most one month of ticks is resident at once. Re-check the
	// cache after acquiring parseMu: a concurrent caller may have populated this month while we
	// waited, in which case we skip the redundant (and memory-heavy) re-parse.
	c.parseMu.Lock()
	defer c.parseMu.Unlock()
	c.mu.Lock()
	if bars, ok := c.monthBars[key]; ok {
		c.mu.Unlock()
		return bars, nil
	}
	c.mu.Unlock()

	var cachePath string
	if c.cacheDir != "" {
		cachePath = filepath.Join(c.cacheDir, "histdata", fxPair, fmt.Sprintf("%04d%02d.zip", year, month))
		if b, err := os.ReadFile(cachePath); err == nil && len(b) > 0 {
			if ticks, perr := parseHistDataZip(b, c.nyLoc); perr == nil {
				return c.storeMonth(key, ticker, ticks), nil
			} else {
				slog.Warn("histdata: corrupt cached ZIP — re-downloading", "path", cachePath, "err", perr)
			}
		}
	}

	zipBytes, err := c.download(ctx, urlSeg, fxPair, year, month)
	if err != nil {
		return nil, err
	}
	ticks, err := parseHistDataZip(zipBytes, c.nyLoc)
	if err != nil {
		return nil, fmt.Errorf("histdata: parse %s: %w", key, err)
	}
	if cachePath != "" {
		if werr := writeFileAtomic(cachePath, zipBytes); werr != nil {
			slog.Warn("histdata: cache write failed", "path", cachePath, "err", werr)
		}
	}
	return c.storeMonth(key, ticker, ticks), nil
}

// storeMonth aggregates a month's ticks into 1-minute bars, caches the bars (dropping the ticks),
// and returns them.
func (c *HistDataClient) storeMonth(key, ticker string, ticks []dukaTick) []Candle {
	bars := aggregateTicksToMinute(ticker, ticks)
	c.mu.Lock()
	c.monthBars[key] = bars
	c.mu.Unlock()
	return bars
}

// download performs the two-step HistData flow: GET the pair/year page to scrape the per-page
// token, then POST it to get.php with a matching Referer to receive the month ZIP.
func (c *HistDataClient) download(ctx context.Context, urlSeg, fxPair string, year, month int) ([]byte, error) {
	page := fmt.Sprintf("%s/download-free-forex-data/?/ascii/tick-data-quotes/%s/%d/%d", c.baseURL, urlSeg, year, month)
	datemonth := fmt.Sprintf("%04d%02d", year, month)

	var lastErr error
	for attempt := range histDataMaxAttempts {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		tk, err := c.fetchToken(ctx, page)
		if err != nil {
			lastErr = err
			continue
		}

		form := fmt.Sprintf("tk=%s&date=%d&datemonth=%s&platform=ASCII&timeframe=T&fxpair=%s", tk, year, datemonth, fxPair)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/get.php", strings.NewReader(form))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Referer", page) // get.php rejects the request without a matching Referer
		req.Header.Set("User-Agent", histDataUserAgent)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("histdata: %s -> %d", datemonth, resp.StatusCode)
			continue
		}
		// A valid response is a ZIP local-file header ("PK\x03\x04"); anything else (an HTML
		// error page, or an empty end-of-central-directory record) is a failure, not data.
		if len(body) < 4 || !bytes.HasPrefix(body, []byte("PK\x03\x04")) {
			lastErr = fmt.Errorf("histdata: %s/%s -> non-ZIP response (%d bytes)", fxPair, datemonth, len(body))
			continue
		}
		return body, nil
	}
	return nil, fmt.Errorf("histdata: download %s/%04d%02d failed after %d attempts: %w", fxPair, year, month, histDataMaxAttempts, lastErr)
}

// fetchToken GETs the pair/year page and scrapes the 32-hex download token.
func (c *HistDataClient) fetchToken(ctx context.Context, page string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", histDataUserAgent)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("histdata: token page -> %d", resp.StatusCode)
	}
	html, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	m := histDataTokenRE.FindSubmatch(html)
	if m == nil {
		return "", fmt.Errorf("histdata: token not found on %s", page)
	}
	return string(m[1]), nil
}

// parseHistDataZip unzips a month archive, finds its tick CSV, and parses it into UTC ticks.
func parseHistDataZip(zipBytes []byte, nyLoc *time.Location) ([]dukaTick, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, fmt.Errorf("zip open: %w", err)
	}
	for _, f := range zr.File {
		if !strings.HasSuffix(strings.ToLower(f.Name), ".csv") {
			continue // the archive also ships a .txt status report
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("zip entry %s: %w", f.Name, err)
		}
		ticks, err := parseHistDataCSV(rc, nyLoc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("csv %s: %w", f.Name, err)
		}
		return ticks, nil
	}
	return nil, fmt.Errorf("no .csv entry in archive")
}

// parseHistDataCSV parses HistData "Generic ASCII" tick rows: `YYYYMMDD HHMMSSmmm,bid,ask,vol`.
// The timestamp is US Eastern (DST-aware); prices are plain decimals; the mid is (bid+ask)/2.
func parseHistDataCSV(r io.Reader, nyLoc *time.Location) ([]dukaTick, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	out := make([]dukaTick, 0, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		// Split into datetime, bid, ask (volume ignored — always 0 for FX/metals).
		dt, rest, ok := strings.Cut(line, ",")
		if !ok {
			return nil, fmt.Errorf("malformed line %q", line)
		}
		bidStr, rest, ok := strings.Cut(rest, ",")
		if !ok {
			return nil, fmt.Errorf("malformed line %q", line)
		}
		bid, err := strconv.ParseFloat(bidStr, 64)
		if err != nil {
			return nil, fmt.Errorf("bad bid in %q: %w", line, err)
		}
		askStr := rest
		if a, _, ok := strings.Cut(rest, ","); ok {
			askStr = a
		}
		ask, err := strconv.ParseFloat(askStr, 64)
		if err != nil {
			return nil, fmt.Errorf("bad ask in %q: %w", line, err)
		}
		t, err := parseHistDataTime(dt, nyLoc)
		if err != nil {
			return nil, err
		}
		out = append(out, dukaTick{t: t, mid: (bid + ask) / 2, vol: 0})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseHistDataTime parses "YYYYMMDD HHMMSSmmm" as US Eastern and returns the UTC instant.
// The layout has no separator before the millisecond field, so fields are sliced by position.
func parseHistDataTime(s string, nyLoc *time.Location) (time.Time, error) {
	if len(s) != 18 || s[8] != ' ' {
		return time.Time{}, fmt.Errorf("bad timestamp %q", s)
	}
	atoi := func(a, b int) (int, error) { return strconv.Atoi(s[a:b]) }
	yr, e1 := atoi(0, 4)
	mo, e2 := atoi(4, 6)
	dy, e3 := atoi(6, 8)
	hh, e4 := atoi(9, 11)
	mi, e5 := atoi(11, 13)
	ss, e6 := atoi(13, 15)
	ms, e7 := atoi(15, 18)
	for _, e := range []error{e1, e2, e3, e4, e5, e6, e7} {
		if e != nil {
			return time.Time{}, fmt.Errorf("bad timestamp %q: %w", s, e)
		}
	}
	return time.Date(yr, time.Month(mo), dy, hh, mi, ss, ms*int(time.Millisecond), nyLoc).UTC(), nil
}

// writeFileAtomic writes data to a temp file in the target dir, then renames it into place, so
// an interrupted write never leaves a partial cache entry a later run would misread.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// histDataMaxTime is a far-future sentinel used to include every tick when aggregating a whole
// month (aggregateTicks excludes ticks at or after `to`).
var histDataMaxTime = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// aggregateTicksToMinute reduces a month's ticks to 1-minute OHLC bars (the cache granularity).
// The whole span is included via a zero..max sentinel range, so no assumption is made about the
// ticks being pre-sorted.
func aggregateTicksToMinute(ticker string, ticks []dukaTick) []Candle {
	if len(ticks) == 0 {
		return nil
	}
	return aggregateTicks(ticker, Timeframe1Min, time.Minute, time.Time{}, histDataMaxTime, ticks)
}

// sliceBars returns a COPY of the bars whose timestamp is in [from, to). Input must be sorted
// ascending by timestamp (the month caches are). The copy protects the shared cache from callers
// that might mutate the returned slice.
func sliceBars(bars []Candle, from, to time.Time) []Candle {
	lo := sort.Search(len(bars), func(i int) bool { return !bars[i].Timestamp.Before(from) })
	hi := sort.Search(len(bars), func(i int) bool { return !bars[i].Timestamp.Before(to) })
	if lo >= hi {
		return nil
	}
	out := make([]Candle, hi-lo)
	copy(out, bars[lo:hi])
	return out
}

// aggregateBars re-buckets already-in-range 1-minute bars into coarser OHLC bars of the given
// timeframe. It mirrors aggregateTicks (bucketing by truncating to the step) but consumes bars —
// high/low take the extremes, open/close the first/last by timestamp, volume the sum — which is
// identical to aggregating the underlying ticks because 1-minute bars preserve those.
func aggregateBars(ticker string, tf Timeframe, step time.Duration, bars []Candle) []Candle {
	buckets := make(map[time.Time]*aggBar)
	for _, b := range bars {
		bucket := b.Timestamp.Truncate(step)
		a := buckets[bucket]
		if a == nil {
			a = &aggBar{open: b.Open, high: b.High, low: b.Low, close: b.Close, openT: b.Timestamp, closeT: b.Timestamp}
			buckets[bucket] = a
			continue
		}
		if b.High > a.high {
			a.high = b.High
		}
		if b.Low < a.low {
			a.low = b.Low
		}
		if b.Timestamp.Before(a.openT) {
			a.openT, a.open = b.Timestamp, b.Open
		}
		if !b.Timestamp.Before(a.closeT) {
			a.closeT, a.close = b.Timestamp, b.Close
		}
		a.vol += float64(b.Volume)
	}
	out := make([]Candle, 0, len(buckets))
	for bucket, a := range buckets {
		out = append(out, Candle{
			Ticker: ticker, Timeframe: tf, Timestamp: bucket,
			Open: a.open, High: a.high, Low: a.low, Close: a.close, Volume: int64(math.Round(a.vol)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out
}

// FetchQuote is not supported by the historical archive (no live pricing).
func (c *HistDataClient) FetchQuote(_ context.Context, ticker string) (*Quote, error) {
	return nil, fmt.Errorf("histdata: FetchQuote not supported for %q (historical archive only)", ticker)
}

// FetchHistory is not supported: callers should use FetchCandles with an explicit range.
func (c *HistDataClient) FetchHistory(_ context.Context, ticker string, _ Timeframe, _ int) ([]Candle, error) {
	return nil, fmt.Errorf("histdata: FetchHistory not supported for %q (use FetchCandles with a range)", ticker)
}

// compile-time assertion that HistDataClient satisfies the provider interface.
var _ MarketDataProvider = (*HistDataClient)(nil)
