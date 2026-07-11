---
id: 742
title: v0.8.0 Security Threat Model
status: draft
date: 2026-07-11
issue: 742
pr: null
supersedes: []
superseded_by: []
---

# v0.8.0 Security Threat Model

This documentation-only spec freezes the v0.8.0 security threat model and
control requirements for provider discovery, quota telemetry, routing, and
agent federation. It covers the surfaces introduced by
[`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md) and the
v0.8.0 child work under roadmap
[#714](https://github.com/jasonhnd/loopcoder/issues/714) and epic
[#720](https://github.com/jasonhnd/loopcoder/issues/720).

This PR adds no Go code, SQLite migration, CLI behavior, workflow change, or
provider integration. The hard start gate from #714 and #742 still applies:
until v0.8.0 implementation is authorized, this document permits only design
and test-spec work. All runtime controls below are deferred to follow-up
implementation issues and must merge separately per [`../PROCESS.md`](../PROCESS.md).

## Goals

- Define trust boundaries, assets, attacker capabilities, abuse cases, data
  flows, and mitigations for adapters, probes, provider outputs, prompts,
  SQLite, logs, worktrees, native sessions, hosts, DeliveryRun records, quota
  telemetry, routing, and provider-native sub-agents.
- Require schema-level classification and redaction so secret material cannot
  be persisted, rendered, replayed, included in reporter output, or copied into
  fixtures by accident.
- Require bounded command, path, network, time, output, storage, and resource
  behavior for provider discovery, quota telemetry, routing, and agent
  federation.
- Bind security-sensitive side effects to typed approvals that match the
  current scope, policy, and plan fingerprint from
  [`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md).
- Define test fixtures and verification evidence that prove credential
  isolation, least privilege, conservative unknown handling, and cross-project
  isolation before v0.8.0 GO.

## Non-Goals

- No implementation in this issue. Schema, command, probe, router, scheduler,
  and federation code belongs in the specific follow-up issues listed below.
- No claim that instructions, prompts, provider policies, or LoopCoder checks
  are equivalent to an OS sandbox, container boundary, hypervisor, provider-side
  access control, or endpoint security product.
- No credential reading, copying, hashing, parsing, exporting, or migration.
- No private provider UI scraping, browser automation against authenticated
  provider pages, reverse engineering of hidden quota endpoints, or inference of
  exact quota from private account pages.
- No guarantee of exactly-once external side effects when a provider or network
  does not expose an idempotent receipt. LoopCoder records evidence and fails
  closed when evidence is incomplete.
- No hosted control plane, team billing model, cross-machine secret store, or
  cloud coordination service.

## Dependency Map

This spec is the security prerequisite for the implementation issues below.
Each issue must reference this document and the more specific functional spec
or issue it implements.

