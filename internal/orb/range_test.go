package orb

import (
	"sync"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

func makeCandle(high, low float64, ts time.Time) data.Candle {
	return data.Candle{
		Ticker:    "TEST",
		Timeframe: data.Timeframe1Min,
		Timestamp: ts,
		Open:      (high + low) / 2,
		High:      high,
		Low:       low,
		Close:     (high + low) / 2,
		Volume:    1000,
	}
}

func TestRangeBuffer15Bars(t *testing.T) {
	buf := NewRangeBuffer("AAPL", 15)
	base := time.Date(2026, 3, 20, 9, 30, 0, 0, time.UTC)

	// Bars with known highs and lows
	for i := 0; i < 14; i++ {
		locked, r := buf.Add(makeCandle(150.0+float64(i)*0.1, 149.0-float64(i)*0.1, base.Add(time.Duration(i)*time.Minute)))
		if locked {
			t.Fatalf("buffer locked at bar %d, expected 15", i+1)
		}
		if r != nil {
			t.Fatal("expected nil range before lock")
		}
	}

	// 15th bar triggers lock
	locked, r := buf.Add(makeCandle(152.0, 148.0, base.Add(14*time.Minute)))
	if !locked {
		t.Fatal("expected buffer to lock after 15 bars")
	}
	if r == nil {
		t.Fatal("expected non-nil range after lock")
	}

	// High should be max(150.0, 150.1, ..., 151.3, 152.0) = 152.0
	if r.High != 152.0 {
		t.Errorf("High = %f, want 152.0", r.High)
	}
	// Low should be min(149.0, 148.9, ..., 147.7, 148.0) = 135.7? No, let me recalculate.
	// Bar 0: low=149.0, Bar 1: low=148.9, ..., Bar 13: low=149.0-13*0.1=147.7, Bar 14: low=148.0
	// Min = 147.7
	if r.Low != 147.7 {
		t.Errorf("Low = %f, want 147.7", r.Low)
	}
	if r.Ticker != "AAPL" {
		t.Errorf("Ticker = %q, want AAPL", r.Ticker)
	}
}

func TestRangeBuffer14BarsNotLocked(t *testing.T) {
	buf := NewRangeBuffer("TSLA", 15)
	base := time.Date(2026, 3, 20, 9, 30, 0, 0, time.UTC)

	for i := 0; i < 14; i++ {
		locked, _ := buf.Add(makeCandle(200.0, 199.0, base.Add(time.Duration(i)*time.Minute)))
		if locked {
			t.Fatalf("buffer locked at bar %d, expected 15", i+1)
		}
	}
	if buf.Range() != nil {
		t.Error("Range() should be nil before lock")
	}
}

func TestRangeBufferSubsequentAddsReturnLockedRange(t *testing.T) {
	buf := NewRangeBuffer("NVDA", 2)
	base := time.Date(2026, 3, 20, 9, 30, 0, 0, time.UTC)

	buf.Add(makeCandle(500.0, 490.0, base))
	locked, r := buf.Add(makeCandle(510.0, 495.0, base.Add(time.Minute)))
	if !locked || r == nil {
		t.Fatal("expected lock after 2 bars")
	}

	// Adding more bars should still return locked
	locked2, r2 := buf.Add(makeCandle(520.0, 480.0, base.Add(2*time.Minute)))
	if !locked2 {
		t.Fatal("expected still locked")
	}
	if r2.High != r.High || r2.Low != r.Low {
		t.Error("range should not change after lock")
	}
}

func TestRangeBufferConcurrent(t *testing.T) {
	buf := NewRangeBuffer("RACE", 10)
	base := time.Date(2026, 3, 20, 9, 30, 0, 0, time.UTC)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			buf.Add(makeCandle(100.0+float64(idx), 99.0-float64(idx), base.Add(time.Duration(idx)*time.Minute)))
		}(i)
	}
	wg.Wait()

	r := buf.Range()
	if r == nil {
		t.Fatal("expected range to be set after 20 concurrent adds with 10-bar threshold")
	}
}
