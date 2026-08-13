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
	if err != nil || first.PeakEquity != 1_000 || first.LastEquity != 990 {
		t.Fatalf("first baseline = %+v, err=%v", first, err)
	}
	second, err := store.ObserveAccountRiskBaseline(ctx, accountA, 950, 990, now.Add(time.Hour))
	if err != nil || second.PeakEquity != 1_000 || second.LastEquity != 990 {
		t.Fatalf("same-day baseline = %+v, err=%v", second, err)
	}
	sameDayRollover, err := store.ObserveAccountRiskBaseline(ctx, accountA, 960, 955, now.Add(2*time.Hour))
	if err != nil || sameDayRollover.PeakEquity != 1_000 || sameDayRollover.LastEquity != 955 {
		t.Fatalf("same-day broker rollover baseline = %+v, err=%v", sameDayRollover, err)
	}
	nextDay, err := store.ObserveAccountRiskBaseline(ctx, accountA, 960, 955, now.AddDate(0, 0, 1))
	if err != nil || nextDay.PeakEquity != 1_000 || nextDay.LastEquity != 955 {
		t.Fatalf("next-day baseline = %+v, err=%v", nextDay, err)
	}
	reset, err := store.ResetAccountRiskBaseline(ctx, accountA, 500, 955, now.AddDate(0, 0, 2))
	if err != nil || reset.PeakEquity != 500 || reset.LastEquity != 500 {
		t.Fatalf("reset baseline = %+v, err=%v", reset, err)
	}
	// This snapshot was fetched before the reset but completes after it; it must
	// not restore the retired 1,000 peak.
	afterReset, err := store.ObserveAccountRiskBaseline(ctx, accountA, 1_000, 955, now.AddDate(0, 0, 1))
	if err != nil || afterReset.PeakEquity != 500 || afterReset.LastEquity != 500 || !afterReset.BaselineDate.Equal(reset.BaselineDate) {
		t.Fatalf("post-reset baseline = %+v, err=%v", afterReset, err)
	}
	// A request started before the reset can carry a different prior-session
	// reference around rollover. It must not replace the reset's daily baseline
	// or consume its stale-reference marker.
	staleRollover, err := store.ObserveAccountRiskBaseline(ctx, accountA, 1_000, 940, now.AddDate(0, 0, 1).Add(time.Hour))
	if err != nil || staleRollover.LastEquity != 500 || !staleRollover.BaselineDate.Equal(reset.BaselineDate) {
		t.Fatalf("late pre-reset rollover changed daily baseline: %+v, err=%v", staleRollover, err)
	}
	postResetGain, err := store.ObserveAccountRiskBaseline(ctx, accountA, 600, 955, now.AddDate(0, 0, 3))
	if err != nil || postResetGain.PeakEquity != 600 || postResetGain.LastEquity != 500 {
		t.Fatalf("post-reset gain did not ratchet peak: %+v, err=%v", postResetGain, err)
	}
	// A running engine may retain its pre-reset high after the CLI reset. A
	// newer broker snapshot must ratchet from its own equity rather than letting
	// that stale in-memory high suppress all post-reset gains.
	postResetRecoveredPeak, err := store.ObserveAccountRiskBaselineWithPeak(ctx, accountA, 650, 1_000, 955, now.AddDate(0, 0, 3).Add(time.Hour), now.AddDate(0, 0, 1))
	if err != nil || postResetRecoveredPeak.PeakEquity != 650 {
		t.Fatalf("post-reset snapshot could not replace stale in-memory peak: %+v, err=%v", postResetRecoveredPeak, err)
	}
	resumed, err := store.ObserveAccountRiskBaseline(ctx, accountA, 540, 505, now.AddDate(0, 0, 3).Add(time.Hour))
	if err != nil || resumed.PeakEquity != 650 || resumed.LastEquity != 505 {
		t.Fatalf("post-reset baseline did not resume after broker reference changed: %+v, err=%v", resumed, err)
	}
	// Even after Alpaca's prior-session reference advances, a request begun
	// before the reset can complete late. Its older observation timestamp must
	// keep the reset peak intact.
	latePreReset, err := store.ObserveAccountRiskBaseline(ctx, accountA, 1_000, 505, now.AddDate(0, 0, 1))
	if err != nil || latePreReset.PeakEquity != 650 {
		t.Fatalf("late pre-reset snapshot restored peak: %+v, err=%v", latePreReset, err)
	}
	// A second reset before Alpaca advances its reference must keep the first
	// stale marker rather than treating the first reset equity as that marker.
	resetAgain, err := store.ResetAccountRiskBaseline(ctx, accountA, 400, 505, now.AddDate(0, 0, 4))
	if err != nil || resetAgain.PeakEquity != 400 || resetAgain.LastEquity != 400 {
		t.Fatalf("second reset baseline = %+v, err=%v", resetAgain, err)
	}
	stillStale, err := store.ObserveAccountRiskBaseline(ctx, accountA, 400, 505, now.AddDate(0, 0, 5))
	if err != nil || stillStale.LastEquity != 400 || !stillStale.BaselineDate.Equal(resetAgain.BaselineDate) {
		t.Fatalf("second reset stale reference was not retained: %+v, err=%v", stillStale, err)
	}
	firstResetAccount := fmt.Sprintf("baseline-test-first-reset-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM account_risk_baselines WHERE account_id = $1`, firstResetAccount)
	})
	firstReset, err := store.ResetAccountRiskBaseline(ctx, firstResetAccount, 500, 1_000, now)
	if err != nil || firstReset.LastEquity != 500 {
		t.Fatalf("first reset baseline = %+v, err=%v", firstReset, err)
	}
	firstResetStale, err := store.ObserveAccountRiskBaseline(ctx, firstResetAccount, 500, 1_000, now.AddDate(0, 0, 1))
	if err != nil || firstResetStale.LastEquity != 500 || !firstResetStale.BaselineDate.Equal(firstReset.BaselineDate) {
		t.Fatalf("first reset stale reference was not retained: %+v, err=%v", firstResetStale, err)
	}
	offlineResetAccount := fmt.Sprintf("baseline-test-offline-reset-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM account_risk_baselines WHERE account_id = $1`, offlineResetAccount)
	})
	if _, err := store.ObserveAccountRiskBaseline(ctx, offlineResetAccount, 900, 900, now); err != nil {
		t.Fatal(err)
	}
	offlineReset, err := store.ResetAccountRiskBaseline(ctx, offlineResetAccount, 500, 1_000, now.AddDate(0, 0, 1))
	if err != nil || offlineReset.LastEquity != 500 {
		t.Fatalf("offline reset baseline = %+v, err=%v", offlineReset, err)
	}
	offlineResetStale, err := store.ObserveAccountRiskBaseline(ctx, offlineResetAccount, 500, 1_000, now.AddDate(0, 0, 2))
	if err != nil || offlineResetStale.LastEquity != 500 || !offlineResetStale.BaselineDate.Equal(offlineReset.BaselineDate) {
		t.Fatalf("offline reset stale reference was not retained: %+v, err=%v", offlineResetStale, err)
	}
	repeatedResetAccount := fmt.Sprintf("baseline-test-repeated-reset-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM account_risk_baselines WHERE account_id = $1`, repeatedResetAccount)
	})
	if _, err := store.ResetAccountRiskBaseline(ctx, repeatedResetAccount, 500, 1_000, now); err != nil {
		t.Fatal(err)
	}
	repeatedReset, err := store.ResetAccountRiskBaseline(ctx, repeatedResetAccount, 700, 1_100, now.AddDate(0, 0, 1))
	if err != nil || repeatedReset.LastEquity != 700 {
		t.Fatalf("repeated reset baseline = %+v, err=%v", repeatedReset, err)
	}
	repeatedResetStale, err := store.ObserveAccountRiskBaseline(ctx, repeatedResetAccount, 700, 1_100, now.AddDate(0, 0, 2))
	if err != nil || repeatedResetStale.LastEquity != 700 || !repeatedResetStale.BaselineDate.Equal(repeatedReset.BaselineDate) {
		t.Fatalf("repeated reset stale reference was not retained: %+v, err=%v", repeatedResetStale, err)
	}
	zeroReferenceAccount := fmt.Sprintf("baseline-test-zero-reference-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM account_risk_baselines WHERE account_id = $1`, zeroReferenceAccount)
	})
	zeroReferenceReset, err := store.ResetAccountRiskBaseline(ctx, zeroReferenceAccount, 500, 0, now)
	if err != nil || zeroReferenceReset.LastEquity != 500 {
		t.Fatalf("zero-reference reset baseline = %+v, err=%v", zeroReferenceReset, err)
	}
	zeroReferenceLoss, err := store.ObserveAccountRiskBaseline(ctx, zeroReferenceAccount, 490, 0, now.Add(time.Hour))
	if err != nil || zeroReferenceLoss.LastEquity != 500 {
		t.Fatalf("zero prior-session equity moved reset baseline with current equity: %+v, err=%v", zeroReferenceLoss, err)
	}
	zeroReferenceResumed, err := store.ObserveAccountRiskBaseline(ctx, zeroReferenceAccount, 490, 480, now.Add(2*time.Hour))
	if err != nil || zeroReferenceResumed.LastEquity != 480 {
		t.Fatalf("nonzero prior-session equity did not resume baseline observation: %+v, err=%v", zeroReferenceResumed, err)
	}
	newZeroReferenceAccount := fmt.Sprintf("baseline-test-new-zero-reference-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM account_risk_baselines WHERE account_id = $1`, newZeroReferenceAccount)
	})
	newZeroReference, err := store.ObserveAccountRiskBaseline(ctx, newZeroReferenceAccount, 500, 0, now)
	if err != nil || newZeroReference.LastEquity != 500 {
		t.Fatalf("new zero-reference baseline = %+v, err=%v", newZeroReference, err)
	}
	newZeroReferenceLoss, err := store.ObserveAccountRiskBaseline(ctx, newZeroReferenceAccount, 490, 0, now.Add(time.Hour))
	if err != nil || newZeroReferenceLoss.LastEquity != 500 {
		t.Fatalf("new zero-reference baseline followed current equity: %+v, err=%v", newZeroReferenceLoss, err)
	}
	rolloverAccount := fmt.Sprintf("baseline-test-rollover-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM account_risk_baselines WHERE account_id = $1`, rolloverAccount)
	})
	if _, err := store.ObserveAccountRiskBaseline(ctx, rolloverAccount, 1_000, 990, now); err != nil {
		t.Fatal(err)
	}
	newerRollover, err := store.ObserveAccountRiskBaseline(ctx, rolloverAccount, 1_010, 1_000, now.Add(time.Hour))
	if err != nil || newerRollover.LastEquity != 1_000 {
		t.Fatalf("newer rollover baseline = %+v, err=%v", newerRollover, err)
	}
	olderRollover, err := store.ObserveAccountRiskBaseline(ctx, rolloverAccount, 1_005, 990, now.Add(30*time.Minute))
	if err != nil || olderRollover.LastEquity != 1_000 {
		t.Fatalf("late older observation restored stale daily reference: %+v, err=%v", olderRollover, err)
	}
	other, err := store.ObserveAccountRiskBaseline(ctx, accountB, 500, 490, now)
	if err != nil || other.PeakEquity != 500 || other.LastEquity != 490 {
		t.Fatalf("independent account baseline = %+v, err=%v", other, err)
	}
	underwaterAccount := fmt.Sprintf("baseline-test-underwater-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM account_risk_baselines WHERE account_id = $1`, underwaterAccount)
	})
	underwater, err := store.ObserveAccountRiskBaseline(ctx, underwaterAccount, 850, 1_000, now)
	if err != nil || underwater.PeakEquity != 1_000 || underwater.LastEquity != 1_000 {
		t.Fatalf("underwater first baseline = %+v, err=%v", underwater, err)
	}
	retainedPeakAccount := fmt.Sprintf("baseline-test-retained-peak-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM account_risk_baselines WHERE account_id = $1`, retainedPeakAccount)
	})
	if _, err := store.ObserveAccountRiskBaseline(ctx, retainedPeakAccount, 1_000, 990, now); err != nil {
		t.Fatal(err)
	}
	// A 1,200 snapshot was remembered in-process while persistence was down.
	// The next successful, lower snapshot must still durablely restore that high.
	retainedPeak, err := store.ObserveAccountRiskBaselineWithPeak(ctx, retainedPeakAccount, 1_100, 1_200, 990, now.Add(time.Hour), now.Add(30*time.Minute))
	if err != nil || retainedPeak.PeakEquity != 1_200 {
		t.Fatalf("retained high watermark was lost after persistence recovery: %+v, err=%v", retainedPeak, err)
	}
}
