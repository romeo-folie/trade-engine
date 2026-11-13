# JEV entry evidence: data fix and replay experiment

The live timing snapshot previously had no current-mark fallback when a
successfully refreshed daily response contained only yesterday's bar. It now
tries a current two-sided quote before freezing the decision cutoff and
selecting news. The quote must match the symbol, be finite and uncrossed, and
be no more than one minute old and no later than the cutoff. Its midpoint is
recorded as `analysis_current_quote_mid`. Invalid/missing evidence remains
unavailable; a snapshot missing-block entry records failure. Existing final
execution quote checks still apply. A fresh primary candle avoids the extra
quote request. This change does not replace the primary strategy candle or
make a stale primary entry price current.

The model question changes are **replay-only**. The running evaluator still
uses its existing question set and numerical policy. No new config switch or
operator change is needed for this PR. Deployment requires the normal review,
merge, build and restart workflow.

## Replay command

```
te jev-replay <snapshot-id> [snapshot-id...] --output /tmp/jev-replay.jsonl
```

This performs two paid calls per snapshot, up to ten explicitly selected
snapshots. It reads the database but does not write decisions, fetch market
data, execute orders, or change settings. The output file must be new and is
created with mode 0600: it contains complete frozen requests and responses,
including private portfolio/context data. Keep it out of version control.
Failed calls and successful calls are flushed immediately; any call failure
produces a nonzero command exit. Credentials are not written into the output.

The baseline sends the recorded state and questions. The experimental variant
identifies the execution timeframe and shortest available timing-evidence
timeframe, exposes candidate entry/stop/target plans using the same rounding
and risk/reward floor as the recorded policy, and adds independent long/short
blocker classifications. An explicit nonempty entry-timing context keeps its
execution role; supporting hourly evidence never silently replaces the actual
execution timeframe. Mechanical plan validity is distinguished from current
mark evidence, and the candidate price's candle timestamp is visible.

Both variants resolve with recorded policy thresholds and frozen inputs.
Blocker answers are diagnostic and never override the entry gates. Unknown
snapshot/policy/question versions are rejected rather than silently replayed
under different semantics. `--model` overrides the configured requested model;
the served version is recorded on each call. Compare served versions before
interpreting a pair, since aliases may change.

This is a combined evidence/question experiment. It does not isolate the
effects of each change, determine fillability, run downstream risk gates, or
measure subsequent returns. Do not treat its BUY/SHORT output as permission
to place an order.

## Initial six-case comparison — November 13, 2026

Twelve successful calls, all served by `jev-1.14.0`, matching the historical
served version. Recorded snapshot IDs below make selection reproducible.

| Symbol | Snapshot | Side inspected | Baseline enter-now | Experiment enter-now | Experiment blocker | Final baseline → experiment |
|---|---|---|---:|---:|---|---|
| HUM | c4fc7ebb-6a8d-4fc7-9e62-1d0c2ac007bf | Long | .11 | .36 | Extension | WAIT → WAIT |
| KKR | c5e4a2f1-9bbe-4f0f-8a75-4750ec0c0232 | Long | .19 | .48 | Event | WAIT → WAIT |
| NBIS | f4e0dc4f-8694-4d2a-ad15-eb427c48a0de | Short | .16 | .43 | None | HOLD → HOLD |
| NIO | 1f5d35cd-0600-4704-acf4-75f6d996daaa | Short | .13 | .39 | None | HOLD → HOLD |
| RKLB | 972058fb-4844-41cf-b555-d24e33b0e23a | Long | .07 | .27 | Extension | WAIT → WAIT |
| TDG | 589ba74e-4ef9-4561-aa18-6cc500f94619 | Long | .04 | .10 | Missing data | WAIT → HOLD |

TDG's experimental long timing choice was `insufficient_evidence` (.52), and
its missing-data blocker score was .97. The other five long timing answers
still chose WAIT; all short timing answers still chose WAIT. NBIS/NIO choosing
blocker `none` while timing chooses WAIT demonstrates that separately asked
questions can disagree. These diagnostics are not enforced as a coherent
joint decision and should not yet become live policy. KKR's event blocker was
only .31 (next alternatives none .28 and extension .24); do not interpret that
label as a proven event. No concrete event name/level is returned by this
classification.

No entries were produced by either variant. The sample was deliberately
selected from strong WAIT, bearish, extended and missing-data cases; it is
neither independent nor representative. Each pair was called once, so model
variability and causal attribution are unresolved. The historical missing
mark was not repaired with today's quote during replay.

## Suggested next steps

1. Verify the quote fallback in fresh paper-session snapshots after deployment;
   track missing marks by symbol and time of day, without weakening freshness.
2. Repeat baselines and test separate ablations (timeframe only, candidate plan
   only, timing wording only) on a benchmark labeled before inspecting future
   returns, with both valid entries and obvious non-entries.
3. Make blocker/timing inconsistency measurable before introducing any joint
   resolution rule. Include concrete evidence references in a subsequent
   version if the provider's supported response types can retain them.
4. Evaluate the complete policy, downstream risk gates, fills and costs on
   held-out sessions before enabling any question change. Keep the current
   thresholds while gathering this evidence.
