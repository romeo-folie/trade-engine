package main

import (
	"context"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
)

type stockOrderRecoverer interface{ Recover(context.Context) error }

// stockOrderRecoveryForStartup keeps DB-optional runtime modes from invoking
// the durable coordinator before its store is connected.
func stockOrderRecoveryForStartup(dbConnected bool, broker execution.Executor) stockOrderRecoverer {
	if !dbConnected || broker == nil {
		return nil
	}
	recoverer, _ := broker.(stockOrderRecoverer)
	return recoverer
}

// recoverStockOrdersPeriodically is deliberately serial: the next tick cannot
// start until the prior recovery finishes, avoiding overlapping replay scans.
func recoverStockOrdersPeriodically(ctx context.Context, recoverer stockOrderRecoverer, interval time.Duration) {
	if recoverer == nil || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recoveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_ = recoverer.Recover(recoveryCtx)
			cancel()
		}
	}
}

type runtimeOverrideLookup func(context.Context, string) (string, bool, error)

const preflightDBTimeout = 5 * time.Second

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

// resolvePreflightMode keeps the optional database override lookup inside the
// preflight database deadline. A connected-but-stalled database must not delay
// the independent Alpaca account request indefinitely.
func resolvePreflightMode(ctx context.Context, yamlMode string, lookup runtimeOverrideLookup) (string, error) {
	return resolveRunMode(ctx, yamlMode, "", lookup)
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

func isStandardStockMode(mode string) bool {
	return mode == "manual" || mode == "auto" || mode == "paper"
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
