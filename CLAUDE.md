# Git workflow

Every new feature must go out on a dedicated branch and have a PR created before any review or merge. Never push directly to `main` unless the user explicitly requests it.

After pushing a feature branch, always create a PR immediately — never leave code sitting on a remote branch without a PR.

After creating a PR, always start a persistent Monitor for that PR number that watches for reviews and merge/close state changes.

# Memory

After each PR is **merged**, save a `project` memory summarising the feature — what it does, why it was built, and any key implementation details worth knowing in future sessions. Use the standard memory frontmatter format and add a pointer to `MEMORY.md`. Do not write memory at PR creation time; wait for merge.

# Code review

Before every push to a PR branch, always run the `feature-dev:code-reviewer` agent over the changes first. Address (or consciously dismiss with a reason) its findings before pushing. This applies to the initial push and every follow-up push to an existing PR.

## End-to-end correctness requirement

Every feature and every review fix must be verified through the complete flow it
can affect, not only through the edited function or its unit tests. Before
pushing, identify the feature's state transitions and verify each relevant
boundary end to end (for example: config/input -> decision -> broker/provider ->
sidecar/journal -> database -> restart/recovery -> API/TUI/web rendering ->
notifications). For execution changes, explicitly test accepted, filled,
partial, cancelled, rejected, delayed, retried, duplicate, stale-snapshot,
failure-recovery, and opposite-position cases where applicable.

Do not ship a half-fix. A review finding is complete only when its invariant is
stated, the failure path is fixed at every affected boundary, a regression test
reproduces it, and the end-to-end flow remains correct after restart and partial
failure. If a change expands beyond the original feature (such as persistence,
reconciliation, or recovery), pause implementation long enough to map the full
state machine and add the corresponding acceptance checks before continuing.

The final pre-push checklist must include focused tests, the full repository
test/vet/build gates, race tests for concurrency-sensitive packages, a diff
self-review, and a fresh review-thread check immediately before pushing.

# PR workflow

After pushing a branch, immediately create a PR and start a persistent Monitor watching that PR for reviews and merge/close events.
