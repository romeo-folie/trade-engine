package engine

import "testing"

// executeSignal stamps the pipeline node an execution was released from onto the
// emitted TradeExecuted/TradeSimulated payload. The web layer routes on this
// rather than on the configured engine mode, because auto/paper run both paths
// concurrently — most signals execute straight off risk approval, while
// first-tick-deferred BUYs are parked and later released by
// processDeferredSignals — and the mode alone cannot tell them apart.
//
// Regression: without a per-execution marker, the approvals → executor edge was
// declared in the topology but never traversed, so it rendered permanently idle.
func TestExecutionSourceNode(t *testing.T) {
	// Released from the pending queue: manual approval (ApproveSignal) or a
	// deferred entry picked up by processDeferredSignals.
	if got := executionSourceNode(true); got != "approvals" {
		t.Errorf("executionSourceNode(true) = %q, want %q", got, "approvals")
	}
	// Straight off risk approval — empty leaves the web layer's default
	// risk_manager → executor attribution in place.
	if got := executionSourceNode(false); got != "" {
		t.Errorf("executionSourceNode(false) = %q, want empty", got)
	}
}
