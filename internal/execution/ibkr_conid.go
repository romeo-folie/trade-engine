package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
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

// ResolveContractID exposes the exact broker contract identity to lifecycle
// stores after order placement. Resolution uses the same cache/static map as
// PlaceOrder, so the usual post-placement call does not add a gateway lookup.
func (b *IBKRBroker) ResolveContractID(ctx context.Context, symbol, secType string) (string, error) {
	return b.resolveConid(ctx, symbol, secType)
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

	path := "/v1/api/iserver/secdef/search?symbol=" + base + "&secType=" + secType
	var rawResults []json.RawMessage
	if err := b.do(ctx, "GET", path, nil, &rawResults); err != nil {
		return "", fmt.Errorf("ibkr resolveConid: search %q: %w", pair, err)
	}

	for _, raw := range rawResults {
		var r struct {
			Conid       ibkrconids.Conid `json:"conid"`
			CompanyName string           `json:"companyName"`
			Description string           `json:"description"`
			Exchange    string           `json:"exchange"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		if r.Conid <= 0 {
			continue
		}
		if r.Exchange == "IDEALPRO" && (r.Description == pair || r.CompanyName == strings.Replace(pair, "/", ".", 1)) {
			conid := strconv.Itoa(int(r.Conid))
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

// ibkrTrsrvFuture is one contract from /trsrv/futures. expirationDate and ltd are
// integer YYYYMMDD dates (e.g. 20260918), NOT strings.
type ibkrTrsrvFuture struct {
	Symbol         string `json:"symbol"`
	Conid          int    `json:"conid"`
	ExpirationDate int    `json:"expirationDate"` // YYYYMMDD
	LTD            int    `json:"ltd"`            // last trading day, YYYYMMDD
}

// resolveConidFuturesDynamic resolves a futures symbol to its nearest front-month
// contract conid via IBKR's /trsrv/futures endpoint, which takes a bare symbol and
// returns the full contract chain. This replaced /iserver/secdef/info?symbol=…,
// which requires a *conid* and rejects a symbol with "Bad Request: invalid conid"
// — so the old dynamic path never worked for any symbol missing from the static
// map (every micro, and every symbol once the map goes stale). exchange is retained
// for signature compatibility but unused: trsrv keys purely by symbol.
// Results are cached by cacheKey to avoid repeat calls within a session.
func (b *IBKRBroker) resolveConidFuturesDynamic(ctx context.Context, symbol, exchange, cacheKey string) (string, error) {
	path := "/v1/api/trsrv/futures?symbols=" + symbol

	var resp map[string][]ibkrTrsrvFuture
	if err := b.do(ctx, "GET", path, nil, &resp); err != nil {
		return "", fmt.Errorf("ibkr resolveConidFutures: trsrv/futures %q: %w", symbol, err)
	}
	contracts := resp[symbol]
	if len(contracts) == 0 {
		// Fallback: take whatever key was returned (guards against case/format drift).
		for _, v := range resp {
			if len(v) > 0 {
				contracts = v
				break
			}
		}
	}
	if len(contracts) == 0 {
		return "", fmt.Errorf("ibkr resolveConidFutures: no contracts returned for %q", symbol)
	}

	// Pick the nearest expiry >= today. Prefer ltd (last trading day); fall back to
	// expirationDate when ltd is absent.
	today, _ := strconv.Atoi(time.Now().Format("20060102"))
	var bestConid, bestExp int
	for _, c := range contracts {
		exp := c.LTD
		if exp == 0 {
			exp = c.ExpirationDate
		}
		if c.Conid <= 0 || exp < today {
			continue
		}
		if bestExp == 0 || exp < bestExp {
			bestExp, bestConid = exp, c.Conid
		}
	}
	if bestConid == 0 {
		return "", fmt.Errorf("ibkr resolveConidFutures: no valid front-month found for %q", symbol)
	}

	conid := strconv.Itoa(bestConid)
	b.conidMu.Lock()
	b.conidCache[cacheKey] = conid
	b.conidMu.Unlock()
	return conid, nil
}
