package disclosures

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type runnerTestExtractor struct{}

func (runnerTestExtractor) Extract([]byte) ([]PageText, error) {
	return []PageText{{Number: 1}}, nil
}

func (runnerTestExtractor) Close() error { return nil }

type countingSenateTransport struct {
	searchBody  []byte
	fetchBody   []byte
	searchCalls int
}

func (t *countingSenateTransport) SearchPTRs(
	context.Context, time.Time, time.Time, int, int,
) ([]byte, error) {
	t.searchCalls++
	return t.searchBody, nil
}

func (t *countingSenateTransport) FetchPTR(context.Context, string) ([]byte, error) {
	return t.fetchBody, nil
}

func (*countingSenateTransport) Close() error { return nil }

func TestDistributeSyncBudgetReservesCapacityForEnabledSources(t *testing.T) {
	tests := []struct {
		name    string
		limit   int
		enabled []bool
		want    []int
	}{
		{name: "all sources", limit: 8, enabled: []bool{true, true, true}, want: []int{3, 3, 2}},
		{name: "two sources", limit: 5, enabled: []bool{true, false, true}, want: []int{3, 0, 2}},
		{name: "single source", limit: 4, enabled: []bool{false, true, false}, want: []int{0, 4, 0}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := distributeSyncBudget(test.limit, test.enabled...)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("budgets = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDistributeSyncBudgetRejectsLimitBelowEnabledSourceCount(t *testing.T) {
	if _, err := distributeSyncBudget(1, true, true, true); err == nil {
		t.Fatal("undersized cross-source limit was accepted")
	}
}

func TestScheduledDisclosureRangeUsesCalendarDayBoundaries(t *testing.T) {
	now := time.Date(2026, time.August, 17, 16, 42, 19, 0,
		time.FixedZone("test", -4*60*60),
	)
	since, until := scheduledDisclosureRange(now, 90)
	wantUntil := time.Date(2026, time.August, 17, 0, 0, 0, 0, time.UTC)
	wantSince := wantUntil.AddDate(0, 0, -90)
	if !since.Equal(wantSince) || !until.Equal(wantUntil) {
		t.Fatalf("range = %s through %s, want %s through %s",
			since, until, wantSince, wantUntil)
	}
}

func TestSenateSyncSkipsTerminalFailureWithoutConsumingBudget(t *testing.T) {
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = &fixtureSenateTransport{
		searchBody: []byte(`{"recordsFiltered":2,"data":[["New","Failure","Office","<a href='/search/view/ptr/failed-newest/'>PTR</a>","08/10/2026"],["Older","Pending","Office","<a href='/search/view/ptr/pending-older/'>PTR</a>","08/09/2026"]]}`),
		fetchBody:  []byte(`<table><tr><th>Transaction Date</th><th>Ticker</th><th>Asset Name</th><th>Transaction Type</th><th>Amount</th></tr><tr><td>08/08/2026</td><td>MSFT</td><td>Microsoft</td><td>Purchase</td><td>$1,001 - $15,000</td></tr></table>`),
	}
	var persisted []string
	runner := &Runner{
		Senate: client,
		reportTerminal: func(_ context.Context, source, externalID string) (bool, error) {
			return source == "senate" && externalID == "failed-newest", nil
		},
		persistReport: func(_ context.Context, report Report) error {
			persisted = append(persisted, report.ExternalID)
			return nil
		},
	}
	processed, err := runner.syncSenate(
		context.Background(),
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 1 || len(persisted) != 1 || persisted[0] != "pending-older" {
		t.Fatalf("processed=%d persisted=%v", processed, persisted)
	}
}

func TestSenateSyncPersistsTransientFailureAsRetryable(t *testing.T) {
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = &fixtureSenateTransport{
		searchBody: []byte(`{"recordsFiltered":1,"data":[["Failure","Later","Office","<a href='/search/view/ptr/retryable/'>PTR</a>","08/10/2026"]]}`),
		fetchErr:   errors.New("temporary upstream failure"),
	}
	var persisted []Report
	runner := &Runner{
		Senate:         client,
		reportTerminal: func(context.Context, string, string) (bool, error) { return false, nil },
		persistReport: func(_ context.Context, report Report) error {
			persisted = append(persisted, report)
			return nil
		},
	}
	_, err = runner.syncSenate(context.Background(),
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), 1)
	if err == nil || len(persisted) != 1 || persisted[0].ExtractionStatus != "failed" || !persisted[0].RetryableFailure {
		t.Fatalf("transient failure was not persisted for retry: reports=%+v err=%v", persisted, err)
	}
}

func TestSenateSyncAuthorizationFailureDoesNotConsumeBudgetOrPersist(t *testing.T) {
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = &fixtureSenateTransport{
		searchBody: []byte(`{"recordsFiltered":1,"data":[["Blocked","Member","Office","<a href='/search/view/ptr/blocked/'>PTR</a>","08/10/2026"]]}`),
		fetchErr:   ErrSenateAuthorizationRequired,
	}
	persisted := 0
	runner := &Runner{Senate: client,
		reportTerminal: func(context.Context, string, string) (bool, error) { return false, nil },
		persistReport:  func(context.Context, Report) error { persisted++; return nil },
	}
	processed, err := runner.syncSenate(context.Background(), time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), 1)
	if !errors.Is(err, ErrSenateAuthorizationRequired) || processed != 0 || persisted != 0 {
		t.Fatalf("processed=%d persisted=%d err=%v", processed, persisted, err)
	}
}

func TestSenateSyncRejectsMixedMalformedPageWithoutFetchingOrPersisting(t *testing.T) {
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = &fixtureSenateTransport{searchBody: []byte(`{"recordsFiltered":2,"data":[["Valid","Member","Office","<a href='/search/view/ptr/valid/'>PTR</a>","08/10/2026"],["malformed"]]}`)}
	persisted := 0
	runner := &Runner{Senate: client, persistReport: func(context.Context, Report) error { persisted++; return nil }}
	processed, err := runner.syncSenate(context.Background(), time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), 2)
	if err == nil || processed != 0 || persisted != 0 {
		t.Fatalf("processed=%d persisted=%d err=%v", processed, persisted, err)
	}
}

func TestSenateSyncPersistsPolicyFailureWithoutRetry(t *testing.T) {
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = &fixtureSenateTransport{
		searchBody: []byte(`{"recordsFiltered":1,"data":[["Failure","Later","Office","<a href='/search/view/ptr/permanent/'>PTR</a>","08/10/2026"]]}`),
		fetchErr:   permanentDisclosureError(errors.New("untrusted document URL")),
	}
	var persisted []Report
	runner := &Runner{Senate: client,
		reportTerminal: func(context.Context, string, string) (bool, error) { return false, nil },
		persistReport:  func(_ context.Context, report Report) error { persisted = append(persisted, report); return nil },
	}
	_, err = runner.syncSenate(context.Background(),
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), 1)
	if err == nil || len(persisted) != 1 || persisted[0].RetryableFailure {
		t.Fatalf("policy failure was persisted as retryable: reports=%+v err=%v", persisted, err)
	}
}

