package data

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ExnessCSVClient is a BACKTEST-only market-data provider backed by local Exness
// MetaTrader OHLC exports. The files are named like XAUUSD_M1_2024_exness.csv but are
// tab-separated, with headers:
//
//	<DATE> <TIME> <OPEN> <HIGH> <LOW> <CLOSE> <TICKVOL> <VOL> <SPREAD>
//
// Bars are treated as UTC. Trading costs are deliberately NOT taken from these files:
// TradeEngine still models fills through its strategy cost model, which should reflect the
// execution venue being tested, e.g. IBKR for live futures/CFD-style research.
type ExnessCSVClient struct {
	dir string

	mu       sync.Mutex
	parseMu  sync.Mutex
	yearBars map[string][]Candle // key: SYMBOL/YYYY, sanitized 1-minute bars
}

// NewExnessCSVClient creates a local Exness CSV provider. dir defaults to "docs" because the
// current research exports live there; callers can override it with markets.exness.data_dir.
func NewExnessCSVClient(dir string) *ExnessCSVClient {
	if strings.TrimSpace(dir) == "" {
		dir = "docs"
	}
	return &ExnessCSVClient{dir: dir, yearBars: make(map[string][]Candle)}
}

// FetchCandles reads every symbol-year file overlapping [from, to), filters out non-1-minute
// rows, slices the requested range, and aggregates to coarser timeframes when needed.
func (c *ExnessCSVClient) FetchCandles(ctx context.Context, ticker string, tf Timeframe, from, to time.Time) ([]Candle, error) {
	step, ok := dukascopyTimeframeStep[tf]
	if !ok {
		return nil, fmt.Errorf("exness: unsupported timeframe %q", tf)
	}
	from, to = from.UTC(), to.UTC()
	if !to.After(from) {
		return nil, nil
	}
	if subMinute(from) || subMinute(to) {
		return nil, fmt.Errorf("exness: FetchCandles needs minute-aligned bounds (1-minute granularity); got from=%s to=%s", from, to)
	}

	symbol := exnessSymbol(ticker)
	var minBars []Candle
	endYear := to.Add(-time.Minute).Year()
	for year := from.Year(); year <= endYear; year++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		bars, err := c.yearMinuteBarsFor(ctx, symbol, year)
		if err != nil {
			return nil, err
		}
		minBars = append(minBars, sliceBars(bars, from, to)...)
	}

	if tf == Timeframe1Min {
		return minBars, nil
	}
	return aggregateBars(ticker, tf, step, minBars), nil
}

func (c *ExnessCSVClient) yearMinuteBarsFor(ctx context.Context, symbol string, year int) ([]Candle, error) {
	key := fmt.Sprintf("%s/%04d", symbol, year)
	c.mu.Lock()
	if bars, ok := c.yearBars[key]; ok {
		c.mu.Unlock()
		return bars, nil
	}
	c.mu.Unlock()

	c.parseMu.Lock()
	defer c.parseMu.Unlock()
	c.mu.Lock()
	if bars, ok := c.yearBars[key]; ok {
		c.mu.Unlock()
		return bars, nil
	}
	c.mu.Unlock()

	path := filepath.Join(c.dir, fmt.Sprintf("%s_M1_%04d_exness.csv", symbol, year))
	raw, err := parseExnessFile(ctx, path, symbol)
	if err != nil {
		return nil, err
	}
	bars := sanitizeExnessM1(raw)

	c.mu.Lock()
	if cached, ok := c.yearBars[key]; ok {
		c.mu.Unlock()
		return cached, nil
	}
	c.yearBars[key] = bars
	c.mu.Unlock()
	return bars, nil
}

