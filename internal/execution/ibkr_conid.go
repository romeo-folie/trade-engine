package execution

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/ibkrconids"
)

// ibkrConids is the authoritative map of IBKR contract IDs for known forex/CFD instruments,
// shared with the data package via internal/ibkrconids.
var ibkrConids = ibkrconids.Conids

// resolveConid returns the IBKR contract ID (conid) for an instrument.
//
// Resolution order differs by secType:
//
//   - CASH / CFD (forex, metals): static map → cache → IDEALPRO secdef search
//   - FUT (futures): FuturesConids static map (keyed "SYMBOL/YYYYMM") → cache →
//     /iserver/secdef/info dynamic search (returns contract list, picks nearest expiry)
func (b *IBKRBroker) resolveConid(ctx context.Context, symbol string, secType string) (string, error) {
	if secType == "FUT" {
		return b.resolveConidFutures(ctx, symbol, "")
	}
	return b.resolveConidForex(ctx, symbol, secType)
}

// resolveConidForex resolves forex/CFD conids (CASH, CFD secTypes).
func (b *IBKRBroker) resolveConidForex(ctx context.Context, pair string, secType string) (string, error) {
	// 1. Hardcoded map.
	if conid, ok := ibkrConids[pair]; ok {
		return conid, nil
	}

	// 2. Cache.
	b.conidMu.RLock()
	if conid, ok := b.conidCache[pair]; ok {
		b.conidMu.RUnlock()
		return conid, nil
	}
	b.conidMu.RUnlock()

	// 3. API search.
	parts := strings.SplitN(pair, "/", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid pair %q: expected BASE/QUOTE format", pair)
	}
	base := parts[0]

	if secType == "" {
		secType = "CASH"
	}

	var results []struct {
		Conid       int    `json:"conid"`
		CompanyName string `json:"companyName"`
		Description string `json:"description"`
		Exchange    string `json:"exchange"`
	}
	path := "/v1/api/iserver/secdef/search?symbol=" + base + "&secType=" + secType
	if err := b.do(ctx, "GET", path, nil, &results); err != nil {
		return "", fmt.Errorf("ibkr resolveConid: search %q: %w", pair, err)
	}

	for _, r := range results {
		if r.Exchange == "IDEALPRO" && (r.Description == pair || r.CompanyName == strings.Replace(pair, "/", ".", 1)) {
			conid := fmt.Sprintf("%d", r.Conid)
			b.conidMu.Lock()
			b.conidCache[pair] = conid
			b.conidMu.Unlock()
			return conid, nil
		}
	}

	return "", fmt.Errorf("ibkr resolveConid: pair %q not found on IDEALPRO", pair)
}

// resolveConidFutures resolves a futures contract conid for symbol (e.g. "ES", "GC").
// exchange is optional — when non-empty it narrows the secdef/info search, which is
// important for instruments listed on multiple venues (e.g. GC on COMEX vs globex).
// cycle is optional — when empty, quarterly is tried first then monthly.
func (b *IBKRBroker) resolveConidFutures(ctx context.Context, symbol, exchange string) (string, error) {
	// Try quarterly first (most common for index futures), then monthly.
	for _, cycle := range []string{"quarterly", "monthly"} {
		key := ibkrconids.FrontMonthKey(symbol, cycle, time.Now())
		if conid, ok := ibkrconids.FuturesConids[key]; ok {
			return conid, nil
		}
	}

	// Cache check — keyed by "FUT:SYMBOL" so cycle ambiguity doesn't matter post-cache.
	cacheKey := "FUT:" + symbol
	b.conidMu.RLock()
	if conid, ok := b.conidCache[cacheKey]; ok {
		b.conidMu.RUnlock()
		return conid, nil
	}
	b.conidMu.RUnlock()

	return b.resolveConidFuturesDynamic(ctx, symbol, exchange, cacheKey)
}