func TestSenateSyncBackoffSkipsFailureThenRetriesWithoutStarvingOlderFiling(t *testing.T) {
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = &fixtureSenateTransport{searchBody: []byte(`{"recordsFiltered":2,"data":[["Newest","Failure","Office","<a href='/search/view/ptr/newest/'>PTR</a>","08/10/2026"],["Older","Pending","Office","<a href='/search/view/ptr/older/'>PTR</a>","08/09/2026"]]}`), fetchBody: []byte(`<table><tr><th>Transaction Date</th><th>Ticker</th><th>Asset Name</th><th>Transaction Type</th><th>Amount</th></tr><tr><td>08/08/2026</td><td>MSFT</td><td>Microsoft</td><td>Purchase</td><td>$1,001 - $15,000</td></tr></table>`)}
	backoff := true
	var persisted []string
	runner := &Runner{Senate: client,
		reportTerminal: func(_ context.Context, _, id string) (bool, error) { return id == "newest" && backoff, nil },
		persistReport: func(_ context.Context, report Report) error {
			persisted = append(persisted, report.ExternalID)
			return nil
		},
	}
	rangeStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	processed, err := runner.syncSenate(context.Background(), rangeStart, rangeEnd, 1)
	if err != nil || processed != 1 || len(persisted) != 1 || persisted[0] != "older" {
		t.Fatalf("backoff starved older filing: processed=%d persisted=%v err=%v", processed, persisted, err)
	}
	backoff = false
	persisted = nil
	processed, err = runner.syncSenate(context.Background(), rangeStart, rangeEnd, 1)
	if err != nil || processed != 1 || len(persisted) != 1 || persisted[0] != "newest" {
		t.Fatalf("due retry was not eligible: processed=%d persisted=%v err=%v", processed, persisted, err)
	}
}

