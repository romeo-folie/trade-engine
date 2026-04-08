package web

import (
	"context"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
)

// brokerCache is a tiny in-process TTL cache in front of a BrokerView.
// It coalesces concurrent requests for the same key via a simple mutex —
// good enough given the low cardinality (4 fixed keys). Each entry is
// evicted lazily on next access after TTL expires.
//
// All TTLs are configurable so callers can pass them from config.
type brokerCache struct {
	bv execution.BrokerView

	accountTTL   time.Duration
	positionsTTL time.Duration
	historyTTL   time.Duration
	ordersTTL    time.Duration

	mu      sync.Mutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	value     any
	err       error
	expiresAt time.Time
	inflight  bool // true while a refresh is in progress
	done      chan struct{}
}

// NewBrokerCache wraps bv with per-method TTLs. Zero TTL disables caching for
// that method (calls pass through directly). Called from cmd/te/run.go.
func NewBrokerCache(bv execution.BrokerView,
	accountTTL, positionsTTL, historyTTL, ordersTTL time.Duration,
) *brokerCache {
	return &brokerCache{
		bv:           bv,
		accountTTL:   accountTTL,
		positionsTTL: positionsTTL,
		historyTTL:   historyTTL,
		ordersTTL:    ordersTTL,
		entries:      make(map[string]*cacheEntry),
	}
}

// get returns a cached value if fresh, otherwise calls fn() and caches the
// result. Concurrent callers for the same key wait on the first fetch (singleflight).
func (c *brokerCache) get(ctx context.Context, key string, ttl time.Duration, fn func(context.Context) (any, error)) (any, error) {
	if ttl == 0 {
		return fn(ctx)
	}

	c.mu.Lock()
	e, ok := c.entries[key]
	if ok && !e.inflight && time.Now().Before(e.expiresAt) {
		// Cache hit.
		v, err := e.value, e.err
		c.mu.Unlock()
		return v, err
	}
	if ok && e.inflight {
		// Another goroutine is refreshing — wait for it.
		done := e.done
		c.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.mu.Lock()
		v, err := e.value, e.err
		c.mu.Unlock()
		return v, err
	}

	// We are responsible for the refresh.
	done := make(chan struct{})
	e = &cacheEntry{inflight: true, done: done}
	c.entries[key] = e
	c.mu.Unlock()

	v, err := fn(ctx)

	c.mu.Lock()
	e.value = v
	e.err = err
	e.inflight = false
	e.expiresAt = time.Now().Add(ttl)
	c.mu.Unlock()
	close(done)

	return v, err
}

// AccountSnapshot wraps BrokerView.AccountSnapshot with the account TTL.
func (c *brokerCache) AccountSnapshot(ctx context.Context) (execution.BrokerAccount, error) {
	v, err := c.get(ctx, "account", c.accountTTL, func(ctx context.Context) (any, error) {
		return c.bv.AccountSnapshot(ctx)
	})
	if err != nil {
		return execution.BrokerAccount{}, err
	}
	return v.(execution.BrokerAccount), nil
}

// PositionsSnapshot wraps BrokerView.PositionsSnapshot with the positions TTL.
func (c *brokerCache) PositionsSnapshot(ctx context.Context) ([]execution.BrokerPosition, error) {
	v, err := c.get(ctx, "positions", c.positionsTTL, func(ctx context.Context) (any, error) {
		return c.bv.PositionsSnapshot(ctx)
	})
	if err != nil {
		return nil, err
	}
	return v.([]execution.BrokerPosition), nil
}

// EquityCurve wraps BrokerView.EquityCurve. Cache key includes period+timeframe.
func (c *brokerCache) EquityCurve(ctx context.Context, period, timeframe string) (execution.EquityHistory, error) {
	key := "history:" + period + ":" + timeframe
	v, err := c.get(ctx, key, c.historyTTL, func(ctx context.Context) (any, error) {
		return c.bv.EquityCurve(ctx, period, timeframe)
	})
	if err != nil {
		return execution.EquityHistory{}, err
	}
	return v.(execution.EquityHistory), nil
}

// RecentFills wraps BrokerView.RecentFills with the orders TTL.
func (c *brokerCache) RecentFills(ctx context.Context) ([]execution.BrokerOrder, error) {
	v, err := c.get(ctx, "orders", c.ordersTTL, func(ctx context.Context) (any, error) {
		return c.bv.RecentFills(ctx)
	})
	if err != nil {
		return nil, err
	}
	return v.([]execution.BrokerOrder), nil
}

// Invalidate clears a specific cache key, forcing the next call to re-fetch.
func (c *brokerCache) Invalidate(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}
