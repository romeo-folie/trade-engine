package scanner

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestPremarketEvidenceFailureCountIncludesErrorsAndOmissions(t *testing.T) {
	result := data.PremarketBatchResult{
		Evidence: map[string]data.PremarketEvidence{"GOOD": {Symbol: "GOOD"}},
		Failures: map[string]error{"FAILED": errors.New("provider failure")},
	}
	if got := premarketEvidenceFailureCount(
		[]string{"GOOD", "FAILED", "OMITTED"}, result,
	); got != 2 {
		t.Fatalf("failure count = %d; want 2", got)
	}
}

func TestPremarketShortlistUsesPriorCloseAndIsDeterministic(t *testing.T) {
	bar := func(close float64) *data.Candle {
		return &data.Candle{Close: close}
	}
	marketCap := 500.0
	members := map[string]store.SwingUniverseMember{
		"BIG": {Ticker: "BIG", MarketCapMM: &marketCap},
		"MID": {Ticker: "MID", MarketCapMM: &marketCap},
		"LOW": {Ticker: "LOW", MarketCapMM: &marketCap},
	}
	snapshots := map[string]data.StockSnapshotEvidence{
		"BIG": {Symbol: "BIG", LatestTradePrice: 11, PreviousDailyBar: bar(10)},
		"MID": {Symbol: "MID", LatestTradePrice: 10.5, PreviousDailyBar: bar(10)},
		"LOW": {Symbol: "LOW", LatestTradePrice: 10.1, PreviousDailyBar: bar(10)},
	}
	cfg := config.Defaults().Scanner.PremarketEpisodic
	got := premarketShortlist(snapshots, members, cfg)
	if len(got) != 2 || got[0] != "BIG" || got[1] != "MID" {
		t.Fatalf("shortlist = %v", got)
	}
}

func TestPremarketConfigCompatibilitySeparatesOperationalAndOutputChanges(t *testing.T) {
	base := config.Defaults().Scanner.PremarketEpisodic
	_, baseHash, err := premarketConfigEvidence(base)
	if err != nil {
		t.Fatal(err)
	}
	operational := base
	operational.Enabled = true
	operational.Checkpoints = []string{"08:30"}
	operational.BatchSize = 50
	operational.RunLeaseMinutes = 30
	_, gotHash, err := premarketConfigEvidence(operational)
	if err != nil {
		t.Fatal(err)
	}
	if gotHash != baseHash {
		t.Fatalf("operational switches changed compatibility hash")
	}
	for name, mutate := range map[string]func(*config.PremarketEpisodicConfig){
		"decision threshold": func(c *config.PremarketEpisodicConfig) {
			c.MinGapPct++
		},
		"hard cutoff": func(c *config.PremarketEpisodicConfig) {
			c.HardCutoff = "09:24"
		},
		"promotion policy": func(c *config.PremarketEpisodicConfig) {
			c.PromoteCandidates = true
			c.MaxPromotionsPerRun = 3
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			_, changedHash, err := premarketConfigEvidence(changed)
			if err != nil {
				t.Fatal(err)
			}
			if changedHash == baseHash {
				t.Fatalf("%s did not change compatibility hash", name)
			}
		})
	}
}

func TestPremarketRunnerStoreCandidatesCapsPromotion(t *testing.T) {
	cfg := config.Defaults().Scanner.PremarketEpisodic
	cfg.PromoteCandidates = true
	cfg.MaxPromotionsPerRun = 1
	runner := PremarketEpisodicRunner{Cfg: cfg}
	rows, err := runner.storeCandidates(
		"run", "universe",
		[]PremarketEpisodicEvaluation{
			{
				Ticker: "A", Outcome: "qualified", Qualified: true, Score: .9,
				Catalyst: PremarketCatalyst{Status: "confirmed_absent"},
				Features: PremarketEpisodicFeatures{
					LastPrice: 10, FloatStatus: "unavailable",
				},
			},
			{
				Ticker: "B", Outcome: "qualified", Qualified: true, Score: .8,
				Catalyst: PremarketCatalyst{Status: "confirmed"},
				Features: PremarketEpisodicFeatures{
					LastPrice: 11, FloatStatus: "unavailable",
				},
			},
		},
		map[string]json.RawMessage{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].PromotionStatus != "pending" ||
		rows[1].PromotionStatus != "not_requested" {
		t.Fatalf("promotion states = %s, %s",
			rows[0].PromotionStatus, rows[1].PromotionStatus)
	}
	if rows[0].Rank == nil || *rows[0].Rank != 1 {
		t.Fatalf("rank = %v", rows[0].Rank)
	}
}
