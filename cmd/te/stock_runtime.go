package main

import (
	"context"
	"fmt"

	"github.com/romeo-folie/trade-engine/internal/execution"
)

type runtimeOverrideLookup func(context.Context, string) (string, bool, error)

// resolveRunMode applies the documented YAML → DB → CLI precedence for the
// engine mode. Stock environment selection stays in markets.stocks.paper.
func resolveRunMode(ctx context.Context, yamlMode, cliMode string, lookup runtimeOverrideLookup) (string, error) {
	mode := yamlMode
	if lookup != nil {
		if dbMode, ok, err := lookup(ctx, "engine.mode"); err != nil {
			return "", fmt.Errorf("read engine.mode runtime override: %w", err)
		} else if ok {
			mode = dbMode
		}
	}
	if cliMode != "" {
		mode = cliMode
	}
	return mode, nil
}

// protectDryRunStockExecutor applies only to the shared Alpaca stock broker
// variable. It therefore covers normal, ORB, and combined-New-York stock paths
// without changing separately constructed IBKR brokers.
func protectDryRunStockExecutor(broker execution.Executor, dryRun bool) execution.Executor {
	if dryRun && broker != nil {
		return execution.NewReadOnlyExecutor(broker)
	}
	return broker
}

func brokerName(broker execution.Executor) string {
	if named, ok := broker.(interface{ BrokerName() string }); ok {
		return named.BrokerName()
	}
	switch broker.(type) {
	case *execution.IBKRBroker:
		return "ibkr"
	}
	return ""
}

// alpacaBrokerView returns the read-only BrokerView used by Alpaca history
// backfill. It deliberately identifies the adapter through its stable name so
// the dry-run wrapper does not disable this read-only startup work.
func alpacaBrokerView(broker execution.Executor) (execution.BrokerView, bool) {
	if brokerName(broker) != "alpaca" {
		return nil, false
	}
	bv, ok := broker.(execution.BrokerView)
	return bv, ok && bv != nil
}
