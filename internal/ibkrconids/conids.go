// Package ibkrconids provides the well-known IBKR contract IDs (conids) for
// major forex pairs and CFD instruments, shared between the data and execution
// packages to ensure a single source of truth.
package ibkrconids

// Conids maps instrument symbols to their IBKR contract IDs.
var Conids = map[string]string{
	// Forex (CASH on IDEALPRO)
	"EUR/USD": "12087792",
	"GBP/USD": "12087797",
	"USD/JPY": "12087802",
	"AUD/USD": "15016059",
	"USD/CHF": "12087820",
	"USD/CAD": "12087817",
	"NZD/USD": "15016065",
	"EUR/GBP": "12087811",
	// Spot metals (IBKR CMDTY "loco London" — asset class "Commodities" on statements, NOT CFD).
	// Verified against the live gateway: /iserver/contract/{conid}/info resolves these to
	// XAUUSD "London Gold" and XAGUSD "London Silver", and /marketdata/history returns bars.
	"XAU/USD": "69067924", // London Gold
	"XAG/USD": "77124483", // London Silver — was 69067925, a dead conid ("Contract details are not available")
}
