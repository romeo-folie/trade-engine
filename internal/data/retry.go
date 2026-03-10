package data

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"net/http"
	"time"
)

const (
	retryMaxAttempts = 3
	retryBaseDelay   = 500 * time.Millisecond
	retryMaxDelay    = 10 * time.Second
)

// httpError wraps an HTTP error response so the retry helper can inspect the
// status code without re-parsing error strings.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

// isRetryable returns true if the error represents a transient failure that
// warrants another attempt. Retries only happen on:
//   - 500, 502, 503, 504 server errors
//   - Network-level timeouts (net.Error.Timeout())
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *httpError
	if errors.As(err, &httpErr) {
		return httpErr.status == 500 ||
			httpErr.status == 502 ||
			httpErr.status == 503 ||
			httpErr.status == 504
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

// jitteredDelay returns an exponentially increasing delay with up to 50%
// random jitter for attempt n (0-indexed). Caps at retryMaxDelay.
func jitteredDelay(attempt int) time.Duration {
	base := retryBaseDelay * (1 << uint(attempt)) // 500ms, 1s, 2s, ...
	if base > retryMaxDelay {
		base = retryMaxDelay
	}
	jitter := time.Duration(rand.Int63n(int64(base / 2)))
	return base + jitter
}

// withRetry executes fn up to retryMaxAttempts times, backing off between
// attempts on transient failures. Non-retryable errors are returned immediately.
// Context cancellation is respected between attempts.
func withRetry(ctx context.Context, fn func() (*http.Response, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < retryMaxAttempts; attempt++ {
		if attempt > 0 {
			delay := jitteredDelay(attempt - 1)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		resp, err := fn()
		if err == nil {
			return resp, nil
		}
		if !isRetryable(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}
