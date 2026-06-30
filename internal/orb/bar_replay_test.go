package orb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

func TestBarReplayTimeframe(t *testing.T) {
	cases := []struct {
		age  time.Duration
		want data.Timeframe
	}{
		{2 * time.Hour, data.Timeframe1Min},
		{24 * time.Hour, data.Timeframe1Min},
		{25 * time.Hour, data.Timeframe1Hour},
		{20 * 24 * time.Hour, data.Timeframe1Hour},
		{28 * 24 * time.Hour, data.Timeframe1Hour},
		{29 * 24 * time.Hour, data.Timeframe1Day},
		{120 * 24 * time.Hour, data.Timeframe1Day},
	}
	for _, tc := range cases {
		if got := barReplayTimeframe(tc.age); got != tc.want {
			t.Errorf("barReplayTimeframe(%v) = %v; want %v", tc.age, got, tc.want)
		}
	}
}

func TestBarReplayTimeframeFallbacks(t *testing.T) {
	// Fresh → finest first, escalating to daily.
	if got := barReplayTimeframeFallbacks(time.Hour); len(got) != 3 || got[0] != data.Timeframe1Min || got[2] != data.Timeframe1Day {
		t.Errorf("fresh fallbacks = %v", got)
	}
	// Month-old → hourly then daily.
	if got := barReplayTimeframeFallbacks(20 * 24 * time.Hour); len(got) != 2 || got[0] != data.Timeframe1Hour || got[1] != data.Timeframe1Day {
		t.Errorf("month fallbacks = %v", got)
	}
	// Ancient → daily only.
	if got := barReplayTimeframeFallbacks(120 * 24 * time.Hour); len(got) != 1 || got[0] != data.Timeframe1Day {
		t.Errorf("ancient fallbacks = %v", got)
	}
}

// stubMarketData returns canned candles (and/or an error) per timeframe and records
// which timeframes were requested, so tests can assert the fallback walk.
type stubMarketData struct {
	byTF      map[data.Timeframe][]data.Candle
	errTF     map[data.Timeframe]error
	requested []data.Timeframe
}

func (m *stubMarketData) FetchCandles(_ context.Context, _ string, tf data.Timeframe, _, _ time.Time) ([]data.Candle, error) {
	m.requested = append(m.requested, tf)
	if m.errTF != nil {
		if err, ok := m.errTF[tf]; ok {
			return nil, err
		}
	}
	return m.byTF[tf], nil
}
func (m *stubMarketData) FetchQuote(context.Context, string) (*data.Quote, error) { return nil, nil }
func (m *stubMarketData) FetchHistory(context.Context, string, data.Timeframe, int) ([]data.Candle, error) {
	return nil, nil
}

func dayBar(d int, high, low float64) data.Candle {
	return data.Candle{
		Timestamp: time.Date(2026, 4, d, 0, 0, 0, 0, time.UTC),
		High:      high, Low: low, Open: high, Close: low,
	}
}

func TestResolveExitViaBarReplay_FreshResolvesOnMinute(t *testing.T) {
	// A trade filled an hour ago: 1-min bars exist and hit the target.
	md := &stubMarketData{byTF: map[data.Timeframe][]data.Candle{
		data.Timeframe1Min: {bar(1, 160, 150), bar(2, 172, 165)},
	}}
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	res, err := ResolveExitViaBarReplay(context.Background(), md, "EUR/USD", "buy", 140, 170, from, now, now)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !res.Found || res.ExitType != "tp" || res.ExitPrice != 170 {
		t.Fatalf("got %+v; want tp@170 found", res)
	}
	if res.Timeframe != data.Timeframe1Min {
		t.Errorf("timeframe = %v; want 1min", res.Timeframe)
	}
	if len(md.requested) != 1 {
		t.Errorf("requested %v; want only 1min", md.requested)
	}
}

func TestResolveExitViaBarReplay_FallsBackToDailyWhenFinerEmpty(t *testing.T) {
	// An old trade: minute/hourly history no longer retained (empty), daily has it.
	md := &stubMarketData{byTF: map[data.Timeframe][]data.Candle{
		data.Timeframe1Min:  {},
		data.Timeframe1Hour: {},
		data.Timeframe1Day:  {dayBar(13, 155, 150), dayBar(14, 152, 138)},
	}}
	now := time.Now().UTC()
	from := now.Add(-60 * 24 * time.Hour) // 60 days old → starts at daily, but ensure walk works
	res, err := ResolveExitViaBarReplay(context.Background(), md, "EUR/USD", "buy", 140, 170, from, now, now)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !res.Found || res.ExitType != "stop" || res.ExitPrice != 140 {
		t.Fatalf("got %+v; want stop@140 found", res)
	}
	if res.Timeframe != data.Timeframe1Day {
		t.Errorf("timeframe = %v; want 1day", res.Timeframe)
	}
}

func TestResolveExitViaBarReplay_BarsExistButNeverTouched(t *testing.T) {
	md := &stubMarketData{byTF: map[data.Timeframe][]data.Candle{
		data.Timeframe1Min: {bar(1, 155, 145), bar(2, 158, 148)},
	}}
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	res, err := ResolveExitViaBarReplay(context.Background(), md, "EUR/USD", "buy", 140, 170, from, now, now)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Found {
		t.Fatalf("expected still-open, got %+v", res)
	}
}

func TestResolveExitViaBarReplay_AllTimeframesErrorReturnsError(t *testing.T) {
	boom := errors.New("gateway down")
	md := &stubMarketData{errTF: map[data.Timeframe]error{
		data.Timeframe1Min:  boom,
		data.Timeframe1Hour: boom,
		data.Timeframe1Day:  boom,
	}}
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	_, err := ResolveExitViaBarReplay(context.Background(), md, "EUR/USD", "buy", 140, 170, from, now, now)
	if err == nil {
		t.Fatal("expected error when every timeframe fails")
	}
}

func TestResolveExitViaBarReplay_NoDataNoErrorIsStillOpen(t *testing.T) {
	// Every timeframe returns empty with no error — not an error, just unresolved.
	md := &stubMarketData{byTF: map[data.Timeframe][]data.Candle{}}
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	res, err := ResolveExitViaBarReplay(context.Background(), md, "EUR/USD", "buy", 140, 170, from, now, now)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Found {
		t.Fatalf("expected not found, got %+v", res)
	}
}
