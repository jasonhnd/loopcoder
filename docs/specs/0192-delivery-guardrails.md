---
id: 192
title: Delivery Guardrails - Budget Caps and Loop Circuit-Breaker
status: accepted
date: 2026-06-28
issue: 192
pr: null
supersedes: []
superseded_by: []
---

# Delivery Guardrails - Budget Caps and Loop Circuit-Breaker

This is a design-only spec for loopcoder 0.3.2. This PR must add the design
record and reference documentation only: no Go code, no `.delivery.yml` policy
changes, no command behavior changes, no labels, and no migration of existing
run state. Implementation belongs in separate code issues after this spec is
reviewed and merged per [`docs/PROCESS.md`](../PROCESS.md).

## Goal

loopcoder must stop delivery loops before they become unbounded or spend past a
configured operator budget. The guardrails are:

- **Budget caps:** `.delivery.yml` can bound the number of delivery runs, total
  worker attempts, token usage, and optionally parsed monetary cost.
- **Loop circuit-breaker:** repeated waves or attempts that do not make
  material delivery progress freeze the affected unit and ask for human input.

These guardrails harden existing `dispatch-wave` and `recover` behavior. They
do not introduce a daemon, a background conductor tick, automatic merging, or a
dependency on reliable LLM `loopreview` output.

## Decisions

1. **Guardrails are pre-dispatch gates.** `dispatch-wave` and `recover` check
   guardrails before starting a new Worker invocation. Adoption of an existing
   PR is still allowed because it does not spend another Worker attempt.
2. **Configuration is opt-in and lives in `.delivery.yml`.** Missing
   `guardrails` config keeps current behavior, including the existing
   `resilience.worker.max_attempts` per-issue retry limit.
3. **Budget evidence comes from durable state.** The implementation reads
   `.loopcoder/runs/<RunId>/` attempt state, guardrail state, and attestation
   records when present. State pulled from `loopcoder/state` is advisory in the
   same way as existing run state.
4. **Token caps use existing attestation usage.** Worker, Verifier, and
   Conductor attestations already carry token usage. Budget accounting sums
   known usage records in the delivery scope and fails closed when a configured
   cap cannot be evaluated from available evidence.
5. **Cost caps are optional and exact-only.** loopcoder must not estimate cost
   from public pricing tables or model names. A cost ceiling is enforceable only
   when provider output or a future attestation extension supplies parsed
   monetary cost. If a cost ceiling is configured but exact cost evidence is
   unavailable, the result is `needs-human`.
6. **A circuit trip is `needs-human`, not `fail`.** The system may be working
   correctly while the task is unclear, too expensive, or repeatedly producing
   no useful change. Human input is required before another attempt.
7. **Freeze is per GitHub issue.** The unit frozen by a circuit-breaker is the
   issue. Sibling issues in the same wave may continue if they have not tripped
   their own guardrails.

## Definitions

- **Delivery scope:** the set of issue numbers and base branch that the
  conductor is trying to deliver as one batch. The first implementation should
  derive a stable scope id from the sorted issue numbers plus base branch and
  store it in guardrail state. If only one issue is known, that issue and base
  branch form the scope.
- **Run:** one `.loopcoder/runs/<RunId>/` directory. A repeated conductor
  session may create a new run id for the same delivery scope.
- **Wave:** one `loopcoder dispatch-wave` invocation.
- **Attempt:** one Worker attempt recorded in `workers/*.attempt.json`.
- **Material progress:** a change that moves an issue toward a deliverable:
  a new or adopted PR, a merged closing PR, a new commit or non-empty diff on an
  issue branch, or a dependency becoming satisfied.
- **No progress:** retries, recovery briefs, attestation records, heartbeats,
  log growth, repeated check states, or repeated failures that do not create a
  new PR, commit, useful branch diff, merged issue, or newly satisfied
  dependency.

## `.delivery.yml` Configuration

