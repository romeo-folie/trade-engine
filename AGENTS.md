# Trade-engine contribution rules

## Mandatory model routing

Use `gpt-5.6-sol` for planning and architectural design. If the active primary
agent is not running Sol, delegate the planning work to a Sol agent before
implementation begins.

Use the active primary model for code implementation and fixes. Do not delegate
code-writing work solely to satisfy model routing.

Run every Codex built-in `/review` with `gpt-5.6-sol`. Do not substitute a
generic reviewer subagent or GitHub Codex review for the required local Sol
`/review`. The project Codex configuration pins the model defaults; this rule
still applies if a session-level override would otherwise replace them.

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

## Mandatory PR watcher persistence

Once a PR watcher is started, keep actively monitoring and handling its review
and CI events without shutting it down until the PR is merged or closed. Do not
claim a detached watcher can take action on its own: keep the Codex turn active
while remediation is expected, and restart the watcher if its heartbeat stops.

## Mandatory post-merge database and engine handling

After merging a PR, the agent must:

1. Sync the local `main` branch to the merged remote state.
2. Compare merged configuration schema, example, and default changes with the
   ignored operator `config.yaml`; apply required live changes without
   committing `config.yaml`, then run `./te config validate`. If validation
   fails, do not migrate or restart; report the failure and diagnose it.
3. Run `./te migrate` from the repository root. `te run` does **not** apply
   migrations automatically; this command applies every pending migration,
   including any introduced by the merged PR.
4. Verify that migration succeeds before continuing. If it fails, do not start
   or restart the engine; report the failure and diagnose it.
5. After successful validation and migration, ask the user whether the engine
   should be restarted. Do not restart it without that confirmation.

When a restart is approved, first verify whether an existing engine process is
still running. Do not rely solely on the PID file: it can be stale while an old
process still owns the web port. Gracefully stop the confirmed existing process,
then restart and verify the new engine's database connectivity and fresh logs
for migration-related SQL errors.
