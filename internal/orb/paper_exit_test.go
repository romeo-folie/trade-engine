package orb

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

// bar is a tiny helper to build a 1-minute candle at minute offset m with the given
// high/low. Open/Close are unused by the resolver so they mirror the high.
func bar(m int, high, low float64) data.Candle {
	return data.Candle{
		Timestamp: time.Date(2026, 4, 13, 14, m, 0, 0, time.UTC),
		High:      high,
		Low:       low,
		Open:      high,
		Close:     low,
	}
}

// TestResolveExitFromCandles covers the pure bars-walking exit logic the paper-mode
// reconciler uses to turn a price path into a TP/stop close for a filled ORB trade.
func TestResolveExitFromCandles(t *testing.T) {
	// Long bracket: stop 140 below entry, target 170 above.
	const buyStop, buyTP = 140.0, 170.0
	// Short bracket: stop 170 above entry, target 140 below.
	const sellStop, sellTP = 170.0, 140.0

	cases := []struct {
		name       string
		side       string
		stopLoss   float64
		takeProfit float64
		candles    []data.Candle
		wantType   string
		wantPrice  float64
		wantMinute int // timestamp minute of the closing bar; -1 when not found
	}{
		{
			name: "buy never reaches a leg stays open", side: "buy", stopLoss: buyStop, takeProfit: buyTP,
			candles:  []data.Candle{bar(1, 155, 145), bar(2, 160, 150)},
			wantType: "", wantMinute: -1,
		},
		{
			name: "buy hits target", side: "buy", stopLoss: buyStop, takeProfit: buyTP,
			candles:  []data.Candle{bar(1, 160, 150), bar(2, 172, 165)},
			wantType: "tp", wantPrice: buyTP, wantMinute: 2,
		},
		{
			name: "buy hits stop", side: "buy", stopLoss: buyStop, takeProfit: buyTP,
			candles:  []data.Candle{bar(1, 155, 150), bar(2, 152, 138)},
			wantType: "stop", wantPrice: buyStop, wantMinute: 2,
		},
		{
			name: "buy stop fires before later target", side: "buy", stopLoss: buyStop, takeProfit: buyTP,
			candles:  []data.Candle{bar(1, 160, 139), bar(2, 175, 168)},
			wantType: "stop", wantPrice: buyStop, wantMinute: 1,
		},
		{
			name: "buy straddle bar resolves to stop (conservative)", side: "buy", stopLoss: buyStop, takeProfit: buyTP,
			candles:  []data.Candle{bar(1, 175, 138)},
			wantType: "stop", wantPrice: buyStop, wantMinute: 1,
		},
		{
			name: "sell hits target", side: "sell", stopLoss: sellStop, takeProfit: sellTP,
			candles:  []data.Candle{bar(1, 160, 150), bar(2, 148, 138)},
			wantType: "tp", wantPrice: sellTP, wantMinute: 2,
		},
		{
			name: "sell hits stop", side: "sell", stopLoss: sellStop, takeProfit: sellTP,
			candles:  []data.Candle{bar(1, 165, 155), bar(2, 172, 168)},
			wantType: "stop", wantPrice: sellStop, wantMinute: 2,
		},
		{
			name: "sell straddle bar resolves to stop (conservative)", side: "sell", stopLoss: sellStop, takeProfit: sellTP,
			candles:  []data.Candle{bar(1, 172, 138)},
			wantType: "stop", wantPrice: sellStop, wantMinute: 1,
		},
		{
			name: "missing target never fires tp", side: "buy", stopLoss: buyStop, takeProfit: 0,
			candles:  []data.Candle{bar(1, 500, 145)},
			wantType: "", wantMinute: -1,
		},
		{
			name: "missing stop never fires stop", side: "buy", stopLoss: 0, takeProfit: buyTP,
			candles:  []data.Candle{bar(1, 160, 1)},
			wantType: "", wantMinute: -1,
		},
		{
			name: "empty bars stays open", side: "buy", stopLoss: buyStop, takeProfit: buyTP,
			candles:  nil,
			wantType: "", wantMinute: -1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotPrice, gotTime, found := resolveExitFromCandles(tc.side, tc.stopLoss, tc.takeProfit, tc.candles)
			wantFound := tc.wantType != ""
			if found != wantFound {
				t.Fatalf("found = %v; want %v", found, wantFound)
			}
			if gotType != tc.wantType || gotPrice != tc.wantPrice {
				t.Errorf("got (%q, %v); want (%q, %v)", gotType, gotPrice, tc.wantType, tc.wantPrice)
			}
			if wantFound && gotTime.Minute() != tc.wantMinute {
				t.Errorf("exit bar minute = %d; want %d", gotTime.Minute(), tc.wantMinute)
			}
		})
	}
}

// TestResolveExitFromCandles_PnLDirection confirms the level chosen by the resolver,
// fed through exitPnL, yields the expected sign: a target is a win, a stop is a loss,
// on both sides.
func TestResolveExitFromCandles_PnLDirection(t *testing.T) {
	const qty = 10
	cases := []struct {
		name     string
		side     string
		fill     float64
		stop, tp float64
		candles  []data.Candle
		wantWin  bool
	}{
		{"buy target is a win", "buy", 150, 140, 170, []data.Candle{bar(1, 172, 168)}, true},
		{"buy stop is a loss", "buy", 150, 140, 170, []data.Candle{bar(1, 145, 138)}, false},
		{"sell target is a win", "sell", 150, 170, 140, []data.Candle{bar(1, 142, 138)}, true},
		{"sell stop is a loss", "sell", 150, 170, 140, []data.Candle{bar(1, 172, 168)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, exitPrice, _, found := resolveExitFromCandles(tc.side, tc.stop, tc.tp, tc.candles)
			if !found {
				t.Fatal("expected an exit")
			}
			pnl := exitPnL(tc.side, tc.fill, exitPrice, qty)
			if tc.wantWin && pnl <= 0 {
				t.Errorf("expected win, got pnl=%v", pnl)
			}
			if !tc.wantWin && pnl >= 0 {
				t.Errorf("expected loss, got pnl=%v", pnl)
			}
		})
	}
}
