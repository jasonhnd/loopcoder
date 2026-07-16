---
id: 968
title: v0.8.0 Orchestration Cost Budget
status: draft
date: 2026-07-16
issue: 968
pr: null
supersedes: []
superseded_by: []
---

# v0.8.0 Orchestration Cost Budget

This spec freezes the local accounting and fail-closed contract for issue
[#968](https://github.com/jasonhnd/loopcoder/issues/968). It builds on the
provider usage rules in [`0803-quota-usage-budget.md`](0803-quota-usage-budget.md)
without claiming that LoopCoder can observe host-account or out-of-band usage.

## Contract

Every unattended run emits `loopcoder.orchestration_cost.v1` with stable role
rows for `planner`, `worker`, `verifier`, `recovery`, `delivery`, and `waiting`.
Events record model calls, reported tokens, lifecycle duration, deterministic
waits, retries, duplicate suppression, delivery-only retries, and bounded
context-packet bytes. Missing provider usage remains `unknown`; it is never
rendered or added as zero.

The following activities are deterministic and must reject any non-zero model
call or token count: waiting, heartbeat, progress-receipt generation, CI
polling, approval polling, quota polling, and delivery retry. Their timing,
retry, and suppression evidence remains observable without provider work.

`worker` and `verifier` calls are useful execution. `planner`, `recovery`,
`delivery`, and `waiting` calls are coordination overhead. The release ratio is
`overhead_tokens / useful_tokens`; it is exact only when every contributing
LoopCoder-owned call has exact reported totals. Unknown input makes the ratio
unknown and therefore not release-eligible. A ratio above the configured
threshold is `needs-human`.

## Budget

`.delivery.yml` exposes `orchestration.cost_budget`:

```yaml
orchestration:
  cost_budget:
    max_model_calls: 8
    max_tokens: 500000
    max_overhead_percent: 10
```

The call and token caps apply per run. Before starting another provider, the
orchestrator evaluates the already observed totals plus the proposed call. A
refusal records the cap, observed value, evidence, and remediation and returns
`needs-human`; it must not invoke a fallback provider. A cap discovered after
completed work preserves that work and blocks only the next automatic action.
Provider-free adoption and reconciliation run before this boundary. Budgeted
provider launches are advanced one at a time so a completed usage report can
reconcile the durable reservation before the next call is admitted.

A durable provider reservation without a terminal report is an unresolved
in-flight call and blocks every later provider launch. A terminal provider
report may truthfully leave token usage `unknown` when that provider does not
expose usage metadata. In that case the next useful worker or verifier call may
proceed under `max_model_calls`; LoopCoder records the explicit
`model-call-cap-fallback` evidence and never invents a token value. Unknown
token usage remains ineligible for automatic release and requires human review
at the release gate.

## Output and persistence

Tick text and JSON include the same normalized summary, role rows, budget
decision, external-host usage state, evidence, and remediation. The normalized
ledger is atomically persisted at
`.loopcoder/runs/<run-id>/orchestration-cost.json` and reloaded before a resumed
tick can start another provider. Event IDs are stable within a run and
duplicate IDs are suppressed rather than double counted. The report is local
runtime evidence and must not fabricate billing, prices, or unsupported host
totals.

Immediately before each LoopCoder-owned provider launch, the run persists a
one-call reservation with unknown usage. A normal return replaces that same
event with the provider report; a confirmed no-launch removes the current
reservation. Host loss leaves the unknown reservation durable, so restart
cannot silently repeat or undercount the call. Duplicate-event suppression is
itself represented by a provider-free ledger event so its count survives
normalization and reload.

## Acceptance mapping

- Unit tests prove every deterministic wait class reports zero calls/tokens.
- Unknown external-host and missing provider token usage render `unknown`.
- An unresolved provider reservation blocks another launch, while a terminal
  report with unknown usage falls back to the model-call cap.
- Retry classes and packet bytes remain independently visible.
- Call/token exhaustion blocks the next provider without cancelling completed
  work or invoking a fallback.
- The integrated tick release gate refuses overhead ratios above ten percent
  by default and emits stable human and JSON remediation.
