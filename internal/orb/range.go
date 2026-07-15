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
	// CloseFrac is where the final formation bar closed within the range, normalised to
	// [0,1]: 0 = at the low, 1 = at the high, 0.5 = mid-range. It captures which way price
	// was leaning as the range locked — a close near the high means buyers held the range's
	// top into the lock, a mid-range close is chop with no directional pressure. A degenerate
	// (zero-width) range yields 0.5 (neutral). Consumed by the close-position entry guard.
	CloseFrac float64
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
		lastClose := b.bars[len(b.bars)-1].Close
		closeFrac := 0.5 // neutral default for a degenerate (zero-width) range
		if high > low {
			closeFrac = (lastClose - low) / (high - low)
		}
		b.result = &OpeningRange{
			Ticker:    b.ticker,
			High:      high,
			Low:       low,
			DefinedAt: b.bars[len(b.bars)-1].Timestamp,
			CloseFrac: closeFrac,
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
