package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Fixtures use deliberately distinctive role/model/ticker names rather than real
// ones ("risk_manager", "claude-haiku-4-5"). GetEntryPerformance aggregates over
// every closed trade in the database and takes no filter, so a test that asserted
// on real names would depend on whatever store_integration_test.go left behind —
// and clearing that shared state would cascade into orders and
// scanner_engine_outcomes. The grouping under test is name-agnostic, so unique
// names buy order-independence for free.
const (
	testRole    = "ztest_role"
	testModelA  = "ztest-model-a"
	testModelB  = "ztest-model-b"
	testTickerP = "ZZTEST" // prefix; cleanup deletes by it
)

// seedRoleVoteTrade inserts one closed trade wired to an analysis whose
// ensemble_results carries the given per-role votes, mirroring what
// AnalyzeSpecialist persists (Go field names, no JSON tags — see
// analysis.AnalystResult).
//
// votes is (role, model, action) triples; dir/pnl describe the executed trade.
func seedRoleVoteTrade(ctx context.Context, t *testing.T, ticker, dir string, pnl float64, votes [][3]string) {
	t.Helper()

	var ensemble strings.Builder
	ensemble.WriteString("[")
	for i, v := range votes {
		if i > 0 {
			ensemble.WriteString(",")
		}
		fmt.Fprintf(&ensemble, `{"Role":%q,"Provider":"p","Model":%q,"Response":{"action":%q,"confidence":0.7}}`,
			v[0], v[1], v[2])
	}
	ensemble.WriteString("]")

	var analysisID string
	if err := GetDB().QueryRow(ctx, `
		INSERT INTO analyses (ticker, strategy, action, confidence, prompt_hash,
		                      model_provider, model_name, ensemble_results)
		VALUES ($1, 'momentum', 'BUY', 0.700, 'h', 'p', 'm', $2::jsonb)
		RETURNING id`, ticker, ensemble.String()).Scan(&analysisID); err != nil {
		t.Fatalf("insert analysis: %v", err)
	}

	var signalID string
	if err := GetDB().QueryRow(ctx, `
		INSERT INTO signals (ticker, action, strategy, signal_price, confidence, analysis_ids)
		VALUES ($1, 'BUY', 'momentum', 100, 0.700, ARRAY[$2::uuid])
		RETURNING id`, ticker, analysisID).Scan(&signalID); err != nil {
		t.Fatalf("insert signal: %v", err)
	}

	if _, err := GetDB().Exec(ctx, `
		INSERT INTO trades (signal_id, ticker, direction, strategy, entry_price,
		                    quantity, status, pnl, pnl_pct)
		VALUES ($1, $2, $3::trade_direction, 'momentum', 100, 1, 'closed', $4, $4)`,
		signalID, ticker, dir, pnl); err != nil {
		t.Fatalf("insert trade: %v", err)
	}
}

