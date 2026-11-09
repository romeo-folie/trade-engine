# internal/analysis — LLM Analysis Engine

Builds prompt-safe market context, dispatches configured model providers, and
returns an auditable provider or aggregate decision. The package supports
single analysts, adaptive-depth ensembles, arbitration, and sequential
specialists; the caller owns final lens validation.

## Components

| Area | Purpose |
|---|---|
| `models.go` | Provider interface plus request, response, and call-metadata contracts |
| `analyzer.go`, `ensemble.go` | Single/ensemble routing, fast path, majority fallback, and arbitration |
| `specialist.go`, `prompts/roles.go` | Sequential specialist and risk-manager workflow |
| `lens_audit.go` | Validates selected/rejected lenses against an execution profile |
| `prompts/stock.go` | Generalist stock prompt and strategy guidance |
| `prompts/timeframe_context.go` | Primary-thesis context for entry-confirmation passes |
| `anthropic.go`, `openai.go`, `gemini.go`, `deepseek.go`, `ollama.go` | Model-provider adapters |
| `cache/`, `classifier/`, `depth/` | Prompt caching, impact classification, and adaptive analyst depth |

## Request Context

`AnalysisRequest` can carry:

- recent candles, computed indicators, news, and portfolio state;
- knowledge-graph, similar-analysis, and performance context;
- scanner discovery provenance and a validated execution profile;
- deterministic pullback facts when `pullback_strength` is admissible; and
- supplemental timeframe evidence (`TimeframeContexts`) for the same decision.

A valid execution profile owns the ordered analyst menu. Discovery-source
labels explain why the candidate was found and are never offered as strategies.
Legacy callers can still supply `Strategy`/`Strategies` without a profile.

## Decision Contract

The stock response contains:

- `action`: `BUY`, `SELL`, `HOLD`, or `WAIT`;
- confidence, sizing, stop, target, and reasoning;
- the observed regime;
- one selected admissible lens, or `none`; and
- a reason for rejecting every other admissible lens on actionable BUYs.

`Analyzer.Analyze` and `Analyzer.AnalyzeFull` do not perform the final lens
audit. Callers must pass their result to `ValidateFinalLensSelection` before
trusting a profile-bound BUY. The engine does this before persistence and
execution: invalid lens selection or incomplete rejection coverage becomes a
non-directional HOLD. SELL decisions are exits and are not suppressed by
ordinary entry-lens validation.

## Analysis Workflows

```mermaid
flowchart LR
    R[AnalysisRequest] --> D{Configured workflow}
    D -->|single| S[One analyst]
    D -->|ensemble| E[Parallel analysts]
    D -->|specialist| P[Sequential specialists]
    E --> A[Fast path / arbiter / majority]
    P --> M[Risk-manager synthesis]
    S --> V[Caller runs lens audit]
    A --> V
    M --> V
    V --> O[Validated response + metadata]
```

For scanner profiles with supplemental timeframes (for example an entry-timing
timeframe), the engine loads that evidence into `TimeframeContexts` on the same
request, so one ensemble judges thesis and timing together. A configured
supplemental timeframe that cannot be loaded blocks new BUYs for the cycle while
still permitting a risk-reducing SELL. The `entry_confirmation` role and
`PrimaryThesisContext` prompt contract remain in `prompts/`, but the live engine
no longer issues a separate confirmation request.

## Audit Metadata

`ModelCallMeta` captures provider/model identity, prompt hash, raw prompt and
response, token counts, latency, and the indicator snapshot. The store retains
the final metadata plus each analyst's metadata (`ensemble_results`), the
arbiter call, selected/rejected lenses, regime, scanner lineage, and the compact
timeframe-workflow audit. Known replay gaps, including failed attempts, the
unrecorded arbiter permutation, and mutable candles/news, are inventoried in
`docs/issue-589-decision-replay.md`.
