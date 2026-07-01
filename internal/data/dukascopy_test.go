package data

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ulikunitz/xz/lzma"
)

// testClient builds a client pointed at a test server with no throttle delay.
func testClient(baseURL string, hc *http.Client) *DukascopyClient {
	return &DukascopyClient{baseURL: baseURL, httpClient: hc, cache: make(map[string][]dukaTick)}
}

// End-to-end over HTTP: the client must request the zero-indexed-month path, LZMA-decompress
// the .bi5 body, decode ticks, and aggregate to a bar — with 404 hours ignored.
func TestDukascopy_FetchCandles_HTTPPipeline(t *testing.T) {
	hour := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	raw := append(tickRec(0, 110100, 110080, 1, 1), tickRec(30000, 110150, 110130, 1, 1)...)
	var buf bytes.Buffer
	w, _ := lzma.NewWriter(&buf)
	_, _ = w.Write(raw)
	_ = w.Close()
	compressed := buf.Bytes()

	const wantPath = "/EURUSD/2026/06/01/08h_ticks.bi5" // July → month "06"
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == wantPath {
			_, _ = rw.Write(compressed)
			return
		}
		rw.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := testClient(srv.URL, srv.Client())
	bars, err := c.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatalf("FetchCandles: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("got %d bars; want 1 (the served hour)", len(bars))
	}
	// mid0 = (110100+110080)/2/1e5 = 1.10090 (open); mid1 = 1.10140 (close).
	if math.Abs(bars[0].Open-1.10090) > 1e-9 || math.Abs(bars[0].Close-1.10140) > 1e-9 {
		t.Errorf("bar = %+v; want open 1.10090 / close 1.10140", bars[0])
	}
}

// A short (≤48h) range that 404s everywhere is a legitimate market-closed gap: no error, no
// bars — but a LONG (>48h) all-404 range means the feed is misconfigured and must error loud.
func TestDukascopy_BrokenFeedGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// 24h all-404 → treated as closed, no error.
	if bars, err := testClient(srv.URL, srv.Client()).
		FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, from, from.Add(24*time.Hour)); err != nil || len(bars) != 0 {
		t.Errorf("short all-404 range: bars=%d err=%v; want 0/nil", len(bars), err)
	}

	// 60h all-404 (fresh client) → definitive misconfig, errors.
	_, err := testClient(srv.URL, srv.Client()).
		FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, from, from.Add(60*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "verify the") {
		t.Errorf("long all-404 range: err = %v; want the broken-feed diagnostic", err)
	}
}

// A transient fetch failure (a 503 that outlasts the retry budget) on a single hour must NOT
// abort the run — it's skipped as a data gap while other hours still produce bars. But if an
// instrument accumulates too many failures the feed is unstable and the run aborts.
func TestDukascopy_TransientFailureSkipsHour(t *testing.T) {
	hour := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	raw := append(tickRec(0, 110100, 110080, 1, 1), tickRec(30000, 110150, 110130, 1, 1)...)
	var buf bytes.Buffer
	w, _ := lzma.NewWriter(&buf)
	_, _ = w.Write(raw)
	_ = w.Close()
	good := buf.Bytes()

	// 08h always 503s (a persistently-transient hour); 09h serves valid data; others 404.
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/08h_ticks.bi5"):
			rw.WriteHeader(http.StatusServiceUnavailable)
		case strings.HasSuffix(r.URL.Path, "/09h_ticks.bi5"):
			_, _ = rw.Write(good)
		default:
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// No throttle/backoff delay: a directly-constructed client has minInterval 0, but backoff
	// still sleeps — so keep the range tiny (2 hours: the 503 hour + the good hour) and lower
	// the retry budget to 2 so the persistently-503 hour fires only one 500 ms backoff.
	c := testClient(srv.URL, srv.Client())
	c.maxAttempts = 2
	bars, err := c.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, hour, hour.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("a single transient-503 hour must not abort the run: %v", err)
	}
	if len(bars) != 1 || !bars[0].Timestamp.Equal(hour.Add(time.Hour)) {
		t.Errorf("got %d bars; want just the 09h bar (the 08h hour skipped)", len(bars))
	}
}

// A terminal non-404 4xx (e.g. 403) is NOT transient — it must propagate as a fatal error,
// not be silently converted into a skipped data-gap hour.
func TestDukascopy_Terminal4xxPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	_, err := testClient(srv.URL, srv.Client()).
		FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, from, from.Add(3*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("terminal 4xx must propagate (not skip); got %v", err)
	}
	if err != nil && errors.Is(err, errDukaTransient) {
		t.Errorf("a 403 must not be classified transient: %v", err)
	}
}

