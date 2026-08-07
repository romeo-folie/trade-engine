package store

import (
	"context"
	"math"
	"testing"
)

const ledgerTickerP = "ZZLEDG" // prefix; cleanup deletes by it

// seedLedgerTrade inserts one closed trade. When attributed is true the trade is
// wired to a signal + analysis (so it reaches entryBaseCTE); when false it is
// inserted with a NULL signal_id, mirroring a position adopted from the broker
// by the startup reconciler (#328) — the exact shape that used to vanish from
// the Performance headline.
//
// ensemble is written verbatim into analyses.ensemble_results; pass "" to leave
// the column NULL, which is what the zero-analyst path produces.
func seedLedgerTrade(ctx context.Context, t *testing.T, ticker string, pnl float64, attributed bool, ensemble string) {
	t.Helper()

	var signalID any // NULL for unattributed rows
	if attributed {
		var ensembleArg any
		if ensemble != "" {
			ensembleArg = ensemble
		}
		var analysisID string
		if err := GetDB().QueryRow(ctx, `
			INSERT INTO analyses (ticker, strategy, action, confidence, prompt_hash,
			                      model_provider, model_name, ensemble_results)
			VALUES ($1, 'momentum', 'BUY', 0.700, 'h', 'p', 'm', $2::jsonb)
			RETURNING id`, ticker, ensembleArg).Scan(&analysisID); err != nil {
			t.Fatalf("insert analysis: %v", err)
		}
		var sid string
		if err := GetDB().QueryRow(ctx, `
			INSERT INTO signals (ticker, action, strategy, signal_price, confidence, analysis_ids)
			VALUES ($1, 'BUY', 'momentum', 100, 0.700, ARRAY[$2::uuid])
			RETURNING id`, ticker, analysisID).Scan(&sid); err != nil {
			t.Fatalf("insert signal: %v", err)
		}
		signalID = sid
	}

	if _, err := GetDB().Exec(ctx, `
		INSERT INTO trades (signal_id, ticker, direction, strategy, entry_price,
		                    quantity, status, pnl, pnl_pct)
		VALUES ($1, $2, 'long'::trade_direction, 'momentum', 100, 1, 'closed', $3, $3)`,
		signalID, ticker, pnl); err != nil {
		t.Fatalf("insert trade: %v", err)
	}
}

