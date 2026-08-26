package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/disclosures"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type mutableSenateTransport struct {
	failNewest bool
}

func rawJSONEqual(left, right json.RawMessage) bool {
	var decodedLeft, decodedRight any
	return json.Unmarshal(left, &decodedLeft) == nil &&
		json.Unmarshal(right, &decodedRight) == nil &&
		reflect.DeepEqual(decodedLeft, decodedRight)
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

func TestPoliticalDisclosureTerminalDispositionLinksScannerRun(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	const ticker = "RUNLINKPD"
	report := store.PoliticalDisclosureReport{
		Source: "house", ExternalID: "run-link-political-disclosure", Chamber: "house",
		FilerName: "Run Link Member", Office: "RL01", ReportType: "periodic_transaction",
		FilingDate: now, DocumentURL: "https://example.test/run-link", ContentHash: "run-link",
		ExtractionStatus: "completed", Transactions: []store.PoliticalDisclosureTransaction{{
			Ordinal: 1, AssetName: "Run Link (RUNLINKPD) [ST]", Ticker: ticker, AssetType: "ST",
			TransactionType: "purchase", TransactionDate: now.AddDate(0, 0, -1),
		}},
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(ctx, "DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPoliticalDisclosureCandidates(ctx, now.AddDate(0, 0, -7), now.Add(time.Minute), time.Hour, 100, "run-link-token")
	if err != nil {
		t.Fatal(err)
	}
	var claimedFixture *store.PoliticalDisclosureCandidate
	for i := range claimed {
		if claimed[i].Ticker == ticker {
			claimedFixture = &claimed[i]
			break
		}
	}
	if claimedFixture == nil {
		t.Fatalf("did not claim political fixture from %+v", claimed)
	}
	runID, err := store.BeginScannerRun(ctx, store.ScannerRun{StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(ctx, "UPDATE political_disclosure_candidates SET scanner_run_id=NULL WHERE scanner_run_id=$1::uuid", runID)
		_, _ = store.GetDB().Exec(ctx, "DELETE FROM scanner_runs WHERE id=$1::uuid", runID)
	})
	if err := store.MarkPoliticalDisclosurePromotion(ctx, ticker, "run-link-token", []string{claimedFixture.ReportID}, "rejected", runID, "deterministic test rejection", now); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScannerRun(ctx, store.ScannerRun{ID: runID, CompletedAt: now, Status: "completed"}, []store.ScannerCandidateSnapshot{{
		ScannerRunID: runID, Ticker: ticker, Sources: []string{"political_disclosure"}, FinalStage: "deterministically_rejected",
	}}); err != nil {
		t.Fatal(err)
	}
	var sourceRunID, snapshotRunID string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT COALESCE(p.scanner_run_id::text,''),COALESCE(s.scanner_run_id::text,'')
		FROM political_disclosure_candidates p
		JOIN scanner_candidate_snapshots s ON s.ticker=p.ticker
		WHERE p.report_id=$1 AND p.ticker=$2 AND s.scanner_run_id=$3::uuid`, claimedFixture.ReportID, ticker, runID).Scan(&sourceRunID, &snapshotRunID); err != nil {
		t.Fatal(err)
	}
	if sourceRunID != runID || snapshotRunID != runID {
		t.Fatalf("terminal linkage source=%q snapshot=%q, want %q", sourceRunID, snapshotRunID, runID)
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

func TestClaimPoliticalDisclosureCandidatesReturnsCurrentClaimedEvidenceAfterConcurrentRefresh(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.March, 1, 12, 0, 0, 0, time.UTC)
	report := politicalDisclosureClaimFixture("test-claim-current-evidence", "PDCURRENT", now)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}

	lockTx, err := store.GetDB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(ctx) //nolint:errcheck
	var backendPID int
	if err := lockTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		t.Fatal(err)
	}
	var reportID string
	if err := lockTx.QueryRow(ctx, `
		SELECT c.report_id::text
		FROM political_disclosure_candidates c
		JOIN political_disclosure_reports r ON r.id=c.report_id
		WHERE r.source=$1 AND r.external_id=$2 AND c.ticker=$3
		FOR UPDATE OF c`, report.Source, report.ExternalID, "PDCURRENT").Scan(&reportID); err != nil {
		t.Fatal(err)
	}

	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	claimedResult := make(chan []store.PoliticalDisclosureCandidate, 1)
	claimErr := make(chan error, 1)
	go func() {
		claimed, err := store.ClaimPoliticalDisclosureCandidates(
			claimCtx, now.AddDate(0, 0, -7), now, time.Hour, 1, "current-evidence-token",
		)
		if err != nil {
			claimErr <- err
			return
		}
		claimedResult <- claimed
	}()

	// Wait until ClaimPoliticalDisclosureCandidates has materialized its eligible
	// history and is blocked attempting to claim this candidate row. Updating the
	// row in the lock holder then reproduces a report re-extraction between the
	// history read and claim update without relying on timing.
	blocked := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if err := store.GetDB().QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity a
				WHERE $1 = ANY(pg_blocking_pids(a.pid))
				  AND a.query LIKE '%political_disclosure_candidates%'
			)`, backendPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
	}
	if !blocked {
		t.Fatal("candidate claim did not block on the locked row")
	}
	freshDate := now.AddDate(0, 0, -2)
	if _, err := lockTx.Exec(ctx, `
		UPDATE political_disclosure_candidates
		SET transaction_count=2, latest_transaction_date=$3,
			evidence='[{"fresh":true}]'::jsonb
		WHERE report_id=$1::uuid AND ticker=$2`, reportID, "PDCURRENT", freshDate); err != nil {
		t.Fatal(err)
	}
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-claimErr:
		t.Fatal(err)
	case claimed := <-claimedResult:
		if len(claimed) != 1 || !claimed[0].Claimed ||
			claimed[0].PromotionClaimToken != "current-evidence-token" ||
			claimed[0].TransactionCount != 2 ||
			claimed[0].LatestTransaction.Format(time.DateOnly) != freshDate.Format(time.DateOnly) {
			t.Fatalf("claimed=%+v, want refreshed claimed evidence", claimed)
		}
		var evidence []map[string]bool
		if err := json.Unmarshal(claimed[0].Evidence, &evidence); err != nil || len(evidence) != 1 || !evidence[0]["fresh"] {
			t.Fatalf("claimed evidence=%s err=%v, want refreshed evidence", claimed[0].Evidence, err)
		}
	case <-time.After(time.Second):
		t.Fatal("candidate claim did not finish after refresh committed")
	}
}

func TestPoliticalDisclosureParentLockPreventsReextractedClaimFromCreatingWatchlist(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.March, 2, 12, 0, 0, 0, time.UTC)
	report := politicalDisclosureClaimFixture("test-parent-lock-refresh", "PDPARENTLOCK", now)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM watchlist WHERE ticker=$1`, "PDPARENTLOCK")
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, "parent-lock-token",
	)
	if err != nil || len(claimed) != 1 || !claimed[0].Claimed {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}

	lockTx, err := store.GetDB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(ctx) //nolint:errcheck
	var backendPID int
	if err := lockTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `
		SELECT 1 FROM political_disclosure_reports WHERE id=$1::uuid FOR UPDATE`, claimed[0].ReportID); err != nil {
		t.Fatal(err)
	}
	item := store.WatchlistItem{Ticker: "PDPARENTLOCK", Platform: "stock", Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, MinConfidence: .8, Source: "scanner"}
	promoteDone := make(chan error, 1)
	go func() {
		promoteDone <- store.PromoteScannerSourcesToWatchlist(ctx, item,
			store.ScannerSourcePromotionClaims{PoliticalDisclosure: &store.PoliticalDisclosurePromotionClaim{
				Ticker: "PDPARENTLOCK", Token: "parent-lock-token", ReportIDs: []string{claimed[0].ReportID},
			}}, "fixture", now)
	}()

	blocked := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if err := store.GetDB().QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity a
				WHERE $1 = ANY(pg_blocking_pids(a.pid))
				  AND a.wait_event_type='Lock'
				  AND a.query LIKE '%political_disclosure_reports%'
			)`, backendPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
	}
	if !blocked {
		select {
		case err := <-promoteDone:
			t.Fatalf("promotion escaped parent lock before re-extraction: %v", err)
		default:
		}
		t.Fatal("promotion did not block on the claimed parent report")
	}
	if _, err := lockTx.Exec(ctx, `
		UPDATE political_disclosure_reports
		SET extraction_status='failed', extraction_error='re-extracted during promotion'
		WHERE id=$1::uuid`, claimed[0].ReportID); err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `
		UPDATE political_disclosure_candidates
		SET promotion_status='superseded', promotion_claim_token=NULL, promotion_claimed_at=NULL
		WHERE report_id=$1::uuid AND ticker=$2`, claimed[0].ReportID, "PDPARENTLOCK"); err != nil {
		t.Fatal(err)
	}
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var claimErr *store.ScannerSourcePromotionClaimError
	select {
	case err := <-promoteDone:
		if !errors.As(err, &claimErr) || claimErr.Source != "political_disclosure" {
			t.Fatalf("promotion error=%v, want stale political disclosure claim", err)
		}
	case <-time.After(time.Second):
		t.Fatal("promotion did not resume after parent lock released")
	}
	watchlistItem, err := store.GetWatchlistItem(ctx, "PDPARENTLOCK", "stock")
	if err != nil || watchlistItem != nil {
		t.Fatalf("stale disclosure created watchlist item=%+v err=%v", watchlistItem, err)
	}
}

