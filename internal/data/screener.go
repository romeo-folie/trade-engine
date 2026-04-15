package data

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
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
	ChangePerc float64 `json:"percent_change"`
	Price      float64 `json:"price"`
	Change     float64 `json:"change"` // absolute dollar change
}

type alpacaMoversResponse struct {
	Gainers []alpacaMover `json:"gainers"`
	Losers  []alpacaMover `json:"losers"`
}

// FetchGapMovers returns stocks gapping >= minGapPct (absolute value),
// sorted by absolute gap descending. Uses the Alpaca Screener API.
func FetchGapMovers(ctx context.Context, client *AlpacaClient, minGapPct float64) ([]GapCandidate, error) {
	params := url.Values{}
	params.Set("top", "50")
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
				PriorClose:    m.Price - m.Change,
			})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return math.Abs(candidates[i].GapPct) > math.Abs(candidates[j].GapPct)
	})

	// Enrich candidates with pre-market volume from the snapshots endpoint.
	// The screener/movers endpoint does not include volume data.
	if len(candidates) > 0 {
		tickers := make([]string, len(candidates))
		for i, c := range candidates {
			tickers[i] = c.Ticker
		}
		vols, err := fetchSnapshotVolumes(ctx, client, tickers)
		if err != nil {
			// Non-fatal: volume is supplementary context for the LLM.
			fmt.Printf("orb: snapshot volumes: %v\n", err)
		} else {
			for i := range candidates {
				candidates[i].PreMarketVol = vols[candidates[i].Ticker]
			}
		}
	}

	return candidates, nil
}

// MostActiveCandidate represents a stock with unusually high trading activity.
type MostActiveCandidate struct {
	Ticker     string
	Volume     int64
	TradeCount int64
	Price      float64
}

type alpacaMostActivesResponse struct {
	MostActives []alpacaMostActive `json:"most_actives"`
}

type alpacaMostActive struct {
	Symbol     string  `json:"symbol"`
	Volume     int64   `json:"volume"`
	TradeCount int64   `json:"trade_count"`
	Price      float64 `json:"price"`
}

// FetchMostActive returns the top most-active stocks by volume using the Alpaca screener API.
func FetchMostActive(ctx context.Context, client *AlpacaClient, top int) ([]MostActiveCandidate, error) {
	if top <= 0 {
		top = 20
	}
	params := url.Values{}
	params.Set("by", "volume")
	params.Set("top", fmt.Sprintf("%d", top))

	resp, err := client.get(ctx, "/v1beta1/screener/stocks/most-actives", params)
	if err != nil {
		return nil, fmt.Errorf("fetch most actives: %w", err)
	}
	defer resp.Body.Close()

	var activesResp alpacaMostActivesResponse
	if err := json.NewDecoder(resp.Body).Decode(&activesResp); err != nil {
		return nil, fmt.Errorf("decode most actives: %w", err)
	}

	candidates := make([]MostActiveCandidate, 0, len(activesResp.MostActives))
	for _, m := range activesResp.MostActives {
		candidates = append(candidates, MostActiveCandidate{
			Ticker:     m.Symbol,
			Volume:     m.Volume,
			TradeCount: m.TradeCount,
			Price:      m.Price,
		})
	}
	return candidates, nil
}

// alpacaSnapshot represents the relevant subset of an Alpaca stock snapshot.
type alpacaSnapshot struct {
	MinuteBar *alpacaBar `json:"minuteBar"`
	DailyBar  *alpacaBar `json:"dailyBar"`
}

// fetchSnapshotVolumes fetches the daily bar volume for the given tickers
// via the Alpaca multi-snapshot endpoint in a single call.
func fetchSnapshotVolumes(ctx context.Context, client *AlpacaClient, tickers []string) (map[string]int64, error) {
	params := url.Values{}
	params.Set("symbols", strings.Join(tickers, ","))
	params.Set("feed", "iex")

	resp, err := client.get(ctx, "/v2/stocks/snapshots", params)
	if err != nil {
		return nil, fmt.Errorf("fetch snapshots: %w", err)
	}
	defer resp.Body.Close()

	var snapshots map[string]alpacaSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snapshots); err != nil {
		return nil, fmt.Errorf("decode snapshots: %w", err)
	}

	vols := make(map[string]int64, len(snapshots))
	for sym, snap := range snapshots {
		if snap.DailyBar != nil {
			vols[sym] = snap.DailyBar.Volume
		}
	}
	return vols, nil
}
