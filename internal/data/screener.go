package data

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
)

// GapCandidate represents a stock gapping significantly from its prior close.
type GapCandidate struct {
	Ticker        string
	GapPct        float64 // % change from prior close (positive = gap up)
	PreMarketVol  int64
	PreMarketLast float64
	PriorClose    float64
}

type alpacaMover struct {
	Symbol     string  `json:"symbol"`
	ChangePerc float64 `json:"change_percent"`
	Price      float64 `json:"price"`
	Volume     int64   `json:"volume"`
	PrevClose  float64 `json:"previous_close"`
}

type alpacaMoversResponse struct {
	Gainers []alpacaMover `json:"gainers"`
	Losers  []alpacaMover `json:"losers"`
}

// FetchGapMovers returns stocks gapping >= minGapPct (absolute value),
// sorted by absolute gap descending. Uses the Alpaca Screener API.
func FetchGapMovers(ctx context.Context, client *AlpacaClient, minGapPct float64) ([]GapCandidate, error) {
	params := url.Values{}
	params.Set("top", "20")
	resp, err := client.get(ctx, "/v1beta1/screener/stocks/movers", params)
	if err != nil {
		return nil, fmt.Errorf("fetch movers: %w", err)
	}
	defer resp.Body.Close()

	var moversResp alpacaMoversResponse
	if err := json.NewDecoder(resp.Body).Decode(&moversResp); err != nil {
		return nil, fmt.Errorf("decode movers: %w", err)
	}

	var candidates []GapCandidate
	all := make([]alpacaMover, 0, len(moversResp.Gainers)+len(moversResp.Losers))
	all = append(all, moversResp.Gainers...)
	all = append(all, moversResp.Losers...)
	for _, m := range all {
		if math.Abs(m.ChangePerc) >= minGapPct {
			candidates = append(candidates, GapCandidate{
				Ticker:        m.Symbol,
				GapPct:        m.ChangePerc,
				PreMarketLast: m.Price,
				PreMarketVol:  m.Volume,
				PriorClose:    m.PrevClose,
			})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return math.Abs(candidates[i].GapPct) > math.Abs(candidates[j].GapPct)
	})
	return candidates, nil
}
