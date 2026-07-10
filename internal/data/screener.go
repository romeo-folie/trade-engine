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

// GapScreen parameterises the gap-mover screen. Zero-valued floors and
// RankByLiquidity=false are disabled, so callers that only want the raw gap
// universe (e.g. the swing scanner) can pass GapScreen{MinGapPct: x} and get the
// historical gap%-ordered behaviour unchanged.
type GapScreen struct {
	MinGapPct float64 // minimum |gap %| to qualify (e.g. 3.0)
	MinPrice  float64 // drop candidates trading below this (0 = no floor); kills sub-$5 penny stocks / warrants
	// MinDollarVolume drops candidates whose price×volume is below this (0 = no
	// floor). Best-effort — only applied when volume is known. CAUTION: the volume
	// comes from Alpaca's iex feed (a single exchange carrying a small fraction of
	// consolidated volume), so a hard floor here can drop genuinely liquid names
	// that trade little on IEX specifically. Left off by default for that reason;
	// only enable it on a SIP-backed deployment.
	MinDollarVolume float64
	// RankByLiquidity ranks results by |gap%| × log10(dollarVolume) instead of raw
	// |gap%| descending. ORB wants this (liquidity-first among floor-clearing
	// names); raw callers like the swing scanner leave it false to preserve their
	// documented "top movers by gap%" ordering.
	RankByLiquidity bool
}

// FetchGapMovers returns stocks gapping >= p.MinGapPct (absolute value) that
// clear the price and dollar-volume floors, ranked liquidity-first. Uses the
// Alpaca Screener API.
//
// Ranking rewards liquidity, not raw gap magnitude: Alpaca's movers list sorted
// by percentage is dominated by sub-$1 penny stocks and warrants (a $0.30 warrant
// popping 120% outranks a mega-cap gapping 6%), which are un-tradeable and get
// skipped downstream. Callers cap the returned slice to a top-N for LLM scoring,
// so a gap%-only rank guarantees only junk gets scored. Here we (1) drop names
// below MinPrice, (2) drop names below MinDollarVolume where volume is known, and
// (3) rank by |gap%| × log10(dollarVolume) so a real gap on a liquid name beats a
// bigger gap on a dead one. Floors and ranking degrade gracefully (fail open) when
// snapshot volume is unavailable.
func FetchGapMovers(ctx context.Context, client *AlpacaClient, p GapScreen) ([]GapCandidate, error) {
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
		if math.Abs(m.ChangePerc) < p.MinGapPct {
			continue
		}
		// Price floor: applied first because price is always present on the
		// movers payload (no extra call) and is the single most effective junk
		// filter — it eliminates penny stocks and warrants outright.
		if p.MinPrice > 0 && m.Price < p.MinPrice {
			continue
		}
		candidates = append(candidates, GapCandidate{
			Ticker:        m.Symbol,
			GapPct:        m.ChangePerc,
			PreMarketLast: m.Price,
			PriorClose:    m.Price - m.Change,
		})
	}

	// Enrich candidates with volume from the snapshots endpoint. The
	// screener/movers endpoint does not include volume data, and we need it
	// BEFORE ranking (liquidity-weighted) and for the dollar-volume floor.
	volKnown := false
	if len(candidates) > 0 {
		tickers := make([]string, len(candidates))
		for i, c := range candidates {
			tickers[i] = c.Ticker
		}
		vols, volErr := fetchSnapshotVolumes(ctx, client, tickers)
		if volErr != nil {
			// Non-fatal: fall back to gap-magnitude ranking with no volume floor.
			fmt.Printf("orb: snapshot volumes: %v\n", volErr)
		} else {
			volKnown = true
			for i := range candidates {
				candidates[i].PreMarketVol = vols[candidates[i].Ticker]
			}
		}
	}

	// Dollar-volume floor: best-effort. Only drop a name we can positively
	// confirm is illiquid (volume known and > 0 but below the floor). A missing
	// or zero snapshot volume is treated as "unknown" and kept — the ranking
	// still deprioritises it.
	if volKnown && p.MinDollarVolume > 0 {
		kept := candidates[:0]
		for _, c := range candidates {
			dollarVol := c.PreMarketLast * float64(c.PreMarketVol)
			if c.PreMarketVol > 0 && dollarVol < p.MinDollarVolume {
				continue
			}
			kept = append(kept, c)
		}
		candidates = kept
	}

	// Rank. Raw callers (RankByLiquidity=false) keep the historical |gap%|-desc
	// order. ORB ranks liquidity-first: score = |gap%| × log10(dollarVolume+10) —
	// the +10 keeps unknown-volume names (dollarVol=0 → log10(10)=1 → score=|gap%|)
	// ranked among themselves by gap, while liquid names get a multiplier up to ~9.
	// The dollar-volume term uses the same (iex) volume as the floor, but as a soft
	// reorder among already price-floored names it only mildly mis-ranks a name that
	// trades thin on IEX — it never drops one.
	if p.RankByLiquidity {
		rankScore := func(c GapCandidate) float64 {
			dollarVol := c.PreMarketLast * float64(c.PreMarketVol)
			if dollarVol < 0 {
				dollarVol = 0
			}
			return math.Abs(c.GapPct) * math.Log10(dollarVol+10)
		}
		sort.Slice(candidates, func(i, j int) bool {
			return rankScore(candidates[i]) > rankScore(candidates[j])
		})
	} else {
		sort.Slice(candidates, func(i, j int) bool {
			return math.Abs(candidates[i].GapPct) > math.Abs(candidates[j].GapPct)
		})
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