All fields are optional. Omit a field to leave that cap disabled. A numeric
field set to zero or a negative value is invalid; it must not silently mean
"unlimited".

```yaml
guardrails:
  budget:
    max_runs: 3
    max_total_attempts: 12
    max_total_tokens: 500000
    max_total_cost_usd: 25.00
  circuit_breaker:
    max_no_progress_waves: 2
    max_no_progress_attempts: 3
```

| Field | Meaning |
|---|---|
| `guardrails.budget.max_runs` | Maximum distinct run ids that may contain work for one delivery scope. |
| `guardrails.budget.max_total_attempts` | Maximum Worker attempts across all issues in one delivery scope. |
| `guardrails.budget.max_total_tokens` | Maximum known attested total tokens across all model invocations in one delivery scope. |
| `guardrails.budget.max_total_cost_usd` | Optional maximum exact parsed monetary cost in USD across one delivery scope. |
| `guardrails.circuit_breaker.max_no_progress_waves` | Maximum consecutive waves without material progress for an issue. |
| `guardrails.circuit_breaker.max_no_progress_attempts` | Maximum consecutive Worker attempts without material progress for an issue. |

These keys must not change model or effort inheritance. They only decide
whether a new delivery attempt is allowed to start.

## Budget Enforcement

The budget check runs after normal ready-set or recovery reconciliation has
identified a candidate for a new Worker attempt, and before the Worker provider
is invoked.

Evaluation order:

1. Resolve the delivery scope from guardrail state, selected issue numbers, and
   base branch.
2. Load all known run ids, attempts, guardrail decisions, and attestation usage
   records for that scope.
3. Count the new action that would be started. For example, starting an
   attempt increments `max_total_attempts`, and generating a new run id
   increments `max_runs` when the run id was not already part of the scope.
4. If any configured cap would be exceeded, do not invoke the Worker. Return
   `needs-human` with the exact cap, current total, proposed increment, run ids,
   issue numbers, and latest recovery brief if one exists.
5. If configured evidence is unavailable or corrupt, fail closed with
   `needs-human` instead of assuming the budget is safe.

Token and cost caps are known-total caps, not future-spend predictions.
loopcoder can block once known usage reaches or exceeds the cap, but it cannot
guarantee the next provider invocation will stay below a token or cost ceiling
unless a separate provider-level limit exists. Provider-level token limiting is
outside this spec.

## Circuit-Breaker

The circuit-breaker records consecutive no-progress outcomes per issue.

For `dispatch-wave`, a selected issue gets one wave outcome:

- progress when the wave opens or adopts a PR, produces a new commit or branch
  diff for that issue, or observes that a dependency became satisfied;
- no progress when the issue is skipped, fails before useful changes, repeats
  the same blocked state, or only writes logs, recovery context, or attestation.

For `recover`, each retry attempt gets one attempt outcome:

- progress when recovery adopts an existing PR, creates useful branch changes,
  or opens a PR;
- no progress when recovery reaches the same failure class again, exits idle,
  or only writes another recovery brief.

When either configured threshold is reached, the issue is frozen:

- `dispatch-wave` must not dispatch that issue in the current wave.
- `recover` must not start another retry for that issue.
- `ready-set` and `resume` should classify the issue as `needs-human` or
  `guardrail-frozen` when the relevant guardrail state is available.
- The report must include the no-progress streak, the last material progress
  timestamp if known, attempt history, latest recovery brief, and concrete
  choices for the human: clarify the issue, raise a cap, close or supersede the
  issue, or explicitly start a new scoped run.

Heartbeats and log growth prevent liveness false positives, but they are not
delivery progress for circuit-breaker accounting.

## Guardrail State

The first implementation should add a small machine-readable guardrail ledger
under each run:

```text
.loopcoder/runs/<RunId>/guardrails/<issue-number>.json
```

Suggested shape:

```json
{
  "version": 1,
  "delivery_scope_id": "main:192",
  "run_id": "run-20260628T120000Z-wave",
  "issue": 192,
  "status": "needs-human",
  "reason": "circuit-breaker.max_no_progress_attempts",
  "observed": {
    "runs": 1,
    "total_attempts": 3,
    "total_tokens": 154000,
    "no_progress_waves": 1,
    "no_progress_attempts": 3
  },
  "decision_at": "2026-06-28T12:00:00Z"
}
```

This state is local and state-branch publishable like existing attempt state. It
is not a replacement for GitHub as the delivery source of truth. Automatic
GitHub label mutation is not required for v0.3.2; a human may still add a
`needs-human` label for visibility.

## Command Behavior

### `dispatch-wave`

`dispatch-wave` already preflights selected issues against ready-set. After that
preflight and before calling `dispatch`, it must evaluate guardrails for each
ready issue.

An over-budget or frozen issue is not dispatched. The wave summary should keep
the existing per-issue shape and add a `needs-human` status or equivalent
classification that includes:

- guardrail name;
- configured cap;
- observed total or streak;
- whether any sibling issue continued;
- the latest attempt path and recovery brief when available.

Successful sibling dispatches must not be rolled back.

### `recover`

`recover` must continue to adopt an existing open PR before checking whether to
spend a new retry attempt. If no PR can be adopted, it evaluates guardrails
before sleeping, creating a retry branch, or invoking a Worker.

When a guardrail trips, `recover` returns a blocked `needs-human` report. The
report should include the same evidence as the existing retry-limit report plus
the budget or circuit-breaker reason.

### `ready-set` And `resume`

`ready-set` and `resume` are read-only. They should read guardrail state when it
is available and classify frozen issues as non-ready. Missing guardrail state
does not mutate anything, but if a cap is configured and evidence is too
incomplete to evaluate, reports must say so instead of marking the issue safe.

## Process And Follow-Up Issues

This design PR is the only deliverable for issue #192.

After this spec merges, code issues should be filed in this order:

1. **G1 budget caps:** implement config parsing, guardrail ledger reading and
   writing, budget evaluation, and `dispatch-wave` / `recover` enforcement per
   `docs/specs/0192-delivery-guardrails.md`.
2. **G2 loop circuit-breaker:** implement no-progress accounting and freezing.
   This issue is blocked by G1 because it should reuse the guardrail ledger and
   report shape.

G1 and G2 do not depend on `loopreview` reliability. Verification for those
code issues can use deterministic config, state, and command-output tests.

## Acceptance Criteria For Code Issues

- `.delivery.yml` parses the optional `guardrails` keys without making them
  required.
- Invalid numeric caps fail loudly.
- With no `guardrails` section, current behavior remains unchanged except for
  existing resilience behavior.
- `dispatch-wave` refuses to start a Worker for an issue that would exceed a
  configured budget cap.
- `recover` adopts an existing PR without spending a retry, but refuses a new
  retry that would exceed a configured budget cap.
- Token budget tests use attestation usage records; cost budget tests cover
  both exact-cost evidence and the fail-closed unavailable-cost path.
- Repeated no-progress waves or attempts freeze only the affected issue and
  report `needs-human`.
- `ready-set` and `resume` show frozen or budget-blocked issues as non-ready
  when guardrail state is present.
- Reports include the cap or threshold, observed count or usage, affected issue,
  run id, and next human decision.

## Non-Goals

- No Go implementation in this design-doc PR.
- No `.delivery.yml` policy change in this design-doc PR.
- No autonomous conductor tick, daemon, cron loop, or cloud scheduler.
- No automatic merge, close, branch deletion, or PR mutation.
- No dependency on reliable LLM verifier output.
- No estimated cost calculation from model names or external pricing.
- No weakening of the human merge gate, doc-first workflow, or model/effort
  inherit-by-default rule.
