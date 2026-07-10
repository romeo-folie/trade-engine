package data

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchGapMovers(t *testing.T) {
	// Use raw JSON matching the real Alpaca API field names to catch
	// deserialization mismatches (the previous Go-struct-based test
	// silently masked a wrong json tag).
	const moversJSON = `{
		"gainers": [
			{"symbol":"AAPL","percent_change":5.2,"change":8.93,"price":180.0},
			{"symbol":"TSLA","percent_change":2.1,"change":5.15,"price":250.0},
			{"symbol":"NVDA","percent_change":4.0,"change":19.23,"price":500.0}
		],
		"losers": [
			{"symbol":"META","percent_change":-6.0,"change":-19.15,"price":300.0},
			{"symbol":"AMZN","percent_change":-1.5,"change":-2.74,"price":180.0}
		]
	}`

	const snapshotsJSON = `{
		"AAPL": {"dailyBar":{"t":"2026-04-05T04:00:00Z","o":171.0,"h":181.0,"l":170.0,"c":180.0,"v":1500000,"vw":176.0}},
		"META": {"dailyBar":{"t":"2026-04-05T04:00:00Z","o":318.0,"h":320.0,"l":298.0,"c":300.0,"v":2200000,"vw":310.0}},
		"NVDA": {"dailyBar":{"t":"2026-04-05T04:00:00Z","o":482.0,"h":505.0,"l":480.0,"c":500.0,"v":3100000,"vw":495.0}}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1beta1/screener/stocks/movers":
			w.Write([]byte(moversJSON))
		case "/v2/stocks/snapshots":
			w.Write([]byte(snapshotsJSON))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	client := NewAlpacaClient("test-key", "test-secret", srv.URL)
	client.minInterval = 0 // disable throttle for tests

	candidates, err := FetchGapMovers(context.Background(), client, GapScreen{MinGapPct: 3.0})
	if err != nil {
		t.Fatalf("FetchGapMovers: %v", err)
	}

	// Should include AAPL (5.2%), META (-6.0%), NVDA (4.0%) — not TSLA (2.1%) or AMZN (-1.5%)
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}

	// Ranked liquidity-first (|gap%| × log10 dollar-volume). Here META has both
	// the biggest gap and high dollar-volume, so the order matches a gap-only
	// sort: META (6.0, $660M), AAPL (5.2, $270M), NVDA (4.0, $1.55B).
	if candidates[0].Ticker != "META" {
		t.Errorf("candidates[0].Ticker = %q, want META", candidates[0].Ticker)
	}
	if candidates[1].Ticker != "AAPL" {
		t.Errorf("candidates[1].Ticker = %q, want AAPL", candidates[1].Ticker)
	}
	if candidates[2].Ticker != "NVDA" {
		t.Errorf("candidates[2].Ticker = %q, want NVDA", candidates[2].Ticker)
	}

	// Verify gap values
	if math.Abs(candidates[0].GapPct-(-6.0)) > 0.01 {
		t.Errorf("candidates[0].GapPct = %f, want -6.0", candidates[0].GapPct)
	}

	// Verify PriorClose is derived correctly (price - change)
	if math.Abs(candidates[1].PriorClose-171.07) > 0.01 {
		t.Errorf("candidates[1].PriorClose = %f, want ~171.07", candidates[1].PriorClose)
	}

	// Verify PreMarketVol is enriched from snapshots
	if candidates[0].PreMarketVol != 2200000 { // META
		t.Errorf("candidates[0].PreMarketVol = %d, want 2200000", candidates[0].PreMarketVol)
	}
	if candidates[1].PreMarketVol != 1500000 { // AAPL
		t.Errorf("candidates[1].PreMarketVol = %d, want 1500000", candidates[1].PreMarketVol)
	}
}

func TestFetchGapMoversHighThreshold(t *testing.T) {
	const moversJSON = `{
		"gainers": [
			{"symbol":"AAPL","percent_change":2.0,"change":3.53,"price":180.0}
		],
		"losers": []
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(moversJSON))
	}))
	defer srv.Close()

	client := NewAlpacaClient("test-key", "test-secret", srv.URL)
	client.minInterval = 0

	candidates, err := FetchGapMovers(context.Background(), client, GapScreen{MinGapPct: 5.0})
	if err != nil {
		t.Fatalf("FetchGapMovers: %v", err)
	}
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates with 5%% threshold, got %d", len(candidates))
	}
}

