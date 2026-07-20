package data

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeExnessFile(t *testing.T, dir, symbol string, year int, rows string) {
	t.Helper()
	path := filepath.Join(dir, symbol+"_M1_"+time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006")+"_exness.csv")
	body := "<DATE>\t<TIME>\t<OPEN>\t<HIGH>\t<LOW>\t<CLOSE>\t<TICKVOL>\t<VOL>\t<SPREAD>\n" + rows
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func TestExnessCSV_FetchCandles_FiltersNonMinutePrefix(t *testing.T) {
	dir := t.TempDir()
	writeExnessFile(t, dir, "XAGUSD", 2021,
		"2021.01.04\t00:00:00\t26.00\t27.00\t25.00\t26.50\t100\t0\t0\n"+
			"2021.01.04\t01:00:00\t26.50\t27.50\t26.00\t27.00\t100\t0\t0\n"+
			"2021.07.12\t12:48:00\t23.00\t23.10\t22.90\t23.05\t10\t0\t20\n"+
			"2021.07.12\t12:49:00\t23.05\t23.20\t23.00\t23.15\t11\t0\t20\n"+
			"2021.07.12\t12:50:00\t23.15\t23.30\t23.10\t23.25\t12\t0\t20\n")

	c := NewExnessCSVClient(dir)
	from := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2021, 12, 31, 0, 0, 0, 0, time.UTC)
	bars, err := c.FetchCandles(context.Background(), "XAG/USD", Timeframe1Min, from, to)
	if err != nil {
		t.Fatalf("FetchCandles: %v", err)
	}
	if len(bars) != 3 {
		t.Fatalf("bars = %d; want 3 minute bars", len(bars))
	}
	if got := bars[0].Timestamp; !got.Equal(time.Date(2021, 7, 12, 12, 48, 0, 0, time.UTC)) {
		t.Fatalf("first bar = %s; want first M1 row", got)
	}
	if bars[0].Ticker != "XAGUSD" || bars[0].Timeframe != Timeframe1Min {
		t.Fatalf("bar metadata = %+v", bars[0])
	}
	if bars[1].Volume != 11 {
		t.Fatalf("volume = %d; want tick volume 11", bars[1].Volume)
	}
}

func TestExnessCSV_FetchCandles_AggregatesFromMinuteBars(t *testing.T) {
	dir := t.TempDir()
	writeExnessFile(t, dir, "XAUUSD", 2024,
		"2024.01.01\t23:05:00\t2000.0\t2001.0\t1999.0\t2000.5\t10\t0\t50\n"+
			"2024.01.01\t23:06:00\t2000.5\t2002.0\t2000.0\t2001.5\t20\t0\t50\n"+
			"2024.01.01\t23:07:00\t2001.5\t2001.8\t2000.8\t2001.0\t30\t0\t50\n")

	c := NewExnessCSVClient(dir)
	from := time.Date(2024, 1, 1, 23, 0, 0, 0, time.UTC)
	to := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	bars, err := c.FetchCandles(context.Background(), "XAU/USD", Timeframe1Hour, from, to)
	if err != nil {
		t.Fatalf("FetchCandles: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("bars = %d; want 1 hourly bar", len(bars))
	}
	b := bars[0]
	if b.Timestamp != from || b.Open != 2000.0 || b.High != 2002.0 || b.Low != 1999.0 || b.Close != 2001.0 || b.Volume != 60 {
		t.Fatalf("hourly bar = %+v", b)
	}
}