func cleanupLedgerFixtures(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM trades   WHERE ticker LIKE $1`,
		`DELETE FROM signals  WHERE ticker LIKE $1`,
		`DELETE FROM analyses WHERE ticker LIKE $1`,
	} {
		if _, err := GetDB().Exec(ctx, q, ledgerTickerP+"%"); err != nil {
			t.Fatalf("cleanup fixtures (%s): %v", q, err)
		}
	}
}

// deref reads a nullable money column as 0, so a snapshot taken against an
// empty test DB compares cleanly against one taken after seeding.
func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// round2 absorbs float64 drift from subtracting two values Postgres already
// rounded to cents, so exact-equality assertions on deltas are safe.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// TestClosedLedgerSurfacesAgree is the regression guard for the defect where the
// Portfolio and Performance screens reported different win rates (27.7% vs
// 25.9%) while portfolio.go claimed they matched. Performance reached the ledger
// through `JOIN signals`, which drops broker-adopted trades; Portfolio counted
// the ledger directly. Both now call GetClosedLedgerStats, so an unattributed
// trade must move both numbers identically or neither.
func TestClosedLedgerSurfacesAgree(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set; store integration tests skipped")
	}
	ctx := context.Background()
	cleanupLedgerFixtures(ctx, t)
	defer cleanupLedgerFixtures(ctx, t)

	// Snapshot first: the test DB is shared, so every assertion below is a delta
	// against whatever rows other tests left behind.
	before, err := GetEntryPerformance(ctx)
	if err != nil {
		t.Fatalf("GetEntryPerformance (before): %v", err)
	}

	// One attributed winner and two unattributed losers. Before the split, the
	// Performance headline saw only the winner (100% win rate) while Portfolio
	// saw all three (33%) — the divergence this test pins down. The three P&Ls
	// are chosen so the ledger delta (+30) and the attributed delta (+100)
	// differ: if the headline silently reverts to summing the attributed
	// population, the ledger delta becomes +100 and this test fails.
	seedLedgerTrade(ctx, t, ledgerTickerP+"A", 100, true, `[{"Role":"r","Model":"m","Response":{"action":"BUY","confidence":0.7}}]`)
	seedLedgerTrade(ctx, t, ledgerTickerP+"B", -40, false, "")
	seedLedgerTrade(ctx, t, ledgerTickerP+"C", -30, false, "")

	portfolio, err := GetPortfolioStats(ctx)
	if err != nil {
		t.Fatalf("GetPortfolioStats: %v", err)
	}
	perf, err := GetEntryPerformance(ctx)
	if err != nil {
		t.Fatalf("GetEntryPerformance: %v", err)
	}

	// The money, which is what actually broke. Win-rate equality alone is
	// tautological now that both surfaces call the same helper — it can only
	// catch the call being removed, not the arithmetic regressing.
	if got := round2(deref(perf.Overall.NetPnL) - deref(before.Overall.NetPnL)); got != 30 {
		t.Errorf("headline net moved %+.2f, want +30.00 — the headline must sum the whole ledger (+100-40-30), not just the attributed +100",
			got)
	}
	if got := round2(deref(perf.Overall.AttributedNetPnL) - deref(before.Overall.AttributedNetPnL)); got != 100 {
		t.Errorf("attributed net moved %+.2f, want +100.00 — the breakdown population must exclude the two unattributed rows",
			got)
	}
	if got := round2(deref(perf.Concentration.NetPnL) - deref(before.Concentration.NetPnL)); got != 30 {
		t.Errorf("concentration net moved %+.2f, want +30.00 — it sits under NET REALIZED and must span the same book",
			got)
	}
	if perf.Overall.AttributedN > perf.Overall.N {
		t.Errorf("AttributedN (%d) exceeds N (%d) — the two populations must share closedLedgerPredicate",
			perf.Overall.AttributedN, perf.Overall.N)
	}

	if portfolio.WinRatePct != perf.Overall.WinRatePct {
		t.Errorf("win rate disagrees: Portfolio %.1f%% vs Performance %.1f%% — the two surfaces must share GetClosedLedgerStats",
			portfolio.WinRatePct, perf.Overall.WinRatePct)
	}
	if portfolio.TotalTrades != perf.Overall.N {
		t.Errorf("trade count disagrees: Portfolio %d vs Performance %d",
			portfolio.TotalTrades, perf.Overall.N)
	}

	// The headline must count the unattributed rows; the breakdowns must not.
	if perf.Overall.N-perf.Overall.AttributedN < 2 {
		t.Errorf("headline N (%d) and attributed N (%d) should differ by at least the 2 unattributed fixtures",
			perf.Overall.N, perf.Overall.AttributedN)
	}
	var breakdownN int
	for _, b := range perf.ByDirection {
		breakdownN += b.N
	}
	if breakdownN != perf.Overall.AttributedN {
		t.Errorf("breakdown rows sum to %d, want AttributedN %d — breakdowns must stay on the attributed population",
			breakdownN, perf.Overall.AttributedN)
	}
}

// TestEntryPerformanceSurvivesNonArrayEnsemble pins the failure mode where a
// single analyses row whose ensemble_results is not a JSON array took down the
// entire Performance endpoint. jsonb_array_length raises on a scalar or object
// ("cannot get array length of a scalar"), and a NULL length surfaced as a NULL
// group label that failed the row scan — so one bad row 500'd every cut, not
// just the analyst-count table.
func TestEntryPerformanceSurvivesNonArrayEnsemble(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set; store integration tests skipped")
	}
	ctx := context.Background()
	cleanupLedgerFixtures(ctx, t)
	defer cleanupLedgerFixtures(ctx, t)

	for _, tc := range []struct {
		name     string
		ensemble string
	}{
		{"NULL column (zero-analyst insert path)", ""},
		{"JSON null scalar (legacy rows)", `null`},
		{"JSON object instead of array", `{"Role":"r"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanupLedgerFixtures(ctx, t)

			// Deltas, not absolutes. Asserting `n >= 1` would also pass if the
			// seeded row were dropped entirely and some unrelated row happened
			// to sit in the "0" bucket — which is exactly what the tempting
			// alternative fix (filtering with `WHERE jsonb_typeof(...)='array'`
			// instead of coercing) would do: endpoint stops erroring, row
			// silently vanishes from AttributedN. That is the same population
			// bug in miniature, and it must not ship green.
			before, err := GetEntryPerformance(ctx)
			if err != nil {
				t.Fatalf("GetEntryPerformance (before): %v", err)
			}
			seedLedgerTrade(ctx, t, ledgerTickerP+"X", 25, true, tc.ensemble)

			ep, err := GetEntryPerformance(ctx)
			if err != nil {
				t.Fatalf("GetEntryPerformance must tolerate %s, got: %v", tc.name, err)
			}
			// A missing ensemble means zero analyst results, so the row belongs
			// in the "0" bucket rather than a NULL label or a dropped trade.
			if got := bucketsByLabel(ep.ByAnalysts)["0"].N - bucketsByLabel(before.ByAnalysts)["0"].N; got != 1 {
				t.Errorf("ByAnalysts[\"0\"] moved by %d, want exactly +1 — the seeded row must be bucketed as zero analysts, not dropped", got)
			}
			if got := ep.Overall.AttributedN - before.Overall.AttributedN; got != 1 {
				t.Errorf("AttributedN moved by %d, want exactly +1 — a bad ensemble must not remove the trade from the attributed population", got)
			}
		})
	}
}