// TestFetchGapMoversPriceFloor verifies the price floor drops sub-threshold
// penny stocks / warrants before they can be scored, which was the root cause
// of the equities ORB scoring only un-tradeable junk.
func TestFetchGapMoversPriceFloor(t *testing.T) {
	// WARR is a warrant popping 120% but priced at $0.30; BIGCO gaps a modest 4%
	// at $80. A gap%-only rank would put WARR first; the price floor removes it.
	const moversJSON = `{
		"gainers": [
			{"symbol":"WARR","percent_change":120.0,"change":0.16,"price":0.30},
			{"symbol":"BIGCO","percent_change":4.0,"change":3.08,"price":80.0}
		],
		"losers": []
	}`
	const snapshotsJSON = `{
		"BIGCO": {"dailyBar":{"t":"2026-04-05T04:00:00Z","o":77.0,"h":81.0,"l":76.0,"c":80.0,"v":900000,"vw":79.0}}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1beta1/screener/stocks/movers":
			w.Write([]byte(moversJSON))
		case "/v2/stocks/snapshots":
			w.Write([]byte(snapshotsJSON))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	client := NewAlpacaClient("test-key", "test-secret", srv.URL)
	client.minInterval = 0

	candidates, err := FetchGapMovers(context.Background(), client, GapScreen{MinGapPct: 3.0, MinPrice: 5.0})
	if err != nil {
		t.Fatalf("FetchGapMovers: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate after $5 price floor, got %d", len(candidates))
	}
	if candidates[0].Ticker != "BIGCO" {
		t.Errorf("candidates[0].Ticker = %q, want BIGCO (WARR should be floored out)", candidates[0].Ticker)
	}
}

// TestFetchGapMoversLiquidityRanking verifies that, for candidates with the same
// gap, the more liquid name ranks first — i.e. dollar-volume is the tie-breaker
// baked into the |gap%| × log10(dollar-volume) blend.
func TestFetchGapMoversLiquidityRanking(t *testing.T) {
	// Both gap 5%. LIQ trades $2B (5×log10(2e9)≈46.5); THIN trades $1.8M
	// (5×log10(1.8e6)≈31.3). LIQ should rank first despite the identical gap.
	const moversJSON = `{
		"gainers": [
			{"symbol":"THIN","percent_change":5.0,"change":0.29,"price":6.0},
			{"symbol":"LIQ","percent_change":5.0,"change":9.52,"price":200.0}
		],
		"losers": []
	}`
	const snapshotsJSON = `{
		"THIN": {"dailyBar":{"t":"2026-04-05T04:00:00Z","o":5.7,"h":6.1,"l":5.6,"c":6.0,"v":300000,"vw":5.9}},
		"LIQ": {"dailyBar":{"t":"2026-04-05T04:00:00Z","o":190.0,"h":201.0,"l":189.0,"c":200.0,"v":10000000,"vw":197.0}}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1beta1/screener/stocks/movers":
			w.Write([]byte(moversJSON))
		case "/v2/stocks/snapshots":
			w.Write([]byte(snapshotsJSON))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	client := NewAlpacaClient("test-key", "test-secret", srv.URL)
	client.minInterval = 0

	candidates, err := FetchGapMovers(context.Background(), client, GapScreen{MinGapPct: 3.0, MinPrice: 5.0, RankByLiquidity: true})
	if err != nil {
		t.Fatalf("FetchGapMovers: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0].Ticker != "LIQ" {
		t.Errorf("candidates[0].Ticker = %q, want LIQ (liquidity should beat a bigger gap on a thin name)", candidates[0].Ticker)
	}
}

// TestFetchGapMoversRawPathKeepsGapOrder guards the swing-scanner path: with
// RankByLiquidity=false, a lower-gap but far more liquid name must NOT displace a
// bigger gainer — ordering stays |gap%|-desc regardless of dollar volume.
func TestFetchGapMoversRawPathKeepsGapOrder(t *testing.T) {
	// BIG gaps 8% at $6 trading ~$1.8M; MEGA gaps 4% at $200 trading $2B. Under
	// liquidity ranking MEGA would win; on the raw path BIG (bigger gap) must lead.
	const moversJSON = `{
		"gainers": [
			{"symbol":"BIG","percent_change":8.0,"change":0.44,"price":6.0},
			{"symbol":"MEGA","percent_change":4.0,"change":7.69,"price":200.0}
		],
		"losers": []
	}`
	const snapshotsJSON = `{
		"BIG": {"dailyBar":{"t":"2026-04-05T04:00:00Z","o":5.6,"h":6.1,"l":5.5,"c":6.0,"v":300000,"vw":5.9}},
		"MEGA": {"dailyBar":{"t":"2026-04-05T04:00:00Z","o":193.0,"h":201.0,"l":192.0,"c":200.0,"v":10000000,"vw":197.0}}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1beta1/screener/stocks/movers":
			w.Write([]byte(moversJSON))
		case "/v2/stocks/snapshots":
			w.Write([]byte(snapshotsJSON))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	client := NewAlpacaClient("test-key", "test-secret", srv.URL)
	client.minInterval = 0

	// Raw path: no floors, no liquidity ranking (swing-scanner default).
	raw, err := FetchGapMovers(context.Background(), client, GapScreen{MinGapPct: 3.0})
	if err != nil {
		t.Fatalf("FetchGapMovers: %v", err)
	}
	if len(raw) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(raw))
	}
	if raw[0].Ticker != "BIG" {
		t.Errorf("raw[0].Ticker = %q, want BIG (raw path must stay gap%%-ordered, not liquidity-ranked)", raw[0].Ticker)
	}
}

// TestFetchGapMoversDollarVolumeFloor verifies the dollar-volume floor drops a
// confirmed-illiquid name but keeps a name whose volume is unknown (fail open).
func TestFetchGapMoversDollarVolumeFloor(t *testing.T) {
	// DEAD: $6 × 10k shares = $60k dollar-volume → below a $1M floor → dropped.
	// GHOST: no snapshot entry → volume unknown → kept (fail open).
	const moversJSON = `{
		"gainers": [
			{"symbol":"DEAD","percent_change":8.0,"change":0.44,"price":6.0},
			{"symbol":"GHOST","percent_change":5.0,"change":0.48,"price":10.0}
		],
		"losers": []
	}`
	const snapshotsJSON = `{
		"DEAD": {"dailyBar":{"t":"2026-04-05T04:00:00Z","o":5.6,"h":6.1,"l":5.5,"c":6.0,"v":10000,"vw":5.9}}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1beta1/screener/stocks/movers":
			w.Write([]byte(moversJSON))
		case "/v2/stocks/snapshots":
			w.Write([]byte(snapshotsJSON))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	client := NewAlpacaClient("test-key", "test-secret", srv.URL)
	client.minInterval = 0

	candidates, err := FetchGapMovers(context.Background(), client, GapScreen{MinGapPct: 3.0, MinPrice: 5.0, MinDollarVolume: 1_000_000})
	if err != nil {
		t.Fatalf("FetchGapMovers: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate after $1M dollar-volume floor, got %d", len(candidates))
	}
	if candidates[0].Ticker != "GHOST" {
		t.Errorf("candidates[0].Ticker = %q, want GHOST (DEAD floored, GHOST kept as unknown-volume)", candidates[0].Ticker)
	}
}
