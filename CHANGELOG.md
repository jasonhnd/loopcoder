# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.8.1] - 2026-07-19

v0.8.1 closes the production-path gaps found after v0.8.0 publication on native
Darwin arm64: automatic Worker/Verifier route authority, DeliveryRun
claim-dispatch, nested permission-safe child routing, typed fallback wiring,
local waiters, progress host contracts, installer custom-directory PATH, and
release evidence harnesses (canaries, Apple sign/notarize, product-path
go/no-go gate). Fixture and packaged gates pass without live providers; live
Codex/Claude canaries and live Apple trust remain owner-gated for Full GO.
Operator checklist: [`docs/reference/v0.8.1-release-runbook.md`](docs/reference/v0.8.1-release-runbook.md).

### Added

- **v0.8.1 release runbook** - operator freeze → Full GO → publish checklist in
  `docs/reference/v0.8.1-release-runbook.md`, linked from the go/no-go gate and
  releasing reference.

- **Non-blocking Grok/Antigravity canaries** - `release-provider-canary.sh`
  accepts `grok` and `antigravity` with `blocking:false`. Missing CLI/auth/model
  surfaces report `not_available` (exit 0) instead of product failure or zero
  quota; workflow live jobs are informational and cannot fail the blocking
  Codex/Claude summary. See `docs/reference/release-provider-canaries.md`.

- **v0.8.1 product-path go/no-go gate** - `scripts/v081-product-path-gate.sh`
  produces machine-readable `loopcoder.v081_go_no_go.v1` evidence and a human
  report. Fixture mode is CI-safe; packaged mode exercises the candidate binary;
  live Codex/Claude canaries and live Apple trust remain explicit opt-in and are
  never substituted by fixtures. See `docs/reference/v0.8.1-go-no-go.md`.

- **Protected Codex/Claude release canaries** - `scripts/release-provider-canary.sh`
  with fixture and live modes, sanitized evidence schema
  `loopcoder.release_provider_canary.v1`, fork/PR guards, and workflow
  `.github/workflows/release-provider-canary.yml` (environment `release-canary`
  for live runs). Live mode never falls back across providers and requires
  `LOOPCODER_REAL_PROVIDER_CANARY=1`.

- **macOS Developer ID sign/notarize harness** - `scripts/macos-codesign-notarize.sh`
  with dry-run and live modes, sanitized `loopcoder.macos_codesign.v1` evidence,
  PR/fork refusal, and release `sign` job integration. Live mode requires
  `APPLE_SIGN=1` plus Developer ID identity, Team ID, and notary keychain
  profile secrets; dry-run remains the default without credentials.

### Fixed

- **Nested permission matrix on macOS Bash 3.2** - clear the ERR trap around
  expected non-zero `nested run` exits so policy-violation cases are not
  misreported as `matrix_unhandled_error`. Isolate fixture repos from global
  git hooks and accept modern refusal `capability_result` shapes.

- **Installer custom directory PATH** - `LOOPCODER_INSTALL_DIR` is now reused
  for PATH detection, profile lines, and printed instructions (quoted for
  spaces). Re-runs stay idempotent; `LOOPCODER_NO_MODIFY_PATH=1` prints
  guidance without editing profiles. Relative install dirs are rejected.

### Changed

- **Typed fallback production wiring** - after Worker classifies a provider
  failure, `dispatch` / `delivery claim-dispatch` call
  `routing.ApplyTypedProviderFailure` when a route decision exists. Pins and
  needs-human classes stay fail-closed; auto-eligible classes may persist a
  bounded successor without relaunching a provider in-process.

- **Host progress visibility contracts** - negotiated foreground sinks for
  Codex, Claude, Paseo, and generic CLI; Worker progress delivery annotates
  the selected host; unknown hosts degrade to generic. See
  `docs/reference/progress-hosts.md`. Active delivery still never uses a model.

- **Production wait wiring** - `loopcoder wait` now ships provider-free
  `approval`, `outbox`, and `detached-worker` watchers against durable local
  state, with restartable file checkpoints and zero provider launches.
  `quota-reset` remains available. GitHub CI waiting stays on the existing
  orchestration path.

- **Nested child route authority** - unpinned nested children resolve a
  permission-safe route from the immutable child execution contract before
  plan persistence and claim/launch. Mixed read-only/write plans may select
  different eligible adapters; explicit child or `--provider` pins pass the
  same nested permission matrix. Orchestrate and unbridged native delegation
  remain refused. Route receipts appear on child results.

- **Worker dispatch route authority** - ordinary unpinned `loopcoder dispatch`
  persists a route decision before provider launch and uses the selected
  adapter/model/effort exactly. Explicit `--provider` is a durable pin.
  Empty-provider Codex defaults are removed from the worker prepare path.
  `no_route` returns exit code 20 with zero provider launches.

- **Independent verifier route authority** - unpinned `loopreview` persists a
  verifier route decision before launch, enforces independence from the
  configured worker, and returns needs-human when no independent read-only
  verifier is available.

## [0.8.0] - 2026-07-16

v0.8.0 was the prior public release for native Darwin arm64. It publishes
resource, routing, nested-run, progress, and waiter contracts alongside durable
process ownership and an auditable migration. A post-publication product-path
audit found that several of those contracts are not connected end to end or
proven with real providers, so v0.8.0 is supported for controlled canary and
development work rather than unattended production orchestration. The binding
status is the
[`v0.8.0 capability and support matrix`](docs/reference/v0.8.0-capability-matrix.md).

### Added

- **Guided DeliveryRun contracts** - versioned runs, tasks, dependency edges,
  attempts, decisions, approvals, overrides, immutable fingerprints, and
  approval-gated `delivery plan|decide|continue` operations are stored as
  normalized SQLite records.
- **Provider and account inventory** - bounded, fixed-argv probes discover
  Codex, Claude, Antigravity, Grok, and declared future adapters without
  reading credential values or treating installation as authentication.
- **Dynamic model capability catalog** - model, role, context, tool, permission,
  and provenance evidence is lifecycle-bound and fails closed when stale or
  ambiguous. Grok uses dynamic provider inventory rather than fabricated
  static defaults.
- **Quota and usage intelligence** - supported machine-readable provider
  sources and optional CodexBar evidence produce immutable quota snapshots;
  unavailable or stale telemetry remains explicit instead of guessed. A local
  append-only usage ledger reconciles provider and LoopCoder observations.
- **Hierarchical budgets and availability** - machine, provider, account,
  model, project, run, and task scopes support atomic reserve, commit, release,
  retry, and conservative overage handling.
- **Routing and fallback contracts** - deterministic task requirements,
  capability-first eligibility, quota/reset-aware scoring, policy-bound
  fallback, independent-verifier policy, and reason-coded decision types are
  implemented for integration in v0.8.1. Ordinary unpinned v0.8.0 dispatch
  still defaults directly to Codex.
- **Nested-run contracts** - durable registrations, parent authority, depth,
  fan-out, concurrency, permission, budget, cancellation, and one-writer
  records provide bounded deterministic test infrastructure. No
  production-provider nested mode is supported in v0.8.0.
- **Grok provider** - the `grok` worker adapter, bounded install/auth/catalog
  discovery, ACP billing telemetry when advertised and explicitly allowed,
  dynamic model attribution, and native-agent capability probing are available
  without auto-installing, auto-updating, or logging in to Grok Build.
- **Durable detached execution** - non-interactive dispatch defaults to a
  detached supervisor, persists provider PID/process-group/birth identity, and
  returns a run ID for `status`, `attach`, and `cancel`. A Darwin guardian reaps
  verified provider groups after abrupt supervisor death.
- **Provider-free waiter and progress infrastructure** - restartable CI,
  approval, quota-reset, outbox, and worker-terminal state-machine components
  plus durable five-minute receipts and a delivery outbox are present. The
  local CI waiter is connected; the other active waiter and unsolicited host
  delivery paths remain incomplete in v0.8.0.
- **Orchestration cost accounting** - model calls, tokens when known, wall
  time, waiting, retries, recovery, delivery, and verifier work are recorded
  per run; unresolved reservations remain fail-closed while terminal providers
  with unavailable usage can continue only under conservative call budgets.
- **Auditable storage migration** - `migrate storage` plans schema 9 through 30
  without side effects, verifies an owner-only backup before mutation, applies
  ordered steps atomically, reports stable rollback limits, and is idempotent
  on replay.

### Changed

- **macOS Apple Silicon only** - native `darwin/arm64` is the sole supported
  v0.8.0 runtime, installer, upgrade, CI, smoke, and release tuple. The release
  contains one `loopcoder_<version>_darwin_arm64.tar.gz` archive plus checksum,
  signature, and provenance material.
- **Four required CI contexts** - pull requests emit `verify`, `test`, `race`,
  and `security` on pinned `macos-15`. PR race work is limited to changed Go
  packages; the release build reruns the complete repository race suite before
  packaging.
- **Machine-local project payloads** - registered projects write attempts,
  events, lifecycle, recovery, relay, audit, logs, and scratch data below
  `$LOOPCODER_HOME/projects/<project_id>/` instead of the repository.
- **Recovery semantics** - live provider authority is reconciled before any
  redispatch. Existing PR, already-applied push, no-change, and delivery retry
  outcomes are typed and idempotent so administrative completion cannot rerun
  useful provider work.