func TestPoliticalDisclosurePromotionRejectsNonCompletedClaimedParent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.March, 3, 12, 0, 0, 0, time.UTC)
	report := politicalDisclosureClaimFixture("test-parent-not-completed", "PDPARENTSTATUS", now)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM watchlist WHERE ticker=$1`, "PDPARENTSTATUS")
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, "parent-status-token",
	)
	if err != nil || len(claimed) != 1 || !claimed[0].Claimed {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_reports SET extraction_status='failed'
		WHERE id=$1::uuid`, claimed[0].ReportID); err != nil {
		t.Fatal(err)
	}
	item := store.WatchlistItem{Ticker: "PDPARENTSTATUS", Platform: "stock", Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, MinConfidence: .8, Source: "scanner"}
	err = store.PromoteScannerSourcesToWatchlist(ctx, item,
		store.ScannerSourcePromotionClaims{PoliticalDisclosure: &store.PoliticalDisclosurePromotionClaim{
			Ticker: "PDPARENTSTATUS", Token: "parent-status-token", ReportIDs: []string{claimed[0].ReportID},
		}}, "fixture", now)
	var claimErr *store.ScannerSourcePromotionClaimError
	if !errors.As(err, &claimErr) || claimErr.Source != "political_disclosure" {
		t.Fatalf("promotion error=%v, want stale political disclosure claim", err)
	}
	watchlistItem, err := store.GetWatchlistItem(ctx, "PDPARENTSTATUS", "stock")
	if err != nil || watchlistItem != nil {
		t.Fatalf("non-completed parent created watchlist item=%+v err=%v", watchlistItem, err)
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

func TestMarkPoliticalDisclosurePromotionRequeuesOnlyCurrentTicker(t *testing.T) {
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
		"pending", "", "candidate watchlist persistence failed", now,
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

func TestClaimPoliticalDisclosureCandidatesFencesEntireAggregateLineage(t *testing.T) {
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
	claimedPending := byExternalID[pending.ExternalID]
	for _, item := range []store.PoliticalDisclosureCandidate{claimedHistory, claimedPending} {
		if !item.Claimed || item.PromotionClaimToken != "history-token" {
			t.Fatalf("aggregate evidence was not fenced: %+v", item)
		}
	}
	if err := store.MarkPoliticalDisclosurePromotion(ctx, "PDHISTORY", "history-token", []string{claimedHistory.ReportID, claimedPending.ReportID}, "rejected", "", "fixture", now); err != nil {
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

func TestClaimPoliticalDisclosureCandidatesSeparatesTriggerFromEvidenceHorizon(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.March, 15, 12, 0, 0, 0, time.UTC)
	history := politicalDisclosureClaimFixture("test-trigger-horizon-history", "PDHORIZON", now.AddDate(0, 0, -21))
	trigger := politicalDisclosureClaimFixture("test-trigger-horizon-current", "PDHORIZON", now.AddDate(0, 0, -1))
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports
			WHERE source='house' AND external_id = ANY($1::text[])`,
			[]string{history.ExternalID, trigger.ExternalID})
	})
	for _, report := range []store.PoliticalDisclosureReport{history, trigger} {
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.ClaimPoliticalDisclosureCandidatesWithEvidence(
		ctx, now.AddDate(0, 0, -7), now.AddDate(0, 0, -30), now, time.Hour, 1, "horizon-token",
	)
	if err != nil || len(rows) != 2 {
		t.Fatalf("claim rows=%+v err=%v", rows, err)
	}
	byExternalID := make(map[string]store.PoliticalDisclosureCandidate, len(rows))
	for _, row := range rows {
		byExternalID[row.ExternalID] = row
	}
	if !byExternalID[history.ExternalID].Claimed || byExternalID[history.ExternalID].PromotionClaimToken != "horizon-token" {
		t.Fatalf("historical context was not aggregate-fenced: %+v", byExternalID[history.ExternalID])
	}
	if !byExternalID[trigger.ExternalID].Claimed || byExternalID[trigger.ExternalID].PromotionClaimToken != "horizon-token" {
		t.Fatalf("fresh trigger was not claimed: %+v", byExternalID[trigger.ExternalID])
	}
	if byExternalID[history.ExternalID].TriggerEligible || !byExternalID[trigger.ExternalID].TriggerEligible {
		t.Fatalf("trigger eligibility = history:%t trigger:%t, want false/true",
			byExternalID[history.ExternalID].TriggerEligible, byExternalID[trigger.ExternalID].TriggerEligible)
	}
}

func TestClaimPoliticalDisclosureCandidatesMarksOnlyFreshClaimableTriggerRows(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.March, 17, 12, 0, 0, 0, time.UTC)
	terminalPurchase := politicalDisclosureClaimFixture("test-trigger-terminal-purchase", "PDTERMINAL", now.AddDate(0, 0, -2))
	pendingSale := politicalDisclosureClaimFixture("test-trigger-pending-sale", "PDTERMINAL", now.AddDate(0, 0, -1))
	pendingSale.Transactions[0].TransactionType = "sale"
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports
			WHERE source='house' AND external_id = ANY($1::text[])`,
			[]string{terminalPurchase.ExternalID, pendingSale.ExternalID})
	})
	for _, report := range []store.PoliticalDisclosureReport{terminalPurchase, pendingSale} {
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_candidates SET promotion_status='promoted'
		WHERE report_id=(SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2)`,
		terminalPurchase.Source, terminalPurchase.ExternalID); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ClaimPoliticalDisclosureCandidatesWithEvidence(
		ctx, now.AddDate(0, 0, -7), now.AddDate(0, 0, -30), now, time.Hour, 1, "terminal-trigger-token",
	)
	if err != nil || len(rows) != 2 {
		t.Fatalf("claim rows=%+v err=%v", rows, err)
	}
	byExternalID := make(map[string]store.PoliticalDisclosureCandidate, len(rows))
	for _, row := range rows {
		byExternalID[row.ExternalID] = row
	}
	terminal := byExternalID[terminalPurchase.ExternalID]
	fresh := byExternalID[pendingSale.ExternalID]
	if !terminal.Claimed || !fresh.Claimed || terminal.TriggerEligible || !fresh.TriggerEligible {
		t.Fatalf("fenced/trigger rows = terminal:%+v fresh:%+v, want both fenced and only fresh trigger-eligible", terminal, fresh)
	}
}

func TestPoliticalDisclosureAggregateFenceReleasePreservesHistoricalStatus(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.March, 16, 12, 0, 0, 0, time.UTC)
	history := politicalDisclosureClaimFixture("test-aggregate-release-history", "PDRELEASE", now.AddDate(0, 0, -1))
	pending := politicalDisclosureClaimFixture("test-aggregate-release-pending", "PDRELEASE", now)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports WHERE source='house' AND external_id = ANY($1::text[])`,
			[]string{history.ExternalID, pending.ExternalID})
	})
	for _, report := range []store.PoliticalDisclosureReport{history, pending} {
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_candidates
		SET promotion_status='promoted', promotion_reason='historical result'
		WHERE report_id=(SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2)`,
		history.Source, history.ExternalID); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPoliticalDisclosureCandidates(ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, "aggregate-release-token")
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	if err := store.ReleasePoliticalDisclosurePromotionClaims(ctx, "aggregate-release-token", "scoring unavailable"); err != nil {
		t.Fatal(err)
	}
	for externalID, wantStatus := range map[string]string{history.ExternalID: "promoted", pending.ExternalID: "pending"} {
		var status, token string
		if err := store.GetDB().QueryRow(ctx, `
			SELECT c.promotion_status, COALESCE(c.promotion_claim_token,'')
			FROM political_disclosure_candidates c
			JOIN political_disclosure_reports r ON r.id=c.report_id
			WHERE r.source='house' AND r.external_id=$1 AND c.ticker='PDRELEASE'`, externalID).Scan(&status, &token); err != nil {
			t.Fatal(err)
		}
		if status != wantStatus || token != "" {
			t.Fatalf("%s after release status=%q token=%q, want %q/empty", externalID, status, token, wantStatus)
		}
	}
}

func TestPoliticalDisclosureAggregateClaimSkipsFreshHistoricalFence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.March, 16, 12, 0, 0, 0, time.UTC)
	history := politicalDisclosureClaimFixture("test-aggregate-fresh-history", "PDFRESHFENCE", now.AddDate(0, 0, -1))
	pending := politicalDisclosureClaimFixture("test-aggregate-fresh-pending", "PDFRESHFENCE", now)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports WHERE source='house' AND external_id = ANY($1::text[])`,
			[]string{history.ExternalID, pending.ExternalID})
	})
	for _, report := range []store.PoliticalDisclosureReport{history, pending} {
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_candidates
		SET promotion_status='promoted', promotion_claim_token='foreign-token', promotion_claimed_at=$3
		WHERE report_id=(SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2)`,
		history.Source, history.ExternalID, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPoliticalDisclosureCandidates(ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, "new-token")
	if err != nil || len(claimed) != 0 {
		t.Fatalf("fresh historical fence was stolen: claim=%+v err=%v", claimed, err)
	}
}

func TestPoliticalDisclosureHistoricalReextractionInvalidatesAggregatePromotion(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.March, 17, 12, 0, 0, 0, time.UTC)
	history := politicalDisclosureClaimFixture("test-aggregate-reextract-history", "PDREEXTRACT", now.AddDate(0, 0, -1))
	pending := politicalDisclosureClaimFixture("test-aggregate-reextract-pending", "PDREEXTRACT", now)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM watchlist WHERE ticker='PDREEXTRACT'`)
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports WHERE source='house' AND external_id = ANY($1::text[])`,
			[]string{history.ExternalID, pending.ExternalID})
	})
	for _, report := range []store.PoliticalDisclosureReport{history, pending} {
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_candidates SET promotion_status='promoted'
		WHERE report_id=(SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2)`,
		history.Source, history.ExternalID); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPoliticalDisclosureCandidates(ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, "aggregate-reextract-token")
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	reportIDs := make([]string, 0, len(claimed))
	for _, item := range claimed {
		reportIDs = append(reportIDs, item.ReportID)
	}
	// Only material re-extraction invalidates a promoted historical row's
	// aggregate fence. The changed raw transaction becomes different candidate
	// evidence while preserving the ticker lineage.
	reextracted := history
	reextracted.Transactions = append([]store.PoliticalDisclosureTransaction(nil), history.Transactions...)
	reextracted.Transactions[0].RawData = json.RawMessage(`{"reextracted":true}`)
	if err := store.SavePoliticalDisclosureReport(ctx, reextracted); err != nil {
		t.Fatal(err)
	}
	var historyStatus, historyToken string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT c.promotion_status, COALESCE(c.promotion_claim_token,'')
		FROM political_disclosure_candidates c
		JOIN political_disclosure_reports r ON r.id=c.report_id
		WHERE r.source=$1 AND r.external_id=$2 AND c.ticker='PDREEXTRACT'`, history.Source, history.ExternalID).Scan(&historyStatus, &historyToken); err != nil {
		t.Fatal(err)
	}
	if historyStatus != "pending" || historyToken != "" {
		t.Fatalf("material historical re-extraction status=%q token=%q, want pending/empty", historyStatus, historyToken)
	}
	item := store.WatchlistItem{Ticker: "PDREEXTRACT", Platform: "stock", Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, MinConfidence: .8, Source: "scanner"}
	err = store.PromoteScannerSourcesToWatchlist(ctx, item,
		store.ScannerSourcePromotionClaims{PoliticalDisclosure: &store.PoliticalDisclosurePromotionClaim{
			Ticker: "PDREEXTRACT", Token: "aggregate-reextract-token", ReportIDs: reportIDs,
		}}, "fixture", now)
	var claimErr *store.ScannerSourcePromotionClaimError
	if !errors.As(err, &claimErr) || claimErr.Source != "political_disclosure" {
		t.Fatalf("promotion error=%v, want stale political disclosure claim", err)
	}
	watchlistItem, err := store.GetWatchlistItem(ctx, "PDREEXTRACT", "stock")
	if err != nil || watchlistItem != nil {
		t.Fatalf("stale aggregate created watchlist item=%+v err=%v", watchlistItem, err)
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

func TestConcurrentPoliticalDisclosureCandidateClaimHasSingleWinner(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.April, 18, 12, 0, 0, 0, time.UTC)
	report := politicalDisclosureClaimFixture("test-concurrent-disclosure-claim", "PDCONCURRENT", now)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	type result struct {
		items []store.PoliticalDisclosureCandidate
		err   error
	}
	results := make(chan result, 2)
	for _, token := range []string{"concurrent-a", "concurrent-b"} {
		go func(token string) {
			<-start
			items, err := store.ClaimPoliticalDisclosureCandidates(
				ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, token,
			)
			results <- result{items: items, err: err}
		}(token)
	}
	close(start)
	winners := 0
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if len(got.items) > 0 {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent claim winners=%d, want 1", winners)
	}
}

func TestPoliticalDisclosureMaterialEvidenceRequeuesTickerFenceButKeepsHistoryTerminal(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.April, 19, 12, 0, 0, 0, time.UTC)
	history := politicalDisclosureClaimFixture("test-ticker-fence-history", "PDFENCE", now.AddDate(0, 0, -1))
	pending := politicalDisclosureClaimFixture("test-ticker-fence-pending", "PDFENCE", now)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports WHERE source='house' AND external_id = ANY($1::text[])`,
			[]string{history.ExternalID, pending.ExternalID})
	})
	for _, report := range []store.PoliticalDisclosureReport{history, pending} {
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, "ticker-fence-token",
	)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_candidates
		SET promotion_status='promoted', promotion_claim_token=NULL, promotion_claimed_at=NULL
		WHERE report_id=(SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2)
		  AND ticker='PDFENCE'`, history.Source, history.ExternalID); err != nil {
		t.Fatal(err)
	}
	// This is the newer report, so it invalidates only in-flight aggregate
	// ownership. The historical terminal outcome is immutable.
	pending.Transactions = append([]store.PoliticalDisclosureTransaction(nil), pending.Transactions...)
	pending.Transactions[0].TransactionDate = pending.Transactions[0].TransactionDate.AddDate(0, 0, 1)
	if err := store.SavePoliticalDisclosureReport(ctx, pending); err != nil {
		t.Fatal(err)
	}
	var historyStatus, pendingStatus, pendingToken string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT promotion_status FROM political_disclosure_candidates
		WHERE report_id=(SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2)
		  AND ticker='PDFENCE'`, history.Source, history.ExternalID).Scan(&historyStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.GetDB().QueryRow(ctx, `
		SELECT promotion_status, COALESCE(promotion_claim_token,'')
		FROM political_disclosure_candidates
		WHERE report_id=(SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2)
		  AND ticker='PDFENCE'`, pending.Source, pending.ExternalID).Scan(&pendingStatus, &pendingToken); err != nil {
		t.Fatal(err)
	}
	if historyStatus != "promoted" || pendingStatus != "pending" || pendingToken != "" {
		t.Fatalf("ticker fence states history=%q pending=%q/%q, want promoted and pending/empty", historyStatus, pendingStatus, pendingToken)
	}
}