// A cancelled/deadline-exceeded context must propagate context.Canceled — not be swallowed as
// a skipped hour (which would let a cancelled backtest keep issuing requests).
func TestDukascopy_ContextCancelPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusServiceUnavailable) // would otherwise retry/backoff
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	_, err := testClient(srv.URL, srv.Client()).
		FetchCandles(ctx, "EUR/USD", Timeframe1Hour, from, from.Add(3*time.Hour))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context must propagate context.Canceled; got %v", err)
	}
}

// The broken-feed guard is PER INSTRUMENT: a valid symbol returning ticks must not mask a
// later misconfigured symbol that 404s for every hour.
func TestDukascopy_BrokenFeedGuard_PerInstrument(t *testing.T) {
	// Serve valid data only for EURUSD; 404 everything else (e.g. a wrong GBP path).
	raw := append(tickRec(0, 110100, 110080, 1, 1), tickRec(1000, 110120, 110100, 1, 1)...)
	var buf bytes.Buffer
	w, _ := lzma.NewWriter(&buf)
	_, _ = w.Write(raw)
	_ = w.Close()
	compressed := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/EURUSD/") {
			_, _ = rw.Write(compressed)
			return
		}
		rw.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := testClient(srv.URL, srv.Client())
	from := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC) // a Monday
	// EUR/USD over 60h returns bars (valid) → no error, sets its own counters nonzero.
	if bars, err := c.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, from, from.Add(60*time.Hour)); err != nil || len(bars) == 0 {
		t.Fatalf("EUR/USD: bars=%d err=%v; want data and no error", len(bars), err)
	}
	// GBP/USD (same client, all 404) over 60h must STILL error — not masked by EUR/USD's data.
	if _, err := c.FetchCandles(context.Background(), "GBP/USD", Timeframe1Hour, from, from.Add(60*time.Hour)); err == nil || !strings.Contains(err.Error(), "verify the") {
		t.Errorf("GBP/USD broken feed masked by EUR/USD data: err = %v; want the broken-feed diagnostic", err)
	}
}

func tickRec(ms, ask, bid uint32, askVol, bidVol float32) []byte {
	b := make([]byte, dukaTickBytes)
	binary.BigEndian.PutUint32(b[0:], ms)
	binary.BigEndian.PutUint32(b[4:], ask)
	binary.BigEndian.PutUint32(b[8:], bid)
	binary.BigEndian.PutUint32(b[12:], math.Float32bits(askVol))
	binary.BigEndian.PutUint32(b[16:], math.Float32bits(bidVol))
	return b
}

func TestDukascopyInstrumentAndDivisor(t *testing.T) {
	if got := dukascopyInstrument("EUR/USD"); got != "EURUSD" {
		t.Errorf("instrument = %q; want EURUSD", got)
	}
	c := &DukascopyClient{pointScale: map[string]float64{"XAU/USD": 100}}
	cases := map[string]float64{
		"EUR/USD": 100000, // 5-decimal FX
		"GBP/USD": 100000,
		"USD/JPY": 1000, // JPY pair
		"XAG/USD": 1000, // metal (default)
		"XAU/USD": 100,  // config override wins over the metal default
	}
	for tk, want := range cases {
		if got := c.priceDivisor(tk); got != want {
			t.Errorf("priceDivisor(%q) = %v; want %v", tk, got, want)
		}
	}
}

// The month in the feed path is ZERO-indexed (00 = January) — the classic Dukascopy
// off-by-one. July (Go month 7) must render as "06".
func TestDukascopyHourPath(t *testing.T) {
	h := time.Date(2026, time.July, 1, 8, 0, 0, 0, time.UTC)
	if got := dukascopyHourPath("EURUSD", h); got != "EURUSD/2026/06/01/08h_ticks.bi5" {
		t.Errorf("path = %q; want EURUSD/2026/06/01/08h_ticks.bi5 (zero-indexed month)", got)
	}
	// January → "00".
	jan := time.Date(2026, time.January, 31, 23, 0, 0, 0, time.UTC)
	if got := dukascopyHourPath("XAUUSD", jan); got != "XAUUSD/2026/00/31/23h_ticks.bi5" {
		t.Errorf("jan path = %q; want month 00", got)
	}
}

