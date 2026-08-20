package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
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
		IsAmendment: true,
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
	if detail == nil || !detail.IsAmendment || len(detail.Transactions) != 1 ||
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

func TestClaimDuePoliticalDisclosureRetriesLeasesWithoutIncrementingAttempts(t *testing.T) {
	ctx := context.Background()
	report := store.PoliticalDisclosureReport{
		Source: "senate", ExternalID: "test-due-disclosure-retry", Chamber: "senate",
		FilerName: "Test Senator", ReportType: "periodic_transaction",
		FilingDate: time.Now().UTC(), DocumentURL: "https://example.test/retry",
		ContentHash: "retry-content-hash", ExtractionStatus: "failed", ExtractionError: "temporary", RetryableFailure: true,
		RawMetadata: json.RawMessage(`{"fixture":true}`),
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), "DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored == nil || stored.RetryClaimToken != "" || stored.RetryClaimedAt != nil || stored.RetryLeaseUntil != nil {
		t.Fatalf("unclaimed retry=%+v err=%v", stored, err)
	}
	if _, err := store.GetDB().Exec(ctx, "UPDATE political_disclosure_reports SET next_retry_at=NOW()-INTERVAL '1 second' WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID); err != nil {
		t.Fatal(err)
	}
	if terminal, err := store.PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID); err != nil || terminal {
		t.Fatalf("due unleased retry terminal=%v err=%v", terminal, err)
	}
	now := time.Now().UTC()
	claimed, err := store.ClaimDuePoliticalDisclosureRetries(ctx, report.Source, now, 1, 2*time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ExternalID != report.ExternalID || claimed[0].RetryAttemptCount != 1 ||
		claimed[0].ContentHash != report.ContentHash || claimed[0].ExtractionError != report.ExtractionError || claimed[0].RetryClaimToken == "" {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	stored, err = store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored == nil || stored.RetryAttemptCount != 1 || stored.NextRetryAt == nil || stored.NextRetryAt.After(now) {
		t.Fatalf("leased retry=%+v err=%v", stored, err)
	}
	if found, err := store.RetryPoliticalDisclosureReport(ctx, report.Source, report.ExternalID); err != nil || !found {
		t.Fatalf("manual retry found=%v err=%v", found, err)
	}
	claimed, err = store.ClaimDuePoliticalDisclosureRetries(ctx, report.Source, time.Now().UTC(), 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ExternalID != report.ExternalID {
		t.Fatalf("manual due claim=%+v err=%v", claimed, err)
	}
}

func TestPoliticalDisclosureRetryClaimLeaseFencingLifecycle(t *testing.T) {
	ctx := context.Background()
	report := store.PoliticalDisclosureReport{Source: "senate", ExternalID: "retry-lease-fence", Chamber: "senate", FilerName: "Lease Senator", ReportType: "periodic_transaction", FilingDate: time.Now().UTC(), DocumentURL: "https://example.test/lease", ExtractionStatus: "failed", ExtractionError: "temporary", RetryableFailure: true, RawMetadata: json.RawMessage(`{"fixture":true}`)}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), "DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDB().Exec(ctx, "UPDATE political_disclosure_reports SET next_retry_at=NOW()-INTERVAL '1 second' WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	a, err := store.ClaimPoliticalDisclosureRetries(ctx, report.Source, now, 1, time.Minute, "token-a")
	if err != nil || len(a) != 1 {
		t.Fatalf("claim A=%+v err=%v", a, err)
	}
	if b, err := store.ClaimPoliticalDisclosureRetries(ctx, report.Source, now, 1, time.Minute, "token-b"); err != nil || len(b) != 0 {
		t.Fatalf("active lease claimed by B=%+v err=%v", b, err)
	}
	if ok, err := store.RenewPoliticalDisclosureRetryLease(ctx, report.Source, report.ExternalID, "token-a", now.Add(time.Second), 2*time.Minute); err != nil || !ok {
		t.Fatalf("renew=%v err=%v", ok, err)
	}
	if terminal, err := store.PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID); err != nil || !terminal {
		t.Fatalf("active retry lease terminal=%v err=%v", terminal, err)
	}
	// An expired lease is still owned by A until a reclaimer has actually
	// replaced its token, so A may safely commit the complete result.
	if _, err := store.GetDB().Exec(ctx, "UPDATE political_disclosure_reports SET retry_lease_until=NOW()-INTERVAL '1 second' WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.RenewPoliticalDisclosureRetryLease(ctx, report.Source, report.ExternalID, "token-a", time.Now().UTC(), time.Minute); err != nil || !ok {
		t.Fatalf("expired but unreclaimed token A renewal=%v err=%v", ok, err)
	}
	if _, err := store.GetDB().Exec(ctx, "UPDATE political_disclosure_reports SET retry_lease_until=NOW()-INTERVAL '1 second' WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID); err != nil {
		t.Fatal(err)
	}
	completed := report
	completed.ExtractionStatus = "completed"
	completed.RetryableFailure = false
	if err := store.SavePoliticalDisclosureRetryResult(ctx, completed, "token-a"); err != nil {
		t.Fatalf("expired unreclaimed token A completion: %v", err)
	}
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDB().Exec(ctx, "UPDATE political_disclosure_reports SET next_retry_at=NOW()-INTERVAL '1 second' WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID); err != nil {
		t.Fatal(err)
	}
	a, err = store.ClaimPoliticalDisclosureRetries(ctx, report.Source, time.Now().UTC(), 1, time.Minute, "token-a")
	if err != nil || len(a) != 1 {
		t.Fatalf("second claim A=%+v err=%v", a, err)
	}
	if _, err := store.GetDB().Exec(ctx, "UPDATE political_disclosure_reports SET retry_lease_until=NOW()-INTERVAL '1 second' WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID); err != nil {
		t.Fatal(err)
	}
	b, err := store.ClaimPoliticalDisclosureRetries(ctx, report.Source, time.Now().UTC(), 1, time.Minute, "token-b")
	if err != nil || len(b) != 1 {
		t.Fatalf("reclaim B=%+v err=%v", b, err)
	}
	if ok, err := store.RenewPoliticalDisclosureRetryLease(ctx, report.Source, report.ExternalID, "token-a", time.Now().UTC(), time.Minute); err != nil || ok {
		t.Fatalf("reclaimed token A renewal=%v err=%v, want lease lost", ok, err)
	}
	completed.Transactions = []store.PoliticalDisclosureTransaction{{
		Ordinal: 1, AssetName: "Microsoft", Ticker: "MSFT",
		TransactionType: "purchase", TransactionDate: report.FilingDate,
	}}
	if err := store.SavePoliticalDisclosureRetryResult(ctx, completed, "token-b"); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored.RetryClaimToken != "" || stored.NextRetryAt != nil || stored.RetryAttemptCount != 0 || stored.ExtractionStatus != "completed" || len(stored.Transactions) != 1 {
		t.Fatalf("B success=%+v err=%v", stored, err)
	}
	type candidateState struct {
		status           string
		transactionCount int
		latest           time.Time
		evidence         []byte
	}
	loadCandidate := func() (candidateState, error) {
		var state candidateState
		err := store.GetDB().QueryRow(ctx, `
			SELECT promotion_status, transaction_count, latest_transaction_date, evidence
			FROM political_disclosure_candidates
			WHERE report_id=(
				SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2
			) AND ticker='MSFT'`, report.Source, report.ExternalID).Scan(
			&state.status, &state.transactionCount, &state.latest, &state.evidence,
		)
		return state, err
	}
	beforeCandidate, err := loadCandidate()
	if err != nil || beforeCandidate.status != "pending" || beforeCandidate.transactionCount != 1 || beforeCandidate.latest.IsZero() || len(beforeCandidate.evidence) == 0 {
		t.Fatalf("B candidate=%+v err=%v", beforeCandidate, err)
	}
	if err := store.SavePoliticalDisclosureRetryResult(ctx, report, "token-a"); !errors.Is(err, store.ErrPoliticalDisclosureRetryLeaseLost) {
		t.Fatalf("late token A failure error=%v, want lease lost", err)
	}
	stored, err = store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored.RetryClaimToken != "" || stored.NextRetryAt != nil || stored.RetryAttemptCount != 0 || stored.ExtractionStatus != "completed" || len(stored.Transactions) != 1 {
		t.Fatalf("late A changed B completion=%+v err=%v", stored, err)
	}
	afterCandidate, err := loadCandidate()
	if err != nil || afterCandidate.status != beforeCandidate.status || afterCandidate.transactionCount != beforeCandidate.transactionCount || !afterCandidate.latest.Equal(beforeCandidate.latest) || string(afterCandidate.evidence) != string(beforeCandidate.evidence) {
		t.Fatalf("late A changed B candidate: before=%+v after=%+v err=%v", beforeCandidate, afterCandidate, err)
	}
	completed.ExtractionStatus = "failed"
	completed.RetryableFailure = true
	completed.ExtractionError = "again"
	if err := store.SavePoliticalDisclosureReport(ctx, completed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDB().Exec(ctx, "UPDATE political_disclosure_reports SET next_retry_at=NOW()-INTERVAL '1 second' WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID); err != nil {
		t.Fatal(err)
	}
	b, err = store.ClaimPoliticalDisclosureRetries(ctx, report.Source, time.Now().UTC(), 1, time.Minute, "token-b")
	if err != nil || len(b) != 1 {
		t.Fatalf("failure claim=%+v err=%v", b, err)
	}
	if err := store.SavePoliticalDisclosureRetryResult(ctx, completed, "token-b"); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored.RetryClaimToken != "" || stored.RetryAttemptCount != 2 || stored.NextRetryAt == nil {
		t.Fatalf("B failure=%+v err=%v", stored, err)
	}
	before := *stored.NextRetryAt
	attempts := stored.RetryAttemptCount
	if err := store.ReleasePoliticalDisclosureRetryClaims(ctx, "token-b"); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored.RetryAttemptCount != attempts || stored.NextRetryAt == nil || !stored.NextRetryAt.Equal(before) {
		t.Fatalf("release altered schedule=%+v err=%v", stored, err)
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
	runner := &disclosures.Runner{Cfg: config.DisclosuresConfig{LookbackDays: 1, MaxReportsPerSync: 1}, Senate: client, Now: time.Now}
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
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("due retry run: %v", err)
	}
	newest, err = store.GetPoliticalDisclosureReport(ctx, source, newestID)
	if err != nil || newest == nil || newest.ExtractionStatus != "completed" ||
		newest.RetryableFailure || newest.RetryAttemptCount != 0 || newest.NextRetryAt != nil ||
		len(newest.Transactions) != 1 || newest.Transactions[0].Ticker != "MSFT" {
		t.Fatalf("successful re-extraction did not clear retry state: report=%+v err=%v", newest, err)
	}
	var candidateCount int
	var evidence []byte
	if err := store.GetDB().QueryRow(ctx, `
		SELECT transaction_count, evidence
		FROM political_disclosure_candidates
		WHERE report_id=(
			SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2
		) AND ticker='MSFT'`, source, newestID,
	).Scan(&candidateCount, &evidence); err != nil || candidateCount != 1 || len(evidence) == 0 {
		t.Fatalf("recovered candidate count=%d evidence=%s err=%v", candidateCount, evidence, err)
	}
}

func TestClaimPoliticalDisclosureCandidatesExcludesFutureTickerFromSelection(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.January, 15, 12, 0, 0, 0, time.UTC)
	current := politicalDisclosureClaimFixture("test-claim-window-current", "PDWINDOWCUR", now)
	future := politicalDisclosureClaimFixture("test-claim-window-future", "PDWINDOWFUT", now.AddDate(0, 0, 1))
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports
			WHERE source='house' AND external_id = ANY($1::text[])`,
			[]string{current.ExternalID, future.ExternalID})
	})
	for _, report := range []store.PoliticalDisclosureReport{current, future} {
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
	}

	claimed, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, "window-selected-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Ticker != "PDWINDOWCUR" || claimed[0].ExternalID != current.ExternalID {
		t.Fatalf("claimed = %+v, want only current in-range ticker", claimed)
	}
}

func TestClaimPoliticalDisclosureCandidatesExcludesFutureReportsFromClaimSet(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.February, 15, 12, 0, 0, 0, time.UTC)
	current := politicalDisclosureClaimFixture("test-claim-window-claim-current", "PDWINDOWSAME", now)
	future := politicalDisclosureClaimFixture("test-claim-window-claim-future", "PDWINDOWSAME", now.AddDate(0, 0, 1))
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports
			WHERE source='house' AND external_id = ANY($1::text[])`,
			[]string{current.ExternalID, future.ExternalID})
	})
	for _, report := range []store.PoliticalDisclosureReport{current, future} {
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
	}

	claimed, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, "window-claimed-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Ticker != "PDWINDOWSAME" || claimed[0].ExternalID != current.ExternalID {
		t.Fatalf("claimed = %+v, want only current in-range report", claimed)
	}
}