func TestPoliticalDisclosureSortedMultiTickerReextractionsDoNotDeadlock(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.April, 20, 12, 0, 0, 0, time.UTC)
	first := politicalDisclosureClaimFixture("test-multiticker-first", "PDLOCKA", now)
	second := politicalDisclosureClaimFixture("test-multiticker-second", "PDLOCKB", now)
	for _, report := range []*store.PoliticalDisclosureReport{&first, &second} {
		report.Transactions = append(report.Transactions, store.PoliticalDisclosureTransaction{
			Ordinal: 2, AssetName: "Other lock ticker", Ticker: map[string]string{"test-multiticker-first": "PDLOCKB", "test-multiticker-second": "PDLOCKA"}[report.ExternalID],
			AssetType: "ST", TransactionType: "purchase", TransactionDate: now,
			AmountText: "$1,001 - $15,000", RawData: json.RawMessage(`{"row":2}`),
		})
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports WHERE source='house' AND external_id = ANY($1::text[])`,
			[]string{first.ExternalID, second.ExternalID})
	})
	for _, report := range []store.PoliticalDisclosureReport{first, second} {
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
	}
	first.Transactions[0].RawData = json.RawMessage(`{"revision":1}`)
	second.Transactions[0].RawData = json.RawMessage(`{"revision":1}`)
	done := make(chan error, 2)
	for _, report := range []store.PoliticalDisclosureReport{first, second} {
		go func(report store.PoliticalDisclosureReport) { done <- store.SavePoliticalDisclosureReport(ctx, report) }(report)
	}
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("multi-ticker re-extractions deadlocked")
		}
	}
}

func TestPoliticalDisclosureIngestionWinsQueuedPromotionWithoutWatchlist(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.April, 21, 12, 0, 0, 0, time.UTC)
	report := politicalDisclosureClaimFixture("test-ingestion-wins-promotion", "PDINGESTWINS", now)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), "DELETE FROM watchlist WHERE ticker=$1", "PDINGESTWINS")
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, "ingestion-wins-token",
	)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}

	// Hold the parent so ingestion acquires the report/ticker advisory locks and
	// queues ahead of promotion. Releasing this lock then proves the save gets
	// to invalidate the claim before the promoter can publish its watchlist row.
	lockTx, err := store.GetDB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(ctx) //nolint:errcheck
	var backendPID int
	if err := lockTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `
		SELECT 1 FROM political_disclosure_reports WHERE id=$1::uuid FOR UPDATE`, claimed[0].ReportID); err != nil {
		t.Fatal(err)
	}
	updated := report
	updated.Transactions = append([]store.PoliticalDisclosureTransaction(nil), report.Transactions...)
	updated.Transactions[0].RawData = json.RawMessage(`{"ingestion_won":true}`)
	saved := make(chan error, 1)
	go func() { saved <- store.SavePoliticalDisclosureReport(ctx, updated) }()
	blocked := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if err := store.GetDB().QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity a
				WHERE $1 = ANY(pg_blocking_pids(a.pid))
				  AND a.query LIKE '%political_disclosure_reports%'
			)`, backendPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
	}
	if !blocked {
		t.Fatal("ingestion did not queue on the parent after taking advisory locks")
	}
	item := store.WatchlistItem{Ticker: "PDINGESTWINS", Platform: "stock", Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, MinConfidence: .8, Source: "scanner"}
	promoted := make(chan error, 1)
	go func() {
		promoted <- store.PromoteScannerSourcesToWatchlist(ctx, item,
			store.ScannerSourcePromotionClaims{PoliticalDisclosure: &store.PoliticalDisclosurePromotionClaim{
				Ticker: "PDINGESTWINS", Token: "ingestion-wins-token", ReportIDs: []string{claimed[0].ReportID},
			}}, "fixture", now)
	}()
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-saved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ingestion did not complete after parent release")
	}
	var claimErr *store.ScannerSourcePromotionClaimError
	select {
	case err := <-promoted:
		if !errors.As(err, &claimErr) || claimErr.Source != "political_disclosure" {
			t.Fatalf("promotion error=%v, want stale political disclosure claim", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued promotion did not finish after ingestion")
	}
	watchlistItem, err := store.GetWatchlistItem(ctx, "PDINGESTWINS", "stock")
	if err != nil || watchlistItem != nil {
		t.Fatalf("stale promotion created watchlist item=%+v err=%v", watchlistItem, err)
	}
}

func TestPoliticalDisclosurePromotionWinsThenMaterialEvidenceIsPending(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2300, time.April, 22, 12, 0, 0, 0, time.UTC)
	report := politicalDisclosureClaimFixture("test-promotion-wins-evidence", "PDPROMOTEWINS", now)
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), "DELETE FROM watchlist WHERE ticker=$1", "PDPROMOTEWINS")
		_, _ = store.GetDB().Exec(context.Background(), `
			DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, now.AddDate(0, 0, -7), now, time.Hour, 1, "promotion-wins-token",
	)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	item := store.WatchlistItem{Ticker: "PDPROMOTEWINS", Platform: "stock", Strategies: []string{"momentum"}, Timeframes: []string{"1D"}, MinConfidence: .8, Source: "scanner"}
	if err := store.PromoteScannerSourcesToWatchlist(ctx, item,
		store.ScannerSourcePromotionClaims{PoliticalDisclosure: &store.PoliticalDisclosurePromotionClaim{
			Ticker: "PDPROMOTEWINS", Token: "promotion-wins-token", ReportIDs: []string{claimed[0].ReportID},
		}}, "fixture", now); err != nil {
		t.Fatal(err)
	}
	updated := report
	updated.Transactions = append([]store.PoliticalDisclosureTransaction(nil), report.Transactions...)
	updated.Transactions[0].RawData = json.RawMessage(`{"promotion_won":true}`)
	if err := store.SavePoliticalDisclosureReport(ctx, updated); err != nil {
		t.Fatal(err)
	}
	var status, token string
	if err := store.GetDB().QueryRow(ctx, `
		SELECT promotion_status, COALESCE(promotion_claim_token,'')
		FROM political_disclosure_candidates
		WHERE report_id=$1::uuid AND ticker='PDPROMOTEWINS'`, claimed[0].ReportID).Scan(&status, &token); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || token != "" {
		t.Fatalf("promotion-winner re-extraction = %q/%q, want pending/empty", status, token)
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

func TestHousePTRParseOutcomesPersistTerminalAndScheduledRetryStates(t *testing.T) {
	ctx := context.Background()
	filingDate := time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		externalID string
		pages      []disclosures.PageText
		status     string
		retryable  bool
	}{
		{
			name:       "no text is terminal",
			externalID: "test-house-ptr-no-text-terminal",
			pages: []disclosures.PageText{{Number: 1, Boxes: []disclosures.TextBox{
				{Text: " \n\t "},
			}}},
			status: "no_text",
		},
		{
			name:       "unreconstructed text schedules retry",
			externalID: "test-house-ptr-unreconstructed-retry",
			pages: []disclosures.PageText{{Number: 1, Boxes: []disclosures.TextBox{
				{Text: "transaction table heading without a parseable row"},
			}}},
			status: "failed", retryable: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed := disclosures.ParseHousePTR(disclosures.HouseFiling{
				First: "Jane", Last: "Member", FilingType: "P", StateDistrict: "TS01",
				Year: 2026, FilingDate: filingDate, DocumentID: test.externalID,
			}, "https://example.test/"+test.externalID+".pdf", []byte("pdf"), test.pages)
			report := store.PoliticalDisclosureReport{
				Source: parsed.Source, ExternalID: parsed.ExternalID, Chamber: parsed.Chamber,
				FilerName: parsed.FilerName, Office: parsed.Office, ReportType: parsed.ReportType,
				FilingDate: parsed.FilingDate, DocumentURL: parsed.DocumentURL, ContentHash: parsed.ContentHash,
				ExtractionStatus: parsed.ExtractionStatus, ExtractionError: parsed.ExtractionError,
				RetryableFailure: parsed.RetryableFailure, RawMetadata: json.RawMessage(parsed.RawMetadata),
			}
			t.Cleanup(func() {
				_, _ = store.GetDB().Exec(context.Background(),
					"DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID)
			})
			if err := store.SavePoliticalDisclosureFirstPass(ctx, report); err != nil {
				t.Fatal(err)
			}
			stored, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
			if err != nil || stored == nil || stored.ExtractionStatus != test.status || stored.RetryableFailure != test.retryable {
				t.Fatalf("stored=%+v err=%v, want status=%q retryable=%v", stored, err, test.status, test.retryable)
			}

			terminal, err := store.PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID)
			if err != nil || !terminal {
				t.Fatalf("freshly persisted report terminal=%v err=%v", terminal, err)
			}
			if !test.retryable {
				if stored.NextRetryAt != nil || stored.RetryAttemptCount != 0 {
					t.Fatalf("terminal no-text report scheduled retry: %+v", stored)
				}
				return
			}
			if stored.NextRetryAt == nil || stored.RetryAttemptCount != 1 {
				t.Fatalf("retryable parser miss was not scheduled: %+v", stored)
			}
			if _, err := store.GetDB().Exec(ctx, `
				UPDATE political_disclosure_reports SET next_retry_at=NOW()-INTERVAL '1 second'
				WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID); err != nil {
				t.Fatal(err)
			}
			terminal, err = store.PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID)
			if err != nil || terminal {
				t.Fatalf("due parser miss terminal=%v err=%v", terminal, err)
			}
			claims, err := store.ClaimDuePoliticalDisclosureRetries(ctx, report.Source, time.Now().UTC(), 1, time.Minute)
			if err != nil || len(claims) != 1 || claims[0].ExternalID != report.ExternalID {
				t.Fatalf("due retry claims=%+v err=%v", claims, err)
			}
			if err := store.ReleasePoliticalDisclosureRetryClaims(ctx, claims[0].RetryClaimToken); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSenatePTRCanonicalHeaderOnlyPersistsScheduledRetryAndClaim(t *testing.T) {
	ctx := context.Background()
	externalID := "test-senate-ptr-header-only-retry"
	parsed, err := disclosures.ParseSenatePTR(disclosures.SenateFiling{
		ExternalID: externalID, FilerName: "Jane Senator", Office: "TS",
		FilingDate:  time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC),
		DocumentURL: "https://efdsearch.senate.gov/search/view/ptr/" + externalID + "/",
	}, []byte("<table><tr><th>Transaction Date</th><th>Asset Name</th><th>Transaction Type</th><th>Amount</th></tr></table>"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ExtractionStatus != "failed" || !parsed.RetryableFailure {
		t.Fatalf("parsed header-only Senate PTR = %+v", parsed)
	}
	report := store.PoliticalDisclosureReport{
		Source: parsed.Source, ExternalID: parsed.ExternalID, Chamber: parsed.Chamber,
		FilerName: parsed.FilerName, Office: parsed.Office, ReportType: parsed.ReportType,
		FilingDate: parsed.FilingDate, DocumentURL: parsed.DocumentURL, ContentHash: parsed.ContentHash,
		ExtractionStatus: parsed.ExtractionStatus, ExtractionError: parsed.ExtractionError,
		RetryableFailure: parsed.RetryableFailure, RawMetadata: json.RawMessage(`{"fixture":true}`),
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(),
			"DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureFirstPass(ctx, report); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored == nil || stored.ExtractionStatus != "failed" || !stored.RetryableFailure ||
		stored.RetryAttemptCount != 1 || stored.NextRetryAt == nil {
		t.Fatalf("stored header-only Senate PTR=%+v err=%v", stored, err)
	}
	if terminal, err := store.PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID); err != nil || !terminal {
		t.Fatalf("backoff report terminal=%v err=%v", terminal, err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_reports SET next_retry_at=NOW()-INTERVAL '1 second'
		WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID); err != nil {
		t.Fatal(err)
	}
	if terminal, err := store.PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID); err != nil || terminal {
		t.Fatalf("due header-only Senate PTR terminal=%v err=%v", terminal, err)
	}
	claims, err := store.ClaimDuePoliticalDisclosureRetries(ctx, report.Source, time.Now().UTC(), 1, time.Minute)
	if err != nil || len(claims) != 1 || claims[0].ExternalID != report.ExternalID || claims[0].RetryClaimToken == "" {
		t.Fatalf("due header-only Senate PTR claims=%+v err=%v", claims, err)
	}
	if err := store.ReleasePoliticalDisclosureRetryClaims(ctx, claims[0].RetryClaimToken); err != nil {
		t.Fatal(err)
	}
}

func TestOGEPDFParseOutcomesPersistTerminalAndScheduledRetryStates(t *testing.T) {
	ctx := context.Background()
	filingDate := time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		externalID string
		pages      []disclosures.PageText
		status     string
		retryable  bool
	}{
		{
			name:       "no text is terminal",
			externalID: "test-oge-pdf-no-text-terminal",
			pages: []disclosures.PageText{{Number: 1, Boxes: []disclosures.TextBox{
				{Text: " \n\t "},
			}}},
			status: "no_text",
		},
		{
			name:       "unreconstructed text schedules retry",
			externalID: "test-oge-pdf-unreconstructed-retry",
			pages: []disclosures.PageText{{Number: 1, Boxes: []disclosures.TextBox{
				{Text: "transaction table heading without a parseable row"},
			}}},
			status: "failed", retryable: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed := disclosures.ParseOGEPDF(disclosures.OGEFiling{
				ExternalID: test.externalID, FilerName: "Jane Official", Office: "Executive Office",
				ReportType: "periodic_transaction", FilingDate: filingDate,
				DocumentURL: "https://example.test/" + test.externalID + ".pdf",
			}, []byte("pdf"), test.pages)
			report := store.PoliticalDisclosureReport{
				Source: parsed.Source, ExternalID: parsed.ExternalID, Chamber: parsed.Chamber,
				FilerName: parsed.FilerName, Office: parsed.Office, ReportType: parsed.ReportType,
				FilingDate: parsed.FilingDate, DocumentURL: parsed.DocumentURL, ContentHash: parsed.ContentHash,
				ExtractionStatus: parsed.ExtractionStatus, ExtractionError: parsed.ExtractionError,
				RetryableFailure: parsed.RetryableFailure, RawMetadata: json.RawMessage(parsed.RawMetadata),
			}
			t.Cleanup(func() {
				_, _ = store.GetDB().Exec(context.Background(),
					"DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID)
			})
			if err := store.SavePoliticalDisclosureFirstPass(ctx, report); err != nil {
				t.Fatal(err)
			}
			stored, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
			if err != nil || stored == nil || stored.ExtractionStatus != test.status || stored.RetryableFailure != test.retryable {
				t.Fatalf("stored=%+v err=%v, want status=%q retryable=%v", stored, err, test.status, test.retryable)
			}

			terminal, err := store.PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID)
			if err != nil || !terminal {
				t.Fatalf("freshly persisted report terminal=%v err=%v", terminal, err)
			}
			if !test.retryable {
				if stored.NextRetryAt != nil || stored.RetryAttemptCount != 0 {
					t.Fatalf("terminal no-text report scheduled retry: %+v", stored)
				}
				return
			}
			if stored.NextRetryAt == nil || stored.RetryAttemptCount != 1 {
				t.Fatalf("retryable parser miss was not scheduled: %+v", stored)
			}
			if _, err := store.GetDB().Exec(ctx, `
				UPDATE political_disclosure_reports SET next_retry_at=NOW()-INTERVAL '1 second'
				WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID); err != nil {
				t.Fatal(err)
			}
			terminal, err = store.PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID)
			if err != nil || terminal {
				t.Fatalf("due parser miss terminal=%v err=%v", terminal, err)
			}
			claims, err := store.ClaimDuePoliticalDisclosureRetries(ctx, report.Source, time.Now().UTC(), 1, time.Minute)
			if err != nil || len(claims) != 1 || claims[0].ExternalID != report.ExternalID {
				t.Fatalf("due retry claims=%+v err=%v", claims, err)
			}
			if err := store.ReleasePoliticalDisclosureRetryClaims(ctx, claims[0].RetryClaimToken); err != nil {
				t.Fatal(err)
			}
		})
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
	late.Chamber = "senate"
	late.FilerName = "Sen. Stale First Pass"
	late.Office = "ST99"
	late.ReportType = "annual"
	late.IsAmendment = true
	late.FilingDate = late.FilingDate.AddDate(1, 0, 0)
	late.DocumentURL = "https://example.test/stale-first-pass.pdf"
	late.ContentHash = ""
	late.RawMetadata = json.RawMessage(`{"fixture":"stale-first-pass"}`)
	late.Transactions = nil
	if err := store.SavePoliticalDisclosureFirstPass(ctx, late); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored == nil || stored.ExtractionStatus != "completed" ||
		stored.ContentHash != "completed-hash" || stored.RetryableFailure || stored.RetryAttemptCount != 0 || len(stored.Transactions) != 1 {
		t.Fatalf("late failure downgraded completed report: %+v err=%v", stored, err)
	}
	if stored.Chamber != report.Chamber || stored.FilerName != report.FilerName ||
		stored.Office != report.Office || stored.ReportType != report.ReportType ||
		stored.IsAmendment != report.IsAmendment ||
		stored.FilingDate.Format("2006-01-02") != report.FilingDate.Format("2006-01-02") ||
		stored.DocumentURL != report.DocumentURL || !rawJSONEqual(stored.RawMetadata, report.RawMetadata) {
		t.Fatalf("late failure changed completed observation: %+v", stored)
	}
	if stored.Transactions[0].Ticker != "CMF" {
		t.Fatalf("completed transaction evidence lost: %+v", stored.Transactions)
	}
}

func TestFirstPassDisclosureDoesNotOverwriteActiveRetryClaim(t *testing.T) {
	ctx := context.Background()
	report := store.PoliticalDisclosureReport{
		Source: "house", ExternalID: "test-disclosure-first-pass-active-retry", Chamber: "house",
		FilerName: "Hon. Retry Owner", Office: "TS06", ReportType: "periodic_transaction",
		FilingDate: time.Now().UTC(), DocumentURL: "https://example.test/retry-owner.pdf",
		ExtractionStatus: "failed", ExtractionError: "temporary", RetryableFailure: true,
		RawMetadata: json.RawMessage(`{"owner":"retry"}`),
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(),
			"DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2",
			report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	claimNow := time.Now().UTC().Add(time.Minute)
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_reports SET next_retry_at=$3
		WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID, claimNow.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPoliticalDisclosureRetries(
		ctx, report.Source, claimNow, 1, time.Minute, "retry-owner",
	)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}

	staleFirstPass := report
	staleFirstPass.Chamber = "senate"
	staleFirstPass.FilerName = "Sen. Stale First Pass"
	staleFirstPass.Office = "ST99"
	staleFirstPass.ReportType = "annual"
	staleFirstPass.FilingDate = report.FilingDate.AddDate(1, 0, 0)
	staleFirstPass.DocumentURL = "https://example.test/stale-completion.pdf"
	staleFirstPass.ContentHash = "stale-completed-hash"
	staleFirstPass.ExtractionStatus = "completed"
	staleFirstPass.ExtractionError = ""
	staleFirstPass.RetryableFailure = false
	staleFirstPass.RawMetadata = json.RawMessage(`{"owner":"stale-first-pass"}`)
	staleFirstPass.Transactions = []store.PoliticalDisclosureTransaction{{
		Ordinal: 1, AssetName: "Stale Corp", Ticker: "STAL", TransactionType: "purchase",
		TransactionDate: staleFirstPass.FilingDate,
	}}
	if err := store.SavePoliticalDisclosureFirstPass(ctx, staleFirstPass); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored == nil || stored.ExtractionStatus != "failed" ||
		stored.RetryAttemptCount != 1 || stored.RetryClaimToken != "retry-owner" ||
		stored.FilerName != report.FilerName || !rawJSONEqual(stored.RawMetadata, report.RawMetadata) ||
		len(stored.Transactions) != 0 {
		t.Fatalf("stale first pass changed active retry: %+v err=%v", stored, err)
	}

	ownedRetryCompletion := staleFirstPass
	ownedRetryCompletion.Chamber = report.Chamber
	ownedRetryCompletion.FilerName = report.FilerName
	ownedRetryCompletion.Office = report.Office
	ownedRetryCompletion.ReportType = report.ReportType
	ownedRetryCompletion.FilingDate = report.FilingDate
	ownedRetryCompletion.DocumentURL = report.DocumentURL
	ownedRetryCompletion.ContentHash = "retry-owner-hash"
	ownedRetryCompletion.RawMetadata = json.RawMessage(`{"owner":"retry-result"}`)
	ownedRetryCompletion.Transactions[0].Ticker = "OWNED"
	if err := store.SavePoliticalDisclosureRetryResult(ctx, ownedRetryCompletion, "retry-owner"); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored == nil || stored.ExtractionStatus != "completed" ||
		stored.RetryClaimToken != "" || stored.ContentHash != "retry-owner-hash" ||
		!rawJSONEqual(stored.RawMetadata, ownedRetryCompletion.RawMetadata) || len(stored.Transactions) != 1 ||
		stored.Transactions[0].Ticker != "OWNED" {
		t.Fatalf("owned retry completion did not win: %+v err=%v", stored, err)
	}
}

func TestPoliticalDisclosureRetryFailureIncrementsAttemptOnce(t *testing.T) {
	ctx := context.Background()
	report := store.PoliticalDisclosureReport{
		Source: "senate", ExternalID: "test-disclosure-retry-failure-once", Chamber: "senate",
		FilerName: "Sen. Retry Failure", ReportType: "periodic_transaction",
		FilingDate: time.Now().UTC(), DocumentURL: "https://example.test/retry-failure.pdf",
		ExtractionStatus: "failed", ExtractionError: "first failure", RetryableFailure: true,
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(),
			"DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2",
			report.Source, report.ExternalID)
	})
	if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	claimNow := time.Now().UTC().Add(time.Minute)
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE political_disclosure_reports SET next_retry_at=$3
		WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID, claimNow.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPoliticalDisclosureRetries(
		ctx, report.Source, claimNow, 1, time.Minute, "retry-failure-owner",
	)
	if err != nil || len(claimed) != 1 || claimed[0].RetryAttemptCount != 1 {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	retryFailure := report
	retryFailure.ExtractionError = "second failure"
	if err := store.SavePoliticalDisclosureRetryResult(ctx, retryFailure, "retry-failure-owner"); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
	if err != nil || stored == nil || stored.ExtractionStatus != "failed" ||
		stored.RetryAttemptCount != 2 || !stored.RetryableFailure || stored.NextRetryAt == nil ||
		stored.RetryClaimToken != "" {
		t.Fatalf("retry failure did not increment exactly once: %+v err=%v", stored, err)
	}
}

func TestFirstPassDuplicateRetryableFailureIsIdempotent(t *testing.T) {
	ctx := context.Background()
	newFailure := func(externalID string) store.PoliticalDisclosureReport {
		return store.PoliticalDisclosureReport{
			Source: "senate", ExternalID: externalID, Chamber: "senate",
			FilerName: "Sen. First Pass Retry", ReportType: "periodic_transaction",
			FilingDate: time.Now().UTC(), DocumentURL: "https://example.test/" + externalID + ".pdf",
			ExtractionStatus: "failed", ExtractionError: "transient fetch failure", RetryableFailure: true,
		}
	}
	assertSingleAttempt := func(t *testing.T, report store.PoliticalDisclosureReport) {
		t.Helper()
		stored, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
		if err != nil || stored == nil || stored.ExtractionStatus != "failed" ||
			stored.RetryAttemptCount != 1 || !stored.RetryableFailure || stored.NextRetryAt == nil {
			t.Fatalf("stored=%+v err=%v, want one scheduled retry", stored, err)
		}
	}
	t.Run("sequential duplicate leaves retry schedule unchanged", func(t *testing.T) {
		report := newFailure("test-first-pass-failure-sequential")
		t.Cleanup(func() {
			_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID)
		})
		if err := store.SavePoliticalDisclosureFirstPass(ctx, report); err != nil {
			t.Fatal(err)
		}
		first, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
		if err != nil || first == nil || first.NextRetryAt == nil {
			t.Fatalf("first=%+v err=%v", first, err)
		}
		if err := store.SavePoliticalDisclosureFirstPass(ctx, report); err != nil {
			t.Fatal(err)
		}
		assertSingleAttempt(t, report)
		second, err := store.GetPoliticalDisclosureReport(ctx, report.Source, report.ExternalID)
		if err != nil || second == nil || second.NextRetryAt == nil || !second.NextRetryAt.Equal(*first.NextRetryAt) {
			t.Fatalf("duplicate moved retry schedule: first=%+v second=%+v err=%v", first.NextRetryAt, second, err)
		}
	})
	t.Run("concurrent duplicate observations retain one attempt", func(t *testing.T) {
		report := newFailure("test-first-pass-failure-concurrent")
		t.Cleanup(func() {
			_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2`, report.Source, report.ExternalID)
		})
		start := make(chan struct{})
		errs := make(chan error, 2)
		var workers sync.WaitGroup
		for range 2 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				errs <- store.SavePoliticalDisclosureFirstPass(ctx, report)
			}()
		}
		close(start)
		workers.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		assertSingleAttempt(t, report)
	})
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

