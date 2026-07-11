---
id: 646
title: Nested Sub-Agent Plan Schema and Parent-Child Run IDs
status: accepted
date: 2026-07-09
issue: 646
pr: 702
supersedes: []
superseded_by: []
---

# Nested Sub-Agent Plan Schema and Parent-Child Run IDs

This accepted spec defines the nested sub-agent orchestration contract shipped
in the v0.7.0 candidate. It covers the run tree model, child plan JSON
contract, durable parent-child edges, report representation, max-depth
defaults, deterministic aggregation rules, and the implementation alignment
landed through PRs #701 and #702.

## Goals

- Represent nested sub-agent work as normal loopcoder runs with explicit parent
  metadata.
- Make parent-child run relationships durable enough for recovery, reporting,
  and audit.
- Require every child unit to declare its scope and permission before it can be
  scheduled.
- Define bounded depth and concurrency defaults so nested orchestration cannot
  recurse without an explicit configured limit.
- Define stable aggregation behavior for machine-readable report JSON.

## Non-Goals

- No scheduler implementation in this design PR.
- No provider-specific nested-agent implementation.
- No unbounded recursion.
- No change to existing dispatch, loopreview, tick, report, or state branch
  behavior until a later code issue implements this spec.
- No hidden mutation channel for child runs. A child can only mutate paths
  allowed by its declared scope and permission.

## Terms

- **Root run:** A run with no parent. Existing conductor, dispatch-wave, tick,
  or manual dispatch runs are root runs unless they are launched by a parent
  child plan.
- **Parent run:** A normal run that owns a child plan and records edges to one
  or more child runs.
- **Child run:** A normal run launched from one child plan item. The child has
  its own `run_id`, attempt state, reports, logs, and recovery records.
- **Run edge:** A durable parent-child relationship from one parent run to one
  child run.
- **Child plan:** The parent-authored JSON document that declares candidate
  child runs, their scopes, permissions, dependency order, and aggregation
  behavior before scheduling.
- **Execution claim:** A durable lease row granting one scheduler/executor
  exclusive permission to launch a child provider for one `run_id` generation.
  Stable child identity is not sufficient by itself; provider launch requires an
  active claim owned by the launching executor.

## Run Tree Model

The run graph is a bounded tree:

- Every run has a stable `run_id`.
- A root run has `parent_run_id: null`.
- A child run has exactly one `parent_run_id`.
- A run can have zero or more children.
- A child run is still a normal run. Existing run-local records remain under
  `.loopcoder/runs/<child-run-id>/` during the repo-local-state era and under
  the future storage backend by the same logical `run_id`.
- A run MUST NOT be its own ancestor.
- The `(root_run_id, run_id)` pair MUST identify exactly one node in the tree.
- `depth` starts at `0` for root runs and increments by one for each child
  edge.
- A scheduler MUST refuse to create a child whose computed depth exceeds the
  configured `max_depth`.

The minimum run metadata needed by future storage and report code is:

```json
{
  "run_id": "run-parent",
  "parent_run_id": null,
  "root_run_id": "run-parent",
  "depth": 0,
  "origin": "conductor",
  "status": "running"
}
```

For a child:

```json
{
  "run_id": "run-child-a",
  "parent_run_id": "run-parent",
  "root_run_id": "run-parent",
  "depth": 1,
  "origin": "sub_agent",
  "status": "running"
}
```

## Child Plan Schema

Child plans are versioned JSON documents. The first version is
`loopcoder.child_plan.v1`.

```json
{
  "schema_version": "loopcoder.child_plan.v1",
  "plan_id": "plan-run-parent-001",
  "parent_run_id": "run-parent",
  "root_run_id": "run-parent",
  "parent_depth": 0,
  "max_depth": 2,
  "max_concurrency": 3,
  "created_at": "2026-07-09T00:00:00Z",
  "items": [
    {
      "child_key": "docs-pass",
      "title": "Review docs contract",
      "role": "worker",
      "scope": {
        "repo": ".",
        "paths": ["docs/specs/"],
        "issues": [646],
        "commands": ["go test ./..."]
      },
      "permission": "read-only",
      "depends_on": [],
      "aggregation": {
        "mode": "collect",
        "required": true,
        "include_report": true
      }
    }
  ]
}
```

### Required Plan Fields

| Field | Required | Description |
| --- | --- | --- |
| `schema_version` | yes | Must be `loopcoder.child_plan.v1` for this contract. |
| `plan_id` | yes | Stable parent-local plan identifier. |
| `parent_run_id` | yes | The run that owns this child plan. |
| `root_run_id` | yes | The root ancestor for all child items. |
| `parent_depth` | yes | Depth of `parent_run_id` when the plan is created. |
| `max_depth` | yes | Maximum allowed depth for descendants created from this plan. |
| `max_concurrency` | yes | Maximum number of child items from this plan that may run at once. |
| `created_at` | yes | RFC3339 timestamp. |
| `items` | yes | Ordered child plan items. |

