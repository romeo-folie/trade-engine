package data

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAlpacaPremarketSnapshotsRequireSIPAndSurfaceEntitlement(t *testing.T) {
	var feed string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		feed = r.URL.Query().Get("feed")
		http.Error(w, `{"message":"subscription does not permit querying recent SIP data"}`,
			http.StatusForbidden)
	}))
	defer server.Close()
	client := NewAlpacaClient("key", "secret", server.URL)
	client.minInterval = 0
	provider := NewAlpacaPremarketClient(client)
	_, err := provider.FetchPremarketSnapshots(
		context.Background(), []string{"AAPL"}, 100, "sip",
	)
	if !errors.Is(err, ErrPremarketEntitlement) {
		t.Fatalf("entitlement error = %v", err)
	}
	if feed != "sip" {
		t.Fatalf("requested feed = %q", feed)
	}
	_, err = provider.FetchPremarketSnapshots(
		context.Background(), []string{"AAPL"}, 100, "iex",
	)
	if !errors.Is(err, ErrPremarketEntitlement) {
		t.Fatalf("IEX downgrade must fail explicitly: %v", err)
	}
}

func TestSplitPremarketBarsUsesSameClockAndBoundsCurrentSession(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	session := time.Date(2026, 7, 27, 0, 0, 0, 0, loc)
	checkpoint := time.Date(2026, 7, 27, 8, 0, 0, 0, loc)
	bar := func(day, hour, minute int, volume int64) PremarketBar {
		return PremarketBar{Candle: Candle{
			Timestamp: time.Date(2026, 7, day, hour, minute, 0, 0, loc),
			Volume:    volume,
		}}
	}
	current, history := splitPremarketBars([]PremarketBar{
		bar(24, 7, 59, 100),
		bar(24, 8, 1, 9_000), // after the same-clock cutoff: excluded
		bar(27, 7, 59, 200),
		bar(27, 8, 1, 8_000),  // incomplete/future bar: excluded
		bar(27, 9, 30, 9_000), // regular hours: excluded
	}, session, checkpoint, 20)
	if len(current) != 1 || current[0].Volume != 200 {
		t.Fatalf("current bars = %+v", current)
	}
	if len(history) != 1 || history[0] != 100 {
		t.Fatalf("same-clock historical volumes = %v", history)
	}
}

func TestPremarketEntitlementErrorIsOperatorReadable(t *testing.T) {
	err := fmtPremarketEntitlementForTest()
	if !strings.Contains(err.Error(), "consolidated premarket data entitlement") {
		t.Fatalf("error is not actionable: %v", err)
	}
}

func fmtPremarketEntitlementForTest() error {
	return errors.Join(ErrPremarketEntitlement, errors.New("Alpaca SIP access denied"))
}
