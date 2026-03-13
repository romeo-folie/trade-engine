package backtest

import (
	"sync"
	"testing"
)

// TestPromptCache_HitRate tests the in-memory hit/miss counter logic directly.
func TestPromptCache_HitRate_HitsOnly(t *testing.T) {
	c := NewPromptCache()
	c.hits.Add(3)
	if c.HitRate() != 1.0 {
		t.Errorf("expected hit rate 1.0 with all hits, got %.2f", c.HitRate())
	}
}

func TestPromptCache_HitRate_MissesOnly(t *testing.T) {
	c := NewPromptCache()
	c.misses.Add(5)
	if c.HitRate() != 0.0 {
		t.Errorf("expected hit rate 0.0 with all misses, got %.2f", c.HitRate())
	}
}

func TestPromptCache_HitRate_Mixed(t *testing.T) {
	c := NewPromptCache()
	c.hits.Add(3)
	c.misses.Add(1)
	got := c.HitRate()
	want := 0.75
	if got != want {
		t.Errorf("expected hit rate %.2f, got %.2f", want, got)
	}
}

func TestPromptCache_HitRate_Zero(t *testing.T) {
	c := NewPromptCache()
	if c.HitRate() != 0 {
		t.Errorf("expected 0 hit rate before any lookups, got %.2f", c.HitRate())
	}
}

func TestPromptCache_Stats(t *testing.T) {
	c := NewPromptCache()
	c.hits.Add(7)
	c.misses.Add(3)
	hits, misses := c.Stats()
	if hits != 7 {
		t.Errorf("expected 7 hits, got %d", hits)
	}
	if misses != 3 {
		t.Errorf("expected 3 misses, got %d", misses)
	}
}

func TestPromptCache_ConcurrentCounters(t *testing.T) {
	c := NewPromptCache()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.hits.Add(1)
			c.misses.Add(1)
		}()
	}
	wg.Wait()
	hits, misses := c.Stats()
	if hits != 100 || misses != 100 {
		t.Errorf("expected 100/100, got %d/%d (race condition?)", hits, misses)
	}
}


