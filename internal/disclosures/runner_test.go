package disclosures

import (
	"context"
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
			got := distributeSyncBudget(test.limit, test.enabled...)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("budgets = %v, want %v", got, test.want)
			}
		})
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
