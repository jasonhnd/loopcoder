---
id: 804
title: v0.8.0 Phase 4 Decomposition And Routing
status: draft
date: 2026-07-12
issue: 805
pr: null
supersedes: []
superseded_by: []
---

# v0.8.0 Phase 4 Decomposition And Routing

This documentation-only spec freezes the phase-4 task decomposition and routing
contract that implementation issues
[#733](https://github.com/jasonhnd/loopcoder/issues/733),
[#734](https://github.com/jasonhnd/loopcoder/issues/734),
[#735](https://github.com/jasonhnd/loopcoder/issues/735),
[#736](https://github.com/jasonhnd/loopcoder/issues/736), and
[#737](https://github.com/jasonhnd/loopcoder/issues/737) must implement for the
v0.8.0 decomposition and routing epic
[#718](https://github.com/jasonhnd/loopcoder/issues/718).
It is delivery order step 4 from roadmap
[#714](https://github.com/jasonhnd/loopcoder/issues/714): implement task
requirements, bounded decomposition, and explainable routing only after
contracts, inventory, and quota/availability designs are frozen.

This spec follows the shared durable-record conventions from
[`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md): opaque
stable IDs, explicit `schema_version` and `record_version`, actor and host
provenance, UTC timestamps, side-effect classes, typed errors, canonical
fingerprints, idempotency, one-transaction mutation rules, and decision
ownership boundaries. It reuses provider inventory, auth readiness, model
capability, and invocation-profile facts from
[`0802-provider-inventory.md`](0802-provider-inventory.md), and quota, usage,
budget, availability, and circuit-breaker facts from
[`0803-quota-usage-budget.md`](0803-quota-usage-budget.md). It applies the
routing controls from
[`0742-v080-security-threat-model.md`](0742-v080-security-threat-model.md):
policy eligibility gates routing before heuristic ranking, rejected candidates
are explicit, fallback does not weaken red-line policy, and user pins are
authority inputs rather than hidden model assumptions.

This document adds no Go code, SQLite migration, CLI behavior, workflow change,
provider integration, scheduler rewiring, guided UX, or federation behavior.
Per [`../PROCESS.md`](../PROCESS.md), this spec must merge before the code
issues above implement it.

## Goals

- Define TaskRequirement records for capability, permission, side-effect, risk,
  verification, and classification provenance.
- Define bounded dependency-aware task graphs that reuse the 0801 Task and
  Dependency Edge contracts and make the graph part of the fingerprinted plan.
- Define capability-first explainable routing: eligibility first, optimization
  second, with persisted selected, rejected, scored, and fallback candidates.
- Define role definitions and routing policy profiles without binding
  Soul/Tera/Luna or any other role label to provider model names.
- Define user pins as explicit authority inputs that override heuristic
  ranking but never override deterministic policy rejection.
- Define policy-preserving fallback, re-planning triggers, verifier
  independence, high-risk verification requirements, and circuit-breaker
  interaction.
- Define storage, doctor, and status JSON exposure for task requirements,
  graphs, routing profiles, routing decisions, and fallback/replan records.
- Provide implementation acceptance mapping for #733 through #737.

## Non-Goals

- No provider-native federation, agent-tree event schema, or child-agent result
  contract. Those belong to phase 0805.
- No guided UX flow, prompt copy, approval wizard, or interactive planner UI.
- No runtime scheduler rewrite beyond the routing and re-planning contract that
  later code issues need.
- No provider credential access, private UI scraping, or quota-source expansion
  beyond 0802 and 0803.
- No hard-coded mapping from Soul, Tera, Luna, or any other role name to model
  IDs, provider names, account profiles, or installation paths.
- No rule that selects the largest quota, lowest cost, fastest latency, or most
  familiar provider when a candidate is otherwise unsuitable or unsafe.

## Terms

**TaskRequirement** is the durable, planner-authored classification record for
one 0801 Task. It states required role, permission, capability, side-effect,
risk, expected output, verification, provenance, and whether any part is a
declared heuristic.

**Task graph** is the bounded DAG formed by 0801 Task records and 0801
Dependency Edge records for one DeliveryRun plan.

**RoleDefinition** is a provider-neutral role envelope such as `worker`,
`verifier`, or `nested-subagent`. It describes capabilities and policy floors.
It is not a provider or model alias.

**RoutingPolicyProfile** is a versioned policy bundle that defines graph
bounds, risk floors, eligibility rules, optimization weights, fallback
legality, and default role requirements.

**Routing candidate** is a provider/account/model/invocation-profile tuple with
references to the inventory, auth, quota, budget, availability, and role
records used for evaluation.

**Eligibility** is the deterministic yes/no gate that decides whether a
candidate may be considered for a task. Eligibility is evaluated before
optimization and cannot be changed by heuristic scores.

**Optimization** ranks only eligible candidates using persisted policy weights
and source-backed or explicitly heuristic inputs.

**RoutingDecision** is the durable consequential decision that records eligible
candidates, rejected candidates with reasons, scored candidates, selected
candidate, fallback chain, profile version, input record references, and
provenance.

**Fallback** is selection of another eligible route for the same approved task
requirement and graph. It is policy-preserving only when it does not weaken
permission, verifier independence, minimum capability, approval freshness,
side-effect class, scope, budget authority, or required confidence.

**Re-planning** changes the task graph, task requirement, route requirement,
scope, side-effect class, budget envelope, or verification plan. Re-planning is
not fallback; it changes the plan fingerprint and may require fresh approval.

## Shared Representation

The implementation must expose one logical contract across Go, SQLite, and
JSON:

- JSON field names use snake_case, explicit `schema_version` strings, and
  stable enum strings.
- SQLite stores queryable identity, lifecycle, policy, provenance, risk,
  capability, route, and relationship fields in normal columns. Bounded
  structured payloads may live in `*_json` columns only when they are not needed
  for joins, uniqueness, lifecycle transitions, eligibility, budget checks, or
  atomic policy decisions.
- SQLite `TEXT` IDs are opaque. Callers must not parse provider names, model
  names, issue numbers, timestamps, role names, or risk tiers from IDs.
- Every record has `schema_version`, `record_version`, timestamps, provenance,
  classification, policy version when policy-relevant, and typed error fields
  where terminal refusal can be represented.
- Project-scoped records carry `project_id`; DeliveryRun-scoped records also
  carry `delivery_run_id`; task-scoped records carry `task_id`. Cross-project
  references fail with `ErrCrossProjectReference`.
- All timestamps are UTC RFC3339 strings rendered with `Z`.
- Unknown enum values in persisted records fail closed with
  `ErrUnknownRecordVersion` or `ErrInvalidRecord`, matching 0801.
- Records that affect DeliveryRun authority, task start eligibility, provider
  launch, routing, fallback, re-planning, verification, or approval must
  contribute their exact record IDs and canonical payload hashes to the
  applicable `input_fingerprint`, `policy_fingerprint`, `plan_fingerprint`, or
  `routing_fingerprint`.
- Every consequential number must be traceable to a persisted record from 0802
  or 0803, a RoutingPolicyProfile field, or an explicitly marked heuristic with
  estimator metadata, input references, timestamp, and confidence.

### ID Scheme

ID prefixes are stable; the bytes after the prefix are opaque:

| Record | ID field | Required form |
| --- | --- | --- |
| TaskRequirement | `task_requirement_id` | `treq_<base32-sha256(project_id, delivery_run_id, task_id, plan_fingerprint)>`. |
| TaskGraphValidation | `task_graph_validation_id` | `tgraphval_<uuidv7-or-random-128-bit-base32>`; immutable per validation attempt. |
| RoleDefinition | `role_definition_id` | `roledef_<base32-sha256(role_key, role_version, policy_version)>`. |
| RoutingPolicyProfile | `routing_policy_profile_id` | `rprof_<base32-sha256(profile_key, profile_version, policy_version)>`. |
| RoutingCandidate | `routing_candidate_id` | `rcand_<base32-sha256(task_id, adapter_id, account_profile_id, model_capability_id, invocation_profile_key, routing_policy_profile_id)>`. |
| RoutingDecision | `routing_decision_id` | `rdec_<base32-sha256(project_id, delivery_run_id, decision_key, task_id, routing_fingerprint)>` plus a collision suffix only for different canonical replay bytes. |
| FallbackDecision | `fallback_decision_id` | `fdec_<base32-sha256(routing_decision_id, fallback_ordinal, routing_fingerprint)>`. |
| ReplanDecision | `replan_decision_id` | `replan_<base32-sha256(project_id, delivery_run_id, prior_plan_fingerprint, replan_ordinal)>`. |
| Routing fingerprint | `routing_fingerprint` | The digest string itself: `sha256:<64-lower-hex>`. |

`decision_key` is caller-stable for idempotent replay. A changed task
requirement, graph, policy profile, inventory record, quota/budget record, or
availability record requires a changed `routing_fingerprint`.

### Common Routing Fields

All durable records in this spec carry the following fields unless a record
table explicitly marks one as not applicable:

| Field | Required | Meaning |
| --- | --- | --- |
| `schema_version` | yes | Stable JSON/storage shape string. |
| `record_version` | yes | Optimistic update version for mutable records; immutable decision and validation rows keep `1`. |
| `project_id` | conditional | Required for project and narrower scopes. |
| `delivery_run_id` | conditional | Required for DeliveryRun-scoped records. |
| `task_id` | conditional | Required for task-scoped records. |
| `created_at` / `updated_at` | yes | UTC persistence timestamps. |
| `created_by` / `updated_by` | yes | 0801 actor provenance object. |
| `host` | yes | 0801 host provenance object. |
| `policy_version` | yes | Deterministic policy version used for this record. |
| `routing_policy_profile_id` | conditional | Profile used when the record depends on routing policy. |
| `plan_fingerprint` | conditional | Plan fingerprint when the record belongs to a plan. |
| `routing_fingerprint` | conditional | Routing input fingerprint when the record affects a route. |
| `classification` | yes | 0742 data classification for the most sensitive field. |
| `side_effect_class` | yes | Maximum 0801 side-effect class involved. |
| `confidence` | conditional | 0801 confidence enum when evidence quality matters. |
| `source` | yes | Structured source descriptor or source record references. |
| `heuristic` | yes | `true` only when the value is not fully reproducible from deterministic persisted inputs. |
| `heuristic_reason` | conditional | Required when `heuristic` is true. |
| `terminal_error_code` | no | Typed error when the record represents a terminal refusal. |
| `gap_reasons` | yes | Ordered typed reasons for unknown, unavailable, stale, partial, heuristic, or conflicting facts. Empty when no gap exists. |

### Typed Errors

Implementations must reuse 0801 and 0803 typed errors where they apply and add
the following stable codes for this phase:

| Error | Required trigger |
| --- | --- |
| `ErrRequirementUnknown` | A hard task requirement cannot be classified from deterministic rules and no policy-approved heuristic path exists. |
| `ErrRequirementConfidenceInsufficient` | A requirement depends on inventory, quota, or availability evidence with insufficient confidence or freshness. |
| `ErrGraphBoundExceeded` | Planned graph exceeds `max_tasks`, `max_depth`, `max_fan_out`, `max_dependencies_per_task`, or `max_parallel_ready`. |
| `ErrGraphCycleDetected` | Graph validation finds a cycle; it is the graph-level equivalent of 0801 `ErrCycleDetected`. |
| `ErrGraphDisconnected` | A non-root task cannot be reached from the DeliveryRun root intent or no terminal task can satisfy the intent. |
| `ErrNoEligibleCandidate` | All routing candidates were rejected by deterministic eligibility. |
| `ErrPinnedCandidateIneligible` | A user-pinned provider, account, model, or profile violates deterministic policy or lacks required evidence. |
| `ErrRoleUnsupported` | Candidate cannot satisfy the required RoleDefinition. |
| `ErrCapabilityUnsupported` | Candidate lacks a required capability or the capability is unknown, unavailable, or stale. |
| `ErrVerifierIndependenceRequired` | Verifier route is not independent enough from the worker route for the task risk tier. |
| `ErrFallbackWouldWeakenPolicy` | Fallback would weaken permission, verifier independence, minimum capability, side-effect class, scope, budget, or required confidence. |
| `ErrReplanRequired` | The run cannot continue through legal fallback and requires a new plan fingerprint. |
| `ErrReplanBoundExceeded` | Re-planning exceeds the profile's bounded retry count or graph expansion limit. |
| `ErrRoutingFingerprintMismatch` | Routing decision inputs do not reproduce the stored routing fingerprint. |

These errors are refusal semantics. They must not become silent fallback,
unchecked provider launch, hidden profile selection, or hidden route recovery.

## Task Requirement Classification

Implementation issue #733 owns the first code implementation of this section.

### TaskRequirement

Schema version: `loopcoder.task_requirement.v1`.

TaskRequirement is immutable for a given `task_id` and `plan_fingerprint`.
Changing the requirement creates a new plan fingerprint and a new requirement
record.

| Field | Required | Meaning |
| --- | --- | --- |
| `task_requirement_id` | yes | Stable requirement identity. |
| `task_id` | yes | 0801 Task being classified. |
| `task_key` | yes | Planner-stable key from the fingerprinted plan. |
| `role_key` | yes | Required role envelope: `worker`, `verifier`, `nested-subagent`, or a policy-defined role key. |
| `risk_tier` | yes | `low`, `medium`, `high`, or `critical`. |
| `permission_required` | yes | Reporter permission enum: `read-only`, `write`, or `orchestrate`. |
| `side_effect_class` | yes | Maximum 0801 side-effect class required by the task. |
| `required_capabilities` | yes | CapabilityRequirement array using 0802 capability dimensions. |
| `required_output` | yes | Expected output contract: `freeform`, `markdown`, `json`, `json-schema`, `patch`, `branch`, `pr`, `report`, or `verification-verdict`. |
| `verification_requirements` | yes | VerificationRequirement array from this spec. |
| `scope_json` | yes | Canonical repo, path, issue, PR, command, data, provider, and network boundary. |
| `data_classification` | yes | Highest 0742 data classification the task may process. |
| `network_required` | yes | `not-needed`, `allowed-if-profile-grants`, or `required`. |
| `nested_allowed` | yes | Whether this task may create nested sub-agent work. |
| `cancellation_required` | yes | Whether policy requires local cancellation support before launch. |
| `quality_floor` | yes | Minimum quality class: `standard`, `strong`, or `adversarial`. |
| `classification_rules` | yes | Ordered deterministic rule IDs that produced the record. |
| `heuristics` | yes | Ordered heuristic classifier outputs, empty when none. |
| `provenance_summary` | yes | Human-readable bounded explanation for doctor/status. |
| `policy_version` | yes | Classifier policy version. |
| `plan_fingerprint` | yes | Plan fingerprint this requirement belongs to. |

`required_capabilities` uses this shape:

```json
{
  "dimension": "json_output",
  "required_value": true,
  "minimum_confidence": "exact",
  "freshness_required": "fresh",
  "source": "deterministic-rule:output-json-schema"
}
```

Allowed `dimension` values are the 0802 capability dimensions:
`roles_supported`, `read_only`, `json_output`, `nested_subagents`,
`mcp_config`, `cancellation`, `token_usage_reporting`,
`context_window_tokens`, `tool_support`, `image_input`, and `image_output`.
Unknown or stale capability fields cannot satisfy hard requirements.

### Risk Tiers

Risk tier classification is deterministic unless a row explicitly says a
heuristic may raise the tier. Heuristics may raise risk; they must not lower
the deterministic floor.

| Risk tier | Deterministic triggers | Minimum verification | Routing implications |
| --- | --- | --- | --- |
| `low` | Read-only inspection, docs-only planning, deterministic validation, no provider launch beyond read-only route, side-effect class `none` or `local-read`. | Self-check or configured docs tests when applicable. | Any eligible worker role may run; verifier optional unless policy requires it. |
| `medium` | Repo writes inside declared scope, local runtime writes, ordinary test or build commands, provider launch for bounded worker task, side-effect class up to `provider-launch`. | Configured local or hosted checks plus route explanation. | Worker route must satisfy role and capability floor; verifier required when output affects merge eligibility. |
| `high` | Git remote writes, GitHub writes, release gates, security-sensitive files, workflow changes, budget override request, stale or estimated quota exception, or orchestrating nested work. | Independent verifier plus deterministic checks; user approval when policy requires. | Verifier must be independent at the `provider-or-account` level unless profile requires stronger independence. |
| `critical` | External writes beyond GitHub/provider launch, credential-adjacent paths, cross-project recovery ambiguity, policy override of a red-line refusal, or inability to prove side-effect state after crash. | Independent verifier, human approval, and explicit override for the exact authorization fingerprint. | Automatic routing may prepare candidates but must stop before launch until approval/override exists. |

### VerificationRequirement

VerificationRequirement records are embedded in TaskRequirement and may also be
rendered in RoutingDecision input references.

| Field | Required | Meaning |
| --- | --- | --- |
| `verification_kind` | yes | `none`, `self-check`, `local-command`, `hosted-check`, `loopreview`, `security-review`, `human-approval`, or `override`. |
| `required_for_risk_tiers` | yes | Risk tiers where this requirement applies. |
| `independence_level` | yes | `none`, `different-model`, `different-account`, `different-provider`, or `human`. |
| `permission_required` | yes | Usually `read-only` for verifiers; stronger permission requires explicit policy reason. |
| `output_contract` | yes | `verification-verdict`, `json-schema`, `markdown`, or another accepted output contract. |
| `source` | yes | Deterministic rule ID, policy profile ID, or user approval reference. |

### Deterministic Classifier Rules

Rules are evaluated in order. The classifier persists every matching rule ID,
not just the winning result.

| Rule ID | Input condition | Required classification effect | Heuristic allowed? |
| --- | --- | --- | --- |
| `scope.docs-only` | Task scope contains only `docs/` or markdown/reference files and no runtime command side effects. | `permission_required: read-only` or `write` by planned edit; `risk_tier` at least `low`; side-effect class at least planned write class. | No. |
| `scope.repo-write` | Task may mutate repository files or branches. | `permission_required: write`; side-effect class at least `repo-write`; `risk_tier` at least `medium`. | No. |
| `scope.github-write` | Task may create/edit GitHub issues, PRs, labels, comments, checks, releases, or workflow metadata. | side-effect class at least `github-write`; `risk_tier` at least `high`. | No. |
| `scope.git-remote-write` | Task may push refs, tags, or branches. | side-effect class at least `git-remote-write`; `risk_tier` at least `high`. | No. |
| `scope.external-write` | Task may write to non-GitHub external services. | side-effect class `external-write`; `risk_tier` at least `critical`; human approval required. | No. |
| `scope.local-runtime-write` | Task may write LoopCoder machine-local runtime state only. | side-effect class at least `local-write`; `risk_tier` at least `medium`. | No. |
| `cap.output-json` | Downstream consumer requires schema-enforced JSON. | Add `json_output: true`; required output `json` or `json-schema`. | No. |
| `cap.verifier-readonly` | Task role is `verifier` or verification kind is `loopreview`. | Add `read_only: true`; `permission_required: read-only`; required output `verification-verdict`. | No. |
| `cap.nested` | Task may create child tasks, child plans, or provider-native sub-agents. | Add `nested_subagents: true`; `permission_required: orchestrate`; `risk_tier` at least `high`. | No. |
| `cap.cancellation` | Task may launch provider work, nested work, long-running commands, or half-open probes. | Add `cancellation: true` unless profile explicitly marks cancellation non-required for that side-effect class. | No. |
| `cap.token-usage-reporting` | Budget policy requires observed or estimated usage at task or child scope. | Add `token_usage_reporting: true` or require a UsageRecord estimator path from 0803. | No. |
| `data.secret-reference` | Scope or inputs include secret-reference names or credential-adjacent diagnostics. | data classification at least `secret-reference`; `risk_tier` at least `high`; verifier required. | No. |
| `data.secret-material` | Planned persisted, rendered, or provider-sent field would contain secret material. | Reject with 0742 secret-material refusal; no route is eligible. | No. |
| `policy.user-pin` | User pins provider, account, model, role profile, or verifier route. | Persist pin as authority input; use it to constrain candidate set after policy validation. | No. |
| `policy.override-requested` | User requests policy exception or route despite deterministic refusal. | Require Override record for exact authorization fingerprint. | No. |
| `quality.security-or-core` | Scope touches security, scheduler, routing, claims, policy, release, workflow, or storage surfaces. | `risk_tier` at least `high`; independent verification required. | Yes, may raise to `critical` with reason. |
| `quality.large-change` | Planned diff or graph estimate exceeds profile thresholds. | Require stronger quality floor or replan into smaller graph. | Yes, may raise risk or split tasks, but thresholds are profile numbers. |
| `quality.ambiguous-intent` | User intent, issue body, or planner scope cannot prove bounded task outcome. | Stop with `ErrRequirementUnknown` or require human clarification. | Yes, may mark ambiguity but cannot invent authority. |

## Bounded Dependency-Aware Task Graphs

Implementation issue #734 owns the first code implementation of this section.

### Graph Bounds

The default RoutingPolicyProfile `balanced-v1` must persist these graph bounds.
Profiles may lower them. Profiles may raise them only by changing
`profile_version`; the changed numbers become part of the policy fingerprint.

| Bound field | Default | Hard maximum | Enforcement |
| --- | --- | --- | --- |
| `max_tasks` | `32` | `64` | Reject plan with `ErrGraphBoundExceeded`. |
| `max_depth` | `4` | `6` | Depth from root intent to any task; reject before edge persistence. |
| `max_fan_out` | `8` | `16` | Maximum outgoing edges from one task. |
| `max_dependencies_per_task` | `8` | `16` | Maximum incoming edges to one task. |
| `max_parallel_ready` | `6` | `12` | Maximum tasks eligible to launch concurrently from one graph layer. |
| `max_replan_passes` | `2` | `4` | More re-planning returns `ErrReplanBoundExceeded`. |
| `max_graph_validation_ms` | `5000` | `30000` | Timeout returns `ErrInvalidRecord` and no partial graph. |
| `max_requirement_bytes` | `65536` | `262144` | Oversized requirement JSON is invalid. |
| `max_scope_paths_per_task` | `128` | `512` | Oversized scope requires split or human approval. |

These defaults are deterministic policy numbers, not model output. If a later
profile changes them, the profile record must explain the provenance.

### TaskGraphValidation

Schema version: `loopcoder.task_graph_validation.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `task_graph_validation_id` | yes | Immutable validation attempt identity. |
| `delivery_run_id` | yes | DeliveryRun whose task graph was validated. |
| `plan_fingerprint` | yes | Fingerprinted plan that includes ordered tasks and edges. |
| `task_count` | yes | Number of tasks in the graph. |
| `edge_count` | yes | Number of dependency edges in the graph. |
| `max_observed_depth` | yes | Observed depth from root intent. |
| `max_observed_fan_out` | yes | Largest outgoing edge count. |
| `max_observed_dependencies` | yes | Largest incoming edge count. |
| `parallel_ready_widths` | yes | Ordered width per DAG layer. |
| `validation_status` | yes | `passed`, `rejected`, or `needs-human`. |
| `rejected_reasons` | yes | Ordered typed reasons. |
| `cycle_path` | no | Ordered task IDs when a cycle is found; redacted if needed. |
| `disconnected_task_ids` | yes | Task IDs not reachable from root intent or terminal outcome. |
| `policy_bounds` | yes | Bounds copied from the RoutingPolicyProfile. |

Graph validation is atomic with task and edge persistence. If validation fails,
no partial task, edge, requirement, routing decision, approval, or attempt row
may remain for the invalid plan.

### Dependency Edge Semantics

0804 does not create a parallel dependency model. It specializes the 0801
Dependency Edge records as follows:

| `edge_kind` | Meaning in 0804 | Scheduling implication | Fingerprint implication |
| --- | --- | --- | --- |
| `requires` | Successor cannot start until predecessor terminal success proves a required artifact, fact, or approval. | Hard dependency; successor not ready until predecessor succeeds. | Included in ordered plan fingerprint and graph validation. |
| `blocks` | Predecessor result may block successor when it fails, finds risk, or requires human input. | Successor becomes `blocked` or `needs-human` when predecessor fails closed. | Included in ordered plan fingerprint and graph validation. |
| `orders-after` | Successor should run after predecessor to reduce conflict or preserve evidence order, but predecessor output is not a semantic input. | Scheduler honors order when both are in same graph; merge-time file overlap still follows existing scheduling specs. | Included in ordered plan fingerprint because it changes launch order authority. |

Cycle rejection reuses 0801 fail-closed conventions. Any cycle in any edge kind
returns `ErrGraphCycleDetected` or 0801 `ErrCycleDetected`, writes no partial
graph state, and leaves the DeliveryRun in `needs-human` when the invalid graph
was already surfaced for approval.

### Plan Fingerprint Interaction

The `plan_fingerprint` must include:

- ordered Task records with `task_key`, title, scope, permission, side-effect
  class, TaskRequirement ID and canonical payload hash;
- ordered Dependency Edge records with `edge_kind`, `from_task_id`,
  `to_task_id`, and `ordinal`;
- RoutingPolicyProfile ID and canonical payload hash;
- graph bounds and validation status;
- expected terminal tasks and verification requirements;
- every deterministic and heuristic planner decision that affected the graph.

Changing graph shape, edge kind, task requirement, routing profile, or graph
bound changes the plan fingerprint and invalidates stale approvals exactly as
0801 requires.

## Roles And Routing Policy Profiles

Implementation issue #736 owns the first code implementation of this section.

### RoleDefinition

Schema version: `loopcoder.role_definition.v1`.

RoleDefinition is provider-neutral. Built-in role keys are `worker`,
`verifier`, and `nested-subagent`. Policy profiles may add role keys, but role
keys still cannot bind to provider or model names.

| Field | Required | Meaning |
| --- | --- | --- |
| `role_definition_id` | yes | Stable role definition identity. |
| `role_key` | yes | `worker`, `verifier`, `nested-subagent`, or policy-defined role key. |
| `role_version` | yes | Version string for this role envelope. |
| `description` | yes | Bounded human summary. |
| `allowed_risk_tiers` | yes | Risk tiers this role may perform before additional profile checks. |
| `minimum_capabilities` | yes | CapabilityRequirement array using 0802 dimensions. |
| `permission_floor` | yes | Minimum permission the role must be able to enforce. |
| `permission_ceiling` | yes | Maximum permission this role may receive automatically. |
| `default_output_contract` | yes | Expected output contract for the role. |
| `independence_requirements` | yes | Required separation from related roles by risk tier. |
| `forbidden_bindings` | yes | Must include `provider_name`, `model_id`, and `account_profile_id` for built-ins. |

Built-in role floors:

| Role key | Required floor | Permission ceiling | Notes |
| --- | --- | --- | --- |
| `worker` | `roles_supported` includes `worker`; required output matches task. | `write` by default; `orchestrate` only when TaskRequirement requires nested work. | Worker route may mutate only the task scope. |
| `verifier` | `roles_supported` includes `verifier` or `audit-review`; `read_only: true`; `json_output: true` when verdict schema required. | `read-only`. | Verifier route must preserve independence and cannot become merge authority. |
| `nested-subagent` | `roles_supported` includes `nested-subagents`; `nested_subagents: true`; `cancellation: true`. | No stronger than parent delegated permission. | Phase 0805 owns execution; 0804 only classifies/routs eligibility. |

### RoutingPolicyProfile

Schema version: `loopcoder.routing_policy_profile.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `routing_policy_profile_id` | yes | Stable profile identity. |
| `profile_key` | yes | Operator-visible profile key such as `balanced-v1`. |
| `profile_version` | yes | Version string. |
| `policy_version` | yes | Deterministic policy version. |
| `graph_bounds` | yes | Bounds from the graph table. |
| `role_definition_ids` | yes | RoleDefinition records this profile uses. |
| `risk_policy` | yes | Minimum verification, approval, independence, and quality floors by risk tier. |
| `eligibility_policy` | yes | Deterministic eligibility rule set version. |
| `optimization_policy` | yes | Weight table and heuristic marker for ranking eligible candidates. |
| `fallback_policy` | yes | Legal fallback table version and bounds. |
| `replan_policy` | yes | Re-planning triggers and bounds. |
| `user_pin_policy` | yes | How pins constrain candidates without bypassing policy. |
| `provenance` | yes | Built-in, config, migration, or user source; includes canonical source hash. |
| `effective_from` | yes | UTC timestamp for profile availability. |
| `supersedes_profile_id` | no | Previous profile, when applicable. |

Profile changes are consequential. They must produce a new profile version and
must contribute to `policy_fingerprint`, `plan_fingerprint`, and
`routing_fingerprint` where applicable.

### User Pins

User pins are owned by the user per roadmap #714. A pin may constrain provider,
account profile, model capability, installation, role profile, verifier
independence, or maximum budget. A pin must be persisted as an Approval,
Override, Decision, or profile input with actor provenance.

Pin semantics:

- A pin narrows the candidate set before optimization.
- A pin may select an eligible candidate even if it is not the highest
  optimization score.
- A pin never makes an ineligible candidate eligible.
- A pinned candidate that violates deterministic policy returns
  `ErrPinnedCandidateIneligible` with the rejected reasons that caused refusal.
- A stale pin whose fingerprint inputs changed returns 0801 `ErrStaleApproval`
  or `ErrRoutingFingerprintMismatch`.

## Capability-First Explainable Routing

Implementation issue #735 owns the first code implementation of this section.

Routing is a two-stage rule:

1. **Eligibility first.** Evaluate capability, permission, risk, quality,
   verification, auth readiness, quota confidence, budget, availability, and
   circuit breakers against persisted 0802/0803 records and the policy profile.
   Rejected candidates leave typed reasons.
2. **Optimization second.** Rank only eligible candidates by quota, cost,
   latency, diversity, availability score, user preference, and other
   profile-declared soft factors. Every number is source-backed or marked
   heuristic.

### RoutingCandidate

Schema version: `loopcoder.routing_candidate.v1`.

RoutingCandidate may be materialized as a row or embedded in a RoutingDecision
snapshot. When persisted separately, it uses the ID scheme above.

| Field | Required | Meaning |
| --- | --- | --- |
| `routing_candidate_id` | yes | Stable candidate identity. |
| `task_id` | yes | Task being routed. |
| `role_key` | yes | Required role. |
| `adapter_id` | yes | 0802 provider adapter key. |
| `provider_installation_id` | no | 0802 ProviderInstallation reference when known. |
| `account_profile_id` | no | 0802 AccountProfile reference when known. |
| `auth_readiness_id` | no | 0802 AuthReadiness reference used for eligibility. |
| `model_catalog_snapshot_id` | no | 0802 catalog snapshot reference. |
| `model_capability_id` | yes | 0802 ModelCapability reference. |
| `invocation_profile_key` | yes | Adapter invocation profile used for launch. |
| `quota_snapshot_ids` | yes | 0803 quota evidence considered. |
| `budget_policy_ids` | yes | 0803 budget policies considered. |
| `budget_reservation_id` | no | Reservation when already acquired; absent before scheduling. |
| `availability_score_id` | no | 0803 availability score considered. |
| `circuit_breaker_ids` | yes | 0803 breaker records considered. |
| `capability_summary` | yes | Redacted bounded capability facts used for eligibility. |
| `candidate_fingerprint` | yes | Hash of candidate identity and referenced evidence records. |

### Eligibility Evaluation Matrix

The router must evaluate every applicable row. "Eligible when" is conjunctive:
a candidate is eligible only if every applicable row passes. "Rejected reason"
values are stable strings stored in rejected candidate records.

| Requirement category | Candidate evidence | Eligible when | Rejected reason |
| --- | --- | --- | --- |
| Role support | 0802 `roles_supported`, RoleDefinition | Required `role_key` is supported with fresh non-stale evidence. | `role-unsupported` |
| Permission enforcement | Invocation profile, RoleDefinition, TaskRequirement | Candidate can enforce `permission_required` exactly or more restrictively while still allowing required side effects; verifier remains read-only. | `permission-unsupported` |
| Side-effect class | TaskRequirement, policy profile, approval/override | Candidate launch and task side effects are within authorized 0801 side-effect class for the current fingerprint. | `side-effect-class-exceeded` |
| Scope boundary | TaskRequirement `scope_json`, provider invocation profile | Candidate can run within declared repo/path/issue/PR/data/provider/network scope without unbounded access. | `scope-unsupported` |
| Read-only support | 0802 `read_only` | Required read-only tasks have `read_only: true` with sufficient confidence. | `read-only-unsupported` |
| JSON output | 0802 `json_output` | Required JSON or verdict schema has `json_output: true` with sufficient confidence. | `json-output-unsupported` |
| Nested sub-agents | 0802 `nested_subagents`, RoleDefinition | Required nested work has `nested_subagents: true` and role permits `orchestrate`. | `nested-subagents-unsupported` |
| MCP config | 0802 `mcp_config` | Required MCP injection has `mcp_config: true`; unknown/stale does not pass. | `mcp-config-unsupported` |
| Cancellation | 0802 `cancellation`, TaskRequirement | Required cancellation has `cancellation: true`; long-running provider/nested work cannot use unknown cancellation. | `cancellation-unsupported` |
| Token usage reporting | 0802 `token_usage_reporting`, 0803 UsageRecord estimator | Required usage reporting has provider support or a policy-approved local estimator path. | `usage-reporting-unsupported` |
| Context window | 0802 `context_window_tokens`, TaskRequirement estimate | Fresh context limit is greater than or equal to required context tokens plus profile reserve; unknown/stale rejects hard requirement. | `context-window-insufficient` |
| Tool support | 0802 `tool_support` | Required tool dimensions are supported by structured evidence. | `tool-support-unsupported` |
| Image input | 0802 `image_input` | Required image input has `true` with sufficient confidence. | `image-input-unsupported` |
| Image output | 0802 `image_output` | Required image output has `true` with sufficient confidence. | `image-output-unsupported` |
| Model lifecycle | 0802 ModelCapability | Model lifecycle is `available` in a fresh applicable catalog snapshot. | `model-unavailable` |
| Auth readiness | 0802 AuthReadiness | Readiness is fresh `ready` for the selected provider/account/profile scope. | `auth-not-ready` |
| Account/profile collision | 0802 AccountProfile | Selected account/profile is collision-safe by opaque ID, not display text alone. | `account-profile-ambiguous` |
| Quota confidence | 0803 QuotaSnapshot, quota policy | Required quota confidence is satisfied; exact-policy tasks require fresh exact evidence. | `quota-confidence-insufficient` |
| Budget capacity | 0803 BudgetPolicy and BudgetReservation | Reservation can be acquired atomically without exceeding hard budget ceilings. | `budget-exhausted` |
| Availability hard reasons | 0803 AvailabilityScore | No `hard_ineligible_reasons` apply. Score alone cannot make candidate eligible. | `availability-hard-ineligible` |
| Circuit breaker | 0803 CircuitBreaker | Relevant breakers are `closed`, or `half-open` with an authorized recovery probe for this exact scope. | `breaker-open` |
| Network permission | 0742 network controls, profile, approval | Network is `not-needed`, or network permission is granted for provider, purpose, side-effect class, and fingerprint. | `network-permission-missing` |
| Data classification | 0742 classification, TaskRequirement | Candidate output/prompt/storage path can handle the highest required classification without secret material. | `data-classification-unsupported` |
| Risk tier | TaskRequirement, RoleDefinition, profile | Candidate role is allowed for the risk tier and required approvals/overrides exist. | `risk-tier-unsupported` |
| Quality floor | RoutingPolicyProfile, model capability/source quality | Candidate satisfies `standard`, `strong`, or `adversarial` floor as declared by profile. | `quality-floor-insufficient` |
| Verification requirement | VerificationRequirement, candidate role | Required verifier route can be produced with required output contract and permission. | `verification-route-missing` |
| Verifier independence | Worker route, verifier candidate, profile | Verifier is independent at the required level for task risk. | `verifier-independence-insufficient` |
| User pin | User pin record, candidate identity | Candidate matches pin when a pin exists; if pinned candidate is otherwise ineligible, reject with both pin and root reasons. | `pinned-candidate-not-matched` or `pinned-candidate-ineligible` |
| Freshness | All referenced 0802/0803 records | Every hard evidence record is fresh for the profile's required freshness window. | `evidence-stale` |
| Fingerprint consistency | Candidate fingerprint, routing fingerprint | Candidate evidence hashes reproduce stored fingerprints. | `routing-fingerprint-mismatch` |
| Unknown enum/version | Any referenced record | Record versions and enum values are known to the binary. | `unknown-record-version` |

Unknown, unavailable, stale, malformed, or conflicting evidence rejects hard
requirements unless the RoutingPolicyProfile explicitly allows a conservative
estimated path for that exact requirement. Such a path must still be persisted
as `heuristic: true` or `confidence: "estimated"` and cannot satisfy an exact
policy.

### Optimization Policy

Optimization uses only eligible candidates. The default `balanced-v1`
optimization policy uses these persisted weights. Each component produces
`0..100`; the weighted total is stored as decimal strings to avoid floating
point fingerprint ambiguity.

| Component | Default weight | Source | Heuristic? |
| --- | --- | --- | --- |
| `availability` | `30` | 0803 AvailabilityScore and breaker state. | Score may be heuristic per 0803. |
| `quota_headroom` | `20` | 0803 QuotaSnapshot, BudgetPolicy, and BudgetReservation records. | Estimated quota remains heuristic/estimated. |
| `quality_fit` | `20` | RoleDefinition, ModelCapability, policy profile quality floor, and fixture/provider conformance results. | Yes unless backed by deterministic conformance record. |
| `latency` | `10` | Persisted recent UsageRecord or AvailabilityObservation timing. | Yes unless provider supplies exact timing semantics. |
| `cost` | `10` | BudgetPolicy, provider-declared unit costs, or operator policy overlay. | Yes unless exact local policy cost table is used. |
| `diversity` | `10` | Prior selected routes in the same DeliveryRun. | Deterministic when calculated from persisted route history. |

The sum must be `100`. If a profile changes weights, the profile version and
policy fingerprint change. If a component lacks evidence, it contributes `0`
unless the profile defines a conservative estimated component; the component
must then be marked `heuristic: true` with confidence and inputs.

### RoutingDecision

Schema version: `loopcoder.routing_decision.v1`.

The RoutingDecision field table is exhaustive for the initial implementation.
Additional fields require a new schema version.

| Field | Required | Meaning |
| --- | --- | --- |
| `routing_decision_id` | yes | Stable decision identity. |
| `decision_key` | yes | Caller-stable key for idempotent replay. |
| `decision_kind` | yes | Must be `routing`. |
| `project_id` | yes | Owning project. |
| `delivery_run_id` | yes | Owning DeliveryRun. |
| `task_id` | yes | Task being routed. |
| `task_requirement_id` | yes | Requirement record evaluated. |
| `routing_policy_profile_id` | yes | Profile used for eligibility and optimization. |
| `role_definition_id` | yes | Role envelope evaluated. |
| `plan_fingerprint` | yes | Current plan fingerprint. |
| `policy_fingerprint` | yes | Current policy fingerprint. |
| `routing_fingerprint` | yes | Fingerprint over requirements, profile, candidate evidence, pins, and scoring inputs. |
| `input_record_refs` | yes | Ordered references to 0801, 0802, 0803, approval, override, and profile records used. |
| `candidate_generation_status` | yes | `complete`, `partial`, or `needs-human`. |
| `eligible_candidates` | yes | Ordered RoutingCandidate snapshots that passed every eligibility row. |
| `rejected_candidates` | yes | Ordered rejected candidate snapshots with typed reasons and source rows. |
| `scored_candidates` | yes | Eligible candidates with component scores, weights, total score, confidence, and heuristic flags. |
| `chosen_candidate_id` | conditional | Required when `decision_status` is `selected`. |
| `chosen_reason` | conditional | Deterministic or heuristic explanation for selection. |
| `user_pin_refs` | yes | Ordered user pin or approval/override IDs that constrained routing. |
| `fallback_chain` | yes | Ordered fallback candidate IDs and legality checks. Empty if no fallback exists. |
| `verifier_route_requirement` | no | Required verifier independence and output contract when applicable. |
| `verifier_candidate_id` | no | Selected verifier candidate when selected in same decision. |
| `budget_reservation_request` | no | Requested reservation shape before scheduler acquisition. |
| `breaker_gate_refs` | yes | Circuit breakers that gated candidate eligibility. |
| `optimization_policy` | yes | Weight table copied from profile. |
| `heuristic_components` | yes | Ordered fields/components marked heuristic with reasons. |
| `decision_status` | yes | `selected`, `no-eligible-candidate`, `needs-human`, or `stale`. |
| `rejected_summary` | yes | Aggregated rejected reason counts for human output. |
| `created_at` / `updated_at` | yes | UTC timestamps. |
| `decided_by` | yes | 0801 provenance with `decision_authority: "router"` or `"user"` for explicit pins. |
| `host` | yes | Host provenance. |
| `terminal_error_code` | no | `ErrNoEligibleCandidate`, `ErrPinnedCandidateIneligible`, or another typed refusal. |

Rejected candidates must include enough bounded evidence for a user to
understand why they failed without exposing credential material, raw provider
output, unredacted paths, account labels beyond safe display, or local-only
reporter blocks.

## Policy-Preserving Fallback

Implementation issue #737 owns fallback, re-planning, and independent
verification behavior.

### Fallback Legality Matrix

Fallback is legal only when every row permits the change. "May degrade" means
the fallback may choose a lower soft score for that dimension after eligibility
passes. "Never degrade" means fallback must reject and either try another
candidate or require re-planning/human action.

| Dimension | May degrade? | Legal fallback | Illegal fallback |
| --- | --- | --- | --- |
| Permission enforcement | Never | Same or stricter enforceable permission within approved scope. | Any route that cannot enforce `read-only`, `write`, or `orchestrate` as required. |
| Minimum capability | Never | Same required capabilities with sufficient confidence and freshness. | Missing, unknown, unavailable, or stale hard capability. |
| Side-effect class | Never | Same or lower side-effect class than approved. | Stronger side-effect class without fresh approval/override. |
| Scope | Never silently | Same or narrower scope. | Wider repo/path/data/provider/network scope without re-planning and approval. |
| Verifier independence | Never | Same or stronger independence level. | Same provider/account/model when profile requires separation. |
| Verification requirement | Never | Same verification kind/output contract or stronger one. | Dropping loopreview, human approval, JSON verdict, or required checks. |
| Risk tier | Never lower | Same or higher risk handling. | Reclassifying high/critical work downward to route it. |
| Auth readiness | Never | Fresh `ready` readiness for selected account/profile. | Unknown, expired, not-authenticated, or stale readiness. |
| Quota confidence | Profile-dependent | Same required confidence; estimated only when profile allowed it before approval. | Falling from exact-required to estimated/unknown/stale. |
| Budget ceiling | Never | Reservation fits the same or narrower budget envelope. | Exceeding hard budget, using another scope budget without authority. |
| Circuit breaker | Never | Closed breaker or authorized half-open probe. | Launching through open breaker or unauthorized half-open probe. |
| Provider/model identity | Yes, if eligible | Different provider/model/account that satisfies all hard rows and pins. | Candidate violates a user pin or deterministic profile allow/deny rule. |
| Availability score | Yes | Lower score may be selected when still eligible and recorded. | Score used to bypass hard ineligible reason. |
| Quota headroom | Yes, after required confidence | Lower headroom among eligible candidates. | Candidate lacks required quota evidence or reservation. |
| Cost | Yes | Higher cost among eligible candidates when within budget and profile permits. | Cost exceeds budget or user cap. |
| Latency | Yes | Slower eligible candidate. | Timeout/cancellation requirement cannot be met. |
| Diversity | Yes | Less diverse eligible candidate if no diversity hard rule applies. | Reusing same worker/verifier identity when independence requires separation. |
| User pin | Never silently | Pinned eligible candidate or explicit user-updated pin. | Ignoring a pin because another candidate scores higher. |
| Plan graph | No | Same graph and requirements. | Adding, removing, splitting, merging, or reordering tasks; this is re-planning. |

Any illegal fallback returns `ErrFallbackWouldWeakenPolicy` or
`ErrReplanRequired`. It must not launch a provider or mutate task state.

### FallbackDecision

Schema version: `loopcoder.fallback_decision.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `fallback_decision_id` | yes | Stable fallback decision identity. |
| `routing_decision_id` | yes | Original route decision. |
| `fallback_ordinal` | yes | Ordered fallback attempt number starting at `1`. |
| `trigger` | yes | `candidate-failed`, `breaker-opened`, `quota-exhausted`, `rate-limited`, `auth-expired`, `model-removed`, `budget-refused`, `verification-failed`, or `user-requested`. |
| `prior_candidate_id` | yes | Candidate being replaced. |
| `fallback_candidate_id` | no | Candidate selected, if legal. |
| `legality_results` | yes | Row-by-row fallback legality table results. |
| `decision_status` | yes | `selected`, `replan-required`, `needs-human`, or `blocked`. |
| `routing_fingerprint` | yes | Fingerprint over original routing inputs and updated fallback evidence. |
| `terminal_error_code` | no | Typed refusal when fallback is blocked. |

Fallback does not change the TaskRequirement or task graph. If fallback needs a
different requirement or graph, it becomes re-planning.

### Re-Planning

Schema version: `loopcoder.replan_decision.v1`.

Re-planning is allowed only through bounded deterministic triggers:

| Trigger | Required action |
| --- | --- |
| `no-eligible-candidate` | Persist rejected candidates, return `ErrNoEligibleCandidate`, and either ask user or create a new plan within bounds. |
| `legal-fallback-exhausted` | Persist fallback chain and start re-plan only if `max_replan_passes` remains. |
| `scope-change-needed` | Create new plan fingerprint; stale approvals cannot authorize wider scope. |
| `capability-gap` | Split, simplify, or ask user; do not pretend the hard capability is optional. |
| `graph-bound-hit` | Stop with `ErrGraphBoundExceeded` unless user accepts a smaller plan. |
| `verification-failed` | Re-plan fix work only within original approved scope or require fresh approval. |
| `ambiguous-side-effect-state` | Stop to `needs-human` unless durable evidence proves safe continuation. |
| `user-changed-intent` | New input fingerprint and plan fingerprint. |

ReplanDecision fields:

| Field | Required | Meaning |
| --- | --- | --- |
| `replan_decision_id` | yes | Stable replan identity. |
| `delivery_run_id` | yes | Owning DeliveryRun. |
| `prior_plan_fingerprint` | yes | Plan being replaced. |
| `new_plan_fingerprint` | no | New plan if produced. |
| `replan_ordinal` | yes | Ordered replan count in this DeliveryRun. |
| `trigger` | yes | One trigger from the table above. |
| `bounds_remaining` | yes | Replan and graph bounds remaining after this decision. |
| `changed_authority_inputs` | yes | Which input, policy, plan, or routing fingerprints changed. |
| `approval_required` | yes | Whether fresh approval or override is required. |
| `decision_status` | yes | `planned`, `blocked`, `needs-human`, or `bound-exceeded`. |
| `terminal_error_code` | no | Typed refusal when blocked. |

### Independent Verification

High-risk and critical work requires independent verification even when worker
fallback changes the selected provider. Verifier routing is its own
RoutingDecision or a verifier section in the worker RoutingDecision. It must
preserve:

- `permission_required: read-only`;
- required output contract, usually `verification-verdict` or `json-schema`;
- independence level from the TaskRequirement and profile;
- access only to bounded PR, diff, spec, check, and report summaries;
- no ability to approve overrides, weaken policy, or mark final acceptance.

Independence levels are ordered:

| Level | Meaning |
| --- | --- |
| `none` | No separation required. |
| `different-model` | Verifier model capability ID differs from worker model capability ID. |
| `different-account` | Verifier account profile differs from worker account profile. |
| `different-provider` | Verifier adapter differs from worker adapter. |
| `human` | Human review or approval is required; automated verifier may still assist. |

Fallback may strengthen independence but never weaken it.

## Storage And JSON Output

### Machine-Local Storage

Records live in the machine-local SQLite store under the global project storage
model from
[`0639-global-data-layout-project-identity.md`](0639-global-data-layout-project-identity.md).
The initial v0.8 schema additions are logical tables equivalent to:

- `task_requirements`;
- `task_graph_validations`;
- `role_definitions`;
- `routing_policy_profiles`;
- `routing_candidates`;
- `routing_decisions`;
- `fallback_decisions`;
- `replan_decisions`;
- `routing_events` for tamper-evident authority events when route records
  affect DeliveryRun inputs, task launch, fallback, or verification.

The v0.8 migration must follow the 0801 migration conventions: backup metadata
before migration, one immediate SQLite write transaction for schema changes,
idempotent re-run, conservative handling of ambiguous legacy state, no
repository mutation, no credential migration, no provider launch, and
machine-local backup paths only.

### Doctor JSON

`loopcoder doctor --format json` must expose decomposition and routing
diagnostics as an additive root object named `decomposition_routing`. Existing
fields remain valid. The new object contains redacted, bounded,
machine-readable state:

```json
{
  "decomposition_routing": {
    "schema_version": "loopcoder.decomposition_routing_json.v1",
    "generated_at": "2026-07-12T00:00:00Z",
    "routing_policy_profiles": [],
    "role_definitions": [],
    "graph_bounds": {},
    "eligibility_policy_version": "0804.eligibility.v1",
    "fallback_policy_version": "0804.fallback.v1",
    "gap_reasons": []
  }
}
```

Doctor JSON must distinguish:

- missing provider inventory required for routing;
- stale model capability or auth readiness;
- unknown quota or unavailable quota source;
- open circuit breaker;
- missing verifier independence;
- configured user pin that is currently ineligible;
- profile version or enum unknown to the binary.

Secret material, raw provider output, unredacted local paths, local-only
reporter blocks, and credential-bearing account labels have no valid JSON form.

### Status JSON

`loopcoder status --format json` must expose references and summaries only when
decomposition or routing records affect a DeliveryRun, Task, attempt, fallback,
or verification route:

```json
{
  "decomposition_routing_refs": {
    "schema_version": "loopcoder.decomposition_routing_refs.v1",
    "plan_fingerprint": "sha256:...",
    "routing_fingerprint": "sha256:...",
    "task_requirement_ids": [],
    "task_graph_validation_ids": [],
    "routing_decision_ids": [],
    "fallback_decision_ids": [],
    "replan_decision_ids": [],
    "routing_policy_profile_id": "rprof_...",
    "decision_status": "selected",
    "hard_ineligible_reasons": [],
    "gap_reasons": []
  }
}
```

When status explains a blocked or needs-human state, it may include bounded
redacted summaries of rejected candidates, fallback illegality rows, and
replan triggers. It must not duplicate full raw inventory, quota, usage,
provider output, local logs, or credential-bearing diagnostics by default.

## Failure Honesty Rules

These rules are normative and testable:

- Eligibility is a gate before optimization. A high score cannot make an
  ineligible candidate eligible.
- Unknown, unavailable, stale, malformed, or conflicting hard evidence rejects
  a hard requirement unless the policy profile explicitly allowed an estimated
  path for that exact requirement.
- Heuristics may rank eligible candidates and may raise risk, but they must
  not lower deterministic risk, bypass approvals, or create side-effect
  authority.
- User pins are explicit authority inputs. They can choose among eligible
  candidates, but they cannot bypass policy, capability, auth, quota, budget,
  breaker, scope, or verification requirements.
- Fallback may trade off quota headroom, cost, latency, diversity, and soft
  availability score among eligible candidates. It must never silently weaken
  permission, verifier independence, minimum capability, scope, side-effect
  class, budget authority, approval freshness, or required evidence confidence.
- Re-planning changes fingerprints. Stale approvals and overrides cannot
  authorize changed graphs, widened scope, stronger side effects, or changed
  verification requirements.
- Roles and profiles are provider-neutral. Built-in role names and future
  profile labels must not encode provider or model names.
- Circuit breakers gate candidates before scoring. Open breakers are not a
  latency or availability penalty; they are hard ineligibility unless a
  half-open probe is explicitly authorized.
- Independent verification remains read-only and independent even after worker
  fallback changes provider/model/account.
- Rejected candidate reasons must be persisted and inspectable in human and
  JSON output without leaking credentials or raw provider output.

## Implementation Acceptance Mapping

### #733 Task Requirement/Risk Classifier

Issue #733 is complete only when its code and tests implement the sections
"Task Requirement Classification", "TaskRequirement", "Risk Tiers",
"VerificationRequirement", and "Deterministic Classifier Rules":

- TaskRequirement records are versioned, immutable for a plan fingerprint, and
  include role, capability, permission, side-effect class, risk tier, output,
  verification, scope, data classification, provenance, and heuristic markers;
- deterministic rules reuse 0802 capability dimensions and 0801 side-effect
  classes exactly;
- heuristics are explicitly marked, may raise risk, and cannot lower policy
  floors;
- unknown or secret-material requirements fail closed with typed errors;
- JSON fixtures cover low, medium, high, and critical classifications.

### #734 Bounded Dependency-Aware Task Graphs

Issue #734 is complete only when its code and tests implement the sections
"Bounded Dependency-Aware Task Graphs", "Graph Bounds",
"TaskGraphValidation", "Dependency Edge Semantics", and "Plan Fingerprint
Interaction":

- graph construction enforces max tasks, depth, fan-out, dependencies, parallel
  ready width, replan passes, validation timeout, and payload-size bounds;
- 0801 Dependency Edge records are reused rather than duplicated;
- cycles, disconnected graphs, cross-project edges, unknown enum values, and
  bound violations fail atomically with no partial graph writes;
- plan fingerprints include task requirements, graph shape, edge semantics,
  bounds, validation status, and planner heuristic decisions;
- deterministic tests prove equivalent inputs reproduce equivalent graph
  validation and fingerprints.

### #735 Explainable Multi-Provider Router

Issue #735 is complete only when its code and tests implement the sections
"Capability-First Explainable Routing", "RoutingCandidate", "Eligibility
Evaluation Matrix", "Optimization Policy", and "RoutingDecision":

- eligibility evaluates every applicable row before scoring;
- rejected candidates persist typed reasons and source record references;
- scored candidates include component scores, weights, total score, confidence,
  and heuristic flags;
- all consequential numbers trace to 0802/0803 records, profile fields, or
  explicitly marked heuristics;
- user pins select only among eligible candidates and produce
  `ErrPinnedCandidateIneligible` when policy rejects the pin;
- doctor/status JSON can explain no eligible candidate, stale evidence, open
  breaker, quota confidence failure, and verifier route failure.

### #736 Roles And Routing Profiles

Issue #736 is complete only when its code and tests implement the sections
"Roles And Routing Policy Profiles", "RoleDefinition",
"RoutingPolicyProfile", and "User Pins":

- built-in worker, verifier, and nested-subagent role definitions are
  provider-neutral and do not bind to provider/model/account names;
- policy profile versioning, provenance, graph bounds, risk policy,
  eligibility policy, optimization policy, fallback policy, replan policy, and
  pin policy are persisted and fingerprinted;
- changing a profile version changes the relevant policy, plan, and routing
  fingerprints;
- user pins are persisted with user provenance and cannot bypass deterministic
  policy;
- fixtures prove adding a provider-neutral role/profile does not require
  hard-coded provider/model assumptions.

### #737 Fallback, Re-Planning, And Independent Verification

Issue #737 is complete only when its code and tests implement the sections
"Policy-Preserving Fallback", "Fallback Legality Matrix",
"FallbackDecision", "Re-Planning", and "Independent Verification":

- fallback legality evaluates every matrix row and rejects any weakening of
  permission, verifier independence, minimum capability, scope, side-effect
  class, budget, approval freshness, or required confidence;
- fallback can select a lower-scoring eligible candidate only when policy is
  preserved and the decision is recorded;
- re-planning triggers create new fingerprints, enforce bounded pass counts,
  and require fresh approval when authority inputs change;
- high-risk and critical work route independent read-only verification with
  required independence level;
- circuit breakers, quota exhaustion, auth expiry, model removal, verification
  failure, and ambiguous side-effect state are tested as distinct triggers.

## Relationship To Existing Specs And Docs

- [`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md) defines
  DeliveryRun, Task, Dependency Edge, Decision, Approval, Override,
  fingerprints, side-effect classes, typed errors, provenance, and decision
  ownership boundaries. This spec uses those records as the authority substrate
  for task requirements, graphs, routing, fallback, and re-planning.
- [`0802-provider-inventory.md`](0802-provider-inventory.md) defines provider
  installation, account/profile, auth readiness, model capability, capability
  dimensions, invocation profiles, freshness, confidence, and future-provider
  adapter contracts. This spec references those facts for route eligibility.
- [`0803-quota-usage-budget.md`](0803-quota-usage-budget.md) defines quota
  snapshots, usage records, budget reservations, availability scores, and
  circuit breakers. This spec treats those records as routing inputs and
  candidate gates, not as route decisions by themselves.
- [`0742-v080-security-threat-model.md`](0742-v080-security-threat-model.md)
  defines routing security controls, credential boundaries, network opt-in,
  classified output, and policy-first routing. This spec makes those controls
  concrete for #733 through #737.
- [`0646-nested-sub-agent-plan.md`](0646-nested-sub-agent-plan.md) defines
  nested run depth, claims, leases, fencing, provider idempotency keys, and
  conservative stale-completion behavior. This spec reuses those constraints
  for nested-subagent requirement classification and routing eligibility while
  deferring federation execution to phase 0805.
- [`0639-global-data-layout-project-identity.md`](0639-global-data-layout-project-identity.md)
  defines machine-local global storage and project identity. Decomposition and
  routing records are stored there and use `project_id` as the isolation key.
