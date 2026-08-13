package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type durableProjectionExecutor struct{ execution.Executor }

func (durableProjectionExecutor) DurableStockOrderIntents() bool { return true }

func TestAcceptedDurableEntryProjectionFailsClosedBeforeLegacyTrade(t *testing.T) {
	durable := durableProjectionExecutor{Executor: execution.NewPaperExecutor()}
	lookupErr := func(context.Context, string) (*store.StockOrderIntent, error) {
		return nil, errors.New("intent lookup unavailable")
	}
	missing := func(context.Context, string) (*store.StockOrderIntent, error) { return nil, nil }
	if _, err := stockIntentIDForAcceptedOrderWithLookup(context.Background(), durable, "entry:x", lookupErr); err == nil {
		t.Fatal("lookup failure must stop an unlinked legacy trade projection")
	}
	if _, err := stockIntentIDForAcceptedOrderWithLookup(context.Background(), durable, "entry:x", missing); err == nil {
		t.Fatal("missing durable intent must stop an unlinked legacy trade projection")
	}
	if _, err := stockIntentIDForAcceptedOrderWithLookup(context.Background(), execution.NewPaperExecutor(), "entry:x", missing); err != nil {
		t.Fatalf("paper execution should remain intent-free: %v", err)
	}
}