func TestRunOnceProcessesLeasedRetryBeforeDiscoveryAndConsumesSourceBudget(t *testing.T) {
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = &fixtureSenateTransport{fetchBody: []byte(`<table><tr><th>Transaction Date</th><th>Ticker</th><th>Asset Name</th><th>Transaction Type</th><th>Amount</th></tr><tr><td>08/08/2026</td><td>MSFT</td><td>Microsoft</td><td>Purchase</td><td>$1,001 - $15,000</td></tr></table>`)}
	var persisted []string
	claimed := 0
	runner := &Runner{
		Cfg: config.DisclosuresConfig{LookbackDays: 1, MaxReportsPerSync: 1}, Senate: client,
		Now: func() time.Time { return time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC) },
		claimDueRetries: func(_ context.Context, source string, _ time.Time, limit int, _ time.Duration) ([]store.PoliticalDisclosureReport, error) {
			if source != "senate" || limit != 1 {
				t.Fatalf("claim source=%q limit=%d", source, limit)
			}
			claimed++
			return []store.PoliticalDisclosureReport{{Source: "senate", ExternalID: "due", Chamber: "senate", FilerName: "Jane Senator", Office: "Office", ReportType: "periodic_transaction", FilingDate: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), DocumentURL: "https://efdsearch.senate.gov/search/view/ptr/due/", RawMetadata: []byte(`{}`)}}, nil
		},
		persistReport: func(_ context.Context, report Report) error {
			persisted = append(persisted, report.ExternalID)
			return nil
		},
	}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if claimed != 1 || len(persisted) != 1 || persisted[0] != "due" {
		t.Fatalf("claimed=%d persisted=%v", claimed, persisted)
	}
}

func TestHouseRetryRejectsMismatchedStoredDocumentIdentityPermanently(t *testing.T) {
	var persisted []Report
	runner := &Runner{
		House: &HouseClient{},
		claimDueRetries: func(context.Context, string, time.Time, int, time.Duration) ([]store.PoliticalDisclosureReport, error) {
			return []store.PoliticalDisclosureReport{{
				Source: "house", ExternalID: "claimed-id", Chamber: "house", FilerName: "Test Member",
				ReportType: "periodic_transaction", FilingDate: time.Now().UTC(), DocumentURL: "https://example.test/claimed-id.pdf",
				RawMetadata: []byte(`{"DocumentID":"other-id","Year":2026}`),
			}}, nil
		},
		persistReport: func(_ context.Context, report Report) error { persisted = append(persisted, report); return nil },
	}
	processed, err := runner.retryHouse(context.Background(), 1)
	if err == nil || processed != 1 || len(persisted) != 1 || persisted[0].RetryableFailure || persisted[0].ExtractionStatus != "failed" {
		t.Fatalf("processed=%d reports=%+v err=%v", processed, persisted, err)
	}
}

