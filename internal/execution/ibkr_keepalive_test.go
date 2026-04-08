package execution

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// newAuthStatusServer returns a test server whose response to
// GET /iserver/auth/status is controlled by the supplied atomic bool.
// authenticated=true and connected=true when the flag is 1; both false when 0.
func newAuthStatusServer(t *testing.T, authenticated *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/iserver/auth/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if authenticated.Load() {
			w.Write([]byte(`{"authenticated":true,"connected":true}`))
		} else {
			w.Write([]byte(`{"authenticated":false,"connected":false}`))
		}
	}))
}

// ─── StartAuthMonitor ─────────────────────────────────────────────────────────

func TestStartAuthMonitor_CallsOnExpiredWhenSessionLost(t *testing.T) {
	var authenticated atomic.Bool
	authenticated.Store(false) // session starts down

	srv := newAuthStatusServer(t, &authenticated)
	defer srv.Close()

	b := NewIBKRBroker(config.IBKRConfig{GatewayURL: srv.URL, InsecureTLS: false})

	expiredCh := make(chan string, 1)
	stop := b.StartAuthMonitor(t.Context(), 1, 0, func(url string) {
		expiredCh <- url
	}, nil)
	defer stop()

	select {
	case gotURL := <-expiredCh:
		if gotURL != srv.URL {
			t.Errorf("onExpired got URL %q; want %q", gotURL, srv.URL)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for onExpired callback")
	}
}

func TestStartAuthMonitor_CallsOnRecoveredAfterRestore(t *testing.T) {
	var authenticated atomic.Bool
	authenticated.Store(false) // start down

	srv := newAuthStatusServer(t, &authenticated)
	defer srv.Close()

	b := NewIBKRBroker(config.IBKRConfig{GatewayURL: srv.URL, InsecureTLS: false})

	expiredCh := make(chan struct{}, 1)
	recoveredCh := make(chan string, 1)

	stop := b.StartAuthMonitor(t.Context(), 1, 0,
		func(_ string) { expiredCh <- struct{}{} },
		func(url string) { recoveredCh <- url },
	)
	defer stop()

	// Wait for first expiry detection.
	select {
	case <-expiredCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial onExpired")
	}

	// Bring session back up.
	authenticated.Store(true)

	select {
	case gotURL := <-recoveredCh:
		if gotURL != srv.URL {
			t.Errorf("onRecovered got URL %q; want %q", gotURL, srv.URL)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for onRecovered callback")
	}
}

func TestStartAuthMonitor_NoCallbackWhenHealthy(t *testing.T) {
	var authenticated atomic.Bool
	authenticated.Store(true) // session stays up

	srv := newAuthStatusServer(t, &authenticated)
	defer srv.Close()

	b := NewIBKRBroker(config.IBKRConfig{GatewayURL: srv.URL, InsecureTLS: false})

	expiredCh := make(chan struct{}, 1)
	stop := b.StartAuthMonitor(t.Context(), 1, 0,
		func(_ string) { expiredCh <- struct{}{} },
		nil,
	)
	defer stop()

	// Give the monitor two tick opportunities; neither should fire.
	select {
	case <-expiredCh:
		t.Error("onExpired called unexpectedly for healthy session")
	case <-time.After(2500 * time.Millisecond):
		// Pass: no expiry fired.
	}
}

func TestStartAuthMonitor_DebouncesSuppressesRepeatedAlerts(t *testing.T) {
	var authenticated atomic.Bool
	authenticated.Store(false) // session stays down

	srv := newAuthStatusServer(t, &authenticated)
	defer srv.Close()

	b := NewIBKRBroker(config.IBKRConfig{GatewayURL: srv.URL, InsecureTLS: false})

	var callCount atomic.Int32
	// 60-minute debounce is effectively infinite for this test; only the first
	// tick should invoke the callback.
	stop := b.StartAuthMonitor(t.Context(), 1, 60,
		func(_ string) { callCount.Add(1) },
		nil,
	)

	// Wait long enough for at least two ticks to fire.
	time.Sleep(3 * time.Second)
	stop()

	if n := callCount.Load(); n != 1 {
		t.Errorf("onExpired called %d times; want exactly 1 (debounce should suppress repeats)", n)
	}
}

func TestStartAuthMonitor_NilCallbacksAreNoOp(t *testing.T) {
	var authenticated atomic.Bool
	authenticated.Store(false)

	srv := newAuthStatusServer(t, &authenticated)
	defer srv.Close()

	b := NewIBKRBroker(config.IBKRConfig{GatewayURL: srv.URL, InsecureTLS: false})

	// Should not panic with nil callbacks.
	stop := b.StartAuthMonitor(t.Context(), 1, 0, nil, nil)
	time.Sleep(1500 * time.Millisecond)
	stop()
}

func TestStartAuthMonitor_StopCancelsGoroutine(t *testing.T) {
	var authenticated atomic.Bool
	authenticated.Store(false)

	srv := newAuthStatusServer(t, &authenticated)
	defer srv.Close()

	b := NewIBKRBroker(config.IBKRConfig{GatewayURL: srv.URL, InsecureTLS: false})

	var callCount atomic.Int32
	stop := b.StartAuthMonitor(t.Context(), 1, 0,
		func(_ string) { callCount.Add(1) },
		nil,
	)

	// Wait for at least one tick, then stop.
	time.Sleep(1500 * time.Millisecond)
	stop()
	countAfterStop := callCount.Load()

	// Wait another full interval and confirm no more calls occurred.
	time.Sleep(1500 * time.Millisecond)
	if n := callCount.Load(); n != countAfterStop {
		t.Errorf("callCount increased after stop: %d → %d", countAfterStop, n)
	}
}

func TestStartAuthMonitor_DefaultsAppliedWhenZero(t *testing.T) {
	// Passing 0 for monitorSec and debounceMin should not panic and should
	// apply the defaults (60s and 30m). We only verify no panic here since
	// exercising the full default intervals would be impractical in a unit test.
	var authenticated atomic.Bool
	authenticated.Store(true)

	srv := newAuthStatusServer(t, &authenticated)
	defer srv.Close()

	b := NewIBKRBroker(config.IBKRConfig{GatewayURL: srv.URL, InsecureTLS: false})
	stop := b.StartAuthMonitor(t.Context(), 0, 0, nil, nil)
	stop()
}