| Area | Issues | Security dependency |
| --- | --- | --- |
| Provider inventory | [#725](https://github.com/jasonhnd/loopcoder/issues/725), [#726](https://github.com/jasonhnd/loopcoder/issues/726), [#727](https://github.com/jasonhnd/loopcoder/issues/727), [#728](https://github.com/jasonhnd/loopcoder/issues/728) | Probes must be bounded, local by default, credential-blind, classification-aware, and provenance-recorded. |
| Quota, usage, budget, availability | [#729](https://github.com/jasonhnd/loopcoder/issues/729), [#730](https://github.com/jasonhnd/loopcoder/issues/730), [#731](https://github.com/jasonhnd/loopcoder/issues/731), [#732](https://github.com/jasonhnd/loopcoder/issues/732) | Quota telemetry must preserve `unknown`, avoid private UI scraping, store no secrets, and fail closed on stale or unauthoritative evidence. |
| Planning and routing | [#733](https://github.com/jasonhnd/loopcoder/issues/733), [#734](https://github.com/jasonhnd/loopcoder/issues/734), [#735](https://github.com/jasonhnd/loopcoder/issues/735), [#736](https://github.com/jasonhnd/loopcoder/issues/736), [#737](https://github.com/jasonhnd/loopcoder/issues/737) | Policy eligibility, side-effect classes, approval freshness, quota confidence, and rejected reasons must gate routing before heuristic ranking. |
| Agent federation | [#738](https://github.com/jasonhnd/loopcoder/issues/738), [#740](https://github.com/jasonhnd/loopcoder/issues/740), [#739](https://github.com/jasonhnd/loopcoder/issues/739) | Provider-native sub-agents must stay inside LoopCoder scopes, budgets, claims, approvals, one-writer isolation, cancellation, and recovery. |
| Observability, evaluation, release | [#741](https://github.com/jasonhnd/loopcoder/issues/741), [#743](https://github.com/jasonhnd/loopcoder/issues/743), [#744](https://github.com/jasonhnd/loopcoder/issues/744) | Human and JSON output, simulations, migration, self-bootstrap, and release gates must prove the security controls before v0.8.0 GO. |

## Trust Boundaries

LoopCoder is a local, single-operator orchestration CLI. The operator chooses
the repository, installed tools, provider CLIs, shell, network, and host. That
does not make all data safe. v0.8.0 must preserve these boundaries:

| Boundary | Trusted side | Untrusted or constrained side | Required stance |
| --- | --- | --- | --- |
| Operator intent and approvals | Explicit user input, typed approvals, configured provider/model pins, and risk decisions. | Provider output, worker output, repository files, generated plans, and inferred defaults. | User authority must be recorded; inferred data must not become approval. |
| Deterministic policy | Built-in policy code, accepted specs, `.delivery.yml` values after validation. | Planner/router heuristics and provider-native suggestions. | Policy eligibility is a gate before ranking or fallback. |
| Provider adapters | Adapter code shipped by LoopCoder and operator-installed provider CLIs as executable tools. | CLI stdout/stderr, model output, account names, profiles, and provider-side error text. | Treat output as untrusted data, parse only bounded schemas, and redact before persistence. |
| Credentials | Provider credential files, tokens, cookies, SSH keys, API keys, environment variable values, OS keychains. | Everything LoopCoder persists, reports, or sends to child agents. | Credential material is out of scope for reads and forbidden in durable records. |
| Project identity | `project_id` from [`0639-global-data-layout-project-identity.md`](0639-global-data-layout-project-identity.md). | Folder names, display names, branch names, issue titles, and provider output. | Cross-project references fail closed. |
| Worktrees | The selected project root and explicitly configured scratch roots. | PR contents, generated files, symlinks, build outputs, ignored directories, and nested repositories. | Paths are root-relative, canonicalized, and symlink-confined. |
| SQLite and local state | LoopCoder-owned global runtime state under the local home directory. | Repository-tracked files, provider logs, imported legacy state, manual edits, corrupt rows. | Runtime state remains local-only and validated before authority decisions. |
| Reporter and logs | Local-only report records and bounded diagnostics. | PR bodies, issue comments, commits, release notes, fixtures, and tracked docs. | Local-only data is referenced by ID or summary only when safe. |
| Host sessions | Codex, Claude Code, Gemini, Paseo, or another host as launch context. | Host transcript text, host-provided summaries, native session files, and UI state. | Host metadata is diagnostic, not authority. No private UI scraping. |

## Assets

Security controls protect these assets:

- credential material: API keys, OAuth tokens, refresh tokens, cookies, SSH and
  private keys, provider auth files, OS keychain data, and environment variable
  values;
- local privacy data: prompts, provider outputs, worktree paths, repository
  names, account/profile labels, model names, quota snapshots, usage estimates,
  local logs, native session IDs, and host process metadata;
- authority data: DeliveryRun records, Task records, dependency edges, Attempts,
  Decisions, Approvals, Overrides, plan fingerprints, policy fingerprints,
  authorization fingerprints, execution claims, idempotency keys, and provider
  receipts;
- project isolation data: project identity, checkout identity, remote identity,
  cross-project references, and imported legacy state;
- release and review evidence: security review findings, audit results,
  adversarial fixtures, migration fixtures, JSON contracts, and release GO
  evidence.

## Attacker Capabilities

The threat model assumes attackers may control or influence:

- repository contents, PR contents, generated files, symlinks, filenames, test
  output, build scripts, and malicious Markdown or JSON fixtures;
- provider stdout/stderr, model text, tool-call suggestions, nested sub-agent
  messages, and adversarial prompt-injection content;
- local run state that is stale, truncated, partially written, replayed,
  manually edited, or migrated from older versions;
- environment variable names and values visible to the LoopCoder process;
- account/profile display names, provider model names, rate-limit messages, and
  quota responses returned by provider CLIs;
- a shared-host observer who can read overly permissive files, process
  arguments, temporary files, logs, or crash artifacts.

The model does not assume LoopCoder can defend against:

- a malicious local administrator or kernel;
- an operator intentionally running unsafe commands with full shell authority;
- provider-side compromise;
- a provider CLI that itself exfiltrates secrets after the operator installed
  and ran it;
- untrusted build code that the operator explicitly chooses to execute outside
  LoopCoder's documented bounds.

## Data Classification

Every persisted, rendered, logged, or JSON-serialized field introduced by
v0.8.0 must declare one classification:

| Classification | Meaning | Persistence rule |
| --- | --- | --- |
| `public` | Safe to copy into PR bodies, issue comments, tracked docs, and release notes. | Allowed everywhere. |
| `local-diagnostic` | Local state useful for diagnosis but not repository-visible. | Allowed in SQLite, local JSON, and logs; forbidden in PR/comment/commit surfaces unless intentionally summarized. |
| `sensitive-path` | Machine-local paths, credential-file paths, home-relative paths, native session paths, or scratch paths. | Store only when needed for local operation; render as basename, project-relative path, or redacted form by default. |
| `provider-output-untrusted` | Raw or parsed provider stdout/stderr, model output, quota messages, and nested agent messages. | Must be size-bounded, marked untrusted, and never used as authority without validation. |
| `secret-reference` | A name or handle that identifies where a secret may exist, such as an environment variable name or provider profile ID. | May store the reference only if it is not itself a secret and is needed for routing or diagnostics. |
| `secret-material` | Token, cookie, private key, credential file contents, auth JSON contents, environment variable value, session cookie, or comparable opaque secret. | Forbidden in all LoopCoder schemas, events, reports, logs, fixtures, and prompts. |

Fields with `secret-material` content have no valid serialized form. The
implementation must reject them before persistence and before reporter/event
emission. Redaction is not a license to read credentials: probes must avoid
reading credential material in the first place.

## Schema And Serialization Controls

All v0.8.0 schemas that persist or render provider, quota, routing, or agent
data must implement these controls:

- Field allowlists: every JSON field is declared with a classification and a
  maximum encoded size.
- Secret-forbidden types: opaque provider output and environment data must not
  be assignable to plain durable string fields without an explicit classifier.
- Redaction at source: any field classified `local-diagnostic`,
  `sensitive-path`, `provider-output-untrusted`, or `secret-reference` renders
  differently for local JSON, human output, PR-safe summaries, and fixtures.
- No raw environment snapshots: environment variable names may be used only as
  `secret-reference` evidence; values are never stored or echoed.
- No raw auth-file snapshots: auth file existence, type, owner/mode diagnostic,
  or provider CLI readiness may be recorded; file bytes and parsed credential
  objects are forbidden.
- No accidental stringification: secret-aware or untrusted-output wrapper types
  must not implement display behavior that reveals raw values.
- Canonical JSON fixtures must include classification metadata or be generated
  from schema definitions that prove every field has a classification.
- Unknown classification or unknown enum values fail closed with the typed
  errors from [`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md),
  not with best-effort serialization.

## DeliveryRun Authority Controls

The records from [`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md)
are security boundaries, not just workflow bookkeeping.

- `input_fingerprint` must include provider inventory evidence, quota
  confidence, routing-eligible candidates, agent-federation capabilities, and
  any host capability requests that affect authority.
- `policy_fingerprint` must include permission, scope, side-effect class,
  budget, quota-confidence, routing, network-opt-in, retention, and
  federation-isolation rules.
- `plan_fingerprint` must include every task scope, route requirement,
  provider launch requirement, nested-agent delegation, expected output class,
  and required verification gate.
- `authorization_fingerprint` binds the input, policy, plan, maximum
  side-effect class, and approved scope. A changed provider, quota confidence,
  route, sub-agent tree, budget, or task scope invalidates old approval.
- Approvals and overrides must carry `approval_kind`, `approved_scope`,
  `approved_side_effect_class`, `authorization_fingerprint`, `expires_at`,
  actor provenance, and policy version.
- Security-sensitive side effects require typed approval when policy says they
  are not automatically allowed. These include provider launch, external
  network probe, git remote write, GitHub write, repo write, native sub-agent
  launch, budget override, stale quota override, and cross-run recovery.
- Child agents never inherit global authority by being descendants. They receive
  only an explicit task scope, budget reservation, side-effect class, claim, and
  cancellation contract.

## Provider Discovery Controls

Provider discovery covers installed provider CLIs, usable accounts/profiles,
model catalogs, capabilities, and authentication readiness.

Normative requirements for #725 through #728:

- Discovery is local-only by default. Network-backed discovery requires explicit
  opt-in for the provider, action, and scope.
- Probes must use fixed argv arrays, bounded environment allowlists, bounded
  working directories, hard timeouts, output byte limits, and process cleanup.
- Probes must not run provider login, refresh credentials, open private UIs,
  scrape authenticated browser pages, copy auth files, parse credential files,
  or echo environment variable values.
- Installing a CLI is not proof of a usable account, usable model, quota, or
  permission. The recorded state must preserve `unknown`, `unavailable`, or
  `stale` when evidence is incomplete.
- Account and profile identifiers are local diagnostics unless the provider
  contract says they are public. They must be redacted in PR-safe output.
- Model catalogs must record source, capture time, provider CLI version,
  command schema version, confidence, and rejected/unsupported fields.
- Future-provider adapters must declare probe surfaces, network behavior,
  output schemas, classification rules, timeout bounds, and unsupported
  guarantees before they can participate in routing.

## Quota, Usage, Budget, And Availability Controls

Quota telemetry must be honest about evidence.

Normative requirements for #729 through #732:

- Quota data may be `exact`, `estimated`, `unknown`, `unavailable`, or `stale`.
  Implementations must not convert unknown or stale quota into false certainty.
- Exact quota can come only from a documented, supported, machine-readable
  provider source or a deterministic local ledger for LoopCoder-owned usage.
- Estimated quota must name its estimator, inputs, freshness, and error bound
  if known. It must be ineligible for policies that require exact quota.
- Local usage ledgers must store usage facts and reservations, not provider
  secrets or raw prompts. Reconciliation must be idempotent and project-scoped.
- Budget reservations must be atomic, hierarchical, and released or committed
  through typed state transitions. Child agents cannot overrun parent budgets.
- Availability and circuit breakers must fail closed when quota evidence is
  stale, rate-limit state is ambiguous, provider health is unknown, or an
  idempotent launch receipt is missing.
- Human and JSON output must show confidence and rejected reasons; it must not
  hide quota uncertainty behind a green route.

## Routing Controls

Routing chooses among policy-eligible provider/model candidates. It does not
grant eligibility.

Normative requirements for #733 through #737:

- The planner must classify task requirements, side effects, data sensitivity,
  expected output class, verification needs, and risk before routing.
- The router must reject candidates that fail permission, side-effect, model
  capability, data-sensitivity, quota-confidence, freshness, budget, network,
  or policy requirements before scoring remaining candidates.
- Rejected candidates must record typed reasons without leaking credentials or
  raw provider output.
- Heuristic ranking must be explicitly marked as heuristic and subordinate to
  deterministic policy. It cannot bypass a policy rejection.
- Provider/model pins by the user are authority inputs, but pins still fail
  closed if the pinned candidate violates policy or lacks required evidence.
- Fallback and re-planning must keep the original authorization fingerprint
  valid. If fallback changes provider, model, scope, side-effect class, budget,
  task graph, or child-agent plan in a consequential way, fresh approval is
  required.
- Independent verification must remain eligible even when the worker provider
  changes. Verifier routing must preserve read-only boundaries.

## Agent Federation Controls

Agent federation allows LoopCoder to delegate scoped work to LoopCoder-managed
workers or provider-native sub-agents. It must not turn provider-native agents
into independent control planes.

Normative requirements for #738, #740, and #739:

- Every child has a durable agent-tree record with parent, owner, provider,
  scope, budget reservation, side-effect class, claim generation, cancellation
  channel, expected outputs, and provenance.
- LoopCoder owns global budgets, permissions, routing policy, approval checks,
  dependency state, final acceptance, and release gates.
- Provider-native sub-agents may execute only within the task scope and
  side-effect class delegated to them. They cannot create sibling tasks, expand
  repo paths, change budgets, approve overrides, or promote output to final
  acceptance.
- One-writer isolation is mandatory for repo writes, local runtime writes,
  task state, and provider receipt state. A stale child completion fails with
  `ErrStaleClaim`.
- Cancellation and recovery must be conservative. If LoopCoder cannot prove
  whether a provider-native child launched or wrote externally, the run becomes
  `needs-human`.
- Child output is provider-output-untrusted until parsed, classified, bounded,
  and verified. Prompt-injection text inside child output must not alter policy,
  approvals, routing, or scheduler authority.

## Command, Path, And Resource Bounds

All v0.8.0 command surfaces must define:

- allowed executable identity and argv shape;
- working directory;
- environment allowlist;
- timeout;
- output byte limit and line limit;
- maximum JSON nesting depth and decoded payload size;
- process group cleanup behavior;
- retry and idempotency behavior;
- classification of stdout, stderr, exit code, and parsed fields.

All path surfaces must define:

- root: project root, LoopCoder home, run directory, scratch root, or provider
  CLI path;
- whether absolute paths are allowed;
- clean path rules using `/` for canonical repo paths;
- symlink resolution rules;
- maximum path length and component count;
- whether the path can cross project boundaries;
- redaction behavior in human and JSON output.

Repository paths used in scopes, fingerprints, fixtures, and diagnostics must
be project-relative unless a schema explicitly allows a machine-local absolute
path. `..` escapes, symlink escapes, UNC/share ambiguity, drive-letter
ambiguity, and cross-project references fail closed before side effects.

## Network Behavior

Network behavior is opt-in and explicit:

- Provider discovery, quota ingestion, model catalog refresh, and
  provider-native federation are local-only unless the user or policy grants
  network permission for that provider action.
- Network permission is scoped by provider, purpose, side-effect class,
  freshness window, and plan fingerprint.
- A provider CLI command that may network must declare that fact even when it
  usually reads local cache.
- Offline mode must not be treated as an error when the task can proceed with
  `unknown` or cached evidence under policy. If policy requires fresh network
  evidence, offline mode blocks with a typed reason.
- Network diagnostics record endpoint class and provider action, not request
  bodies, auth headers, cookies, tokens, or raw credential-bearing URLs.

## Retention And Deletion

v0.8.0 runtime state remains machine-local. Implementation issues must define
retention for their records using these defaults:

- Secret material has no retention because it is never read or stored.
- Provider probe output, quota snapshots, routing candidates, and agent events
  are local runtime state and are not copied into repositories.
- Local diagnostics retain only bounded, classified summaries needed for
  recovery, audit, and release evidence.
- Stale provider output and quota snapshots must age into `stale`; they must not
  be silently reused as fresh evidence.
- Deletion must be explicit, bounded to LoopCoder-owned roots, symlink-safe, and
  project-scoped. It must not delete provider credentials, provider caches, user
  repositories, or unrelated host files.
- Incident bundles and support diagnostics must be redacted, local by default,
  and generated only by explicit operator command.

## Tamper-Evident Provenance

Tamper evidence is for local diagnosis and replay integrity; it is not a remote
attestation guarantee.

Where v0.8.0 records affect authority, implementation issues must add
canonical event hashes:

- each authority event records `event_hash`, `previous_event_hash` within the
  same project/run stream, schema version, policy version, actor provenance,
  host provenance, and canonical classified payload hash;
- hash inputs exclude secret material because secret material is forbidden;
- redacted display text is not the hashed authority payload;
- replay verifies hashes before using a record for approval, routing, budget,
  claim, or recovery decisions;
- a missing, unknown-version, or mismatched hash fails closed to `needs-human`
  for authority decisions.

A local attacker who can modify all local state may still rewrite or recompute a
hash chain. This control detects corruption, partial writes, stale replay, and
accidental tampering; it does not replace signed releases, OS permissions, or
remote provider receipts.

## Abuse Cases And Required Tests

The following cases are required acceptance fixtures for v0.8.0 security work.

| Abuse case | Required fixture | Expected result |
| --- | --- | --- |
| Token in provider stdout | `sk-...`, OAuth-like bearer token, GitHub token, and random high-entropy strings in bounded stdout/stderr. | Secret scanner rejects raw persistence; safe output redacts and records typed finding. |
| Cookie in provider output | `Cookie:` and `Set-Cookie:` headers in CLI output. | Header values are classified as `secret-material` and never serialized. |
| Private key in worktree or log | PEM blocks and SSH private key markers in files, provider output, and logs. | Secret scanner blocks fixture persistence outside expected redacted test golden files. |
| Auth file probe | Provider auth JSON, token cache, keychain path, and `.netrc`-like fixture. | Probe records at most existence/readiness metadata and never copies bytes or parsed secrets. |
| Environment variable leak | Secret-looking values in process environment. | Values never appear in events, reports, JSON, logs, prompts, or errors. |
| Malicious provider output | Output attempts to change instructions, ask for approval bypass, or embed JSON with unexpected fields. | Output remains `provider-output-untrusted`; parser ignores or rejects unexpected fields and cannot change policy. |
| Prompt injection | Repository file instructs worker/router/verifier to ignore budgets or exfiltrate credentials. | Policy, approvals, and scope are unchanged; fixture records rejected reason or bounded untrusted text. |
| Path traversal | `../`, absolute paths, mixed separators, drive letters, UNC paths, and encoded traversal. | Path validation rejects escapes before reads/writes or fingerprint use. |
| Symlink escape | Symlink under project or run root points to credential file or another project. | Realpath confinement rejects with typed diagnostic and no content read. |
| Shell injection | Provider name, profile, model, issue title, or path contains shell metacharacters. | Probe/routing commands use argv arrays; metacharacters are data, not shell syntax. |
| Cross-project data leak | Task or child agent references another `project_id`, checkout, run, report, or budget. | Atomic write fails with `ErrCrossProjectReference`; no partial state remains. |
| Stale quota route | Cached quota is beyond freshness window. | Candidate is rejected or requires override; unknown is not promoted to exact. |
| Child budget bypass | Provider-native child reports success after parent budget was cancelled or exhausted. | Completion fails with stale claim or budget error; parent needs human review if side effect is ambiguous. |
| Reporter/database leak | Event, report, runstatus, incident, or fixture tries to serialize a secret wrapper. | Serialization fails in tests; PR-safe output contains only redacted classified summaries. |

## Verification Matrix

Implementation PRs that claim conformance to this spec must include exact
commands and representative human/JSON evidence for the applicable rows:

| Layer | Required evidence |
| --- | --- |
| Unit tests | Classifier, redactor, secret scanner, path validator, argv validator, approval fingerprint validation, quota confidence transitions, routing rejection reasons, and child-agent scope checks. |
| Integration tests | Provider probe harness with fake CLIs, fake quota sources, fake model catalogs, local ledger reconciliation, router selection, agent-tree launch/cancel/recover, and reporter/runstatus output. |
| Adversarial/failure injection | Fixtures from the abuse-case table, corrupt JSON, oversized output, timeout, partial SQLite write, stale claim, missing provider receipt, and unsupported provider schema. |
| Restart/replay | Crash before persistence, after persistence before side effect, after claim before launch, during launch, after provider receipt, and after stale child completion. |
| Migration/fixture | v0.7-to-v0.8 local state import with secret-like legacy logs, ambiguous claims, project collisions, and hash-chain validation. |
| JSON contract | Stable schemas with classification metadata, redacted PR-safe rendering, unknown enum failure, and cross-platform path canonicalization for Linux, macOS, and Windows. |
| Markdown/docs | Link check and docs tests pass for every PR that changes docs. |
| Security review | Independent security review findings are either resolved in code/docs or explicitly accepted with issue links before v0.8.0 GO. |

Minimum local commands for a documentation-only PR against this spec:

```text
go test ./... -run 'TestMarkdownInternalLinks|TestEmbeddedPlaybookFilesAreNonEmptyAndMatchRootFiles'
```

Implementation PRs must also run the relevant package tests and configured
security audit checks. If a tool is unavailable locally, the PR evidence must
state the exact command, failure mode, and hosted check expected to cover it.

## Deferred Implementation Controls

This issue is complete when this spec is merged. The following controls remain
deferred and must not be bundled into this documentation PR:

- provider probe command runner, classification schema, fake CLI fixtures, and
  provider inventory JSON contracts: #725 through #728;
- quota ingestion, local usage ledger, budget reservation, and availability
  circuit breakers: #729 through #732;
- planner requirement classification, task graph validation, explainable
  routing, role/policy profiles, fallback, and verifier routing: #733 through
  #737;
- provider-native sub-agent registration, scope enforcement, one-writer
  isolation, dynamic run/cancel/recover, and child budget enforcement: #738,
  #740, and #739;
- human/JSON observability, deterministic simulations, migration evidence, and
  release gates: #741, #743, and #744.

The PR body for this documentation issue must state that these controls are
deferred to the listed implementation issues and that this PR intentionally
contains no runtime control changes.

## Relationship To Existing Specs And Docs

- [`0801-delivery-run-contracts.md`](0801-delivery-run-contracts.md) defines
  DeliveryRun records, fingerprints, side-effect classes, typed approvals,
  overrides, idempotency, crash recovery, and migration shape. This spec makes
  those records security authority boundaries for provider discovery, quota,
  routing, and agent federation.
- [`0639-global-data-layout-project-identity.md`](0639-global-data-layout-project-identity.md)
  defines project identity and machine-local runtime storage. This spec uses
  `project_id` as the cross-project isolation key.
- [`0646-nested-sub-agent-plan.md`](0646-nested-sub-agent-plan.md) defines
  nested runs, execution claims, leases, fencing, provider idempotency keys, and
  receipts. This spec applies those rules to provider-native sub-agents.
- [`0567-reporter.md`](0567-reporter.md) defines local-only report behavior.
  This spec keeps reporter data local and requires classification before any
  summary is rendered.
- [`0484-security-robustness-hardening.md`](0484-security-robustness-hardening.md)
  defines earlier security hardening around file modes, path confinement,
  command execution, and bounded reads. This spec extends the same posture to
  v0.8.0 provider, quota, routing, and federation surfaces.
- [`0518-loopcoder-audit.md`](0518-loopcoder-audit.md) defines the audit command
  and secret/security scan posture. This spec requires v0.8.0 fixtures to be
  covered by that audit surface or by more specific package tests.
- [`../security/audit-rubric.md`](../security/audit-rubric.md) remains the
  living read-only audit rubric. Security review for v0.8.0 should use this
  spec as an additional rubric input, not as a replacement.
