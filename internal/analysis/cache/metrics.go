package cache

import (
	"sync/atomic"
)

// Metrics tracks cache hit/miss/invalidation/eviction counters.
// All fields are updated atomically; safe for concurrent use.
// Counters are process-lifetime monotonic values — reset only on restart.
//
// These counters are exposed to the TUI and reports layers via Snapshot().
// If Prometheus is added to go.mod in a future phase, these can be bridged
// to prometheus.Counter via a thin adapter without changing call sites.
//
// Metrics is exported so tests can reset the global with `cache.Global = cache.Metrics{}`.
type Metrics struct {
	Hits         atomic.Int64
	Misses       atomic.Int64
	Invalidations atomic.Int64
	Evictions    atomic.Int64 // TTL sweep deletions
	Errors       atomic.Int64 // store errors on read/write
}

// Global is the process-wide cache metrics singleton.
var Global Metrics

// RecordHit increments the hit counter.
func RecordHit()         { Global.Hits.Add(1) }

// RecordMiss increments the miss counter.
func RecordMiss()        { Global.Misses.Add(1) }

// RecordInvalidation increments the invalidation counter.
func RecordInvalidation() { Global.Invalidations.Add(1) }

// RecordEviction increments the TTL eviction counter by n.
func RecordEviction(n int64) { Global.Evictions.Add(n) }

// RecordError increments the store error counter.
func RecordError()       { Global.Errors.Add(1) }

// Snapshot returns a point-in-time copy of all counters.
type Snapshot struct {
	Hits         int64
	Misses       int64
	Invalidations int64
	Evictions    int64
	Errors       int64
}

// TakeSnapshot returns a consistent snapshot of current counter values.
func TakeSnapshot() Snapshot {
	return Snapshot{
		Hits:         Global.Hits.Load(),
		Misses:       Global.Misses.Load(),
		Invalidations: Global.Invalidations.Load(),
		Evictions:    Global.Evictions.Load(),
		Errors:       Global.Errors.Load(),
	}
}

// HitRate returns hits / (hits + misses) as a percentage, or 0 when no requests.
func (s Snapshot) HitRate() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total) * 100
}
