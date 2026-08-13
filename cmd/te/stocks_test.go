package main

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
)

func TestRuntimeOverridePrecedence(t *testing.T) {
	lookup := func(_ context.Context, key string) (string, bool, error) {
		switch key {
		case "engine.mode":
			return "auto", true, nil
		default:
			return "", false, nil
		}
	}
	cfg := config.Defaults()
	cfg.Engine.Mode = "manual"
	mode, err := resolveRunMode(context.Background(), cfg.Engine.Mode, "paper", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "paper" {
		t.Fatalf("mode = %q, want CLI override paper", mode)
	}
}

func TestValidRiskBaselineEquity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		equity float64
		want   bool
	}{
		{name: "positive", equity: 1_000, want: true},
		{name: "zero", equity: 0},
		{name: "negative", equity: -1},
		{name: "nan", equity: math.NaN()},
		{name: "positive infinity", equity: math.Inf(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validRiskBaselineEquity(tc.equity); got != tc.want {
				t.Fatalf("validRiskBaselineEquity(%v) = %t, want %t", tc.equity, got, tc.want)
			}
		})
	}
}

func TestRiskBaselineEquityMatchesSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested float64
		snapshot  float64
		want      bool
	}{
		{name: "exact", requested: 1_000, snapshot: 1_000, want: true},
		{name: "cent rounding", requested: 1_000.001, snapshot: 1_000, want: true},
		{name: "operator typo", requested: 1_100, snapshot: 1_000},
		{name: "invalid broker equity", requested: 1_000, snapshot: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := riskBaselineEquityMatchesSnapshot(tc.requested, tc.snapshot); got != tc.want {
				t.Fatalf("riskBaselineEquityMatchesSnapshot(%v, %v) = %t, want %t", tc.requested, tc.snapshot, got, tc.want)
			}
		})
	}
}