func TestDecodeTicks(t *testing.T) {
	hour := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	raw := append(tickRec(1500, 110120, 110100, 1.5, 2.0), tickRec(60000, 110200, 110180, 1.0, 1.0)...)

	ticks, err := decodeTicks(raw, hour, 100000)
	if err != nil {
		t.Fatalf("decodeTicks: %v", err)
	}
	if len(ticks) != 2 {
		t.Fatalf("got %d ticks; want 2", len(ticks))
	}
	// Tick 0: t = 08:00:01.5, mid = (110120+110100)/2/1e5 = 1.10110, vol = 3.5.
	if !ticks[0].t.Equal(hour.Add(1500 * time.Millisecond)) {
		t.Errorf("tick0 time = %v; want +1.5s", ticks[0].t)
	}
	if math.Abs(ticks[0].mid-1.10110) > 1e-9 {
		t.Errorf("tick0 mid = %v; want 1.10110", ticks[0].mid)
	}
	if math.Abs(ticks[0].vol-3.5) > 1e-6 {
		t.Errorf("tick0 vol = %v; want 3.5", ticks[0].vol)
	}
	// Length not a multiple of the record size is an error.
	if _, err := decodeTicks([]byte{0x01, 0x02}, hour, 100000); err == nil {
		t.Error("expected error on non-multiple-of-20 length")
	}
}

// parseBI5 must LZMA-decompress the classic-format payload and decode it — the end-to-end
// integration with the lzma reader.
func TestParseBI5_LZMARoundTrip(t *testing.T) {
	hour := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	raw := append(tickRec(0, 110100, 110080, 1.0, 1.0), tickRec(30000, 110150, 110130, 2.0, 2.0)...)

	var buf bytes.Buffer
	w, err := lzma.NewWriter(&buf)
	if err != nil {
		t.Fatalf("lzma.NewWriter: %v", err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ticks, err := parseBI5(buf.Bytes(), hour, 100000)
	if err != nil {
		t.Fatalf("parseBI5: %v", err)
	}
	if len(ticks) != 2 || math.Abs(ticks[1].mid-1.10140) > 1e-9 {
		t.Fatalf("round-trip mismatch: %+v", ticks)
	}
}

func TestAggregateTicks(t *testing.T) {
	base := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	mk := func(sec int, ms int, mid float64) dukaTick {
		return dukaTick{t: base.Add(time.Duration(sec)*time.Second + time.Duration(ms)*time.Millisecond), mid: mid, vol: 1}
	}
	from := base
	to := base.Add(2 * time.Minute)
	ticks := []dukaTick{
		{t: base.Add(-time.Second), mid: 9.99, vol: 1}, // before `from` → excluded
		mk(10, 0, 1.10),            // 08:00 bar open
		mk(30, 0, 1.12),            // 08:00 high
		mk(50, 0, 1.09),            // 08:00 low + close
		mk(65, 0, 1.11),            // 08:01 bar (single tick)
		{t: to, mid: 8.88, vol: 1}, // exactly `to` → excluded (half-open)
	}

	bars := aggregateTicks("EUR/USD", Timeframe1Min, time.Minute, from, to, ticks)
	if len(bars) != 2 {
		t.Fatalf("got %d bars; want 2", len(bars))
	}
	// Sorted ascending.
	if !bars[0].Timestamp.Equal(base) || !bars[1].Timestamp.Equal(base.Add(time.Minute)) {
		t.Fatalf("bar timestamps = %v, %v", bars[0].Timestamp, bars[1].Timestamp)
	}
	b0 := bars[0]
	if b0.Open != 1.10 || b0.High != 1.12 || b0.Low != 1.09 || b0.Close != 1.09 {
		t.Errorf("08:00 OHLC = %v/%v/%v/%v; want 1.10/1.12/1.09/1.09", b0.Open, b0.High, b0.Low, b0.Close)
	}
	if b0.Volume != 3 {
		t.Errorf("08:00 volume = %d; want 3 (three ticks in-bucket)", b0.Volume)
	}
	b1 := bars[1]
	if b1.Open != 1.11 || b1.High != 1.11 || b1.Low != 1.11 || b1.Close != 1.11 {
		t.Errorf("08:01 OHLC = %v/%v/%v/%v; want all 1.11", b1.Open, b1.High, b1.Low, b1.Close)
	}
}

// testClientWithCache is testClient plus an on-disk cache root.
func testClientWithCache(baseURL string, hc *http.Client, cacheDir string) *DukascopyClient {
	c := testClient(baseURL, hc)
	c.cacheDir = cacheDir
	return c
}

// lzmaBI5 packs raw tick bytes into an LZMA .bi5 body the way the feed serves them.
func lzmaBI5(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := lzma.NewWriter(&buf)
	if err != nil {
		t.Fatalf("lzma.NewWriter: %v", err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("lzma write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("lzma close: %v", err)
	}
	return buf.Bytes()
}

// The disk cache makes an hour fetch-once: a warm run populates cacheDir, then a second client
// with the SAME cacheDir and a server that would fail every request must still return the bar —
// proving it read from disk and never touched the network.
func TestDukascopy_DiskCache_ServesWithoutNetwork(t *testing.T) {
	hour := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	raw := append(tickRec(0, 110100, 110080, 1, 1), tickRec(30000, 110150, 110130, 1, 1)...)
	compressed := lzmaBI5(t, raw)
	const wantPath = "/EURUSD/2026/06/01/08h_ticks.bi5"

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == wantPath {
			hits++
			_, _ = rw.Write(compressed)
			return
		}
		rw.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()

	// Warm the cache.
	warm := testClientWithCache(srv.URL, srv.Client(), dir)
	if _, err := warm.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, hour, hour.Add(time.Hour)); err != nil {
		t.Fatalf("warm FetchCandles: %v", err)
	}
	if hits != 1 {
		t.Fatalf("warm run made %d server hits; want 1", hits)
	}

	// A fresh client with the same cacheDir but a dead server must serve from disk.
	dead := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()

	cold := testClientWithCache(dead.URL, dead.Client(), dir)
	bars, err := cold.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatalf("cold (disk) FetchCandles: %v", err)
	}
	if len(bars) != 1 || math.Abs(bars[0].Close-1.10140) > 1e-9 {
		t.Fatalf("disk-served bars = %+v; want 1 bar close 1.10140", bars)
	}
}

// A confirmed no-data (404) hour is persisted as a 0-byte marker and never re-fetched.
func TestDukascopy_DiskCache_PersistsNoDataHour(t *testing.T) {
	hour := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits++
		rw.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := testClientWithCache(srv.URL, srv.Client(), dir)
	for i := range 2 {
		bars, err := c.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, hour, hour.Add(time.Hour))
		if err != nil {
			t.Fatalf("run %d FetchCandles: %v", i, err)
		}
		if len(bars) != 0 {
			t.Fatalf("run %d bars = %d; want 0", i, len(bars))
		}
	}
	// In-memory cache short-circuits the second call in the SAME client, so this proves the
	// write path only: a brand-new client reading the marker must not hit the network.
	fresh := testClientWithCache(srv.URL, srv.Client(), dir)
	hitsBefore := hits
	if _, err := fresh.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, hour, hour.Add(time.Hour)); err != nil {
		t.Fatalf("fresh FetchCandles: %v", err)
	}
	if hits != hitsBefore {
		t.Fatalf("fresh client hit network %d times for a cached no-data hour; want 0", hits-hitsBefore)
	}
}

