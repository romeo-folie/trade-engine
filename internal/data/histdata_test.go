package data

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testTok = "0123456789abcdef0123456789abcdef"

// zipWithCSV builds a HistData-style month archive: a .csv of tick rows plus a .txt sidecar
// (which the parser must ignore).
func zipWithCSV(t *testing.T, name, csv string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name + ".csv")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write([]byte(csv)); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	tw, _ := zw.Create(name + ".txt")
	_, _ = tw.Write([]byte("HistData status report\n"))
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// histDataTestServer serves the token page on GET and the given ZIP on a valid get.php POST.
// It records how many POSTs it received so a test can assert the disk cache prevented network.
func histDataTestServer(t *testing.T, zipBytes []byte, posts *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "download-free-forex-data"):
			fmt.Fprintf(rw, `<html><input type="hidden" id="tk" name="tk" value="%s"></html>`, testTok)
		case r.Method == http.MethodPost && r.URL.Path == "/get.php":
			if posts != nil {
				*posts++
			}
			_ = r.ParseForm()
			if r.FormValue("tk") != testTok {
				http.Error(rw, "bad token", http.StatusForbidden)
				return
			}
			if r.Header.Get("Referer") == "" {
				http.Error(rw, "missing referer", http.StatusForbidden)
				return
			}
			rw.Write(zipBytes)
		default:
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTestHistData(t *testing.T, baseURL, cacheDir string, hc *http.Client) *HistDataClient {
	t.Helper()
	c, err := NewHistDataClient(baseURL, cacheDir)
	if err != nil {
		t.Fatalf("NewHistDataClient: %v", err)
	}
	if hc != nil {
		c.httpClient = hc
	}
	return c
}

// The crux of the provider: HistData timestamps are US Eastern WITH DST. A summer time must
// resolve at UTC-4 (EDT) and a winter time at UTC-5 (EST) — a fixed offset would be wrong for
// one of them. These are the exact conversions verified against Dukascopy on live data.
func TestHistData_ParseTime_DST(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	cases := []struct {
		in     string
		wantUTC string
	}{
		{"20250501 080000000", "2025-05-01 12:00:00"}, // EDT: 08:00 - (-4) = 12:00 UTC
		{"20241105 080000000", "2024-11-05 13:00:00"}, // EST: 08:00 - (-5) = 13:00 UTC
		{"20250501 043015146", "2025-05-01 08:30:15.146"},
	}
	for _, tc := range cases {
		got, err := parseHistDataTime(tc.in, ny)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.in, err)
		}
		want, _ := time.Parse("2006-01-02 15:04:05.999", tc.wantUTC)
		if !got.Equal(want) {
			t.Errorf("parse %q = %s; want %s UTC", tc.in, got.Format("2006-01-02 15:04:05.000"), tc.wantUTC)
		}
		if got.Location() != time.UTC {
			t.Errorf("parse %q returned non-UTC location %v", tc.in, got.Location())
		}
	}
}

func TestHistData_EnumerateMonths(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	from := time.Date(2024, 11, 20, 0, 0, 0, 0, ny)
	to := time.Date(2025, 2, 3, 0, 0, 0, 0, ny)
	got := enumerateHistDataMonths(from, to)
	want := []histDataYM{{2024, 11}, {2024, 12}, {2025, 1}, {2025, 2}}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v; want %v", got, want)
		}
	}
}

