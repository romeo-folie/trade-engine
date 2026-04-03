package orb

import (
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

// OpeningRange represents the high/low of the first N minutes of trading.
type OpeningRange struct {
	Ticker    string
	High      float64
	Low       float64
	DefinedAt time.Time
}

// RangeBuffer accumulates 1-min candles and locks the opening range
// once the configured number of bars has been received.
type RangeBuffer struct {
	mu           sync.Mutex
	ticker       string
	rangeMinutes int
	bars         []data.Candle
	timestamps   map[time.Time]bool
	locked       bool
	result       *OpeningRange
}

// NewRangeBuffer creates a buffer that locks after rangeMinutes bars.
func NewRangeBuffer(ticker string, rangeMinutes int) *RangeBuffer {
	return &RangeBuffer{
		ticker:       ticker,
		rangeMinutes: rangeMinutes,
		timestamps:   make(map[time.Time]bool),
	}
}

// Add appends a 1-min candle. Returns (true, *OpeningRange) when the range is locked.
func (b *RangeBuffer) Add(candle data.Candle) (bool, *OpeningRange) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.locked {
		return true, b.result
	}
	// De-duplicate: only add unique timestamps.
	// This guards against repeated FetchCandles calls that return overlapping data.
	if b.timestamps[candle.Timestamp] {
		return b.locked, b.result
	}
	b.timestamps[candle.Timestamp] = true
	b.bars = append(b.bars, candle)

	if len(b.bars) >= b.rangeMinutes {
		high, low := b.bars[0].High, b.bars[0].Low
		for _, bar := range b.bars[1:] {
			if bar.High > high {
				high = bar.High
			}
			if bar.Low < low {
				low = bar.Low
			}
		}
		b.result = &OpeningRange{
			Ticker:    b.ticker,
			High:      high,
			Low:       low,
			DefinedAt: b.bars[len(b.bars)-1].Timestamp,
		}
		b.locked = true
	}
	return b.locked, b.result
}

// Range returns the computed opening range, or nil if not yet locked.
func (b *RangeBuffer) Range() *OpeningRange {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.result
}
