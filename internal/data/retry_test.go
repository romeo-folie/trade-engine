package data

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"5xx retryable", &httpError{status: 500, msg: "internal server error"}, true},
		{"502 retryable", &httpError{status: 502, msg: "bad gateway"}, true},
		{"503 retryable", &httpError{status: 503, msg: "service unavailable"}, true},
		{"504 retryable", &httpError{status: 504, msg: "gateway timeout"}, true},
		{"400 not retryable", &httpError{status: 400, msg: "bad request"}, false},
		{"401 not retryable", &httpError{status: 401, msg: "unauthorized"}, false},
		{"403 not retryable", &httpError{status: 403, msg: "forbidden"}, false},
		{"404 not retryable", &httpError{status: 404, msg: "not found"}, false},
		{"429 not retryable", &httpError{status: 429, msg: "rate limited"}, false},
		{"generic error not retryable", errors.New("some error"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isRetryable(tc.err)
			if got != tc.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestJitteredDelay(t *testing.T) {
	// Delays should be at least the base for each attempt.
	for attempt := 0; attempt < 3; attempt++ {
		d := jitteredDelay(attempt)
		base := retryBaseDelay * (1 << uint(attempt))
		if base > retryMaxDelay {
			base = retryMaxDelay
		}
		if d < base {
			t.Errorf("attempt %d: delay %v < base %v", attempt, d, base)
		}
		max := base + base/2
		if max > retryMaxDelay+retryMaxDelay/2 {
			max = retryMaxDelay + retryMaxDelay/2
		}
		if d > max {
			t.Errorf("attempt %d: delay %v > max jittered %v", attempt, d, max)
		}
	}
}

func TestWithRetry_SuccessFirstAttempt(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	resp, err := withRetry(context.Background(), func() (*http.Response, error) {
		return http.Get(srv.URL) //nolint:noctx
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestWithRetry_RetriesOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 — retryable
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	resp, err := withRetry(context.Background(), func() (*http.Response, error) {
		r, err := http.Get(srv.URL) //nolint:noctx
		if err != nil {
			return nil, err
		}
		if r.StatusCode >= 500 {
			r.Body.Close()
			return nil, &httpError{status: r.StatusCode, msg: "server error"}
		}
		return r, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestWithRetry_NoRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest) // 400 — not retryable
	}))
	defer srv.Close()

	_, err := withRetry(context.Background(), func() (*http.Response, error) {
		r, httpErr := http.Get(srv.URL) //nolint:noctx
		if httpErr != nil {
			return nil, httpErr
		}
		if r.StatusCode >= 400 {
			r.Body.Close()
			return nil, &httpError{status: r.StatusCode, msg: "client error"}
		}
		return r, nil
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected 1 call (no retry on 4xx), got %d", calls)
	}
}

func TestWithRetry_ExhaustsAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError) // always 500
	}))
	defer srv.Close()

	_, err := withRetry(context.Background(), func() (*http.Response, error) {
		r, httpErr := http.Get(srv.URL) //nolint:noctx
		if httpErr != nil {
			return nil, httpErr
		}
		if r.StatusCode >= 500 {
			r.Body.Close()
			return nil, &httpError{status: r.StatusCode, msg: "server error"}
		}
		return r, nil
	})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if atomic.LoadInt32(&calls) != int32(retryMaxAttempts) {
		t.Errorf("expected %d calls, got %d", retryMaxAttempts, calls)
	}
}

func TestWithRetry_RespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := withRetry(ctx, func() (*http.Response, error) {
		r, httpErr := http.Get(srv.URL) //nolint:noctx
		if httpErr != nil {
			return nil, httpErr
		}
		if r.StatusCode >= 500 {
			r.Body.Close()
			return nil, &httpError{status: r.StatusCode, msg: "server error"}
		}
		return r, nil
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("retry didn't respect context: took %v", elapsed)
	}
}
