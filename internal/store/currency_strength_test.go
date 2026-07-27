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
	accountID := "DU-CURRENCY-STRENGTH-TEST"
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
			AccountID:        accountID,
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
	if err := store.MarkCurrencyStrengthEvaluating(ctx, db, accountID, base); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCurrencyStrengthEntry(ctx, db, store.CurrencyStrengthRun{
		AccountID:         accountID,
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
		AccountID:     accountID,
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
		AccountID:     accountID,
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
		ctx, db, accountID, dates[1], "minimum strength gap not met",
	); err != nil {
		t.Fatal(err)
	}
	if err := store.StartCurrencyStrengthRun(ctx, db, store.CurrencyStrengthRun{
		AccountID:        accountID,
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
	immutable, err := store.GetCurrencyStrengthRunByDate(ctx, db, accountID, dates[1])
	if err != nil {
		t.Fatal(err)
	}
	if immutable == nil || immutable.Timezone != "UTC" || immutable.Units != 100000 ||
		immutable.StrengthStart != "06:00" {
		t.Fatalf("completed run configuration was mutated: %+v", immutable)
	}
	if err := store.RecordCurrencyStrengthEntry(ctx, db, store.CurrencyStrengthRun{
		AccountID:      accountID,
		SessionDate:    dates[2],
		SignalSymbol:   "GBP/USD",
		SignalSide:     "buy",
		EntryOrderID:   "ENTRY-PENDING",
		EntryStatus:    "pending",
		EntryFilledQty: 0,
	}); err != nil {
		t.Fatal(err)
	}
	submitted, err := store.GetCurrencyStrengthRunByDate(ctx, db, accountID, dates[2])
	if err != nil {
		t.Fatal(err)
	}
	if submitted == nil || submitted.Status != "entry_submitted" {
		t.Fatalf("pending entry presented as entered: %+v", submitted)
	}
	pendingExitAt := dates[2].Add(18 * time.Hour)
	if err := store.RecordCurrencyStrengthExit(ctx, db, store.CurrencyStrengthRun{
		AccountID:   accountID,
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
	active, err := store.GetActiveCurrencyStrengthRun(ctx, db, accountID, base)
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
		fmt.Sprintf("page-%d-new", suffix),
	}
	t.Cleanup(func() {
		_, _ = db.Exec(
			context.Background(),
			`DELETE FROM orb_sessions WHERE session_name = ANY($1)`,
			names,
		)
	})
	for _, name := range names[:3] {
		if _, err := store.CreateORBSession(ctx, db, date, name); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Millisecond)
	}

	snapshot, err := store.ORBSessionsSnapshotTime(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.GetRecentORBSessionsPageAt(ctx, db, 2, 0, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateORBSession(ctx, db, date, names[3]); err != nil {
		t.Fatal(err)
	}
	second, err := store.GetRecentORBSessionsPageAt(ctx, db, 2, 2, snapshot)
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

func TestCurrencyStrengthRunsAreAccountScopedAndAggregateExitAttempts(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	sessionDate := time.Date(2197, 7, 27, 0, 0, 0, 0, time.UTC)
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(),
			`DELETE FROM currency_strength_runs WHERE session_date=$1`, sessionDate)
	})
	start := func(account string) {
		t.Helper()
		if err := store.StartCurrencyStrengthRun(ctx, db, store.CurrencyStrengthRun{
			AccountID: account, SessionDate: sessionDate, Timezone: "UTC",
			StrengthStart: "06:00", StrengthEnd: "09:00", ExitTime: "18:00",
			RankInstruments: []string{"GBP/USD"}, TradeInstruments: []string{"GBP/USD"},
			Units: 100000,
		}); err != nil {
			t.Fatal(err)
		}
	}
	start("DU-ACCOUNT-A")
	start("DU-ACCOUNT-B")

	if err := store.RecordCurrencyStrengthEntry(ctx, db, store.CurrencyStrengthRun{
		AccountID: "DU-ACCOUNT-A", SessionDate: sessionDate,
		SignalSymbol: "GBP/USD", SignalSide: "buy", EntryOrderID: "ENTRY-A",
		EntryStatus: "filled", EntryFillPrice: 1.1, EntryFilledQty: 100000,
	}); err != nil {
		t.Fatal(err)
	}
	firstPnL, secondPnL := 200.0, 360.0
	for _, attempt := range []store.CurrencyStrengthRun{
		{
			AccountID: "DU-ACCOUNT-A", SessionDate: sessionDate,
			ExitOrderID: "EXIT-A-1", ExitStatus: "residual",
			ExitFillPrice: 1.105, ExitFilledQty: 40000, RealizedPnL: &firstPnL,
		},
		{
			AccountID: "DU-ACCOUNT-A", SessionDate: sessionDate,
			ExitOrderID: "EXIT-A-2", ExitStatus: "filled",
			ExitFillPrice: 1.106, ExitFilledQty: 60000, RealizedPnL: &secondPnL,
		},
	} {
		if err := store.RecordCurrencyStrengthExit(ctx, db, attempt); err != nil {
			t.Fatal(err)
		}
	}
	// Replaying the second acknowledgement must not double-count it.
	if err := store.RecordCurrencyStrengthExit(ctx, db, store.CurrencyStrengthRun{
		AccountID: "DU-ACCOUNT-A", SessionDate: sessionDate,
		ExitOrderID: "EXIT-A-2", ExitStatus: "filled",
		ExitFillPrice: 1.106, ExitFilledQty: 60000, RealizedPnL: &secondPnL,
	}); err != nil {
		t.Fatal(err)
	}

	runA, err := store.GetCurrencyStrengthRunByDate(ctx, db, "DU-ACCOUNT-A", sessionDate)
	if err != nil {
		t.Fatal(err)
	}
	runB, err := store.GetCurrencyStrengthRunByDate(ctx, db, "DU-ACCOUNT-B", sessionDate)
	if err != nil {
		t.Fatal(err)
	}
	if runA == nil || runA.ExitFilledQty != 100000 || runA.RealizedPnL == nil ||
		*runA.RealizedPnL != 560 || runA.ExitFillPrice < 1.10559 ||
		runA.ExitFillPrice > 1.10561 {
		if runA == nil {
			t.Fatal("account A run missing")
		}
		t.Fatalf("exit attempts were not aggregated idempotently: qty=%.2f price=%.8f pnl=%v run=%+v",
			runA.ExitFilledQty, runA.ExitFillPrice, runA.RealizedPnL, runA)
	}
	if runB == nil || runB.Status != "scheduled" || runB.EntryOrderID != "" {
		t.Fatalf("account B was contaminated by account A: %+v", runB)
	}
}

func TestCurrencyStrengthPaginationSnapshotExcludesNewRuns(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()
	accountID := "DU-CURRENCY-SNAPSHOT-TEST"
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(),
			`DELETE FROM currency_strength_runs WHERE account_id=$1`, accountID)
	})
	start := func(date time.Time) {
		t.Helper()
		if err := store.StartCurrencyStrengthRun(ctx, db, store.CurrencyStrengthRun{
			AccountID: accountID, SessionDate: date, Timezone: "UTC",
			StrengthStart: "06:00", StrengthEnd: "09:00", ExitTime: "18:00",
			RankInstruments: []string{"GBP/USD"}, TradeInstruments: []string{"GBP/USD"},
			Units: 100000,
		}); err != nil {
			t.Fatal(err)
		}
	}
	dates := []time.Time{
		time.Date(2200, 1, 10, 0, 0, 0, 0, time.UTC),
		time.Date(2200, 1, 9, 0, 0, 0, 0, time.UTC),
		time.Date(2200, 1, 8, 0, 0, 0, 0, time.UTC),
	}
	for _, date := range dates {
		start(date)
	}
	snapshot, err := store.HistorySnapshotTime(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.ListCurrencyStrengthRunsPageAt(ctx, db, 2, 0, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	start(time.Date(2201, 1, 1, 0, 0, 0, 0, time.UTC))
	second, err := store.ListCurrencyStrengthRunsPageAt(ctx, db, 2, 2, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || !first[0].SessionDate.Equal(dates[0]) ||
		!first[1].SessionDate.Equal(dates[1]) {
		t.Fatalf("unexpected first snapshot page: %+v", first)
	}
	if len(second) != 1 || !second[0].SessionDate.Equal(dates[2]) {
		t.Fatalf("new run shifted the pinned history page: %+v", second)
	}
}
