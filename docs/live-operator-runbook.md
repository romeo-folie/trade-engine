# Live stock operator runbook

This runbook is for the separate live deployment. Do not copy these values to
the local paper host merely to test them.

## Frozen initial $1,000 profile

```yaml
engine:
  mode: auto
markets:
  stocks:
    enabled: true
    paper: false
    base_url: https://api.alpaca.markets
    api_key_env: ALPACA_LIVE_API_KEY
    secret_env: ALPACA_LIVE_API_SECRET
    expected_account_number: <live account number>
risk:
  confidence_threshold: 0.72
  confidence_tiers:
    - rr_min: 2.0
      conf_min: 0.70
    - rr_min: 1.5
      conf_min: 0.72
  min_risk_reward: 1.5
  max_position_pct: 20.0
  max_dollar_risk_pct: 0.5
  max_open_positions: 10
  max_per_ticker: 2
  pyramid_enabled: true
  pyramid_min_gain_pct: 2.5
  daily_loss_limit_pct: 2.0
  max_drawdown_pct: 10.0
  cooldown_after_loss_min: 15
  default_stop_loss_pct: 2.0
  default_take_profit_pct: 4.0
  trailing_stop:
    enabled: true
    rule: atr
    atr_multiple: 1.5
    atr_period: 14
    interval_sec: 60
    activate_at_gain_pct: 5.0
  uncap_take_profit: true
  exit_conf_min: 0.72
  bracket_exits_enabled: false
```

`max_position_pct` applies to the combined ticker exposure, including a
pyramid add. `max_dollar_risk_pct` applies to each submitted lot at its actual
whole-share quantity. The first added lot is eligible only after the existing
position has gained 2.5%.

## Before starting

1. Set only the live-server environment variables shown above; never place a
   key in YAML.
2. Run `./te migrate`, then `./te stocks preflight` and confirm the masked
   account identity is the expected live account.
3. Run `./te config validate` and `./te buys status`.
4. Start the engine and inspect `./te status`. A paused buy control must remain
   paused after restart; trailing stops and exits are intentionally unaffected.

## Emergency action

To stop creating new stock risk while allowing every protective, trailing, and
analyst exit to continue:

```sh
./te buys pause --source operator
```

To resume only after investigating the reason:

```sh
./te buys resume --source operator
```

`./te buys status` reports the persisted state, operator source, and timestamp.
Do not use a mode change as an emergency exit control; it changes more than the
buy gate and does not replace broker reconciliation.

For a loopback-only operator dashboard, `GET /api/system/status` exposes the
same readiness state (workflow, environment, masked broker account, buying
power availability, reconciliation snapshot, and buy control). `GET`/`POST
`/api/system/buys` reads or updates the buy control; a POST body is
`{"enabled": false, "source": "operator"}`.
