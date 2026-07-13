package orb

import (
	"errors"
	"fmt"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis"
)

// orbVote builds one analyst result carrying an ORB-schema raw response.
func orbVote(action string, confidence float64) analysis.AnalystResult {
	return analysis.AnalystResult{
		Meta: analysis.ModelCallMeta{RawResponse: fmt.Sprintf(`{"action":%q,"confidence":%g}`, action, confidence)},
	}
}

// orbVoteErr is a failed analyst — excluded from both numerator and denominator.
func orbVoteErr() analysis.AnalystResult {
	return analysis.AnalystResult{Err: errors.New("provider down")}
}

func TestAggregateORBScore(t *testing.T) {
	const thr = 0.68
	cases := []struct {
		name       string
		votes      []analysis.AnalystResult
		wantAction string
		wantConv   float64 // -1 = don't assert
	}{
		{
			name:       "majority trade, mean above threshold",
			votes:      []analysis.AnalystResult{orbVote("trade", 0.80), orbVote("trade", 0.70), orbVote("skip", 0.90)},
			wantAction: "trade", wantConv: 0.75, // mean of the two trade-voters; skip does not dilute
		},
		{
			name:       "single reachable analyst says trade",
			votes:      []analysis.AnalystResult{orbVote("trade", 0.72)},
			wantAction: "trade", wantConv: 0.72,
		},
		{
			name:       "one trade vote of three — no quorum",
			votes:      []analysis.AnalystResult{orbVote("trade", 0.80), orbVote("skip", 0.5), orbVote("skip", 0.5)},
			wantAction: "skip", wantConv: -1,
		},
		{
			name:       "trade wins vote but mean below threshold",
			votes:      []analysis.AnalystResult{orbVote("trade", 0.60), orbVote("trade", 0.62)},
			wantAction: "skip", wantConv: 0.61, // conviction preserved even though gated out
		},
		{
			name:       "even split — tie goes to trade",
			votes:      []analysis.AnalystResult{orbVote("trade", 0.70), orbVote("skip", 0.9)},
			wantAction: "trade", wantConv: 0.70,
		},
		{
			name:       "all skip",
			votes:      []analysis.AnalystResult{orbVote("skip", 0.9), orbVote("skip", 0.8)},
			wantAction: "skip", wantConv: -1,
		},
		{
			name:       "failed analysts excluded from the vote",
			votes:      []analysis.AnalystResult{orbVoteErr(), orbVote("trade", 0.50), orbVote("skip", 0.9)},
			wantAction: "skip", wantConv: -1, // 1 trade / 2 successful = tie → trade wins vote, but 0.50 < 0.68 → skip
		},
		{
			name:       "no successful analysts",
			votes:      []analysis.AnalystResult{orbVoteErr(), orbVoteErr()},
			wantAction: "skip", wantConv: -1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := aggregateORBScore(tc.votes, thr)
			if got.Action != tc.wantAction {
				t.Errorf("action = %q; want %q", got.Action, tc.wantAction)
			}
			if tc.wantConv >= 0 && (got.Conviction < tc.wantConv-1e-9 || got.Conviction > tc.wantConv+1e-9) {
				t.Errorf("conviction = %.4f; want %.4f", got.Conviction, tc.wantConv)
			}
		})
	}
}

func TestEffectiveScanThreshold(t *testing.T) {
	if got := effectiveScanThreshold(0); got != defaultScanConfidenceThreshold {
		t.Errorf("unset → %.2f; want default %.2f", got, defaultScanConfidenceThreshold)
	}
	if got := effectiveScanThreshold(0.55); got != 0.55 {
		t.Errorf("configured → %.2f; want 0.55", got)
	}
}
