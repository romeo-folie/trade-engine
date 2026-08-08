package store_test

import (
	"context"
	"encoding/json"
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
	completed, err := store.PoliticalDisclosureReportCompleted(ctx, report.Source, report.ExternalID)
	if err != nil || !completed {
		t.Fatalf("completed=%v err=%v", completed, err)
	}

	// Freshness is based on the filing date, not the older transaction date.
	claimed, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, now.AddDate(0, 0, -7), now, time.Hour, 10, "claim-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Ticker != "TESTDISC" ||
		claimed[0].PromotionClaimToken != "claim-token" {
		t.Fatalf("claimed = %+v", claimed)
	}
	if err := store.MarkPoliticalDisclosurePromotion(
		ctx, "TESTDISC", "claim-token", "rejected", "",
		"fixture rejection", now,
	); err != nil {
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