- **Release verification** - staged smoke used the exact final archive for
  fresh install, self-bootstrap, v0.7 schema migration, copied-backup rollback,
  already-latest upgrade, and public artifact checks before publication.

### Breaking Changes

- Windows, Linux/Ubuntu, WSL, containers used as a LoopCoder runtime, Intel
  macOS, and Rosetta/amd64 macOS are unsupported in v0.8.0. No v0.8 artifact,
  native CI job, smoke job, or compatibility commitment exists for those
  tuples. v0.7.0 remains the final legacy multi-platform release.
- The v0.8.0 binary rejects unsupported hosts with exit code `78` and stable
  `ErrUnsupportedPlatform` human/JSON diagnostics before network, credentials,
  provider launch, storage migration, or repository mutation.
- A schema-30 database cannot be opened by v0.7.0. Rollback requires stopping
  every LoopCoder process, copying the verified schema-9 backup into an offline
  home, and accepting loss of v0.8-only state created after migration.

### Upgrade From 0.7.0

On native Darwin arm64:

```text
loopcoder upgrade --version 0.8.0
loopcoder version
loopcoder migrate storage --format json
loopcoder migrate storage --apply --format json
loopcoder skill install --repo .
loopcoder projects register --repo .
loopcoder doctor --repo . --format json
```

Planning is read-only; `--apply` is required for migration. Stop all LoopCoder
processes before apply or rollback. See
[`docs/reference/storage-migration.md`](docs/reference/storage-migration.md)
for backup verification, interruption behavior, limitation codes, and the
offline v0.7.0 restore procedure.

### Known Limitations

- Automatic Worker routing, routed Verifier independence, typed fallback, and
  quota-reset waiting are not connected product paths. Explicitly pinned
  Worker and Verifier use is canary-only.
- No real-provider nested mode is supported. The accepted read-only nested path
  does not enforce mutation-free behavior in the Worker adapter, and write or
  orchestrate production-provider modes are refused.
- Progress receipts and delivery-outbox records are durable, but active work
  does not have release-proven unsolicited host delivery. Approval, outbox,
  and detached-terminal waiters are not fully integrated.
- The public Mach-O is not Apple Developer ID signed or notarized. Sigstore
  verifies checksum provenance and archive integrity, not Gatekeeper trust.
- Protected exact-v0.8.0 real-provider canaries are absent. Historical adapter
  smokes and deterministic fixtures do not replace release evidence.

- Provider quota is used for routing only when a supported, sufficiently fresh
  source supplies the required evidence. LoopCoder does not scrape private
  credentials, invent exact five-hour or weekly limits, or treat CLI presence
  as usable capacity.
- Antigravity remains write-only in the verified adapter contract and cannot be
  selected as the read-only verifier. Direct `gemini` remains experimental.
- Grok dynamic models and quota depend on capabilities exposed by the installed
  Grok CLI; missing or unsupported protocols remain explicit unavailable facts.
- Durable supervision covers provider work on the local supported machine. It
  does not create an autonomous cloud conductor or guarantee universal
  exactly-once external side effects after arbitrary provider failures.

## [0.7.0] - 2026-07-11

v0.7.0 was the customer install target for its release cycle and remains the
final legacy multi-platform release. It moved loopcoder's local runtime from
repo-only files toward a machine-local project registry and SQLite-backed
runtime store, added explicit local-state migration and nested run-tree
observability, and shipped through the staged signed release flow.

### Added

- **Machine-local runtime store** - v0.7.0 adds a SQLite-backed storage layer
  under `$LOOPCODER_HOME/data/loopcoder.db` plus machine-local project, log,
  and temporary directories. The store indexes projects, runs, reports, import
  records, leases, and nested run graph metadata without turning local runtime
  evidence into repository-visible state.
- **Project registry** - `loopcoder projects register|list|show|remove`
  manages stable local project identities across multiple checkouts. Identity
  prefers normalized GitHub owner/name, then sanitized normalized git remote,
  then canonical local path. `remove` detaches the active registry entry while
  preserving the project row, run history, reports, import records, and
  migration status for re-registration.
- **Explicit v0.6.x local-state migration** - `loopcoder migrate local-state
  --repo . --dry-run` previews compatible repo-local `.loopcoder/` imports, and
  the non-dry-run path copies attempts, events, reports, recovery briefs, and
  relay records into the machine-local store. Migration is idempotent,
  source-hash tracked, and never deletes or rewrites the source `.loopcoder/`
  files.
- **Storage and registry diagnostics** - `loopcoder doctor --repo .` now
  reports storage reachability, schema health, migration status, project
  registry identity, detached/ambiguous project state, nested run graph health,
  and safe `--fix` actions. JSON output adds `runtime`, `host_profile`, and
  `provider_compatibility` fields for support tooling.
- **Owner-only storage protection on Unix-like systems** - the storage layer
  creates and tightens `$LOOPCODER_HOME`, `$LOOPCODER_HOME/data`, the SQLite
  database, and SQLite sidecars to owner-only permissions, refuses symlink and
  non-regular storage paths, and lets `doctor --fix` repair insecure existing
  modes in place.
- **Provider and host compatibility matrix** - `internal/runtimecap`,
  `internal/hostprofile`, and `internal/provider` describe support levels for
  worker, verifier, audit, nested, hook, and JSON-output modes across Codex,
  Claude, Antigravity, and local host profiles. `doctor` exposes the matrix
  without treating unsupported modes as silently usable.
- **Nested orchestration runtime** - `loopcoder nested run --repo . --plan
  child-plan.json --provider <provider>` validates the
  `loopcoder.child_plan.v1` envelope, persists parent/child run graph data,
  schedules dependency-aware child runs with max-depth and concurrency bounds,
  supports cancellation/resume/failure propagation, and executes production
  child work through the normal Worker dispatch path. The reserved
  `test-subprocess` provider exists only for deterministic local and release
  smoke tests.
- **Run-tree observability** - `loopcoder status --format json` and
  `loopcoder report --run <id> --format json` include an additive `run_tree`
  object with root/parent/child IDs, lifecycle status, issue/PR metadata,
  provider/model/effort, report summaries, and aggregate counts.
- **Reporter receipt rendering** - Worker, Verifier, audit, and Conductor
  pretty output now uses compact `Target`, `Verdict`, `Review summary`, `Run`,
  and `Next` receipts; `loopcoder report --verbose` exposes raw canonical
  records in text mode while `--format json` remains JSON-only.
- **v0.7.0 self-bootstrap acceptance** - added
  `docs/reference/self-bootstrap.md` and
  `scripts/self-bootstrap-smoke.ps1` so the release can prove loopcoder's own
  registry, machine-local database, provider compatibility visibility, and
  nested parent/child run-tree observability before tagging.
- **v0.7.0 release readiness artifacts** - added
  `.github/release-notes/v0.7.0.md` and
  `docs/reference/v0.7.0-go-no-go.md`, now completed with the post-publication
  GO decision and release evidence.
- **Expanded v0.7.0 release smoke and CI gates** - the release workflow stages
  a signed draft release, runs native smoke on Ubuntu, macOS, and Windows, keeps
  failed candidates draft-only, and requires the `release-publication`
  environment before publishing. `scripts/release-smoke.ps1` verifies signed
  release assets, archive extraction, `doctor --format json`,
  `report --format json`, project registry registration/list/show,
  machine-local database placement outside the repo, `migrate local-state
  --dry-run`, provider compatibility, self-bootstrap nested run-tree
  observability, already-latest upgrade behavior, and upgrade from v0.6.1.

### Changed

- **Runtime state location** - new v0.7.0 records prefer machine-local storage
  after registration, while repo-local `.loopcoder/` remains the compatibility
  fallback during migration. The explicit `state push` boundary remains the
  only publishing path for local run summaries.
- **Doctor repair boundary** - `doctor --fix` can tighten Unix storage
  permissions, migrate legacy reporter config keys, refresh conductor hook
  commands, rewrite eligible local report keys, and prune cleanup-eligible
  gitignored repo-local state. It does not delete or recreate the SQLite
  database, install provider CLIs, run provider login, flush relay records,
  choose models, commit, push, or mutate GitHub.
- **Nested execution authority** - provider-native sub-agent features are not
  treated as authoritative orchestration. The accepted child plan, permission
  ceiling, scope validation, persistence, cancellation, timeout, resume, and
  aggregation rules belong to loopcoder.
- **Release process** - v0.7.0 assets were created as a draft release first;
  publication was a separate human-gated environment step after staged smoke
  passed. README install and upgrade examples now target the final signed
  v0.7.0 release.
- **README and usage command inventory** - v0.7.0 commands and output fields
  are documented as the current public release surface.

### Fixed

- **Reporter output modes** - `dispatch`, `dispatch-wave`, `loopreview`,
  `audit`, and `attest` now default to concise human receipts for merged-stream
  host integrations; `--format json` emits JSON only, and `--verbose` is the
  opt-in compatibility/debug path for canonical reporter records.
- **Remote credential redaction** - project identity and diagnostics sanitize
  git remotes before persistence or output, dropping URL userinfo, tokens,
  credential-like query strings, fragments, and unsafe raw remote strings.
- **Project identity stability on macOS** - path canonicalization accounts for
  aliased temporary directories and physical paths so the same checkout does
  not fragment into duplicate project identities.
- **Nested scheduler reliability** - child run IDs remain unique, terminal
  events are mirrored consistently, required skipped children propagate
  correctly, and project list JSON remains stable for smoke automation.