// resolveConidFuturesWithCycle resolves using a known expiry cycle, avoiding the
// cycle-guessing loop in resolveConidFutures. Preferred when the instrument config
// is available (e.g. from FuturesCandidateScanner execution path).
func (b *IBKRBroker) resolveConidFuturesWithCycle(ctx context.Context, symbol, cycle, exchange string) (string, error) {
	key := ibkrconids.FrontMonthKey(symbol, cycle, time.Now())
	if conid, ok := ibkrconids.FuturesConids[key]; ok {
		return conid, nil
	}

	cacheKey := "FUT:" + symbol
	b.conidMu.RLock()
	if conid, ok := b.conidCache[cacheKey]; ok {
		b.conidMu.RUnlock()
		return conid, nil
	}
	b.conidMu.RUnlock()

	return b.resolveConidFuturesDynamic(ctx, symbol, exchange, cacheKey)
}

// WarmFuturesConidCache resolves and caches conids for all configured instruments
// at startup time. For instruments in the static FuturesConids map (e.g. ES, NQ)
// no network call is made. For instruments not in the map (e.g. GC, SI with monthly
// expiry) the IBKR secdef/info endpoint is called and the result is cached so that
// order placement later in the session never hits a resolution miss.
//
// Returns an error if any instrument cannot be resolved via either path. Call this
// after CheckAuth succeeds so the gateway is guaranteed to be reachable.
func (b *IBKRBroker) WarmFuturesConidCache(ctx context.Context, instruments []config.FuturesInstrument) error {
	for _, inst := range instruments {
		key := ibkrconids.FrontMonthKey(inst.Symbol, inst.ExpiryCycle, time.Now())

		if _, ok := ibkrconids.FuturesConids[key]; ok {
			// Static map hit — no network call needed.
			continue
		}

		// Not in static map — attempt dynamic resolution now while the gateway is warm.
		if _, err := b.resolveConidFuturesWithCycle(ctx, inst.Symbol, inst.ExpiryCycle, inst.Exchange); err != nil {
			return fmt.Errorf(
				"cannot resolve conid for %s (%s, %s expiry): %w\n"+
					"  → add %q to internal/ibkrconids/futures.go or ensure IBKR gateway returns the contract",
				inst.Symbol, inst.Exchange, inst.ExpiryCycle, err, key,
			)
		}
	}
	return nil
}

// ibkrSecdefContract is one entry from /iserver/secdef/info for FUT contracts.
type ibkrSecdefContract struct {
	Conid          int    `json:"conid"`
	Symbol         string `json:"symbol"`
	SecType        string `json:"secType"`
	Exchange       string `json:"exchange"`
	LastTradingDay string `json:"lastTradingDay"` // "YYYYMMDD"
}

// resolveConidFuturesDynamic queries IBKR's secdef/info endpoint for the nearest
// expiry front-month contract. When exchange is non-empty the search is scoped to
// that venue (e.g. "COMEX" for gold), preventing ambiguous multi-venue results.
// Results are cached by cacheKey to avoid repeat calls within a session.
func (b *IBKRBroker) resolveConidFuturesDynamic(ctx context.Context, symbol, exchange, cacheKey string) (string, error) {
	path := fmt.Sprintf("/v1/api/iserver/secdef/info?symbol=%s&secType=FUT", symbol)
	if exchange != "" {
		path += "&exchange=" + exchange
	}

	var contracts []ibkrSecdefContract
	if err := b.do(ctx, "GET", path, nil, &contracts); err != nil {
		return "", fmt.Errorf("ibkr resolveConidFutures: secdef/info %q: %w", symbol, err)
	}
	if len(contracts) == 0 {
		return "", fmt.Errorf("ibkr resolveConidFutures: no contracts returned for %q on %q", symbol, exchange)
	}

	// Sort by LastTradingDay ascending and pick the nearest future expiry >= today.
	today := time.Now().Format("20060102")
	sort.Slice(contracts, func(i, j int) bool {
		return contracts[i].LastTradingDay < contracts[j].LastTradingDay
	})

	for _, c := range contracts {
		if c.LastTradingDay >= today && c.Conid > 0 {
			conid := fmt.Sprintf("%d", c.Conid)
			b.conidMu.Lock()
			b.conidCache[cacheKey] = conid
			b.conidMu.Unlock()
			return conid, nil
		}
	}

	return "", fmt.Errorf(
		"ibkr resolveConidFutures: no valid front-month found for %q on %q — static map may need updating",
		symbol, exchange,
	)
}
