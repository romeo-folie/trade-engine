# Trade-engine contribution rules

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