- **Release smoke already-latest assertion** - joined native command output
  before matching so multi-line PowerShell arrays no longer produce a false
  negative for a good already-latest upgrade.
- **Host-sensitive provider compatibility smoke** - the codex Worker
  compatibility assertion now accepts `supported` on known hosts and
  `experimental` on the generic-local fallback host, while still failing closed
  for unsupported compatibility.
- **Self-bootstrap build path** - `scripts/self-bootstrap-smoke.ps1 -Repo
  <path>` now builds `./cmd/loopcoder` from the supplied repo path instead of
  the caller's current working directory.

### Security

- **Machine-local database permission hardening** - Unix-like storage paths are
  created and repaired as owner-only, SQLite sidecars inherit owner-only file
  modes, and symlink/non-regular storage targets are refused before chmod or
  open. Windows reports the current DACL-hardening limitation explicitly rather
  than claiming POSIX-style protection.
- **Credential-safe registry metadata** - remote URL credentials and
  credential-like query material are removed before registry persistence,
  project display output, doctor output, or migration diagnostics.

### Deprecated

- **Repo-local runtime state as the primary store** - `.loopcoder/` remains
  readable and is not deleted, but v0.7.0 moves the authoritative local runtime
  index for registered projects to `$LOOPCODER_HOME/data/loopcoder.db`.
- **Raw reporter records in default text output** - default text output is now
  the compact receipt. Use `--verbose` for local compatibility/debug inspection
  of canonical headers and records, or `--format json` for machines.

### Compatibility

- v0.7.0 is the current public release, with signed platform archives,
  `SHA256SUMS`, and `SHA256SUMS.sigstore` published on 2026-07-11.
- Existing repo-local `.loopcoder/` state remains readable during the
  compatibility window. `migrate local-state` copies compatible records into
  machine-local storage and leaves the source files in place.
- SQLite storage is machine-local and not a cloud sync surface. Back up
  `$LOOPCODER_HOME/data/loopcoder.db` plus `$LOOPCODER_HOME/projects/`,
  `$LOOPCODER_HOME/logs/`, and `$LOOPCODER_HOME/tmp/` when present and no
  loopcoder command is running.
- `audit --format both` has been removed. `loopcoder audit` now accepts
  `--format text` or `--format json`; use separate invocations when both human
  and machine outputs are required.
- Windows does not enforce owner-only DACL hardening for the v0.7.0 storage
  directory. `doctor` reports that limitation and `doctor --fix` cannot repair
  it in this version.

### Upgrade

Upgrade from v0.6.1 with `loopcoder upgrade --version 0.7.0`, run
`loopcoder skill install --repo .`, register each checkout with
`loopcoder projects register --repo .`, inspect
`loopcoder migrate local-state --repo . --dry-run`, and run the non-dry-run
migration only after reviewing the copied record set. Roll back by selecting a
prior released binary such as v0.6.1 and keeping or backing up
`$LOOPCODER_HOME`; do not delete repo-local `.loopcoder/` history unless the
operator explicitly chooses to.

## [0.6.1] - 2026-07-08

0.6.1 is the customer-ready bridge for the public 0.6 line. The latest public
release before this bridge was v0.5.4; customer install and upgrade commands
target v0.6.1 rather than a nonexistent public v0.6.0 release. It packages the
0.6 model/depth, Antigravity, reporter, doctor, upgrade, local-state, report,
state, lease, and process-management work into an operable first-run flow.

### Added

- **First-run repository safety** - `loopcoder init --repo <path>` scaffolds a
  repository from any working directory, supports `--gate human-merge|auto`,
  defaults new scaffolds to `adapters.gate: human-merge`, and protects
  `.loopcoder/` through local `.git/info/exclude`.
- **Project hook local-state protection** - `loopcoder skill install --repo
  <repo>` refreshes the global bundled skill, project hook settings,
  `.loopcoder/conductor-workspace`, and local `.git/info/exclude` protection.
- **Machine-readable doctor** - `loopcoder doctor --format text|json` keeps
  text output while adding JSON with `repo_path`, build metadata, `exit_code`,
  and ordered checks with `name`, `status`, `hard`, `message`, and
  `fix_command`.
- **Local-state and reportquery diagnostics** - doctor reports whether
  `.loopcoder/` is excluded, whether local state is already tracked, whether
  reportquery can read local records, whether the managed skill is fresh, and
  whether project conductor hooks are installed.
- **Richer report JSON** - `loopcoder report --format json` keeps the
  compatibility `reports` array and adds `records[]` entries containing the
  report plus source, run id, and local path context.
- **Customer release note source** - `.github/release-notes/v0.6.1.md` is the
  public release-note body for the bridge release.

### Changed

- **Release-facing documentation truth** - README, usage docs, stability
  policy, roadmap, and release notes now consistently describe v0.6.1 as the
  current customer install target for the 0.6 line.
- **Customer quickstart** - the first-run flow is now install, `loopcoder
  version`, `loopcoder init --repo .`, `loopcoder skill install --repo .`,
  `loopcoder doctor --repo .`, `loopcoder report --repo .`, then
  dispatch/tick/loopreview through the conductor workflow.
- **Command inventory coverage** - README and usage command lists now cover
  every command registered by `internal/cli.Commands()`, including `report`,
  `state`, `lease`, `ps`, and `kill`, with internal or advanced commands
  labeled.
- **Pretty-output docs** - reference docs now match the current pretty renderer,
  where the provider line combines vendor and provider key, such as
  `OpenAI Codex / codex`.

### Upgrade

Upgrade from v0.5.4 or older 0.5.x releases with:

```text
loopcoder upgrade --version 0.6.1
loopcoder version
loopcoder skill install --repo .
loopcoder doctor --repo .
```

`loopcoder upgrade --version 0.6.1` selects the machine-level binary, verifies
signed checksums, and refreshes the global bundled skill. Each project still
needs `loopcoder skill install --repo <repo>` to refresh project hooks and local
`.loopcoder/` exclude protection, followed by `loopcoder doctor --repo <repo>`
to confirm readiness. A second upgrade to v0.6.1 should report that the
selected binary is already latest.

### Notes

- `.loopcoder/` remains repo-local machine state. It is protected through local
  `.git/info/exclude` and must not be committed to normal business branches;
  `loopcoder state push` is the explicit state-branch publishing path.
- v0.6.1 introduces no SQLite database, global project registry, project alias
  map, or native sub-agent scheduler.
- The 0.5.x to 0.6.x reporter compatibility window remains intact: legacy
  inputs and aliases are still accepted, while new output and new docs use
  reporter terminology.

## [0.6.0] - 2026-07-08

0.6.0 is loopcoder's first breaking transition release. It completes three related units: model/depth discovery and validation, the live operator-facing rename from attestation to reporter, and the 0.5.x to 0.6.0 upgrade/migration/doctor path. The release keeps old local machine contracts readable for one transition window while making new output, docs, hooks, and config use the reporter vocabulary.

### Breaking

- **Operator-visible reporter rename** - new Worker, Verifier, audit, and Conductor output uses `[reporter]` headers and result JSON `report` objects. The old `[attestation]` token and result JSON `attestation` key are still accepted by readers, relay gates, and conductor hooks for this 0.6.0 transition release, but new output emits the new names only.
- **Conductor hook command rename** - the current hook command is `loopcoder hook conductor-reporter`. The old `loopcoder hook conductor-attest` command remains a one-version alias so upgraded binaries, installed skills, and host hook settings cannot lock each other out during rollout.
- **Config/env rename window** - `.delivery.yml report:` replaces the old `attestation:` root, and `report.channel` replaces `attestation.channel`. The new env vars are `LOOPCODER_CONDUCTOR_REPORTER_SCOPE` and `LOOPCODER_CONDUCTOR_REPORTER_STATE_DIR`; the old `LOOPCODER_CONDUCTOR_ATTEST_SCOPE` and `LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR` remain accepted for one release, with the new vars winning when both are set.
- **Frozen local machinery stays frozen** - `.loopcoder/relay/*.attest` ledger extensions, canonical report JSON field names such as `usage.total_tokens`, old historical specs, and existing local run records are not terminology-swept. This deliberately avoids breaking recovery and relay evidence that was produced by 0.5.x.

### Added

