package orb

import (
	"context"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/depth"
	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

// EquitiesCandidateScanner scans for gap movers and scores them with the LLM.
type EquitiesCandidateScanner struct {
	AlpacaClient *data.AlpacaClient
	Analyzer     *analysis.Analyzer
	News         data.NewsProvider
	Cfg          config.ORBConfig
	Now          func() time.Time
}

// ScanCandidates fetches gap movers and LLM-scores each one, returning all
// scored candidates (including "skip" actions). Filtering and sorting are
// handled by the caller (ORBScheduler.tickScanning).
func (s *EquitiesCandidateScanner) ScanCandidates(ctx context.Context) ([]ScoredCandidate, error) {
	gaps, err := data.FetchGapMovers(ctx, s.AlpacaClient, s.Cfg.GapThresholdPct)
	if err != nil {
		return nil, fmt.Errorf("scan gap movers: %w", err)
	}
	if len(gaps) == 0 {
		return nil, nil
	}

	// Cap how many candidates get LLM-scored. FetchGapMovers already ranks by
	// |gap%| descending, so this keeps the strongest movers and bounds the
	// number of (rate-limited) model calls — a large gap universe otherwise
	// sprays dozens of calls into the pre-open window and stalls on the
	// provider's per-minute quota, finishing after the range opens (candidates
	// then discarded).
	maxScan := s.Cfg.MaxScanCandidates
	if maxScan <= 0 {
		maxScan = defaultMaxScanCandidates
	}
	if len(gaps) > maxScan {
		fmt.Printf("orb: gap universe %d → scoring top %d by gap%%\n", len(gaps), maxScan)
		gaps = gaps[:maxScan]
	}

	nowFn := s.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	var scored []ScoredCandidate
	for _, gap := range gaps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		var newsItems []prompts.ORBNewsItem
		if s.News != nil {
			since := nowFn().Add(-time.Duration(s.Cfg.NewsLookbackHours) * time.Hour)
			rawNews, newsErr := s.News.FetchNews(ctx, gap.Ticker, since)
			if newsErr != nil {
				fmt.Printf("orb: news fetch %s: %v\n", gap.Ticker, newsErr)
			} else {
				if len(rawNews) > maxNewsPerTicker {
					rawNews = rawNews[:maxNewsPerTicker]
				}
				for _, n := range rawNews {
					sentiment := "NEUTRAL"
					if n.SentimentScore > sentimentPositiveThreshold {
						sentiment = "POSITIVE"
					} else if n.SentimentScore < sentimentNegativeThreshold {
						sentiment = "NEGATIVE"
					}
					newsItems = append(newsItems, prompts.ORBNewsItem{
						Headline:    n.Headline,
						Sentiment:   sentiment,
						PublishedAt: n.PublishedAt,
					})
				}
			}
		}

		sysPrompt, userMsg := prompts.BuildORBPrompt(
			prompts.ORBCandidate{
				Ticker:        gap.Ticker,
				GapPct:        gap.GapPct,
				PreMarketVol:  gap.PreMarketVol,
				PreMarketLast: gap.PreMarketLast,
				PriorClose:    gap.PriorClose,
			},
			newsItems,
			"",
		)

		resp, meta, analyzeErr := s.Analyzer.Analyze(ctx, analysis.AnalysisRequest{
			Ticker:             gap.Ticker,
			CustomSystemPrompt: sysPrompt,
			CustomUserMessage:  userMsg,
		}, depth.ImpactScore{Score: 1.0, Depth: 3})
		if analyzeErr != nil {
			fmt.Printf("orb: LLM scoring %s failed: %v\n", gap.Ticker, analyzeErr)
			continue
		}

		result := parseORBResult(meta.RawResponse, resp)
		scored = append(scored, ScoredCandidate{Gap: gap, Score: result})
	}

	return scored, nil
}
