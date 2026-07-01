package data

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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

	mu         sync.Mutex
	monthTicks map[string][]dukaTick // "PAIR/YYYYMM" → parsed ticks (UTC), so a month is parsed once per run
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
	nyLoc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, fmt.Errorf("histdata: load America/New_York: %w", err)
	}
	return &HistDataClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		cacheDir:   cacheDir,
		httpClient: &http.Client{Timeout: 120 * time.Second}, // month ZIPs are ~15 MB
		nyLoc:      nyLoc,
		monthTicks: make(map[string][]dukaTick),
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

	// The archive is keyed by ET month. A tick's ET time is monotonic in its UTC time, so every
	// tick whose UTC time is in [from, to) has an ET month between from-in-ET and to-in-ET
	// inclusive — enumerate exactly those, no padding needed.
	months := enumerateHistDataMonths(from.In(c.nyLoc), to.In(c.nyLoc))

	var all []dukaTick
	for _, ym := range months {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ticks, err := c.monthTicksFor(ctx, ticker, ym.year, ym.month)
		if err != nil {
			return nil, err
		}
		all = append(all, ticks...)
	}
	return aggregateTicks(ticker, tf, step, from, to, all), nil
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

// monthTicksFor returns the parsed ticks (UTC) for one instrument-ET-month, served from the
// in-memory cache, then the on-disk ZIP cache, then a network download. The ZIP is cached only
// AFTER it parses cleanly, and a disk-cached ZIP that fails to parse is treated as a miss and
// re-downloaded — so a truncated/corrupt cache entry self-heals instead of wedging the run.
func (c *HistDataClient) monthTicksFor(ctx context.Context, ticker string, year, month int) ([]dukaTick, error) {
	urlSeg, fxPair := histDataPair(ticker)
	key := fmt.Sprintf("%s/%04d%02d", fxPair, year, month)

	c.mu.Lock()
	if ticks, ok := c.monthTicks[key]; ok {
		c.mu.Unlock()
		return ticks, nil
	}
	c.mu.Unlock()

	var cachePath string
	if c.cacheDir != "" {
		cachePath = filepath.Join(c.cacheDir, "histdata", fxPair, fmt.Sprintf("%04d%02d.zip", year, month))
		if b, err := os.ReadFile(cachePath); err == nil && len(b) > 0 {
			if ticks, perr := parseHistDataZip(b, c.nyLoc); perr == nil {
				return c.storeMonth(key, ticks), nil
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
	return c.storeMonth(key, ticks), nil
}

// storeMonth records a parsed month in the in-memory cache and returns it.
func (c *HistDataClient) storeMonth(key string, ticks []dukaTick) []dukaTick {
	c.mu.Lock()
	c.monthTicks[key] = ticks
	c.mu.Unlock()
	return ticks
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
