package stockorders

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type unavailableStoreBroker struct{ mutations int }

func (b *unavailableStoreBroker) PlaceOrder(context.Context, execution.OrderRequest) (execution.OrderResult, error) {
	b.mutations++
	return execution.OrderResult{}, nil
}
func (b *unavailableStoreBroker) CancelOrder(context.Context, string) error {
	b.mutations++
	return nil
}
func (b *unavailableStoreBroker) GetOrderStatus(context.Context, string) (execution.OrderResult, error) {
	return execution.OrderResult{}, nil
}
func (*unavailableStoreBroker) GetPositions(context.Context) ([]execution.Position, error) {
	return nil, nil
}
func (*unavailableStoreBroker) GetAccount(context.Context) (execution.AccountInfo, error) {
	return execution.AccountInfo{}, nil
}
func (b *unavailableStoreBroker) ReplaceStopOrder(context.Context, string, float64) (execution.OrderResult, error) {
	b.mutations++
	return execution.OrderResult{}, nil
}

func TestClientIDForKeyIsDeterministicAndBounded(t *testing.T) {
	first := clientIDForKey("entry:signal-123")
	if got := clientIDForKey("entry:signal-123"); got != first {
		t.Fatalf("same intent key produced %q then %q", first, got)
	}
	if got := clientIDForKey("entry:signal-456"); got == first {
		t.Fatal("different intent keys share client id")
	}
	if !strings.HasPrefix(first, clientIDPrefix) || len(first) > 128 {
		t.Fatalf("client id %q is not a valid bounded Alpaca key", first)
	}
}

func TestCoordinatorFailsClosedWithoutDurableStore(t *testing.T) {
	if store.IsConnected() {
		t.Skip("requires disconnected store")
	}
	broker := &unavailableStoreBroker{}
	coordinator := New(broker)
	ctx := context.Background()
	if _, err := coordinator.PlaceOrder(ctx, execution.OrderRequest{IntentKey: "entry:no-db"}); !errors.Is(err, ErrDurableStoreUnavailable) {
		t.Fatalf("PlaceOrder error=%v, want durable-store error", err)
	}
	if err := coordinator.CancelOrder(ctx, "stock-order"); !errors.Is(err, ErrDurableStoreUnavailable) {
		t.Fatalf("CancelOrder error=%v, want durable-store error", err)
	}
	if _, err := coordinator.ReplaceStopOrder(ctx, "stock-order", 99); !errors.Is(err, ErrDurableStoreUnavailable) {
		t.Fatalf("ReplaceStopOrder error=%v, want durable-store error", err)
	}
	if err := coordinator.Recover(ctx); !errors.Is(err, ErrDurableStoreUnavailable) {
		t.Fatalf("Recover error=%v, want durable-store error", err)
	}
	if broker.mutations != 0 {
		t.Fatalf("durable-store failure mutated broker %d times", broker.mutations)
	}
}

func TestAnalystExitSignalIDAcceptsOnlyValidatedRetryLineage(t *testing.T) {
	const (
		signalID = "signal-123"
		ticker   = "SPY"
	)
	for _, tc := range []struct {
		name string
		key  string
		ok   bool
	}{
		{"base", "analyst-exit:signal-123:SPY", true},
		{"broker retry", "analyst-exit:signal-123:SPY:retry:broker-123", true},
		{"brokerless retry", "analyst-exit:signal-123:SPY:retry:intent-123", true},
		{"multiple retries", "analyst-exit:signal-123:SPY:retry:intent-123:retry:broker-456", true},
		{"missing signal", "analyst-exit::SPY", false},
		{"mismatched ticker", "analyst-exit:signal-123:QQQ", false},
		{"missing retry identity", "analyst-exit:signal-123:SPY:retry", false},
		{"empty retry identity", "analyst-exit:signal-123:SPY:retry:", false},
		{"unknown suffix", "analyst-exit:signal-123:SPY:replacement:broker-123", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := analystExitSignalID(tc.key, ticker)
			if ok != tc.ok || (ok && got != signalID) {
				t.Fatalf("analystExitSignalID(%q) = %q, %v; want %q, %v", tc.key, got, ok, signalID, tc.ok)
			}
		})
	}
}

func TestIsRecoveredMarketExitKey_BrokerProtectionFlatten(t *testing.T) {
	if !isRecoveredMarketExitKey("broker-protection-flatten:trade-123") {
		t.Fatal("broker-protection flatten must be recognized as a recovered market exit")
	}
}
