---
id: 805
title: v0.8.0 Phase 5 Agent Federation
status: draft
date: 2026-07-12
issue: 808
pr: null
supersedes: []
superseded_by: []
---

# v0.8.0 Phase 5 Agent Federation

This documentation-only spec freezes the phase-5 provider-native sub-agent
federation contract that implementation issues
[#738](https://github.com/jasonhnd/loopcoder/issues/738),
[#740](https://github.com/jasonhnd/loopcoder/issues/740), and
[#739](https://github.com/jasonhnd/loopcoder/issues/739) must implement for the
v0.8.0 agent-federation epic
[#719](https://github.com/jasonhnd/loopcoder/issues/719). It is delivery order
step 5 from roadmap [#714](https://github.com/jasonhnd/loopcoder/issues/714):
provider-native child execution may be enabled only after durable run
contracts, provider inventory, quota/budget records, and routing policy are
defined.

This spec follows the shared durable-record conventions from
[`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md): opaque
stable IDs, explicit `schema_version` and `record_version`, actor and host
provenance, UTC timestamps, side-effect classes, typed errors, canonical
fingerprints, idempotency, one-transaction mutation rules, and decision
ownership boundaries. It reuses provider inventory and runtime capability facts
from [`0802-provider-inventory.md`](0802-provider-inventory.md), hierarchical
budgets and reservations from
[`0803-quota-usage-budget.md`](0803-quota-usage-budget.md), routing and role
eligibility from [`0804-decomposition-routing.md`](0804-decomposition-routing.md),
security red lines from
[`0742-v080-security-threat-model.md`](0742-v080-security-threat-model.md), and
the accepted nested run tree, execution-claim, lease, heartbeat, fencing,
idempotency-key, provider-receipt, aggregation, and conservative recovery
semantics from
[`0646-nested-sub-agent-plan.md`](0646-nested-sub-agent-plan.md).

This document adds no Go code, SQLite migration, CLI behavior, workflow change,
provider integration, UX rendering, observability UI, evaluation, migration, or
release behavior. Per [`../PROCESS.md`](../PROCESS.md), this spec must merge
before the implementation issues above implement it.

## Goals

- Define the provider-native sub-agent bridge and durable registration records
  that must exist before a native child starts.
- Reuse `0646` nested-run claim, lease, heartbeat, fencing, idempotency, and
  recovery machinery rather than defining a parallel launcher.
- Define canonical read, write, path, repository, worktree, command, network,
  credential, and side-effect scopes with monotonic inheritance.
- Define ownership and one-writer isolation for repository files, worktrees,
  local runtime records, task state, provider receipts, GitHub resources, and
  external side-effect targets.
- Define dynamic nested run, cancel, and recovery behavior under the `0803`
  hierarchical budget ceilings.
- Define durable storage and JSON exposure for agent-tree registration,
  scope grants, ownership locks, budget references, doctor output, and status
  output.
- Provide implementation acceptance mapping for #738, #740, and #739.

## Non-Goals

- No UX, graph visualization, observability rendering, or event timeline UI.
  Those belong to #741.
- No evaluation harness, simulation scoring, or model-quality measurement.
  Those belong to #743.
- No v0.8 migration, release packaging, GO evidence, or release gate behavior.
  Those belong to #744.
- No new provider capability probing beyond the inventory contract in `0802`.
- No replacement for `0646` run edges, execution claims, leases, heartbeats,
  fencing, idempotency keys, provider receipts, or aggregation.
- No OS sandbox, container, hypervisor, cloud control plane, or provider-side
  access-control guarantee.
- No stored credential material and no credential delegation to child agents.

## Red Line

Child agents NEVER bypass LoopCoder budgets, scopes, routing policy,
approvals, one-writer ownership, execution claims, cancellation, recovery, or
final acceptance. A provider-native child is an execution technique inside a
LoopCoder-owned DeliveryRun. It is not an independent control plane and it
does not inherit global authority from its parent or provider.

Any code path that cannot prove the child has an active registration, a valid
scope, an owned `0646` execution claim, a live budget reservation, and any
required approval for the exact fingerprints must refuse before provider
launch. Any code path that cannot prove whether a provider-native child
launched or wrote externally must fail closed to `needs-human`.

## Terms

**Agent federation** is LoopCoder's controlled delegation of scoped work to
LoopCoder-managed workers or provider-native sub-agents while keeping
LoopCoder as the owner of budgets, scopes, policy, approvals, registration,
claims, cancellation, recovery, and final acceptance.

**Provider-native sub-agent** is a child execution unit created through a
provider feature that can run subordinate agent work inside the provider's own
runtime. LoopCoder still records it as a child run and applies all constraints
before launch, during execution, and at completion.

**Agent registration** is the durable record that binds a child agent identity
to its parent run, task, attempt, scope, permission, budget reservation,
ownership, provider session reference, plan fingerprint, policy fingerprint,
authorization fingerprint, and `0646` execution claim.

**Agent tree** is the durable parent-child structure formed by `0646` runs and
run edges plus the agent registration records in this spec.

**Scope grant** is the canonical, fingerprinted permission envelope delegated
to one child. It contains read, write, path, repository, worktree, command,
network, credential, and side-effect scope dimensions.

**Monotonic inheritance** means a child scope can only be equal to or narrower
than the effective parent scope. It can never widen, add a new dimension, raise
side-effect class, add network authority, add command authority, gain
credential access, or escape the parent repository or worktree boundary.

**Ownership lock** is a durable one-writer claim for a mutable resource. It is
separate from the `0646` execution claim: the execution claim grants launch
ownership for a child run, while ownership locks grant exclusive write
authority for resources the child may mutate.

**Budget reference** means the exact `0803` BudgetPolicy and
BudgetReservation rows that reserve capacity for the child. A child may spend
only within its reservation and applicable ancestor ceilings.

## Provider Eligibility

Provider eligibility comes from `0802` provider inventory and
[`../reference/runtime-capabilities.md`](../reference/runtime-capabilities.md).
The live matrix for nested sub-agents is:

| Provider | Nested sub-agents | Federation launch rule |
| --- | --- | --- |
| `claude` | supported | Eligible only when inventory, routing, scope, budget, claim, cancellation, and approval checks pass. |
| `codex` | unsupported | Must fail closed before native-sub-agent launch. It may still run ordinary Worker or Verifier roles where eligible. |
| `gemini` | unsupported | Must fail closed before native-sub-agent launch. Experimental direct Gemini worker support does not imply federation support. |
| `antigravity` | unsupported | Must fail closed before native-sub-agent launch. Antigravity is worker-only in the live matrix and lacks read-only, JSON, MCP, token-usage, and nested-agent support. |

Unknown, stale, unavailable, or conflicting `nested_subagents` capability
evidence never satisfies a hard federation requirement. A future provider may
become eligible only through the `0802` future-provider adapter contract and
fresh inventory evidence; the scheduler core must not hard-code provider names
as authority.

## Execution Substrate

This spec extends, but does not replace, `0646`:

- Every provider-native child is a normal child run with `run_id`,
  `parent_run_id`, `root_run_id`, `depth`, `run_edges`, and child-plan
  metadata from `0646`.
- Every provider-native child must have an accepted child plan item before
  registration.
- Provider launch requires an active `0646` `run_claims` row owned by the
  scheduler generation that is launching the child.
- Claim acquisition, child run transition, parent edge transition, agent
  registration, budget reservation attachment, and initial ownership locks
  must commit in one immediate SQLite write transaction.
- Claim owners renew leases and heartbeats with fenced `run_id`,
  `executor_id`, and `claim_generation` predicates.
- Completion, cancellation, and recovery persist terminal state only through
  the fenced owner/generation rules from `0646`.
- Provider idempotency keys and provider receipts keep the exact `0646`
  meaning: an idempotency key is not proof of completion, and a receipt must
  come from a real provider response, external resource ID, or verifiable local
  execution record.

## Shared Representation

The implementation must expose one logical contract across Go, SQLite, and
JSON:

- JSON field names use snake_case, explicit `schema_version` strings, and
  stable enum strings.
- SQLite stores queryable identity, lifecycle, scope, policy, budget, claim,
  ownership, and relationship fields in normal columns. Bounded structured
  payloads may live in `*_json` columns only when they are not needed for
  joins, uniqueness, lifecycle transitions, lock conflict checks, budget
  checks, or policy decisions.
- SQLite `TEXT` IDs are opaque. Callers must not parse provider names,
  timestamps, task keys, scope dimensions, or paths from IDs.
- Every record has `schema_version`, `record_version`, timestamps,
  provenance, classification, policy version, fingerprint references, and typed
  terminal error fields where refusal can be represented.
- Project-scoped records carry `project_id`; DeliveryRun-scoped records also
  carry `delivery_run_id`; task-scoped records carry `task_id`; child-scoped
  records carry `child_agent_id` and `run_id`. Cross-project references fail
  with `ErrCrossProjectReference`.
- All timestamps are UTC RFC3339 strings rendered with `Z`.
- Unknown enum values in persisted records fail closed with
  `ErrUnknownRecordVersion` or `ErrInvalidRecord`, matching `0801`.
- Records that affect DeliveryRun authority, task start eligibility, provider
  launch, child cancellation, recovery, or terminal acceptance must contribute
  their exact record IDs and canonical payload hashes to the applicable
  `input_fingerprint`, `policy_fingerprint`, `plan_fingerprint`,
  `routing_fingerprint`, or `agent_federation_fingerprint`.
- Child output is `provider-output-untrusted` until parsed, classified,
  bounded, and verified under `0742`.

### ID Scheme

ID prefixes are stable; the bytes after the prefix are opaque:

| Record | ID field | Required form |
| --- | --- | --- |
| AgentRegistration | `child_agent_id` | `agent_<base32-sha256(project_id, delivery_run_id, parent_run_id, task_id, attempt_id, child_key, plan_fingerprint)>`. |
| AgentScopeGrant | `agent_scope_grant_id` | `ascope_<base32-sha256(child_agent_id, scope_canonical_json, policy_fingerprint)>`. |
| AgentOwnershipLock | `agent_ownership_lock_id` | `alock_<base32-sha256(project_id, resource_kind, resource_key, child_agent_id)>` plus a collision suffix only for different canonical replay bytes. |
| AgentBudgetBinding | `agent_budget_binding_id` | `abudget_<base32-sha256(child_agent_id, budget_reservation_id, budget_policy_id)>`. |
| AgentEvent | `agent_event_id` | `aevt_<uuidv7-or-random-128-bit-base32>`. |
| Agent federation fingerprint | `agent_federation_fingerprint` | The digest string itself: `sha256:<64-lower-hex>`. |

`child_key` is the stable key from the `0646` child plan item. Replaying the
same canonical registration request returns the existing record. Replaying the
same ID with different canonical request bytes fails with `ErrDuplicateReplay`.

### Common Fields

All durable records in this spec carry the following fields unless a record
table explicitly marks one as not applicable:

| Field | Required | Meaning |
| --- | --- | --- |
| `schema_version` | yes | Stable JSON/storage shape string. |
| `record_version` | yes | Optimistic update version for mutable records; immutable events keep `1`. |
| `project_id` | yes | Project identity from `0639`, reused through `0801`. |
| `delivery_run_id` | yes | Owning DeliveryRun from `0801`. |
| `root_run_id` | yes | Root run from `0646`. |
| `parent_run_id` | conditional | Required for child records. |
| `run_id` | conditional | Child run ID from `0646` when the record belongs to one run. |
| `task_id` | conditional | `0801` task that owns the child work. |
| `attempt_id` | conditional | `0801` attempt that launches or supervises the child. |
| `created_at` / `updated_at` | yes | UTC timestamps. |
| `created_by` / `updated_by` | yes | `0801` actor provenance object. |
| `host` | yes | `0801` host provenance object. |
| `policy_version` | yes | Deterministic policy version used for this record. |
| `plan_fingerprint` | yes | Plan fingerprint that authorized this child. |
| `policy_fingerprint` | yes | Policy fingerprint that governed the child. |
| `authorization_fingerprint` | conditional | Required when approval or override gates apply. |
| `agent_federation_fingerprint` | yes | Fingerprint over registration, scope, ownership, budget, and provider-session inputs. |
| `classification` | yes | `0742` data classification for the most sensitive field. |
| `side_effect_class` | yes | Maximum `0801` side-effect class delegated to this child. |
| `terminal_error_code` | no | Typed refusal or terminal error. |
| `gap_reasons` | yes | Ordered reasons for unknown, unavailable, stale, partial, ambiguous, or refused federation evidence. |

### Typed Errors

Implementations must reuse `0801`, `0803`, `0804`, and `0646` typed errors
where they apply and add the following stable codes for this phase:

| Error | Required trigger |
| --- | --- |
| `ErrAgentRegistrationRequired` | Provider-native child launch is attempted without an active AgentRegistration. |
| `ErrAgentRegistrationConflict` | A stable child identity replays with different canonical registration bytes or mismatched parent/task/attempt references. |
| `ErrUnsupportedNativeSubAgent` | The selected provider lacks fresh `nested_subagents: true` evidence. |
| `ErrScopeWidening` | A child requests any scope dimension broader than the effective parent grant. |
| `ErrScopeUnknown` | Scope comparison cannot prove equality or narrowing because a dimension is unknown, stale, malformed, unsupported, or unclassified. |
| `ErrCredentialScopeDenied` | A child requests credential material, credential values, auth-file bytes, environment values, cookies, tokens, or private keys. |
| `ErrCommandScopeDenied` | A child requests a command outside the parent allowlist, a shell-interpolated command, or a stronger command side effect. |
| `ErrNetworkScopeDenied` | A child requests undeclared, unapproved, or broader network access. |
| `ErrOneWriterConflict` | A requested ownership lock conflicts with an active lock. |
| `ErrOwnershipRequired` | A write, provider receipt update, task state update, or side effect is attempted without the required ownership lock. |
| `ErrOwnershipStale` | A completion or write uses a stale lock generation, expired lease, or non-owner identity. |
| `ErrChildBudgetReservationRequired` | Provider-native child launch is attempted without a live budget reservation. |
| `ErrChildBudgetExceeded` | Reserve, renew, spend, or completion reconciliation would exceed the child reservation or ancestor hard budget. |
| `ErrChildCancellationRequired` | Policy requires cancellation support but the provider route lacks fresh cancellation evidence. |
| `ErrChildCancellationAmbiguous` | Cancellation cannot prove whether the provider-native child stopped before external side effects. |
| `ErrChildRecoveryAmbiguous` | Recovery cannot prove launch, receipt, ownership, or side-effect state. |
| `ErrAgentFingerprintMismatch` | Persisted registration, scope, ownership, budget, or provider-session inputs do not reproduce the stored fingerprint. |

These errors are refusal semantics. They must not become hidden fallback,
unchecked launch, silent scope clipping, synthetic approval, or guessed
completion.

## Agent Registration

Implementation issue #738 owns the first code implementation of this section.

### AgentRegistration

Schema version: `loopcoder.agent_registration.v1`.

AgentRegistration is required before any provider-native child starts. A
scheduler may create a planned registration before provider launch, but it must
not invoke the provider until the registration is active, the child run claim
is owned, and the budget and ownership checks are committed.

| Field | Required | Meaning |
| --- | --- | --- |
| `child_agent_id` | yes | Stable child agent identity. |
| `parent_agent_id` | no | Parent agent when the parent itself is a registered child; null for a root parent run. |
| `parent_run_id` | yes | Parent run from `0646`. |
| `child_run_id` / `run_id` | yes | Child run from `0646`; both names may render for compatibility, but JSON canonical form uses `run_id`. |
| `root_run_id` | yes | Root run from `0646`. |
| `depth` | yes | Child depth from `0646`; must equal `runs.depth`. |
| `plan_id` | yes | `0646` child plan ID. |
| `child_key` | yes | Stable item key from the child plan. |
| `task_id` | yes | `0801` task reference. |
| `attempt_id` | yes | `0801` attempt reference. |
| `adapter_id` | yes | Provider key from `0802`. |
| `provider_installation_id` | conditional | Required when inventory has resolved the local provider installation. |
| `account_profile_id` | conditional | Required when routing selected or pinned an account/profile. |
| `model_capability_id` | conditional | Required when routing selected a model capability. |
| `routing_decision_id` | conditional | Required when `0804` routing selected the provider route. |
| `provider_session_ref` | no | Bounded local-diagnostic provider session reference or receipt handle, never credential material. |
| `scope_grant_id` | yes | AgentScopeGrant row for this child. |
| `permission` | yes | Reporter permission enum: `read-only`, `write`, or `orchestrate`, no stronger than parent. |
| `side_effect_class` | yes | Maximum side-effect class delegated to the child. |
| `budget_binding_ids` | yes | Ordered AgentBudgetBinding rows. |
| `ownership_lock_ids` | yes | Initial locks required before launch. May be empty only for read-only children with no mutable resource. |
| `claim_generation` | yes | Current `0646` run claim generation at launch. |
| `executor_id` | yes | Scheduler/executor that owns the claim at launch. |
| `provider_idempotency_key` | yes | Logical child operation key aligned with `0646`. |
| `provider_receipt` | no | Real provider response, external resource ID, or verifiable local execution record. |
| `cancellation_channel` | yes | Provider-neutral local cancellation handle or `unsupported` refusal state before launch. |
| `expected_outputs` | yes | Bounded output contract from task/routing requirements. |
| `registration_state` | yes | One AgentRegistration lifecycle state. |

`provider_session_ref` is local diagnostic data. It must not include provider
tokens, cookies, auth file paths beyond redacted forms, private URLs, raw
native session transcripts, or host-private UI state.

### Registration Lifecycle

Registration states are:

| State | Meaning |
| --- | --- |
| `planned` | Child plan and child run exist, but launch prerequisites are not all committed. |
| `registered` | Scope, budget, ownership, provider route, fingerprints, and claim references are persisted. |
| `launching` | The claim owner is starting the provider-native child. |
| `running` | Provider-native child is executing and heartbeat renewal is active. |
| `cancelling` | Cancellation has been requested and cleanup is in progress. |
| `succeeded` | Child terminal success is durably persisted with fenced claim ownership. |
| `failed` | Child terminal failure is durably persisted with fenced claim ownership. |
| `cancelled` | Cancellation is durably persisted and side-effect ambiguity is resolved or represented. |
| `needs-human` | Launch, cancellation, completion, ownership, budget, receipt, or side-effect state is ambiguous. |
| `superseded` | A newer recovery generation owns continuation and this row is retained for audit. |

Registration transition table:

| Current | register | launch | heartbeat | cancel | complete success | complete failure | recover takeover | supersede | invalidate |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `planned` | `registered` | `ErrAgentRegistrationRequired` | `ErrInvalidTransition` | `cancelled` before launch | `ErrInvalidTransition` | `failed` only for pre-launch refusal | `planned` | `superseded` | `needs-human` |
| `registered` | `registered` | `launching` | `ErrInvalidTransition` | `cancelled` before launch | `ErrInvalidTransition` | `failed` | `registered` only if claim not launched and lease expired | `superseded` | `needs-human` |
| `launching` | `ErrInvalidTransition` | `launching` | `running` | `cancelling` | `ErrInvalidTransition` | `failed` | `needs-human` after expired lease | `ErrInvalidTransition` | `needs-human` |
| `running` | `ErrInvalidTransition` | `ErrInvalidTransition` | `running` | `cancelling` | `succeeded` | `failed` | `needs-human` after expired lease | `ErrInvalidTransition` | `needs-human` |
| `cancelling` | `ErrInvalidTransition` | `ErrInvalidTransition` | `cancelling` | `cancelling` | `ErrInvalidTransition` | `cancelled` when cleanup fails closed | `needs-human` when stop cannot be proven | `ErrInvalidTransition` | `needs-human` |
| `succeeded` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `succeeded` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` |
| `failed` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `failed` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` |
| `cancelled` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `cancelled` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` |
| `needs-human` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `needs-human` |
| `superseded` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `superseded` | `ErrTerminalState` |

Every transition that launches, heartbeats, cancels, or completes must also
validate the `0646` claim owner and generation. A stale claim or stale lock
cannot advance the registration.

### Bridge Contract

A provider-native bridge must satisfy these steps in order:

1. Read the accepted `0646` child plan item.
2. Validate provider eligibility from fresh `0802` inventory and `0804`
   routing records.
3. Validate monotonic scope inheritance and canonicalize the AgentScopeGrant.
4. Reserve or attach the exact `0803` child budget reservation.
5. Acquire required one-writer ownership locks.
6. Acquire or take over the `0646` execution claim in an eligible phase.
7. Persist AgentRegistration with plan, policy, routing, scope, budget, claim,
   provider, and ownership references.
8. Start the provider-native child only after the transaction commits.
9. Renew heartbeat and budget reservation leases while executing.
10. Persist provider receipt, usage, ownership releases, child status, and
    parent edge status through fenced completion.

If any step fails, later steps must not run and no provider-native child may
start.

## Scope Grants

Implementation issue #740 owns the first code implementation of this section.

### Canonical Scope Dimensions

AgentScopeGrant schema version: `loopcoder.agent_scope_grant.v1`.

| Dimension | Required field | Values and constraints |
| --- | --- | --- |
| Read scope | `read_scope` | Ordered path/resource set the child may inspect. Empty means no reads. |
| Write scope | `write_scope` | Ordered path/resource set the child may mutate. Empty means read-only or no mutation. |
| Path scope | `path_scope` | Project-relative canonical paths, glob subsets, or named resources after `0801` path canonicalization. `..`, absolute paths, symlink escapes, drive ambiguity, and UNC escapes are invalid. |
| Repository scope | `repository_scope` | Exact `project_id`, normalized remote identity, and allowed repo-relative roots. Cross-project references are invalid. |
| Worktree scope | `worktree_scope` | Exact worktree identity, root, and scratch roots. Child cannot add another worktree unless parent has orchestrate permission and explicit scope. |
| Command scope | `command_scope` | Fixed command allowlist with argv arrays, working directory class, environment-key allowlist, timeout, output cap, and side-effect class. Shell interpolation is not a valid scope. |
| Network scope | `network_scope` | `none`, or provider/purpose/endpoint-class/freshness/fingerprint grants. Network is denied unless explicitly granted. |
| Credential scope | `credential_scope` | Always `none` for credential material. Secret references may be passed only as named non-secret references when allowed by `0742`; values are forbidden. |
| Side-effect scope | `side_effect_scope` | Maximum `0801` side-effect class and explicit external/GitHub/provider targets. Child may only request same or lower class. |
| Approval scope | `approval_scope` | Exact approval or override IDs and authorization fingerprint when policy requires approval. |

The grant is evaluated as a conjunction. A child operation is legal only when
every dimension permits it and the child owns any required writer lock.

### Permission Inheritance

Permission levels are ordered:

```text
read-only < write < orchestrate
```

| Parent permission | Child `read-only` | Child `write` | Child `orchestrate` |
| --- | --- | --- | --- |
| `read-only` | allowed if scope is equal or narrower | denied: `ErrScopeWidening` | denied: `ErrScopeWidening` |
| `write` | allowed if scope is equal or narrower | allowed if write scope is equal or narrower | denied: `ErrScopeWidening` |
| `orchestrate` | allowed if scope is equal or narrower | allowed if write scope is equal or narrower | allowed only if child-plan creation, max-depth, max-concurrency, budget, and scope are all equal or narrower |

### Scope Inheritance Legality Matrix

The following matrix is exhaustive for each canonical dimension. The same
relation vocabulary applies independently to read, write, path, repository,
worktree, command, network, credential, side-effect, and approval scope.

| Parent effective scope | Child request: empty / no authority | Child request: exact subset | Child request: equal | Child request: disjoint | Child request: superset | Child request: unknown / stale / unclassified |
| --- | --- | --- | --- | --- | --- | --- |
| Empty / no authority | allowed | denied | allowed only when both are empty | denied | denied | denied |
| Bounded exact set | allowed | allowed | allowed | denied | denied | denied |
| Bounded glob or prefix set | allowed | allowed only after canonical expansion or proof of subset | allowed | denied | denied | denied |
| Whole project root grant | allowed | allowed | allowed | denied outside project | denied outside project | denied |
| Machine-local non-project grant | allowed | allowed only when parent explicitly contains the same local root and classification permits it | allowed | denied | denied | denied |
| Network grant | allowed | allowed only for same provider, purpose, endpoint class, freshness window, and fingerprint subset | allowed | denied | denied | denied |
| Side-effect grant | allowed | allowed only for same or lower ordered side-effect class and same target subset | allowed | denied | denied | denied |
| Credential material grant | allowed only because credential material grant must be empty | denied | denied | denied | denied | denied |

If the relation cannot be proven, the result is denied with `ErrScopeUnknown`.
Implementations must not silently narrow an illegal child request and continue,
because that would change the approved plan and fingerprint without authority.

### Dimension-Specific Legality

| Dimension | Equal or narrower child request | Always denied child request | Required error |
| --- | --- | --- | --- |
| `read_scope` | Same or subset of parent readable paths/resources, after canonical path validation. | Reading outside parent, following symlink outside root, reading secret material, or reading another project. | `ErrScopeWidening`, `ErrCredentialScopeDenied`, or `ErrCrossProjectReference` |
| `write_scope` | Same or subset of parent writable paths/resources, plus active ownership lock. | Writing outside parent, writing unbounded `**`, writing without lock, writing another project, or writing protected local-only state without explicit resource lock. | `ErrScopeWidening` or `ErrOwnershipRequired` |
| `path_scope` | Same or subset of canonical project-relative paths. | `..`, absolute paths, UNC or drive escapes, ambiguous symlinks, nested repo escapes, or unclassified paths. | `ErrInvalidRecord` or `ErrScopeWidening` |
| `repository_scope` | Same `project_id` and same normalized remote, with root subset. | Cross-project, different remote, path-only identity collision, or unproven alias. | `ErrCrossProjectReference` |
| `worktree_scope` | Same worktree or explicitly declared child scratch root under parent-owned root. | Creating or mutating another worktree, branch checkout, or scratch root outside parent. | `ErrScopeWidening` |
| `command_scope` | Same command ID with argv, env keys, cwd class, timeout, output cap, and side-effect class equal or stricter. | Shell interpolation, broader env, longer timeout when capped, larger output cap when capped, broader cwd, undeclared command, or stronger side effect. | `ErrCommandScopeDenied` |
| `network_scope` | Same provider/purpose/endpoint class/freshness/fingerprint subset. | Network when parent has `none`, broader provider or purpose, credential-bearing URL, private UI scraping, or undeclared network behavior. | `ErrNetworkScopeDenied` |
| `credential_scope` | `none`, or secret-reference names only when parent explicitly allowed the non-secret reference. | Credential values, auth-file bytes, cookies, tokens, private keys, parsed credential objects, or environment variable values. | `ErrCredentialScopeDenied` |
| `side_effect_scope` | Same or lower ordered `0801` side-effect class and same target subset. | Stronger side-effect class, new external target, new GitHub target, provider launch not in plan, or approval-gated side effect without fresh approval. | `ErrSideEffectClassExceeded` or `ErrScopeWidening` |
| `approval_scope` | Same authorization fingerprint and same or narrower approved scope. | Stale approval, missing override, changed plan/policy/input/federation fingerprint, or broader approved scope. | `ErrStaleApproval`, `ErrApprovalRequired`, or `ErrOverrideRequired` |

### Side-Effect Class Inheritance

The ordered classes are the `0801` classes:

```text
none < local-read < local-write < repo-write < git-remote-write < github-write < provider-launch < external-write
```

| Parent maximum | Child may request | Child must be denied |
| --- | --- | --- |
| `none` | `none` | Any stronger class. |
| `local-read` | `none`, `local-read` | `local-write` or stronger. |
| `local-write` | `none`, `local-read`, `local-write` | `repo-write` or stronger. |
| `repo-write` | Up to `repo-write` inside approved path and writer locks. | `git-remote-write`, `github-write`, `provider-launch`, or `external-write` unless parent also has them. |
| `git-remote-write` | Up to `git-remote-write` inside approved refs. | `github-write`, `provider-launch`, or `external-write` unless parent also has them. |
| `github-write` | Up to `github-write` inside approved issue/PR/comment/check targets. | `provider-launch` or `external-write` unless parent also has them. |
| `provider-launch` | Up to `provider-launch` for selected provider route and child plan. | `external-write` unless parent also has it. |
| `external-write` | Up to `external-write` only for exact approved external target subset. | Any new external target or broader operation. |

Provider-native sub-agent launch itself is `provider-launch`. If the child may
write repository files through the provider session, its effective maximum is
both `provider-launch` and `repo-write`, and both policy checks must pass.

## One-Writer Isolation

One-writer isolation applies to any mutable resource. It is not limited to
files. A write-capable child must own all required locks before launch or
before a later scoped write is authorized.

### Lockable Resource Kinds

| Resource kind | Resource key | Writer lock required for |
| --- | --- | --- |
| `repo-path` | `project_id` plus canonical path or path prefix | Creating, editing, deleting, moving, formatting, or generating files. |
| `worktree` | Worktree identity and root | Changing checkout state, deleting worktree, or mutating shared scratch roots. |
| `branch-ref` | Normalized remote/ref | Pushing, force-updating, tagging, or deleting refs. |
| `github-issue` | Repository identity plus issue number | Editing title/body/labels/assignees/state or posting authoritative comments. |
| `github-pr` | Repository identity plus PR number | Editing PR metadata, body, labels, checks, reviews, or merge state. |
| `github-comment-thread` | Issue/PR/comment/thread identity | Creating, editing, resolving, or deleting a comment/thread. |
| `runtime-run` | `run_id` | Mutating run lifecycle, terminal status, or run events. |
| `runtime-task` | `task_id` | Mutating task lifecycle, active attempt, or task outcome. |
| `runtime-attempt` | `attempt_id` | Mutating attempt lifecycle, claim generation, or attempt outcome. |
| `provider-receipt` | `run_id` plus provider route | Writing provider receipt, provider session reference, or completion proof. |
| `budget-reservation` | `budget_reservation_id` | Renewing, committing, releasing, expiring, or cancelling reservation capacity. |
| `external-target` | Provider/action/endpoint/resource tuple | External side effect where LoopCoder can name the target. |

Read-only operations do not need writer locks, but they still need read scope
and classification checks. A read-only operation must not mutate local caches,
provider state, files, GitHub, or external targets through a hidden side
effect.

### One-Writer Conflict Matrix

This matrix is exhaustive for lock conflict decisions. "Conflict" means the
second lock request must be denied with `ErrOneWriterConflict` while the first
lock is active. "Allowed" means the relation alone does not conflict; all other
scope, budget, claim, and policy checks still apply.

| Existing active lock | Requested lock on same resource kind | Requested lock on ancestor | Requested lock on descendant | Requested lock on overlapping range/set | Requested lock on disjoint resource | Requested read-only access |
| --- | --- | --- | --- | --- | --- | --- |
| `repo-path` exact file | conflict | conflict | conflict when descendant exists by path relation | conflict | allowed | allowed if read scope permits |
| `repo-path` directory/prefix | conflict for same path | conflict | conflict | conflict | allowed | allowed if read scope permits |
| `worktree` | conflict | conflict | conflict | conflict | allowed only for different worktree | allowed if read scope permits |
| `branch-ref` | conflict | conflict for wildcard/ref namespace | conflict | conflict | allowed for different ref | allowed if read scope permits |
| `github-issue` | conflict | conflict for issue set lock | conflict | conflict | allowed for different issue | allowed if read scope permits |
| `github-pr` | conflict | conflict for PR set lock | conflict | conflict | allowed for different PR | allowed if read scope permits |
| `github-comment-thread` | conflict | conflict for parent issue/PR comment set | conflict | conflict | allowed for different thread | allowed if read scope permits |
| `runtime-run` | conflict | conflict for root run tree lock | conflict for child run lock when parent run status may change | conflict | allowed for unrelated run tree | allowed if local-only output rules permit |
| `runtime-task` | conflict | conflict for DeliveryRun task-set lock | conflict for attempt/task child lock when task state may change | conflict | allowed for unrelated task | allowed if local-only output rules permit |
| `runtime-attempt` | conflict | conflict for task attempt-set lock | conflict | conflict | allowed for unrelated attempt | allowed if local-only output rules permit |
| `provider-receipt` | conflict | conflict for run/provider receipt set | conflict | conflict | allowed for unrelated provider route | allowed only as redacted summary |
| `budget-reservation` | conflict | conflict for parent reservation aggregate when same transaction cannot prove atomic update | conflict | conflict | allowed when atomic aggregate check covers both | allowed if local-only output rules permit |
| `external-target` | conflict | conflict for target group | conflict | conflict | allowed only when target identity proves disjointness | allowed only when external read is scoped and approved |

For `repo-path`, path overlap uses canonical project-relative paths after
normalization and symlink confinement. A file conflicts with itself, its parent
directories, and any descendant path under a directory lock. Two paths are
disjoint only when canonical comparison proves neither is equal, ancestor,
descendant, or glob-overlapping.

For resources whose target identity is unknown, stale, or unclassifiable, the
lock manager must assume overlap and deny the second writer with
`ErrScopeUnknown` or `ErrOneWriterConflict`.

### Ownership Lock Lifecycle

AgentOwnershipLock schema version: `loopcoder.agent_ownership_lock.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `agent_ownership_lock_id` | yes | Stable lock identity. |
| `child_agent_id` | yes | Lock owner. |
| `run_id` | yes | Child run owning the lock. |
| `claim_generation` | yes | `0646` claim generation tied to lock ownership. |
| `lock_generation` | yes | Monotonic generation for lock renewal and stale-write rejection. |
| `resource_kind` | yes | One lockable resource kind. |
| `resource_key` | yes | Canonical resource key. |
| `lock_mode` | yes | `write` initially. Future modes must be specified before use. |
| `state` | yes | `requested`, `held`, `releasing`, `released`, `expired`, `conflict`, or `needs-human`. |
| `lease_expires_at` | yes | Expiry for held locks. |
| `heartbeat_at` | yes | Last renewal by current owner. |
| `conflicts_with` | yes | Ordered conflicting lock IDs when state is `conflict`; empty otherwise. |

Lock renewal and release must be fenced by `child_agent_id`, `run_id`,
`claim_generation`, and `lock_generation`. Terminal child completion must
release or mark all held locks in the same transaction that persists terminal
state, unless release ambiguity requires `needs-human`.

## Budgeted Nested Run, Cancel, And Recover

Implementation issue #739 owns the first code implementation of this section.

### Budget Binding

AgentBudgetBinding schema version: `loopcoder.agent_budget_binding.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `agent_budget_binding_id` | yes | Stable binding identity. |
| `child_agent_id` | yes | Child that owns this reservation. |
| `budget_policy_id` | yes | `0803` BudgetPolicy row. |
| `budget_reservation_id` | yes | `0803` BudgetReservation row. |
| `reservation_scope` | yes | Must be `sub-agent` or a narrower provider/account/model scope tied to this child. |
| `reserved_quantities` | yes | Ordered quantities reserved for the child. |
| `ancestor_budget_refs` | yes | Machine, project, DeliveryRun, task, worker, provider, account, or model budgets checked atomically. |
| `reservation_state` | yes | Mirrored `0803` reservation state for JSON convenience; `0803` row is authoritative. |

Budget binding rules:

- A provider-native child cannot launch without at least one active
  BudgetReservation for every configured hard budget dimension it may consume.
- A child cannot reserve or spend more than its reservation.
- A child reservation cannot exceed any ancestor hard ceiling after active
  reservations and committed usage are included.
- Budget reserve, renew, commit, release, cancel, and expire actions reuse the
  `0803` reservation lifecycle and atomic accounting rules.
- Budget exhaustion before launch refuses launch with `ErrChildBudgetExceeded`
  or `ErrBudgetExhausted`.
- Budget exhaustion during execution requests cancellation when policy requires
  cancellation and marks the child `needs-human` if side-effect state is
  ambiguous.
- A child cannot create a grandchild unless it has `orchestrate` permission and
  explicitly reserves a budget subset for that grandchild.

### Scheduler Integration

The scheduler must evaluate the following gates in this order for every
provider-native child:

| Gate | Required inputs | Refusal |
| --- | --- | --- |
| Child plan accepted | `0646` child plan and run edge | `ErrMissingReference` |
| Depth and concurrency | `0646` `max_depth`, `max_concurrency`, active child counts | `ErrGraphBoundExceeded` or `ErrPolicyDenied` |
| Provider eligibility | `0802` inventory, runtime capability matrix, `0804` route | `ErrUnsupportedNativeSubAgent` or `ErrNoEligibleCandidate` |
| Scope inheritance | Parent effective AgentScopeGrant or root task scope | `ErrScopeWidening` or `ErrScopeUnknown` |
| Approval freshness | `0801` approval/override and authorization fingerprint | `ErrApprovalRequired`, `ErrOverrideRequired`, or `ErrStaleApproval` |
| Budget reservation | `0803` policies, reservations, aggregates | `ErrChildBudgetReservationRequired` or `ErrChildBudgetExceeded` |
| One-writer locks | Lock manager and resource keys | `ErrOneWriterConflict` or `ErrOwnershipRequired` |
| Execution claim | `0646` `run_claims` owner/generation/phase | `ErrClaimRequired`, `ErrClaimConflict`, or `ErrStaleClaim` |
| Cancellation capability | `0802` capability and `0804` task requirement | `ErrChildCancellationRequired` |
| Registration | AgentRegistration and fingerprint | `ErrAgentRegistrationRequired` or `ErrAgentFingerprintMismatch` |

No gate may be skipped because a provider reports that it can manage its own
children. Provider-side policy is advisory at most and cannot weaken
LoopCoder policy.

### Cancellation

Cancellation is a LoopCoder-owned state transition:

- Parent cancellation cascades to registered children whose policy marks them
  cancellable.
- Child-specific cancellation may be requested by scheduler policy, budget
  exhaustion, circuit breaker, user action, parent failure, or dependency
  invalidation.
- Cancellation requires the current claim owner or an allowed recovery owner.
  Another scheduler observing an active owner records an observation and does
  not overwrite the child.
- During provider execution, cancellation uses a bounded cleanup context that
  is independent of the cancelled caller context, matching `0646`.
- If provider-local process cancellation succeeds before external side effects
  and terminal persistence succeeds, the child becomes `cancelled`.
- If LoopCoder cannot prove whether the provider-native child launched, wrote,
  spent budget, or produced external side effects, the child becomes
  `needs-human` with `ErrChildCancellationAmbiguous`.
- A provider receipt or local execution record may support cancellation
  recovery, but a stored idempotency key alone is not proof.

### Crash Recovery

Recovery reuses `0646` lease and fencing semantics:

| Crash point | Required recovery behavior |
| --- | --- |
| Before AgentRegistration commit | Replay may create the same registration if canonical bytes match and no provider launched. |
| After registration before provider launch | If claim phase proves pre-launch and lease expired, recovery may take over with a new generation. |
| During `launching` | Expired lease is ambiguous; recovery fails closed to `needs-human` unless a provider receipt proves safe continuation. |
| During `running` with active heartbeat | Observers must not duplicate launch; they may report active owner and lease. |
| During `running` after lease expiry | Recovery fails closed to `needs-human` unless provider receipt, idempotency evidence, and ownership state prove exactly what happened. |
| After provider receipt before terminal persistence | Recovery may persist terminal state only if receipt, claim generation, ownership locks, and budget reconciliation are still valid. |
| After external side effect without receipt | Recovery becomes `needs-human`; LoopCoder does not claim exactly-once external effects. |
| After stale child completion | Completion is rejected with `ErrStaleClaim` or `ErrOwnershipStale`; parent terminal state must not advance from stale evidence. |

Recovery must reconcile budget reservations and ownership locks before parent
aggregation changes. A parent may become terminal only after every
aggregation-participating child terminal state, budget outcome, and lock
release has persisted successfully or the parent is marked `needs-human`.

## Storage

Records live in the machine-local SQLite store under the global project
storage model from
[`0639-global-data-layout-project-identity.md`](0639-global-data-layout-project-identity.md).
The initial v0.8 schema additions are logical tables equivalent to:

- `agent_registrations`;
- `agent_scope_grants`;
- `agent_ownership_locks`;
- `agent_budget_bindings`;
- `agent_events` for tamper-evident authority events when registration, scope,
  lock, budget, cancellation, or recovery records affect DeliveryRun inputs or
  task launch.

The implementation must also reuse the existing `runs`, `run_edges`,
`child_plans`, and `run_claims` tables from `0646` and must not create a
parallel parent-child graph.

Minimum logical schema:

```sql
CREATE TABLE agent_scope_grants (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  delivery_run_id TEXT NOT NULL,
  child_agent_id TEXT NOT NULL,
  schema_version TEXT NOT NULL,
  record_version INTEGER NOT NULL,
  scope_json TEXT NOT NULL,
  permission TEXT NOT NULL,
  side_effect_class TEXT NOT NULL,
  policy_version TEXT NOT NULL,
  policy_fingerprint TEXT NOT NULL,
  plan_fingerprint TEXT NOT NULL,
  authorization_fingerprint TEXT NOT NULL DEFAULT '',
  agent_federation_fingerprint TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  terminal_error_code TEXT NOT NULL DEFAULT ''
);

CREATE TABLE agent_registrations (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  delivery_run_id TEXT NOT NULL,
  root_run_id TEXT NOT NULL,
  parent_run_id TEXT NOT NULL,
  child_run_id TEXT NOT NULL,
  parent_agent_id TEXT NOT NULL DEFAULT '',
  task_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  plan_id TEXT NOT NULL,
  child_key TEXT NOT NULL,
  adapter_id TEXT NOT NULL,
  provider_installation_id TEXT NOT NULL DEFAULT '',
  account_profile_id TEXT NOT NULL DEFAULT '',
  model_capability_id TEXT NOT NULL DEFAULT '',
  routing_decision_id TEXT NOT NULL DEFAULT '',
  provider_session_ref TEXT NOT NULL DEFAULT '',
  scope_grant_id TEXT NOT NULL,
  permission TEXT NOT NULL,
  side_effect_class TEXT NOT NULL,
  claim_generation INTEGER NOT NULL,
  executor_id TEXT NOT NULL,
  provider_idempotency_key TEXT NOT NULL,
  provider_receipt TEXT NOT NULL DEFAULT '',
  cancellation_channel TEXT NOT NULL,
  expected_outputs_json TEXT NOT NULL,
  registration_state TEXT NOT NULL,
  policy_version TEXT NOT NULL,
  plan_fingerprint TEXT NOT NULL,
  policy_fingerprint TEXT NOT NULL,
  authorization_fingerprint TEXT NOT NULL DEFAULT '',
  agent_federation_fingerprint TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  terminal_error_code TEXT NOT NULL DEFAULT '',
  UNIQUE(project_id, delivery_run_id, parent_run_id, child_key),
  UNIQUE(child_run_id),
  FOREIGN KEY(scope_grant_id) REFERENCES agent_scope_grants(id)
);

CREATE TABLE agent_ownership_locks (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  delivery_run_id TEXT NOT NULL,
  child_agent_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  claim_generation INTEGER NOT NULL,
  lock_generation INTEGER NOT NULL,
  resource_kind TEXT NOT NULL,
  resource_key TEXT NOT NULL,
  lock_mode TEXT NOT NULL,
  state TEXT NOT NULL,
  lease_expires_at TEXT NOT NULL,
  heartbeat_at TEXT NOT NULL,
  conflicts_with_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(project_id, resource_kind, resource_key, state)
);

CREATE TABLE agent_budget_bindings (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  delivery_run_id TEXT NOT NULL,
  child_agent_id TEXT NOT NULL,
  budget_policy_id TEXT NOT NULL,
  budget_reservation_id TEXT NOT NULL,
  reservation_scope TEXT NOT NULL,
  reserved_quantities_json TEXT NOT NULL,
  ancestor_budget_refs_json TEXT NOT NULL,
  reservation_state TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(child_agent_id, budget_policy_id, budget_reservation_id)
);

CREATE TABLE agent_events (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  delivery_run_id TEXT NOT NULL,
  child_agent_id TEXT NOT NULL,
  event_kind TEXT NOT NULL,
  event_hash TEXT NOT NULL,
  previous_event_hash TEXT NOT NULL DEFAULT '',
  payload_hash TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  created_at TEXT NOT NULL
);
```

SQLite implementations may use partial indexes rather than the simplified
`UNIQUE(project_id, resource_kind, resource_key, state)` shown above, but they
must enforce the one-writer conflict matrix atomically. An implementation must
not rely on application-side preflight alone for conflict prevention.

Storage invariants:

- `agent_registrations.child_run_id` must reference a `0646` `runs.id`.
- `agent_registrations.parent_run_id` must match the child run's
  `parent_run_id` and a `0646` `run_edges` row.
- `agent_registrations.scope_grant_id` must reference the grant for the same
  child agent.
- `agent_budget_bindings.budget_reservation_id` must reference an active
  `0803` reservation at launch.
- `agent_ownership_locks.child_agent_id` must reference an existing
  registration before a lock can become `held`.
- `claim_generation` in registration and locks must match the current `0646`
  claim generation for launch, heartbeat, write, and completion.
- Provider receipt writes require both the execution claim and the
  `provider-receipt` ownership lock.
- Cross-project, cross-DeliveryRun, and mismatched root-run references fail in
  the same transaction with no partial rows.
- Agent event hashes follow the tamper-evident provenance rules in `0742`.

## JSON Exposure

### Doctor JSON

`loopcoder doctor --format json` must expose federation capability and policy
diagnostics as an additive root object named `agent_federation`. Existing
fields remain valid.

```json
{
  "agent_federation": {
    "schema_version": "loopcoder.agent_federation_json.v1",
    "generated_at": "2026-07-12T00:00:00Z",
    "agent_federation_policy_version": "0805.agent_federation.v1",
    "providers": [
      {
        "adapter_id": "claude",
        "nested_subagents": true,
        "capability_confidence": "exact",
        "support": "supported",
        "gap_reasons": []
      }
    ],
    "scope_policy": {
      "monotonic_inheritance": true,
      "credential_material_allowed": false,
      "one_writer_required": true
    },
    "budget_policy": {
      "hierarchical_budgets_required": true,
      "child_reservation_required": true
    },
    "gap_reasons": []
  }
}
```

Doctor JSON must distinguish:

- provider supports nested sub-agents versus unsupported;
- unknown, unavailable, or stale capability evidence;
- missing cancellation support when policy requires it;
- missing or invalid budget policy;
- disabled one-writer enforcement;
- schema version unknown to the binary;
- credential-material requests, which are invalid rather than warnings.

Doctor JSON must not include provider-native session transcripts, raw provider
output, credentials, unredacted local paths, local-only reporter blocks, or
authorization secrets.

### Status JSON

`loopcoder status --format json` must expose agent-tree records only when they
affect a DeliveryRun, Task, attempt, child run, cancellation, or recovery.

```json
{
  "agent_tree": {
    "schema_version": "loopcoder.agent_tree.v1",
    "root_run_id": "run_...",
    "agent_federation_fingerprint": "sha256:...",
    "registrations": [
      {
        "child_agent_id": "agent_...",
        "parent_agent_id": null,
        "parent_run_id": "run_parent",
        "run_id": "run_child",
        "task_id": "task_...",
        "attempt_id": "att_...",
        "adapter_id": "claude",
        "registration_state": "running",
        "scope_grant_id": "ascope_...",
        "budget_reservation_ids": ["bres_..."],
        "ownership_lock_ids": ["alock_..."],
        "claim_generation": 2,
        "gap_reasons": []
      }
    ],
    "blocked": [],
    "needs_human": []
  }
}
```

Status JSON may include bounded redacted summaries of scope dimensions, lock
conflicts, budget refusal, cancellation ambiguity, and recovery ambiguity. It
must not duplicate full raw inventory, provider output, native transcripts,
local logs, credential-bearing diagnostics, or local-only reporter blocks by
default.

## Failure Honesty Rules

These rules are normative and testable:

- Provider-native child launch requires durable registration before launch.
- A stable child ID is not enough to launch; the scheduler must own the active
  `0646` execution claim.
- Scope inheritance is monotonic. A child may narrow but never widen parent
  scope, permission, network, command, repository, worktree, credential, or
  side-effect authority.
- Credential material is never a legal child scope.
- One-writer isolation is mandatory for every mutable resource. Unknown
  overlap fails closed.
- Budget reservations are hard gates. A provider-native child cannot exceed
  its reservation or ancestor budgets.
- Provider-native child output is untrusted data and cannot alter policy,
  approvals, routing, budgets, ownership, or final acceptance.
- Cancellation and recovery are conservative. Ambiguous launch, write, budget,
  receipt, or side-effect state becomes `needs-human`.
- Parent aggregation cannot hide child absence, child ambiguity, stale
  completion, budget refusal, or lock conflict.
- Provider capability evidence must be fresh enough for policy. Unsupported,
  unknown, unavailable, or stale nested-sub-agent evidence refuses launch.

## Implementation Acceptance Mapping

### #738 Native Sub-Agent Bridge And Durable Registration

Issue #738 is complete only when its code and tests implement the sections
"Provider Eligibility", "Execution Substrate", "Agent Registration", "Bridge
Contract", storage rows for registration/scope/budget/ownership references,
and doctor/status JSON references:

- provider-native launch refuses without an AgentRegistration committed before
  launch;
- registrations include stable child ID, parent ID, task and attempt refs,
  scope, permission, budget refs, ownership refs, provider session reference,
  provider route refs, plan fingerprint, policy fingerprint, authorization
  fingerprint when required, and provenance;
- child launch reuses `0646` `runs`, `run_edges`, `child_plans`, and
  `run_claims`; no duplicate graph or claim system exists;
- `claude` is eligible only when current inventory and routing checks pass;
  `codex`, `gemini`, and `antigravity` fail closed for native sub-agents under
  the live matrix;
- idempotent replay returns identical registration records and rejects changed
  canonical bytes with `ErrDuplicateReplay` or
  `ErrAgentRegistrationConflict`;
- provider session references are bounded local diagnostics and never contain
  credential material or raw native transcripts.

### #740 Scope, Permission, Ownership, And One-Writer Enforcement

Issue #740 is complete only when its code and tests implement the sections
"Scope Grants" and "One-Writer Isolation":

- canonical scope dimensions cover read, write, path, repository, worktree,
  command, network, credential, side-effect, and approval scope;
- inheritance validation evaluates the exhaustive legality matrix and fails
  closed on widening, unknown, stale, malformed, or unclassified dimensions;
- permission inheritance enforces `read-only < write < orchestrate`;
- command scope uses fixed argv, bounded cwd/env/timeout/output rules and
  rejects shell interpolation;
- network scope is opt-in by provider, purpose, endpoint class, freshness, and
  fingerprint;
- credential material requests are impossible to authorize and return
  `ErrCredentialScopeDenied`;
- one-writer locks cover repository paths, worktrees, refs, GitHub resources,
  runtime run/task/attempt state, provider receipts, budget reservations, and
  named external targets;
- conflict tests cover same, ancestor, descendant, overlapping, disjoint,
  unknown, stale, and cross-project resources;
- stale lock or stale claim writes fail with `ErrOwnershipStale`,
  `ErrStaleClaim`, or `ErrOwnershipRequired` and do not advance child or parent
  status.

### #739 Dynamic Nested Run, Cancel, Recover Under Budgets

Issue #739 is complete only when its code and tests implement the sections
"Budgeted Nested Run, Cancel, And Recover", "Budget Binding", "Scheduler
Integration", "Cancellation", and "Crash Recovery":

- child launch requires a live `0803` budget reservation for every applicable
  hard budget dimension;
- reserve, renew, commit, release, cancel, expire, and refused reservation
  paths are transactional and idempotent;
- children cannot exceed their reservation or any ancestor machine, project,
  DeliveryRun, task, worker, sub-agent, provider, account, or model budget;
- budget exhaustion before launch refuses launch, and budget exhaustion during
  execution cancels or blocks according to policy without pretending success;
- scheduler gates run in the documented order and no provider-native capability
  can skip them;
- cancellation cascades from parent to child and uses fenced claim ownership
  plus bounded cleanup context;
- crash recovery covers before registration, after registration before launch,
  during launch, during running, after provider receipt, after external side
  effect without receipt, and stale completion;
- ambiguous launch, cancellation, receipt, ownership, budget, or external
  side-effect state fails closed to `needs-human`;
- parent aggregation changes only after child terminal state, budget
  reconciliation, and lock release are durably persisted.

## Relationship To Existing Specs And Docs

- [`0646-nested-sub-agent-plan.md`](0646-nested-sub-agent-plan.md) defines the
  run tree, child plans, run edges, execution claims, claim phases, leases,
  heartbeat renewal, fencing, provider idempotency keys, provider receipts,
  aggregation, and conservative nested recovery. This spec reuses those
  mechanics for provider-native children.
- [`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md) defines
  DeliveryRun, Task, Attempt, Decision, Approval, Override, stable IDs,
  provenance, fingerprints, side-effect classes, typed errors, idempotency, and
  decision ownership boundaries. This spec uses those records as the authority
  substrate for federation.
- [`0802-provider-inventory.md`](0802-provider-inventory.md) defines provider
  inventory, account/profile, model capability, capability freshness, and the
  `nested_subagents` dimension. This spec consumes those facts and refuses
  unsupported or stale evidence.
- [`0803-quota-usage-budget.md`](0803-quota-usage-budget.md) defines
  hierarchical budgets, budget reservations, usage records, availability, and
  circuit breakers. This spec requires child budget reservations and
  reconciles child execution against those ceilings.
- [`0804-decomposition-routing.md`](0804-decomposition-routing.md) defines
  task requirements, role definitions, routing policy profiles, eligibility,
  fallback, and re-planning. This spec uses its `nested-subagent` role
  eligibility and does not make routing decisions by provider name.
- [`0742-v080-security-threat-model.md`](0742-v080-security-threat-model.md)
  names agent federation as an attack surface and requires child agents to
  stay inside LoopCoder scopes, budgets, claims, approvals, one-writer
  isolation, cancellation, and recovery. This spec makes those controls
  concrete for #738, #740, and #739.
- [`../reference/runtime-capabilities.md`](../reference/runtime-capabilities.md)
  is the live provider and host capability matrix. This spec uses its current
  nested-sub-agent support row and fails closed for unsupported providers.
- Roadmap [#714](https://github.com/jasonhnd/loopcoder/issues/714) owns the
  v0.8.0 delivery order and red-line policy. This spec preserves the decision
  ownership rule that provider-native children never bypass LoopCoder budgets,
  scopes, approvals, routing, or final acceptance.
