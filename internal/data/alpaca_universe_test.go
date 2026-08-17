package data

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	_ AssetUniverseProvider     = (*AlpacaUniverseClient)(nil)
	_ BulkStockSnapshotProvider = (*AlpacaUniverseClient)(nil)
	_ BulkDailyBarsProvider     = (*AlpacaUniverseClient)(nil)
)

func newTestAlpacaUniverseClient(serverURL string) *AlpacaUniverseClient {
	client := newTestAlpacaClient(serverURL)
	universe := NewAlpacaUniverseClient(client, serverURL, AlpacaUniverseFeeds{
		Snapshots: "delayed_sip", Historical: "sip",
	})
	universe.now = func() time.Time {
		return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	}
	return universe
}

func TestAlpacaUniverseFetchStockAssetsDeterministicDedupe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/assets" {
			t.Fatalf("path = %q, want /v2/assets", r.URL.Path)
		}
		if got := r.URL.Query().Get("status"); got != "" {
			t.Errorf("status = %q, want unfiltered asset-master request", got)
		}
		if got := r.URL.Query().Get("asset_class"); got != "us_equity" {
			t.Errorf("asset_class = %q, want us_equity", got)
		}
		_ = json.NewEncoder(w).Encode([]alpacaAsset{
			{ID: "z", Class: "us_equity", Status: "active", Symbol: " msft ", Name: "Microsoft", Exchange: "nasdaq", Tradable: true},
			{ID: "b", Class: "us_equity", Status: "active", Symbol: "AAPL", Name: "Duplicate", Exchange: "NASDAQ"},
			{ID: "a", Class: "us_equity", Status: "active", Symbol: "aapl", Name: "Apple", Exchange: "nasdaq", Tradable: true, Attributes: []string{"has_options", "ipo"}},
			{ID: "inactive", Class: "us_equity", Status: "inactive", Symbol: "OLD"},
			{ID: "crypto", Class: "crypto", Status: "active", Symbol: "BTCUSD"},
		})
	}))
	defer srv.Close()

	got, err := newTestAlpacaUniverseClient(srv.URL).FetchStockAssets(context.Background())
	if err != nil {
		t.Fatalf("FetchStockAssets: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("assets = %+v, want three", got)
	}
	if got[0].Symbol != "AAPL" || got[0].AssetID != "a" ||
		got[1].Symbol != "MSFT" || got[2].Symbol != "OLD" ||
		got[2].Status != "inactive" {
		t.Fatalf("assets not deterministic/deduplicated: %+v", got)
	}
	if got[0].Provider != "alpaca" || got[0].ObservedAt.IsZero() {
		t.Fatalf("missing provider evidence: %+v", got[0])
	}
	if strings.Join(got[0].Attributes, ",") != "has_options,ipo" {
		t.Fatalf("attributes = %v, want sorted", got[0].Attributes)
	}
}

func TestRequestableAlpacaSymbolsQuarantinesMalformedAssetMasterValues(t *testing.T) {
	got, failures := requestableAlpacaSymbols([]string{
		"A", "123", "BRK.B", "BF-B", "0029900E0", "TOO_LONG9", "BAD/SLASH",
	})
	if strings.Join(got, ",") != "A,123,BRK.B,BF-B" {
		t.Fatalf("requestable = %v, want valid symbols preserved", got)
	}
	if len(failures) != 3 || failures["0029900E0"] == nil ||
		failures["TOO_LONG9"] == nil || failures["BAD/SLASH"] == nil {
		t.Fatalf("failures = %+v, want all malformed symbols", failures)
	}
}

