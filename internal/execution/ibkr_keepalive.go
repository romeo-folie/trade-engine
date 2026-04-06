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