// End-to-end over HTTP: scrape token → POST with Referer → unzip → parse ET ticks → aggregate to
// a UTC hourly bar. Two ticks in the same UTC hour form one bar with mid open/close.
func TestHistData_FetchCandles_HTTPPipeline(t *testing.T) {
	// ET 04:00 and 04:30 on 2025-05-01 (EDT) → UTC 08:00 and 08:30 → same 08:00 UTC bar.
	csv := "20250501 040000000,1.10080,1.10100,0\n" + // mid 1.10090 (open)
		"20250501 043000000,1.10140,1.10160,0\n" // mid 1.10150 (close/high)
	zb := zipWithCSV(t, "DAT_ASCII_EURUSD_T_202505", csv)
	srv := histDataTestServer(t, zb, nil)
	defer srv.Close()

	c := newTestHistData(t, srv.URL, "", srv.Client())
	from := time.Date(2025, 5, 1, 8, 0, 0, 0, time.UTC)
	bars, err := c.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, from, from.Add(time.Hour))
	if err != nil {
		t.Fatalf("FetchCandles: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("got %d bars; want 1", len(bars))
	}
	b := bars[0]
	if !b.Timestamp.Equal(from) {
		t.Errorf("bar ts = %s; want %s", b.Timestamp, from)
	}
	if math.Abs(b.Open-1.10090) > 1e-9 || math.Abs(b.Close-1.10150) > 1e-9 || math.Abs(b.High-1.10150) > 1e-9 {
		t.Errorf("bar = %+v; want open 1.10090 close/high 1.10150", b)
	}
}

// A warm on-disk cache serves the month without any network: a second client with the same
// cacheDir and a dead server still returns the bar.
func TestHistData_DiskCache_ServesWithoutNetwork(t *testing.T) {
	csv := "20250501 040000000,1.10080,1.10100,0\n"
	zb := zipWithCSV(t, "DAT_ASCII_EURUSD_T_202505", csv)
	var posts int
	srv := histDataTestServer(t, zb, &posts)
	defer srv.Close()
	dir := t.TempDir()
	from := time.Date(2025, 5, 1, 8, 0, 0, 0, time.UTC)

	warm := newTestHistData(t, srv.URL, dir, srv.Client())
	if _, err := warm.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, from, from.Add(time.Hour)); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if posts != 1 {
		t.Fatalf("warm posts = %d; want 1", posts)
	}

	// Fresh client, same cacheDir, dead server → must read the cached ZIP off disk.
	dead := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()
	cold := newTestHistData(t, dead.URL, dir, dead.Client())
	bars, err := cold.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, from, from.Add(time.Hour))
	if err != nil {
		t.Fatalf("cold (disk): %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("disk-served bars = %d; want 1", len(bars))
	}
}

// A non-ZIP body (an HTML error page) must fail loudly, not be treated as empty data.
func TestHistData_NonZipResponseErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprintf(rw, `<input id="tk" value="%s">`, testTok)
			return
		}
		fmt.Fprint(rw, "<html>error: not available</html>") // 200 but not a ZIP
	}))
	defer srv.Close()
	c := newTestHistData(t, srv.URL, "", srv.Client())
	from := time.Date(2025, 5, 1, 8, 0, 0, 0, time.UTC)
	if _, err := c.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, from, from.Add(time.Hour)); err == nil {
		t.Fatal("expected error on non-ZIP response, got nil")
	}
}