// A transient failure (retries exhausted → skip) must NOT be persisted, so a later run can still
// fetch the real data instead of inheriting a phantom no-data marker.
func TestDukascopy_DiskCache_DoesNotPersistTransientSkip(t *testing.T) {
	hour := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	const wantPath = "/EURUSD/2026/06/01/08h_ticks.bi5"
	raw := append(tickRec(0, 110100, 110080, 1, 1), tickRec(30000, 110150, 110130, 1, 1)...)
	compressed := lzmaBI5(t, raw)

	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		if fail {
			rw.WriteHeader(http.StatusServiceUnavailable) // 503 every attempt → transient skip
			return
		}
		_, _ = rw.Write(compressed)
	}))
	defer srv.Close()

	dir := t.TempDir()

	// First run: 503 exhausts retries → hour skipped, nothing persisted.
	skip := testClientWithCache(srv.URL, srv.Client(), dir)
	skip.maxAttempts = 2 // keep the retry loop short for the test
	if bars, err := skip.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, hour, hour.Add(time.Hour)); err != nil || len(bars) != 0 {
		t.Fatalf("transient run: bars=%d err=%v; want 0/nil (skipped)", len(bars), err)
	}

	// Second run against a now-healthy server must fetch the real bar (no phantom marker on disk).
	fail = false
	ok := testClientWithCache(srv.URL, srv.Client(), dir)
	bars, err := ok.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatalf("recovery run: %v", err)
	}
	if len(bars) != 1 || math.Abs(bars[0].Close-1.10140) > 1e-9 {
		t.Fatalf("recovery bars = %+v; want 1 bar close 1.10140 (transient skip must not have been cached)", bars)
	}
}