func TestRetryHeartbeatRenewsClaimedBatch(t *testing.T) {
	called := make(chan struct{}, 1)
	runner := &Runner{RetryLease: 9 * time.Millisecond, Now: time.Now,
		renewRetryLease: func(context.Context, string, string, string, time.Time, time.Duration) (bool, error) {
			select {
			case called <- struct{}{}:
			default:
			}
			return true, nil
		},
	}
	stop := runner.heartbeatRetries(context.Background(), []store.PoliticalDisclosureReport{{Source: "senate", ExternalID: "x", RetryClaimToken: "token"}})
	defer stop()
	select {
	case <-called:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("retry heartbeat did not renew lease")
	}
}

func TestSenateBlockedRetryReleasesClaimsWithoutPersistingAttempt(t *testing.T) {
	client, _ := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	client.Transport = &fixtureSenateTransport{fetchErr: ErrSenateAuthorizationRequired}
	released := 0
	persisted := 0
	r := &Runner{Senate: client, claimDueRetries: func(context.Context, string, time.Time, int, time.Duration) ([]store.PoliticalDisclosureReport, error) {
		return []store.PoliticalDisclosureReport{{Source: "senate", ExternalID: "x", FilerName: "X", FilingDate: time.Now(), DocumentURL: "https://efdsearch.senate.gov/search/view/ptr/x/", RetryClaimToken: "t"}}, nil
	}, releaseRetryClaims: func(context.Context, string) error { released++; return nil }, persistReport: func(context.Context, Report) error { persisted++; return nil }}
	_, err := r.retrySenate(context.Background(), 1)
	if !errors.Is(err, ErrSenateAuthorizationRequired) || released == 0 || persisted != 0 {
		t.Fatalf("err=%v released=%d persisted=%d", err, released, persisted)
	}
}

func TestDisclosureFailureClassifier(t *testing.T) {
	if shouldRetryDisclosureFailure(permanentDisclosureError(errors.New("bad URL"))) {
		t.Fatal("policy failure was retryable")
	}
	for status, want := range map[int]bool{400: false, 404: false, 408: true, 429: true, 500: true} {
		if got := shouldRetryDisclosureFailure(disclosureHTTPStatusError("fixture", status, http.StatusText(status))); got != want {
			t.Fatalf("HTTP %d retryable=%v, want %v", status, got, want)
		}
	}
}

func TestNewRunnerUsesSenateBrowserTransportByDefault(t *testing.T) {
	cfg := config.Defaults().Disclosures
	cfg.House.Enabled = false
	cfg.Senate.Enabled = true
	runner, err := NewRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close() //nolint:errcheck
	if _, ok := runner.Senate.Transport.(*SenateChromeTransport); !ok {
		t.Fatalf("transport = %T", runner.Senate.Transport)
	}
}

func TestNewRunnerAllowsExplicitSenateHTTPFallback(t *testing.T) {
	cfg := config.Defaults().Disclosures
	cfg.House.Enabled = false
	cfg.Senate.Enabled = true
	cfg.Senate.Transport = "http"
	runner, err := NewRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close() //nolint:errcheck
	if runner.Senate.Transport != nil {
		t.Fatalf("transport = %T, want legacy HTTP", runner.Senate.Transport)
	}
}

func TestNewRunnerRejectsInvalidEnabledDisclosureConfigBeforeResources(t *testing.T) {
	cfg := config.Defaults().Disclosures
	cfg.Enabled = true
	cfg.RequestTimeoutSeconds = 0
	if _, err := NewRunner(cfg); err == nil || !strings.Contains(err.Error(), "request_timeout_seconds") {
		t.Fatalf("NewRunner error=%v, want disclosure validation", err)
	}
}

