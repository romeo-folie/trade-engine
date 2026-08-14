# Issue #550 — Discovery context and execution profiles

## Purpose

Issue #550 separates **why a ticker was discovered** from **how it may be
analysed and traded**. A discovery source (for example, weekly leadership,
premarket catalyst, graph/sympathy, potent industry, or a future political
disclosure) is provenance and evidence; it is not an analytical strategy.

The desired outcome is a durable, auditable path from scanner evidence through
deep analysis and entry gates. The analyst may choose only from a
scanner-supplied set of valid execution lenses; it must never turn an
attribution tag or an arbitrary price dip into a tradable strategy.

## Scope and non-goals

This work covers scanner promotion, watchlist persistence, deep-analysis prompt
inputs, lens selection, pullback-strength facts, timeframe roles, entry-gate
selection, and audit records.

It does not alter the correctness or promotion behavior of PR #532. That PR is
being completed separately. This issue supplies a generic source contract so a
political-disclosure source can plug in after PR #532 merges, with its source
evidence and source-specific default profile, without special-case schema or
prompt work.

## Canonical contract

Persist the following versioned structured values on a promoted watchlist item,
and capture the equivalent immutable values in scanner-run snapshots. The exact
Go type and SQL column names should be settled in implementation, but their
meaning is fixed here.

### `discovery_context`

```text
version
sources[]                 // attribution only; may contain more than one source
thesis                    // concise scanner-produced discovery thesis
evidence_by_source        // structured measurements, catalyst/source references,
                           // timestamps, freshness/expiry, and links/identifiers
```

`graph_context` and current per-source free-text snapshot fields are legacy
inputs to migrate or project into this object. Free text may be rendered for a
prompt, but is not the durable cross-system contract.

### `execution_profile`

```text
version
recommended_lens          // valid execution lens preferred by deterministic rules
admissible_lenses[]       // ordered, recognised execution lenses only
primary_timeframe         // thesis timeframe: normally 1D for swing candidates
entry_timing_timeframe?   // normally 4H when timing confirmation is required
scorer_rationale          // why this bounded menu and timeframe role apply
```

Valid lenses include the existing analytical lenses plus `pullback_strength`.
Source labels such as `weekly_leader`, `premarket_episodic`, and the future
political-disclosure label are never lenses. Existing `strategies` remains a
read-compatible transitional field for legacy rows and current configuration,
but is no longer the canonical carrier of mixed attribution and strategy data.

### Analysis and gate audit

Persist the validated selected lens, rejected lens reasons, primary and entry
timeframes actually used, and gate outcomes with the analysis/signal lineage.
The selected lens, not the first legacy strategy label, keys applicable entry
gates.

## Agreed decisions

1. Source-specific defaults are deterministic code; an LLM may refine only
   inside the allowed lens menu and may select no lens.
2. `pullback_strength` is an analytical lens and also retains its deterministic
   eligibility gate. An analyst cannot bypass failed AVWAP/structure
   qualification.
3. Swing work uses a daily thesis first. A profile may request 4H confirmation
   for entry timing. 1H is reserved for explicitly event/intraday profiles.
4. Run 4H confirmation only after an actionable daily outcome (`BUY` or a
   qualified `WAIT`) and only when `entry_timing_timeframe` is `4H`; 4H must
   confirm timing before emitting a BUY.
5. PR #532 is a plug-in dependency, not a reason to block the generic contract:
   its political-disclosure source is enabled only once its counterpart fixes
   are merged and source defaults/evidence mapping are connected.

## Delivery plan — two PRs maximum

### PR 1 — Persist profiles and make analysis context-aware

**Goal:** Establish the contract, feed it from existing scanner sources, and
make `pullback_strength` a selectable but deterministically constrained lens.

#### Task 1 — Add the typed, versioned storage contract

- Add migration(s), Go DTOs, store read/write paths, and scanner-snapshot
  projection for `discovery_context` and `execution_profile`.
- Add compatibility defaults for existing watchlist rows and retain legacy
  columns until a later explicit removal decision.
- Decide the storage location for selected-lens and gate audit fields based on
  existing analysis/signal lineage, rather than duplicating unrelated history.

**Acceptance:** An old row remains analyzable; a new promoted row round-trips
both contracts and retains an immutable scanner-time copy.

**Suggested next step:** Write contract serialization, migration, and
read-compatibility tests before wiring any scanner source.

#### Task 2 — Build deterministic scanner profiles and structured evidence

- Create one shared profile builder for ordinary, weekly-leader,
  potent-industry, premarket-episodic, graph/sympathy, and fallback promotions.
- Map each source's current raw evidence into `discovery_context`; retain every
  source and independent source score when candidates are deduplicated.
- Generate a recommended lens, bounded ordered alternatives, and timeframe
  roles from source-aware deterministic rules. Permit scorer refinement only to
  recognised values within that set.
- Define the political-disclosure mapping as an extension point, but do not
  enable or depend on PR #532 implementation before it lands.

**Acceptance:** No source tag appears in `admissible_lenses`; every promoted
candidate has structured provenance and a valid profile, including fallback
paths.

**Suggested next step:** Start mappings with weekly leader, premarket episodic,
and graph/sympathy because their evidence already exists in scanner snapshots.

#### Task 3 — Feed profiles to deep analysis and record selected lenses

