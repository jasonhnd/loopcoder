---
id: 801
title: v0.8.0 Phase 1 DeliveryRun Contracts
status: draft
date: 2026-07-11
issue: 775
pr: null
supersedes: []
superseded_by: []
---

# v0.8.0 Phase 1 DeliveryRun Contracts

This documentation-only spec freezes the phase-1 contract that code issue
[#721](https://github.com/jasonhnd/loopcoder/issues/721) must implement for the
v0.8.0 DeliveryRun workflow in epic
[#715](https://github.com/jasonhnd/loopcoder/issues/715). It is the delivery
order step 1 design called for by roadmap
[#714](https://github.com/jasonhnd/loopcoder/issues/714): freeze contracts,
threat model, simulations, and migration strategy before any v0.8 runtime,
schema, CLI, or workflow behavior changes merge.

The hard start gate in #714 and #715 still applies. Until v0.7.0 has a public
signed release, the recorded decision is GO, #632 and #713 are closed, and no
v0.7.0 `release-blocker` remains open, this spec authorizes only design and
test-spec work. Implementation issues must follow [`../PROCESS.md`](../PROCESS.md):
this document merges first, then code implements per this accepted contract.

## Goals

- Define versioned durable records for DeliveryRun, Task, dependency edge,
  Attempt, Decision, Approval, Override, and immutable plan fingerprint.
- Define stable identifiers, Go/SQLite/JSON representation strategy, version
  fields, provenance, timestamps, side-effect classes, policy version, and
  typed errors for those records.
- Freeze DeliveryRun and Task lifecycle state machines as exhaustive transition
  tables.
- Bind approvals and overrides to exact plan, policy, and input fingerprints so
  changed work cannot be authorized by stale consent.
- Define idempotency, duplicate replay, and crash recovery semantics without
  weakening the accepted nested run claim/lease contract in
  [`0646-nested-sub-agent-plan.md`](0646-nested-sub-agent-plan.md).
- Define v0.7 schema-v9 to v0.8 forward migration, backup metadata, rollback
  expectations, multi-project isolation, and conservative handling of ambiguous
  legacy state.
- Restate the #714 decision ownership boundaries as normative v0.8 rules.
- Provide only outline-level shared conventions for later durable concepts that
  belong to phase specs 0802-0805.

## Non-Goals

- No Go implementation, SQLite migration, CLI behavior, workflow change, or
  reference-doc rewrite in this issue.
- No routing heuristics, provider-specific behavior, quota math, guided UX
  flows, or model selection policy.
- No provider credential probing or provider-native sub-agent federation schema.
- No replacement for the nested run, execution claim, lease, fencing,
  idempotency-key, and provider-receipt semantics already accepted in
  [`0646-nested-sub-agent-plan.md`](0646-nested-sub-agent-plan.md).
- No repository-tracked runtime payloads. Runtime state remains machine-local
  under the global project storage model from
  [`0639-global-data-layout-project-identity.md`](0639-global-data-layout-project-identity.md).

## Terms

**DeliveryRun** is the durable root workflow record for one guided delivery. A
DeliveryRun may contain a side-effect-free plan, user or policy approvals,
tasks, dependency edges, attempts, decisions, overrides, reporter links, and
terminal outcome.

**Task** is one planned unit of work inside a DeliveryRun. It is not necessarily
a GitHub issue. A Task may later dispatch a normal Worker run, a Verifier run,
or a provider-native child run, but this phase only freezes the durable task
contract.

**Dependency edge** is a directed edge between two tasks in the same project and
DeliveryRun. The task graph must be a DAG.

**Attempt** is one execution try for a task. Attempts align with the existing
run, reporter, recovery, and nested execution-claim concepts.

**Decision** is a durable consequential choice made by a user, deterministic
policy engine, planner, router, scheduler, or provider-native sub-agent.

**Approval** is explicit authorization for a specific fingerprint-bound plan,
policy, and input set.

**Override** is explicit authorization to bypass or relax a normally blocking
policy outcome for the exact same fingerprint-bound work.

**Plan fingerprint** is the immutable content hash of the canonical planned work
and its authority inputs. It is not a display label and not a mutable run field.

**Consequential decision** means any decision that changes scope, permission,
side effects, provider launch, budget, routing eligibility, scheduling order,
approval requirement, override requirement, or terminal outcome.

**Reporter** means the local-only report subsystem defined by
[`0567-reporter.md`](0567-reporter.md). DeliveryRun records may reference
report IDs and report summaries, but reporter blocks remain local-only and must
not be copied into PR bodies, issue comments, commits, fixtures, or tracked
docs except deliberate documentation examples.

## Representation Strategy

The v0.8 implementation must expose one logical contract across Go, SQLite, and
JSON:

- Go structs define typed fields and validation methods, but Go field names are
  not the public wire contract.
- JSON uses snake_case field names, explicit `schema_version` strings, and
  stable enum strings. JSON is the host-facing and fixture-facing contract.
- SQLite stores normalized rows for identity, lifecycle, and query fields.
  Bounded structured payloads may live in `*_json` columns only when they are
  not needed for joins, uniqueness, lifecycle transitions, or atomic policy
  checks.
- SQLite `TEXT` IDs are opaque. Callers must not parse timestamps, issue
  numbers, or provider names from IDs.
- Every mutating write that validates references, lifecycle state, approval
  freshness, idempotency, and side-effect permission must run in one immediate
  SQLite write transaction with the bounded busy-retry behavior used by the
  existing storage layer.
- Every record has `schema_version` and `record_version`. `schema_version`
  identifies the JSON/storage shape, such as `loopcoder.delivery_run.v1`.
  `record_version` starts at `1` and increments on optimistic updates to a
  mutable row. Immutable rows keep `record_version: 1`.
- All timestamps are UTC RFC3339 strings. Implementations may retain
  nanosecond precision, but canonical JSON must trim trailing fractional zeros
  and render `Z`, not a numeric offset.
- Unknown enum values in persisted records fail closed with
  `ErrUnknownRecordVersion` or `ErrInvalidRecord`, not best-effort guessing.

### Stable ID Scheme

ID prefixes are stable; the bytes after the prefix are opaque:

| Record | ID field | Required form |
| --- | --- | --- |
| Project | `project_id` | Existing `proj_<opaque>` from `0639-global-data-layout-project-identity.md`. |
| DeliveryRun | `delivery_run_id` / `run_id` | `run_<uuidv7-or-random-128-bit-base32>` for new v0.8 runs; migrated legacy run IDs keep their original value and record `legacy_id_source`. |
| Task | `task_id` | `task_<base32-sha256(project_id, delivery_run_id, task_key)>` truncated only to a collision-resistant length. |
| Dependency edge | `edge_id` | `edge_<base32-sha256(project_id, delivery_run_id, from_task_id, to_task_id, edge_kind)>`; `(delivery_run_id, from_task_id, to_task_id, edge_kind)` is also unique. |
| Attempt | `attempt_id` | `att_<base32-sha256(task_id, attempt_ordinal)>`; `attempt_ordinal` starts at `1` and is unique per task. |
| Decision | `decision_id` | `dec_<base32-sha256(project_id, delivery_run_id, decision_key, created_sequence)>`. |
| Approval | `approval_id` | `appr_<base32-sha256(project_id, delivery_run_id, authorization_fingerprint, approver_actor_id)>` plus a collision suffix when one actor approves the same fingerprint more than once. |
| Override | `override_id` | `ovr_<base32-sha256(project_id, delivery_run_id, authorization_fingerprint, override_kind, actor_id)>` plus a collision suffix when needed. |
| Fingerprint | `fingerprint_id` | The digest string itself: `sha256:<64-lower-hex>`. |

`task_key` is the planner-assigned stable key within the plan. It must be
unique within a DeliveryRun and remain stable across idempotent replay of the
same canonical plan. A changed task meaning requires a changed plan
fingerprint, not silent reuse of a task key with different authority.

## Common Fields

All durable records in this spec carry the following fields unless marked as
not applicable:

| Field | Meaning |
| --- | --- |
| `schema_version` | Stable JSON/storage shape string. |
| `record_version` | Optimistic update version for mutable records. |
| `project_id` | Required project identity from `0639`. Cross-project references are invalid. |
| `delivery_run_id` | Required DeliveryRun identity except on a pre-run project-level migration record. |
| `created_at` | UTC timestamp for initial persistence. |
| `updated_at` | UTC timestamp for latest mutation; immutable rows equal `created_at`. |
| `created_by` | Provenance object for the actor that created the record. |
| `updated_by` | Provenance object for the actor that last changed mutable state. |
| `host` | Host provenance object for the process or host session that persisted the record. |
| `policy_version` | Version string of the deterministic policy rules evaluated for the record. |
| `side_effect_class` | Maximum side-effect class authorized or attempted by the record. |
| `error_code` | Null for valid active records; typed error string for terminal error records. |
| `error_message` | Optional human diagnostic; not a stable machine contract. |

### Provenance

`created_by`, `updated_by`, `decided_by`, `approved_by`, and `overridden_by`
use this shape:

```json
{
  "actor_kind": "user",
  "actor_id": "local-user",
  "display": "Local user",
  "decision_authority": "user",
  "source": "cli"
}
```

Allowed `actor_kind` values are `user`, `policy-engine`, `planner`, `router`,
`scheduler`, `worker`, `verifier`, `provider-native-sub-agent`, `conductor`, and
`migration`.

Allowed `decision_authority` values are `user`, `deterministic-policy-engine`,
`planner`, `router`, `scheduler`, `provider-native-sub-agent`, `worker`,
`verifier`, `conductor`, and `migration`. The authority value must match the
ownership boundaries in this spec.

`host` uses this shape:

```json
{
  "host_kind": "codex",
  "host_id": "host-local-opaque",
  "session_id": "session-opaque",
  "process_id": 12345,
  "loopcoder_version": "0.8.0",
  "platform": "windows-amd64"
}
```

Host metadata is diagnostic. It must never replace actor authority, approvals,
or execution claims.

### Side-Effect Classes

Side-effect classes are ordered from least to most authority:

| Class | Meaning |
| --- | --- |
| `none` | Pure planning, validation, inspection, or deterministic computation. |
| `local-read` | Reads local files, config, git metadata, or machine-local loopcoder state. |
| `local-write` | Writes machine-local loopcoder runtime state only. |
| `repo-write` | Mutates files in a worktree or branch. |
| `git-remote-write` | Pushes branches, tags, or refs. |
| `github-write` | Creates or edits GitHub issues, PRs, labels, comments, checks, or releases. |
| `provider-launch` | Starts a provider CLI, model invocation, or provider-native sub-agent. |
| `external-write` | Writes to any external service other than GitHub or the selected provider launch contract. |

Each record stores the maximum class involved. A task or attempt may execute
only if its class is less than or equal to the class approved by policy,
approval, and any override.

### Typed Error Taxonomy

All atomic validation failures return a typed error. User-facing text may vary,
but JSON and tests use the stable code.

| Error | Required trigger |
| --- | --- |
| `ErrInvalidTransition` | Lifecycle event is not allowed from the current state. |
| `ErrTerminalState` | Mutation attempts to change a terminal record except allowed metadata backfill by migration. |
| `ErrCycleDetected` | Task dependency write would create a cycle. |
| `ErrCrossProjectReference` | A record references a project, run, task, approval, override, attempt, or report from another project. |
| `ErrMissingReference` | A required referenced record does not exist. |
| `ErrDuplicateRecord` | A unique record already exists with different intended identity. |
| `ErrDuplicateReplay` | Same idempotency key is replayed with different canonical request bytes. |
| `ErrStaleApproval` | Approval or override fingerprint does not exactly match current plan, policy, and input fingerprints. |
| `ErrPolicyDenied` | Deterministic policy rejects an operation without an applicable override path. |
| `ErrOverrideRequired` | Policy allows continuation only with an explicit override. |
| `ErrApprovalRequired` | Work cannot continue until an approval for the exact authorization fingerprint exists. |
| `ErrExpiredApproval` | Approval or override is past its `expires_at`. |
| `ErrSideEffectClassExceeded` | Attempted side effect is stronger than authorized. |
| `ErrClaimRequired` | Provider launch or task execution attempted without owning the current durable execution claim. |
| `ErrClaimConflict` | Another live owner holds the claim. |
| `ErrStaleClaim` | Completion or renewal uses an old executor or claim generation. |
| `ErrAmbiguousLegacyState` | Migration or recovery cannot prove intent, ownership, or launch status. |
| `ErrUnknownRecordVersion` | Persisted record version is newer or unknown to the binary. |
| `ErrInvalidRecord` | Required field, enum, timestamp, or canonical payload is invalid. |

## Durable Records

This section defines logical fields. SQLite column layout may denormalize for
query speed only when the same invariants are enforceable atomically.

### DeliveryRun

Schema version: `loopcoder.delivery_run.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `delivery_run_id` | yes | Stable run identity. Also rendered as `run_id` where existing run APIs require that term. |
| `project_id` | yes | Owning project. |
| `root_run_id` | yes | For a root DeliveryRun, equals `delivery_run_id`; nested runs follow `0646`. |
| `parent_run_id` | no | Parent run for nested orchestration, otherwise null. |
| `state` | yes | One DeliveryRun lifecycle state. |
| `intent_summary` | yes | Human-readable intent captured from user or host input. |
| `input_fingerprint` | yes after planning starts | Hash of canonical input bundle. |
| `policy_fingerprint` | yes after planning starts | Hash of canonical policy bundle. |
| `plan_fingerprint` | yes after canonical plan persistence | Hash of canonical plan bundle. |
| `authorization_fingerprint` | yes after canonical plan persistence | Hash binding input, policy, and plan fingerprints. |
| `policy_version` | yes | Deterministic policy version applied to the run. |
| `max_side_effect_class` | yes | Highest side-effect class any task may require. |
| `approval_status` | yes | `not-required`, `required`, `approved`, `rejected`, `expired`, or `stale`. |
| `override_status` | yes | `none`, `required`, `granted`, `expired`, `stale`, or `rejected`. |
| `created_at` / `updated_at` | yes | Lifecycle timestamps. |
| `planned_at` / `approved_at` / `started_at` / `ended_at` | conditional | State transition timestamps when those states occur. |
| `created_by` / `updated_by` / `host` | yes | Provenance. |
| `terminal_error_code` | no | Error code when terminal state is `failed` or `needs-human`. |
| `report_ids` | no | Local-only reporter record references. |
| `legacy_id_source` | no | Migration source for a v0.7 imported run ID. |

### Task

Schema version: `loopcoder.task.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `task_id` | yes | Stable task identity. |
| `task_key` | yes | Planner-stable key unique in the DeliveryRun. |
| `delivery_run_id` | yes | Owning DeliveryRun. |
| `project_id` | yes | Owning project. |
| `state` | yes | One Task lifecycle state. |
| `title` | yes | Human-readable task title. |
| `requirements_json` | yes | Canonical task requirements from planner output. |
| `scope_json` | yes | Repo, path, issue, PR, command, and data boundary. |
| `permission` | yes | Reporter permission enum: `read-only`, `write`, or `orchestrate`. |
| `side_effect_class` | yes | Maximum class this task may perform. |
| `policy_version` | yes | Policy version used for task validation. |
| `plan_fingerprint` | yes | Plan fingerprint this task belongs to. |
| `authorization_fingerprint` | yes if approval-gated | Fingerprint required for task execution authority. |
| `attempt_count` | yes | Number of persisted attempts. |
| `active_attempt_id` | no | Current attempt when claimed or running. |
| `depends_on` | yes | Ordered list of predecessor `task_id` values for JSON rendering. |
| `created_at` / `updated_at` | yes | Lifecycle timestamps. |
| `ready_at` / `started_at` / `ended_at` | conditional | State transition timestamps when applicable. |
| `created_by` / `updated_by` / `host` | yes | Provenance. |
| `terminal_error_code` | no | Terminal typed error. |

### Dependency Edge

Schema version: `loopcoder.dependency_edge.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `edge_id` | yes | Stable edge identity. |
| `delivery_run_id` | yes | Owning DeliveryRun. |
| `project_id` | yes | Owning project. |
| `from_task_id` | yes | Predecessor task. |
| `to_task_id` | yes | Successor task. |
| `edge_kind` | yes | `requires`, `blocks`, or `orders-after`. |
| `ordinal` | yes | Deterministic order from canonical plan. |
| `plan_fingerprint` | yes | Fingerprint that created this edge. |
| `created_at` / `updated_at` | yes | Timestamps. |
| `created_by` / `host` | yes | Provenance. |

Edges must be intra-project and intra-DeliveryRun. Inserts and updates must
check the full graph for cycles inside the same transaction. A cycle fails with
`ErrCycleDetected` and writes no partial edge.

### Attempt

Schema version: `loopcoder.attempt.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `attempt_id` | yes | Stable attempt identity. |
| `task_id` | yes | Owning task. |
| `delivery_run_id` | yes | Owning DeliveryRun. |
| `project_id` | yes | Owning project. |
| `attempt_ordinal` | yes | Monotonic attempt number per task. |
| `state` | yes | `planned`, `claimed`, `launching`, `running`, `succeeded`, `failed`, `cancelled`, `needs-human`, `stale`, or `superseded`. |
| `claim_generation` | conditional | Current generation for claim-owned execution. |
| `executor_id` | conditional | Claim owner for executing attempts. |
| `provider_idempotency_key` | conditional | Logical provider launch key, aligned with `0646`. |
| `provider_receipt` | no | Real provider response, external resource ID, or verifiable local execution record. |
| `side_effect_class` | yes | Maximum side-effect class attempted. |
| `started_at` / `ended_at` | conditional | Execution timestamps. |
| `created_at` / `updated_at` | yes | Persistence timestamps. |
| `created_by` / `updated_by` / `host` | yes | Provenance. |
| `report_id` | no | Local-only reporter record reference. |
| `terminal_error_code` | no | Terminal typed error. |

Attempt launch and completion must respect the execution-claim rules in
[`0646-nested-sub-agent-plan.md`](0646-nested-sub-agent-plan.md): a stable
`attempt_id` or `run_id` is not enough to launch a provider. The scheduler must
own the active claim generation, and stale completion must be rejected.

### Decision

Schema version: `loopcoder.decision.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `decision_id` | yes | Stable decision identity. |
| `decision_key` | yes | Caller-stable key for idempotent replay. |
| `decision_kind` | yes | `policy`, `planning`, `routing`, `scheduling`, `approval-request`, `override-request`, `recovery`, or `terminal-outcome`. |
| `delivery_run_id` | yes | Owning DeliveryRun. |
| `task_id` | no | Task when task-scoped. |
| `project_id` | yes | Owning project. |
| `decided_by` | yes | Actor authority that owns this decision. |
| `inputs_fingerprint` | yes | Canonical inputs used for the decision. |
| `output_json` | yes | Canonical decision result. |
| `alternatives_json` | conditional | Required for routing candidates and policy-denied alternatives when known. |
| `heuristic` | yes | Boolean. `true` only when the decision is not fully reproducible from persisted deterministic inputs. |
| `heuristic_reason` | conditional | Required when `heuristic` is `true`. |
| `policy_version` | yes | Policy version in force. |
| `side_effect_class` | yes | Maximum class authorized by the decision. |
| `created_at` | yes | Immutable timestamp. |
| `created_by` / `host` | yes | Provenance. |

Every consequential decision must be reproducible from `inputs_fingerprint` and
persisted inputs unless `heuristic: true` is explicitly set. Heuristic
decisions may rank or recommend; they must not bypass deterministic policy,
approval, or override requirements.

### Approval

Schema version: `loopcoder.approval.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `approval_id` | yes | Stable approval identity. |
| `delivery_run_id` | yes | Owning DeliveryRun. |
| `project_id` | yes | Owning project. |
| `approval_kind` | yes | `plan`, `task`, `side-effect`, or `continue`. |
| `authorization_fingerprint` | yes | Exact binding hash for input, policy, and plan. |
| `input_fingerprint` | yes | Input hash bound by approval. |
| `policy_fingerprint` | yes | Policy hash bound by approval. |
| `plan_fingerprint` | yes | Plan hash bound by approval. |
| `approved_side_effect_class` | yes | Maximum approved class. |
| `approved_scope_json` | yes | Canonical scope approved. |
| `approved_by` | yes | User or policy actor with approval authority. |
| `status` | yes | `active`, `rejected`, `expired`, `revoked`, or `stale`. |
| `approved_at` | conditional | Required when status is `active`. |
| `expires_at` | no | Expiry timestamp when policy requires one. |
| `created_at` / `updated_at` | yes | Timestamps. |
| `created_by` / `updated_by` / `host` | yes | Provenance. |

An approval authorizes only the exact authorization fingerprint and only up to
its approved side-effect class and scope. If any fingerprint input changes, the
approval is stale and cannot be used. Exact idempotency replay of the original
approval request may return the historical approval record, but that replay does
not refresh authority for the current run fingerprint.

### Override

Schema version: `loopcoder.override.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `override_id` | yes | Stable override identity. |
| `delivery_run_id` | yes | Owning DeliveryRun. |
| `project_id` | yes | Owning project. |
| `override_kind` | yes | `policy-denial`, `budget`, `scope`, `side-effect-class`, `routing`, or `recovery`. |
| `authorization_fingerprint` | yes | Exact binding hash for input, policy, and plan. |
| `policy_fingerprint` | yes | Policy hash whose result is overridden. |
| `policy_decision_id` | yes | Decision being overridden. |
| `overridden_by` | yes | User actor with override authority. |
| `justification` | yes | Human-readable reason. |
| `limits_json` | yes | Narrow replacement limits; overrides must not be unbounded. |
| `status` | yes | `active`, `expired`, `revoked`, `rejected`, or `stale`. |
| `created_at` / `updated_at` | yes | Timestamps. |
| `expires_at` | conditional | Required for budget, scope, side-effect, and recovery overrides. |
| `created_by` / `updated_by` / `host` | yes | Provenance. |

Overrides cannot change user intent silently. They only permit continuation
inside explicit replacement limits for the exact fingerprint-bound work. Exact
idempotency replay of the original override request may return the historical
override record, but that replay does not refresh authority for the current run
fingerprint.

### Immutable Plan Fingerprint

Schema version: `loopcoder.plan_fingerprint.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `fingerprint_id` | yes | `sha256:<hex>` digest. |
| `delivery_run_id` | yes | Owning DeliveryRun. |
| `project_id` | yes | Owning project. |
| `input_fingerprint` | yes | Input bundle digest. |
| `policy_fingerprint` | yes | Policy bundle digest. |
| `plan_fingerprint` | yes | Plan bundle digest. |
| `authorization_fingerprint` | yes | Combined digest used by approvals and overrides. |
| `canonicalization_version` | yes | `loopcoder.canonical_json.v1`. |
| `algorithm` | yes | `sha256`. |
| `created_at` | yes | Immutable timestamp. |
| `created_by` / `host` | yes | Provenance. |

Fingerprint rows are append-only. A changed plan creates a new fingerprint row;
it does not mutate or delete the old one.

## Lifecycle State Machines

Lifecycle transitions are validated inside the same write transaction as any
record mutation they authorize. `replay_same` is idempotent and returns the
already persisted state without changing timestamps. Any event not listed in a
table is invalid by definition; the tables below list every state and event.

### DeliveryRun States And Events

DeliveryRun states:

| State | Terminal | Meaning |
| --- | --- | --- |
| `draft` | no | Run shell exists; no planning has started. |
| `planning` | no | Side-effect-free planner is constructing a plan. |
| `awaiting-approval` | no | Immutable plan exists and requires approval, rejection, or override. |
| `approved` | no | Required approval or override is active for the current fingerprint. |
| `queued` | no | Scheduler may claim ready tasks. |
| `running` | no | At least one task is active or scheduler is supervising execution. |
| `paused` | no | No new task may start; active claim owners may complete or be cancelled by policy. |
| `cancelling` | no | Cancellation requested; active attempts are closing. |
| `succeeded` | yes | All required tasks completed successfully. |
| `failed` | yes | Deterministic failure with no safe automatic continuation. |
| `cancelled` | yes | Cancellation completed. |
| `needs-human` | yes | Human decision is required before any continuation. |
| `abandoned` | yes | User abandoned the run without claiming success or failure. |

DeliveryRun events:

| Event | Meaning |
| --- | --- |
| `start_planning` | Begin side-effect-free planning. |
| `plan_ready_requires_approval` | Persist canonical plan and fingerprints when approval or override is required. |
| `plan_ready_no_approval` | Persist canonical plan and fingerprints when deterministic policy requires no approval or override. |
| `approve` | Bind active approval or override to current authorization fingerprint. |
| `reject` | User rejects plan or required approval. |
| `queue` | Mark approved work schedulable. |
| `start_execution` | Scheduler starts execution after claims are available. |
| `pause` | Stop starting new work. |
| `resume` | Continue a paused run. |
| `cancel` | Request cancellation. |
| `finish_success` | Persist successful terminal outcome. |
| `finish_failure` | Persist failed terminal outcome. |
| `escalate` | Persist `needs-human`. |
| `abandon` | User abandons the run. |
| `replay_same` | Idempotent replay of the exact same canonical request. |
| `approve_stale` | Approval or override does not match current fingerprint. |

DeliveryRun transition table:

| State | start_planning | plan_ready_requires_approval | plan_ready_no_approval | approve | reject | queue | start_execution | pause | resume | cancel | finish_success | finish_failure | escalate | abandon | replay_same | approve_stale |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `draft` | `planning` | `ErrInvalidTransition` | `ErrInvalidTransition` | `ErrApprovalRequired` | `abandoned` | `ErrApprovalRequired` | `ErrApprovalRequired` | `ErrInvalidTransition` | `ErrInvalidTransition` | `cancelled` | `ErrInvalidTransition` | `failed` | `needs-human` | `abandoned` | `draft` | `ErrStaleApproval` |
| `planning` | `planning` | `awaiting-approval` | `approved` | `ErrApprovalRequired` | `abandoned` | `ErrApprovalRequired` | `ErrApprovalRequired` | `paused` | `ErrInvalidTransition` | `cancelling` | `ErrInvalidTransition` | `failed` | `needs-human` | `abandoned` | `planning` | `ErrStaleApproval` |
| `awaiting-approval` | `ErrInvalidTransition` | `ErrDuplicateRecord` | `ErrDuplicateRecord` | `approved` | `abandoned` | `ErrApprovalRequired` | `ErrApprovalRequired` | `paused` | `ErrInvalidTransition` | `cancelling` | `ErrInvalidTransition` | `failed` | `needs-human` | `abandoned` | `awaiting-approval` | `ErrStaleApproval` |
| `approved` | `ErrInvalidTransition` | `ErrDuplicateRecord` | `ErrDuplicateRecord` | `approved` | `abandoned` | `queued` | `running` | `paused` | `ErrInvalidTransition` | `cancelling` | `ErrInvalidTransition` | `failed` | `needs-human` | `abandoned` | `approved` | `ErrStaleApproval` |
| `queued` | `ErrInvalidTransition` | `ErrDuplicateRecord` | `ErrDuplicateRecord` | `queued` | `ErrInvalidTransition` | `queued` | `running` | `paused` | `ErrInvalidTransition` | `cancelling` | `succeeded` only when all tasks already terminal-success | `failed` | `needs-human` | `abandoned` | `queued` | `ErrStaleApproval` |
| `running` | `ErrInvalidTransition` | `ErrDuplicateRecord` | `ErrDuplicateRecord` | `running` | `ErrInvalidTransition` | `running` | `running` | `paused` | `ErrInvalidTransition` | `cancelling` | `succeeded` | `failed` | `needs-human` | `abandoned` | `running` | `ErrStaleApproval` |
| `paused` | `ErrInvalidTransition` | `ErrDuplicateRecord` | `ErrDuplicateRecord` | `paused` | `ErrInvalidTransition` | `ErrInvalidTransition` | `ErrInvalidTransition` | `paused` | `queued` | `cancelling` | `succeeded` only when all tasks already terminal-success | `failed` | `needs-human` | `abandoned` | `paused` | `ErrStaleApproval` |
| `cancelling` | `ErrInvalidTransition` | `ErrDuplicateRecord` | `ErrDuplicateRecord` | `ErrInvalidTransition` | `ErrInvalidTransition` | `ErrInvalidTransition` | `ErrInvalidTransition` | `ErrInvalidTransition` | `ErrInvalidTransition` | `cancelling` | `ErrInvalidTransition` | `cancelled` when cancellation cleanup fails closed | `needs-human` | `abandoned` | `cancelling` | `ErrStaleApproval` |
| `succeeded` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `succeeded` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `succeeded` | `ErrStaleApproval` |
| `failed` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `failed` | `ErrTerminalState` | `ErrTerminalState` | `failed` | `ErrStaleApproval` |
| `cancelled` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `cancelled` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `cancelled` | `ErrStaleApproval` |
| `needs-human` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `needs-human` | `ErrTerminalState` | `needs-human` | `ErrStaleApproval` |
| `abandoned` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `abandoned` | `abandoned` | `ErrStaleApproval` |

### Task States And Events

Task states:

| State | Terminal | Meaning |
| --- | --- | --- |
| `pending` | no | Planned but dependencies or policy have not made it runnable. |
| `blocked` | no | Waiting on dependency, policy, missing reference, or human decision. |
| `awaiting-approval` | no | Task-specific approval or override is required. |
| `ready` | no | Scheduler may claim the task. |
| `claimed` | no | Scheduler owns a durable execution claim but provider has not launched. |
| `running` | no | Attempt is launching or executing. |
| `paused` | no | Task is not allowed to start or continue new side effects. |
| `cancelling` | no | Cancellation requested for active attempt. |
| `succeeded` | yes | Task completed successfully. |
| `failed` | yes | Task failed deterministically. |
| `skipped` | yes | Task was intentionally not executed because policy or dependency outcome made it unnecessary. |
| `cancelled` | yes | Task cancellation completed. |
| `needs-human` | yes | Human decision is required. |

Task events:

| Event | Meaning |
| --- | --- |
| `dependencies_ready` | All predecessor tasks satisfy the edge requirement. |
| `dependencies_blocked` | At least one predecessor is not satisfied. |
| `require_approval` | Policy requires task-specific approval or override. |
| `approval_bound` | Active approval or override matches authorization fingerprint. |
| `claim` | Scheduler atomically claims this task/attempt. |
| `start` | Claim owner begins provider launch or local execution. |
| `pause` | Stop new side effects for this task. |
| `resume` | Resume from paused. |
| `cancel` | Request task cancellation. |
| `complete_success` | Persist successful completion. |
| `complete_failure` | Persist failed completion. |
| `skip` | Persist intentional skip. |
| `escalate` | Persist `needs-human`. |
| `replay_same` | Idempotent replay of exact canonical request. |
| `approval_stale` | Task approval or override does not match current fingerprint. |

Task transition table:

| State | dependencies_ready | dependencies_blocked | require_approval | approval_bound | claim | start | pause | resume | cancel | complete_success | complete_failure | skip | escalate | replay_same | approval_stale |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `pending` | `ready` or `awaiting-approval` if approval required | `blocked` | `awaiting-approval` | `ready` when dependencies satisfied | `ErrInvalidTransition` | `ErrClaimRequired` | `paused` | `ErrInvalidTransition` | `cancelled` | `ErrInvalidTransition` | `failed` | `skipped` | `needs-human` | `pending` | `ErrStaleApproval` |
| `blocked` | `ready` or `awaiting-approval` if approval required | `blocked` | `awaiting-approval` | `ready` when dependencies satisfied | `ErrInvalidTransition` | `ErrClaimRequired` | `paused` | `ErrInvalidTransition` | `cancelled` | `ErrInvalidTransition` | `failed` | `skipped` | `needs-human` | `blocked` | `ErrStaleApproval` |
| `awaiting-approval` | `awaiting-approval` | `blocked` | `awaiting-approval` | `ready` | `ErrApprovalRequired` | `ErrApprovalRequired` | `paused` | `ErrInvalidTransition` | `cancelled` | `ErrInvalidTransition` | `failed` | `skipped` | `needs-human` | `awaiting-approval` | `ErrStaleApproval` |
| `ready` | `ready` | `blocked` | `awaiting-approval` | `ready` | `claimed` | `ErrClaimRequired` | `paused` | `ErrInvalidTransition` | `cancelled` | `succeeded` only for zero-side-effect already-satisfied tasks | `failed` | `skipped` | `needs-human` | `ready` | `ErrStaleApproval` |
| `claimed` | `claimed` | `ErrInvalidTransition` | `ErrInvalidTransition` | `claimed` | `claimed` | `running` | `paused` | `ErrInvalidTransition` | `cancelling` | `succeeded` only before provider launch | `failed` | `ErrInvalidTransition` | `needs-human` | `claimed` | `ErrStaleApproval` |
| `running` | `running` | `ErrInvalidTransition` | `ErrInvalidTransition` | `running` | `ErrClaimConflict` | `running` | `paused` | `ErrInvalidTransition` | `cancelling` | `succeeded` | `failed` | `ErrInvalidTransition` | `needs-human` | `running` | `ErrStaleApproval` |
| `paused` | `paused` | `blocked` | `awaiting-approval` | `paused` | `ErrInvalidTransition` | `ErrInvalidTransition` | `paused` | `ready` | `cancelling` | `succeeded` only when completion was already durably proven | `failed` | `skipped` | `needs-human` | `paused` | `ErrStaleApproval` |
| `cancelling` | `cancelling` | `cancelling` | `ErrInvalidTransition` | `ErrInvalidTransition` | `ErrInvalidTransition` | `ErrInvalidTransition` | `ErrInvalidTransition` | `ErrInvalidTransition` | `cancelling` | `ErrInvalidTransition` | `cancelled` when cleanup fails closed | `ErrInvalidTransition` | `needs-human` | `cancelling` | `ErrStaleApproval` |
| `succeeded` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `succeeded` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `succeeded` | `ErrStaleApproval` |
| `failed` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `failed` | `ErrTerminalState` | `ErrTerminalState` | `failed` | `ErrStaleApproval` |
| `skipped` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `skipped` | `ErrTerminalState` | `skipped` | `ErrStaleApproval` |
| `cancelled` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `cancelled` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `cancelled` | `ErrStaleApproval` |
| `needs-human` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `needs-human` | `needs-human` | `ErrStaleApproval` |

Any task `claim` must validate that the parent DeliveryRun is `queued` or
`running`, the task belongs to the same project and DeliveryRun, dependencies
are satisfied, approval is active for the current fingerprint, side-effect
class is authorized, and no active owner holds the claim. Failure writes no
attempt, claim, or side effect.

## Fingerprint Binding

Approvals and overrides bind to the exact `authorization_fingerprint`. The
authorization fingerprint is a digest over the input, policy, and plan
fingerprints, not over mutable approval rows.

### Fingerprint Inputs

`input_fingerprint` covers:

- `project_id`, project identity source, normalized remote identity when
  available, and the selected checkout identity from `0639`;
- user intent text or host request JSON;
- explicit issue, PR, branch, base ref, and base commit SHA inputs;
- relevant `.delivery.yml` configuration values after compatibility migration
  and default expansion;
- host capability request metadata supplied by Paseo, Codex, Claude Code, or
  another host;
- any imported legacy state that affects the plan, represented by imported
  record IDs and source hashes, not raw log text.

`policy_fingerprint` covers:

- `policy_version`;
- deterministic permission, scope, side-effect, concurrency, budget ceiling,
  approval, override, release-gate, and migration-safety rules;
- effective policy inputs from config and command flags;
- policy decision schema versions.

`plan_fingerprint` covers:

- planner schema version;
- ordered task list with `task_key`, title, requirements, scope, permission,
  side-effect class, approval requirement, and task-local policy decisions;
- ordered dependency edge list;
- expected terminal conditions;
- required reporter roles and verification gates by reference, not reporter
  block content;
- every consequential planner decision, including whether it is deterministic
  or heuristic.

`authorization_fingerprint` is:

```text
sha256("loopcoder.authorization.v1\n" +
       input_fingerprint + "\n" +
       policy_fingerprint + "\n" +
       plan_fingerprint + "\n" +
       max_side_effect_class + "\n" +
       approved_scope_canonical_json)
```

### Canonicalization

Canonical JSON version: `loopcoder.canonical_json.v1`.

Canonicalization rules are testable and mandatory:

- Encode as UTF-8 with LF line endings. The bytes hashed are the canonical JSON
  bytes with no trailing newline unless the algorithm prefix explicitly adds
  one.
- Use JSON objects, arrays, strings, integers, booleans, and null. Floats are
  not allowed in fingerprint inputs; encode rational or decimal values as
  strings with a named unit.
- Object keys sort lexicographically by Unicode code point after JSON string
  unescaping.
- Arrays preserve order when order is semantic, such as tasks, edges, and
  ordered fallback lists. Arrays that represent sets must be sorted by each
  element's canonical JSON byte string before hashing.
- Optional absent fields are absent. Required empty fields are encoded as
  `null`, `""`, `[]`, or `{}` according to the schema. Implementations must not
  treat absent and null as equivalent unless the schema says so.
- Strings are normalized to Unicode NFC before JSON escaping. Repository paths
  in scopes use `/` separators, remove `.` segments, reject `..` escapes, and
  are relative to the project root unless a schema field explicitly allows an
  absolute machine-local path.
- Git remotes use the normalized remote identity rules from
  [`0639-global-data-layout-project-identity.md`](0639-global-data-layout-project-identity.md).
- Timestamps are UTC RFC3339 with `Z`. Fractional seconds are retained only to
  the precision originally persisted and trailing fractional zeros are removed.
- Integers are base-10 with no leading zeros except `0`.
- Hashes render as lowercase hex with the algorithm prefix, for example
  `sha256:0123abcd...`.
- Generated record IDs, `created_at`, `updated_at`, host process IDs, and
  reporter record IDs are excluded from plan fingerprints unless explicitly
  listed as semantic inputs above.

If a binary cannot reproduce the stored fingerprint from persisted canonical
inputs, it must return `ErrInvalidRecord` or `ErrUnknownRecordVersion` and fail
closed to `needs-human` for any authority decision.

## Idempotency And Crash Recovery

Every mutating API or command path that creates or advances these records must
accept or derive an idempotency key. The key is stored with canonical request
bytes and the resulting record IDs.

Replay rules:

- Same idempotency key and same canonical request bytes returns the existing
  result without changing lifecycle timestamps. This replay check is evaluated
  before current-fingerprint freshness for approval and override records, and
  returns only the historical result.
- Same idempotency key and different canonical request bytes fails with
  `ErrDuplicateReplay`.
- A replay that would recreate the same task, decision, approval, override, or
  edge by stable identity returns the existing record only when the canonical
  payload is identical.
- A fresh approval or override request whose fingerprint no longer matches the
  current run fails with `ErrStaleApproval`; changed payloads cannot regain
  authority by reusing an old idempotency key.
- A replay that would create a duplicate side effect must stop before the side
  effect and return the existing durable result or `ErrDuplicateReplay`.
- Side effects may start only after durable intent, policy checks,
  authorization fingerprint, and claim ownership are persisted.
- Provider launches follow the accepted `0646` contract: durable claim
  acquisition, claim phase, lease renewal, provider idempotency key, and real
  provider receipt are the recovery boundary. A provider idempotency key without
  a receipt is not proof of completion.

Crash recovery rules:

- Crash before task or attempt persistence: replay may create the record.
- Crash after task persistence but before side effects: replay returns or
  advances the persisted task if approval and policy still match.
- Crash after claim but before provider launch: recovery may take over only if
  the durable claim phase proves the provider did not launch, as in `0646`.
- Crash during provider launch or execution: an active lease blocks duplicate
  launch. An expired `launching` or `executing` claim fails closed to
  `needs-human`.
- Crash after external side effects but before terminal persistence: recovery
  uses durable ownership, provider receipts, local execution records, and human
  review. It must not claim exactly-once side effects without evidence.
- Stale completion from an old claim generation is rejected with
  `ErrStaleClaim` and must not publish task, edge, DeliveryRun, or reporter
  terminal events.
- Invalid transitions, cycles, stale approvals, duplicate replay, and
  cross-project references fail atomically. No partial edge, decision,
  approval, override, attempt, claim, or side-effect-intent row may remain.

## v0.7 To v0.8 Migration Strategy

The starting point is the v0.7 machine-global database from
[`0639-global-data-layout-project-identity.md`](0639-global-data-layout-project-identity.md)
at schema version 9: `projects`, `runs`, `run_events`, `run_edges`, `reports`,
`child_plans`, `run_claims`, `legacy_import_records`, and
`legacy_import_status`.

### What Migrates

Forward migration copies or enriches only facts that v0.7 already proves:

- `projects.id` becomes `project_id` and remains stable.
- `runs.id` becomes the DeliveryRun-compatible `delivery_run_id` / `run_id`
  for imported run records.
- Existing `runs.project_id`, `parent_run_id`, `root_run_id`, `depth`,
  `origin`, `status`, `started_at`, `ended_at`, `created_at`, and `updated_at`
  are preserved.
- Existing nested `run_edges`, `child_plans`, and `run_claims` are preserved as
  nested-run records and may be referenced by DeliveryRun attempts.
- Existing `reports` remain local-only reporter records and may be linked by ID
  or source metadata.
- Existing `legacy_import_records` and `legacy_import_status` remain migration
  provenance and backup evidence.
- Existing schema-v9 non-terminal claim rows that were migrated to
  `phase = executing` remain ambiguous execution claims. After expiry, they
  fail closed to `needs-human`; they are never auto-taken-over as pre-launch
  work.

### What Does Not Migrate

Migration must not invent v0.8 intent:

- It does not synthesize approvals, overrides, or user intent from logs,
  reporter text, issue titles, or branch names.
- It does not infer a DeliveryRun plan fingerprint for legacy runs unless the
  exact canonical v0.8 input, policy, and plan bundles are available.
- It does not silently convert an old run graph into an approved v0.8
  DeliveryRun.
- It does not rewrite repo-local `.loopcoder/`, delete legacy payloads, mutate
  GitHub, edit `.delivery.yml`, launch providers, or push state.
- It does not merge projects with ambiguous identity. The collision rules from
  `0639` still fail closed.

Ambiguous legacy records become inspectable imported records with
`migration_status: needs-human` and `ErrAmbiguousLegacyState` diagnostics. This
matches the v8-to-v9 lesson in `0646`: non-terminal legacy claims migrate
conservatively because they do not prove whether a provider launched.

### Backup And Rollback

Before applying v0.8 migration to a schema-v9 database, implementation must
record backup metadata:

| Field | Meaning |
| --- | --- |
| `backup_id` | Opaque backup record ID. |
| `source_db_path` | Machine-local database path. |
| `source_schema_version` | Must be `9` for this migration. |
| `source_db_hash` | Hash of the database file or a transactionally consistent backup image. |
| `backup_path` | Machine-local backup image path under loopcoder home, not the project repo. |
| `created_at` | Backup timestamp. |
| `loopcoder_version` | Binary that created the backup. |
| `migration_plan_fingerprint` | Fingerprint of the migration plan, not user delivery work. |

Rollback story:

- If migration fails before commit, the SQLite transaction rolls back and the
  original database remains at schema 9.
- If migration commits but a later v0.8 command fails, rollback is selecting a
  v0.7 binary plus restoring the recorded schema-v9 backup image. v0.7 binaries
  are not required to read v0.8 schema tables.
- Migration must be idempotent. Re-running after a partial failed attempt uses
  migration metadata to continue or report the exact recovery action.
- Backup images are machine-local runtime state. They must not be copied into
  project repositories, PR bodies, issue comments, commits, fixtures, or tracked
  docs.

### Multi-Project Isolation

Every migrated row must retain `project_id`. Any v0.8 row derived from a v0.7
row must reference only records with the same `project_id`. Cross-project
references fail with `ErrCrossProjectReference` in the migration transaction and
leave the database unchanged.

For multiple checkouts of the same repository, project identity follows `0639`:
same normalized GitHub owner/name or same normalized remote resolves to one
logical project; path-only projects do not merge unless a later explicit attach
command proves identity. Migration must not use `display_name` as an identity
key.

## Decision Ownership Boundaries

These boundaries are normative restatements of #714.

| Owner | May decide | Must not decide |
| --- | --- | --- |
| User | Intent, risk tolerance, budgets, approvals, provider/model pins, and final exceptions. | Hidden policy weakening, unrecorded approvals, or provider launch mechanics. |
| Deterministic policy engine | Non-negotiable permissions, side-effect classes, scope, concurrency, budget ceilings, release gates, stale approval rejection, and override requirements. | User intent, heuristic ranking, or accepting changed work without fresh authority. |
| Planner | Bounded task graph, task requirements, dependency edges, and side-effect-free estimates. | Provider launch, budget override, final approval, or silently expanding scope after approval. |
| Router | Eligible provider/model candidate scoring, rejected reasons, fallbacks, and heuristic ranking among policy-eligible choices. | Making an ineligible candidate eligible, bypassing quota confidence, or deciding final user exceptions. |
| Scheduler | Capacity reservation, claim acquisition, start order, retries, rebalancing, and pausing without weakening policy. | Launching without claim ownership, duplicating side effects, overriding policy, or changing approved plan semantics. |
| Provider-native sub-agents | Scoped execution delegated by LoopCoder within declared task scope and permission. | Global budgets, permissions, routing policy, approval decisions, cross-task ownership, or final acceptance. |

Every consequential decision must be persisted as a Decision record. If it is
not reproducible from persisted deterministic inputs, it must be marked
`heuristic: true` with a reason and remain subordinate to deterministic policy,
approval, and override records.

## Later Durable Concepts

The following concepts are required by #714 but are intentionally outline-only
in this phase. They must be specified by later phase specs 0802-0805 before code
implements their deep schemas:

- provider installations and probe results;
- accounts/profiles and authentication readiness;
- model-catalog snapshots and provenance;
- quota snapshots and freshness confidence states;
- usage reservations and reconciliation;
- routing candidates with rejected reasons and fallbacks;
- agent trees, scopes, ownership, events, results, and cancellation.

Shared conventions those later specs must follow:

- IDs use opaque stable prefixes and never encode provider credentials or raw
  account identifiers.
- Every record has `schema_version`, `record_version`, `project_id` where
  project-scoped, provenance, timestamps, policy version when policy-relevant,
  and typed errors.
- Durable records that affect DeliveryRun authority must contribute to
  `input_fingerprint`, `policy_fingerprint`, or a later explicitly named
  fingerprint input.
- Provider, account, model, quota, and routing records must never store secrets
  or raw credential material.
- Confidence enum values are exactly `exact`, `estimated`, `unknown`,
  `unavailable`, and `stale`. Later specs may add fields that explain the
  evidence for each confidence state, but they must not rename these values.
- Rejected routing candidates must record a typed rejected reason and the
  deterministic or heuristic inputs that produced it.
- Agent-tree records must keep LoopCoder ownership, provider-native ownership,
  and scope boundaries explicit. Provider-native sub-agents never inherit
  global authority by being children.

## Implementation Acceptance Mapping For #721

Issue #721 is complete only when its code and tests implement this accepted
spec:

- Versioned Go, SQLite, and JSON representations exist for every record in
  this document.
- Invalid transitions, cycles, stale approvals, duplicate replay, and
  cross-project references fail atomically with the typed errors named here.
- Approval and override records bind to the exact authorization fingerprint and
  cannot authorize changed work.
- Crash/restart and idempotent replay tests prove no duplicate tasks,
  decisions, or side effects.
- Migration tests cover schema-v9 forward migration, backup metadata, rollback
  story, multi-project isolation, and ambiguous legacy records becoming
  `needs-human`.
- JSON fixtures cover Linux, macOS, and Windows path canonicalization where
  path inputs affect fingerprints.
- Reporter references remain local-only, and nested run claim/lease semantics
  match `0646` rather than a new duplicate mechanism.

## Relationship To Existing Specs

- [`0639-global-data-layout-project-identity.md`](0639-global-data-layout-project-identity.md)
  defines project identity, global storage, local-only runtime state, and
  migration safety. This spec uses `project_id` as the isolation key.
- [`0646-nested-sub-agent-plan.md`](0646-nested-sub-agent-plan.md) defines
  nested runs, run edges, execution claims, claim phases, leases, fencing,
  provider idempotency keys, and provider receipts. This spec aligns
  DeliveryRun attempts with that contract.
- [`0567-reporter.md`](0567-reporter.md) defines the reporter terminology and
  local-only report invariant. DeliveryRun records may link reports but do not
  make reporter output repository-visible.
- [`0041-resilience.md`](0041-resilience.md) defines crash recovery,
  idempotent adoption before retry, stale/hung/idle classification, and
  conservative `needs-human` handling. This spec applies the same stance to
  DeliveryRun records.
- [`0028-scheduling.md`](0028-scheduling.md) defines dependency DAG scheduling
  and separates real dependency order from file-overlap merge order. Task
  dependency edges preserve that separation.
- [`0583-upgrade-migration-doctor.md`](0583-upgrade-migration-doctor.md)
  defines safe upgrade and doctor migration behavior. This spec keeps v0.8
  migration explicit, backed up, idempotent, and conservative.