func TestAlpacaUniverseSnapshotsBatchBoundPartialFailureAndDedupe(t *testing.T) {
	var mu sync.Mutex
	var requests [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		symbols := strings.Split(r.URL.Query().Get("symbols"), ",")
		if got := r.URL.Query().Get("feed"); got != "delayed_sip" {
			t.Errorf("snapshot feed = %q, want delayed_sip", got)
		}
		mu.Lock()
		requests = append(requests, append([]string(nil), symbols...))
		mu.Unlock()
		if len(symbols) > 2 {
			t.Fatalf("batch exceeded caller bound: %v", symbols)
		}
		if len(symbols) == 1 && symbols[0] == "C" {
			http.Error(w, "bad batch", http.StatusBadRequest)
			return
		}
		payload := make(map[string]alpacaSnapshot, len(symbols))
		for _, symbol := range symbols {
			payload[strings.ToLower(symbol)] = alpacaSnapshot{
				DailyBar: &alpacaBar{
					Timestamp: "2026-07-24T04:00:00Z", Close: 100, Volume: 500,
				},
				LatestTrade: &alpacaTrade{
					Timestamp: "2026-07-24T19:59:59.123Z", Price: 101,
				},
				LatestQuote: &alpacaQuote{
					Timestamp: "2026-07-24T19:59:59.456Z", BidPrice: 100.9, AskPrice: 101.1,
				},
			}
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	got, err := newTestAlpacaUniverseClient(srv.URL).FetchStockSnapshots(
		context.Background(), []string{" c ", "B", "a", "A"}, 2,
	)
	if err != nil {
		t.Fatalf("FetchStockSnapshots: %v", err)
	}
	if len(got.Snapshots) != 2 || got.Snapshots["A"].DailyBar.Volume != 500 {
		t.Fatalf("snapshots = %+v", got.Snapshots)
	}
	if got.Snapshots["B"].LatestTradePrice != 101 {
		t.Fatalf("latest trade evidence = %+v", got.Snapshots["B"])
	}
	if got.Snapshots["B"].LatestBidPrice != 100.9 ||
		got.Snapshots["B"].LatestAskPrice != 101.1 ||
		got.Snapshots["B"].LatestQuoteAt.IsZero() {
		t.Fatalf("latest quote evidence = %+v", got.Snapshots["B"])
	}
	if got.Failures["C"] == nil {
		t.Fatalf("missing explicit C failure: %+v", got.Failures)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || strings.Join(requests[0], ",") != "A,B" ||
		strings.Join(requests[1], ",") != "C" {
		t.Fatalf("requests = %v, want [A,B] then [C]", requests)
	}
}

func TestAlpacaUniverseSnapshotsCancellationMarksAllSymbols(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := newTestAlpacaUniverseClient("http://unused").FetchStockSnapshots(
		ctx, []string{"B", "A"}, 1,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if !errors.Is(got.Failures["A"], context.Canceled) ||
		!errors.Is(got.Failures["B"], context.Canceled) {
		t.Fatalf("failures = %+v, want every unprocessed symbol canceled", got.Failures)
	}
}

func TestAlpacaUniverseSnapshotsIsolatesInvalidSymbolFromBatch(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		symbols := r.URL.Query().Get("symbols")
		requests = append(requests, symbols)
		if strings.Contains(symbols, "BAD") {
			http.Error(w, `{"message":"invalid symbol: BAD"}`, http.StatusBadRequest)
			return
		}
		payload := make(map[string]alpacaSnapshot)
		for _, symbol := range strings.Split(symbols, ",") {
			payload[symbol] = alpacaSnapshot{DailyBar: &alpacaBar{
				Timestamp: "2026-07-24T04:00:00Z", Close: 100, Volume: 500,
			}}
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	got, err := newTestAlpacaUniverseClient(srv.URL).FetchStockSnapshots(
		context.Background(), []string{"C", "BAD", "A"}, 3,
	)
	if err != nil {
		t.Fatalf("FetchStockSnapshots: %v", err)
	}
	if len(got.Snapshots) != 2 || got.Snapshots["A"].DailyBar == nil ||
		got.Snapshots["C"].DailyBar == nil {
		t.Fatalf("snapshots = %+v, want valid symbols preserved", got.Snapshots)
	}
	if len(got.Failures) != 1 || got.Failures["BAD"] == nil {
		t.Fatalf("failures = %+v, want only BAD", got.Failures)
	}
	if len(requests) < 4 || requests[0] != "A,BAD,C" {
		t.Fatalf("requests = %v, want initial mixed batch followed by isolation", requests)
	}
}

func TestAlpacaUniverseDailyBarsPaginationBatchBoundAndDedupe(t *testing.T) {
	var mu sync.Mutex
	requests := make([]string, 0, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		symbols := r.URL.Query().Get("symbols")
		token := r.URL.Query().Get("page_token")
		if got := r.URL.Query().Get("feed"); got != "sip" {
			t.Errorf("historical feed = %q, want sip", got)
		}
		mu.Lock()
		requests = append(requests, symbols+"|"+token)
		mu.Unlock()
		if len(strings.Split(symbols, ",")) > 2 {
			t.Fatalf("batch exceeded caller bound: %s", symbols)
		}
		if r.URL.Query().Get("adjustment") != "split" ||
			r.URL.Query().Get("timeframe") != "1Day" {
			t.Errorf("missing split-adjusted daily params: %s", r.URL.RawQuery)
		}

		switch symbols + "|" + token {
		case "A,B|":
			_ = json.NewEncoder(w).Encode(alpacaBulkBarsResponse{
				Bars: map[string][]alpacaBar{
					"A": {
						{Timestamp: "2026-07-23T04:00:00Z", Close: 10},
						{Timestamp: "2026-07-24T04:00:00Z", Close: 11},
					},
				},
				NextPageToken: "next",
			})
		case "A,B|next":
			_ = json.NewEncoder(w).Encode(alpacaBulkBarsResponse{
				Bars: map[string][]alpacaBar{
					"A": {{Timestamp: "2026-07-24T04:00:00Z", Close: 12}},
					"B": {{Timestamp: "2026-07-24T04:00:00Z", Close: 20}},
				},
			})
		case "C|":
			_ = json.NewEncoder(w).Encode(alpacaBulkBarsResponse{
				Bars: map[string][]alpacaBar{
					"C": {{Timestamp: "2026-07-24T04:00:00Z", Close: 30}},
				},
			})
		default:
			t.Fatalf("unexpected request %s|%s", symbols, token)
		}
	}))
	defer srv.Close()

	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	got, err := newTestAlpacaUniverseClient(srv.URL).FetchDailyBars(
		context.Background(), []string{"C", "A", "B", "a"}, from, to, 2,
	)
	if err != nil {
		t.Fatalf("FetchDailyBars: %v", err)
	}
	if len(got.Failures) != 0 || len(got.Bars) != 3 {
		t.Fatalf("result = %+v", got)
	}
	if len(got.Bars["A"]) != 2 || got.Bars["A"][1].Close != 12 {
		t.Fatalf("A bars not deduplicated last-page-wins: %+v", got.Bars["A"])
	}
	if !got.Bars["A"][0].Timestamp.Before(got.Bars["A"][1].Timestamp) {
		t.Fatalf("A bars not sorted: %+v", got.Bars["A"])
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(requests, ",") != "A,B|,A,B|next,C|" {
		t.Fatalf("requests = %v", requests)
	}
}

func TestAlpacaUniverseSIPHistoryClampsRecentEndWithoutSnapshotFeed(t *testing.T) {
	now := time.Date(2026, 8, 17, 20, 30, 0, 0, time.UTC)
	wantEnd := now.Add(-AlpacaSIPRecentDataDelay).Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("feed"); got != "sip" {
			t.Errorf("historical feed = %q, want sip", got)
		}
		if got := r.URL.Query().Get("end"); got != wantEnd {
			t.Errorf("historical end = %q, want entitlement-safe %q", got, wantEnd)
		}
		_ = json.NewEncoder(w).Encode(alpacaBulkBarsResponse{Bars: map[string][]alpacaBar{
			"A": {{Timestamp: "2026-08-17T04:00:00Z", Close: 10}},
		}})
	}))
	defer srv.Close()

	client := NewAlpacaUniverseClient(newTestAlpacaClient(srv.URL), srv.URL,
		AlpacaUniverseFeeds{Historical: "sip"})
	client.now = func() time.Time { return now }
	result, err := client.FetchDailyBars(
		context.Background(), []string{"A"}, now.AddDate(0, 0, -30), now, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Bars["A"]) != 1 {
		t.Fatalf("bars = %+v, want one", result.Bars)
	}
}

func TestAlpacaUniverseDailyBarsPartialBatchFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		symbols := r.URL.Query().Get("symbols")
		if symbols == "C" {
			http.Error(w, `{"message":"invalid symbol: C"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(alpacaBulkBarsResponse{
			Bars: map[string][]alpacaBar{
				"A": {{Timestamp: "2026-07-24T04:00:00Z", Close: 10}},
				"B": {{Timestamp: "2026-07-24T04:00:00Z", Close: 20}},
			},
		})
	}))
	defer srv.Close()

	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	got, err := newTestAlpacaUniverseClient(srv.URL).FetchDailyBars(
		context.Background(), []string{"A", "B", "C"}, from, to, 2,
	)
	if err != nil {
		t.Fatalf("FetchDailyBars: %v", err)
	}
	if len(got.Bars) != 2 || got.Failures["C"] == nil {
		t.Fatalf("partial result = %+v", got)
	}
}

func TestAlpacaUniverseDailyBarsTransportFailureReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	got, err := newTestAlpacaUniverseClient(srv.URL).FetchDailyBars(
		context.Background(), []string{"A", "B"}, from, to, 2,
	)
	if err == nil || !strings.Contains(err.Error(), "daily-bars batch") {
		t.Fatalf("transport error = %v, result = %+v", err, got)
	}
}

func TestAlpacaUniverseDailyBarsCancellationMarksCurrentBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		http.Error(w, "request canceled", http.StatusRequestTimeout)
	}))
	defer srv.Close()
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	got, err := newTestAlpacaUniverseClient(srv.URL).FetchDailyBars(
		ctx, []string{"B", "A"}, from, to, 2,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if !errors.Is(got.Failures["A"], context.Canceled) ||
		!errors.Is(got.Failures["B"], context.Canceled) {
		t.Fatalf("failures = %+v, want every symbol canceled", got.Failures)
	}
}

func TestAlpacaUniverseDailyBarsIsolatesInvalidSymbolFromBatch(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		symbols := r.URL.Query().Get("symbols")
		requests = append(requests, symbols)
		if strings.Contains(symbols, "BAD") {
			http.Error(w, `{"message":"invalid symbol: BAD"}`, http.StatusBadRequest)
			return
		}
		bars := make(map[string][]alpacaBar)
		for _, symbol := range strings.Split(symbols, ",") {
			bars[symbol] = []alpacaBar{{Timestamp: "2026-07-24T04:00:00Z", Close: 10}}
		}
		_ = json.NewEncoder(w).Encode(alpacaBulkBarsResponse{Bars: bars})
	}))
	defer srv.Close()

	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	got, err := newTestAlpacaUniverseClient(srv.URL).FetchDailyBars(
		context.Background(), []string{"C", "BAD", "A"}, from, to, 3,
	)
	if err != nil {
		t.Fatalf("FetchDailyBars: %v", err)
	}
	if len(got.Bars) != 2 || len(got.Bars["A"]) != 1 || len(got.Bars["C"]) != 1 {
		t.Fatalf("bars = %+v, want valid symbols preserved", got.Bars)
	}
	if len(got.Failures) != 1 || got.Failures["BAD"] == nil {
		t.Fatalf("failures = %+v, want only BAD", got.Failures)
	}
	if len(requests) < 4 || requests[0] != "A,BAD,C" {
		t.Fatalf("requests = %v, want initial mixed batch followed by isolation", requests)
	}
}
