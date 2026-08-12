# Trade-engine contribution rules

## Mandatory model routing

Use `gpt-5.6-sol` for planning and architectural design. If the active primary
agent is not running Sol, delegate the planning work to a Sol agent before
implementation begins.

Use `gpt-5.6-terra` for code implementation and fixes. If the active primary
agent is not running Terra, delegate all code-writing work to a Terra agent and
retain orchestration in the primary thread.

Run every Codex built-in `/review` with `gpt-5.3-codex-spark`. Do not
substitute a Terra review, a generic reviewer subagent, or GitHub Codex review
for the required local Spark `/review`. The project Codex configuration pins
the model defaults; this rule still applies if a session-level override would
otherwise replace them.

## Mandatory pre-push review

Before every push to any PR branch, run Codex's built-in `/review` against the
complete branch diff. Treat it as the required local review: trace changed
behavior end to end, identify correctness and regression risks, and add or
update tests for every actionable finding. A clean `/review` is sufficient to
proceed; no second `feature-dev` Quality Review/code-reviewer pass is required.

Do not push until the review findings are fixed or explicitly documented as
intentional exceptions. After that review, run the focused tests, full test
suite, vet, build, applicable race tests, diff checks, and a final review-thread
check. GitHub Codex review is a secondary safety net, not a substitute for this
local gate.
