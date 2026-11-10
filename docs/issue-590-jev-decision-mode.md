# Jev decision mode (issue #590)

TypeSafe's Jev can be the **sole decision maker** for stock analysis. In that
mode, no analysts or arbiter run, which is the cost motivation for this
feature. Jev answers typed questions about the frozen decision inputs. A
versioned, deterministic policy composes those answers into the engine's
BUY/SELL/HOLD/WAIT response. The response then follows the unchanged engine
path: final lens audit, `analyses` persistence, risk checks, entry gates,
execution, and protective exits.

Code: `internal/jev` (client, `jev-questions/v1`, `jev-policy/v1`) and
`internal/engine/jev_mode.go` (wiring).

## Modes

```yaml
analysis:
  decision_mode: jev            # default: ensemble
  jev:
    authority: shadow           # shadow | authoritative
    allow_live: false
    model: jev-latest           # GET /v1/models offers aliases only
    api_key_env: TYPESAFE_API_KEY
```

| | shadow | authoritative |
|---|---|---|
| Who decides and executes | The configured analyst workflow | Jev alone |
| Analysts / arbiter run | Yes | **No** |
| Jev call | Concurrent with the baseline, on identical inputs | The decision |
| Recorded as | `jev_shadow` stage call on the baseline's snapshot, with `executed_action` and `agrees` | `jev` stage call; snapshot `workflow = jev`, `policy_version = jev-policy/v1`, `prompt_version = jev-questions/v1` |
| Can delay a decision | At most `shadow_grace_ms`, and only after the signal has been generated | n/a |
| Cost | Baseline + Jev | Jev only |
| Live account | Allowed (never executes) | Requires `allow_live: true` |

In authoritative mode, analysts, the arbiter, and specialist roles are not
required: they can be removed along with their API keys, which is where the
cost saving comes from. Roles that are used independently are still validated:
classifier, news enricher, and embedder. One consequence: without analysts,
knowledge-graph news extraction needs an explicit `models.roles.news_enricher`.
Otherwise extraction reports that no provider is available, because the
legacy fallback picks an extraction model from the analysts.

The engine refuses to start when Jev is configured but its evaluator cannot
be built, or when authoritative Jev would run against a live account without
`allow_live`.

## Model versions

`GET /v1/models` currently offers only aliases: `jev-latest` and `jev-preview`.
Every call records the version that actually answered (for example
`jev-1.14.0`) as the stage call's `resolved_model`. When comparing periods,
split by `resolved_model`: an alias can move to a new version without a config
change.

A live smoke test on 2026-11-10 (`jev-latest` → `jev-1.14.0`) used about 3.4k
input tokens and 0.24k output tokens, with about 210 ms latency, for one
five-question entry evaluation.

## Decision contract

- **New entries** are asked five questions: thesis support, material adverse
  evidence, lens fit (exactly the ticker's admissible lenses plus `none`),
  entry timing, and regime. Each judgement has an explicit
  `insufficient_evidence` option.
- **Held longs** are asked about exit and regime only. Positions are never
  pyramided.
- **Exit-only strategies** with no held position return HOLD without calling
  Jev.
- **The policy order** is fixed and versioned:

  ```
  abstain → adverse → thesis → lens fit → timing → confidence floor → plan
  ```

  Every outcome carries reason codes and the measured probabilities. The
  policy writes no narrative.
- **Plans are deterministic** because Jev returns probabilities, not prices:
  - stop = entry − `stop_atr_multiple` × ATR;
  - target = entry + max(`target_r_multiple`, `risk.min_risk_reward`) × risk;
  - the target is rounded up so the risk manager's R:R check always passes;
  - size is `size_pct`.
- **Confidence** is P(thesis support) × (1 − P(adverse)). BUYs below
  `min_confidence` (default `risk.confidence_threshold`) become HOLD. This is
  a mapping, not a calibrated probability of profit.
- **Failures** (malformed answers, API errors, exhausted retries) are
  analysis failures. No other model decides, and existing protective orders
  are unaffected.

## Rollout and rollback

1. Run `decision_mode: jev` with `authority: shadow`. Compare the
   `jev_shadow` stage calls against the executed decisions on identical
   snapshots: agreement, abstention, latency, and cost.
2. Calibrate the thresholds on that data. This is a policy-version change:
   bump `PolicyVersion` if the semantics change.
3. Switch to `authority: authoritative` on paper.
4. Set `allow_live: true` only as a separate, deliberate decision.

To roll back, set `decision_mode: ensemble` and restart. Snapshots keep each
decision's mode and versions, so mixed periods stay distinguishable.
