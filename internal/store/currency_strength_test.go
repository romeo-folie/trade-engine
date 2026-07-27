package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestCurrencyStrengthRunLifecycleAndPagination(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	base := time.Date(2198, 7, 27, 0, 0, 0, 0, time.UTC)
	dates := []time.Time{base, base.AddDate(0, 0, -1), base.AddDate(0, 0, -2)}
	t.Cleanup(func() {
		_, _ = db.Exec(
			context.Background(),
			`DELETE FROM currency_strength_runs WHERE session_date BETWEEN $1 AND $2`,
			dates[2], dates[0],
		)
	})

	for _, date := range dates {
		if err := store.StartCurrencyStrengthRun(ctx, db, store.CurrencyStrengthRun{
			SessionDate:      date,
			Timezone:         "UTC",
			StrengthStart:    "06:00",
			StrengthEnd:      "09:00",
			ExitTime:         "18:00",
			RankInstruments:  []string{"EUR/USD", "GBP/USD", "USD/JPY"},
			TradeInstruments: []string{"GBP/USD"},
			Units:            100000,
		}); err != nil {
			t.Fatalf("StartCurrencyStrengthRun(%s): %v", date.Format("2006-01-02"), err)
		}
	}

	enteredAt := base.Add(9 * time.Hour)
	if err := store.MarkCurrencyStrengthEvaluating(ctx, db, base); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCurrencyStrengthEntry(ctx, db, store.CurrencyStrengthRun{
		SessionDate:       base,
		StrongestCurrency: "GBP",
		WeakestCurrency:   "USD",
		StrengthGap:       0.0123,
		SignalSymbol:      "GBP/USD",
		SignalSide:        "buy",
		EntryOrderID:      "ENTRY-TEST",
		EntryStatus:       "filled",
		EntryFillPrice:    1.1,
		EntryFilledQty:    100000,
		EnteredAt:         &enteredAt,
	}); err != nil {
		t.Fatal(err)
	}
	exitedAt := base.Add(18 * time.Hour)
	pnl := 500.0
	if err := store.RecordCurrencyStrengthExit(ctx, db, store.CurrencyStrengthRun{
		SessionDate:   base,
		ExitOrderID:   "EXIT-TEST",
		ExitStatus:    "filled",
		ExitFillPrice: 1.105,
		ExitFilledQty: 100000,
		ExitedAt:      &exitedAt,
		RealizedPnL:   &pnl,
	}); err != nil {
		t.Fatal(err)
	}
	// Replaying the same terminal acknowledgement after a crash is idempotent.
	if err := store.RecordCurrencyStrengthExit(ctx, db, store.CurrencyStrengthRun{
		SessionDate:   base,
		ExitOrderID:   "EXIT-TEST",
		ExitStatus:    "filled",
		ExitFillPrice: 1.105,
		ExitFilledQty: 100000,
		ExitedAt:      &exitedAt,
		RealizedPnL:   &pnl,
	}); err != nil {
		t.Fatalf("terminal exit replay: %v", err)
	}
	if err := store.RecordCurrencyStrengthNoSignal(
		ctx, db, dates[1], "minimum strength gap not met",
	); err != nil {
		t.Fatal(err)
	}
	if err := store.StartCurrencyStrengthRun(ctx, db, store.CurrencyStrengthRun{
		SessionDate:      dates[1],
		Timezone:         "America/New_York",
		StrengthStart:    "07:00",
		StrengthEnd:      "10:00",
		ExitTime:         "19:00",
		RankInstruments:  []string{"AUD/USD", "NZD/USD", "USD/CAD"},
		TradeInstruments: []string{"AUD/USD"},
		Units:            250000,
	}); err != nil {
		t.Fatal(err)
	}
	immutable, err := store.GetCurrencyStrengthRunByDate(ctx, db, dates[1])
	if err != nil {
		t.Fatal(err)
	}
	if immutable == nil || immutable.Timezone != "UTC" || immutable.Units != 100000 ||
		immutable.StrengthStart != "06:00" {
		t.Fatalf("completed run configuration was mutated: %+v", immutable)
	}
	if err := store.RecordCurrencyStrengthEntry(ctx, db, store.CurrencyStrengthRun{
		SessionDate:    dates[2],
		SignalSymbol:   "GBP/USD",
		SignalSide:     "buy",
		EntryOrderID:   "ENTRY-PENDING",
		EntryStatus:    "pending",
		EntryFilledQty: 0,
	}); err != nil {
		t.Fatal(err)
	}
	submitted, err := store.GetCurrencyStrengthRunByDate(ctx, db, dates[2])
	if err != nil {
		t.Fatal(err)
	}
	if submitted == nil || submitted.Status != "entry_submitted" {
		t.Fatalf("pending entry presented as entered: %+v", submitted)
	}
	pendingExitAt := dates[2].Add(18 * time.Hour)
	if err := store.RecordCurrencyStrengthExit(ctx, db, store.CurrencyStrengthRun{
		SessionDate: dates[2],
		ExitOrderID: "EXIT-PENDING",
		ExitStatus:  "pending",
		ExitedAt:    &pendingExitAt,
	}); err != nil {
		t.Fatal(err)
	}

	first, err := store.ListCurrencyStrengthRunsPage(ctx, db, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || !first[0].SessionDate.Equal(base) ||
		first[0].Status != "exited" || first[0].RealizedPnL == nil {
		t.Fatalf("unexpected first page: %+v", first)
	}
	second, err := store.ListCurrencyStrengthRunsPage(ctx, db, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) == 0 || !second[0].SessionDate.Equal(dates[2]) {
		t.Fatalf("unexpected second page: %+v", second)
	}
	if second[0].Status != "exit_submitted" {
		t.Fatalf("pending flatten presented as %q, want exit_submitted", second[0].Status)
	}
	active, err := store.GetActiveCurrencyStrengthRun(ctx, db, base)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || !active.SessionDate.Equal(dates[2]) ||
		active.ExitOrderID != "EXIT-PENDING" {
		t.Fatalf("active prior-day obligation not found: %+v", active)
	}
}

func TestGetRecentORBSessionsPageUsesOffset(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	date := time.Date(2199, 1, 1, 0, 0, 0, 0, time.UTC)
	suffix := time.Now().UnixNano()
	names := []string{
		fmt.Sprintf("page-%d-0", suffix),
		fmt.Sprintf("page-%d-1", suffix),
		fmt.Sprintf("page-%d-2", suffix),
	}
	t.Cleanup(func() {
		_, _ = db.Exec(
			context.Background(),
			`DELETE FROM orb_sessions WHERE session_name = ANY($1)`,
			names,
		)
	})
	for _, name := range names {
		if _, err := store.CreateORBSession(ctx, db, date, name); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Millisecond)
	}

	first, err := store.GetRecentORBSessionsPage(ctx, db, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.GetRecentORBSessionsPage(ctx, db, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].SessionName != names[2] ||
		first[1].SessionName != names[1] {
		t.Fatalf("unexpected first ORB page: %+v", first)
	}
	if len(second) == 0 || second[0].SessionName != names[0] {
		t.Fatalf("unexpected second ORB page: %+v", second)
	}
}
