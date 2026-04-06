package execution

import (
	"context"
	"fmt"
	"strings"
)

// ibkrConids contains well-known IBKR contract IDs for forex pairs and CFD instruments.
var ibkrConids = map[string]string{
	// Forex (CASH on IDEALPRO)
	"EUR/USD": "12087792",
	"GBP/USD": "12087797",
	"USD/JPY": "12087802",
	"AUD/USD": "15016059",
	"USD/CHF": "12087820",
	"USD/CAD": "12087817",
	"NZD/USD": "15016065",
	"EUR/GBP": "12087811",
	// Metals (CFD)
	"XAU/USD": "69067924",
	"XAG/USD": "69067925",
}

// resolveConid returns the IBKR contract ID (conid) for a symbol such as "EUR/USD" or "XAU/USD".
// The secType parameter is used only for the API search fallback (e.g. "CASH" or "CFD").
// Resolution order:
//  1. Hardcoded map (no network round-trip).
//  2. In-process cache populated by previous API lookups.
//  3. IBKR secdef search API.
func (b *IBKRBroker) resolveConid(ctx context.Context, pair string, secType string) (string, error) {
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

	// Default to CASH if secType is not specified.
	if secType == "" {
		secType = "CASH"
	}

	var results []struct {
		Conid       int    `json:"conid"`
		CompanyName string `json:"companyName"`
		Description string `json:"description"`
		Exchange    string `json:"exchange"`
	}
	path := "/iserver/secdef/search?symbol=" + base + "&secType=" + secType
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
