package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/disclosures"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type mutableSenateTransport struct {
	failNewest bool
}

func (t *mutableSenateTransport) SearchPTRs(
	context.Context, time.Time, time.Time, int, int,
) ([]byte, error) {
	return []byte(`{"recordsFiltered":2,"data":[["Newest","Member","Office","<a href='/search/view/ptr/retry-newest/'>PTR</a>","08/10/2026"],["Older","Member","Office","<a href='/search/view/ptr/retry-older/'>PTR</a>","08/09/2026"]]}`), nil
}

func (t *mutableSenateTransport) FetchPTR(_ context.Context, documentURL string) ([]byte, error) {
	if t.failNewest && strings.Contains(documentURL, "/retry-newest/") {
		return nil, errors.New("temporary Senate eFD failure")
	}
	return []byte(`<table><tr><th>Transaction Date</th><th>Ticker</th><th>Asset Name</th><th>Transaction Type</th><th>Amount</th></tr><tr><td>08/08/2026</td><td>MSFT</td><td>Microsoft</td><td>Purchase</td><td>$1,001 - $15,000</td></tr></table>`), nil
}

func (t *mutableSenateTransport) Close() error { return nil }

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
		claimed[0].PromotionClaimToken != "claim-token" ||
		claimed[0].ExternalID == "" || claimed[0].Chamber != "house" ||
		claimed[0].Office != "TS01" || claimed[0].ReportType != "periodic_transaction" ||
		claimed[0].ExtractionStatus != "completed" {
		t.Fatalf("claimed = %+v", claimed)
	}
	reportIDs := []string{claimed[0].ReportID, claimed[1].ReportID}
	item := store.WatchlistItem{Ticker: "TESTDISC", Platform: "stock", Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, MinConfidence: .8, Source: "scanner"}
	err = store.PromoteScannerSourcesToWatchlist(ctx, item, store.ScannerSourcePromotionClaims{PoliticalDisclosure: &store.PoliticalDisclosurePromotionClaim{Ticker: "TESTDISC", Token: "stale-token", ReportIDs: reportIDs}}, "stale", now)
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
	err = store.PromoteScannerSourcesToWatchlist(ctx, item, store.ScannerSourcePromotionClaims{PoliticalDisclosure: &store.PoliticalDisclosurePromotionClaim{Ticker: "TESTDISC", Token: "claim-token", ReportIDs: reportIDs}}, "incomplete set", now)
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
	if err := store.PromoteScannerSourcesToWatchlist(ctx, item, store.ScannerSourcePromotionClaims{PoliticalDisclosure: &store.PoliticalDisclosurePromotionClaim{Ticker: "TESTDISC", Token: "claim-token", ReportIDs: reportIDs}}, "fixture success", now); err != nil {
		t.Fatal(err)
	}
	var promoted int
	if err := store.GetDB().QueryRow(ctx, "SELECT count(*) FROM political_disclosure_candidates WHERE ticker=$1 AND promotion_status='promoted'", "TESTDISC").Scan(&promoted); err != nil {
		t.Fatal(err)
	}
	if promoted != 2 {
		t.Fatalf("promoted rows = %d", promoted)
	}
	watchlistItem, err := store.GetWatchlistItem(ctx, "TESTDISC", "stock")
	if err != nil || watchlistItem == nil || len(watchlistItem.Strategies) != 1 ||
		watchlistItem.Strategies[0] != "momentum" {
		t.Fatalf("political source leaked into watchlist strategies: item=%+v err=%v", watchlistItem, err)
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

func TestPersistedTransientSenateFailureRetriesLaterWithoutStarvingOlderFiling(t *testing.T) {
	ctx := context.Background()
	const source = "senate"
	newestID, olderID := "retry-newest", "retry-older"
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports
			WHERE source=$1 AND external_id = ANY($2::text[])`, source, []string{newestID, olderID})
	})
	client, err := disclosures.NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	transport := &mutableSenateTransport{failNewest: true}
	client.Transport = transport
	runner := &disclosures.Runner{Senate: client}
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)

	first, err := runner.SyncRange(ctx, from, until, 1)
	if err == nil || first.SenateProcessed != 1 {
		t.Fatalf("first sync result=%+v err=%v", first, err)
	}
	newest, err := store.GetPoliticalDisclosureReport(ctx, source, newestID)
	if err != nil || newest == nil || newest.ExtractionStatus != "failed" ||
		!newest.RetryableFailure || newest.NextRetryAt == nil {
		t.Fatalf("transient failure was not durably queued: report=%+v err=%v", newest, err)
	}

	second, err := runner.SyncRange(ctx, from, until, 1)
	if err != nil || second.SenateProcessed != 1 {
		t.Fatalf("backoff sync result=%+v err=%v", second, err)
	}
	older, err := store.GetPoliticalDisclosureReport(ctx, source, olderID)
	if err != nil || older == nil || older.ExtractionStatus != "completed" {
		t.Fatalf("older filing was starved during retry backoff: report=%+v err=%v", older, err)
	}

	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_reports SET next_retry_at=NOW()-INTERVAL '1 second'
		WHERE source=$1 AND external_id=$2`, source, newestID); err != nil {
		t.Fatal(err)
	}
	transport.failNewest = false
	third, err := runner.SyncRange(ctx, from, until, 1)
	if err != nil || third.SenateProcessed != 1 {
		t.Fatalf("due retry result=%+v err=%v", third, err)
	}
	newest, err = store.GetPoliticalDisclosureReport(ctx, source, newestID)
	if err != nil || newest == nil || newest.ExtractionStatus != "completed" ||
		newest.RetryableFailure || newest.RetryAttemptCount != 0 || newest.NextRetryAt != nil {
		t.Fatalf("successful re-extraction did not clear retry state: report=%+v err=%v", newest, err)
	}
}