func TestResolvePreflightModeBoundsDatabaseOverrideLookup(t *testing.T) {
	dbCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	mode, err := resolvePreflightMode(dbCtx, "manual", func(ctx context.Context, key string) (string, bool, error) {
		if key != "engine.mode" {
			t.Fatalf("override key = %q, want engine.mode", key)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second {
			t.Fatalf("lookup context deadline = %v (ok=%t), want active bounded deadline", deadline, ok)
		}
		return "auto", true, nil
	})
	if err != nil || mode != "auto" {
		t.Fatalf("resolvePreflightMode = %q, %v; want auto, nil", mode, err)
	}
}

func TestResolvePreflightModePreservesConnectedDatabaseLookupErrors(t *testing.T) {
	want := context.DeadlineExceeded
	_, err := resolvePreflightMode(context.Background(), "manual", func(context.Context, string) (string, bool, error) {
		return "", false, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("resolvePreflightMode error = %v, want wrapping %v", err, want)
	}
}

func TestProtectDryRunStockExecutorWrapsEveryAlpacaStockMode(t *testing.T) {
	broker := execution.NewPaperExecutor()
	for _, mode := range []string{"manual", "auto", "paper", "orb", "orb-newyork"} {
		if _, ok := protectDryRunStockExecutor(broker, true).(*execution.ReadOnlyExecutor); !ok {
			t.Fatalf("%s dry-run did not receive read-only executor", mode)
		}
	}
	if got := protectDryRunStockExecutor(broker, false); got != broker {
		t.Fatal("non-dry-run executor was changed")
	}
}

type namedExecutor struct{ execution.Executor }

func (*namedExecutor) BrokerName() string { return "alpaca" }

func TestBrokerNamePreservesDryRunAlpacaIdentity(t *testing.T) {
	broker := &namedExecutor{Executor: execution.NewPaperExecutor()}
	dryRun := protectDryRunStockExecutor(broker, true)
	if got := brokerName(dryRun); got != "alpaca" {
		t.Fatalf("brokerName(dry-run Alpaca) = %q, want alpaca", got)
	}
}

func TestAlpacaBrokerViewSurvivesDryRunWrapping(t *testing.T) {
	broker := execution.NewAlpacaBroker(config.StocksConfig{})
	dryRun := protectDryRunStockExecutor(broker, true)
	bv, ok := alpacaBrokerView(dryRun)
	if !ok || bv == nil {
		t.Fatal("dry-run Alpaca broker no longer supports history backfill")
	}
	if _, ok := any(bv).(*execution.ReadOnlyExecutor); !ok {
		t.Fatalf("backfill view = %T, want read-only wrapped Alpaca", bv)
	}
	if _, ok := alpacaBrokerView(execution.NewPaperExecutor()); ok {
		t.Fatal("non-Alpaca executor incorrectly selected for Alpaca backfill")
	}
}

func TestDryRunAlpacaRetainsOptionDataForOptionsORB(t *testing.T) {
	dryRun := protectDryRunStockExecutor(execution.NewAlpacaBroker(config.StocksConfig{}), true)
	if _, ok := dryRun.(interface {
		GetOptionChain(context.Context, string, execution.OptionChainFilter) ([]execution.OptionSnapshot, error)
		GetOptionSnapshots(context.Context, []string) ([]execution.OptionSnapshot, error)
	}); !ok {
		t.Fatal("dry-run Alpaca no longer exposes read-only option data needed by options-ORB")
	}
}

type preflightSpy struct {
	account execution.BrokerAccount
	err     error
	calls   int
}

func (s *preflightSpy) AccountSnapshot(context.Context) (execution.BrokerAccount, error) {
	s.calls++
	return s.account, s.err
}

func activeAccount() execution.BrokerAccount {
	return execution.BrokerAccount{
		AccountID: "account-id", AccountNumber: "PA123", Status: "ACTIVE", Currency: "USD", Cash: 0, Equity: 100,
		BuyingPower: 100, LastEquity: 100, Multiplier: 1, CashAvailable: true, BuyingPowerAvailable: true,
		LastEquityAvailable: true, MultiplierAvailable: true, FetchedAt: time.Now().UTC(),
	}
}

func TestPreflightReadsOnlyAccountSnapshot(t *testing.T) {
	spy := &preflightSpy{account: activeAccount()}
	result, err := preflightStockAccount(context.Background(), config.StocksConfig{Paper: true, ExpectedAccountNumber: "PA123"}, spy)
	if err != nil {
		t.Fatal(err)
	}
	if spy.calls != 1 {
		t.Fatalf("AccountSnapshot calls = %d, want 1", spy.calls)
	}
	if result.Environment != "paper" || result.AccountID != "account-id" || result.AccountNumber != "PA123" || result.Cash != 0 {
		t.Fatalf("unexpected preflight result: %+v", result)
	}
}

func TestPreflightRejectsUnsafeAccount(t *testing.T) {
	tests := []struct {
		name    string
		stocks  config.StocksConfig
		account execution.BrokerAccount
		err     error
	}{
		{"expected account mismatch", config.StocksConfig{ExpectedAccountNumber: "PA999"}, activeAccount(), nil},
		{"inactive", config.StocksConfig{}, execution.BrokerAccount{AccountNumber: "PA123", Status: "INACTIVE"}, nil},
		{"trading blocked", config.StocksConfig{}, execution.BrokerAccount{AccountNumber: "PA123", Status: "ACTIVE", TradingBlocked: true}, nil},
		{"missing admission balance metadata", config.StocksConfig{}, func() execution.BrokerAccount { a := activeAccount(); a.LastEquityAvailable = false; return a }(), nil},
		{"timeout", config.StocksConfig{}, execution.BrokerAccount{}, context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &preflightSpy{account: tt.account, err: tt.err}
			_, err := preflightStockAccount(context.Background(), tt.stocks, spy)
			if err == nil {
				t.Fatal("unsafe account accepted")
			}
			if tt.err != nil && !errors.Is(err, tt.err) {
				t.Fatalf("error = %v, want wrapping %v", err, tt.err)
			}
		})
	}
}
