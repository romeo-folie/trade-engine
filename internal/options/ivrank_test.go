package options

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestIVRankOf(t *testing.T) {
	series := []float64{0.10, 0.30, 0.20}
	cases := []struct {
		current float64
		want    float64
	}{
		{0.20, 50}, {0.10, 0}, {0.30, 100}, {0.05, 0 /*clamped*/}, {0.40, 100 /*clamped*/},
	}
	for _, c := range cases {
		got, ok := ivRankOf(series, c.current)
		if !ok || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("ivRankOf(%v) = %v,%v; want %v", c.current, got, ok, c.want)
		}
	}
	// Flat window -> undefined -> not ok (fail closed).
	if _, ok := ivRankOf([]float64{0.2, 0.2, 0.2}, 0.2); ok {
		t.Error("flat series should be unavailable")
	}
	if _, ok := ivRankOf(nil, 0.2); ok {
		t.Error("empty series should be unavailable")
	}
}

// fakeIVStore is an in-memory IVStore for pipeline tests.
type fakeIVStore struct {
	rows map[string][]DailyIV
}

func newFakeIVStore() *fakeIVStore { return &fakeIVStore{rows: map[string][]DailyIV{}} }

func (f *fakeIVStore) UpsertOptionIV(_ context.Context, u string, date time.Time, iv float64, _ string) error {
	day := dayFloor(date)
	list := f.rows[u]
	for i := range list {
		if list[i].Date.Equal(day) {
			list[i].ATMIV = iv
			return nil
		}
	}
	f.rows[u] = append(list, DailyIV{Date: day, ATMIV: iv})
	return nil
}

func (f *fakeIVStore) GetIVHistory(_ context.Context, u string, from, to time.Time) ([]DailyIV, error) {
	var out []DailyIV
	for _, d := range f.rows[u] {
		if !d.Date.Before(dayFloor(from)) && !d.Date.After(dayFloor(to)) {
			out = append(out, d)
		}
	}
	return out, nil
}

func TestIVRankService_ColdStart_FailsClosed(t *testing.T) {
	st := newFakeIVStore()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// Only 10 days stored, below the 40 minSamples.
	for i := range 10 {
		st.UpsertOptionIV(context.Background(), "SPY", base.AddDate(0, 0, i), 0.15+0.001*float64(i), "derived")
	}
	svc := NewIVRankService(st, 252, 40)
	if _, ok := svc.IVRank(context.Background(), "SPY", base.AddDate(0, 0, 10)); ok {
		t.Error("expected cold-start unavailable below minSamples")
	}
}

func TestIVRankService_HappyPath(t *testing.T) {
	st := newFakeIVStore()
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	// 50 days: IV sweeps 0.10 -> 0.30, so min=0.10, max≈0.30, and the last day is
	// the max -> rank should be ~100.
	n := 50
	for i := range n {
		iv := 0.10 + 0.20*float64(i)/float64(n-1)
		st.UpsertOptionIV(context.Background(), "SPY", base.AddDate(0, 0, i), iv, "derived")
	}
	svc := NewIVRankService(st, 252, 40)
	rank, ok := svc.IVRank(context.Background(), "SPY", base.AddDate(0, 0, n))
	if !ok {
		t.Fatal("expected rank available with 50 samples")
	}
	if rank < 99 {
		t.Errorf("rank = %.2f; want ~100 (last day is the window max)", rank)
	}
}
