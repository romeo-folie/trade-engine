// Package ibkrconids provides static IBKR contract IDs (conids) for futures.
// Keys use the format "SYMBOL/YYYYMM" (e.g. "ES/202506") so the map is
// asset-class agnostic — index futures, metals, energy all share the same key
// scheme. Add new asset classes by populating entries; no code change needed.
//
// # Quarterly update process
//
// CME equity index futures (ES, NQ, YM, RTY) roll on the third Friday of
// March, June, September, and December. One week before expiry the front-month
// liquidity migrates to the next contract. Keep two quarters populated at all
// times: the current front-month and the next. Obtain conids from the IBKR
// TWS contract search or via:
//
//	GET /v1/api/iserver/secdef/info?symbol=ES&secType=FUT&exchange=CME
//
// Monthly futures (GC, CL, etc.) roll on a per-contract schedule; populate
// the current and next two months. The dynamic fallback in execution/ibkr_conid.go
// handles stale entries during the gap between a roll and the next map update.
package ibkrconids

import "time"

// FuturesConids maps "SYMBOL/YYYYMM" to the IBKR contract ID.
// Two quarters are populated for each instrument so a roll-week map update
// has a one-quarter grace period.
//
// Last updated: 2026-07-10 (Q3 2026 front-month SEP26 + Q4 2026 next quarter DEC26).
// Next update due: before September 2026 expiry (third Friday, ~Sep 18 2026).
// Verified live against the paper gateway via `/v1/api/trsrv/futures?symbols=…` —
// this is now the authoritative source and the runtime resolver uses it too, so
// this map is a warm-cache/offline fallback rather than the sole truth. The prior
// hand-entered full-size conids (751458xxx) did NOT match the gateway and were
// replaced. Refresh via:
//   GET /v1/api/trsrv/futures?symbols=ES,NQ,YM,RTY,MES,MNQ,MYM,M2K
var FuturesConids = map[string]string{
	// ── US Equity Index Futures (CME/CBOT) ── quarterly expiry (HMUZ)
	// Full-size E-minis
	"ES/202609": "649180671", "ES/202612": "515416632", // E-mini S&P 500
	"NQ/202609": "770561204", "NQ/202612": "563947726", // E-mini Nasdaq-100
	"YM/202609": "815824220", "YM/202612": "840227352", // E-mini Dow Jones (CBOT)
	"RTY/202609": "793356259", "RTY/202612": "780341989", // E-mini Russell 2000
	// Micro E-minis (1/10th of the full-size contract)
	"MES/202609": "793356217", "MES/202612": "815824257", // Micro E-mini S&P 500
	"MNQ/202609": "793356225", "MNQ/202612": "815824267", // Micro E-mini Nasdaq-100
	"MYM/202609": "815824200", "MYM/202612": "840227334", // Micro E-mini Dow Jones (CBOT)
	"M2K/202609": "793356214", "M2K/202612": "815824254", // Micro E-mini Russell 2000

	// ── Metals Futures (COMEX) ── monthly expiry; add GC/SI entries when enabling metals ORB
	// "GC/202506": "",

	// ── Energy Futures (NYMEX) ── monthly expiry; add CL/NG entries when enabling energy ORB
	// "CL/202506": "",
}

// FrontMonthFor returns the "YYYYMM" string for the active front-month contract
// given the current time and the instrument's expiry cycle.
//
// Supported cycles:
//   - "quarterly": contract rolls on the first of March, June, September, December.
//     Months outside these four map to the next upcoming roll month.
//     E.g. January → "YYYY03", April → "YYYY06".
//   - "monthly": the front-month is always the current calendar month.
//     E.g. April 2026 → "202604".
//
// The quarterly roll rule is conservative: we switch to the new contract on the
// first of the expiry month rather than the exact expiry Friday. This is safe
// because liquidity migrates to the next contract well before the actual expiry.
func FrontMonthFor(t time.Time, cycle string) string {
	year, month := t.Year(), int(t.Month())

	switch cycle {
	case "monthly":
		return formatYYYYMM(year, month)

	case "quarterly":
		// Quarterly expiry months: 3 (Mar), 6 (Jun), 9 (Sep), 12 (Dec).
		// Find the next (or current) expiry month >= current month.
		expiries := [4]int{3, 6, 9, 12}
		for _, em := range expiries {
			if month <= em {
				return formatYYYYMM(year, em)
			}
		}
		// Past December of this year → March of next year.
		return formatYYYYMM(year+1, 3)

	default:
		// Unknown cycle: fall back to monthly so callers get a usable key
		// rather than a silent empty string. The dynamic fallback will handle
		// any lookup miss.
		return formatYYYYMM(year, month)
	}
}

// FrontMonthKey returns the full FuturesConids lookup key for a symbol.
// Convenience wrapper: FrontMonthKey("ES", "quarterly", time.Now()) → "ES/202506".
func FrontMonthKey(symbol, cycle string, t time.Time) string {
	return symbol + "/" + FrontMonthFor(t, cycle)
}

func formatYYYYMM(year, month int) string {
	// Avoid fmt.Sprintf for a zero-alloc hot path.
	buf := [6]byte{}
	y := year
	buf[0] = byte('0' + y/1000)
	buf[1] = byte('0' + (y/100)%10)
	buf[2] = byte('0' + (y/10)%10)
	buf[3] = byte('0' + y%10)
	buf[4] = byte('0' + month/10)
	buf[5] = byte('0' + month%10)
	return string(buf[:])
}
