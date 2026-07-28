package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestBuildBrokerReconcileReportIncludesOnlyDriftDetails(t *testing.T) {
	checkedAt := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC)
	positions := []execution.BrokerPosition{
		{Symbol: "IBKR", Quantity: 9.778289, Side: "long", AvgEntryPrice: 92.391},
		{Symbol: "AAPL", Quantity: 10, Side: "long", AvgEntryPrice: 100},
	}
	trades := []store.Trade{
		{ID: "trade-ibkr", Ticker: "IBKR", Direction: "long", Quantity: 20.778289, Status: "open", Strategy: "trend_following", OpenedAt: checkedAt},
		{ID: "trade-aapl", Ticker: "AAPL", Direction: "long", Quantity: 10, Status: "open", Strategy: "trend_following", OpenedAt: checkedAt},
	}
	drifts := []engine.BrokerDrift{{
		Ticker: "IBKR", BrokerQty: 9.778289, DBQty: 20.778289,
		DBRowCount: 1, Source: "mismatch",
	}}

	report := buildBrokerReconcileReport("alpaca", "", positions, trades, drifts, checkedAt)
	if report.Clean {
		t.Fatal("report marked clean despite drift")
	}
	if len(report.Drifts) != 1 || report.Drifts[0].Delta < -11.000001 || report.Drifts[0].Delta > -10.999999 {
		t.Fatalf("drifts = %+v, want one delta of -11", report.Drifts)
	}
	if len(report.BrokerPositions) != 1 || report.BrokerPositions[0].Symbol != "IBKR" {
		t.Fatalf("broker positions = %+v, want only IBKR", report.BrokerPositions)
	}
	if len(report.DBTrades) != 1 || report.DBTrades[0].ID != "trade-ibkr" {
		t.Fatalf("DB trades = %+v, want only trade-ibkr", report.DBTrades)
	}
	if report.Drifts[0].Recommendation == "" {
		t.Fatal("drift recommendation is empty")
	}
}

func TestBuildBrokerReconcileReportUsesEmptyDriftsArrayWhenClean(t *testing.T) {
	report := buildBrokerReconcileReport("alpaca", "", nil, nil, nil, time.Now().UTC())
	if report.Drifts == nil {
		t.Fatal("clean report Drifts is nil; JSON consumers require an empty array")
	}
	if len(report.Drifts) != 0 || !report.Clean {
		t.Fatalf("clean report = %+v", report)
	}
}

func TestReconcileRecommendation(t *testing.T) {
	tests := []struct {
		name  string
		drift engine.BrokerDrift
		want  string
	}{
		{name: "db only", drift: engine.BrokerDrift{Source: "db_only"}, want: "stale DB"},
		{name: "side mismatch", drift: engine.BrokerDrift{Source: "side_mismatch"}, want: "Direction conflict"},
		{name: "db overstates", drift: engine.BrokerDrift{Source: "mismatch", BrokerQty: 9, DBQty: 20}, want: "DB overstates"},
		{name: "broker surplus", drift: engine.BrokerDrift{Source: "mismatch", BrokerQty: 20, DBQty: 9}, want: "untracked surplus"},
		{name: "unequal opposite signs", drift: engine.BrokerDrift{Source: "mismatch", BrokerQty: -20, DBQty: 10}, want: "Direction conflict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reconcileRecommendation(tt.drift); !strings.Contains(got, tt.want) {
				t.Fatalf("recommendation = %q, want substring %q", got, tt.want)
			}
		})
	}
}

func TestUseConfiguredDatabaseEnv(t *testing.T) {
	t.Setenv("CUSTOM_DATABASE_URL", "postgres://custom.example/tradeengine")
	t.Setenv("DATABASE_URL", "postgres://original.example/tradeengine")
	cfg := &config.Config{}
	cfg.Database.URLEnv = "CUSTOM_DATABASE_URL"
	restore, err := useConfiguredDatabaseEnv(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("DATABASE_URL"); got != "postgres://custom.example/tradeengine" {
		t.Fatalf("DATABASE_URL = %q, want custom DSN", got)
	}
	restore()
	if got := os.Getenv("DATABASE_URL"); got != "postgres://original.example/tradeengine" {
		t.Fatalf("DATABASE_URL after restore = %q, want original DSN", got)
	}
}

func TestFilterReconcileSnapshotsIsCaseInsensitive(t *testing.T) {
	positions := filterBrokerPositions([]execution.BrokerPosition{
		{Symbol: "IBKR", Quantity: 1}, {Symbol: "AAPL", Quantity: 2},
	}, "ibkr")
	if len(positions) != 1 || positions[0].Symbol != "IBKR" {
		t.Fatalf("positions = %+v, want IBKR only", positions)
	}
	trades := filterReconcileTrades([]store.Trade{
		{Ticker: "IBKR", Quantity: 1}, {Ticker: "AAPL", Quantity: 2},
	}, "ibkr")
	if len(trades) != 1 || trades[0].Ticker != "IBKR" {
		t.Fatalf("trades = %+v, want IBKR only", trades)
	}
}