- Make prompt construction consume the explicit profile and discovery context,
  rather than filtering the mixed `strategies` array as its canonical source.
- Validate `selected_strategy` against `admissible_lenses`, record selected and
  rejected lenses, and preserve the selected result across ensemble/specialist
  paths.
- Continue using legacy behavior only as a compatibility fallback where a
  profile is absent.

**Acceptance:** The prompt presents only its profile's allowed lenses and
evidence; invalid selections are rejected or degraded safely; selected-lens
audit data survives the full analysis path.

**Suggested next step:** Add a table-driven source/profile/prompt/selection
lineage test before changing entry behavior.

#### Task 4 — Promote pullback strength to a lens with deterministic facts

- Include `pullback_strength` in recognised-lens validation, prompt guidance,
  specialist/ensemble validation, and history-window calculation.
- Expose a dedicated deterministic pullback DTO to the prompt: anchor and
  timestamps, support components, tolerance, consolidation, flush/reclaim,
  qualification failures, stop, and invalidation.
- Keep the existing AVWAP/structure qualification as a hard downstream gate.

**Acceptance:** Pullback can be selected when offered; factual AVWAP context is
available to the analyst; an unqualified pullback can never be emitted as a
tradable entry.

**Suggested next step:** First make a minimal end-to-end qualified-pullback
selection test pass, then enumerate individual gate-failure cases.

### PR 2 — Enforce timeframe roles and final gate/audit behavior

**Goal:** Make daily thesis plus optional 4H timing confirmation explicit, and
make the selected execution lens authoritative for final entry controls.

#### Task 5 — Replace ordered-timeframe semantics with a defined workflow

- Use `primary_timeframe` for the first analysis pass: daily for swing profiles,
  and 1H only for explicit event/intraday profiles.
- When the profile requests 4H timing and the daily outcome is actionable, run
  a confirmation analysis that receives the daily thesis as structured context.
- Combine the two outputs into one final decision; do not run independent,
  unconnected analyses merely because both timeframes are configured.

**Acceptance:** `1D,4H` has fixed daily-then-4H semantics; no 4H confirmation
runs for a non-actionable daily result; BUY requires requested confirmation.

**Suggested next step:** Define and test the daily-result-to-4H input/output
DTO before changing engine scheduling.

#### Task 6 — Canonicalize gates and final audit

- Apply run-up and other strategy-specific controls using the validated
  selected lens.
- Require deterministic pullback qualification for a selected pullback trade.
- Persist the used timeframe roles, analysis outcomes, gate decisions, and
  reasons into the final audit lineage used by reports/notifications where
  operators need visibility.

**Acceptance:** One integration test traces discovery → profile → prompt →
selected lens → timeframe confirmation → gate → signal, and explains every
declined or emitted trade.

**Suggested next step:** Trace an existing signal end to end to select the
single audit owner before adding report/UI projections.

## Dependency sequence

```text
PR 1: contract → profile builder → prompt/lens context → pullback facts
PR 2: daily thesis → optional 4H confirmation → selected-lens gates/audit

PR #532: independently completed → add political-disclosure source mapper to PR 1 contract
```

PR #532 does not gate starting PR 1. Its source should be wired only after its
outstanding issues are resolved and it is merged, using the already-defined
source-evidence/profile extension seam.

## Risks and safeguards

| Risk | Safeguard |
|---|---|
| Mixed source tags remain mistaken for strategies | Validate profiles against a central execution-lens registry; reject tags from `admissible_lenses`. |
| LLM strategy drift | Source defaults own the menu; model output is validated against it and may choose `none`. |
| Pullback becomes an ungated dip-buying rule | Supply evaluator facts and require the deterministic AVWAP/structure gate after selection. |
| Legacy rows regress | Preserve `strategies` and derive safe compatibility profiles while old rows exist. |
| Daily/4H output conflicts | Give 4H the narrow role of entry confirmation and require a structured daily thesis as input. |
| PR #532 integration causes a second redesign | Define the political-disclosure source mapping as data/profile configuration atop the generic contract. |

## Test and rollout plan

1. Unit-test contract serialization, validation, migrations, and legacy
   fallback behavior.
2. Table-test every source's evidence mapping, allowable lens menu, and
   timeframe profile; include source deduplication and fallback promotion.
3. Test prompts and parser/ensemble/specialist paths so no invalid lens can
   become selected.
4. Test qualified and failed pullback outcomes, including deterministic gate
   rejection after analyst selection.
5. Test the daily-only, daily-to-4H-confirmed, daily-to-4H-rejected, and
   explicit-1H event/intraday paths.
6. Add one database-backed end-to-end lineage test before enabling production
   behavior.
7. Roll out source/profile persistence first, observe profile-versus-selected
   lens divergence and gate outcomes, then enable the explicit 4H confirmation
   path. Enable political-disclosure production mapping only after PR #532 is
   merged and verified against the same lineage tests.

## Completion criteria

- Every newly promoted ticker has distinct discovery and execution data.
- Deep analysis can select only a persisted admissible execution lens or none.
- Pullback analysis has deterministic evidence and cannot bypass qualification.
- Timeframes have explicit roles and produce one auditable final decision.
- Political-disclosure evidence can be added through the shared contract after
  PR #532 merges, without another schema or prompt redesign.