func TestRetryPersistenceErrorDoesNotBecomeExtractionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fixture PDF"))
	}))
	defer server.Close()

	filingDate := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	validSenatePTR := []byte(`<table><tr><th>Transaction Date</th><th>Ticker</th><th>Asset Name</th><th>Transaction Type</th><th>Amount</th></tr><tr><td>08/08/2026</td><td>MSFT</td><td>Microsoft</td><td>Purchase</td><td>$1,001 - $15,000</td></tr></table>`)
	tests := []struct {
		name  string
		retry func(*Runner, context.Context) (int, error)
		make  func() *Runner
	}{
		{
			name:  "house",
			retry: func(r *Runner, ctx context.Context) (int, error) { return r.retryHouse(ctx, 1) },
			make: func() *Runner {
				return &Runner{
					House:     &HouseClient{BaseURL: server.URL, HTTP: server.Client()},
					Extractor: runnerTestExtractor{},
					claimDueRetries: func(context.Context, string, time.Time, int, time.Duration) ([]store.PoliticalDisclosureReport, error) {
						return []store.PoliticalDisclosureReport{{
							Source: "house", ExternalID: "retry-house", Chamber: "house", FilerName: "Jane Member", Office: "CA01", ReportType: "periodic_transaction", FilingDate: filingDate,
							RawMetadata: []byte(`{"First":"Jane","Last":"Member","FilingType":"P","StateDistrict":"CA01","Year":2026,"FilingDate":"2026-08-10T00:00:00Z","DocumentID":"retry-house"}`), RetryClaimToken: "token",
						}}, nil
					},
				}
			},
		},
		{
			name:  "senate",
			retry: func(r *Runner, ctx context.Context) (int, error) { return r.retrySenate(ctx, 1) },
			make: func() *Runner {
				client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
				if err != nil {
					t.Fatal(err)
				}
				client.Transport = &fixtureSenateTransport{fetchBody: validSenatePTR}
				return &Runner{Senate: client, claimDueRetries: func(context.Context, string, time.Time, int, time.Duration) ([]store.PoliticalDisclosureReport, error) {
					return []store.PoliticalDisclosureReport{{Source: "senate", ExternalID: "retry-senate", Chamber: "senate", FilerName: "Jane Senator", Office: "Senate", ReportType: "periodic_transaction", FilingDate: filingDate, DocumentURL: "https://efdsearch.senate.gov/search/view/ptr/retry-senate/", RawMetadata: []byte(`{}`), RetryClaimToken: "token"}}, nil
				}}
			},
		},
		{
			name:  "oge",
			retry: func(r *Runner, ctx context.Context) (int, error) { return r.retryOGE(ctx, 1) },
			make: func() *Runner {
				return &Runner{
					OGE:       NewOGEClient(server.URL, time.Second),
					Extractor: runnerTestExtractor{},
					claimDueRetries: func(context.Context, string, time.Time, int, time.Duration) ([]store.PoliticalDisclosureReport, error) {
						return []store.PoliticalDisclosureReport{{Source: "oge", ExternalID: "retry-oge", Chamber: "executive", FilerName: "Jane Official", Office: "White House", ReportType: "periodic_transaction", FilingDate: filingDate, DocumentURL: server.URL + "/retry-oge.pdf", RawMetadata: []byte(`{}`), RetryClaimToken: "token"}}, nil
					},
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			persistenceErr := errors.New("temporary database failure")
			saves := 0
			runner := test.make()
			runner.saveRetryResult = func(context.Context, store.PoliticalDisclosureReport, string) error {
				saves++
				return persistenceErr
			}
			runner.releaseRetryClaims = func(context.Context, string) error { return nil }
			processed, err := test.retry(runner, context.Background())
			if processed != 1 || !errors.Is(err, persistenceErr) || saves != 1 {
				t.Fatalf("processed=%d saves=%d err=%v; want one save and persistence error", processed, saves, err)
			}
		})
	}
}