// A corrupt (non-parseable) cached ZIP must be treated as a miss and re-downloaded, not error.
func TestHistData_DiskCache_CorruptEntryReDownloads(t *testing.T) {
	csv := "20250501 040000000,1.10080,1.10100,0\n"
	zb := zipWithCSV(t, "DAT_ASCII_EURUSD_T_202505", csv)
	var posts int
	srv := histDataTestServer(t, zb, &posts)
	defer srv.Close()
	dir := t.TempDir()

	// Pre-seed a garbage "PK...." file at the expected cache path.
	corrupt := filepath.Join(dir, "histdata", "EURUSD", "202505.zip")
	if err := os.MkdirAll(filepath.Dir(corrupt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt, []byte("PK\x03\x04garbage-not-a-zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := newTestHistData(t, srv.URL, dir, srv.Client())
	from := time.Date(2025, 5, 1, 8, 0, 0, 0, time.UTC)
	bars, err := c.FetchCandles(context.Background(), "EUR/USD", Timeframe1Hour, from, from.Add(time.Hour))
	if err != nil {
		t.Fatalf("corrupt cache should self-heal, got: %v", err)
	}
	if len(bars) != 1 || posts != 1 {
		t.Fatalf("bars=%d posts=%d; want 1 bar re-downloaded once", len(bars), posts)
	}
	// The cache file must now be the valid re-downloaded ZIP.
	if b, _ := os.ReadFile(corrupt); !bytes.Equal(b, zb) {
		t.Error("cache not overwritten with valid ZIP after self-heal")
	}
}

// The bar-cache path (ticks → 1-minute bars → re-aggregate to a coarser tf) must be identical to
// aggregating the raw ticks directly. This is the correctness guarantee behind caching bars
// instead of ticks. Covers multiple minutes across an hour boundary and a range-aligned query.
func TestHistData_BarCacheEqualsTickAggregation(t *testing.T) {
	base := time.Date(2025, 5, 1, 8, 0, 0, 0, time.UTC)
	var ticks []dukaTick
	// ~90 minutes of ticks, a few per minute with varied prices.
	for m := range 90 {
		for s := range 3 {
			ts := base.Add(time.Duration(m)*time.Minute + time.Duration(s*17)*time.Second)
			mid := 1.10000 + float64((m*7+s*3)%50)*0.0001 // deterministic wiggle
			ticks = append(ticks, dukaTick{t: ts, mid: mid, vol: 1})
		}
	}
	from, to := base, base.Add(90*time.Minute)

	for _, tc := range []struct {
		tf   Timeframe
		step time.Duration
	}{
		{Timeframe1Min, time.Minute},
		{Timeframe5Min, 5 * time.Minute},
		{Timeframe15Min, 15 * time.Minute},
		{Timeframe1Hour, time.Hour},
	} {
		ref := aggregateTicks("EUR/USD", tc.tf, tc.step, from, to, ticks)

		// Bar-cache path: ticks → 1-minute bars → slice → re-aggregate.
		minBars := aggregateTicksToMinute("EUR/USD", ticks)
		window := sliceBars(minBars, from, to)
		var got []Candle
		if tc.tf == Timeframe1Min {
			got = window
		} else {
			got = aggregateBars("EUR/USD", tc.tf, tc.step, window)
		}

		if len(got) != len(ref) {
			t.Fatalf("%s: got %d bars, ref %d", tc.tf, len(got), len(ref))
		}
		for i := range ref {
			r, g := ref[i], got[i]
			if !g.Timestamp.Equal(r.Timestamp) ||
				math.Abs(g.Open-r.Open) > 1e-12 || math.Abs(g.High-r.High) > 1e-12 ||
				math.Abs(g.Low-r.Low) > 1e-12 || math.Abs(g.Close-r.Close) > 1e-12 {
				t.Errorf("%s bar %d mismatch:\n  ref=%+v\n  got=%+v", tc.tf, i, r, g)
			}
		}
	}
}

// A sub-minute range must floor to whole minutes and return the same bars as the minute-aligned
// query — deterministic, not the silent edge-bar drop/keep of raw bucket-start comparison.
// A 1-minute-granular provider cannot honour a sub-minute half-open range, so it must reject
// non-minute-aligned bounds (fail loud) rather than silently return subtly wrong bars. A
// minute-aligned query still works.
func TestHistData_FetchCandles_RejectsSubMinuteRange(t *testing.T) {
	c := newTestHistData(t, "http://unused.invalid", "", nil)
	ctx := context.Background()
	aligned := time.Date(2025, 5, 1, 8, 0, 0, 0, time.UTC)
	end := time.Date(2025, 5, 1, 9, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name     string
		from, to time.Time
	}{
		{"sub-second from", aligned.Add(500 * time.Nanosecond), end},
		{"sub-minute from", aligned.Add(30 * time.Second), end},
		{"sub-minute to", aligned, end.Add(45 * time.Second)},
	} {
		if _, err := c.FetchCandles(ctx, "EUR/USD", Timeframe1Hour, tc.from, tc.to); err == nil {
			t.Errorf("%s: expected error for non-minute-aligned bounds, got nil", tc.name)
		}
	}

	// Minute-aligned bounds are accepted (no data served here, but no alignment error and it must
	// not panic — the request reaches the fetch path).
	if _, err := c.FetchCandles(ctx, "EUR/USD", Timeframe1Hour, aligned, end); err != nil {
		if strings.Contains(err.Error(), "minute-aligned") {
			t.Errorf("minute-aligned query wrongly rejected: %v", err)
		}
	}
}

func TestHistData_SubMinuteHelper(t *testing.T) {
	base := time.Date(2025, 5, 1, 8, 0, 0, 0, time.UTC)
	if subMinute(base) {
		t.Error("whole-minute time reported sub-minute")
	}
	if !subMinute(base.Add(time.Second)) || !subMinute(base.Add(time.Nanosecond)) {
		t.Error("sub-minute time not detected")
	}
}

// The one timeframe where a bucket spans the ET-month seam is 1-day (the seam lands at a UTC hour,
// mid-day). Aggregating each month's ticks to 1-minute bars separately, concatenating, and
// re-bucketing to 1-day must equal aggregating all ticks together.
func TestHistData_SeamConcatenation_DayBucket(t *testing.T) {
	day := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	var prevMonth, newMonth, all []dukaTick
	mk := func(h, m int, mid float64) dukaTick {
		return dukaTick{t: day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute), mid: mid, vol: 1}
	}
	// Prev ET month contributes 00:00–03:59 UTC of May 1; new ET month contributes 04:00 UTC on
	// (the seam is UTC 04:00 = ET midnight in EDT). Prices chosen so open/high/low/close are
	// unambiguous across the seam.
	for h := range 4 {
		tk := mk(h, 30, 1.100+float64(h)*0.001) // 1.100,1.101,1.102,1.103
		prevMonth = append(prevMonth, tk)
		all = append(all, tk)
	}
	for h := 4; h < 8; h++ {
		tk := mk(h, 30, 1.110-float64(h-4)*0.001) // 1.110,1.109,1.108,1.107
		newMonth = append(newMonth, tk)
		all = append(all, tk)
	}

	ref := aggregateTicks("EUR/USD", Timeframe1Day, 24*time.Hour, time.Time{}, histDataMaxTime, all)
	// Per-month path: aggregate each month to 1-minute, concatenate, re-bucket to 1-day.
	concat := append(aggregateTicksToMinute("EUR/USD", prevMonth), aggregateTicksToMinute("EUR/USD", newMonth)...)
	got := aggregateBars("EUR/USD", Timeframe1Day, 24*time.Hour, concat)

	if len(ref) != 1 || len(got) != 1 {
		t.Fatalf("ref=%d got=%d bars; want 1 day bar each", len(ref), len(got))
	}
	r, g := ref[0], got[0]
	if !g.Timestamp.Equal(r.Timestamp) || g.Open != r.Open || g.High != r.High || g.Low != r.Low || g.Close != r.Close {
		t.Errorf("seam day bar mismatch (per-month path must equal direct tick aggregation):\n ref=%+v\n got=%+v", r, g)
	}
	// Sanity: the day bar must actually span the seam (open from prev month, close from new month).
	eq := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if !eq(g.Open, 1.100) || !eq(g.Close, 1.107) || !eq(g.High, 1.110) || !eq(g.Low, 1.100) {
		t.Errorf("day bar OHLC = %.3f/%.3f/%.3f/%.3f; want 1.100/1.110/1.100/1.107 (open pre-seam, close post-seam)", g.Open, g.High, g.Low, g.Close)
	}
}

func TestHistData_PairMapping(t *testing.T) {
	for _, tc := range []struct{ in, url, fx string }{
		{"EUR/USD", "eurusd", "EURUSD"},
		{"XAU/USD", "xauusd", "XAUUSD"},
	} {
		u, fx := histDataPair(tc.in)
		if u != tc.url || fx != tc.fx {
			t.Errorf("histDataPair(%q) = %q,%q; want %q,%q", tc.in, u, fx, tc.url, tc.fx)
		}
	}
}
