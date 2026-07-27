# Trade-engine contribution rules

## Mandatory pre-push review

Before every push to any PR branch, run the `feature-dev` skill's Quality
Review/code-reviewer pass against the complete branch diff. Treat it as a
local review: trace changed behavior end to end, identify correctness and
regression risks, and add or update tests for every actionable finding.

Do not push until the review findings are fixed or explicitly documented as
intentional exceptions. After that review, run the focused tests, full test
suite, vet, build, applicable race tests, diff checks, and a final review-thread
check. GitHub Codex review is a secondary safety net, not a substitute for this
local gate.