func TestRetryLoopPropagatesCancellationBetweenClaims(t *testing.T) {
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = &fixtureSenateTransport{fetchBody: []byte(`<table><tr><th>Transaction Date</th><th>Ticker</th><th>Asset Name</th><th>Transaction Type</th><th>Amount</th></tr><tr><td>08/08/2026</td><td>MSFT</td><td>Microsoft</td><td>Purchase</td><td>$1,001 - $15,000</td></tr></table>`)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	filingDate := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	runner := &Runner{Senate: client,
		claimDueRetries: func(context.Context, string, time.Time, int, time.Duration) ([]store.PoliticalDisclosureReport, error) {
			return []store.PoliticalDisclosureReport{
				{Source: "senate", ExternalID: "first", FilerName: "Jane Senator", FilingDate: filingDate, DocumentURL: "https://efdsearch.senate.gov/search/view/ptr/first/", RawMetadata: []byte(`{}`), RetryClaimToken: "token"},
				{Source: "senate", ExternalID: "second", FilerName: "Jane Senator", FilingDate: filingDate, DocumentURL: "https://efdsearch.senate.gov/search/view/ptr/second/", RawMetadata: []byte(`{}`), RetryClaimToken: "token"},
			}, nil
		},
		releaseRetryClaims: func(context.Context, string) error { return nil },
	}
	saves := 0
	runner.saveRetryResult = func(context.Context, store.PoliticalDisclosureReport, string) error {
		saves++
		cancel()
		return nil
	}
	processed, err := runner.retrySenate(ctx, 2)
	if processed != 1 || !errors.Is(err, context.Canceled) || saves != 1 {
		t.Fatalf("processed=%d saves=%d err=%v", processed, saves, err)
	}
}

func TestRetryLoopChecksCancellationBeforeSuccessfulReturn(t *testing.T) {
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = &fixtureSenateTransport{fetchBody: []byte(`<table><tr><th>Transaction Date</th><th>Ticker</th><th>Asset Name</th><th>Transaction Type</th><th>Amount</th></tr><tr><td>08/08/2026</td><td>MSFT</td><td>Microsoft</td><td>Purchase</td><td>$1,001 - $15,000</td></tr></table>`)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &Runner{Senate: client,
		claimDueRetries: func(context.Context, string, time.Time, int, time.Duration) ([]store.PoliticalDisclosureReport, error) {
			return []store.PoliticalDisclosureReport{{Source: "senate", ExternalID: "only", FilerName: "Jane Senator", FilingDate: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), DocumentURL: "https://efdsearch.senate.gov/search/view/ptr/only/", RawMetadata: []byte(`{}`), RetryClaimToken: "token"}}, nil
		},
		releaseRetryClaims: func(context.Context, string) error { return nil },
		saveRetryResult: func(context.Context, store.PoliticalDisclosureReport, string) error {
			cancel()
			return nil
		},
	}
	processed, err := runner.retrySenate(ctx, 1)
	if processed != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
}

func TestSyncChecksCancellationBeforeSuccessfulReturn(t *testing.T) {
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = &fixtureSenateTransport{
		searchBody: []byte(`{"recordsFiltered":1,"data":[["Jane","Member","Office","<a href='/search/view/ptr/only/'>PTR</a>","08/10/2026"]]}`),
		fetchBody:  []byte(`<table><tr><th>Transaction Date</th><th>Ticker</th><th>Asset Name</th><th>Transaction Type</th><th>Amount</th></tr><tr><td>08/08/2026</td><td>MSFT</td><td>Microsoft</td><td>Purchase</td><td>$1,001 - $15,000</td></tr></table>`),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &Runner{Senate: client,
		reportTerminal: func(context.Context, string, string) (bool, error) { return false, nil },
		persistReport: func(context.Context, Report) error {
			cancel()
			return nil
		},
	}
	processed, err := runner.syncSenate(ctx,
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), 1)
	if processed != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
}

func TestSyncRangeAndRunOnceShortCircuitCancellation(t *testing.T) {
	index := "Prefix\tLast\tFirst\tSuffix\tFilingType\tStateDst\tYear\tFilingDate\tDocID\n" +
		"Hon.\tMember\tJane\t\tP\tCA01\t2026\t08/10/2026\tfirst\n" +
		"Hon.\tMember\tJane\t\tP\tCA01\t2026\t08/10/2026\tsecond\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(index))
	}))
	defer server.Close()
	newRunner := func(cancel context.CancelFunc, senateTransport *countingSenateTransport) *Runner {
		senate, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		senate.Transport = senateTransport
		return &Runner{
			Cfg:   config.DisclosuresConfig{LookbackDays: 0, MaxReportsPerSync: 2},
			House: &HouseClient{BaseURL: server.URL, HTTP: server.Client()}, Senate: senate,
			Now: func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) },
			claimDueRetries: func(context.Context, string, time.Time, int, time.Duration) ([]store.PoliticalDisclosureReport, error) {
				return nil, nil
			},
			reportTerminal: func(context.Context, string, string) (bool, error) {
				cancel()
				return true, nil
			},
		}
	}

	t.Run("SyncRange", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		senateTransport := &countingSenateTransport{searchBody: []byte(`{"recordsFiltered":0,"data":[]}`)}
		runner := newRunner(cancel, senateTransport)
		_, err := runner.SyncRange(ctx, time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), 2)
		if !errors.Is(err, context.Canceled) || senateTransport.searchCalls != 0 {
			t.Fatalf("err=%v senate calls=%d", err, senateTransport.searchCalls)
		}
	})

	t.Run("RunOnce", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		senateTransport := &countingSenateTransport{searchBody: []byte(`{"recordsFiltered":0,"data":[]}`)}
		runner := newRunner(cancel, senateTransport)
		if err := runner.RunOnce(ctx); !errors.Is(err, context.Canceled) || senateTransport.searchCalls != 0 {
			t.Fatalf("err=%v senate calls=%d", err, senateTransport.searchCalls)
		}
	})
}