- **Static model and depth registry** - `loopcoder models` lists the curated provider/model/depth table used by Worker and Verifier selection. The initial registry covers `codex` (`gpt-5.5`, default `high`), `claude` (`claude-opus-4-8[1m]`, default `max`), and `antigravity` (`Gemini 3.1 Pro`, default `High`, plus `Opus 4.6` / `Thinking` and `GPT-OSS 120B` / `Medium`).
- **Role-scoped model/depth validation** - Worker and Verifier providers resolve independently from command flags, `.delivery.yml`, and registry defaults. Invalid provider, model, or depth values warn by default and pass through for compatibility; `models.strict: true` or per-run `--strict` rejects invalid selections before launch.
- **Google Antigravity provider** - provider key `antigravity` runs the `agy` CLI, using `agy -p <prompt> --add-dir <worktree> --model "<model> (<Depth>)"`. The required `--add-dir` pins the worktree so Antigravity edits the intended repository instead of its own scratch directory.
- **`loopcoder report`** - a read-only reporter query surface for recent local reports, keyed by work ID, role, provider, model/depth, timing, tokens, and result context.
- **0.5.x to 0.6.0 migration diagnostics** - `loopcoder upgrade` classifies the selected and target versions as pre-breaking, breaking transition, post-transition, or unknown, refreshes bundled skills, and reports whether compatibility aliases are active.
- **Expanded `loopcoder doctor`** - `doctor --repo .` is now the read-only operational health entry point for git/gh, provider CLI readiness, provider auth probes where stable, `.delivery.yml` validity, model/depth validation, reporter/relay wiring, selected binary version, migration status, installed skill freshness, audit readiness, and stale local state counts.
- **Explicit `doctor --fix` mode** - `loopcoder doctor --repo . --fix` is the opt-in mutating repair path for reporter config-key migration, conductor hook command migration, hook state migration, eligible local state key rewrites, and stale local state cleanup.
- **Stale local state retention policy** - cleanup retains active runs, newest run directories, recent run directories, pending relay obligations, recent `.attest` ledgers, newest `.attest` ledgers, recent audit logs, referenced audit logs, and recent worktree-liveness artifacts. Cleanup is bounded, skips symlinks, and stays under the repo's `.loopcoder/` tree.
- **Release documentation rule** - [`docs/reference/releasing.md`](docs/reference/releasing.md) now requires every version bump to rewrite the changelog entry, GitHub Release Note, and README release-facing sections completely and in detail before the release is ready.

### Changed

- **Reporter terminology is the live surface** - Go package/type names, CLI pretty output, current reference docs, hook templates, relay guard wording, worker/verifier/audit result objects, and conductor playbooks now talk about reports and reporter records. Historical changelog entries and shipped specs keep their old wording as history.
- **Pretty report output shows stronger context** - reports can include work ID, issue, branch, worktree, round, selected model/depth, provider vendor, tool key, model source, host-local timing, grouped token counts, and verified status. Antigravity Worker reports are self-reported and accept absent token usage because `agy` does not expose stable parseable usage in this path.
- **Release workflow uses the full release-note body** - the tag-triggered release job now reads `.github/release-notes/<tag>.md` when present, so `v0.6.0` publishes the detailed operator-facing GitHub Release Note instead of the previous one-line automated body.
- **README current-release documentation** - the top-level README now documents 0.6.0 as current, including model/depth selection, Antigravity setup, reporter transition aliases, upgrade commands, `doctor --fix`, and stale local state cleanup.

### Upgrade

Users upgrading from 0.5.x should run:

```text
loopcoder upgrade --version 0.6.0
loopcoder doctor --repo .
loopcoder doctor --repo . --fix
loopcoder doctor --repo .
```

`loopcoder upgrade` selects the 0.6.0 binary from GitHub Releases, verifies signed checksums, swaps the binary atomically or stages the Windows deferred replacement, and refreshes the bundled loopcoder skill. The first `doctor` run is diagnose-only and safe anytime; `doctor --fix` performs the explicit local migration/cleanup actions; the final `doctor` confirms no legacy surfaces or cleanup-eligible state remain. Environment variables cannot be rewritten by doctor, so any old `LOOPCODER_CONDUCTOR_ATTEST_*` shell settings must be changed by the operator.

### Notes

- The 0.6.0 transition intentionally accepts both old and new reporter tokens and keys, but new writes use `[reporter]`, `report`, `report.channel`, and `conductor-reporter`. A later release will decide when to remove old aliases.
- Antigravity is a write-capable Worker provider. It is not a verified read-only Verifier or audit-review provider, so read-only selections fail closed instead of launching `agy` in a mode it cannot safely provide.
- `doctor` is not a reporter role and does not emit or satisfy Worker, Verifier, audit, or Conductor report obligations. It is an operator preflight and repair command.
- CI remains the full loopcoder gate: verify, Go build/test, staticcheck, govulncheck, and audit.

## [0.5.4] - 2026-07-06

Two reliability lines: make the 0.5.3 `loopcoder audit` usable in real consumer repositories, and harden the `loopreview` verifier against false `needs-human` verdicts on large PRs. Per [`docs/specs/0533-audit-consumer-repo-usability.md`](docs/specs/0533-audit-consumer-repo-usability.md), [`docs/specs/0535-loopreview-packet-truncation-reliability.md`](docs/specs/0535-loopreview-packet-truncation-reliability.md), and [`docs/specs/0539-loopreview-cited-spec-not-conformance-target.md`](docs/specs/0539-loopreview-cited-spec-not-conformance-target.md). Built by loopcoder itself under the self-hosting guard (human merge gate).

### Changed

- **Audit scans default to git-tracked files** — native secret and file-permission scans scope to git-tracked files by default, with a non-Git fallback and configurable default excludes, so `.gitignore`d paths such as loopcoder's own `.loopcoder/` are no longer walked. (per spec 0533)
- **Secret detection is signature-first with an entropy floor** — findings are split into a high-confidence signature tier (known key/token formats) and a lower-confidence entropy/keyword tier, cutting the `process.env`-style false positives that made the audit unusable on real trees. (per spec 0533)
- **Audit gate posture is net-new only** — the default CI gate fails only on net-new signature-tier findings via baseline diff; entropy/keyword-tier findings warn without failing the gate, baselined findings remain visible as `waived: true`, and both human text and JSON output distinguish gate findings, warnings, waived findings, and needs-human items. Exit-code precedence (runtime > needs-human > findings > clean) is preserved. (per spec 0533)
- **loopreview review packets stay bounded and fresh** — the verifier fetches fresh PR refs and reads PR-head files through a read-only helper, and documentation/added-file bodies are emitted as a bounded PR-head body packet, so large valid PRs no longer truncate into a spurious `needs-human`. (per spec 0535)
- **loopreview does not fail closed on a body-cited unmerged sibling spec** — a design doc merely referenced in a PR body is distinguished from the conformance target, so citing an unmerged sibling spec no longer forces `needs-human`. (per spec 0539)

### Fixed

