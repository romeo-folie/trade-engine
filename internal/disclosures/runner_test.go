package disclosures

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

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
