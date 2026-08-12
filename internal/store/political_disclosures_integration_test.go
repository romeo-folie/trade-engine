package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestPoliticalDisclosurePersistenceAndClaimLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2099, time.April, 4, 15, 0, 0, 0, time.UTC)
	transactionDate := now.AddDate(0, 0, -30)
	minimum, maximum := int64(15001), int64(50000)
	report := store.PoliticalDisclosureReport{
		Source: "house", ExternalID: "test-disclosure-claim", Chamber: "house",
		FilerName: "Hon. Test Member", Office: "TS01",
		ReportType: "periodic_transaction", FilingDate: now.Add(-time.Hour),
		DocumentURL: "https://example.test/test-disclosure-claim.pdf",
		ContentHash: "fixture", ExtractionStatus: "completed",
		RawMetadata: json.RawMessage(`{"fixture":true}`),
		Transactions: []store.PoliticalDisclosureTransaction{{
			Ordinal: 1, AssetName: "Example Corporation (TESTDISC) [ST]",
			Ticker: "TESTDISC", AssetType: "ST", TransactionType: "purchase",
			TransactionDate: transactionDate, AmountMin: &minimum, AmountMax: &maximum,
			AmountText: "$15,001 - $50,000", RawData: json.RawMessage(`{"row":1}`),
		}},
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(),
			"DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2",
			report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	summaries, total, err := store.ListPoliticalDisclosureReports(
		ctx, store.PoliticalDisclosureReportFilter{
			From: now.AddDate(0, 0, -1), To: now,
			Source: "house", Ticker: "testdisc", Limit: 10,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(summaries) != 1 || summaries[0].TransactionCount != 1 ||
		len(summaries[0].Tickers) != 1 || summaries[0].Tickers[0] != "TESTDISC" {
		t.Fatalf("summaries total=%d rows=%+v", total, summaries)
	}
	detail, err := store.GetPoliticalDisclosureReport(ctx, "house", report.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if detail == nil || len(detail.Transactions) != 1 ||
		detail.Transactions[0].Ticker != "TESTDISC" {
		t.Fatalf("detail = %+v", detail)
	}
	terminal, err := store.PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID)
	if err != nil || !terminal {
		t.Fatalf("terminal=%v err=%v", terminal, err)
	}
	report2 := report
	report2.ExternalID = "test-disclosure-claim-2"
	report2.DocumentURL += "?second=1"
	if err := store.SavePoliticalDisclosureReport(ctx, report2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), "DELETE FROM watchlist WHERE ticker=$1", "TESTDISC")
		_, _ = store.GetDB().Exec(context.Background(), "DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2", report2.Source, report2.ExternalID)
	})

	// Freshness is based on the filing date, not the older transaction date.
	claimed, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, now.AddDate(0, 0, -7), now, time.Hour, 10, "claim-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || claimed[0].Ticker != "TESTDISC" ||
		claimed[0].PromotionClaimToken != "claim-token" {
		t.Fatalf("claimed = %+v", claimed)
	}
	item := store.WatchlistItem{Ticker: "TESTDISC", Platform: "stock", Strategies: []string{"political_disclosure"}, Timeframes: []string{"1D"}, MinConfidence: .8, Source: "scanner"}
	err = store.PromoteScannerSourcesToWatchlist(ctx, item, store.ScannerSourcePromotionClaims{PoliticalDisclosure: &store.PoliticalDisclosurePromotionClaim{Ticker: "TESTDISC", Token: "stale-token", ReportCount: 2}}, "stale", now)
	var claimErr *store.ScannerSourcePromotionClaimError
	if !errors.As(err, &claimErr) || claimErr.Source != "political_disclosure" {
		t.Fatalf("stale claim error = %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_candidates
		SET promotion_status='superseded', promotion_claim_token=NULL,
			promotion_claimed_at=NULL
		WHERE report_id=$1 AND ticker=$2`, claimed[0].ReportID, "TESTDISC"); err != nil {
		t.Fatal(err)
	}
	err = store.PromoteScannerSourcesToWatchlist(ctx, item, store.ScannerSourcePromotionClaims{PoliticalDisclosure: &store.PoliticalDisclosurePromotionClaim{Ticker: "TESTDISC", Token: "claim-token", ReportCount: 2}}, "incomplete set", now)
	if !errors.As(err, &claimErr) || claimErr.Source != "political_disclosure" {
		t.Fatalf("incomplete multi-report claim error = %v", err)
	}
	var stillProcessing int
	if err := store.GetDB().QueryRow(ctx, `
		SELECT count(*) FROM political_disclosure_candidates
		WHERE ticker=$1 AND promotion_status='processing'
		  AND promotion_claim_token=$2`, "TESTDISC", "claim-token").Scan(&stillProcessing); err != nil {
		t.Fatal(err)
	}
	if stillProcessing != 1 {
		t.Fatalf("remaining processing rows after rollback = %d", stillProcessing)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_candidates
		SET promotion_status='processing', promotion_claim_token=$3,
			promotion_claimed_at=$4
		WHERE report_id=$1 AND ticker=$2`, claimed[0].ReportID, "TESTDISC", "claim-token", now); err != nil {
		t.Fatal(err)
	}
	if err := store.PromoteScannerSourcesToWatchlist(ctx, item, store.ScannerSourcePromotionClaims{PoliticalDisclosure: &store.PoliticalDisclosurePromotionClaim{Ticker: "TESTDISC", Token: "claim-token", ReportCount: 2}}, "fixture success", now); err != nil {
		t.Fatal(err)
	}
	var promoted int
	if err := store.GetDB().QueryRow(ctx, "SELECT count(*) FROM political_disclosure_candidates WHERE ticker=$1 AND promotion_status='promoted'", "TESTDISC").Scan(&promoted); err != nil {
		t.Fatal(err)
	}
	if promoted != 2 {
		t.Fatalf("promoted rows = %d", promoted)
	}
	if _, err := store.GetDB().Exec(ctx, "DELETE FROM watchlist WHERE ticker=$1", "TESTDISC"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDB().Exec(ctx, "UPDATE political_disclosure_candidates SET promotion_status='rejected' WHERE ticker=$1", "TESTDISC"); err != nil {
		t.Fatal(err)
	}

	// Re-extraction refreshes evidence but does not resurrect a terminal review.
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	again, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, now.AddDate(0, 0, -7), now.Add(time.Minute), time.Hour, 10, "second-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("terminal candidate was reclaimed: %+v", again)
	}
}

func TestFailedPoliticalDisclosureReportIsTerminalForAutomaticSync(t *testing.T) {
	ctx := context.Background()
	report := store.PoliticalDisclosureReport{
		Source: "oge", ExternalID: "test-terminal-failed-disclosure",
		Chamber: "executive", FilerName: "Test Official",
		ReportType: "periodic_transaction", FilingDate: time.Now().UTC(),
		DocumentURL:      "https://example.test/failed.pdf",
		ExtractionStatus: "failed", ExtractionError: "fixture extraction error",
		RawMetadata: json.RawMessage(`{"fixture":true}`),
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(),
			"DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2",
			report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	terminal, err := store.PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID)
	if err != nil || !terminal {
		t.Fatalf("terminal=%v err=%v", terminal, err)
	}
}