// cleanupRoleVoteFixtures removes only this test's rows, leaving every other
// test's data (and the orders / scanner_engine_outcomes that FK into it) intact.
func cleanupRoleVoteFixtures(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM trades  WHERE ticker LIKE $1`,
		`DELETE FROM signals WHERE ticker LIKE $1`,
		`DELETE FROM analyses WHERE ticker LIKE $1`,
	} {
		if _, err := GetDB().Exec(ctx, q, testTickerP+"%"); err != nil {
			t.Fatalf("cleanup fixtures (%s): %v", q, err)
		}
	}
}

func bucketsByLabel(rows []EntryPerfBucket) map[string]EntryPerfBucket {
	m := make(map[string]EntryPerfBucket, len(rows))
	for _, r := range rows {
		m[r.Label] = r
	}
	return m
}

// TestEntryRoleAccuracySplitsByModel is the regression guard for the defect this
// cut exists to prevent: analysis.specialist_roles is remappable, so one role
// accumulates votes from every model ever assigned to it. The angle-only cut
// averages across that swap and silently reports a blended hit rate; the
// role·model cut must keep them separate.
func TestEntryRoleAccuracySplitsByModel(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set; store integration tests skipped")
	}
	ctx := context.Background()
	cleanupRoleVoteFixtures(ctx, t)
	defer cleanupRoleVoteFixtures(ctx, t)

	// Same role, two models, opposite outcomes. modelA backed the winning long;
	// modelB backed a long that lost. Blended that is 50%; split it is 100/0.
	seedRoleVoteTrade(ctx, t, testTickerP+"A", "long", 10, [][3]string{{testRole, testModelA, "BUY"}})
	seedRoleVoteTrade(ctx, t, testTickerP+"B", "long", -10, [][3]string{{testRole, testModelB, "BUY"}})

	ep, err := GetEntryPerformance(ctx)
	if err != nil {
		t.Fatalf("GetEntryPerformance: %v", err)
	}

	// Angle-only cut: one row, blended across both models.
	if got := bucketsByLabel(ep.ByRole)[testRole]; got.N != 2 || got.WinRatePct != 50 {
		t.Errorf("ByRole[%s] = n%d/%.1f%%, want n2/50.0%% (the blend this cut makes visible)",
			testRole, got.N, got.WinRatePct)
	}

	// Role·model cut: the two models must not be averaged together.
	rm := bucketsByLabel(ep.ByRoleModel)
	for _, tc := range []struct {
		model   string
		wantPct float64
	}{
		{testModelA, 100},
		{testModelB, 0},
	} {
		label := testRole + " · " + tc.model
		got, ok := rm[label]
		if !ok {
			t.Errorf("ByRoleModel missing %q; models must not be collapsed into the role", label)
			continue
		}
		if got.N != 1 || got.WinRatePct != tc.wantPct {
			t.Errorf("ByRoleModel[%s] = n%d/%.1f%%, want n1/%.1f%%",
				label, got.N, got.WinRatePct, tc.wantPct)
		}
	}
}

// TestEntryRoleAccuracyDissentAndAbstention pins the accuracy semantics the
// label depends on: a dissent is correct when the trade lost, and HOLD/WAIT
// votes are excluded entirely rather than counted as wrong.
func TestEntryRoleAccuracyDissentAndAbstention(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set; store integration tests skipped")
	}
	ctx := context.Background()
	cleanupRoleVoteFixtures(ctx, t)
	defer cleanupRoleVoteFixtures(ctx, t)

	// One losing long. Dissenter said SELL → correct. Backer said BUY → wrong.
	// Abstainer said HOLD → excluded from the cut entirely.
	dissent, backer, abstain := testRole+"_dissent", testRole+"_backer", testRole+"_abstain"
	seedRoleVoteTrade(ctx, t, testTickerP+"C", "long", -5, [][3]string{
		{dissent, testModelA, "SELL"},
		{backer, testModelA, "BUY"},
		{abstain, testModelA, "HOLD"},
	})

	ep, err := GetEntryPerformance(ctx)
	if err != nil {
		t.Fatalf("GetEntryPerformance: %v", err)
	}
	rm := bucketsByLabel(ep.ByRoleModel)

	for role, wantPct := range map[string]float64{
		dissent: 100, // dissented from a losing long
		backer:  0,   // backed a losing long
	} {
		label := role + " · " + testModelA
		got, ok := rm[label]
		if !ok {
			t.Errorf("ByRoleModel missing %q", label)
			continue
		}
		if got.WinRatePct != wantPct {
			t.Errorf("ByRoleModel[%s] = %.1f%%, want %.1f%%", label, got.WinRatePct, wantPct)
		}
	}
	if _, ok := rm[abstain+" · "+testModelA]; ok {
		t.Error("HOLD abstention was counted; it must be excluded from the cut")
	}

	// Net/avg-P&L stay NULL on role rows — attributing the trade's P&L to a role
	// that dissented would book the loss against a correct call (Codex #390).
	for _, r := range ep.ByRoleModel {
		if r.NetPnL != nil || r.AvgPnLPct != nil {
			t.Errorf("ByRoleModel[%s] carries P&L (net=%v avg=%v); role cuts must leave both NULL",
				r.Label, r.NetPnL, r.AvgPnLPct)
		}
	}
}
