package execution

import (
	"context"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/logger"
)

// CheckAuth verifies that the IBKR Client Portal gateway has an authenticated session.
// It calls GET /iserver/auth/status and returns an error when either the gateway is
// unreachable or the session reports unauthenticated / disconnected.
func (b *IBKRBroker) CheckAuth(ctx context.Context) error {
	var status struct {
		Authenticated bool `json:"authenticated"`
		Connected     bool `json:"connected"`
	}
	if err := b.do(ctx, "GET", "/iserver/auth/status", nil, &status); err != nil {
		return fmt.Errorf("ibkr CheckAuth: %w", err)
	}
	if !status.Authenticated || !status.Connected {
		return fmt.Errorf("ibkr CheckAuth: gateway not authenticated (authenticated=%v, connected=%v)",
			status.Authenticated, status.Connected)
	}
	return nil
}

// StartAuthMonitor polls CheckAuth every monitorSec seconds and invokes
// onExpired when the gateway session is lost, and onRecovered when it comes
// back after a previous failure.
//
// Repeated onExpired calls are debounced: the callback is suppressed until
// debounceMin minutes have elapsed since the last invocation, so the user
// does not receive a flood of alerts while re-authenticating. onRecovered is
// only called when an expiry alert was actually sent, keeping the two
// notifications coherent.
//
// Callbacks are dispatched in their own goroutines so that blocking network
// I/O (e.g. a Telegram send) does not stall the poll loop or delay shutdown.
//
// Either callback may be nil. The returned stop function cancels the goroutine.
func (b *IBKRBroker) StartAuthMonitor(
	ctx context.Context,
	monitorSec int,
	debounceMin int,
	onExpired func(gatewayURL string),
	onRecovered func(gatewayURL string),
) func() {
	if monitorSec <= 0 {
		monitorSec = 60
	}
	if debounceMin <= 0 {
		debounceMin = 30
	}

	ctx, cancel := context.WithCancel(ctx)

	go func() {
		ticker := time.NewTicker(time.Duration(monitorSec) * time.Second)
		defer ticker.Stop()

		var lastAlerted time.Time
		wasDown := false
		alertSent := false // tracks whether an expiry alert was actually dispatched

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := b.CheckAuth(ctx)
				if err != nil {
					if !wasDown {
						logger.Warn("ibkr auth monitor: session not authenticated", "err", err)
					}
					if onExpired != nil && time.Since(lastAlerted) >= time.Duration(debounceMin)*time.Minute {
						url := b.gatewayURL
						go onExpired(url)
						lastAlerted = time.Now()
						alertSent = true
					}
					wasDown = true
				} else {
					if wasDown && alertSent && onRecovered != nil {
						url := b.gatewayURL
						go onRecovered(url)
					}
					wasDown = false
					alertSent = false
				}
			}
		}
	}()

	return cancel
}

// StartKeepalive pings POST /iserver/auth/tickle every tickleSec seconds to prevent
// the Client Portal gateway from timing out the session. The returned stop function
// cancels the background goroutine; call it on shutdown.
//
// Failures are logged as warnings and do not stop the keepalive loop — transient
// network blips should not terminate the session.
func (b *IBKRBroker) StartKeepalive(ctx context.Context, tickleSec int) func() {
	if tickleSec <= 0 {
		tickleSec = 240 // Default to a 4-minute keepalive interval when unspecified.
	}

	ctx, cancel := context.WithCancel(ctx)

	go func() {
		ticker := time.NewTicker(time.Duration(tickleSec) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := b.do(ctx, "POST", "/iserver/auth/tickle", nil, nil); err != nil {
					logger.Warn("ibkr keepalive tickle failed", "err", err)
				}
			}
		}
	}()

	return cancel
}