func TestFailedPoliticalDisclosureReportRetriesWithBoundedBackoffAndManualReset(t *testing.T) {
	ctx := context.Background()
	report := store.PoliticalDisclosureReport{
		Source: "oge", ExternalID: "test-terminal-failed-disclosure",
		Chamber: "executive", FilerName: "Test Official",
		ReportType: "periodic_transaction", FilingDate: time.Now().UTC(),
		DocumentURL:      "https://example.test/failed.pdf",
		ExtractionStatus: "failed", ExtractionError: "fixture extraction error", RetryableFailure: true,
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
		t.Fatalf("future-backoff report terminal=%v err=%v", terminal, err)
	}
	stored, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored == nil || !stored.RetryableFailure || stored.RetryAttemptCount != 1 || stored.NextRetryAt == nil {
		t.Fatalf("stored retry state = %+v err=%v", stored, err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_reports SET next_retry_at=NOW()-INTERVAL '1 second'
		WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID); err != nil {
		t.Fatal(err)
	}
	terminal, err = store.PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID)
	if err != nil || terminal {
		t.Fatalf("due retry terminal=%v err=%v", terminal, err)
	}
	// The next two persisted transient failures exhaust the bounded automatic
	// retry budget instead of consuming every future sync allocation.
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored == nil || stored.RetryableFailure || stored.RetryAttemptCount != 3 || stored.NextRetryAt != nil {
		t.Fatalf("exhausted retry state = %+v err=%v", stored, err)
	}
	terminal, err = store.PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID)
	if err != nil || !terminal {
		t.Fatalf("exhausted report terminal=%v err=%v", terminal, err)
	}
	found, err := store.RetryPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || !found {
		t.Fatalf("manual reset found=%v err=%v", found, err)
	}
	terminal, err = store.PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID)
	if err != nil || terminal {
		t.Fatalf("manual reset terminal=%v err=%v", terminal, err)
	}
	completed := report
	completed.ExtractionStatus = "completed"
	completed.ExtractionError = ""
	completed.RetryableFailure = false
	if err := store.SavePoliticalDisclosureReport(ctx, completed); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored == nil || stored.RetryAttemptCount != 0 || stored.RetryableFailure || stored.NextRetryAt != nil {
		t.Fatalf("successful re-extraction did not clear retry state: %+v err=%v", stored, err)
	}
	found, err = store.RetryPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || found {
		t.Fatalf("completed report was re-queued: found=%v err=%v", found, err)
	}
}

func TestNonCompletedDisclosureReExtractionClearsChildEvidence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2099, time.May, 5, 0, 0, 0, 0, time.UTC)
	for _, status := range []string{"failed", "no_text"} {
		t.Run(status, func(t *testing.T) {
			externalID := "test-disclosure-reextract-" + status
			report := store.PoliticalDisclosureReport{
				Source: "house", ExternalID: externalID, Chamber: "house",
				FilerName: "Hon. Re-extraction Test", Office: "TS02",
				ReportType: "periodic_transaction", FilingDate: now,
				DocumentURL: "https://example.test/" + externalID + ".pdf",
				ContentHash: "completed-fixture", ExtractionStatus: "completed",
				RawMetadata: json.RawMessage(`{"fixture":true}`),
				Transactions: []store.PoliticalDisclosureTransaction{{
					Ordinal: 1, AssetName: "Stale Evidence Corp (STALE)",
					Ticker: "STALE", TransactionType: "purchase",
					TransactionDate: now.AddDate(0, 0, -1), AmountText: "$1,001 - $15,000",
					RawData: json.RawMessage(`{"row":1}`),
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
			if _, err := store.GetDB().Exec(ctx, `
				UPDATE political_disclosure_candidates
				SET promotion_status='processing', promotion_claim_token='stale-claim',
					promotion_claimed_at=$3
				WHERE report_id=(
					SELECT id FROM political_disclosure_reports
					WHERE source=$1 AND external_id=$2
				)`, report.Source, report.ExternalID, now); err != nil {
				t.Fatal(err)
			}

			reextracted := report
			reextracted.ExtractionStatus = status
			reextracted.ExtractionError = "fixture " + status
			reextracted.ContentHash = ""
			reextracted.Transactions = nil
			if err := store.SavePoliticalDisclosureReport(ctx, reextracted); err != nil {
				t.Fatal(err)
			}

			detail, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
			if err != nil {
				t.Fatal(err)
			}
			if detail == nil || detail.ExtractionStatus != status || len(detail.Transactions) != 0 {
				t.Fatalf("detail = %+v", detail)
			}
			var candidateStatus, claimToken, evidence string
			var transactionCount int
			if err := store.GetDB().QueryRow(ctx, `
				SELECT promotion_status, COALESCE(promotion_claim_token,''),
					transaction_count, evidence::text
				FROM political_disclosure_candidates
				WHERE report_id=(
					SELECT id FROM political_disclosure_reports
					WHERE source=$1 AND external_id=$2
				) AND ticker='STALE'`, report.Source, report.ExternalID,
			).Scan(&candidateStatus, &claimToken, &transactionCount, &evidence); err != nil {
				t.Fatal(err)
			}
			if candidateStatus != "superseded" || claimToken != "" ||
				transactionCount != 0 || evidence != "[]" {
				t.Fatalf("candidate status=%q claim=%q count=%d evidence=%s",
					candidateStatus, claimToken, transactionCount, evidence)
			}
		})
	}
}
