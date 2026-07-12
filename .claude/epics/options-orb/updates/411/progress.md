---
issue: 411
started: 2026-07-11T18:23:02Z
last_sync: 2026-07-11T18:23:02Z
completion: 100%
---

# Issue #411 — Alpaca options adapter

## Status: implemented (pending epic PR merge)

## Done
- [x] OCC symbol build (OCCSymbol) + parse (ParseOCCSymbol), 100 multiplier
- [x] GetOptionChain (DATA host /v1beta1/options/snapshots) — greeks+IV, null-safe (*float64), paginated (capped), feed warning when not opra
- [x] ResolveOptionContracts (TRADING host /v2/options/contracts) — OCC symbol + tradable, paginated (capped)
- [x] placeOptionOrder — SecType "OPT" branch; whole-contract qty; limit-at-price; no bracket/OCO (Alpaca rejects)
- [x] GetPositions option-aware — asset_class "us_option" -> Position.Option via ParseOCCSymbol
- [x] do() refactored into do/doData/doURL for data-host routing
- [x] config: StocksConfig.OptionFeed + config.example.yaml (drift guard green)
- [x] tests: OCC round-trip/invalid, chain greeks+nulls, contracts, placeOptionOrder(+missing), positions option-aware
- [x] go build ./... OK; execution+config suites green; code-reviewed (validated vs Alpaca API); added maxOptionPages guard

## Notes
- Real-time OPRA feed required for live/paper; warns on indicative. Config threshold surface is #418.
- do→doData split: chain=data host, orders/contracts/positions=trading host.