func TestMarkPoliticalDisclosurePromotionRequiresAllClaimedReportIDs(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.April, 10, 12, 0, 0, 0, time.UTC)
	first := politicalDisclosureClaimFixture("test-disclosure-partial-claim-1", "PDPARTIAL", now.AddDate(0, 0, -1))
	second := politicalDisclosureClaimFixture("test-disclosure-partial-claim-2", "PDPARTIAL", now)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports
			WHERE source='house' AND external_id = ANY($1::text[])`,
			[]string{first.ExternalID, second.ExternalID})
	})
	for _, report := range []store.PoliticalDisclosureReport{first, second} {
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := store.ClaimPoliticalDisclosureCandidates(ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, "partial-token")
	if err != nil {
		t.Fatal(err)
	}
	var reportIDs []string
	for _, item := range claimed {
		if item.Claimed {
			reportIDs = append(reportIDs, item.ReportID)
		}
	}
	if len(reportIDs) != 2 {
		t.Fatalf("claim report IDs = %v", reportIDs)
	}
	// Invalidate one claimed row, simulating re-extraction during scoring.
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_candidates
		SET promotion_status='pending', promotion_claim_token=NULL, promotion_claimed_at=NULL
		WHERE report_id=$1 AND ticker=$2`, reportIDs[0], "PDPARTIAL"); err != nil {
		t.Fatal(err)
	}
	var claimErr *store.ScannerSourcePromotionClaimError
	err = store.MarkPoliticalDisclosurePromotion(ctx, "PDPARTIAL", "partial-token", reportIDs, "rejected", "", "fixture", now)
	if !errors.As(err, &claimErr) || claimErr.Source != "political_disclosure" {
		t.Fatalf("partial claim update error=%v, want political disclosure claim loss", err)
	}
	var status string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT promotion_status FROM political_disclosure_candidates
		WHERE report_id=$1 AND ticker=$2`, reportIDs[0], "PDPARTIAL").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("invalidated report status=%q, want pending", status)
	}
	if err := store.GetDB().QueryRow(ctx, `
		SELECT promotion_status FROM political_disclosure_candidates
		WHERE report_id=$1 AND ticker=$2`, reportIDs[1], "PDPARTIAL").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "processing" {
		t.Fatalf("other report status=%q, want still processing after partial claim rejection", status)
	}
}

func TestMarkPoliticalDisclosurePromotionRequeuesOnlyCapSuppressedTicker(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.April, 11, 12, 0, 0, 0, time.UTC)
	suppressed := politicalDisclosureClaimFixture("test-disclosure-cap-suppressed", "PDCAPONE", now)
	sibling := politicalDisclosureClaimFixture("test-disclosure-cap-sibling", "PDCAPTWO", now.AddDate(0, 0, -1))
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports
			WHERE source='house' AND external_id = ANY($1::text[])`,
			[]string{suppressed.ExternalID, sibling.ExternalID})
	})
	for _, report := range []store.PoliticalDisclosureReport{suppressed, sibling} {
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, now.AddDate(0, 0, -7), now, time.Hour, 2, "cap-batch-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	byTicker := make(map[string]store.PoliticalDisclosureCandidate, len(claimed))
	for _, item := range claimed {
		if item.Claimed {
			byTicker[item.Ticker] = item
		}
	}
	suppressedClaim, foundSuppressed := byTicker["PDCAPONE"]
	siblingClaim, foundSibling := byTicker["PDCAPTWO"]
	if !foundSuppressed || !foundSibling {
		t.Fatalf("claimed=%+v, want both tickers", claimed)
	}
	if err := store.MarkPoliticalDisclosurePromotion(
		ctx, suppressedClaim.Ticker, "cap-batch-token", []string{suppressedClaim.ReportID},
		"pending", "", "candidate ranked beyond available scanner slots", now,
	); err != nil {
		t.Fatal(err)
	}
	var siblingStatus, siblingToken string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT promotion_status, COALESCE(promotion_claim_token,'')
		FROM political_disclosure_candidates WHERE report_id=$1`, siblingClaim.ReportID,
	).Scan(&siblingStatus, &siblingToken); err != nil {
		t.Fatal(err)
	}
	if siblingStatus != "processing" || siblingToken != "cap-batch-token" {
		t.Fatalf("sibling claim was disturbed: status=%q token=%q", siblingStatus, siblingToken)
	}
	reclaimed, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, now.AddDate(0, 0, -7), now.Add(time.Minute), time.Hour, 2, "cap-reclaim-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Ticker != "PDCAPONE" ||
		!reclaimed[0].Claimed || reclaimed[0].PromotionClaimToken != "cap-reclaim-token" {
		t.Fatalf("reclaimed=%+v, want only cap-suppressed ticker", reclaimed)
	}
}

func TestClaimPoliticalDisclosureCandidatesReturnsHistoryButClaimsOnlyPendingRows(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.March, 15, 12, 0, 0, 0, time.UTC)
	history := politicalDisclosureClaimFixture("test-claim-history-terminal", "PDHISTORY", now.AddDate(0, 0, -1))
	pending := politicalDisclosureClaimFixture("test-claim-history-pending", "PDHISTORY", now)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports
			WHERE source='house' AND external_id = ANY($1::text[])`,
			[]string{history.ExternalID, pending.ExternalID})
	})
	for _, report := range []store.PoliticalDisclosureReport{history, pending} {
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_candidates
		SET promotion_status='promoted', promotion_claim_token=NULL, promotion_claimed_at=NULL
		WHERE report_id=(SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2)`, history.Source, history.ExternalID); err != nil {
		t.Fatal(err)
	}

	claimed, err := store.ClaimPoliticalDisclosureCandidates(ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, "history-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 {
		t.Fatalf("rows=%+v, want history and pending rows", claimed)
	}
	byExternalID := make(map[string]store.PoliticalDisclosureCandidate, len(claimed))
	for _, item := range claimed {
		byExternalID[item.ExternalID] = item
	}
	claimedHistory := byExternalID[history.ExternalID]
	if claimedHistory.Claimed || claimedHistory.PromotionClaimToken != "" {
		t.Fatalf("terminal history row was claimed: %+v", claimedHistory)
	}
	claimedPending := byExternalID[pending.ExternalID]
	if !claimedPending.Claimed || claimedPending.PromotionClaimToken != "history-token" {
		t.Fatalf("pending row claim=%+v", claimedPending)
	}
	if err := store.MarkPoliticalDisclosurePromotion(ctx, "PDHISTORY", "history-token", []string{claimedPending.ReportID}, "rejected", "", "fixture", now); err != nil {
		t.Fatal(err)
	}
	var historyStatus, pendingStatus string
	if err := store.GetDB().QueryRow(ctx, `SELECT promotion_status FROM political_disclosure_candidates WHERE report_id=$1 AND ticker=$2`, claimedHistory.ReportID, "PDHISTORY").Scan(&historyStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.GetDB().QueryRow(ctx, `SELECT promotion_status FROM political_disclosure_candidates WHERE report_id=$1 AND ticker=$2`, claimedPending.ReportID, "PDHISTORY").Scan(&pendingStatus); err != nil {
		t.Fatal(err)
	}
	if historyStatus != "promoted" || pendingStatus != "rejected" {
		t.Fatalf("history=%q pending=%q, want promoted/rejected", historyStatus, pendingStatus)
	}
	if rows, err := store.ClaimPoliticalDisclosureCandidates(ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, "no-claim-token"); err != nil || len(rows) != 0 {
		t.Fatalf("no claimable row returned history: rows=%+v err=%v", rows, err)
	}
}

func politicalDisclosureClaimFixture(externalID, ticker string, filingDate time.Time) store.PoliticalDisclosureReport {
	minimum, maximum := int64(1_001), int64(15_000)
	return store.PoliticalDisclosureReport{
		Source: "house", ExternalID: externalID, Chamber: "house",
		FilerName: "Hon. Claim Window Test", Office: "TS03",
		ReportType: "periodic_transaction", FilingDate: filingDate,
		DocumentURL: "https://example.test/" + externalID + ".pdf",
		ContentHash: "fixture-" + externalID, ExtractionStatus: "completed",
		RawMetadata: json.RawMessage(`{"fixture":true}`),
		Transactions: []store.PoliticalDisclosureTransaction{{
			Ordinal: 1, AssetName: "Claim Window Corp (" + ticker + ")", Ticker: ticker,
			AssetType: "ST", TransactionType: "purchase", TransactionDate: filingDate,
			AmountMin: &minimum, AmountMax: &maximum, AmountText: "$1,001 - $15,000",
			RawData: json.RawMessage(`{"row":1}`),
		}},
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

func TestFirstPassDisclosureFailureDoesNotDowngradeCompletedReport(t *testing.T) {
	ctx := context.Background()
	report := store.PoliticalDisclosureReport{
		Source: "house", ExternalID: "test-disclosure-downgrade-fence", Chamber: "house",
		FilerName: "Hon. Completed Evidence", Office: "TS04",
		ReportType: "periodic_transaction", FilingDate: time.Now().UTC(),
		DocumentURL: "https://example.test/completed.pdf", ContentHash: "completed-hash",
		ExtractionStatus: "completed", RawMetadata: json.RawMessage(`{"fixture":true}`),
		Transactions: []store.PoliticalDisclosureTransaction{{
			Ordinal: 1, AssetName: "Completed Corp (CMF)",
			Ticker: "CMF", TransactionType: "purchase",
			TransactionDate: time.Now().UTC().AddDate(0, 0, -1),
			RawData:         json.RawMessage(`{"row":1}`),
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
	late := report
	late.ExtractionStatus = "failed"
	late.ExtractionError = "temporary loser"
	late.RetryableFailure = true
	late.ContentHash = ""
	late.Transactions = nil
	if err := store.SavePoliticalDisclosureFirstPass(ctx, late); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored == nil || stored.ExtractionStatus != "completed" ||
		stored.ContentHash != "completed-hash" || stored.RetryableFailure || stored.RetryAttemptCount != 0 || len(stored.Transactions) != 1 {
		t.Fatalf("late failure downgraded completed report: %+v err=%v", stored, err)
	}
	if stored.Transactions[0].Ticker != "CMF" {
		t.Fatalf("completed transaction evidence lost: %+v", stored.Transactions)
	}
}

func TestReplacingDisclosureEvidenceInvalidatesActiveCandidateClaim(t *testing.T) {
	ctx := context.Background()
	report := store.PoliticalDisclosureReport{
		Source: "house", ExternalID: "test-disclosure-evidence-replacement", Chamber: "house",
		FilerName: "Hon. Claim Evidence", Office: "TS05",
		ReportType: "periodic_transaction", FilingDate: time.Now().UTC(),
		DocumentURL: "https://example.test/evidence.pdf", ContentHash: "hash-1",
		ExtractionStatus: "completed", RawMetadata: json.RawMessage(`{"fixture":true}`),
		Transactions: []store.PoliticalDisclosureTransaction{{
			Ordinal: 1, AssetName: "Original Corp (ORIG)",
			Ticker: "ORIG", TransactionType: "purchase",
			TransactionDate: time.Now().UTC().AddDate(0, 0, -1),
			RawData:         json.RawMessage(`{"row":1}`),
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
	_, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, time.Now().UTC().AddDate(0, 0, -7), time.Now().UTC(), time.Hour, 1, "claim-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	replaced := report
	replaced.ContentHash = "hash-2"
	replaced.Transactions = []store.PoliticalDisclosureTransaction{{
		Ordinal: 2, AssetName: "Replacement Corp (RPLC)",
		Ticker: "RPLC", TransactionType: "sale",
		TransactionDate: time.Now().UTC(), RawData: json.RawMessage(`{"row":2}`),
	}}
	if err := store.SavePoliticalDisclosureReport(ctx, replaced); err != nil {
		t.Fatal(err)
	}
	var oldStatus, oldToken string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT promotion_status, COALESCE(promotion_claim_token,'')
		FROM political_disclosure_candidates
		WHERE report_id=(
			SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2
		) AND ticker='ORIG'`, report.Source, report.ExternalID).Scan(&oldStatus, &oldToken); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "superseded" || oldToken != "" {
		t.Fatalf("removed ticker retained an active claim: status=%q token=%q", oldStatus, oldToken)
	}
	var newStatus, newToken string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT promotion_status, COALESCE(promotion_claim_token,'')
		FROM political_disclosure_candidates
		WHERE report_id=(
			SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2
		) AND ticker='RPLC'`, report.Source, report.ExternalID).Scan(&newStatus, &newToken); err != nil {
		t.Fatal(err)
	}
	if newStatus != "pending" || newToken != "" {
		t.Fatalf("replacement ticker was not resettable: status=%q token=%q", newStatus, newToken)
	}

	// Reclaim the replacement ticker, then replace its evidence with a new row.
	_, err = store.ClaimPoliticalDisclosureCandidates(
		ctx, time.Now().UTC().AddDate(0, 0, -7), time.Now().UTC(), time.Hour, 1, "second-claim-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	sameTickerReplacement := replaced
	sameTickerReplacement.ContentHash = "hash-3"
	sameTickerReplacement.Transactions = []store.PoliticalDisclosureTransaction{{
		Ordinal: 3, AssetName: "Replacement Corp (RPLC)",
		Ticker: "RPLC", TransactionType: "exchange",
		TransactionDate: time.Now().UTC(), RawData: json.RawMessage(`{"row":3}`),
	}}
	if err := store.SavePoliticalDisclosureReport(ctx, sameTickerReplacement); err != nil {
		t.Fatal(err)
	}
	var activeStatus, activeToken string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT promotion_status, COALESCE(promotion_claim_token,'')
		FROM political_disclosure_candidates
		WHERE report_id=(
			SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2
		) AND ticker='RPLC'`, report.Source, report.ExternalID).Scan(&activeStatus, &activeToken); err != nil {
		t.Fatal(err)
	}
	if activeStatus != "pending" || activeToken != "" {
		t.Fatalf("same-ticker evidence replacement preserved active claim: status=%q token=%q", activeStatus, activeToken)
	}
}
