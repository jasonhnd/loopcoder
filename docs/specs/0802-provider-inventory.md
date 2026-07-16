---
id: 802
title: v0.8.0 Phase 2 Provider Inventory
status: draft
date: 2026-07-12
issue: 785
pr: null
supersedes: []
superseded_by: []
---

# v0.8.0 Phase 2 Provider Inventory

This documentation-only spec freezes the phase-2 provider, account, and model
inventory contract that implementation issues
[#725](https://github.com/jasonhnd/loopcoder/issues/725),
[#726](https://github.com/jasonhnd/loopcoder/issues/726),
[#727](https://github.com/jasonhnd/loopcoder/issues/727), and
[#728](https://github.com/jasonhnd/loopcoder/issues/728) must implement for the
v0.8.0 provider inventory epic
[#716](https://github.com/jasonhnd/loopcoder/issues/716).

This spec follows the shared durable-record conventions from
[`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md): opaque
stable IDs, explicit `schema_version` and `record_version`, actor and host
provenance, UTC timestamps, typed errors, machine-local runtime state, and the
confidence enum values `exact`, `estimated`, `unknown`, `unavailable`, and
`stale`. It also applies the provider-discovery controls from
[`0742-v080-security-threat-model.md`](0742-v080-security-threat-model.md):
provider discovery is local-only by default, credential material is forbidden
in LoopCoder state and output, provider output is untrusted and bounded, and
unknown evidence is never promoted to false certainty.

This document adds no Go code, SQLite migration, CLI behavior, workflow change,
provider integration, or runtime control. Per [`../PROCESS.md`](../PROCESS.md),
this spec must merge before the code issues above implement it.

## Goals

- Define durable inventory records for provider installations, probe results,
  account profiles, auth readiness, model catalog snapshots, and model
  capabilities.
- Define bounded provider adapter probes that discover installed CLIs without
  proving authentication, authorization, quota, or usable models.
- Define credential-blind auth readiness detection that can represent multiple
  accounts and profiles while never reading, copying, hashing, parsing, or
  serializing credential material.
- Define dynamic model catalog snapshots with per-entry provenance, lifecycle,
  freshness, confidence, and capability fields aligned with the existing
  provider compatibility matrix.
- Define a future-provider adapter contract that lets a fourth provider
  participate in probe, auth readiness, catalog, and invocation-profile
  inventory without scheduler-core edits.
- Define failure honesty rules and JSON output surfaces so users and machines
  can see inventory confidence, staleness, partial failures, and explicit gaps.

## Non-Goals

- No quota, usage, budget, or availability math. Those belong to phase 0803.
- No routing heuristics, role-envelope ranking, fallback policy, or scheduler
  selection behavior. Those belong to phase 0804.
- No provider-native federation, agent-tree execution, or child-agent result
  schema. Those belong to phase 0805.
- No UX flows, guided setup, automatic login, browser consent automation,
  credential migration, or private provider UI scraping.
- No binary plugin loading or execution of untrusted third-party adapter code.
- No repository-tracked runtime payloads. Inventory state is machine-local.

## Terms

**Provider adapter** means LoopCoder-owned code and declaration metadata for a
provider key such as `codex`, `claude`, `gemini`, or `antigravity`. It declares
allowed executables, probe commands, auth-readiness mechanisms, catalog sources,
output schemas, capabilities, and invocation profiles.

**Provider installation** means one discovered local executable path or
explicit configured executable for a provider adapter. One provider may have
zero, one, or many installations because PATH entries, shims, symlinks, package
manager versions, and explicit config can coexist.

**Probe** means a bounded local command or local metadata check run through an
adapter declaration to discover whether an installation exists and whether it
is minimally inspectable. A probe is not authentication, model authorization,
quota evidence, or provider launch readiness unless a separate record proves
that narrower fact.

**Account profile** means a credential-blind local reference to a provider
account, profile, tenant, project, organization, or comparable provider context.
The reference may be a provider-declared public identifier, a redacted display
label, or a collision-safe local hash of a non-secret identifier. It is never a
token, cookie, refresh token, private key, credential file content, or parsed
credential object.

**Auth readiness** means the latest credential-blind evidence that an account
profile is ready, not authenticated, expired, or unknown for a provider adapter.
It does not prove authorization for every model or operation.

**Model catalog snapshot** means one immutable captured view of model names,
aliases, lifecycle, capabilities, constraints, and provenance for a provider,
installation, account profile, or adapter-declared static source.

**Model capability** means one model entry inside one catalog snapshot. It is
an inventory fact, not a routing decision.

**Inventory fingerprint** means a canonical hash of the exact inventory records
used as an authority input for a DeliveryRun plan, approval, or later routing
decision. It follows the canonicalization rules in
[`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md).

## Shared Representation

The implementation must expose one logical provider inventory contract across
Go, SQLite, and JSON:

- JSON field names use snake_case, explicit `schema_version` strings, and
  stable enum strings.
- SQLite stores queryable identity, lifecycle, freshness, confidence, and
  relationship fields in normal columns. Bounded structured evidence may be
  stored in `*_json` columns only when it is not needed for joins, uniqueness,
  lifecycle transitions, or policy checks.
- SQLite `TEXT` IDs are opaque. Callers must not parse provider names,
  timestamps, paths, account labels, or model names from IDs.
- Every record has `schema_version`, `record_version`, timestamps, provenance,
  classification, freshness metadata, and confidence. Project-scoped rows have
  `project_id`; machine-scoped rows set `scope: "machine"` and `project_id:
  null`.
- All timestamps are UTC RFC3339 strings rendered with `Z`.
- Unknown enum values in persisted inventory fail closed with
  `ErrUnknownRecordVersion` or `ErrInvalidRecord`, matching 0801.
- Inventory records that affect a DeliveryRun, Task, approval, or later route
  must contribute their exact record IDs and canonical payload hashes to the
  applicable `input_fingerprint`, `policy_fingerprint`, or later routing
  fingerprint.

### Inventory ID Scheme

ID prefixes are stable; the bytes after the prefix are opaque:

| Record | ID field | Required form |
| --- | --- | --- |
| ProviderInstallation | `provider_installation_id` | `pinst_<base32-sha256(adapter_id, normalized_executable_identity, path_hash, platform)>` plus a collision suffix when two installations are indistinguishable until probed. |
| ProbeResult | `probe_result_id` | `probe_<uuidv7-or-random-128-bit-base32>`; immutable per probe attempt. |
| AccountProfile | `account_profile_id` | `acct_<base32-sha256(adapter_id, profile_source, normalized_profile_reference_hash)>`. |
| AuthReadiness | `auth_readiness_id` | `auth_<uuidv7-or-random-128-bit-base32>`; immutable per readiness capture. |
| ModelCatalogSnapshot | `model_catalog_snapshot_id` | `mcatsnap_<uuidv7-or-random-128-bit-base32>`; immutable per catalog capture. |
| ModelCapability | `model_capability_id` | `mcap_<base32-sha256(model_catalog_snapshot_id, canonical_model_id, lifecycle)>`. |
| AdapterDeclaration | `adapter_declaration_id` | `adapter_<base32-sha256(adapter_id, declaration_schema_version, adapter_version)>`. |
| Inventory fingerprint | `inventory_fingerprint` | The digest string itself: `sha256:<64-lower-hex>`. |

`normalized_executable_identity` is derived from the adapter declaration and
the resolved executable identity. It must not include raw credential material.
`path_hash` is the hash of the canonical executable path bytes after local path
normalization. Human and PR-safe output may show an executable basename or
redacted path, but raw absolute paths remain local diagnostics.

### Common Inventory Fields

All durable records in this spec carry the following fields unless a record
table explicitly marks one as not applicable:

| Field | Required | Meaning |
| --- | --- | --- |
| `schema_version` | yes | Stable JSON/storage shape string. |
| `record_version` | yes | Optimistic update version for mutable records; immutable snapshots keep `1`. |
| `scope` | yes | `machine` or `project`. |
| `project_id` | conditional | Required when `scope` is `project`; null when `scope` is `machine`. |
| `created_at` / `updated_at` | yes | UTC persistence timestamps. |
| `captured_at` | yes for observations | UTC time when evidence was captured. |
| `valid_until` | no | Timestamp until which policy may treat evidence as fresh. |
| `stale_after` | no | Timestamp after which output MUST report `confidence: "stale"` unless refreshed. |
| `freshness_state` | yes | `fresh`, `stale`, `expired`, or `not-applicable`. |
| `confidence` | yes | Exactly one of `exact`, `estimated`, `unknown`, `unavailable`, or `stale` from 0801. |
| `created_by` / `updated_by` | yes | Actor provenance object from 0801. |
| `host` | yes | Host provenance object from 0801. |
| `policy_version` | conditional | Required when the record affects eligibility, authority, or output filtering. |
| `side_effect_class` | yes | Maximum side-effect class involved; inventory probes are usually `local-read`. |
| `classification` | yes | One of the 0742 data classifications for the record's most sensitive field. |
| `source` | yes | Structured source descriptor for adapter, command, file-existence check, static registry, machine-readable listing, or configured overlay. |
| `evidence` | yes | Bounded classified evidence summary; never raw secret material. |
| `gap_reasons` | yes | Ordered typed reasons for unknown, unavailable, stale, partial, or conflicting facts. Empty when no gap exists. |
| `terminal_error_code` | no | Typed error when the record represents a failed terminal observation. |

`created_by` and `updated_by` use the 0801 provenance object. Inventory created
by deterministic adapter code should normally use `actor_kind:
"policy-engine"` or `actor_kind: "migration"` as applicable. Host metadata is
diagnostic and never authorizes provider launch, credential access, or routing.

### Freshness And Confidence

Freshness is separate from confidence:

| Field | Meaning |
| --- | --- |
| `captured_at` | When the fact was observed. |
| `valid_until` | Optional provider or policy validity horizon, such as an auth status expiry. |
| `stale_after` | Local freshness horizon after which the fact cannot satisfy fresh-evidence policy. |
| `freshness_state` | Current state computed from the timestamps and policy. |
| `confidence` | Evidence quality: `exact`, `estimated`, `unknown`, `unavailable`, or `stale`. |

An exact fact becomes `confidence: "stale"` when reused after `stale_after`.
Staleness does not delete the old record. Implementations MUST retain stale
historical snapshots until retention policy deletes them through an explicit
machine-local cleanup path.

## Durable Records

### ProviderInstallation

Schema version: `loopcoder.provider_installation.v1`.

ProviderInstallation records are mutable only for latest-observation metadata.
Historical facts are preserved through immutable ProbeResult records.

| Field | Required | Meaning |
| --- | --- | --- |
| `provider_installation_id` | yes | Stable installation identity. |
| `adapter_id` | yes | Provider key from the adapter declaration, such as `codex` or `antigravity`. |
| `adapter_declaration_id` | yes | Declaration version used for discovery. |
| `provider_display_name` | yes | Human display name from the adapter declaration. |
| `executable_name` | yes | Declared executable name, such as `codex` or `agy`. |
| `executable_identity` | yes | Structured local identity: basename, platform, path hash, symlink resolution result, and optional binary hash when safe and bounded. |
| `canonical_path` | local only | Machine-local absolute path, classified `sensitive-path`, redacted by default. |
| `canonical_path_redacted` | yes | Display-safe path form for human and JSON output. |
| `discovery_source` | yes | `path`, `explicit-config`, `default-location`, `fixture`, or `migration`. |
| `discovery_order` | yes | Deterministic order within the scan source. |
| `platform` | yes | OS/arch string such as `windows-amd64`. |
| `version` | no | Parsed provider CLI version when the adapter has exact evidence. |
| `version_confidence` | yes | Confidence enum for `version`. |
| `latest_probe_result_id` | no | Most recent ProbeResult for this installation. |
| `installation_state` | yes | `installed`, `not-installed`, `installed-but-unusable`, `probe-failed`, or `stale`. |
| `usable_for_invocation` | yes | `true`, `false`, or `unknown`; MUST NOT be true from install evidence alone. |
| `known_limitations` | yes | Bounded adapter-declared or observed limitations. |

Multiple installations for one provider are represented by multiple
ProviderInstallation rows with the same `adapter_id` and different
`provider_installation_id` values. The current preferred installation is not
stored by overwriting the others; it is selected by a policy or user choice
that references one exact installation ID.

### ProbeResult

Schema version: `loopcoder.probe_result.v1`.

ProbeResult is immutable and records one bounded probe execution or local
metadata check.

| Field | Required | Meaning |
| --- | --- | --- |
| `probe_result_id` | yes | Immutable probe identity. |
| `adapter_id` | yes | Provider adapter key. |
| `adapter_declaration_id` | yes | Declaration version used. |
| `provider_installation_id` | conditional | Installation inspected, or null for an absent PATH/config probe. |
| `probe_kind` | yes | `install`, `version`, `auth-readiness`, `catalog`, or `health`. |
| `probe_command_id` | conditional | Adapter allowlist key when a subprocess ran. |
| `probe_method` | yes | `look-path`, `lstat`, `fixed-command`, `machine-readable-command`, `static-declaration`, or `configured-overlay`. |
| `outcome` | yes | `installed`, `not-installed`, `installed-but-unusable`, or `probe-failed` for install probes; other probe kinds use their record-specific state plus `probe-failed` when execution failed. |
| `argv` | conditional | Fixed argv array from the adapter declaration, with no shell interpolation. |
| `working_directory` | yes | Declared working directory class, not arbitrary repository input. |
| `environment_keys` | yes | Names of allowed environment variables passed; values are forbidden. |
| `timeout_ms` | yes | Effective hard timeout. |
| `stdout_limit_bytes` / `stderr_limit_bytes` | yes | Per-stream output caps. |
| `combined_output_limit_bytes` | yes | Combined cap. |
| `exit_code` | conditional | Process exit code when available. |
| `timed_out` / `killed` | yes | Process supervision result. |
| `network_declared` | yes | Whether the adapter declared possible network behavior for this probe. |
| `network_permission` | yes | `not-needed`, `denied`, or `granted`. |
| `stdout_summary` / `stderr_summary` | no | Redacted bounded summaries classified as `provider-output-untrusted`. |
| `parsed_fields_json` | no | Bounded parsed non-secret fields from declared schema. |
| `secret_finding_count` | yes | Count of redaction or rejection findings; raw findings are not persisted. |

ProbeResult records MUST preserve partial success. For example, if version
parsing succeeds but catalog parsing fails, the installation and version facts
remain available while the catalog gap is explicit.

### AccountProfile

Schema version: `loopcoder.account_profile.v1`.

AccountProfile represents a credential-blind provider context. It may be
machine-scoped or project-scoped when a project policy pins a profile.

| Field | Required | Meaning |
| --- | --- | --- |
| `account_profile_id` | yes | Stable profile identity. |
| `adapter_id` | yes | Provider adapter key. |
| `provider_installation_id` | no | Installation that produced the profile evidence, if applicable. |
| `profile_source` | yes | `provider-status-command`, `provider-config-reference`, `environment-reference`, `user-config`, `fixture`, or `unknown`. |
| `profile_reference_hash` | yes | Collision-safe hash of the non-secret profile reference. |
| `profile_display` | no | Redacted display label safe for local output. |
| `provider_account_kind` | no | Provider-declared kind such as user, org, tenant, project, workspace, or subscription. |
| `tenant_or_project_reference_hash` | no | Hash for provider tenant/project context when safe and available. |
| `selection_state` | yes | `default`, `explicit`, `candidate`, `superseded`, or `unknown`. |
| `latest_auth_readiness_id` | no | Most recent AuthReadiness record. |
| `allowed_scope_summary` | no | Bounded non-secret provider-declared scope summary, never a credential. |
| `collision_set` | yes | Other profile IDs with the same display label or ambiguous reference. |

Account/profile selection MUST be collision-safe. If two profiles render the
same display label, user policy or CLI flags must select by opaque
`account_profile_id` or another collision-safe handle, not by display text.

### AuthReadiness

Schema version: `loopcoder.auth_readiness.v1`.

AuthReadiness is immutable and records one credential-blind readiness capture
for an account profile, installation, or adapter when no profile is known.

| Field | Required | Meaning |
| --- | --- | --- |
| `auth_readiness_id` | yes | Immutable readiness identity. |
| `adapter_id` | yes | Provider adapter key. |
| `provider_installation_id` | no | Installation inspected, if known. |
| `account_profile_id` | no | Profile inspected, if known. Null means adapter-level readiness only. |
| `readiness_state` | yes | `ready`, `not-authenticated`, `expired`, or `unknown`. |
| `readiness_confidence` | yes | Confidence enum. |
| `evidence_kind` | yes | `exit-code`, `sanctioned-status-command`, `machine-readable-status`, `file-existence`, `environment-name-existence`, `provider-error-class`, `unsupported`, or `not-run`. |
| `authorization_scope_state` | yes | `all-known`, `partial`, `unknown`, or `not-applicable`. |
| `authorization_scope_summary` | no | Bounded non-secret summary such as "profile can list models" or "model authorization unknown". |
| `expires_at` | no | Provider-declared auth expiry when exposed without credential access. The initial #726 implementation intentionally omits this field from Go structs and SQLite payloads because no current adapter exposes a credential-blind machine-readable expiry value. |
| `refresh_required` | yes | Whether evidence says the provider requires login or refresh; this never triggers refresh automatically. |
| `unsupported_reason` | no | Required when readiness is unknown because the adapter lacks a safe readiness mechanism. |

`ready` means the adapter has credential-blind evidence that the provider
accepted a supported readiness check for that profile or installation.
`ready` MUST NOT mean every model is authorized, quota is available, a remote
call will succeed, or provider launch has already been approved.

### ModelCatalogSnapshot

Schema version: `loopcoder.model_catalog_snapshot.v1`.

ModelCatalogSnapshot is immutable and groups the exact catalog evidence used
for later policy, routing, or user display.

| Field | Required | Meaning |
| --- | --- | --- |
| `model_catalog_snapshot_id` | yes | Immutable snapshot identity. |
| `adapter_id` | yes | Provider adapter key. |
| `provider_installation_id` | no | Installation used for the snapshot, if applicable. |
| `account_profile_id` | no | Account/profile context used, if applicable. |
| `auth_readiness_id` | no | Readiness evidence associated with account-scoped catalog capture. |
| `catalog_source_kind` | yes | `adapter-declared`, `provider-machine-readable`, `configured-overlay`, `fixture`, or `migration`. |
| `catalog_source_reference` | yes | Bounded source descriptor, not raw provider output. |
| `source_schema_version` | no | Provider or overlay schema version when known. |
| `provider_cli_version` | no | Version of provider CLI that produced the snapshot. |
| `source_precedence` | yes | Ordered precedence class used when conflicts exist. |
| `entry_count` | yes | Number of ModelCapability records in the snapshot. |
| `conflict_count` | yes | Number of conflicts preserved in entries. |
| `stale_policy` | yes | Freshness policy name and stale horizon used. |
| `inventory_fingerprint` | yes | Hash of snapshot metadata and entry hashes. |

Snapshots MUST be durable. Routing or policy decisions that use catalog facts
must reference the exact `model_catalog_snapshot_id` and entry IDs they used.

### ModelCapability

Schema version: `loopcoder.model_capability.v1`.

ModelCapability records one model entry inside one catalog snapshot.

| Field | Required | Meaning |
| --- | --- | --- |
| `model_capability_id` | yes | Stable entry identity within the snapshot. |
| `model_catalog_snapshot_id` | yes | Owning snapshot. |
| `adapter_id` | yes | Provider adapter key. |
| `canonical_model_id` | yes | Provider model ID normalized for exact matching within the provider. |
| `display_name` | no | Human model label from provider or overlay. |
| `aliases` | yes | Ordered aliases with source and confidence for each alias. |
| `lifecycle_state` | yes | `available`, `renamed`, `deprecated`, or `removed`. |
| `replacement_model_id` | no | Replacement target when lifecycle is `renamed` or `deprecated` and known. |
| `availability_state` | yes | `available`, `account-restricted`, `temporarily-unavailable`, `unknown`, or `removed`. |
| `roles_supported` | yes | Set of role labels the model may satisfy by inventory evidence: `worker`, `verifier`, `audit-review`, or `nested-subagents`; empty means unknown. |
| `read_only` | yes | `true`, `false`, or `unknown`. |
| `json_output` | yes | `true`, `false`, or `unknown`. |
| `nested_subagents` | yes | `true`, `false`, or `unknown`. |
| `mcp_config` | yes | `true`, `false`, or `unknown`. |
| `cancellation` | yes | `true`, `false`, or `unknown`. |
| `token_usage_reporting` | yes | `true`, `false`, or `unknown`. |
| `context_window_tokens` | no | Exact or estimated context window with confidence and source. |
| `tool_support` | no | Bounded tool capability facts with confidence. |
| `image_input` / `image_output` | no | `true`, `false`, or `unknown` with source. |
| `constraints` | yes | Bounded provider or overlay constraints. |
| `entry_sources` | yes | All sources that contributed to this entry. |
| `conflicts` | yes | Preserved source conflicts and precedence result. |

The capability dimension names `read_only`, `nested_subagents`,
`json_output`, `mcp_config`, `cancellation`, and `token_usage_reporting` align
with [`../reference/runtime-capabilities.md`](../reference/runtime-capabilities.md)
and the `provider_compatibility[]` matrix in `doctor --format json`. A field
with value `unknown` MUST NOT satisfy a hard requirement for that capability.

## Bounded Adapter Probes

Implementation issue #725 owns the first code implementation of this section.

### Discovery Sources

Provider CLI discovery MUST inspect only:

- executable names declared by the adapter and resolved through PATH lookup;
- explicitly configured executable paths from validated LoopCoder config;
- adapter-declared fixed default locations, only when the declaration lists
  the exact path pattern and platform;
- fixture paths supplied by tests.

Discovery MUST NOT scan an entire disk, enumerate arbitrary process names,
infer providers from unrelated files, mutate repositories, install packages,
run provider login, or open browsers.

### Probe Command Allowlist

Each adapter declaration MUST define an allowlist of probe commands. A probe
command declaration contains:

| Field | Meaning |
| --- | --- |
| `probe_command_id` | Stable adapter-local key. |
| `probe_kind` | `install`, `version`, `auth-readiness`, `catalog`, or `health`. |
| `executable_source` | `resolved-installation` or exact declared executable. |
| `argv_template` | Fixed argv array with only declared placeholders, such as executable path. |
| `working_directory_class` | `empty-temp`, `loopcoder-home`, `project-root-readonly`, or `none`. |
| `environment_allowlist` | Exact env var names that may be passed by name; values must not be persisted. |
| `may_network` | Boolean declaration of possible network behavior. |
| `requires_network_permission` | Boolean policy gate. |
| `timeout_ms_max` | Hard maximum. |
| `stdout_limit_bytes` / `stderr_limit_bytes` | Per-stream caps. |
| `combined_output_limit_bytes` | Combined cap. |
| `output_schema` | Parser schema version or `plain-summary-only`. |
| `secret_policy` | Classification and redaction policy. |

Probe runners MUST execute argv arrays directly, without shell interpolation.
Provider names, model names, profile names, paths, and issue titles are data.
They MUST NOT alter the executable or argv shape.

### Execution Bounds

The following bounds are mandatory minimum constraints for v0.8.0:

- Install and version probes MUST have `timeout_ms <= 5000`.
- Auth-readiness probes MUST have `timeout_ms <= 10000`.
- Catalog probes MUST have `timeout_ms <= 15000` unless network permission is
  explicitly granted, in which case the adapter may declare `timeout_ms <=
  30000`.
- Each stdout and stderr stream MUST have a cap no larger than 65536 bytes.
- Combined captured output MUST have a cap no larger than 131072 bytes.
- Decoded JSON payloads MUST have a maximum byte size and nesting depth; the
  initial v0.8.0 maximum nesting depth is 32.
- Process groups MUST be cleaned up on timeout or cancellation where the
  platform supports it.
- Retry is disabled by default. Any retry must be adapter-declared,
  idempotent, bounded to one retry, and must write a separate ProbeResult.

Adapters may declare stricter bounds. Implementations MUST reject adapter
declarations that exceed these ceilings unless a later accepted spec changes
the bound.

### Network Behavior

Install probes MUST be local-only and MUST NOT require network. Version probes
SHOULD be local-only. Auth-readiness and catalog probes are local-only unless
the adapter declaration marks the command as possibly networked and policy or
user input grants network permission for that provider, purpose, and scope.

When network permission is denied, the probe MUST either skip the command and
record `unknown` with `gap_reasons: ["network-permission-denied"]`, or use a
declared local-only fallback. It MUST NOT run a possibly networked command and
then claim it was local.

### Probe Outcomes

Install probes use exactly these outcomes:

| Outcome | Meaning |
| --- | --- |
| `installed` | A declared executable resolved and the required local inspection succeeded within bounds. |
| `not-installed` | No declared executable or configured path was found for that scan source. |
| `installed-but-unusable` | An executable was found, but local inspection proves it cannot satisfy even basic adapter invocation, such as unsupported platform, incompatible version, or non-executable file. |
| `probe-failed` | The probe timed out, crashed, emitted malformed required data, exceeded bounds, or failed for another typed reason. |

`installed` is only installation evidence. It MUST NOT set auth readiness to
`ready`, catalog availability to `available`, quota to known, or invocation
authorization to true.

## Auth Readiness Without Credential Access

Implementation issue #726 owns the first code implementation of this section.

### Credential Isolation

Auth readiness detection MUST follow
[`0742-v080-security-threat-model.md`](0742-v080-security-threat-model.md).
The implementation MUST NOT:

- read, copy, hash, parse, migrate, export, or serialize provider credential
  file contents;
- read, copy, hash, parse, migrate, export, or serialize API keys, OAuth
  tokens, refresh tokens, cookies, SSH private keys, OS keychain entries, or
  browser session data;
- log environment variable values or persist provider auth file bytes;
- automate login, credential refresh, browser consent, or private UI scraping;
- pass credential material to child agents, fixtures, reporter records, PR
  bodies, issue comments, commits, crash dumps, or docs.

Redaction is not permission to read secrets. The safe design is to avoid
reading credential material in the first place.

### Acceptable Evidence

AuthReadiness may use only the following evidence kinds:

| Evidence kind | Allowed evidence | Forbidden evidence |
| --- | --- | --- |
| `exit-code` | Exit code from an adapter-declared readiness command. | Raw credential-bearing output. |
| `sanctioned-status-command` | Provider-supported auth/status command declared by adapter. | Login, refresh, browser, or hidden/private commands. |
| `machine-readable-status` | Declared non-secret JSON/status fields from provider CLI. | Unexpected fields that contain secrets or unbounded provider text. |
| `file-existence` | Existence, type, owner/mode diagnostic, or mtime of an adapter-declared auth path. | File bytes, parsed JSON, token cache contents, keychain values. |
| `environment-name-existence` | Whether a named variable exists, if the adapter declares the variable name as a secret reference. | Environment variable values. |
| `provider-error-class` | Stable error class such as unauthenticated or expired from bounded command result. | Raw bearer tokens, cookies, credential paths beyond redacted local diagnostics. |
| `unsupported` | Adapter declares no safe readiness check. | Guessing readiness from installation. |

If an adapter cannot perform a credential-blind readiness check, it MUST record
`readiness_state: "unknown"` with an `unsupported_reason`. It MUST NOT infer
readiness from installation, model registry membership, or a prior successful
provider invocation unless that invocation produced a non-secret readiness
receipt within the current freshness window.

### Readiness States

Readiness states are exhaustive:

| State | Meaning |
| --- | --- |
| `ready` | Supported readiness evidence indicates the profile or installation is currently authenticated for at least the readiness check itself. |
| `not-authenticated` | Supported evidence indicates login or credentials are absent. |
| `expired` | Supported evidence indicates credentials existed but are expired, revoked, or require refresh. |
| `unknown` | Evidence is unavailable, unsupported, stale, ambiguous, inaccessible, failed, or not safe to collect. |

`unknown` is first-class. It is not a synonym for false and not a soft success.
Policies may decide whether unknown evidence blocks a later operation, but the
inventory layer MUST preserve `unknown` exactly.

### Multi-Profile Representation

Each profile discovered safely becomes an AccountProfile. Each readiness
capture references one AccountProfile when known. If a provider exposes a
default profile plus named profiles, the default and each named profile are
separate rows. If the provider supports tenant, organization, project, or
workspace contexts, the adapter records non-secret reference hashes and redacted
labels rather than raw credential-bearing config.

Partially authorized accounts are represented by `authorization_scope_state:
"partial"` and a bounded non-secret summary. They MUST NOT be rendered as
globally ready.

## Model Capability Catalog

Implementation issue #727 owns the first code implementation of this section.

### Catalog Sources

Catalog snapshots may come from:

- adapter-declared static metadata, including the current static model registry
  defaults from [`0554-model-depth-selection.md`](0554-model-depth-selection.md);
- provider-supported machine-readable model listing commands or APIs exposed
  through provider CLIs;
- configured overlays shipped with LoopCoder or explicitly configured by the
  operator;
- deterministic fixture providers used by tests;
- migration metadata that preserves previously known catalog facts as stale.

Every snapshot and every entry MUST preserve source provenance. When sources
conflict, the implementation MUST record all contributing sources, the
conflict, the chosen precedence rule, and confidence. It MUST NOT silently
merge incompatible facts.

### Lifecycle

ModelCapability `lifecycle_state` values are:

| State | Meaning |
| --- | --- |
| `available` | Source indicates the model ID is current. |
| `renamed` | Source indicates the model ID has a new canonical name. |
| `deprecated` | Source indicates the model is still callable or visible but discouraged or scheduled for removal. |
| `removed` | Source indicates the model is no longer available in that catalog context, or a prior known model is absent in a fresh authoritative snapshot. |

Removed entries remain in historical snapshots. A later fresh snapshot may
record the same canonical model ID as removed while keeping older available
snapshots for audit and replay.

### Capability Dimensions

The catalog must model the existing provider compatibility vocabulary and
future model-specific dimensions:

| Dimension | Values | Notes |
| --- | --- | --- |
| `roles_supported` | array | Inventory evidence for `worker`, `verifier`, `audit-review`, or `nested-subagents`; routing policy may add stricter role envelopes later. |
| `read_only` | true / false / unknown | Aligns with verifier and audit read-only requirements. |
| `json_output` | true / false / unknown | Aligns with schema-enforced verifier output. |
| `nested_subagents` | true / false / unknown | Inventory fact only; 0805 owns federation behavior. |
| `mcp_config` | true / false / unknown | Aligns with MCP configuration injection support. |
| `cancellation` | true / false / unknown | Means local supervision/cancellation support, not provider-side exactly-once cancellation. |
| `token_usage_reporting` | true / false / unknown | Inventory fact only; 0803 owns usage math. |
| `context_window_tokens` | exact / estimated / unknown | Numeric value plus confidence and source when known. |
| `tool_support` | structured / unknown | Bounded provider-declared tool dimensions. |
| `image_input` / `image_output` | true / false / unknown | Capability fact with source and confidence. |

Unknown or stale capability fields MUST NOT satisfy hard requirements. A stale
catalog may be shown and may support operator diagnosis, but policy that
requires fresh catalog evidence must reject it or require an explicit override
in a later phase.

### Stale Catalog Handling

Catalog snapshots are immutable. Refresh creates a new snapshot and links it to
prior snapshots through source metadata or replacement fields. When a snapshot
is beyond `stale_after`, output MUST mark it stale, and any derived
ModelCapability confidence MUST become `stale` unless a fresher snapshot
supersedes it.

If refresh partially fails, the old snapshot remains available as stale
history, and the failed refresh writes a ProbeResult plus explicit gaps. The
implementation MUST NOT delete the older snapshot or rewrite it to look fresh.

## Future-Provider Adapter Contract

Implementation issue #728 owns the first code implementation of this section.

### Adapter Declaration

Schema version: `loopcoder.adapter_declaration.v1`.

An adapter declaration is versioned metadata compiled into LoopCoder or loaded
from a trusted LoopCoder-owned configuration surface. It is not untrusted binary
plugin code. It contains:

| Field | Required | Meaning |
| --- | --- | --- |
| `adapter_id` | yes | Stable provider key used by commands and records. |
| `adapter_version` | yes | Adapter contract version. |
| `declaration_schema_version` | yes | Schema version, initially `loopcoder.adapter_declaration.v1`. |
| `display_name` / `vendor` | yes | Human labels. |
| `executable_names` | yes | Declared executable names and platforms. |
| `probe_commands` | yes | Allowlisted bounded commands from this spec. |
| `auth_readiness_contract` | yes | Supported evidence kinds and readiness parser schemas. |
| `catalog_contract` | yes | Supported catalog sources and model entry parser schemas. |
| `invocation_profile` | yes | Provider launch argv/config shape, permission modes, output modes, and cancellation support. |
| `capability_defaults` | yes | Runtime capability defaults aligned with `runtime-capabilities.md`. |
| `classification_rules` | yes | Field classifications and redaction rules. |
| `network_declarations` | yes | Commands that may network and required permission scopes. |
| `unsupported_operations` | yes | Unsupported capabilities with typed reason codes and suggestions. |
| `conformance_version` | yes | Conformance suite version the adapter passes. |

Adapter declaration changes are versioned. A breaking adapter declaration
change requires a new `adapter_version`, migration handling for existing
records, and fixture coverage for old records failing closed or migrating
conservatively.

### Scheduler-Core Independence

The planner, router, and scheduler core MUST depend on provider-neutral
inventory and capability interfaces, not provider-specific branches. Adding a
fourth provider must require only:

- a new adapter declaration;
- provider-specific parser/runner code behind the adapter boundary when needed;
- model catalog fixtures and conformance tests;
- documentation for operator setup and adapter author obligations.

It MUST NOT require edits to scheduler algorithms, task lifecycle transitions,
DeliveryRun approval semantics, or routing policy core to recognize the new
provider by name.

### Required Interfaces

Every production adapter must implement or explicitly mark unsupported:

| Interface | Required behavior |
| --- | --- |
| `Probe(ctx, request)` | Bounded install/version/health probes returning ProbeResult and ProviderInstallation records. |
| `AuthReadiness(ctx, request)` | Credential-blind readiness capture returning AccountProfile and AuthReadiness records or unknown with reason. |
| `Catalog(ctx, request)` | Snapshot capture returning ModelCatalogSnapshot and ModelCapability records or stale/unknown gaps. |
| `InvocationProfile()` | Provider-neutral launch capabilities: permissions, read-only support, JSON output, MCP config, cancellation, reporter normalization, and idempotency evidence support. |
| `ClassifyAndRedact(output)` | Classification and redaction for provider stdout/stderr before persistence or display. |
| `NormalizeErrors(result)` | Typed error and gap reason mapping for timeouts, malformed output, auth failures, unsupported capabilities, and schema mismatches. |

Unsupported operations are valid only when explicit. They must return
`unknown`, `unavailable`, or a typed unsupported capability result; they must
not fake support.

### Conformance Tests

The conformance suite must include a fixture provider and cover:

- absent executable, duplicate PATH entries, aliases, shims, spaces, symlinks,
  multiple versions, broken executables, hung commands, and platform path
  behavior;
- installed but unauthenticated, expired auth, multiple profiles, inaccessible
  config, headless session, partially authorized account, and unsupported auth
  readiness;
- dynamic catalog addition, rename, alias, deprecation, removal,
  account-restricted model, stale snapshot, conflicting source, malformed
  schema, and unknown capability fields;
- partial implementation, unsupported operations, timeout, cancellation,
  malformed output, schema mismatch, redaction, secret-like output rejection,
  and adapter declaration version upgrades;
- proof that the fixture provider can be registered without scheduler-core
  edits.

## Failure Honesty Rules

These rules are normative and testable:

- Installed does not mean authenticated.
- Authenticated does not mean authorized for every model.
- A visible model does not mean quota is available.
- A catalog entry does not mean the provider can run in read-only mode.
- A provider that supports cancellation locally does not prove remote
  provider-side cancellation or exactly-once execution.
- `unknown` MUST remain `unknown` until a supported source proves a narrower
  state.
- `unavailable` MUST remain distinct from `unknown`: unavailable means a
  supported source proved absence or unsupported status; unknown means evidence
  is missing, unsafe, stale, or ambiguous.
- `stale` MUST remain distinct from fresh confidence. Stale exact evidence is
  stale, not exact.
- Partially failing probes MUST yield partial inventory with explicit
  `gap_reasons`; they MUST NOT discard successful independent records.
- Human and JSON output MUST show confidence, freshness, and gaps for provider
  inventory facts used by doctor, status, planning, or later routing.

## Storage And JSON Output

### Machine-Local Storage

Inventory records live in the machine-local SQLite store under the global
project storage model from
[`0639-global-data-layout-project-identity.md`](0639-global-data-layout-project-identity.md).
The initial v0.8 schema additions are logical tables equivalent to:

- `adapter_declarations`;
- `provider_installations`;
- `provider_probe_results`;
- `account_profiles`;
- `auth_readiness`;
- `model_catalog_snapshots`;
- `model_capabilities`;
- `inventory_events` for tamper-evident authority events when inventory affects
  DeliveryRun inputs.

The v0.8 migration must follow the 0801 migration conventions: backup metadata
before migration, one immediate SQLite write transaction for schema changes,
idempotent re-run, conservative handling of ambiguous legacy state, no
repository mutation, no credential migration, no provider launch, and
machine-local backup paths only.

### Doctor JSON

`loopcoder doctor --format json` must expose provider inventory as an additive
root object named `provider_inventory`. Existing fields such as
`provider_compatibility[]`, `host_profile`, `runtime`, and `checks[]` remain
valid. The new object contains redacted, bounded, machine-readable inventory:

```json
{
  "provider_inventory": {
    "schema_version": "loopcoder.provider_inventory_json.v1",
    "generated_at": "2026-07-12T00:00:00Z",
    "inventory_fingerprint": "sha256:...",
    "confidence": "unknown",
    "installations": [],
    "probe_results": [],
    "account_profiles": [],
    "auth_readiness": [],
    "model_catalog_snapshots": [],
    "model_capabilities": [],
    "gap_reasons": []
  }
}
```

Doctor JSON MUST distinguish:

- configured provider missing executable;
- installed provider not authenticated;
- authenticated profile with unknown model authorization;
- stale catalog;
- partially failing probe with retained partial records;
- unsupported provider capability from the compatibility matrix.

Raw absolute paths, account labels, and provider output are local diagnostics
and must be redacted by default. Secret material has no valid JSON form.

### Status JSON

`loopcoder status --format json` must expose inventory references only when
they affect a DeliveryRun, Task, attempt, or future route. It MUST NOT duplicate
full raw inventory by default. The additive shape is:

```json
{
  "inventory_refs": {
    "schema_version": "loopcoder.inventory_refs.v1",
    "inventory_fingerprint": "sha256:...",
    "provider_installation_ids": [],
    "account_profile_ids": [],
    "auth_readiness_ids": [],
    "model_catalog_snapshot_ids": [],
    "model_capability_ids": [],
    "confidence": "unknown",
    "gap_reasons": []
  }
}
```

When status needs to explain a blocked or needs-human state, it may include
bounded redacted summaries of referenced records. It must not include credential
material, raw provider output, unredacted sensitive paths, or PR-unsafe local
diagnostics.

## Implementation Acceptance Mapping

### #725 Installed Provider CLI Discovery

Issue #725 is complete only when its code and tests implement the sections
"Shared Representation", "ProviderInstallation", "ProbeResult", and "Bounded
Adapter Probes":

- strict timeout, output, argv, environment, network, process cleanup, and
  no-shell bounds are enforced by unit and integration tests;
- absent executable, duplicate PATH entries, aliases, shims, spaces, symlinks,
  multiple versions, hung or broken executables, and Windows/macOS/Linux path
  behavior are fixture-tested;
- human and JSON output show provenance, freshness, confidence, and
  installation state without calling installed software usable capacity;
- refresh creates new ProbeResult history and preserves stale installations
  rather than silently rewriting evidence.

### #726 Account/Profile Auth Readiness

Issue #726 is complete only when its code and tests implement the sections
"AccountProfile", "AuthReadiness", and "Auth Readiness Without Credential
Access":

- installed-but-logged-out, expired auth, multiple profiles, inaccessible
  config, headless session, partially authorized account, and unsupported
  readiness are covered;
- logs, reporter output, SQLite, crash dumps, and JSON fixtures reject or
  redact secret-like material and never contain credential bytes or environment
  values;
- readiness probes are read-only, bounded, and use sanctioned provider
  mechanisms when available;
- unsupported providers return `unknown` with an explicit reason;
- account/profile selection is collision-safe and can be pinned by user policy
  through opaque IDs, not display labels alone.

### #727 Dynamic Model Capability Catalog

Issue #727 is complete only when its code and tests implement the sections
"ModelCatalogSnapshot", "ModelCapability", and "Model Capability Catalog":

- new, renamed, aliased, removed, deprecated, account-restricted, temporarily
  unavailable, stale, and unknown models are fixture-tested without requiring a
  LoopCoder code release for every provider-side catalog change;
- conflicts preserve every source and apply documented precedence/confidence;
- stale or unknown capability fields cannot satisfy hard requirements;
- snapshots are durable, immutable, freshness-marked, and referenced exactly by
  later decisions that use them.

### #728 Future-Provider Adapter Contract

Issue #728 is complete only when its code and tests implement the section
"Future-Provider Adapter Contract":

- a fourth fixture provider registers through adapter wiring and inventory
  declarations without scheduler-core algorithm edits;
- adapters receive least-privilege typed inputs and cannot access unrelated
  project/global state through the provider-neutral interfaces;
- conformance tests cover partial implementation, malformed output, timeouts,
  schema mismatch, cancellation, redaction, unsupported operations, and version
  upgrades;
- adapter author documentation explains declaration fields, security
  obligations, backward compatibility, and example implementations.

## Relationship To Existing Specs And Docs

- [`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md) defines
  the shared durable-record conventions, confidence enum, fingerprints,
  side-effect classes, typed errors, provenance, and v0.8 migration posture.
  This spec reuses those conventions for provider inventory.
- [`0742-v080-security-threat-model.md`](0742-v080-security-threat-model.md)
  defines credential isolation, provider output classification, local-only
  discovery by default, command bounds, network opt-in, and unknown-preserving
  security requirements. This spec makes those controls concrete for #725
  through #728.
- [`0639-global-data-layout-project-identity.md`](0639-global-data-layout-project-identity.md)
  defines machine-local global storage and project identity. Inventory state is
  stored there and uses `project_id` only for project-scoped facts.
- [`0554-model-depth-selection.md`](0554-model-depth-selection.md) defines the
  current static model/depth registry. This spec treats that registry as one
  catalog source, not as the entire dynamic catalog.
- [`../reference/runtime-capabilities.md`](../reference/runtime-capabilities.md)
  defines the current provider compatibility matrix and capability vocabulary.
  ModelCapability fields align with that vocabulary and extend it with
  per-model provenance and freshness.
