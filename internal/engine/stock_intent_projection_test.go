package engine

import (
	"errors"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/execution"
)

type durableProjectionExecutor struct{ execution.Executor }

func (durableProjectionExecutor) DurableStockOrderIntents() bool { return true }

func TestAcceptedDurableEntryProjectionFailsClosedBeforeLegacyTrade(t *testing.T) {
	durable := durableProjectionExecutor{Executor: execution.NewPaperExecutor()}
	if err := durableStockIntentProjectionError(durable, "", errors.New("intent lookup unavailable")); err == nil {
		t.Fatal("lookup failure must stop an unlinked legacy trade projection")
	}
	if err := durableStockIntentProjectionError(durable, "", nil); err == nil {
		t.Fatal("missing durable intent must stop an unlinked legacy trade projection")
	}
	if err := durableStockIntentProjectionError(execution.NewPaperExecutor(), "", nil); err != nil {
		t.Fatalf("paper execution should remain intent-free: %v", err)
	}
}
