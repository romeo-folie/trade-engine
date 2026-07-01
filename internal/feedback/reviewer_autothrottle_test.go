package feedback

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestRunReview_WritesThrottleForNonFirstWindow proves the reviewer processes EVERY
// configured feedback window — not just the first — by asserting that the auto-throttle
// risk_multiplier lands for the "30d" window even though "1d" is processed first. If
// runReview returned inside the window loop after the first window, the 30d insight (and
// thus the throttle) would never be written. Guards against that regression.
func TestRunReview_WritesThrottleForNonFirstWindow(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping reviewer integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	const ticker = "EURATW/USD"
	const sessionName = "autothrottle-test"

	// Idempotent across runs: the feedback TestMain truncates only the feedback tables, so
	// clean up this test's own ORB rows (own ticker / session name) before re-seeding.
	if _, err := db.Exec(ctx, `DELETE FROM orb_trades WHERE ticker=$1`, ticker); err != nil {
		t.Fatalf("cleanup trades: %v", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM orb_sessions WHERE session_name=$1`, sessionName); err != nil {
		t.Fatalf("cleanup sessions: %v", err)
	}

	sess, err := store.CreateORBSession(ctx, db, time.Now(), sessionName)
	if err != nil {
		t.Fatalf("CreateORBSession: %v", err)
	}
	// Three closed trades within every window (closed_at = NOW()), so trade_count clears the
	// throttle's MinTrades gate (set low below).
	for range 3 {
		id, err := store.CreateORBTrade(ctx, db, store.ORBTrade{
			SessionID: sess.ID, Ticker: ticker, Side: "buy",
			EntryStop: 1.10, StopLoss: 1.09, TakeProfit: 1.12, Qty: 10000,
			Status: "filled",
		})
		if err != nil {
			t.Fatalf("CreateORBTrade: %v", err)
		}
		if err := store.CloseORBTrade(ctx, db, id, "tp", "broker", 1.12, 20.0); err != nil {
			t.Fatalf("CloseORBTrade: %v", err)
		}
	}

	cfg := config.FeedbackConfig{
		Windows: []int{1, 7, 30}, // 1d processed first, 30d last
		ORBAutoThrottle: config.ORBAutoThrottleConfig{
			Enabled: true, Window: "30d", MinTrades: 2, Sensitivity: 1.0, Floor: 0.5, Cap: 1.5,
		},
	}
	r := NewReviewer(cfg, events.New(), db)
	if err := r.runReview(ctx); err != nil {
		t.Fatalf("runReview: %v", err)
	}

	// The 30d throttle multiplier must exist — only possible if the loop reached the 30d
	// window. notBefore well in the past so freshness doesn't hide it.
	notBefore := time.Now().AddDate(0, 0, -31)
	if _, ok, err := store.GetFreshORBRiskMultiplier(ctx, ticker, "30d", notBefore); err != nil || !ok {
		t.Errorf("no 30d risk_multiplier written (ok=%v err=%v) — the reviewer did not reach the 30d window", ok, err)
	}
}