### Required Item Fields

| Field | Required | Description |
| --- | --- | --- |
| `child_key` | yes | Stable key unique within `plan_id`; used before a child `run_id` exists. |
| `title` | yes | Human-readable child work title. |
| `role` | yes | Logical role such as `worker`, `verifier`, `auditor`, or `sub_agent`. |
| `scope` | yes | The allowed repo, path, issue, PR, command, and data boundary. |
| `permission` | yes | One of `read-only`, `write`, or `orchestrate`. |
| `depends_on` | yes | Ordered list of sibling `child_key` values that must finish first. |
| `aggregation` | yes | How the parent consumes this child's result. |

`scope` MUST be present and non-empty. A child item with `permission: write`
MUST declare at least one path, issue, PR, or other bounded mutable resource.
An implementation MUST reject a write-capable child with an unbounded repository
scope such as `"paths": ["**"]` unless a later accepted spec defines a safer
escape hatch.

`permission` is a maximum, not an instruction to mutate. The implementation uses
the repository's canonical reporter permission enum rather than introducing a
second nested-only enum. Earlier draft examples used `read` and `review`; code
MUST normalize legacy in-process `read` to `read-only` where compatibility is
needed, and MUST fail closed on unknown persisted JSON values. The initial
permission levels are:

| Permission | Meaning |
| --- | --- |
| `read-only` | Inspect files, local state, and provider output only. |
| `write` | Mutate only resources declared in `scope`. |
| `orchestrate` | Create child plans or child runs within the same max-depth and scope rules. |

## Durable Storage Schema

Future storage backends MUST represent runs and edges separately. A backend may
denormalize for lookup speed, but the logical schema is:

```sql
CREATE TABLE runs (
  id TEXT PRIMARY KEY,
  project_id TEXT NULL,
  parent_run_id TEXT NULL,
  issue_number INTEGER NULL,
  root_run_id TEXT NOT NULL,
  depth INTEGER NOT NULL,
  origin TEXT NOT NULL,
  status TEXT NOT NULL,
  started_at TEXT NULL,
  ended_at TEXT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE child_plans (
  plan_id TEXT PRIMARY KEY,
  parent_run_id TEXT NOT NULL,
  root_run_id TEXT NOT NULL,
  schema_version TEXT NOT NULL,
  max_depth INTEGER NOT NULL,
  max_concurrency INTEGER NOT NULL,
  plan_json TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE run_edges (
  parent_run_id TEXT NOT NULL,
  child_run_id TEXT NOT NULL,
  edge_type TEXT NOT NULL DEFAULT 'child',
  root_run_id TEXT NOT NULL,
  plan_id TEXT NOT NULL,
  child_key TEXT NOT NULL,
  depth INTEGER NOT NULL,
  ordinal INTEGER NOT NULL,
  scope_json TEXT NOT NULL,
  permission TEXT NOT NULL,
  aggregation_json TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (parent_run_id, child_run_id),
  UNIQUE (plan_id, child_key),
  UNIQUE (parent_run_id, ordinal)
);

CREATE TABLE run_claims (
  run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
  executor_id TEXT NOT NULL,
  claim_generation INTEGER NOT NULL,
  claimed_at TEXT NOT NULL,
  lease_expires_at TEXT NOT NULL,
  heartbeat_at TEXT NOT NULL,
  phase TEXT NOT NULL DEFAULT 'claimed',
  provider_idempotency_key TEXT NOT NULL DEFAULT '',
  provider_receipt TEXT NOT NULL DEFAULT ''
);
```

Required storage invariants:

- `run_edges.child_run_id` MUST reference a row in `runs`.
- `run_edges.parent_run_id` MUST reference a row in `runs`.
- `runs.parent_run_id`, when non-null, MUST match exactly one
  `run_edges.parent_run_id` for the same `runs.run_id`.
- `run_edges.depth` MUST equal `runs.depth` for `child_run_id`.
- `run_edges.root_run_id` MUST equal both the parent and child `root_run_id`.
- `ordinal` is assigned from the ordered child plan item list and is stable for
  deterministic report rendering.
- The storage layer MUST reject cycles.
- A child provider MUST NOT launch unless the scheduler atomically inserts or
  takes over a `run_claims` row for that child and transitions the child run and
  parent edge to `running` in the same write transaction.
- Claim acquisition MUST distinguish `claimed`, `already-running`,
  `terminal-reused`, `blocked`, and stale-claim takeover. Only the claim owner
  for the current generation may write terminal completion.
