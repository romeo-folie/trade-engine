package cache_test

import (
	"sync"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis/cache"
)

func resetGlobal() {
	// Reset each counter to zero individually; atomic.Int64 cannot be copied
	// after first use so we store via the Store method.
	cache.Global.Hits.Store(0)
	cache.Global.Misses.Store(0)
	cache.Global.Invalidations.Store(0)
	cache.Global.Evictions.Store(0)
	cache.Global.Errors.Store(0)
}

func TestMetricsCounters(t *testing.T) {
	resetGlobal()

	cache.RecordHit()
	cache.RecordHit()
	cache.RecordMiss()
	cache.RecordInvalidation()
	cache.RecordEviction(3)
	cache.RecordError()

	s := cache.TakeSnapshot()
	if s.Hits != 2 {
		t.Errorf("hits: want 2, got %d", s.Hits)
	}
	if s.Misses != 1 {
		t.Errorf("misses: want 1, got %d", s.Misses)
	}
	if s.Invalidations != 1 {
		t.Errorf("invalidations: want 1, got %d", s.Invalidations)
	}
	if s.Evictions != 3 {
		t.Errorf("evictions: want 3, got %d", s.Evictions)
	}
	if s.Errors != 1 {
		t.Errorf("errors: want 1, got %d", s.Errors)
	}
}

func TestMetricsHitRate(t *testing.T) {
	resetGlobal()

	cache.RecordHit()
	cache.RecordHit()
	cache.RecordHit()
	cache.RecordMiss()

	s := cache.TakeSnapshot()
	rate := s.HitRate()
	// 3 hits / 4 total = 75 %
	if rate < 74.9 || rate > 75.1 {
		t.Errorf("expected hit rate ~75%%, got %.2f%%", rate)
	}
}

func TestMetricsHitRateZeroTotal(t *testing.T) {
	resetGlobal()
	s := cache.TakeSnapshot()
	if s.HitRate() != 0 {
		t.Errorf("expected 0%% hit rate with no events, got %.2f%%", s.HitRate())
	}
}

func TestMetricsConcurrentSafe(t *testing.T) {
	resetGlobal()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.RecordHit()
			cache.RecordMiss()
		}()
	}
	wg.Wait()
	s := cache.TakeSnapshot()
	if s.Hits != 100 || s.Misses != 100 {
		t.Errorf("concurrent counter mismatch: hits=%d misses=%d", s.Hits, s.Misses)
	}
}
