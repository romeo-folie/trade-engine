package execution

import (
	"context"
	"fmt"
	"strings"
)

// forexConids contains well-known IBKR contract IDs for major forex pairs on IDEALPRO.
var forexConids = map[string]string{
	"EUR/USD": "12087792",
	"GBP/USD": "12087797",
	"USD/JPY": "12087802",
	"AUD/USD": "15016059",
	"USD/CHF": "12087820",
	"USD/CAD": "12087817",
	"NZD/USD": "15016065",
	"EUR/GBP": "12087811",
}

// resolveConid returns the IBKR contract ID (conid) for a forex pair such as "EUR/USD".
// Resolution order:
//  1. Hardcoded map (no network round-trip).
//  2. In-process cache populated by previous API lookups.
//  3. IBKR secdef search API.
func (b *IBKRBroker) resolveConid(ctx context.Context, pair string) (string, error) {
	// 1. Hardcoded map.
	if conid, ok := forexConids[pair]; ok {
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
		return "", fmt.Errorf("invalid forex pair %q: expected BASE/QUOTE format", pair)
	}
	base := parts[0]

	var results []struct {
		Conid       int    `json:"conid"`
		CompanyName string `json:"companyName"`
		Description string `json:"description"`
		Exchange    string `json:"exchange"`
	}
	path := "/iserver/secdef/search?symbol=" + base + "&secType=CASH"
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
