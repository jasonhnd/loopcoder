---
id: 803
title: v0.8.0 Phase 3 Quota, Usage, Budget, And Availability
status: draft
date: 2026-07-12
issue: 797
pr: null
supersedes: []
superseded_by: []
---

# v0.8.0 Phase 3 Quota, Usage, Budget, And Availability

This documentation-only spec freezes the phase-3 quota, usage, budget, and
availability contract that implementation issues
[#729](https://github.com/jasonhnd/loopcoder/issues/729),
[#730](https://github.com/jasonhnd/loopcoder/issues/730),
[#731](https://github.com/jasonhnd/loopcoder/issues/731), and
[#732](https://github.com/jasonhnd/loopcoder/issues/732) must implement for the
v0.8.0 quota epic [#717](https://github.com/jasonhnd/loopcoder/issues/717).

This spec follows the shared durable-record conventions from
[`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md): opaque
stable IDs, explicit `schema_version` and `record_version`, actor and host
provenance, UTC timestamps, typed errors, policy fingerprints, idempotency, and
one-transaction mutation rules. It reuses the provider inventory record and
bounded command patterns from
[`0802-provider-inventory.md`](0802-provider-inventory.md), including
credential-blind account references, auth readiness records, machine-local
storage, freshness metadata, and declared network behavior. It also applies the
quota controls from
[`0742-v080-security-threat-model.md`](0742-v080-security-threat-model.md):
quota telemetry is a named attack surface, quota evidence must preserve
`exact`, `estimated`, `unknown`, `unavailable`, and `stale`, and unsupported or
stale evidence must fail closed instead of becoming false capacity.

This document adds no Go code, SQLite migration, CLI behavior, workflow change,
provider integration, routing policy, or runtime control. Per
[`../PROCESS.md`](../PROCESS.md), this spec must merge before the code issues
above implement it.

## Goals

- Define supported quota telemetry sources and explicitly forbid private UI
  scraping, reverse-engineered quota endpoints, credential reuse, and inferred
  exact quota.
- Define immutable QuotaSnapshot records with source, scope, unit, reset
  semantics, freshness, confidence, redacted diagnostics, and disagreement
  handling.
- Define a normalized append-only UsageRecord ledger across providers for
  tokens, requests, wall time, and provider-defined quantities while preserving
  original units and confidence.
- Define conservative estimation and provider reconciliation rules where
  estimates are always `confidence: "estimated"` and never masquerade as exact.
- Define hierarchical atomic budgets and reservation accounting across machine,
  project, DeliveryRun, task, worker, and provider-native sub-agent scopes.
- Define deterministic availability scores and circuit breakers that combine
  inventory, auth readiness, quota, usage, reservations, recent failures, and
  rate-limit evidence without hiding uncertainty.
- Define machine-local storage, doctor/status JSON exposure, retention, and
  implementation acceptance mapping for #729 through #732.

## Non-Goals

- No routing decisions, candidate ranking, fallback ordering, or "pick the most
  quota" behavior. Phase 0804 owns explainable routing.
- No provider-native federation or agent-tree execution semantics. Phase 0805
  owns federation.
- No UX flows, guided setup, login, browser automation, billing-page scraping,
  or private account-page access.
- No provider credential reading, copying, hashing, parsing, migration, or
  serialization.
- No claim that local estimates describe all provider-side usage. Work outside
  LoopCoder remains outside the local ledger until an official provider source
  reconciles it.
- No repository-tracked runtime payloads. Quota, usage, budget, and
  availability state is machine-local.

## Terms

**Quota telemetry source** means an adapter-declared source that reports
provider quota, usage, rate-limit, reset, or capacity data. A source is
supported only when it is official, machine-readable, bounded, classified, and
declared by the adapter or explicitly configured as a local policy overlay.

**QuotaSnapshot** means one immutable capture of quota or rate-limit facts for
one provider/account/profile/model/window scope. A snapshot is evidence, not a
routing decision.

**UsageRecord** means one append-only local ledger event that records estimated
or observed LoopCoder usage, provider-reported usage, reservation movement, or
reconciliation. It is never rewritten to hide prior facts.

**BudgetPolicy** means a versioned local policy ceiling for one scope and
quantity. It may be hard or soft, but soft budgets still require explicit
policy behavior and output.

**BudgetReservation** means an atomic claim on budgeted capacity before a task,
worker, or sub-agent starts. Reservations are committed to observed usage,
released when unused, expired through deterministic recovery, or refused before
side effects.

**AvailabilityScore** means a persisted diagnostic score for one provider,
account/profile, model, and scope. It separates hard eligibility from soft
health. A high score cannot make an ineligible candidate eligible.

**CircuitBreaker** means a persisted state machine for suppressing launches or
telemetry probes after quota exhaustion, rate limits, auth failures, provider
outages, malformed responses, or repeated transient failures.

**Window** means a quota, usage, budget, or rate-limit interval such as fixed
hour, fixed day, fixed week, rolling duration, provider-defined reset time, or
unbounded local budget.

**Quantity** means a typed measurable unit such as input tokens, output tokens,
total tokens, requests, wall milliseconds, concurrent launches, provider-defined
credits, or a local policy unit.

## Shared Representation

The implementation must expose one logical contract across Go, SQLite, and
JSON:

- JSON field names use snake_case, explicit `schema_version` strings, and
  stable enum strings.
- SQLite stores queryable identity, scope, lifecycle, freshness, confidence,
  quantity, and relationship fields in normal columns. Bounded structured
  details may live in `*_json` columns only when they are not needed for joins,
  uniqueness, lifecycle transitions, atomic budget checks, or policy checks.
- SQLite `TEXT` IDs are opaque. Callers must not parse provider names, issue
  numbers, timestamps, account labels, model names, or units from IDs.
- Every record has `schema_version`, `record_version`, timestamps, provenance,
  classification, source, policy version when policy-relevant, and confidence
  where evidence quality matters.
- Project-scoped records carry `project_id`; machine-scoped records set
  `scope: "machine"` and `project_id: null`. Cross-project references fail
  with `ErrCrossProjectReference`.
- All timestamps are UTC RFC3339 strings rendered with `Z`.
- Unknown enum values in persisted records fail closed with
  `ErrUnknownRecordVersion` or `ErrInvalidRecord`, matching 0801.
- Records that affect DeliveryRun authority, task start eligibility, provider
  launch, budget denial, or recovery must contribute their exact record IDs and
  canonical payload hashes to the applicable `input_fingerprint`,
  `policy_fingerprint`, `plan_fingerprint`, or later routing fingerprint.
- Every consequential number must be traceable to a persisted source record or
  explicitly marked `heuristic: true` with the estimator, inputs, timestamp,
  and confidence. A displayed number without source or heuristic provenance is
  invalid.

### ID Scheme

ID prefixes are stable; the bytes after the prefix are opaque:

| Record | ID field | Required form |
| --- | --- | --- |
| QuotaTelemetrySource | `quota_source_id` | `qsrc_<base32-sha256(adapter_id, source_kind, source_key, source_schema_version)>`. |
| QuotaSnapshot | `quota_snapshot_id` | `qsnap_<uuidv7-or-random-128-bit-base32>`; immutable per capture. |
| UsageRecord | `usage_record_id` | `usage_<uuidv7-or-random-128-bit-base32>`; immutable per ledger event. |
| UsageReconciliation | `usage_reconciliation_id` | `urec_<uuidv7-or-random-128-bit-base32>`; immutable per reconciliation event. |
| BudgetPolicy | `budget_policy_id` | `bpol_<base32-sha256(scope_key, quantity_kind, window_kind, policy_version, ordinal)>`. |
| BudgetReservation | `budget_reservation_id` | `bres_<base32-sha256(idempotency_key, budget_policy_id, requester_id)>` plus a collision suffix only when canonical replay bytes differ and fail. |
| AvailabilityObservation | `availability_observation_id` | `avobs_<uuidv7-or-random-128-bit-base32>`; immutable per health or failure observation. |
| AvailabilityScore | `availability_score_id` | `avscore_<uuidv7-or-random-128-bit-base32>`; immutable per calculation. |
| CircuitBreaker | `circuit_breaker_id` | `breaker_<base32-sha256(scope_key, breaker_kind, adapter_id, optional_model_key)>`. |
| Quota usage fingerprint | `quota_usage_fingerprint` | The digest string itself: `sha256:<64-lower-hex>`. |

`scope_key` is the canonical JSON scope object, not a display label. It must
include only stable IDs and redacted hashes, never provider credential material.

### Common Fields

All durable records in this spec carry the following fields unless a record
table explicitly marks one as not applicable:

| Field | Required | Meaning |
| --- | --- | --- |
| `schema_version` | yes | Stable JSON/storage shape string. |
| `record_version` | yes | Optimistic update version for mutable records; immutable snapshots and ledger rows keep `1`. |
| `scope` | yes | `machine`, `project`, `delivery-run`, `task`, `worker`, `sub-agent`, or `provider-scope`. |
| `project_id` | conditional | Required for project and narrower scopes; null for machine-only records. |
| `delivery_run_id` | conditional | Required for DeliveryRun, task, worker, and sub-agent scopes. |
| `task_id` | conditional | Required for task, worker, and sub-agent scopes when tied to a task. |
| `adapter_id` | conditional | Provider adapter key when provider-specific. |
| `provider_installation_id` | no | 0802 ProviderInstallation reference when known. |
| `account_profile_id` | no | 0802 AccountProfile reference when known. |
| `model_capability_id` | no | 0802 ModelCapability reference when known. |
| `created_at` / `updated_at` | yes | UTC persistence timestamps. |
| `captured_at` | yes for observations | UTC time when evidence was captured or calculated. |
| `valid_until` | no | Provider or policy validity horizon when known. |
| `stale_after` | no | Local freshness horizon after which output MUST report stale confidence unless refreshed. |
| `freshness_state` | yes when observable | `fresh`, `stale`, `expired`, or `not-applicable`. |
| `confidence` | yes when observable | Exactly `exact`, `estimated`, `unknown`, `unavailable`, or `stale`. |
| `created_by` / `updated_by` | yes | 0801 actor provenance object. |
| `host` | yes | 0801 host provenance object. |
| `policy_version` | conditional | Required when a record affects eligibility, authority, budget, or output filtering. |
| `side_effect_class` | yes | Maximum side-effect class involved. |
| `classification` | yes | 0742 data classification for the record's most sensitive field. |
| `source` | yes | Structured source descriptor, source record reference, or estimator descriptor. |
| `evidence` | yes | Bounded classified evidence summary; never raw secret material. |
| `gap_reasons` | yes | Ordered typed reasons for unknown, unavailable, stale, partial, estimated, or conflicting facts. Empty when no gap exists. |
| `terminal_error_code` | no | Typed error when the record represents a terminal failed observation or refused operation. |

### Typed Errors

Implementations must reuse 0801 typed errors where they apply and add the
following stable codes for this phase:

| Error | Required trigger |
| --- | --- |
| `ErrQuotaSourceForbidden` | Telemetry attempts to use UI scraping, a reverse-engineered endpoint, credential material, a non-declared source, or an unsafe source kind. |
| `ErrQuotaSourceUnsupported` | The adapter has no supported quota source for the requested provider/account/model/window. |
| `ErrTelemetryNetworkDenied` | A telemetry source may use the network but network permission was not declared and granted for the provider, purpose, and fingerprint. |
| `ErrQuotaConfidenceInsufficient` | Policy requires exact or fresh quota evidence, but the best available confidence is estimated, unknown, unavailable, or stale. |
| `ErrBudgetExhausted` | An atomic reserve or commit would exceed an applicable hard budget ceiling. |
| `ErrBudgetScopeConflict` | A budget operation references mismatched machine, project, DeliveryRun, task, worker, sub-agent, provider, account, or model scope. |
| `ErrReservationExpired` | Commit, release, or renewal uses a reservation beyond its expiry or lease generation. |
| `ErrReservationStateConflict` | A reserve, commit, release, cancel, or expire event is invalid for the current reservation state. |
| `ErrUsageReconciliationConflict` | Provider-reported usage cannot be reconciled with local records without losing prior facts or double counting. |
| `ErrBreakerOpen` | Scheduler or telemetry attempted an operation blocked by an open circuit breaker. |
| `ErrRateLimited` | Supported provider evidence reports rate limiting for the scope and window. |
| `ErrAvailabilityUnknown` | Availability cannot be proven sufficiently for the requested policy because required inputs are unknown, stale, or conflicting. |

These errors are refusal semantics. They must not be converted into silent
degradation, hidden fallback, or unchecked provider launch.

## Freshness And Confidence

Freshness is separate from confidence. Confidence describes evidence quality;
freshness describes whether policy may still use that evidence at the current
time.

| Confidence | Meaning | May satisfy exact policy? |
| --- | --- | --- |
| `exact` | Supported source or deterministic local accounting reports a precise value for its declared scope. | Yes, only while fresh and in scope. |
| `estimated` | Value is derived from local observations, configured overlays, heuristics, or partial provider data. | No. |
| `unknown` | Evidence is missing, unsafe, ambiguous, unsupported for that scope, or not collected. | No. |
| `unavailable` | A supported source proves the fact is not available or the provider explicitly does not expose it. | No. |
| `stale` | Evidence was previously available but is beyond `stale_after`, `valid_until`, or provider reset horizon. | No. |

### Confidence-State Transitions On Staleness

This table is exhaustive for reuse of an existing observation at evaluation
time:

| Previous confidence | Event | Required resulting confidence | Notes |
| --- | --- | --- | --- |
| `exact` | Evaluated before `stale_after` and before `valid_until` | `exact` | Only for the same scope, unit, source, and window. |
| `exact` | Evaluated at or after `stale_after` | `stale` | Old numeric value may display as stale history but cannot satisfy exact policy. |
| `exact` | Evaluated after provider reset boundary when reset was known | `stale` | Reset invalidates remaining/used values even if `stale_after` is later. |
| `exact` | Source later reports conflict for same window | `unknown` | Conflict must be explicit; do not choose the larger capacity. |
| `estimated` | Evaluated before `stale_after` | `estimated` | Estimation never upgrades to exact. |
| `estimated` | Evaluated at or after `stale_after` | `stale` | Preserve estimator metadata and prior estimated value. |
| `estimated` | Exact provider reconciliation arrives | `exact` on new snapshot; prior estimate remains `estimated` | Never mutate old estimate to exact. |
| `unknown` | No new supported evidence | `unknown` | Unknown remains first-class. |
| `unknown` | Source proves unsupported or absent fact | `unavailable` | Only when a supported source proves unavailability. |
| `unknown` | Supported fresh source succeeds | `exact` or `estimated` | Confidence follows source and estimator rules. |
| `unavailable` | No source capability change | `unavailable` | Unavailable is not stale unless tied to a time-limited provider statement. |
| `unavailable` | Adapter declaration changes source support | `unknown` until refreshed | New support requires a new capture. |
| `stale` | No refresh | `stale` | Stale remains stale. |
| `stale` | Refresh succeeds | `exact`, `estimated`, `unknown`, or `unavailable` | New capture decides; old row remains historical. |
| any | Record version unknown or invalid | fail closed | Return `ErrUnknownRecordVersion` or `ErrInvalidRecord`. |

Stale rows are not deleted or rewritten by evaluation. Refresh always creates a
new observation row or a mutable latest-reference update that points to the new
row.

## Supported Quota Telemetry

Implementation issue #729 owns the first code implementation of this section.

### Source Allowlist

Quota telemetry sources are allowed only when they fit this table:

| Source kind | Allowed evidence | Maximum confidence | Network rule | Forbidden examples |
| --- | --- | --- | --- | --- |
| `official-machine-readable-api` | Provider-documented API or endpoint with stable machine-readable quota/rate-limit schema and non-secret request metadata. | `exact` for fields the provider defines as exact; otherwise `estimated` or `unknown`. | Must declare possible network and require granted network permission. | Hidden endpoints, browser-only account pages, copied cookies, undocumented response shapes. |
| `official-cli-machine-readable-command` | Provider-supported CLI command with declared fixed argv and JSON or comparable structured output. | `exact` for provider-declared exact fields. | Must declare possible network when the command may contact provider services, even if it often uses cache. | Parsing human tables as exact, shell interpolation, login/refresh commands, private UI automation. |
| `official-cli-status-or-error-class` | Bounded provider CLI result that reports quota exhausted, rate limited, reset time, unauthenticated, or model unavailable as a stable code. | `exact` for the error class and reset field when provider-defined; `unknown` for unspecified capacity. | Same declaration and grant rule as the command. | Treating free-form text like "try later" as exact reset time. |
| `provider-export-file` | Operator-supplied machine-readable export generated by a documented provider tool or portal export format. | `exact` only for signed or schema-stable fields whose capture time is known; otherwise `estimated` or `unknown`. | No network by LoopCoder if file is local; file path is local diagnostic. | Scraped HTML, screenshots, copy-pasted text, credential-bearing exports. |
| `loopcoder-local-ledger` | Deterministic UsageRecord totals and BudgetReservation accounting for LoopCoder-owned work. | `exact` for LoopCoder-local usage and reservations only. | Local-only. | Claiming it covers provider-side work outside LoopCoder. |
| `operator-configured-policy-overlay` | User-configured ceilings, default windows, or safety factors used as local policy. | `estimated` for provider capacity; exact only as a local BudgetPolicy ceiling. | Local-only unless config source itself is remote, which this phase does not define. | Calling a manually typed provider limit exact remaining quota. |
| `fixture` | Deterministic test provider or fixture source. | Whatever the fixture declares. | Test-only and explicitly marked. | Using fixtures in production state. |

All other source kinds MUST fail with `ErrQuotaSourceForbidden` or
`ErrQuotaSourceUnsupported`. Exact provider remaining quota may come only from
an official machine-readable provider surface that declares exactness for that
field. Exact LoopCoder-local usage may come from the local ledger, but it is
not provider-authoritative remaining quota.

### Network Declaration

Quota telemetry commands that may hit the network follow the same declared and
gateable rule as 0802:

- The adapter declaration must name the command or API, purpose, provider,
  scope, timeout, output limits, environment keys, classification rules,
  parser schema, and whether network may occur.
- The effective plan or policy must grant network permission for the provider,
  purpose, side-effect class, freshness window, and fingerprint before the
  command runs.
- Offline mode is not a generic failure. If policy allows unknown quota, the
  result is `unknown` or stale cached history with explicit gap reasons. If
  policy requires fresh telemetry, the operation refuses with
  `ErrQuotaConfidenceInsufficient` or `ErrTelemetryNetworkDenied`.
- Network diagnostics may record endpoint class and provider action. They must
  not record request bodies, auth headers, cookies, tokens, raw URLs containing
  credentials, or credential-bearing output.

### QuotaTelemetrySource

Schema version: `loopcoder.quota_telemetry_source.v1`.

QuotaTelemetrySource records are adapter or policy declarations. They describe
what can be called or consumed; they are not quota observations.

| Field | Required | Meaning |
| --- | --- | --- |
| `quota_source_id` | yes | Stable source identity. |
| `adapter_id` | yes | Provider adapter key. |
| `source_kind` | yes | One source kind from the allowlist. |
| `source_key` | yes | Adapter-stable key, such as `quota-json-v1` or `rate-limit-header-v1`. |
| `source_schema_version` | yes | Provider or LoopCoder parser schema version. |
| `supported_quantities` | yes | Quantity kinds this source may report. |
| `supported_windows` | yes | Window kinds this source may report. |
| `scope_dimensions` | yes | Provider/account/profile/model/project/run/task dimensions this source can distinguish. |
| `confidence_contract` | yes | For each field, whether exact, estimated, unknown, unavailable, or stale is possible. |
| `network_declared` | yes | Whether source execution may contact provider services. |
| `network_permission_scope` | conditional | Provider, action, side-effect class, and freshness scope required when network is declared. |
| `argv` | conditional | Fixed argv array when a subprocess runs. |
| `environment_keys` | yes | Names of allowed environment variables; values are forbidden. |
| `timeout_ms` | yes | Effective hard timeout for calls and parsing. |
| `output_limits` | yes | Per-stream and decoded payload caps. |
| `classification_rules` | yes | Field classifications and redaction rules from 0742. |
| `unsupported_reason` | no | Required when an adapter intentionally has no safe source. |

Adapter declarations MUST be testable with fixtures. A source declaration that
does not classify every field or bound every command is invalid.

### QuotaSnapshot

Schema version: `loopcoder.quota_snapshot.v1`.

QuotaSnapshot is immutable and records one provider, fixture, operator overlay,
or local-ledger quota observation.

| Field | Required | Meaning |
| --- | --- | --- |
| `quota_snapshot_id` | yes | Immutable snapshot identity. |
| `quota_source_id` | yes | QuotaTelemetrySource that produced or justified the snapshot. |
| `source_kind` | yes | Copied for queryability and audit. |
| `adapter_id` | conditional | Provider adapter key when provider-specific. |
| `provider_installation_id` | no | Installation used for the command, if any. |
| `account_profile_id` | no | Account/profile scope, if known. |
| `model_capability_id` | no | Model scope, if known. |
| `quantity_kind` | yes | `input-tokens`, `output-tokens`, `total-tokens`, `requests`, `wall-ms`, `concurrency`, `provider-defined`, or `local-policy`. |
| `provider_quantity_name` | no | Original provider unit name when not one of the normalized kinds. |
| `window_kind` | yes | `fixed-hour`, `fixed-day`, `fixed-week`, `rolling`, `provider-defined`, `unbounded`, or `unknown`. |
| `window_start` / `window_end` | conditional | Required when source defines exact window bounds. |
| `rolling_duration_ms` | conditional | Required for rolling windows when known. |
| `reset_at` | no | Provider-declared reset time. Guessed reset times are forbidden. |
| `limit_value` | no | Provider or policy limit for the window and quantity. |
| `used_value` | no | Used value for the window and quantity. |
| `remaining_value` | no | Remaining value for the window and quantity. |
| `reserved_value` | no | Local reserved amount when source is local ledger or budget accounting. |
| `value_scale` | yes | Integer scale for decimal-safe representation; floats are forbidden in canonical payloads. |
| `confidence` | yes | Confidence enum for the quota values as a group. |
| `field_confidences` | yes | Per-field confidence when limit, used, remaining, or reset differ. |
| `freshness_state` | yes | Freshness at capture time. |
| `captured_at` / `valid_until` / `stale_after` | yes/no | Freshness metadata. |
| `raw_source_hash` | no | Hash of bounded source bytes after classification; raw bytes are not required and may be forbidden. |
| `redacted_diagnostics` | no | Bounded local diagnostic summary. |
| `conflict_set` | yes | Snapshot IDs for same scope/window with conflicting facts. |
| `gap_reasons` | yes | Typed gaps such as `unsupported-source`, `partial-scope`, `malformed-field`, `network-denied`, `stale-cache`, `provider-disagreement`, or `not-collected`. |

If a provider reports only account-level quota while a task needs model-level
quota, the model-level value is `unknown` or `estimated`; it MUST NOT inherit
account-level exactness unless the source contract says the account limit
applies to all models uniformly.

### Source Disagreement

When fresh supported sources disagree for the same provider/account/model,
quantity, and window:

- Preserve every snapshot and link them in `conflict_set`.
- Mark the derived availability input as `unknown` with
  `gap_reasons: ["provider-disagreement"]` unless a deterministic precedence
  rule in the source declaration identifies the authoritative field.
- Never select the largest remaining value as a tie-breaker.
- Human and JSON output must show the disagreement and source IDs.

## Normalized Local Usage Ledger

Implementation issue #730 owns the first code implementation of this section.
The v0.8.0 slice persists `usage_records` and `usage_reconciliations`, derives
usage records from the real local reporter surfaces, and exposes conservative
doctor/status JSON. Budget reservations, availability scoring, and circuit
breaker state remain separate implementation slices and therefore render empty
arrays until their owning issues land.

### Quantity Normalization

All usage quantities use integer values plus a unit and scale. Floating point
values are forbidden in canonical JSON and budget math.

| Normalized kind | Required unit | Normalization rule |
| --- | --- | --- |
| `input-tokens` | `token` | Provider prompt/input token count when reported, or estimate from tokenizer/byte heuristic. |
| `output-tokens` | `token` | Provider completion/output token count when reported, or estimate from streamed/generated text. |
| `total-tokens` | `token` | Exact only when provider reports exact total or exact input plus exact output. Otherwise estimated. |
| `requests` | `request` | One provider launch or telemetry request as defined by source; retries are separate records. |
| `wall-ms` | `millisecond` | Local elapsed wall time measured by LoopCoder; exact only for local clock measurement, not provider billing. |
| `concurrency` | `slot` | Active local reservation count. |
| `provider-defined` | provider unit | Original provider unit preserved with source and conversion status. |
| `local-policy` | policy unit | Operator-defined local unit for budgets; not provider quota. |

Provider-specific units that cannot be safely converted remain
`provider-defined`. A conversion must name the conversion rule, source, and
confidence.

### UsageRecord

Schema version: `loopcoder.usage_record.v1`.

UsageRecord is append-only. Corrections and reconciliation create new records;
they do not mutate or delete prior facts.

| Field | Required | Meaning |
| --- | --- | --- |
| `usage_record_id` | yes | Immutable ledger event identity. |
| `event_kind` | yes | `estimate`, `reservation-created`, `started`, `stream-update`, `completion`, `cancellation`, `failure`, `reservation-committed`, `reservation-released`, `provider-reported`, or `correction`. |
| `event_time` | yes | UTC time the usage fact occurred or was observed. |
| `project_id` | conditional | Project scope when tied to a project. |
| `delivery_run_id` / `task_id` | no | Work scope when known. |
| `attempt_id` | no | 0801 Attempt reference when usage belongs to one attempt. |
| `worker_id` / `sub_agent_id` | no | Local worker or future agent identity when known. |
| `adapter_id` | conditional | Provider adapter key when provider-specific. |
| `account_profile_id` / `model_capability_id` | no | Provider scope when known. |
| `budget_reservation_id` | no | Reservation this event consumes or releases. |
| `quantity_kind` | yes | One normalized quantity kind. |
| `value` | yes | Signed integer delta. Positive consumes or reserves; negative releases or corrects downward. |
| `unit` | yes | Unit string. |
| `value_scale` | yes | Decimal scale; `0` for integer units. |
| `original_quantity_json` | no | Bounded original provider quantity and unit. |
| `confidence` | yes | Exact for supported provider-reported values or deterministic local counters; estimated for heuristic values. |
| `estimator` | conditional | Required when `confidence: "estimated"`. |
| `source_record_ids` | yes | QuotaSnapshot, provider receipt, reporter record, attempt, or reservation IDs that justify the event. |
| `idempotency_key` | yes | Stable key for duplicate event replay. |
| `dedupe_key` | no | Provider event ID or local stream offset when available. |
| `replaces_usage_record_id` | no | Prior record corrected by this event. |
| `gap_reasons` | yes | Gaps such as partial stream, provider rounding, missing receipt, or out-of-band provider usage. |

Duplicate reporter events, resumed streams, repeated provider receipts, and
crash replay must use idempotency and dedupe keys to return the existing record
or fail with `ErrDuplicateReplay`. They must not double-count.
Reporter ingestion uses the same persisted surfaces as `loopcoder report`:
imported machine-local report rows, run attempt files, run JSON/JSONL report
objects, relay ledgers, and pending relay records. The SQL `reports` table is
not assumed to be complete. Malformed usage payloads are counted and surfaced
as `malformed-report-payloads:<n>` gap reasons rather than skipped silently.

### Conservative Estimation

Estimation rules are mandatory:

- Estimates MUST use `confidence: "estimated"` and name `estimator`,
  `estimator_version`, inputs, capture time, and known error bound when known.
- Estimates MUST NOT be rendered as exact in human output, JSON output,
  BudgetReservation decisions, or AvailabilityScore inputs.
- When estimating tokens, implementations must use a provider/model-specific
  tokenizer only if the tokenizer source and version are persisted. Otherwise
  use a documented conservative byte or character heuristic and mark the result
  estimated.
- When estimating remaining quota from local ledger data, subtract all known
  local committed usage and active reservations from the most conservative
  applicable ceiling. Work outside LoopCoder remains unknown unless a supported
  provider source reconciles it.
- Missing usage events must bias toward refusal or lower availability when
  policy requires capacity proof. They must not create unlimited capacity.

### UsageReconciliation

Schema version: `loopcoder.usage_reconciliation.v1`.

UsageReconciliation records how provider-reported usage and local ledger facts
were compared.

| Field | Required | Meaning |
| --- | --- | --- |
| `usage_reconciliation_id` | yes | Immutable reconciliation identity. |
| `provider_snapshot_id` | conditional | QuotaSnapshot or provider-reported UsageRecord being reconciled. |
| `local_record_ids` | yes | UsageRecord IDs included in the local total. |
| `scope_key` | yes | Canonical scope used for comparison. |
| `quantity_kind` | yes | Quantity reconciled. |
| `window_kind` / `window_start` / `window_end` | yes/no | Window reconciled. |
| `local_total` | yes | Total from selected local ledger records. |
| `provider_total` | no | Provider-reported total when available. |
| `delta` | no | Provider minus local total. |
| `delta_confidence` | yes | Confidence for the difference. |
| `outcome` | yes | `matched`, `provider-higher`, `provider-lower`, `partial`, `conflicting`, or `unavailable`. |
| `correction_usage_record_ids` | yes | Correction records created by reconciliation. |
| `idempotency_key` | yes | Stable replay key. |

Provider higher usage may mean work outside LoopCoder, provider rounding, late
stream updates, or missing local records. The reconciliation must preserve that
uncertainty. It must not rewrite history to pretend LoopCoder launched work it
did not launch.

### Retention Policy

Usage and quota data is machine-local runtime state:

- QuotaSnapshot, UsageRecord, UsageReconciliation, BudgetReservation,
  AvailabilityObservation, AvailabilityScore, and CircuitBreaker records
  retain for at least the longest configured budget window plus 30 days, unless
  an explicit cleanup policy is configured.
- Records referenced by a non-terminal DeliveryRun, unresolved reservation,
  open circuit breaker, active audit event, or migration backup must not be
  deleted.
- Cleanup is explicit, bounded to LoopCoder-owned roots, symlink-safe, and
  project-scoped. It must not delete provider credentials, provider caches,
  repositories, or unrelated host files.
- Deletion writes a local cleanup event with record type, IDs or range,
  retention rule, actor provenance, and timestamp. It must not write deleted
  raw provider data into the event.

## Hierarchical Atomic Budgets

Implementation issue #731 owns the first code implementation of this section.

### Budget Scopes

Budget scopes form a hierarchy. A child reservation must fit every applicable
ancestor hard budget.

| Scope | Parent | Required identity fields |
| --- | --- | --- |
| `machine` | none | Local machine or LoopCoder home identity. |
| `project` | `machine` | `project_id`. |
| `delivery-run` | `project` | `project_id`, `delivery_run_id`. |
| `task` | `delivery-run` | `project_id`, `delivery_run_id`, `task_id`. |
| `worker` | `task` | `project_id`, `delivery_run_id`, `task_id`, `attempt_id` or worker ID. |
| `sub-agent` | `task` or `worker` | Future 0805 agent identity plus parent task/worker reference. |
| `provider-scope` | any above | Adapter/account/model dimension layered onto the owning scope. |

Provider/account/model budgets are dimensions on a scope, not separate
authority owners. A provider-specific project budget and a general project
budget can both apply to one reservation.

### BudgetPolicy

Schema version: `loopcoder.budget_policy.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `budget_policy_id` | yes | Stable policy identity. |
| `scope_key` | yes | Canonical scope object. |
| `parent_budget_policy_ids` | yes | Ancestor policies that must be checked. |
| `quantity_kind` | yes | Quantity controlled. |
| `unit` / `value_scale` | yes | Unit and scale for integer math. |
| `window_kind` | yes | Budget window. |
| `window_start` / `window_end` | conditional | Required for fixed windows. |
| `ceiling_value` | yes | Maximum allowed value for the scope/window. |
| `policy_strength` | yes | `hard` or `soft`. |
| `overflow_behavior` | yes | `refuse`, `requires-approval`, or `warn-only`; hard budgets MUST use `refuse` or `requires-approval`. |
| `override_policy` | yes | Whether a budget override is allowed and which approval kind is required. |
| `effective_from` / `effective_until` | yes/no | Policy validity. |
| `source_record_ids` | yes | User policy, config, QuotaSnapshot, or migration records that justify the ceiling. |
| `confidence` | yes | Confidence for the ceiling. User local ceilings can be exact local policy; provider capacity ceilings follow quota confidence rules. |

A budget override is not a permission override, safety override, or approval for
changed work. It only authorizes the budget exception named by policy and bound
to the current authorization fingerprint.

### BudgetReservation

Schema version: `loopcoder.budget_reservation.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `budget_reservation_id` | yes | Stable reservation identity. |
| `state` | yes | Reservation lifecycle state. |
| `scope_key` | yes | Scope receiving the reservation. |
| `budget_policy_ids` | yes | Every applicable budget policy checked. |
| `quantity_kind` | yes | Reserved quantity. |
| `reserved_value` | yes | Original reserved amount. |
| `committed_value` | yes | Amount committed to usage so far. |
| `released_value` | yes | Amount released unused so far. |
| `expires_at` | yes | Reservation lease expiry. |
| `lease_generation` | yes | Incremented on renewal. |
| `requester_id` | yes | Scheduler, task, worker, or sub-agent requesting capacity. |
| `idempotency_key` | yes | Stable replay key. |
| `authorization_fingerprint` | conditional | Required when approval or override can affect the reservation. |
| `approval_id` / `override_id` | no | 0801 authority records for required exceptions. |
| `source_estimate_usage_record_ids` | yes | Estimates or requirements that justified reservation size. |
| `commit_usage_record_ids` | yes | Usage records committed against the reservation. |
| `release_usage_record_ids` | yes | Release records tied to this reservation. |
| `refusal_error_code` | no | Typed refusal when reservation was refused before side effects. |

### Atomic Accounting

All reserve, renew, commit, release, cancel, and expire operations must run in
one immediate SQLite write transaction:

- Read every applicable BudgetPolicy, ancestor BudgetReservation aggregate,
  existing UsageRecord total, override, approval, and idempotency row.
- Verify the scope hierarchy, project isolation, policy version,
  authorization fingerprint, lease generation, and reservation state.
- Check the hard-budget invariant for every applicable policy:

```text
committed_usage_for_window
+ active_reserved_for_window
+ requested_new_reservation
<= effective_hard_ceiling
```

- Write the reservation or usage event and aggregate updates in the same
  transaction.
- On refusal, write either no row or a refusal record that cannot be mistaken
  for capacity. No provider launch, task claim, or external side effect may
  occur after a refused reservation.

Concurrent schedulers cannot oversubscribe a known hard budget because only
one write transaction may validate and reserve against the same aggregate at a
time. Optimistic updates must use `record_version`; conflicts retry within the
bounded busy-retry policy or fail with a typed error.

### Reservation Lifecycle

States are:

| State | Meaning |
| --- | --- |
| `requested` | Optional pre-validation row used only inside the transaction or for refused audit. It does not grant capacity. |
| `reserved` | Capacity is held and may be committed or released. |
| `partially-committed` | Some held capacity was committed and some remains reserved. |
| `committed` | All held capacity is committed to usage. |
| `released` | All held capacity was released unused. |
| `expired` | Lease expired and recovery released or marked the hold unusable. |
| `cancelled` | Parent task/run cancellation released the hold before commit. |
| `refused` | Reservation was denied before side effects. It never grants capacity. |

This table is exhaustive for lifecycle events:

| Current state | `reserve` | `renew` | `commit` | `release` | `expire` | `cancel` | `replay-same` | `replay-different` |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| none | `reserved` or `refused` | `ErrMissingReference` | `ErrMissingReference` | `ErrMissingReference` | `ErrMissingReference` | `ErrMissingReference` | existing result if key exists | `ErrDuplicateReplay` |
| `requested` | `reserved` or `refused` | `ErrReservationStateConflict` | `ErrReservationStateConflict` | `ErrReservationStateConflict` | `refused` | `refused` | `requested` | `ErrDuplicateReplay` |
| `reserved` | `ErrDuplicateRecord` | `reserved` with new `lease_generation` | `partially-committed` or `committed` | `released` or `partially-committed` | `expired` after deterministic recovery | `cancelled` or `partially-committed` | `reserved` | `ErrDuplicateReplay` |
| `partially-committed` | `ErrDuplicateRecord` | `partially-committed` with new `lease_generation` | `partially-committed` or `committed` | `released` if all remaining held capacity is released | `expired` for remaining held capacity | `cancelled` for remaining held capacity | `partially-committed` | `ErrDuplicateReplay` |
| `committed` | `ErrTerminalState` | `ErrTerminalState` | `committed` for idempotent same commit | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `committed` | `ErrDuplicateReplay` |
| `released` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `released` for idempotent same release | `ErrTerminalState` | `ErrTerminalState` | `released` | `ErrDuplicateReplay` |
| `expired` | `ErrTerminalState` | `ErrReservationExpired` | `ErrReservationExpired` unless exact prior commit replay | `ErrReservationExpired` unless exact prior release replay | `expired` | `ErrTerminalState` | `expired` | `ErrDuplicateReplay` |
| `cancelled` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` unless exact prior commit replay | `ErrTerminalState` | `ErrTerminalState` | `cancelled` | `cancelled` | `ErrDuplicateReplay` |
| `refused` | `ErrBudgetExhausted` or original refusal | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `ErrTerminalState` | `refused` | `ErrDuplicateReplay` |

Commit may exceed the original reserved amount only when policy explicitly
allows a bounded overrun and the same transaction reserves the additional
amount against every applicable hard budget before committing it. Otherwise it
fails with `ErrBudgetExhausted` and the parent task becomes blocked or
`needs-human` according to policy.

### Budget Exhaustion Semantics

Budget exhaustion is a typed refusal:

- A scheduler that cannot reserve required hard budget must return
  `ErrBudgetExhausted` and record rejected scope, quantity, effective ceiling,
  committed total, active reserved total, requested value, confidence, and
  source policy IDs.
- It must not silently reduce model, provider, task scope, verification
  strictness, or side-effect class to fit the budget. Any consequential change
  belongs to planning/routing and requires a fresh decision or approval when
  the fingerprint changes.
- Unknown provider quota may still allow a local budget reservation only if
  policy permits conservative scheduling with local ceilings. Output must show
  that provider quota is unknown and the budget is local, not provider
  capacity.
- Child workers and provider-native sub-agents cannot overrun parent budgets.
  A stale child completion after parent cancellation or budget exhaustion fails
  with the appropriate stale claim or reservation error.

### Policy Ownership

Budget ownership follows 0801:

| Owner | Budget authority |
| --- | --- |
| User | Sets local budgets, grants budget-specific approvals or overrides, and accepts final exceptions. |
| Deterministic policy engine | Enforces hard ceilings, confidence requirements, override requirements, and refusal semantics. |
| Planner | Estimates task requirements and records estimator confidence; it does not approve budget exceptions. |
| Router | May later score only candidates already budget-eligible; it does not make ineligible budgets eligible. |
| Scheduler | Reserves, renews, commits, releases, expires, and pauses work without weakening policy. |
| Provider-native sub-agents | Consume only the reservation delegated to their explicit scope. They never own global budgets. |

Every budget decision that affects task start, pause, refusal, or recovery must
be persisted as a 0801 Decision record or as a BudgetReservation lifecycle row
referenced by that decision.

## Availability And Circuit Breakers

Implementation issue #732 owns the first code implementation of this section.

### Availability Inputs

Availability is derived from persisted inputs only:

| Input | Source record | Required handling |
| --- | --- | --- |
| Installation health | 0802 ProviderInstallation and ProbeResult | Missing or stale probe lowers confidence; installed does not mean usable. |
| Auth readiness | 0802 AuthReadiness | `ready` helps auth component only for the same scope; `unknown` is not ready. |
| Model capability | 0802 ModelCapability | Unknown/stale hard capability fields cannot satisfy hard requirements. |
| Quota | QuotaSnapshot | Unknown/stale/estimated quota cannot satisfy exact-capacity policy. |
| Local budget | BudgetPolicy and BudgetReservation | Hard reservation refusal blocks launch. |
| Local usage | UsageRecord and UsageReconciliation | Exact only for LoopCoder-local usage or provider-reported values. |
| Recent failures | AvailabilityObservation | Failure class determines breaker event. |
| Rate limit | QuotaSnapshot or provider error class | Provider-defined reset/cooldown controls breaker timing. |
| Provider health probe | AvailabilityObservation from bounded probe | Probe uses 0802 command bounds and network declaration. |

### AvailabilityObservation

Schema version: `loopcoder.availability_observation.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `availability_observation_id` | yes | Immutable observation identity. |
| `observation_kind` | yes | `probe-success`, `probe-failure`, `auth-failure`, `quota-exhausted`, `rate-limited`, `model-unavailable`, `transport-failure`, `provider-outage`, `malformed-response`, `launch-success`, or `launch-failure`. |
| `scope_key` | yes | Provider/account/model/project/run/task scope observed. |
| `source_record_ids` | yes | ProbeResult, AuthReadiness, QuotaSnapshot, UsageRecord, Attempt, or provider receipt IDs. |
| `observed_at` | yes | UTC observation time. |
| `failure_class` | no | Stable class for failure observations. |
| `retry_after` | no | Provider-declared retry time or duration. Guessed retry times are forbidden unless marked heuristic. |
| `cooldown_until` | no | Deterministic cooldown chosen by breaker policy. |
| `confidence` | yes | Evidence confidence. |
| `network_declared` / `network_permission` | conditional | Required for health probes that may network. |

### AvailabilityScore

Schema version: `loopcoder.availability_score.v1`.

AvailabilityScore is a diagnostic score, not routing authority. It is
calculated from persisted records with deterministic inputs:

| Component | Weight | Score rule |
| --- | --- | --- |
| `auth` | 25 | 25 for fresh `ready`; 0 for `not-authenticated`, `expired`, `unknown`, unavailable, or stale. |
| `health` | 25 | 25 for fresh successful probe or launch success; 10 for no recent probe but no failures; 0 for recent outage, malformed response, or stale required probe. |
| `quota` | 20 | 20 for fresh exact quota with remaining capacity; 10 for policy-allowed conservative local estimate; 0 for exhausted, unknown, unavailable, stale, or conflict when exact policy is required. |
| `rate_limit` | 15 | 15 when no active rate-limit breaker applies; 0 when rate limited or cooldown active. |
| `recent_failures` | 15 | 15 with no relevant failures in window; 8 with transient failures below threshold; 0 when failure threshold opens breaker. |

The total is `0` through `100`. The calculation must also persist:

- `score_confidence`: `exact` only when every component uses fresh exact or
  deterministic local evidence; otherwise `estimated`, `unknown`, or `stale`.
- `hard_ineligible_reasons`: typed reasons that block launch regardless of
  score, such as missing auth, missing hard capability, open breaker, budget
  exhaustion, or insufficient quota confidence.
- `heuristic: true` when any component uses the default weight table rather
  than provider-declared exact health semantics.

Unknown quota degrades safely: it contributes `0` to the quota component when
policy requires exact quota and at most `10` when policy explicitly allows
conservative local scheduling. It never fabricates remaining capacity.

### CircuitBreaker

Schema version: `loopcoder.circuit_breaker.v1`.

| Field | Required | Meaning |
| --- | --- | --- |
| `circuit_breaker_id` | yes | Stable breaker identity. |
| `breaker_kind` | yes | `quota`, `rate-limit`, `auth`, `health`, `model`, or `transport`. |
| `state` | yes | `closed`, `open`, or `half-open`. |
| `scope_key` | yes | Provider/account/model/project/run/task scope. |
| `opened_at` | no | Time breaker opened. |
| `open_until` | no | Earliest deterministic half-open time. |
| `half_open_probe_budget` | yes | Number of allowed recovery probes or launches. |
| `half_open_probe_count` | yes | Used probes in current half-open period. |
| `failure_count` | yes | Count in the configured failure window. |
| `success_count` | yes | Consecutive successes relevant to recovery. |
| `last_observation_id` | no | Latest observation that changed state. |
| `state_reason` | yes | Typed reason for current state. |
| `policy_version` | yes | Breaker policy version. |
| `record_version` | yes | Optimistic version for state transitions. |

Breaker transitions are persisted in the same transaction as the observation
or scheduler refusal they affect. Unknown or invalid breaker records fail
closed with `ErrInvalidRecord` or `ErrUnknownRecordVersion`.

### Breaker State Machine

Events are normalized before transition:

| Event | Meaning |
| --- | --- |
| `success` | Fresh successful launch or health probe for the same scope. |
| `transient-failure` | Transport failure, timeout, or provider 5xx without provider-declared outage. |
| `provider-outage` | Supported source reports provider outage or repeated failures meet outage threshold. |
| `rate-limit` | Provider reports rate limit or 429-equivalent with scope. |
| `quota-exhausted` | Supported quota source reports zero remaining or quota-exhausted class. |
| `auth-failure` | Auth readiness or launch reports unauthenticated, expired, revoked, or unauthorized. |
| `model-unavailable` | Model no longer available for the account/profile. |
| `malformed-response` | Bounded parser rejects provider response for the expected schema. |
| `cooldown-elapsed` | Deterministic clock reaches `open_until`. |
| `probe-success` | Half-open recovery probe succeeds. |
| `probe-failure` | Half-open recovery probe fails. |
| `manual-reset` | Explicit user or policy reset with authority and fingerprint. |
| `config-changed` | Relevant adapter, auth, model, or budget config changed. |

This table is exhaustive for breaker states:

| Current state | Event | Next state | Required outcome |
| --- | --- | --- | --- |
| `closed` | `success` | `closed` | Reset relevant failure count. |
| `closed` | `transient-failure` below threshold | `closed` | Record failure; score lowers. |
| `closed` | `transient-failure` at threshold | `open` | Set deterministic `open_until`; block launches. |
| `closed` | `provider-outage` | `open` | Open health breaker; use provider or policy cooldown. |
| `closed` | `rate-limit` | `open` | Open rate-limit breaker until provider reset or deterministic cooldown. |
| `closed` | `quota-exhausted` | `open` | Open quota breaker until provider reset or quota refresh proves capacity. |
| `closed` | `auth-failure` | `open` | Open auth breaker until fresh AuthReadiness is ready or manual reset. |
| `closed` | `model-unavailable` | `open` | Open model breaker until fresh catalog proves availability. |
| `closed` | `malformed-response` | `open` when threshold met, else `closed` | Never parse malformed output as capacity. |
| `closed` | `cooldown-elapsed` | `closed` | No-op. |
| `closed` | `probe-success` | `closed` | Treat as success. |
| `closed` | `probe-failure` | `closed` or `open` by normalized failure class | Apply corresponding failure rule. |
| `closed` | `manual-reset` | `closed` | Persist reset decision. |
| `closed` | `config-changed` | `closed` | Reset only affected counters if policy says the evidence changed. |
| `open` | `success` | `open` | Ignored unless it is an authorized half-open probe or manual recovery. |
| `open` | any failure event | `open` | Extend or keep cooldown deterministically; record observation. |
| `open` | `cooldown-elapsed` | `half-open` | Allow bounded recovery probes only. |
| `open` | `probe-success` | `open` | Invalid unless half-open probe was authorized; return `ErrBreakerOpen`. |
| `open` | `probe-failure` | `open` | Keep or extend cooldown. |
| `open` | `manual-reset` | `closed` or `half-open` | User/policy decision chooses explicit target and records reason. |
| `open` | `config-changed` | `half-open` when relevant evidence may have changed, otherwise `open` | Requires deterministic rule. |
| `half-open` | `success` | `closed` when required success count reached, otherwise `half-open` | Consume probe budget and record success. |
| `half-open` | `probe-success` | `closed` when required success count reached, otherwise `half-open` | Same as success. |
| `half-open` | any failure event | `open` | Reopen with deterministic cooldown. |
| `half-open` | `cooldown-elapsed` | `half-open` | No-op while probe budget remains. |
| `half-open` | `manual-reset` | `closed` | Persist reset decision. |
| `half-open` | `config-changed` | `half-open` | Recompute scope and probe budget deterministically. |

Half-open probes must use the same bounded execution and network declaration
rules as provider inventory and quota telemetry. They must avoid thundering
herds by using deterministic cooldown and probe-budget state. Jitter, if used,
must come from a persisted deterministic seed so tests can reproduce it.

### Breaker Triggers

Minimum triggers are normative:

- A provider-declared rate limit or 429-equivalent opens the rate-limit breaker
  for the reported scope. If `retry_after` is exact, use it. If absent, use the
  policy cooldown and mark the cooldown heuristic.
- A fresh exact quota snapshot with `remaining_value <= 0` opens the quota
  breaker until the provider reset time or until a fresh exact snapshot proves
  capacity.
- Unknown, unavailable, stale, or conflicting quota does not by itself open the
  quota breaker, but it makes exact-capacity policy ineligible with
  `ErrQuotaConfidenceInsufficient`.
- Auth failure opens the auth breaker and blocks launch until fresh readiness
  proves `ready` or an explicit user action updates the account/profile.
- Model unavailable opens the model breaker for that model/account scope until
  a fresh catalog proves availability.
- Malformed quota or health response never becomes capacity. Repeated malformed
  responses open a health breaker.

## Storage And JSON Output

### Machine-Local Storage

Records live in the machine-local SQLite store under the global project storage
model from
[`0639-global-data-layout-project-identity.md`](0639-global-data-layout-project-identity.md).
The initial v0.8 schema additions are logical tables equivalent to:

- `quota_telemetry_sources`;
- `quota_snapshots`;
- `usage_records`;
- `usage_reconciliations`;
- `budget_policies`;
- `budget_reservations`;
- `budget_aggregates` for transactionally maintained query totals;
- `availability_observations`;
- `availability_scores`;
- `circuit_breakers`;
- `quota_budget_events` for tamper-evident authority events when records
  affect DeliveryRun inputs or task launch.

The v0.8 migration must follow the 0801 migration conventions: backup metadata
before migration, one immediate SQLite write transaction for schema changes,
idempotent re-run, conservative handling of ambiguous legacy state, no
repository mutation, no credential migration, no provider launch, and
machine-local backup paths only.

### Doctor JSON

`loopcoder doctor --format json` must expose quota, usage, budget, and
availability diagnostics as an additive root object named
`quota_usage_budget`. Existing fields remain valid. The new object contains
redacted, bounded, machine-readable state:

```json
{
  "quota_usage_budget": {
    "schema_version": "loopcoder.quota_usage_budget_json.v1",
    "generated_at": "2026-07-12T00:00:00Z",
    "quota_usage_fingerprint": "sha256:...",
    "confidence": "unknown",
    "quota_sources": [],
    "quota_snapshots": [],
    "usage_summary": [],
    "budget_summary": [],
    "availability_scores": [],
    "circuit_breakers": [],
    "gap_reasons": []
  }
}
```

Doctor JSON MUST distinguish:

- no supported quota source;
- network-capable source not granted;
- exact fresh provider quota;
- local usage estimate;
- stale quota history;
- configured local budget ceiling;
- active reservations;
- budget exhaustion refusal;
- open, closed, and half-open circuit breakers;
- provider rate limit versus quota exhaustion versus auth failure.

Secret material, raw provider output, unredacted local paths, request bodies,
auth headers, cookies, and credential-bearing URLs have no valid JSON form.

### Status JSON

`loopcoder status --format json` must expose references and summaries only when
these records affect a DeliveryRun, Task, attempt, or future route. It MUST NOT
duplicate full raw quota or usage history by default:

```json
{
  "quota_usage_refs": {
    "schema_version": "loopcoder.quota_usage_refs.v1",
    "quota_usage_fingerprint": "sha256:...",
    "quota_snapshot_ids": [],
    "usage_record_ids": [],
    "budget_policy_ids": [],
    "budget_reservation_ids": [],
    "availability_score_ids": [],
    "circuit_breaker_ids": [],
    "confidence": "unknown",
    "hard_ineligible_reasons": [],
    "gap_reasons": []
  }
}
```

When status explains blocked or needs-human state, it may include bounded
redacted summaries of referenced records: effective ceiling, requested
reservation, committed total, active reserved total, quota confidence, breaker
state, and typed refusal code. It must not expose secrets or raw provider
diagnostics.

## Failure Honesty Rules

These rules are normative and testable:

- A supported quota source list is an allowlist. Everything outside it is
  forbidden, not "best effort".
- Private provider UI scraping, authenticated browser-page scraping,
  reverse-engineered quota endpoints, credential-file parsing, copied cookies,
  and environment value inspection are forbidden.
- `unknown` MUST remain `unknown` until supported evidence proves a narrower
  state.
- `unavailable` MUST remain distinct from `unknown`: unavailable means a
  supported source proved absence or unsupported status.
- `stale` MUST remain distinct from fresh confidence. Stale exact evidence is
  stale, not exact.
- Estimated usage, estimated quota, and configured local overlays MUST NOT
  satisfy policies that require exact provider quota.
- The local ledger is exact only for LoopCoder-local records. It MUST NOT claim
  exact provider-wide usage or remaining quota.
- Budget reservation refusal is explicit and typed. It MUST NOT silently reduce
  task scope, skip verification, switch provider, or launch without capacity.
- Availability score is diagnostic. It MUST NOT bypass hard ineligibility,
  policy denial, budget exhaustion, open breakers, or insufficient confidence.
- Every consequential number in human or JSON output MUST link to persisted
  source records or be marked heuristic with confidence and estimator metadata.

## Implementation Acceptance Mapping

### #729 Supported Quota Telemetry

Issue #729 is complete only when its code and tests implement the sections
"Supported Quota Telemetry", "QuotaTelemetrySource", "QuotaSnapshot",
"Freshness And Confidence", and the related doctor/status JSON:

- source allowlists reject UI scraping, reverse-engineered endpoints, browser
  cookies, credential material, shell interpolation, and undeclared commands;
- official CLI/API, provider export, local ledger, configured overlay, and
  fixture sources are represented with correct maximum confidence;
- exact, estimated, unknown, unavailable, stale, conflicting, malformed,
  delayed, and partially scoped telemetry are fixture-tested;
- fixed, rolling, provider-defined, and reset-boundary windows are tested under
  deterministic clocks, including timezone and DST boundaries;
- telemetry commands that may network declare network behavior and refuse when
  network permission is denied;
- human and JSON output names scope, unit, source, confidence, freshness, reset
  semantics, conflict sets, and gap reasons without claiming unsupported
  precision.

### #730 Local Usage Ledger And Estimates

Issue #730 is complete only when its code and tests implement the sections
"Normalized Local Usage Ledger", "UsageRecord", "UsageReconciliation",
"Conservative Estimation", and "Retention Policy":

- retries, resumed streams, duplicate reporter events, partial usage, provider
  rounding, work outside LoopCoder, crash recovery, and idempotent replay do
  not double-count or fabricate precision;
- token, request, wall-time, concurrency, provider-defined, and local-policy
  units are normalized with original units preserved;
- estimates always use `confidence: "estimated"` with estimator metadata and
  never satisfy exact-quota policy;
- reconciliation creates append-only correction records, preserves prior facts,
  remains idempotent, and exposes provider higher/lower disagreements;
- per-provider, account, model, project, DeliveryRun, task, worker, and
  sub-agent queries preserve project isolation and redaction;
- deterministic clocks, provider fixtures, restart/replay tests, and JSON
  contract tests require no real paid provider calls.

### #731 Hierarchical Atomic Budgets

Issue #731 is complete only when its code and tests implement the sections
"Hierarchical Atomic Budgets", "BudgetPolicy", "BudgetReservation",
"Atomic Accounting", "Reservation Lifecycle", and "Budget Exhaustion
Semantics":

- machine, project, DeliveryRun, task, worker, sub-agent, and provider/account
  dimensions are represented without cross-project leakage;
- reserve, renew, commit, release, expire, cancel, and refused paths are
  transactional, idempotent, and covered by state-machine tests;
- concurrent schedulers cannot reserve beyond any applicable hard ceiling;
- crashes after reserve, during launch, after partial commit, after
  cancellation, and after stale lease expiry reconcile without leaking or
  duplicating reservations;
- unknown quota and estimated requirements apply documented conservative policy
  and require approval for configured exceptions;
- human and JSON output explain effective ceiling, inheritance, confidence,
  available value, reserved value, committed value, denial, approval, and
  override provenance.

### #732 Availability And Circuit Breakers

Issue #732 is complete only when its code and tests implement the sections
"Availability And Circuit Breakers", "AvailabilityObservation",
"AvailabilityScore", "CircuitBreaker", "Breaker State Machine", and "Breaker
Triggers":

- hard ineligibility is separate from score, and a high score cannot bypass
  capability, permission, risk, quota confidence, budget, auth, or breaker
  policy;
- 429/rate-limit, quota exhausted, auth failure, model unavailable, transient
  transport, provider outage, malformed response, stale evidence, and unknown
  telemetry drive distinct documented transitions;
- closed, open, and half-open breaker transitions are deterministic,
  persistent, replayable, and tested with fake clocks and deterministic random
  seeds when jitter is configured;
- recovery probes use the bounded command, output, network declaration, and
  classification rules from 0802 and avoid thundering-herd behavior;
- stale or unknown telemetry lowers confidence and triggers policy refusal or
  conservative scheduling rather than appearing fully available;
- doctor and status JSON show breaker state, reason, cooldown, probe budget,
  source observation IDs, hard ineligible reasons, and score confidence.

## Relationship To Existing Specs And Docs

- [`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md) defines
  stable IDs, provenance, confidence enum values, policy fingerprints,
  side-effect classes, typed errors, idempotency, transaction conventions, and
  decision ownership boundaries. This spec reuses those conventions for quota,
  usage, budget, and availability records.
- [`0802-provider-inventory.md`](0802-provider-inventory.md) defines provider
  installation, probe, account/profile, auth readiness, model catalog, bounded
  adapter commands, network declarations, machine-local storage, and JSON
  exposure. This spec references those records rather than duplicating provider
  identity or auth readiness.
- [`0742-v080-security-threat-model.md`](0742-v080-security-threat-model.md)
  names quota telemetry as an attack surface and forbids credential access,
  private UI scraping, fabricated exact quota, and hidden uncertainty. This
  spec makes those controls concrete for #729 through #732.
- [`0639-global-data-layout-project-identity.md`](0639-global-data-layout-project-identity.md)
  defines machine-local global storage and project identity. This spec uses
  `project_id` as the isolation key and keeps runtime records out of project
  repositories.
- [`0646-nested-sub-agent-plan.md`](0646-nested-sub-agent-plan.md) defines
  nested runs, claims, leases, fencing, and stale completion behavior. This
  spec aligns budget reservations and child usage with those ownership and
  recovery rules.