func parseExnessFile(ctx context.Context, path, symbol string) ([]Candle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("exness: open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("exness: read header %s: %w", path, err)
		}
		return nil, nil
	}
	header := strings.TrimSpace(sc.Text())
	if !strings.HasPrefix(header, "<DATE>\t<TIME>\t<OPEN>\t<HIGH>\t<LOW>\t<CLOSE>") {
		return nil, fmt.Errorf("exness: %s has unexpected header %q", path, header)
	}

	out := make([]Candle, 0, 400_000)
	lineNo := 1
	for sc.Scan() {
		lineNo++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 9 {
			return nil, fmt.Errorf("exness: %s:%d malformed row %q", path, lineNo, line)
		}
		ts, err := parseExnessTime(fields[0], fields[1])
		if err != nil {
			return nil, fmt.Errorf("exness: %s:%d: %w", path, lineNo, err)
		}
		open, err := strconv.ParseFloat(fields[2], 64)
		if err != nil {
			return nil, fmt.Errorf("exness: %s:%d bad open: %w", path, lineNo, err)
		}
		high, err := strconv.ParseFloat(fields[3], 64)
		if err != nil {
			return nil, fmt.Errorf("exness: %s:%d bad high: %w", path, lineNo, err)
		}
		low, err := strconv.ParseFloat(fields[4], 64)
		if err != nil {
			return nil, fmt.Errorf("exness: %s:%d bad low: %w", path, lineNo, err)
		}
		closePx, err := strconv.ParseFloat(fields[5], 64)
		if err != nil {
			return nil, fmt.Errorf("exness: %s:%d bad close: %w", path, lineNo, err)
		}
		tickVol, err := strconv.ParseFloat(fields[6], 64)
		if err != nil {
			return nil, fmt.Errorf("exness: %s:%d bad tick volume: %w", path, lineNo, err)
		}
		out = append(out, Candle{
			Ticker: symbol, Timeframe: Timeframe1Min, Timestamp: ts,
			Open: open, High: high, Low: low, Close: closePx, Volume: int64(math.Round(tickVol)),
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("exness: scan %s: %w", path, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

func sanitizeExnessM1(raw []Candle) []Candle {
	if len(raw) == 0 {
		return nil
	}
	out := make([]Candle, 0, len(raw))
	for i, b := range raw {
		prevOneMinute := i > 0 && b.Timestamp.Sub(raw[i-1].Timestamp) == time.Minute
		nextOneMinute := i+1 < len(raw) && raw[i+1].Timestamp.Sub(b.Timestamp) == time.Minute
		if prevOneMinute || nextOneMinute {
			out = append(out, b)
		}
	}
	return out
}

func parseExnessTime(dateStr, timeStr string) (time.Time, error) {
	if len(dateStr) != 10 || dateStr[4] != '.' || dateStr[7] != '.' {
		return time.Time{}, fmt.Errorf("bad date %q", dateStr)
	}
	if len(timeStr) != 8 || timeStr[2] != ':' || timeStr[5] != ':' {
		return time.Time{}, fmt.Errorf("bad time %q", timeStr)
	}
	atoi := func(s string) (int, error) { return strconv.Atoi(s) }
	yr, e1 := atoi(dateStr[0:4])
	mo, e2 := atoi(dateStr[5:7])
	dy, e3 := atoi(dateStr[8:10])
	hh, e4 := atoi(timeStr[0:2])
	mi, e5 := atoi(timeStr[3:5])
	ss, e6 := atoi(timeStr[6:8])
	for _, err := range []error{e1, e2, e3, e4, e5, e6} {
		if err != nil {
			return time.Time{}, fmt.Errorf("bad timestamp %q %q: %w", dateStr, timeStr, err)
		}
	}
	return time.Date(yr, time.Month(mo), dy, hh, mi, ss, 0, time.UTC), nil
}

func exnessSymbol(ticker string) string {
	return strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(ticker)), "/", "")
}

// FetchQuote is not supported by local historical exports.
func (c *ExnessCSVClient) FetchQuote(_ context.Context, ticker string) (*Quote, error) {
	return nil, fmt.Errorf("exness: FetchQuote not supported for %q (historical exports only)", ticker)
}

// FetchHistory is not supported: callers should use FetchCandles with an explicit range.
func (c *ExnessCSVClient) FetchHistory(_ context.Context, ticker string, _ Timeframe, _ int) ([]Candle, error) {
	return nil, fmt.Errorf("exness: FetchHistory not supported for %q (use FetchCandles with a range)", ticker)
}

var _ MarketDataProvider = (*ExnessCSVClient)(nil)