- Claim owners MUST record the current claim phase. `claimed` means the
  provider has not launched; `launching` and `executing` are ambiguous after
  lease expiry and MUST fail closed to `needs-human` rather than auto-take over.
- While a provider is executing, the scheduler MUST renew `heartbeat_at` and
  `lease_expires_at` with a fenced `run_id`, `executor_id`, and
  `claim_generation` predicate. Renewal stops and joins before completion.
- Provider launch MUST carry a durable idempotency key or receipt when the
  provider supports it. The key is metadata for recovery, not a universal
  exactly-once guarantee. The idempotency key is bound to the logical child
  operation, not to `claim_generation`; generation is a fencing value only.
- `provider_receipt` MUST start empty and MUST NOT be fabricated from the
  idempotency key, status, or claim metadata. It may be filled only from a real
  provider response, external resource identifier, or verifiable local
  execution record. Recovery MUST treat a missing receipt as no proof of
  provider completion.
- Terminal completion MUST be fenced by `run_id`, `executor_id`, and
  `claim_generation` so an older owner cannot overwrite a newer recovery owner.
- If terminal child completion persistence fails for any reason, that scheduler
  MUST NOT publish child finished events or parent terminal state. The parent
  may become terminal only after every aggregation-participating child terminal
  state has persisted successfully.
- Write-intent storage paths use an immediate SQLite write transaction with a
  bounded retry around the full database-only transaction after `SQLITE_BUSY`.

The v0.7 SQLite migration is additive over the existing storage schema: `runs`
keeps its established primary key column name `id`, and v7 adds
`root_run_id`, `depth`, `origin`, and `created_at` rather than rebuilding the
table. Existing minimal `run_edges` rows remain valid; v7 adds the enriched
columns and enforces plan child-key and parent ordinal uniqueness only for
enriched nested edges.

The v8-to-v9 claim lifecycle migration is conservative for existing claims.
Legacy claim rows do not prove whether a provider launched, so every
non-terminal legacy claim MUST migrate to an ambiguous execution phase such as
`executing`. After expiry, those rows fail closed to `needs-human` rather than
automatic takeover. Only a post-v9 `claimed` phase created before provider
launch is eligible for automatic expired-lease takeover.

During the repo-local-state transition, the scheduler MUST keep the
`.loopcoder/runs/` event mirror consistent with the SQL graph by writing the
accepted child plan and queued SQL edges before emitting queued compatibility
events, and by writing terminal SQL edge/run status before emitting terminal
compatibility events. Status/report tree renderers may continue to read the
compatibility mirror until their storage read path is replaced, but the SQL
graph is the authoritative recovery source for accepted plans and edges.

## Report JSON Contract

Report JSON MUST be able to represent both a single run and a run tree. The
existing top-level report remains valid for a root run. Tree-aware reports add
optional parent and child fields:

```json
{
  "run_id": "run-parent",
  "parent_run_id": null,
  "root_run_id": "run-parent",
  "depth": 0,
  "report": {
    "role": "worker",
    "provider": "codex",
    "action": "implement issue #646",
    "verified": true
  },
  "children": [
    {
      "run_id": "run-child-a",
      "parent_run_id": "run-parent",
      "root_run_id": "run-parent",
      "depth": 1,
      "child_key": "docs-pass",
      "permission": "read-only",
      "scope": {
        "repo": ".",
        "paths": ["docs/specs/"]
      },
      "aggregation": {
        "mode": "collect",
        "required": true,
        "include_report": true
      },
      "status": "succeeded",
      "report": {
        "role": "worker",
        "provider": "codex",
        "action": "review docs contract",
        "verified": true
      }
    }
  ],
  "aggregation": {
    "status": "succeeded",
    "required_total": 1,
    "required_succeeded": 1,
    "required_failed": 0,
    "optional_failed": 0
  }
}
```

Tree fields are additive and optional for older consumers:

- `parent_run_id` is `null` for root runs and a string for child runs.
- `root_run_id` is always the root ancestor.
- `depth` is always an integer.
- `children` is an ordered array sorted by `run_edges.ordinal`, then
  `child_run_id` as a deterministic tie-breaker.
- Each child entry MUST include `child_key`, `scope`, `permission`,
  `aggregation`, `status`, and `run_id`.
- A child entry MAY omit its nested `report` only when
  `aggregation.include_report` is `false` or the child has not produced a
  report yet. The omission must not hide the child edge.

## Max Depth and Concurrency Defaults

Default nested execution limits:

| Setting | Default | Meaning |
| --- | --- | --- |
| `max_depth` | `2` | Root at depth `0`, children at depth `1`, grandchildren at depth `2`. |
| `max_concurrency` | `3` | At most three children from one plan run at a time. |