func TestSourceSyncReturnsCancellationBetweenFilings(t *testing.T) {
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)

	t.Run("House", func(t *testing.T) {
		index := "Prefix\tLast\tFirst\tSuffix\tFilingType\tStateDst\tYear\tFilingDate\tDocID\n" +
			"Hon.\tMember\tJane\t\tP\tCA01\t2026\t08/10/2026\tfirst\n" +
			"Hon.\tMember\tJane\t\tP\tCA01\t2026\t08/10/2026\tsecond\n"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(index))
		}))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runner := &Runner{House: &HouseClient{BaseURL: server.URL, HTTP: server.Client()}, reportTerminal: func(context.Context, string, string) (bool, error) {
			cancel()
			return true, nil
		}}
		processed, err := runner.syncHouse(ctx, from, until, 2)
		if processed != 0 || !errors.Is(err, context.Canceled) {
			t.Fatalf("processed=%d err=%v", processed, err)
		}
	})

	t.Run("Senate", func(t *testing.T) {
		client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		client.Transport = &fixtureSenateTransport{searchBody: []byte(`{"recordsFiltered":2,"data":[["Jane","Member","Office","<a href='/search/view/ptr/first/'>PTR</a>","08/10/2026"],["Jane","Member","Office","<a href='/search/view/ptr/second/'>PTR</a>","08/10/2026"]]}`)}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runner := &Runner{Senate: client, reportTerminal: func(context.Context, string, string) (bool, error) {
			cancel()
			return true, nil
		}}
		processed, err := runner.syncSenate(ctx, from, until, 2)
		if processed != 0 || !errors.Is(err, context.Canceled) {
			t.Fatalf("processed=%d err=%v", processed, err)
		}
	})

	t.Run("OGE", func(t *testing.T) {
		var server *httptest.Server
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"recordsTotal":2,"data":[{"type":"<a href='` + server.URL + `/first.pdf'>278 Transaction</a>","name":"Jane Official","docDate":"2026-08-10"},{"type":"<a href='` + server.URL + `/second.pdf'>278 Transaction</a>","name":"Jane Official","docDate":"2026-08-10"}]}`))
		}))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runner := &Runner{OGE: NewOGEClient(server.URL, time.Second), reportTerminal: func(context.Context, string, string) (bool, error) {
			cancel()
			return true, nil
		}}
		processed, err := runner.syncOGE(ctx, from, until, 2)
		if processed != 0 || !errors.Is(err, context.Canceled) {
			t.Fatalf("processed=%d err=%v", processed, err)
		}
	})
}
