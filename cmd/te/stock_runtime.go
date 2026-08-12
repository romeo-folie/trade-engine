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

func isStandardStockMode(mode string) bool {
	return mode == "manual" || mode == "auto" || mode == "paper"
}

func protectDryRunStockExecutor(broker execution.Executor, mode string, dryRun bool) execution.Executor {
	if dryRun && isStandardStockMode(mode) && broker != nil {
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
