# Task Requirement Classification

LoopCoder v0.8 records planner task requirements as immutable
`loopcoder.task_requirement.v1` JSON and SQLite rows before routing can inspect
provider candidates. The classifier is deterministic by default: matched rule
IDs are stored in `classification_rules`, any heuristic can only raise risk or
quality floors, and unknown, stale, or unavailable facts do not satisfy hard
routing constraints.

## Operator Notes

TaskRequirement rows are scoped by `project_id`, `delivery_run_id`, `task_id`,
and `plan_fingerprint`. A changed classification creates a changed plan
fingerprint and a new requirement ID. Operators can persist scoped corrections
as `loopcoder.task_requirement_override.v1` rows; later classification includes
matching active override IDs in `source.record_ids` and adds the
`policy.user-correction` rule.

Corrections are revalidated every time they apply. A correction may raise
`risk_tier`, `permission_required`, or `side_effect_class`, but it cannot lower
the deterministic floor produced by the task scope. Invalid corrections fail
closed with a typed error and add `invalid-user-correction` to `gap_reasons`.

## Policy Contract

The built-in classifier policy is `task-requirement-classifier-v1`. Stable rule
IDs come from spec 0804, including `scope.docs-only`, `scope.repo-write`,
`scope.github-write`, `scope.git-remote-write`, `scope.external-write`,
`scope.local-runtime-write`, `cap.output-json`, `cap.verifier-readonly`,
`cap.nested`, `cap.cancellation`, `cap.token-usage-reporting`,
`data.secret-reference`, `data.secret-material`, `policy.user-pin`,
`policy.override-requested`, `quality.security-or-core`,
`quality.large-change`, and `quality.ambiguous-intent`.

Ambiguous high-risk classifications stop with `ErrRequirementUnknown` and add a
human approval verification requirement instead of defaulting to lower risk.
Secret material is classified as `critical` and terminally refused with
`ErrPolicyDenied`.

## Schema Contract

The JSON schema lives at
[`docs/schemas/task_requirement.v1.schema.json`](../schemas/task_requirement.v1.schema.json).
Storage schema version 14 adds:

- `task_requirements`: queryable requirement identity, risk, permission,
  side-effect, policy, scope, provenance, typed error, and canonical payload
  columns.
- `task_requirement_overrides`: scoped user correction records with validation
  status, value JSON, provenance, and canonical payload columns.

Requirement IDs use `treq_<base32-sha256(project_id, delivery_run_id, task_id,
plan_fingerprint)>`. Requirement payloads also carry
`task_requirement_fingerprint` as a canonical SHA-256 digest.

## Adapter Boundary

Adapters do not classify tasks and must not infer missing hard requirements.
Routing and adapter selection consume `required_capabilities`,
`permission_required`, `side_effect_class`, `network_required`,
`quality_floor`, and `verification_requirements` from TaskRequirement records.
Candidate evidence with `unknown`, `unavailable`, `stale`, or expired freshness
fails hard capability checks unless a later routing policy explicitly persists a
conservative estimated path for that exact requirement.
