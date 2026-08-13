package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestBuyControlPersistsOperatorAudit(t *testing.T) {
	ctx := context.Background()
	if err := store.SetBuyControl(ctx, store.BuyControlStocks, false, "test-operator"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM buy_controls WHERE scope = $1`, store.BuyControlStocks)
	})
	control, err := store.GetBuyControl(ctx, store.BuyControlStocks)
	if err != nil {
		t.Fatal(err)
	}
	if control.Enabled || control.UpdatedBy != "test-operator" || control.UpdatedAt.IsZero() {
		t.Fatalf("persisted buy control = %+v", control)
	}
	enabled, err := store.StockBuyEntriesEnabled(ctx)
	if err != nil || enabled {
		t.Fatalf("shared stock-buy gate = enabled:%t err:%v, want disabled/nil", enabled, err)
	}
}

func TestAccountRiskBaselineIsAccountScopedAndKeepsPeak(t *testing.T) {
	ctx := context.Background()
	accountA := fmt.Sprintf("baseline-test-a-%d", time.Now().UnixNano())
	accountB := fmt.Sprintf("baseline-test-b-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM account_risk_baselines WHERE account_id IN ($1, $2)`, accountA, accountB)
	})
	now := time.Date(2026, time.August, 13, 15, 0, 0, 0, time.UTC)
	first, err := store.ObserveAccountRiskBaseline(ctx, accountA, 1_000, 990, now)
	if err != nil || first.PeakEquity != 1_000 || first.LastEquity != 1_000 {
		t.Fatalf("first baseline = %+v, err=%v", first, err)
	}
	second, err := store.ObserveAccountRiskBaseline(ctx, accountA, 950, 990, now.Add(time.Hour))
	if err != nil || second.PeakEquity != 1_000 || second.LastEquity != 1_000 {
		t.Fatalf("same-day baseline = %+v, err=%v", second, err)
	}
	nextDay, err := store.ObserveAccountRiskBaseline(ctx, accountA, 960, 955, now.AddDate(0, 0, 1))
	if err != nil || nextDay.PeakEquity != 1_000 || nextDay.LastEquity != 955 {
		t.Fatalf("next-day baseline = %+v, err=%v", nextDay, err)
	}
	other, err := store.ObserveAccountRiskBaseline(ctx, accountB, 500, 490, now)
	if err != nil || other.PeakEquity != 500 || other.LastEquity != 500 {
		t.Fatalf("independent account baseline = %+v, err=%v", other, err)
	}
}
