package analysis

import (
	"context"
	"testing"
	"time"
)

func TestModelRateLimiter_ReserveStaggers(t *testing.T) {
	l := &modelRateLimiter{minInterval: 100 * time.Millisecond}
	s1 := l.reserve()
	s2 := l.reserve()
	s3 := l.reserve()
	// After the first (now-anchored) slot, each reservation advances the
	// schedule by exactly one interval — concurrent callers can't collide.
	if d := s2.Sub(s1); d != 100*time.Millisecond {
		t.Errorf("slot2-slot1 = %v, want 100ms", d)
	}
	if d := s3.Sub(s2); d != 100*time.Millisecond {
		t.Errorf("slot3-slot2 = %v, want 100ms", d)
	}
}

func TestModelRateLimiter_NilAndZeroAreNoops(t *testing.T) {
	if got := newModelRateLimiter(0); got != nil {
		t.Errorf("newModelRateLimiter(0) = %v, want nil", got)
	}
	if got := newModelRateLimiter(-5); got != nil {
		t.Errorf("newModelRateLimiter(-5) = %v, want nil", got)
	}
	var nilLimiter *modelRateLimiter
	if err := nilLimiter.wait(context.Background()); err != nil {
		t.Errorf("nil limiter wait = %v, want nil", err)
	}
}

func TestModelRateLimiter_WaitFirstCallImmediate(t *testing.T) {
	l := newModelRateLimiter(60) // 1s interval
	start := time.Now()
	if err := l.wait(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("first wait took %v, want ~immediate", d)
	}
}

func TestModelRateLimiter_WaitRespectsContext(t *testing.T) {
	l := &modelRateLimiter{minInterval: 10 * time.Second}
	l.reserve() // push nextFree ~10s out so the next wait must block

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	start := time.Now()
	err := l.wait(ctx)
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("wait blocked %v after cancel, want prompt return", d)
	}
}

func TestParseRetryDelay(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{"structured field", `{"error":{"details":[{"retryDelay": "35s"}]}}`, 35 * time.Second},
		{"human hint", "Please retry in 1.5s.", 1500 * time.Millisecond},
		{"no hint falls back", `{"error":{"message":"quota exceeded"}}`, defaultRetryDelay},
		{"clamped to max", `{"retryDelay": "600s"}`, maxRetryDelay},
		{"non-numeric falls back", `{"retryDelay": "soons"}`, defaultRetryDelay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryDelay([]byte(tc.body)); got != tc.want {
				t.Errorf("parseRetryDelay(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestSleepWithinDeadline(t *testing.T) {
	t.Run("non-positive returns immediately", func(t *testing.T) {
		if !sleepWithinDeadline(context.Background(), 0) {
			t.Error("d=0 should return true")
		}
	})

	t.Run("fits deadline", func(t *testing.T) {
		// Deadline must exceed delay + retryMargin (2s) for the retry to be
		// worth attempting; a 10ms sleep under a 5s deadline qualifies.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if !sleepWithinDeadline(ctx, 10*time.Millisecond) {
			t.Error("10ms within 5s deadline should sleep and return true")
		}
	})

	t.Run("does not fit deadline returns false fast", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		if sleepWithinDeadline(ctx, time.Second) {
			t.Error("1s delay should not fit 50ms deadline")
		}
		if d := time.Since(start); d > 20*time.Millisecond {
			t.Errorf("returned after %v, want immediate (no sleep)", d)
		}
	})

	t.Run("cancelled context returns false", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if sleepWithinDeadline(ctx, 10*time.Millisecond) {
			t.Error("cancelled ctx should return false")
		}
	})
}