Configured limits MAY lower these defaults. Raising them requires explicit
configuration in a later implementation issue. The absolute hard cap for
v1-style nested orchestration is `max_depth <= 4`; implementations MUST reject
higher values unless a later accepted spec changes the cap.

When `parent_depth >= max_depth`, a parent may still aggregate existing
children but MUST NOT create another child plan item that would spawn a run.

## Aggregation Rules

Aggregation is deterministic and stable in JSON:

- Parent aggregation walks direct children in `ordinal` order.
- Recursive aggregation walks depth-first in `ordinal` order.
- Required children affect parent status; optional children are reported but do
  not fail the parent.
- A required child with status `failed`, `needs-human`, `hung`, `idle`, or
  `blocked` makes the parent aggregate status `needs-human` unless a later
  scheduler-specific spec defines an automatic bounded recovery pass.
- A required child with status `running` or `pending` makes the parent aggregate
  status `running`.
- If every required child is `succeeded` and at least one optional child failed,
  parent aggregate status is `succeeded_with_optional_failures`.
- If every required child is `succeeded` and no optional child failed, parent
  aggregate status is `succeeded`.
- Token usage, durations, and counts MUST remain per-report fields. Aggregators
  MAY add summary totals, but they MUST NOT rewrite child report records or
  merge multiple child reports into one synthetic role record.

The stable aggregation object uses these fields:

```json
{
  "status": "needs-human",
  "required_total": 2,
  "required_succeeded": 1,
  "required_failed": 1,
  "optional_total": 1,
  "optional_failed": 0,
  "child_run_ids": ["run-child-a", "run-child-b", "run-child-c"]
}
```

`child_run_ids` MUST be emitted in deterministic child order. Consumers MUST NOT
infer aggregation order from object key iteration.

## Recovery and Visibility

Nested work does not change the existing recovery principle that GitHub issues,
PRs, checks, and explicit local run records are the source of truth.

- A parent can recover its child list from `run_edges`.
- A child can recover its parent from `runs.parent_run_id` and `run_edges`.
- If a child run is interrupted, recovery is bounded by the same retry and
  liveness rules as normal runs.
- If a scheduler observes another active claim, it must not execute the child.
  It returns a non-error observation with the owner, generation, lease, and
  replay action so the parent can observe or retry after the lease state changes.
- If a process crashes after claiming but before provider launch, observers see
  the active lease and `claimed` phase. After expiry, recovery may take over
  with a new generation because the durable phase proves the provider did not
  launch.
- If a process crashes during provider execution, the active lease prevents a
  duplicate launch while heartbeats renew it. If the lease expires in
  `launching` or `executing`, recovery MUST mark or return `needs-human` rather
  than launch a duplicate provider. Stale completions are rejected and must not
  publish finished events.
- If a process crashes after external side effects but before terminal
  persistence, loopcoder does not claim universal exactly-once side effects.
  Recovery uses durable ownership, fencing, provider idempotency keys or
  receipts where available, and `needs-human` when completion cannot be proven.
  A stored idempotency key without a receipt is not proof that the provider
  completed.
- If cancellation happens during provider execution, terminal child persistence
  uses a bounded cleanup context that is independent of the cancelled caller
  context. Rollback paths also use independent cleanup context and discard a
  connection if rollback cannot be proven.
- Cancellation while another scheduler owns the claim is an observation path:
  the cancelling scheduler must not cancel or overwrite the owner unless a
  later explicit recovery policy takes over an expired lease.
- Parent reports MUST surface child runs even when a child has no final report,
  so interrupted or hidden child work cannot disappear from the tree.
- Local-only reporter and relay records remain local-only. Nested reports do not
  make them safe for PR bodies, issue comments, commits, merge artifacts, docs,
  examples, fixtures, or tracked files.

## Validation Requirements for Future Code Issues

The implementation issue for this spec must include:

- Schema tests that reject cycles, duplicate `child_key` values, duplicate
  child ordinals, mismatched `root_run_id`, and child depth above `max_depth`.
- JSON contract tests for root reports, child reports, parent reports with
  ordered `children`, and deterministic aggregation fields.
- Permission/scope validation tests proving that every child has scope and
  permission, and that write-capable children cannot declare an unbounded
  mutable scope.
- Backward-compatibility tests proving existing flat report JSON still parses
  when tree fields are absent.

## Acceptance Criteria Mapping

- **Design spec exists:** this document is the design spec for issue #646.
- **Storage schema supports run edges:** `runs`, `child_plans`, and
  `run_edges` are defined with parent-child invariants.
- **Report JSON can represent parent/child relationships:** tree-aware report
  fields are defined as additive JSON contract fields.
- **Max depth behavior is documented:** defaults, hard cap, and refusal behavior
  are defined in "Max Depth and Concurrency Defaults".
