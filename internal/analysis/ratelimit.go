package analysis

import (
	"context"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// modelRateLimiter paces outbound requests for a single model so the process
// stays under a provider's per-model rate quota. Each caller reserves the next
// time slot atomically under the lock, so N concurrent goroutines are
// serialised into an evenly-spaced schedule instead of all reading a stale
// "last call" timestamp and bursting together. (That stale-read race is exactly
// why the old enricher limiter let traffic spike past the free-tier quota.)
type modelRateLimiter struct {
	mu          sync.Mutex
	minInterval time.Duration
	nextFree    time.Time
}

// newModelRateLimiter returns a limiter pacing to rpm requests per minute, or
// nil (a no-op when passed to wait) if rpm <= 0.
func newModelRateLimiter(rpm int) *modelRateLimiter {
	if rpm <= 0 {
		return nil
	}
	return &modelRateLimiter{minInterval: time.Minute / time.Duration(rpm)}
}

// sharedLimiters holds one limiter per provider+model, so every client of the
// same model — across roles (analyst, enricher, classifier, ORB scan, specialist
// panel, …) — paces against ONE schedule. Provider quotas are per-model-per-key,
// so without this each role's own limiter would ADD UP and blow past the free-
// tier quota (the RESOURCE_EXHAUSTED storm that stalled the ORB scan). The first
// client to register a model fixes its rpm; later clients reuse that limiter.
var (
	sharedLimitersMu sync.Mutex
	sharedLimiters   = map[string]*modelRateLimiter{}
)

// sharedModelRateLimiter returns the process-wide limiter for key (e.g.
// "google:gemini-2.5-flash"), creating it at rpm on first use. rpm <= 0 disables
// pacing (returns nil).
func sharedModelRateLimiter(key string, rpm int) *modelRateLimiter {
	if rpm <= 0 {
		return nil
	}
	sharedLimitersMu.Lock()
	defer sharedLimitersMu.Unlock()
	if l, ok := sharedLimiters[key]; ok {
		return l
	}
	l := newModelRateLimiter(rpm)
	sharedLimiters[key] = l
	return l
}

// reserve claims the next available slot and advances the schedule. It returns
// the wall-clock time this caller is cleared to send. Holding the lock across
// the read-and-advance is what makes concurrent reservations stagger correctly.
func (l *modelRateLimiter) reserve() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	slot := l.nextFree
	if slot.Before(now) {
		slot = now
	}
	l.nextFree = slot.Add(l.minInterval)
	return slot
}

// wait blocks until this caller's reserved slot arrives, or ctx is done. It
// returns ctx.Err() if the context is cancelled first. A nil limiter or a
// non-positive interval is a no-op so callers never need to nil-check.
func (l *modelRateLimiter) wait(ctx context.Context) error {
	if l == nil || l.minInterval <= 0 {
		return nil
	}
	d := time.Until(l.reserve())
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

const (
	defaultRetryDelay = 2 * time.Second
	maxRetryDelay     = 60 * time.Second
	// retryMargin reserves time for the retry request itself, so we never sleep
	// so long that the call would time out the instant it retries.
	retryMargin = 2 * time.Second
)

// retryDelayRe matches both the structured RetryInfo field
// (`"retryDelay": "35s"`) and the human-readable hint (`Please retry in 35.1s`)
// that Google's RESOURCE_EXHAUSTED / UNAVAILABLE bodies carry.
var retryDelayRe = regexp.MustCompile(`(?i)(?:"retryDelay"\s*:\s*"|retry in\s*)([0-9]+(?:\.[0-9]+)?)\s*s`)

// parseRetryDelay extracts the server-requested retry delay from a Gemini error
// body, clamped to maxRetryDelay. It falls back to defaultRetryDelay when the
// body carries no parseable hint.
func parseRetryDelay(body []byte) time.Duration {
	m := retryDelayRe.FindSubmatch(body)
	if m == nil {
		return defaultRetryDelay
	}
	secs, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil || secs <= 0 {
		return defaultRetryDelay
	}
	d := time.Duration(secs * float64(time.Second))
	if d > maxRetryDelay {
		d = maxRetryDelay
	}
	return d
}

// sleepWithinDeadline waits for d, respecting ctx. It returns false WITHOUT
// sleeping when ctx has a deadline that d (plus retryMargin) would overrun —
// signalling the caller to give up rather than burn its remaining budget on a
// sleep that leaves no time to actually retry. A non-positive d returns true
// immediately; a ctx cancellation during the sleep returns false.
func sleepWithinDeadline(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < d+retryMargin {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