- **Windows file-permission false positives** — native file-permission scanning skips synthesized Unix mode-bit findings on Windows, where they carry no meaning. (per spec 0533)
- **Audit self-scan noise closed ([#532](https://github.com/jasonhnd/loopcoder/issues/532))** — git-tracked default selection, signature-first precision, and warn-only entropy tiers together close the 492-findings / 0-true-positives CI-gating problem the audit exhibited on loopcoder's own tree.
- **`sigstore/cosign-installer` bumped 4.0.0 → 4.1.2** via Dependabot, keeping the release-signing action current.

### Notes

- All 0.5.3 audit invariants (the 0518 read-only Layer-2 boundary, H5 exit-code discipline, secret redaction, deterministic fingerprints), the 0.5.1 hardening, and the 0.5.2 behavior-preservation are preserved; loopcoder's own self-audit stays green in CI. Every slice was gated through the independent read-only verifier and the full CI suite (verify/go/staticcheck/govulncheck/audit) under the human merge gate.

## [0.5.3] - 2026-07-06

Add `loopcoder audit`, a read-only built-in security audit, per [`docs/specs/0518-loopcoder-audit.md`](docs/specs/0518-loopcoder-audit.md). It institutionalizes catching the class of issue that led to the 0.5.1 hardening — on demand and in CI — with two layers: a deterministic SAST floor (CI-gateable) and an adversarial LLM security-review lens. Built by loopcoder itself under the self-hosting guard.

### Added

- **`loopcoder audit` command** — a read-only audit that emits structured findings (severity/file/rule/evidence) with H5-style exit codes: `0` clean, `1` findings at/above the configured severity threshold, `2` needs-human, `3` command/runtime failure.
- **Layer 1 — deterministic SAST floor** — runs a configurable command set (default Go: `govulncheck`, `staticcheck`, `gosec`) plus native secret and file-permission/sensitive-write scans, normalizes all output into one finding schema with secret redaction, and is CI-gateable (no LLM; deterministic sort; timestamp-independent fingerprints).
- **Layer 2 — LLM security-review lens** — an adversarial, language-agnostic, design-level review that reuses the read-only verifier path (`agent.Runner` with `Invocation.ReadOnly`) with a built-in threat-model rubric plus any repo-configured rubric; attested exactly like a read-only verifier and degrading to `needs-human` on infrastructure/timeout/parse failure (never a silent clean). Only read-only-classified MCP servers are offered to the invocation.
- **Configurable via `.delivery.yml audit`** — severity threshold, SAST command set, native-scan toggles, review rubric path, and a baseline/waiver file (additive, `omitempty`/`Default()`-safe, unknown-field-tolerant per 0161 M3).
- **Required CI `audit` check** — loopcoder now audits itself in CI through the deterministic floor and is green on its own tree; `audit` is added to `.delivery.yml ci.checks`. A promotion red-line for audit status is deferred.
- **`loopcoder doctor` audit readiness** — reports config validity, effective threshold, the SAST commands that will run, required tools on `PATH`, parser recognition, rubric/baseline validity (including stale/expired waivers), the required-check wiring, and Layer-2 read-only verifier provider resolution.
- **Docs + example security rubric** describing the two layers, exit codes, config, thresholding, baselines/waivers, CI usage, and local-only attestation.

### Fixed

- **Worker-layer prompt and recovery-brief writes hardened to `0o600`** — closing a 0.5.1 shared-host-disclosure gap the new audit surfaced (the earlier A1 hardening had covered only the agent layer, not the worker-layer scratch writes).
- **`golang.org/x/sys` bumped to v0.46.0** — clearing a real dependency vulnerability the self-audit's `govulncheck` flagged.

### Notes

- The deterministic floor is what gates CI; the LLM lens is not a required hosted CI dependency. Audit findings and Layer-2 attestation are local-only. The 0161 E1 `ReadOnly` boundary, the 0.4.2 H5 exit-code discipline, the self-hosting guard, the 0.5.1 hardening, and the 0.5.2 behavior-preservation are all preserved. Wiring the self-audit surfaced real findings — the worker `0o600` gap and an `x/sys` vulnerability — which were fixed rather than waived: the audit's first act was to harden loopcoder itself.

## [0.5.2] - 2026-07-05

Behavior-preserving core refactor, per [`docs/specs/0507-core-refactor.md`](docs/specs/0507-core-refactor.md). This release has **zero observable behavior change**: it decomposes god-functions and centralizes scattered defaults for readability, testability, and reduced drift. Every slice proved behavior-preservation via golden/inventory tests and independent verifier path-tracing, and was gated by the full 0.5.1 CI suite (build/vet/test/`-race`/staticcheck/govulncheck). Built by loopcoder itself under the self-hosting guard.

### Changed

- **`worker.Dispatch` decomposed** into focused, independently-tested helpers (`prepareDispatch`/`prepareWorktree`/`buildInvocation`/`runAgent`/`handleHungOrPartialWork`/`commitAndOpenPR`/`writeRecovery`/`cleanup`) behind the unchanged `Dispatch` entrypoint and `worker.Result` contract.
- **Orchestration state/render split** — `tick`, `promote`, and `dispatch-wave` keep state progression in report-returning functions, while text/JSON rendering moves to dedicated render files with byte-identical output (`RenderTickText`, `RenderPromoteText`, `RenderDispatchWaveText`, and the JSON marshallers are preserved).
- **MCP validation consolidated** into a single shared parse-time validator reused by config parsing, the MCP bridge, and the provider layer; the accepted/rejected config set is unchanged and Codex/Claude/Gemini argv stays byte-identical, with the read-only verifier filter still fail-closed.
- **Defaults/limits centralized** into a new `internal/defaults` leaf package (branch names, dispatch-wave throttle, hard caps, retry backoff, packet budgets, list limits, and other bounds), read from a single documented source with no value tuning and copy-returned mutable slices.

### Notes

- Zero behavior change: every value and rendered effect is identical, and the absent-config code profile is byte-for-byte unchanged. The 0161 F1–F5/M1–M4/E1 invariants, the 0.4.2 H5 exit-code contract, the `Invocation.ReadOnly` verifier boundary, the self-hosting guard, and the entire 0.5.1 security hardening are preserved. `internal/defaults` is a leaf package that imports only the standard library, so no import cycle is introduced.

## [0.5.1] - 2026-07-05

Security and robustness hardening from an external security audit of the codebase, per [`docs/specs/0484-security-robustness-hardening.md`](docs/specs/0484-security-robustness-hardening.md). loopcoder is a local single-operator dev CLI, so most findings were Low–Medium hardening rather than active-exploit fixes, but every verified finding is closed. Built by loopcoder itself under the self-hosting guard (human merge gate): the spec merged first, then the fixes landed as file-disjoint slices gated through the read-only verifier and CI.

### Security

- **Supply-chain integrity** — release `SHA256SUMS` is now signed with cosign (keyless/OIDC). The shell installer, PowerShell installer, and `loopcoder upgrade` verify the signature before trusting the checksum and fail closed when it is missing, malformed, or wrong-identity. CI adds `govulncheck` and `staticcheck` as required checks, and all GitHub Actions in CI and release workflows are pinned to full commit SHAs with Dependabot keeping the pins and Go modules current.
- **Shared-host disclosure** — provider prompt/schema/summary/settings/log files and statebranch log tails are written `0o600`; the Gemini prompt is passed via stdin instead of argv, so it is no longer visible in the process list on a shared host.
- **Statebranch path confinement** — `discoverLogSources` confines log sources to the run directory and configured scratch roots, rejecting absolute, `..`-escaping, and symlink sources outside allowed roots with a diagnosable manifest entry.
- **Config-command hardening** — the domain evidence producer and custom liveness command accept an additive, no-shell `argv` array form (the legacy shell `command` string remains supported for backward compatibility); the evidence producer now runs under the process-group + hard-timeout supervisor.

### Fixed

- **Honest failure reporting** — `runJSON` treats empty-where-JSON-expected output as an error, with an explicit allow-empty opt-in for the idempotent GitHub merge API (both merge call sites); `CreateIssue`/`UpdateIssue` return the partial object plus an error when a mutation succeeds but its read-back fails; issue and PR list calls report truncation at the API limit instead of silently dropping data; Codex log-read errors are surfaced, and metadata-parse failure is distinguished from provider exec failure so attestation never silently reports zero usage.
- **Bounded local I/O** — hook stdin is capped with `io.LimitedReader`; `loopcoder status` scans only known run-record filenames bounded by size/mtime/depth and reports diagnosable errors on corrupt or oversized state; worktree-liveness directory walks skip `.git`/ignored directories, early-exit on the first newer mtime, and cap the number of files examined.

### Notes

- No behavior change to the code profile: absent config and absent release-signature assets behave exactly as before. The 0.4.2 H5 loopreview exit-code contract, the `Invocation.ReadOnly` verifier boundary, and the self-hosting guard are preserved. The audit's two "Critical" labels were, on inspection of the real code, overstated (the installer is checksum-gated with isolated extraction and copies only the top-level binary; statebranch log tails are scrubbed and sourced from loopcoder-authored run records), and real severities were set accordingly. New installs and `loopcoder upgrade` now require `cosign` on `PATH` to verify the release signature.

## [0.5.0] - 2026-07-04

Generalize loopcoder from a code-delivery loop into a general autonomous-delivery engine for any verifiable, repo-based, AI-doable work -- documents, content, data, governance packets, reports, and code -- via configurable **domain profiles**, per [`docs/specs/0459-domain-profiles.md`](docs/specs/0459-domain-profiles.md). Code becomes the first domain profile, not the definition of the engine. The core loop (`tick`, `compile`, `dispatch`, `dispatch-wave`, `loopreview`, `risk-gate`, `promote`, guardrails, watchdog, relay, recovery) keeps its ordering and authority unchanged; 0.5.0 only adds optional plug points those existing stages consume. An absent or empty `domain` section behaves exactly like the 0.4.x code profile.

### Added

- **Domain profile bundle** -- a new optional top-level `.delivery.yml domain` section declares a project's domain as a bundle of plug points (skills, verification rubric, evidence producer, red lines, partial-work/liveness policy) plus an `mcp` section. All fields are additive, optional, snake_case, and `omitempty`/`Default()`-safe (0161 M3).
- **Configurable skill sources** (plug point 1) -- `domain.skills` extends worker skill discovery with ordered repo globs (including recursive `**`), an optional machine-readable skill library, `select` filtering by normalized name/path-stem/tag, and a prompt byte budget. The `.claude/skills/*/SKILL.md` rule remains the default; discovery stays metadata-first and bounded (never inlines skill bodies past budget).
- **Injectable verification rubric** (plug point 2) -- `domain.verification.rubric` copies repo QA-checklist files and inline checklist items into a bounded "Rubric" review-packet section, and `review_packet_order` configures top-level section ordering (docs profiles can put rendered artifact + rubric before diff excerpts). A missing configured rubric file is missing evidence and forces `needs-human`. The closed verdict enum and H5 exit-code split are preserved.
- **Rendered-artifact evidence producer** (plug point 3) -- `domain.evidence.producer` runs a configured command in the PR worktree after worker output and before `loopreview`, collects an allow-list of declared outputs, and feeds a bounded rendered-artifact section into the verifier packet. `verification.browser` becomes a compatibility rendered-artifact producer class so document/data/content domains can feed their actual product to the verifier. A failed, timed-out, or absent declared output routes `needs-human`.
- **Append-only domain red lines** (plug point 4) -- `.delivery.yml domain.red_lines[]` append to the deterministic risk-gate floor via the existing `AdditionalRedLines` path with strict matcher validation. Domain red lines may only add vetoes; they cannot lower, rename, or bypass the built-in destructive, build-not-green, or loopcoder-core red lines (0161 M2/M4).
- **MCP servers** (plug point 5) -- an optional `mcp.servers` section plus a pure-append `MCPServers` field on `agent.Invocation` let workers and verifiers reach local stdio and external HTTP MCP servers. `roles` gates which invocations receive a server; remote auth must come from env vars or secret references (never hardcoded); loopcoder's local `read_only` classification -- not a server self-report -- decides verifier availability, and `Invocation.ReadOnly` remains the one permission boundary. Provider runners (codex/claude/gemini) translate invocation MCP servers into their native config and fail closed when `ReadOnly` cannot be represented safely.
- **Configurable partial-work and liveness policy** -- `domain.partial_work.mode` (`harvest-needs-human` default, or `report-only`) and `domain.liveness.mode` (`worktree-mtime` default, `log-only`, or `custom`) make the 0.4.2 H1/H2 fold-ins domain-configurable without weakening hard caps, guardrails, relay, or the rule that salvaged work is never auto-merged.
- **Docs-domain validation** -- an `examples/` docs domain profile plus validation proving the target end-to-end: governance spec, QA rubric, deterministic CI, rendered evidence, disclosure/compliance red lines, and promotion approval.

### Notes

- The core engine is unchanged: every plug point feeds an existing stage. The self-hosting guard (0161 M2/F4) classifies all domain-support machinery as loopcoder-core, routes it `needs-human`, and requires a human rebuild plus tick restart before it can affect a running loop -- so 0.5.0 was itself built through the 0.4.x loop under a human merge gate. Each of the nine code slices was built against spec 0459 and gated through `loopreview` (Opus 4.8 1M, read-only verifier, attestation verified) before landing.

## [0.4.2] - 2026-07-03

### Fixed

- **Operational reliability hardening** per [`docs/specs/0423-operational-reliability-hardening.md`](docs/specs/0423-operational-reliability-hardening.md) and #407.
- **H1 harvest-before-discard** -- hung or stalled workers harvest committable work before discard, opening a needs-human PR instead of losing a finished or partially finished deliverable.
- **H2 worktree-mtime liveness** -- worker and verifier supervision treats worktree file activity as progress in addition to log growth, with raised watchdog windows for realistic build/test cycles.
- **H3 source-first review packet** -- `loopreview` admits source/config diffs before generated or very large diffs so generated artifacts cannot consume the whole review budget ahead of the code under review.
- **H4 loud config resolution** -- both config loaders fail loud when `.delivery.yml` is absent from the working tree but present on the base branch, `doctor` reports the mismatch, and `--config-from-base` is the explicit opt-in to read config from base.
- **H5 distinguishable loopreview exit codes** -- `loopreview` now reserves `0`/`1`/`2` for clean verifier verdicts (`pass`/`fail`/`needs-human`) and returns `3` when the command itself fails, such as bad flags, bad `--repo`, config/provider/git setup failure, or output/relay write failure.
- **Relay-enforcement hard gate** per [`docs/specs/0447-relay-enforcement-hardgate.md`](docs/specs/0447-relay-enforcement-hardgate.md) -- mechanical commands refuse to proceed with reserved exit code `4` while unacknowledged local Worker/Verifier relay blocks are pending, and `loopcoder relay flush` / `loopcoder relay list` provide the foreground clear and inspection surfaces.
- **Relay guard coverage** -- `conductor-relay-guard` covers PowerShell/pwsh in addition to Bash and treats backgrounded `dispatch`, `dispatch-wave`, and `loopreview` output as pending until surfaced.
- **Foreground streaming dispatch-wave** -- `dispatch-wave` keeps workers concurrent while streaming each completed Worker's pretty attestation block to stdout as a contiguous unit, removing the need to background a wave just to keep parallelism.

### Notes

- The 0.4.2 hardening keeps the self-hosting model and human gate intact: harvested work is needs-human, verifier judgments remain explicit verdicts, and command failures are no longer confused with `fail` or `needs-human` verdicts by CI runners.

## [0.4.1] - 2026-07-03

### Changed

- **E2 auto-promote to production (default-on)** per [`docs/specs/0403-e2-auto-promote-production.md`](docs/specs/0403-e2-auto-promote-production.md): the default production promotion gate is now `auto`. An unset `adapters.gate` normalizes to `auto`, and newly scaffolded `.delivery.yml` files set `gate: auto`.
- The `auto` gate is deterministic, conjunctive, and veto-only over CI-green, `loopreview` pass, configured evidence present, and an independently re-evaluated red-line floor. Missing, unknown, or failing inputs still fail closed to `needs-human`; the policy can only add vetoes, never lower the floor.
- Production auto-rollback is deterministic and never LLM-driven: auto-promoted production merges record `merge_commit` and `prior_stable_commit`, and a failed post-promote check reverts production to the recorded prior-stable SHA.
- `human-merge` remains fully supported as the explicit opt-out for projects where humans choose production merges.
- loopcoder itself remains explicitly configured with `gate: human-merge`; 0.4.1 is human-gated for self-hosting safety even though new projects default to `auto`.

## [0.4.0] - 2026-07-03

The autonomous delivery loop, per [`docs/specs/0161-autonomous-delivery-loop.md`](docs/specs/0161-autonomous-delivery-loop.md). A human touches only two ends -- approving the plan and promoting to production -- while a deterministic orchestrator (`tick`) drives three LLM nodes (Planner = `compile`, Generator = worker, Evaluator = `loopreview`) around the loop in between.

### Added

- **`loopcoder tick`** -- one deterministic pass of the delivery loop: compile the ready set, dispatch a worker per ready issue, gate each result through the risk gate, and auto-merge passing work into the `pre-prod` branch. A tick never merges to `main` (invariant F1).
- **Risk gate** -- classifies each worker PR (diff size, dangerous commands, CI signal, core-path touch) and either auto-merges into `pre-prod` or escalates to `needs-human`. Blanket-guards `internal/orchestration/*.go` and other loopcoder-core paths as red lines requiring human review.
- **`pre-prod` environment + auto-revert-keeps-green** -- merged work lands on `pre-prod`, not `main`. When post-merge CI goes red and the failure is attributable to a specific merge, that merge is auto-reverted to keep `pre-prod` green; non-attributable or unknown failures escalate to `needs-human`. In-progress CI is left for a later tick.
- **`loopcoder promote`** -- the production gate (invariant F3, human-only): promotes the whole `pre-prod` batch to `main`, or kicks back individual PRs. Promotion is idempotent and ledgered (invariant E2); kick-back reverts are idempotent and matched by anchored PR/SHA parsing.
- **Failure-loop recover** -- a failed worker is retried up to a bounded number of attempts (default 2), same-config first and upgraded (higher effort) only on the final attempt, then escalated to `needs-human`.
- **Self-hosting guard** -- changes to loopcoder-core orchestration paths are a red line: the loop can propose them but requires a human and a rebuild to take effect, so the loop can never silently rewrite its own safety machinery (invariant F4).
- **Automation triggers** -- `cron`, goal-loop, and hook-driven entry points that run `tick` unattended within guardrail bounds.
- **Discover (D1)** -- turns CI failures into deduplicated issues, excluding held/closed trackers, so the loop can self-refill its own work queue.
- **Skills injection (D2)** -- relevant repo skills are injected into the worker prompt at dispatch.
- **Evidence + report** -- `.delivery.yml` carries evidence/preview metadata; `loopcoder report` surfaces pending-promotion, needs-human, failures, and evidence as a program-rendered panel.
- **Epic support** -- decomposition (`compile` emits a slice DAG with one plan approval), dependency graph and ordering (`go list` backbone, Kahn leaf-first, SCC condensation with graceful degradation on `go list`/parse failure), an equivalence gate for migrations (golden-master + differential + parallel-run), incremental migration discipline (Branch-by-Abstraction + build-tag toggle + dark-slice), and a batched promotion panel with reconciliation and a toggle inventory.
- **Process watchdog** (per [`docs/specs/0390-process-watchdog.md`](docs/specs/0390-process-watchdog.md)) -- no spawned CLI subprocess can hang forever. A shared `internal/supervisedexec` helper bounds every subprocess with a hard wall-clock cap and gives the worker/verifier LLM CLIs output-stall detection; a detected hang is killed and fed into the recovery loop as a distinct `hung` outcome (same-config retry within the bounded budget, then `needs-human`). Every spawned child is tagged `LOOPCODER_MANAGED` and placed in a per-run kill-group (a Windows Job Object with kill-on-close, or a Unix process group with Linux `PR_SET_PDEATHSIG`) so a whole subtree is reaped at once and orphans are cleaned up on crash. `SIGINT`/`SIGTERM` gracefully terminates the instance's children, and `loopcoder ps` / `loopcoder kill` operate only on loopcoder-managed processes -- never by bare process name. Per-project caps live under `resilience.worker` / `resilience.verifier` in `.delivery.yml`.

### Notes

- All five failsafe invariants ship enforced: F1 (tick never merges to `main`), F2 (the Evaluator/verifier runs read-only), F3 (promote is human-only), F4 (guardrail bounds on automation and self-modification), F5 (attestation on every LLM node). Every slice was built against spec 0161 and gated through `loopreview` (Opus 4.8 1M, read-only verifier, attestation verified) before landing.

## [0.3.9] - 2026-07-02

### Fixed

- The `conductor-attest` Stop hook no longer blocks or loops on ordinary planning and chat turns. Activating the hook in v0.3.8 exposed a design flaw: it demanded a Conductor self-attestation before *every* Stop in a conductor workspace and never honored Claude Code's `stop_hook_active` escape valve, so a session with no delivery — or any turn where the attestation was not recorded — could hard-lock the conversation. The gate now applies only to turns that actually ran a delivery or merge command (`loopcoder dispatch` / `dispatch-wave` / `loopreview`, or `gh pr merge`), blocks at most once and then self-clears, and honors `stop_hook_active`. `conductor-relay-guard` also honors `stop_hook_active` as a safety net. The gate can no longer loop or block non-delivery turns.

## [0.3.8] - 2026-07-01

### Changed

- Conductor hooks are now invoked via `loopcoder hook <name>` (embedded in the binary) instead of `node hooks/*.js`, fixing hooks that never resolved in consumer repos because `loopcoder skill install` never copied the `.js` files. The hook logic is a faithful Go port (package `internal/conductorhooks`); the `node` scripts are removed. `loopcoder doctor` now verifies the hook command form and that `loopcoder` resolves on `PATH` instead of only matching the command string, and `loopcoder skill install` merges an idempotent upgrade that strips stale `node hooks/*.js` entries and writes a gitignored `.loopcoder/conductor-workspace` marker that activates auto-enforcement in installed repos.

## [0.3.7] - 2026-07-01

### Added

- Conductor local enforcement per [`docs/specs/0316-conductor-local-enforcement.md`](docs/specs/0316-conductor-local-enforcement.md): the `conductor-relay-guard` hook backstops hidden Worker and Verifier attestation blocks from `dispatch` and `loopreview`, while `conductor-attest` remains the Conductor self-attestation gate before a delivery or merge turn completes.
- `loopcoder status` renders read-only delivery run status from gitignored `.loopcoder/` state so conductors report a program-rendered local surface instead of a hand-typed table.

### Changed

- `loopcoder skill install --repo <project>` now wires both conductor hooks into project `.claude/settings.json`, preserving unrelated settings, and `loopcoder doctor` warns when either conductor hook is missing.

### Notes

- Attestation and status remain local-only. The relay guard, status output, and Conductor attestation records have no PR body, issue, comment, commit, merge artifact, docs, fixture, or tracked-file footprint.

## [0.3.6] - 2026-07-01

### Changed

- Attestation is now local-only per [`docs/specs/0306-local-only-attestation.md`](docs/specs/0306-local-only-attestation.md): PR bodies, merge commits, and merge comments no longer carry the attestation header or canonical JSON. Attestation is surfaced only via the stderr pretty block, the `dispatch` / `loopreview` result JSON, and gitignored `.loopcoder/` run records.

### Notes

- Machine contracts changed: PR bodies and merge artifacts are removed from the attestation contract; canonical JSON, `Header()`, validation, fail-closed behavior, and the local stdout / result-JSON surfaces are otherwise unchanged.

## [0.3.5] - 2026-06-30

### Fixed

- `loopcoder skill install` now updates a stale managed skill file instead of silently skipping it; `loopcoder upgrade` refreshes the bundled conductor skill from the newly selected binary; and `loopcoder doctor` warns on stale or partial installs per [`docs/specs/0291-skill-propagation-on-upgrade.md`](docs/specs/0291-skill-propagation-on-upgrade.md). Upgrading the binary now propagates the conductor playbook.
- Claude attestation now reports the pinned/configured model when that model is present in the provider's reported usage, instead of attributing the invocation to a token-dominant auxiliary model per [`docs/specs/0300-model-attribution.md`](docs/specs/0300-model-attribution.md).

### Changed

- The human-readable attestation block now shows the provider vendor (OpenAI/Anthropic/Google) plus the CLI `tool`, renders model source as `(detected)` or `(self-reported)`, uses host-local timestamps to the second, reports duration in seconds, and uses thousands-separated token counts with a derived total when only input/output are reported per [`docs/specs/0296-attestation-display-polish.md`](docs/specs/0296-attestation-display-polish.md).
- `.delivery.yml` pins the verifier model and effort with `model: "claude-opus-4-8[1m]"` and `reasoning_effort: max`.

### Notes

- Machine contracts are unchanged: canonical JSON, the `[attestation]` header, validation, and fail-closed behavior keep their existing behavior.

## [0.3.4] - 2026-06-30

### Changed

- `dispatch`, `loopreview`, and `dispatch-wave` now emit the human-readable pretty attestation block to stderr by default per [`docs/specs/0282-default-pretty-attestation.md`](docs/specs/0282-default-pretty-attestation.md). The default uses emoji on a TTY and plain ASCII on non-TTY output.
- `--pretty` and `LOOPCODER_PRETTY` force emoji pretty output even on non-TTY output; `--no-pretty` and `LOOPCODER_NO_PRETTY` suppress pretty output and win over force.
- `dispatch-wave` emits one pretty Worker attestation block per dispatched issue.
- The conductor playbook now relays Worker and Verifier pretty attestation blocks verbatim from command stderr instead of hand-formatting attestation report lines.

### Notes

- Machine contracts are unchanged: canonical JSON, `Header()` / `[attestation] ...`, PR bodies, verifier JSON, and fail-closed attestation validation keep their existing behavior.

## [0.3.3] - 2026-06-29

### Added

- `loopreview` now builds a bounded review packet per [`docs/specs/0194-reliable-loopreview-verifier.md`](docs/specs/0194-reliable-loopreview-verifier.md) and #202, including bounded changed-file, issue, merged-spec, and diff excerpts with visible truncation markers. If the packet is insufficient for a safe verdict, `loopreview` returns `needs-human` without invoking the provider.
- `codex` and `claude` are verified `loopreview` verifier providers in the mechanism sense per #205: each can return a valid structured verdict plus Verifier attestation within the timeout.
- A tag-triggered release workflow builds Windows, macOS, and Linux binaries for amd64 and arm64 and publishes `SHA256SUMS` per [`docs/specs/0212-release-distribution-and-upgrade.md`](docs/specs/0212-release-distribution-and-upgrade.md).
- No-Go install scripts, `scripts/install.sh` and `scripts/install.ps1`, install from GitHub Releases with checksum verification per spec 0212.
- `loopcoder version` plus root `--version` and `-v` print the selected binary version, commit, build date, Go version, and platform.
- `loopcoder doctor` runs a read-only preflight reporting `git`, `gh` authentication, configured provider CLIs, the origin remote and detected default branch, `.delivery.yml` validity, binary version and `min_loopcoder_version` compatibility, and conductor-runtime ownership per [`docs/specs/0212-release-distribution-and-upgrade.md`](docs/specs/0212-release-distribution-and-upgrade.md).
- `dispatch` now surfaces Worker attestation with the stable header, canonical JSON, and final result JSON `attestation` object per [`docs/specs/0218-surface-worker-attestation.md`](docs/specs/0218-surface-worker-attestation.md).
- Human-readable attestation pretty rendering, including `--pretty` on `dispatch`, `loopreview`, and `attest`, per [`docs/specs/0214-human-readable-attestation.md`](docs/specs/0214-human-readable-attestation.md). `dispatch` and `loopreview` keep machine stdout stable and write the pretty display to stderr; `attest --pretty` is an explicit opt-in and the default durable output is unchanged.
- Optional `.delivery.yml` `verifier.model` and `verifier.reasoning_effort` settings configure the verifier role per [`docs/specs/0215-per-role-model-override.md`](docs/specs/0215-per-role-model-override.md).
- Worker token usage captures input and output splits when the provider exposes them per spec 0218.
- [`docs/reference/stability-policy.md`](docs/reference/stability-policy.md) documents the 0.x compatibility policy for `.delivery.yml`, CLI flags, and labels.
- `loopcoder init` scaffolds `.delivery.yml` and `ROADMAP.md`, ensures the default labels, and can persist first-run worker and verifier model and effort defaults per spec 0212 and [`docs/specs/0215-per-role-model-override.md`](docs/specs/0215-per-role-model-override.md).
- The conductor playbook (`SKILL.md` and `AGENTS.md`) is embedded in the binary, and `loopcoder skill install` writes it to the Claude skill directory per spec 0212.
- `loopcoder upgrade [--version]` self-updates from GitHub Releases with checksum-before-install verification, an atomic swap, and a Windows deferred-swap fallback per spec 0212.
- A `~/.loopcoder` home with a versioned binary store, `LOOPCODER_HOME` and `LOOPCODER_BIN` resolution, and semver-aware version ordering per spec 0212.
- `dispatch` and `loopreview` accept provider-agnostic `--model` and `--effort` overrides for per-run model and reasoning-effort selection per spec 0215.
- `dispatch-wave` surfaces each worker's attestation facts (provider, model, effort, permission, duration, token usage, and verified) per spec 0218.
- A "Quickstart (new project)" guide in [`docs/reference/usage.md`](docs/reference/usage.md) documents install, `doctor`, `skill install`, per-repo `init`, and driving the loop per spec 0212.

### Changed

- Verifier provider invocation is read-only and headless-hardened per #204: `claude` uses `--print` with a `Read Grep Glob` allowlist and no plan mode, and `codex` uses `exec -s read-only`.
- `.delivery.yml adapters.verifier` now uses the real `claude` provider instead of the invalid `opus` value per spec 0215.
- Release and CI workflows use GitHub Action versions that no longer rely on the deprecated Node 20 runtime per spec 0212.

### Fixed

- Follow-up loopreview reliability polish from #208 and #209, including clearer documentation wording and visible omitted-file names when diff packet content is truncated.
- `loopreview` no longer forces `needs-human` for a brand-new doc-first spec PR whose referenced spec is naturally absent from the base branch; code PRs with a missing merged spec still return `needs-human` per [`docs/specs/0220-loopreview-new-spec-not-a-blocker.md`](docs/specs/0220-loopreview-new-spec-not-a-blocker.md).

### Notes

- The verified-provider proof is about the verifier mechanism, not deterministic model judgment. The LLM `pass` or `fail` verdict itself remains non-deterministic across otherwise valid runs.
- This release line also accepted design specs 0212, 0214, 0215, 0218, and 0220.

## [0.3.2] - 2026-06-28

### Added

- Delivery guardrails per [`docs/specs/0192-delivery-guardrails.md`](docs/specs/0192-delivery-guardrails.md), #198, and #203. `.delivery.yml guardrails.budget` can opt in to `max_runs`, `max_total_attempts`, `max_total_tokens`, and `max_total_cost_usd`; token accounting consumes attestation usage, cost caps are exact-only, and missing or corrupt evidence fails closed to `needs-human`.
- `.delivery.yml guardrails.circuit_breaker` can opt in to no-progress streak thresholds that freeze only the affected issue and require human input before more work is dispatched for that issue.

### Changed

- `dispatch-wave` and `recover` enforce guardrails as pre-dispatch gates and reuse a guardrail ledger for decisions. `ready-set` and `resume` surface budget-blocked or circuit-frozen issues as `needs-human` / `guardrail-frozen` instead of marking them ready.

## [0.3.1] - 2026-06-28

### Added

- Per-invocation attestation for Worker, Verifier, and Conductor roles per [`docs/specs/0146-attestation.md`](docs/specs/0146-attestation.md): worker PR bodies and verifier verdicts carry binary-stamped records with provider, parsed model, effort, permission, duration, and token usage; `loopcoder attest` emits Conductor self-attestation; the Conductor hook enforces the self-attestation step; and missing required identity or usage fails closed with no worker PR, a `needs-human` verifier verdict, or a non-zero `attest` exit.

## [0.3.0] - 2026-06-28

### Added

- Provider-neutral agent abstraction and registry for dispatch and verification, with actionable errors for unknown providers.
- `claude` verified worker adapter and experimental/unverified `gemini` worker adapter alongside the default `codex` adapter.
- Independent `loopcoder loopreview` verifier command that checks a PR branch in read-only mode, emits a structured `pass`, `fail`, or `needs-human` verdict when the verifier completes, and degrades a slow or hung verifier to `needs-human` at the timeout.
- `.delivery.yml adapters` role slots for `conductor`, `worker`, and `verifier`, plus a reviewer-not-worker advisory warning when the verifier is configured to match the worker.

### Changed

- Worker output and repo-facing artifacts are documented as English.
- Worker `--provider`, `--model`, and `--effort` behavior is provider-specific: `codex` remains the default, `claude` can honor effort, and experimental/unverified `gemini` ignores effort with an advisory.
- Documentation now describes loopcoder in runtime- and ecosystem-agnostic terms, removing paseo and internal-ecosystem framing from the user-facing surface.

### Notes

- The `gemini` adapter is present and registered, but it was not verified end-to-end because the Gemini CLI was not usable in the development environment due to missing authentication.
- `loopreview` ships as a command with a working timeout safety net. LLM verifier provider reliability is experimental in v0.3.0: a real `claude` verifier run did not complete within the 180s timeout and returned `needs-human`, and `gemini` verification is unverified. Reliable provider verification is a v0.3.1 follow-up.

## [0.2.0] - 2026-06-27

### Added

- Native cross-platform `loopcoder` Go binary with subcommands at parity with the v0.1.x PowerShell helpers: `dispatch`, `ready-set`, `resume`, `recover`, `verify-local`, plus native `dispatch-wave` (one-wave orchestration) and `state` / `lease` (cross-session state branch + conductor lease per docs/resilience.md).
- Cross-platform Codex execution: `exec.Command` with a real file-handle stdin (the portable closed-stdin fix), replacing the Windows `cmd /c` redirection.
- Cross-platform worktree-add lock via `github.com/gofrs/flock`, replacing the Windows named mutex.
- A CI `go` job (build / vet / test) and `.delivery.yml ci.checks: [verify, go]` so Go code is gated.
- `go install github.com/jasonhnd/loopcoder/cmd/loopcoder@latest` distribution.
- Secret scrubbing + recovery briefs, durable run state, and bounded retry ported to Go with deterministic unit tests.

### Changed

- SKILL.md backend selection: the conductor calls the `loopcoder` binary (resolution: `LOOPCODER_BIN` -> `loopcoder` on `PATH`, required on all platforms including Windows). Removed the PowerShell helper layer (`scripts/*.ps1`); the `loopcoder` binary is the sole mechanical backend. The CI `verify` job was de-PowerShelled (now runs in bash). The conductor model (human-merge only, doc-first, observe-at-merge, model/effort inheritance, verification gate) is unchanged; only the helper command names changed.

### Notes

- Before removing the PowerShell layer, the native binary was validated end-to-end: built locally, then ran `loopcoder dispatch`, producing a real PR via `codex` + `git` + `gh`.
- Command parity is covered by unit tests and the `go` CI gate; real-codex end-to-end is validated by the operator on their platform.

## [0.1.2] - 2026-06-26

### Added

- `docs/verification.md`: design for the verification & quality-gate layer (required checks, spec-driven conformance, agent/browser verification, pass/fail/needs-human verdicts).
- `docs/self-improvement.md`: design for a bounded, human-gated self-improvement loop (append-only learnings, reflection-as-proposal, off-limits targets).
- `docs/resilience.md`: design for resilience (worker heartbeat, stuck/hung/idle detection, bounded retry with recovery context, GitHub-first crash recovery).
- `docs/learnings.md`: append-only operational learnings file with entry template and advisory-authority order.
- SKILL.md "Learnings (self-improvement)" subsection: conductor read path (relevant excerpts, advisory) and human-approved, separate-PR close-out write path.
- SKILL.md "Worker liveness & recovery" subsection: stale/hung/idle classification, idle-is-not-done, bounded retry, GitHub-first resume.
- `scripts/dispatch-worker.ps1`: per-attempt heartbeat/attempt JSON sidecar written at phase boundaries (job_id, phase, status, started_at/heartbeat_at/last_progress_at, log_bytes, exit_code, error); attempt fields added to the success JSON; failed-attempt artifacts preserved.
- `.delivery.yml`: optional commented configuration surfaces for `ci` (tests/typecheck/build), `verification`, and `resilience`.
- `.github/workflows/ci.yml`: required `verify` check covering PowerShell parse validation for scripts and YAML validity.
- `docs/learnings.md`: first three append-only operational learning entries from the v0.1.2 run.
- SKILL.md "Improvement review" subsection: bounded, human-gated self-improvement M3 reflection pass that drafts improvement candidates with evidence, target, risk, and verification.
- `scripts/dispatch-worker.ps1`: durable run state under `.loopcoder/runs/<RunId>/workers/*.attempt.json` plus append-only `.loopcoder/runs/<RunId>/events.jsonl`; added `-RunId` batch grouping and gitignored `.loopcoder/`.
- Resilience recovery: `scripts/dispatch-worker.ps1` writes secret-scrubbed recovery briefs under `.loopcoder/runs/<RunId>/recovery/`; new `scripts/recover-and-retry.ps1` adopts an existing PR first, otherwise retries with backoff up to the configured maximum and blocks after max attempts.
- `scripts/resume.ps1`: read-only GitHub-first resume/reconcile report that combines GitHub and local run state, classifies attempts as `done`, `in-review`, `running`, `stale`, `hung`, `orphaned`, or `ready`, and prints next ready actions without dispatching or merging.

### Changed

- SKILL.md verification: the verifier procedure now enforces required `ci.checks` and spec conformance against the referenced merged design doc, and ends every PR review with an explicit `pass`/`fail`/`needs-human` verdict and fix-pass routing, instead of advisory-only review. Human-merge remains the only merge gate.
- `.delivery.yml`: `ci.checks` now declares `[verify]`, so the verification gate enforces green-before-merge-eligible instead of remaining inert with empty checks.
- The `verify` CI job now also asserts that every `.delivery.yml` `ci.checks` name maps to a real workflow job id, so gate config drift (a renamed or removed required check) fails CI loudly instead of silently stalling the conductor.

### Fixed

- Recovery briefs written by `scripts/dispatch-worker.ps1` now use proper triple-backtick fenced code blocks (the brief here-string previously emitted collapsed fences for the changed-files, PR-status, and log-tail sections).

## [0.1.1] - 2026-06-26

### Added

- Mandatory doc-first process in `docs/PROCESS.md` and the `SKILL.md` "Process discipline" section, requiring document-first work, separate code implementation, and final verification.
- Documentation set for current v1 behavior: `docs/architecture.md`, `docs/worker.md`, `docs/usage.md`, and `docs/scheduling.md`.
- Optional worker model and speed overrides through `-Model` and `-Effort` in `scripts/dispatch-worker.ps1`; when absent, Codex inherits the user's global config, and loopcoder does not choose for the user.
- Scheduler playbook coverage in `SKILL.md` for layered ready-set dispatch, observe-at-merge ordering, and conflict eviction, per `docs/scheduling.md`.
- MIT `LICENSE`.

### Changed

- Serialized git worktree creation in `scripts/dispatch-worker.ps1` with a per-repo mutex so concurrent worker dispatches do not race on `git worktree add`.

## [0.1.0] - 2026-06-26

### Added

- Worker adapter in `scripts/dispatch-worker.ps1` for the issue -> git worktree -> Codex -> commit -> push -> PR flow.
- Conductor playbook in `SKILL.md` for planning issues, dispatching workers, reviewing PRs, reporting progress, and merging only on user instruction.
- `.delivery.yml` configuration for v1 adapters, worker defaults, checks, and chat reporting.
- Ports and adapters architecture covering work items, workspaces, workers, VCS hosting, verification, gate, and reporting.
- `ROADMAP.md` template for human-written work units and dependency planning.
- v1 design spec in `docs/specs/2026-06-26-loopcoder-v1-design.md`.
- Self-hosting materials: loopcoder built its own `SKILL.md`, `.delivery.yml`, `README.md`, and `ROADMAP.md`.
