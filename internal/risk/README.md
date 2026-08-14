# internal/risk — Portfolio Admission Controls

Pure, deterministic risk checks applied after analysis and entry-quality gates.
The engine supplies broker-aware signal and portfolio state through
`CheckInput`; this package performs no database or network I/O.

## Checks

`CheckAll` short-circuits on the first failure, in this order:

1. SELL ownership and held-quantity validation.
2. Pyramid eligibility for an existing long.
3. Static or risk/reward-tiered confidence floor.
4. Stop/target risk-reward geometry.
5. Per-position sizing.
6. Combined exposure in the ticker after the proposed order.
7. Maximum dollar risk using the broker-serialized quantity/notional.
8. Maximum open positions.
9. Maximum positions per ticker.
10. Daily realized-loss limit.
11. Account drawdown from peak.
12. Cooldown after the last losing trade.

Failed signals are persisted by the engine with `status=blocked` and the first
human-readable failure reason. `CheckEntryQuality` exposes the shared
confidence and risk/reward subset for price-sensitive revalidation and replay.

```go
results, passed := risk.CheckAll(input)
if !passed {
    reason := risk.BlockReason(results)
    // Persist the blocked signal; do not submit an order.
}
```

Daily trend alignment, pullback qualification, news tape confirmation, and
momentum anti-chase are strategy-specific entry checks owned by
`internal/engine/entry_gates.go`; they run before this portfolio-level gate.