func TestPoliticalDisclosureCandidateRefreshRequeuesOnlyMaterialChanges(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2099, time.January, 2, 3, 4, 5, 0, time.UTC)
	scannerRunID := "00000000-0000-0000-0000-000000000532"
	if _, err := store.GetDB().Exec(ctx, `
		INSERT INTO scanner_runs (
			id, started_at, status, pullback_universe_enabled,
			pullback_min_momentum_return_pct, pullback_min_market_cap_mm,
			pullback_min_dollar_volume, candidate_cap, min_conviction, ttl_days
		) VALUES ($1, NOW(), 'completed', false, 0, 0, 0, 1, 0, 1)
		ON CONFLICT (id) DO NOTHING`, scannerRunID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(),
			"DELETE FROM scanner_runs WHERE id=$1", scannerRunID)
	})

	type candidateState struct {
		status, token, scannerRunID, reason string
		claimedAt, promotedAt               *time.Time
	}
	readCandidate := func(t *testing.T, report store.PoliticalDisclosureReport, ticker string) candidateState {
		t.Helper()
		var state candidateState
		if err := store.GetDB().QueryRow(ctx, `
			SELECT promotion_status, COALESCE(promotion_claim_token,''),
				COALESCE(scanner_run_id::text,''), COALESCE(promotion_reason,''),
				promotion_claimed_at, promoted_at
			FROM political_disclosure_candidates
			WHERE report_id=(
				SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2
			) AND ticker=$3`, report.Source, report.ExternalID, ticker,
		).Scan(&state.status, &state.token, &state.scannerRunID, &state.reason,
			&state.claimedAt, &state.promotedAt); err != nil {
			t.Fatal(err)
		}
		return state
	}
	setCandidateState := func(t *testing.T, report store.PoliticalDisclosureReport, ticker, status, token, reason string) {
		t.Helper()
		if _, err := store.GetDB().Exec(ctx, `
			UPDATE political_disclosure_candidates
			SET promotion_status=$4, promotion_claim_token=$5, promotion_claimed_at=$6,
				scanner_run_id=$7::uuid, promoted_at=$6, promotion_reason=$8
			WHERE report_id=(
				SELECT id FROM political_disclosure_reports WHERE source=$1 AND external_id=$2
			) AND ticker=$3`, report.Source, report.ExternalID, ticker, status, token, now, scannerRunID, reason,
		); err != nil {
			t.Fatal(err)
		}
	}
	assertReset := func(t *testing.T, state candidateState) {
		t.Helper()
		if state.status != "pending" || state.token != "" || state.scannerRunID != "" ||
			state.reason != "" || state.claimedAt != nil || state.promotedAt != nil {
			t.Fatalf("candidate was not requeued cleanly: %+v", state)
		}
	}
	assertPreserved := func(t *testing.T, state candidateState, status, token, reason string) {
		t.Helper()
		if state.status != status || state.token != token || state.scannerRunID != scannerRunID ||
			state.reason != reason || state.claimedAt == nil || state.promotedAt == nil {
			t.Fatalf("candidate outcome/fence was not preserved: %+v", state)
		}
	}
	newReport := func(t *testing.T, suffix string) store.PoliticalDisclosureReport {
		t.Helper()
		ticker := "RQ" + strings.ToUpper(suffix)
		report := politicalDisclosureClaimFixture("test-candidate-requeue-"+suffix, ticker, now)
		report.ContentHash = "original-content-hash"
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = store.GetDB().Exec(context.Background(),
				"DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2", report.Source, report.ExternalID)
		})
		return report
	}

	t.Run("changed evidence resets terminal and active outcomes", func(t *testing.T) {
		for _, status := range []string{"rejected", "processing"} {
			t.Run(status, func(t *testing.T) {
				report := newReport(t, "changed-"+status)
				ticker := report.Transactions[0].Ticker
				setCandidateState(t, report, ticker, status, status+"-fence", status+" outcome")
				changed := report
				changed.Transactions = append([]store.PoliticalDisclosureTransaction(nil), report.Transactions...)
				changed.Transactions[0].RawData = json.RawMessage(`{"changed":true}`)
				if err := store.SavePoliticalDisclosureReport(ctx, changed); err != nil {
					t.Fatal(err)
				}
				assertReset(t, readCandidate(t, report, ticker))
			})
		}
	})

	t.Run("idempotent and content-only refresh preserve outcomes", func(t *testing.T) {
		report := newReport(t, "idempotent")
		ticker := report.Transactions[0].Ticker
		setCandidateState(t, report, ticker, "processing", "active-fence", "active outcome")
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
		assertPreserved(t, readCandidate(t, report, ticker), "processing", "active-fence", "active outcome")

		setCandidateState(t, report, ticker, "rejected", "terminal-fence", "terminal outcome")
		contentOnly := report
		contentOnly.ContentHash = "new-content-hash"
		contentOnly.RawMetadata = json.RawMessage(`{"changed":"metadata only"}`)
		if err := store.SavePoliticalDisclosureReport(ctx, contentOnly); err != nil {
			t.Fatal(err)
		}
		assertPreserved(t, readCandidate(t, report, ticker), "rejected", "terminal-fence", "terminal outcome")
	})

	t.Run("every evidence tuple component is material", func(t *testing.T) {
		tests := []struct {
			name   string
			change func(*store.PoliticalDisclosureReport)
		}{
			{
				name: "transaction count",
				change: func(report *store.PoliticalDisclosureReport) {
					additional := report.Transactions[0]
					additional.Ordinal = 2
					report.Transactions = append(report.Transactions, additional)
				},
			},
			{
				name: "latest transaction date",
				change: func(report *store.PoliticalDisclosureReport) {
					report.Transactions[0].TransactionDate = report.Transactions[0].TransactionDate.AddDate(0, 0, 1)
				},
			},
			{
				name: "evidence json",
				change: func(report *store.PoliticalDisclosureReport) {
					report.Transactions[0].RawData = json.RawMessage(`{"different":"evidence"}`)
				},
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				report := newReport(t, strings.ReplaceAll(test.name, " ", "-"))
				ticker := report.Transactions[0].Ticker
				setCandidateState(t, report, ticker, "rejected", "terminal-fence", "terminal outcome")
				changed := report
				changed.Transactions = append([]store.PoliticalDisclosureTransaction(nil), report.Transactions...)
				test.change(&changed)
				if err := store.SavePoliticalDisclosureReport(ctx, changed); err != nil {
					t.Fatal(err)
				}
				assertReset(t, readCandidate(t, report, ticker))
			})
		}
	})

	t.Run("superseded reappearance and parent scoring changes requeue", func(t *testing.T) {
		report := newReport(t, "parent")
		ticker := report.Transactions[0].Ticker
		setCandidateState(t, report, ticker, "superseded", "old-fence", "old outcome")
		if err := store.SavePoliticalDisclosureReport(ctx, report); err != nil {
			t.Fatal(err)
		}
		assertReset(t, readCandidate(t, report, ticker))

		parentChanges := []struct {
			name   string
			change func(*store.PoliticalDisclosureReport)
		}{
			{"chamber", func(report *store.PoliticalDisclosureReport) { report.Chamber = "senate" }},
			{"filer", func(report *store.PoliticalDisclosureReport) { report.FilerName = "Hon. Parent Score Changed" }},
			{"office", func(report *store.PoliticalDisclosureReport) { report.Office = "TS99" }},
			{"report type", func(report *store.PoliticalDisclosureReport) { report.ReportType = "annual" }},
			{"amendment", func(report *store.PoliticalDisclosureReport) { report.IsAmendment = !report.IsAmendment }},
			{"filing date", func(report *store.PoliticalDisclosureReport) { report.FilingDate = report.FilingDate.AddDate(0, 0, 1) }},
			{"document URL", func(report *store.PoliticalDisclosureReport) { report.DocumentURL += "?rescored=1" }},
		}
		current := report
		for _, parentChange := range parentChanges {
			t.Run(parentChange.name, func(t *testing.T) {
				setCandidateState(t, current, ticker, "rejected", "terminal-fence", "terminal outcome")
				changed := current
				parentChange.change(&changed)
				if err := store.SavePoliticalDisclosureReport(ctx, changed); err != nil {
					t.Fatal(err)
				}
				assertReset(t, readCandidate(t, changed, ticker))
				current = changed
			})
		}

		setCandidateState(t, current, ticker, "processing", "status-fence", "status outcome")
		statusChanged := current
		statusChanged.ExtractionStatus = "no_text"
		statusChanged.Transactions = nil
		if err := store.SavePoliticalDisclosureReport(ctx, statusChanged); err != nil {
			t.Fatal(err)
		}
		state := readCandidate(t, statusChanged, ticker)
		if state.status != "superseded" || state.token != "" || state.claimedAt != nil {
			t.Fatalf("non-completed parent status did not invalidate candidate: %+v", state)
		}
	})
}
