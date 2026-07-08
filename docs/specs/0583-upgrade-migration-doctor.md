---
id: 583
title: 0.6.0 Unit C - Upgrade, Migration, And Doctor
status: draft
date: 2026-07-08
issue: 583
pr: null
supersedes: []
superseded_by: []
---

# 0.6.0 Unit C - Upgrade, Migration, And Doctor

This is a design-only spec for loopcoder 0.6.0 Unit C. This PR adds only this
document: no Go code, no `.delivery.yml` change, no command behavior change, no
reference-doc update, no `CHANGELOG.md` rewrite, and no `README.md` rewrite.
Code and release-documentation work must be filed separately after this spec
merges, per [`docs/PROCESS.md`](../PROCESS.md).

Unit C implements the upgrade, migration, stale-local-state cleanup, and
operational-health definition called out in [`ROADMAP.md`](../../ROADMAP.md)
section "0.6.0 - Upgrade, migration & operational health (doctor)".

0.6.0 is loopcoder's first BREAKING release because Unit B renames the live
operator-facing attestation subsystem to reporter. The upgrade must be boring:
frozen machinery remains readable and unchanged, old names keep working for one
release, and operators get a clear `doctor` report before anything mutates.

## Goals

- Define a clean 0.5.x to 0.6.0 upgrade path for reporter-rename surfaces.
- Detect old binary/config/state surfaces and report whether migration is
  needed.
- Forward-migrate renamed configuration keys on read and on write without data
  loss.
- Keep the frozen `.attest` relay ledger extension and canonical report JSON
  field names untouched.
- Preserve Unit B's one-version dual-token and dual-env transition window across
  the 0.5.x to 0.6.0 boundary.
- Define bounded stale-local-state cleanup for run logs, worktree-liveness
  artifacts, and relay state under an explicit retention policy.
- Define `loopcoder doctor` as an operator-run readiness check whose default is
  read-only and safe anytime.
- Define `doctor --fix` as the only doctor mode that performs upgrade,
  config-key migration, or stale-log cleanup mutations.
- Establish the 0.6.0 release-documentation obligation and the standing
  per-release rule for changelog, release note, and README updates.

## Non-Goals

- No Go implementation in this design PR.
- No rewrite of `CHANGELOG.md`, release notes, `README.md`, or
  `docs/reference/releasing.md` in this PR.
- No rewrite of shipped `docs/specs/*` history.
- No rename of `.loopcoder/relay/*.attest`.
- No rename of existing `Report.CanonicalJSON()` field names.
- No destructive cleanup of active runs, pending relay obligations, or recovery
  state that is still inside the retention window.
- No new role named `doctor`; doctor is an operator preflight command, not an
  LLM reporting role.
- No weakening of Worker, Verifier, Conductor reporter validation, relay
  hard-gate behavior, or local-only report handling.

## Terms

**Upgrade** means selecting or installing the 0.6.0 loopcoder binary from a
previous selected version, typically 0.5.x, using `loopcoder upgrade`.

**Migration** means rewriting repo-local or host-local configuration surfaces
from legacy 0.5.x names to 0.6.0 names. Reading legacy names as aliases is
compatibility; writing the new names is migration.

**Frozen machinery** means the invisible machine contracts Unit B deliberately
left unchanged: `.loopcoder/relay/*.attest` ledger extensions, canonical JSON
field names, existing `.loopcoder/` records, and shipped historical specs.

**Stale local state** means gitignored operator-machine artifacts that are old
enough and inactive enough to prune without changing GitHub issues, PRs,
branches, tracked files, or reporter schema.

## Upgrade Model

The 0.6.0 binary must understand how to operate against 0.5.x repositories and
host-hook installs without requiring a manual one-shot edit before dispatch.

When `loopcoder upgrade` selects 0.6.0, the target state is:

1. The selected binary reports version `0.6.0`.
2. Bundled skills are refreshed per
   [`0291-skill-propagation-on-upgrade.md`](0291-skill-propagation-on-upgrade.md).
3. Current hook installation uses `loopcoder hook conductor-reporter` and
   `loopcoder hook conductor-relay-guard`.
4. Old hook commands, old env vars, old result/state keys, and old
   `[attestation]` tokens are still accepted for exactly the 0.6.0 transition
   release.
5. `doctor` reports any remaining old surfaces and the exact command that fixes
   them.

An already-published 0.5.x binary cannot contain new 0.6.0 migration logic. The
compatibility requirement is therefore two-layered:

- 0.6.0 must remain compatible with 0.5.x state immediately after the binary
  swap, so upgrade-lag cannot lock out dispatch, relay recovery, or doctor.
- The selected 0.6.0 binary must expose the shared migration through both
  `loopcoder upgrade` when it is running the upgrade code path and
  `loopcoder doctor --fix` for the first post-upgrade repair pass.

Release docs must tell users upgrading from 0.5.x to run:

```text
loopcoder upgrade --version 0.6.0
loopcoder doctor --repo .
loopcoder doctor --repo . --fix
```

`doctor --fix` is idempotent; running it when no migration is needed reports
`unchanged` / `not-needed` and exits successfully.

## Old-Version Detection

The upgrade and doctor implementations must detect version state from multiple
sources because not every machine has the same install path.

Required detection inputs:

- selected binary build info: version, commit, date, and selected executable
  path;
- requested upgrade target, when `loopcoder upgrade --version` is used;
- `.delivery.yml` schema `version` and optional `min_loopcoder_version`;
- installed managed skill content compared with the selected binary embedded
  `SKILL.md` and `AGENTS.md`;
- installed project hook commands in `.claude/settings.json`;
- repo-local `.loopcoder/` state containing old `attestation` result keys or
  old hook state labels; and
- legacy reporter env vars visible in the current process environment.

Version classification:

| Selected version | Classification | Required behavior |
|---|---|---|
| `0.5.x` | pre-breaking | Report that upgrade to 0.6.0 is available or required when target is 0.6.0; do not attempt 0.6-only mutations from the old binary. |
| `0.6.0` | breaking transition | Accept old and new names; report migration status; `--fix` may rewrite old config/hook/state names. |
| `>0.6.0` | post-transition | A later spec decides when old aliases are removed. Unit C must not remove them. |
| `dev`, `unknown`, or unparsable | unknown | Warn, keep compatibility behavior, and avoid destructive assumptions. |

Old-version detection is diagnostic unless `--fix` is explicit. It must not
rewrite `.delivery.yml`, hooks, relay state, or run state while rendering the
default doctor report.

## Rename And Migration Map

Unit B's rename map is the source of truth for live reporter names. Unit C adds
the upgrade handling for those names.

| Legacy 0.5.x surface | 0.6.0 surface | Read behavior in 0.6.0 | Write / `--fix` behavior |
|---|---|---|---|
| Emitted `[attestation]` header token | `[reporter]` | Accept both tokens in relay and conductor hook matchers. | New output emits only `[reporter]`; no bulk rewrite of old output. |
| Command/result JSON key `attestation` | `report` | Readers accept both keys, preferring `report` when both exist. | New writes use `report`; sidecars rewritten for other reasons must write `report`. |
| Persisted attempt/review record field `attestation` | `report` | Readers accept both keys, preferring `report`. | New writes use `report`; `doctor --fix` may rewrite eligible sidecars only when inside the config/state migration slice and preserving all other fields. |
| `LOOPCODER_CONDUCTOR_ATTEST_SCOPE` | `LOOPCODER_CONDUCTOR_REPORTER_SCOPE` | Accept both env vars for one release; if both are set, prefer the new env var and warn about the old one. | Env vars cannot be rewritten; doctor reports the shell-specific fix command text. |
| `LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR` | `LOOPCODER_CONDUCTOR_REPORTER_STATE_DIR` | Accept both env vars for one release; if both are set, prefer the new env var and warn about the old one. | Env vars cannot be rewritten; doctor reports the shell-specific fix command text. |
| Hook command `loopcoder hook conductor-attest` | `loopcoder hook conductor-reporter` | Old command remains an alias for one release and runs reporter-aware hook logic. | `loopcoder skill install --repo <repo>` and `doctor --fix` write the new hook command. |
| Hook state label/path `conductor-attest` under `.loopcoder/hooks/` | `conductor-reporter` | Read old state if new state is absent. | Move or copy to the new state label atomically, preserving timestamps/content; leave no duplicate when the move succeeds. |
| `.delivery.yml` root `attestation:` | `.delivery.yml` root `report:` | Accept as a legacy alias if present. If both exist, `report` wins and doctor warns. | Rewrite to `report:` preserving nested values and comments where practical. |
| `.delivery.yml` key `attestation.channel` | `report.channel` | Accept as a legacy alias if present. If both exist, `report.channel` wins and doctor warns. | Rewrite to `report.channel`; remove the old key after a successful write. |
| Any future Unit B-era config key containing `attestation` | Same path with `report` / `reporter` according to the owning config section | Must be listed in the migration table before implementation. | Must have an idempotent migration test before release. |

Current tracked `.delivery.yml` uses `report.channel`, and no tracked
`.delivery.yml` reporter-rename key is expected to require migration. The
legacy `attestation:` entries above exist to make user-owned 0.5.x configs and
fixtures safe if they appear in the wild.

The `loopcoder attest` command verb remains a one-version compatibility alias
for Conductor self-report emission. Unit C does not rename the command verb; it
only ensures doctor and upgrade explain that the output is a report.

## Frozen Surfaces

The following must be no-op migrations:

- `.loopcoder/relay/*.attest` ledger extension stays `.attest`;
- old `.attest` files remain readable;
- canonical JSON field names stay unchanged, including `role`, `provider`,
  `model`, `model_source`, `effort`, `permission`, `action`, `exit_code`,
  `started_at`, `ended_at`, `duration_ms`, `usage`, and `verified`;
- `usage.input_tokens`, `usage.output_tokens`, and `usage.total_tokens` stay
  unchanged;
- accepted historical specs under `docs/specs/*` are not terminology-swept;
- `CHANGELOG.md` history is not terminology-swept; and
- old local run records are not bulk-rewritten unless they are part of an
  explicit migration target above.

No migration may rename `.attest` to `.report`, `.reporter`, `.json`, or any
other extension. The extension is invisible machinery and changing it creates
transition risk without operator benefit.

## Config-Key Migration Semantics

Config migration must be implemented as a shared core used by config loading,
config writing, `loopcoder upgrade`, and `doctor --fix`.

Read semantics:

- Parse legacy and new keys into the same in-memory config shape.
- Prefer the new key when both old and new keys are present.
- Emit a diagnostic identifying the old key, the new key, and the fix command.
- Do not rewrite files during ordinary reads.

Write semantics:

- Any command that writes `.delivery.yml` after this spec must write only the
  new key names.
- `doctor --fix` may rewrite `.delivery.yml` solely to perform the migration
  map above.
- Rewrites must preserve unrelated keys and values.
- Rewrites must be atomic: write a same-directory temp file, fsync/close where
  supported, then rename into place.
- If comments cannot be perfectly preserved, the command must avoid rewriting
  unrelated sections and must report that only legacy key paths were normalized.
- If both old and new keys exist with different values, the new value wins; the
  old key is removed; doctor reports the conflict and chosen value.

Config migration is not a schema-version bump. `.delivery.yml version: 1`
remains valid unless a later spec changes the schema.

## Old-File Handling And Retention

Unit C cleanup is for gitignored local state only. It must never mutate tracked
files, GitHub issues, GitHub PRs, branches, tags, release assets, provider auth
stores, or the user's shell profile.

### Retention Policy

Default retention policy:

- Retain every local run directory newer than 30 days.
- Retain at least the newest 50 run directories even when older than 30 days.
- Retain every run with any non-terminal attempt or any still-live recorded PID.
- Retain every run referenced by pending relay state.
- Retain every recovery brief newer than its owning run retention window.
- Retain every pending relay record until it is surfaced with
  `loopcoder relay flush`; cleanup must not silently acknowledge or delete an
  unrelayed obligation.
- Retain `.attest` ledgers newer than 30 days.
- Retain at least the newest 100 `.attest` ledger files even when older than
  30 days.
- Retain audit LLM logs under `.loopcoder/audit/` for 30 days, then prune old
  logs only when they are not referenced by current audit output.
- Retain old worktree-liveness artifacts for 7 days.

The implementation may expose explicit retention flags later, but the default
policy above is the 0.6.0 contract. If flags are added in the implementation
slices, they must be more conservative by default and must be documented in the
release-docs slice.

Terminal attempt statuses are `succeeded`, `failed`, `hung`, `idle`, `blocked`,
and `needs-human`. Unknown status is not terminal.

### Cleanup Targets

Eligible cleanup targets:

- `.loopcoder/runs/<run-id>/` directories that are outside the retention window
  and terminal;
- `.loopcoder/runs/<run-id>/workers/*.attempt.json` only as part of deleting an
  eligible owning run directory;
- `.loopcoder/runs/<run-id>/events.jsonl` only as part of deleting an eligible
  owning run directory;
- `.loopcoder/runs/<run-id>/recovery/` only as part of deleting an eligible
  owning run directory;
- old `.loopcoder/worktree-liveness/**` files if that directory exists from
  older versions and files are older than 7 days;
- acknowledged relay ledgers under `.loopcoder/relay/<run-id>/*.attest` that
  are outside relay retention; and
- old `.loopcoder/audit/llm-*.log` logs outside retention.

Non-cleanup targets:

- `.loopcoder/relay/pending/*.json` pending records;
- `.loopcoder/relay/**/*.attest` files inside retention;
- any `.attest` file schema, header, or extension;
- any worktree registered by `git worktree list`;
- any scratch directory not provably created by loopcoder; and
- any file outside the target repo's `.loopcoder/` tree unless a recorded
  attempt/recovery record points to a loopcoder-created temp directory and the
  implementation proves the path is safe to remove.

Cleanup must be bounded. Directory walkers must have entry-count and file-size
limits comparable to the existing runstatus/reportquery limits, must skip
unreadable entries with diagnostics instead of panicking, and must never follow
symlinks out of `.loopcoder/`.

## `loopcoder doctor`

`loopcoder doctor` is an operator-run operational-health command. It is not a
Worker, Verifier, Conductor, audit reviewer, or any other reporter role. It
must not emit a Worker/Verifier/Conductor report, must not satisfy a reporter
obligation, and must not create a new reporter obligation.

Default behavior is diagnose-only:

```text
loopcoder doctor --repo .
```

The default command is read-only and safe anytime. It may read local files,
inspect git configuration, resolve executables on `PATH`, and run bounded
read-only provider/auth probes. It must not rewrite config, install hooks,
flush relay records, delete logs, upgrade binaries, call GitHub mutations,
launch workers, or touch provider auth stores.

### Required Checks

Doctor must report at least these checks:

| Check | Diagnose-only behavior | Example fix command |
|---|---|---|
| `git` availability | Resolve `git` on `PATH`; fail if absent. | Install Git and reopen the shell. |
| Git repo context | Confirm `--repo` is a git worktree and inspect origin/default branch when available. | `git remote add origin <url>` or run from the repo root. |
| `gh` availability and auth | Resolve `gh`; run bounded `gh auth status`. | `gh auth login` |
| Provider CLI availability | Resolve configured Worker and Verifier provider CLIs from the model registry. | Install `codex`, `claude`, or `agy`. |
| Codex auth/reachability | Run a bounded non-mutating Codex readiness probe. | `codex login` |
| Claude auth/reachability | Run a bounded non-mutating Claude readiness probe. | `claude login` |
| Antigravity auth/reachability | Run bounded `agy models` for configured `antigravity`. | `agy login` |
| `.delivery.yml` validity | Parse config, report schema errors, and detect legacy keys. | `loopcoder doctor --repo . --fix` |
| Model/depth validity | Validate Worker and Verifier model/depth against Unit A registry; respect `models.strict`. | Edit `.delivery.yml` or use `loopcoder models`. |
| Reporter/relay wiring | Check current hook commands, legacy aliases, pending relay state, and `loopcoder` on `PATH` for host hooks. | `loopcoder skill install --repo .` or `loopcoder relay flush --repo .` |
| Version status | Report selected binary version, track, commit/date, `.delivery.yml` min version, and 0.6.0 migration status. | `loopcoder upgrade --version 0.6.0` |
| Stale local state | Count cleanup-eligible run logs, liveness artifacts, relay ledgers, and audit logs without deleting them. | `loopcoder doctor --repo . --fix` |
| Installed skill freshness | Compare installed managed skill files with selected binary embedded files. | `loopcoder skill install` |

For Codex and Claude, the implementation must choose the cheapest stable
read-only/auth-status probe available in the provider adapter at implementation
time. If a provider does not expose a stable auth-status command, doctor must
say that auth could not be conclusively probed instead of claiming success. A
missing or failed auth probe is still reported with the fix command
`codex login` or `claude login`.

Every problem must include:

- status: `info`, `ok`, `warn`, or `fail`;
- a short problem description;
- whether it blocks Worker dispatch, Verifier review, conductor hook
  enforcement, or only cleanup hygiene; and
- the exact fix command when loopcoder can provide one.

Doctor's exit code must be non-zero only for hard failures that block normal
loopcoder operation, such as missing `git`, invalid `.delivery.yml`, strict
model validation rejection, or a configured provider that cannot be reached.
Warnings such as stale logs or old-but-still-accepted env vars do not fail the
default command.

### `doctor --fix`

`doctor --fix` is the explicit opt-in mutating mode:

```text
loopcoder doctor --repo . --fix
```

It may perform only the mutations defined in this spec:

- run the config-key migration map;
- update project hook settings from legacy `conductor-attest` to
  `conductor-reporter` by delegating to the same managed hook-install logic as
  `loopcoder skill install --repo <repo>`;
- migrate old hook state labels from `conductor-attest` to
  `conductor-reporter`;
- run stale-local-state cleanup under the retention policy; and
- run the same post-upgrade migration status repair used by `loopcoder upgrade`.

`doctor --fix` must not:

- install or upgrade provider CLIs;
- run `codex login`, `claude login`, `agy login`, or `gh auth login`;
- flush pending relay records;
- delete active run state;
- rewrite `.attest` ledgers;
- edit tracked docs, changelog, README, or specs;
- commit, push, create PRs, or mutate GitHub; or
- choose Worker/Verifier models for the user.

Fix mode must print a before/after summary for each mutation and stay
idempotent. A partial failure must report the completed steps and the remaining
fix command; it must not hide earlier successful mutations.

## Upgrade Command Behavior

`loopcoder upgrade` keeps the release and integrity model from
[`0212-release-distribution-and-upgrade.md`](0212-release-distribution-and-upgrade.md):
download from GitHub Releases, verify signed checksums, stage, swap atomically,
handle Windows running-executable replacement, and refresh bundled skills.

Unit C adds migration reporting:

- before selecting the target, report current path/version and requested target;
- after selecting 0.6.0, report whether 0.5.x compatibility aliases are active;
- when a repo is available, run the same diagnose-only migration scan as doctor;
- if no repo is available, state that repo migration is deferred to
  `loopcoder doctor --repo <repo> --fix`;
- never rewrite repo config or prune local state during ordinary binary
  download/selection unless the user passed an explicit future upgrade fix flag
  defined by an implementation slice; and
- always recommend `loopcoder doctor --repo .` after a 0.5.x to 0.6.0 upgrade.

If implementation chooses to add `loopcoder upgrade --fix` or
`loopcoder upgrade --migrate`, that flag must delegate to the same migration
core as `doctor --fix`. The default `loopcoder upgrade` remains primarily a
binary/skill upgrade command and must not surprise-delete logs.

## Release-Docs Rule

0.6.0 must ship complete release documentation. A later release-docs slice must
rewrite all three surfaces:

- `CHANGELOG.md`: add the full 0.6.0 entry, including Unit A model/depth,
  Unit B reporter rename, Unit C upgrade/migration/doctor, breaking-change
  compatibility window, and upgrade instructions.
- GitHub Release Note source/body: provide detailed feature notes,
  breaking-change guidance, upgrade steps, doctor usage, and known transition
  aliases.
- `README.md`: update the operator-facing feature and how-to-use sections for
  model selection, reporter terminology, upgrade, migration, and doctor.

The release-docs slice must also create or update
`docs/reference/releasing.md` with a standing rule:

> Every version bump must rewrite the changelog entry, GitHub Release Note, and
> README release-facing sections completely and in detail. A version bump is
> not release-ready until all three are current, internally consistent, and
> describe how to use the shipped behavior.

This spec does not create `docs/reference/releasing.md` because this issue is
the design slice only and must produce this spec file only.

## Implementation Slices

After this spec merges, implementation should be split into dependency-ordered
issues. Each issue must reference this accepted spec:

1. **Config/env-key migration core.** Add the shared migration map and
   diagnostics for legacy reporter keys; wire config reads to accept old names;
   wire config writes to emit new names; preserve Unit B dual-token/dual-env
   aliases.
2. **`loopcoder upgrade` migration status.** Add 0.5.x to 0.6.0 detection,
   post-upgrade migration reporting, skill refresh verification, old-surface
   diagnostics, and the recommendation to run doctor. Delegate any explicit
   fix mode to the shared migration core.
3. **Stale-log cleanup and retention.** Implement the retention policy for
   `.loopcoder/runs`, old worktree-liveness artifacts, relay ledgers, and audit
   logs. Prove active runs and pending relay records are retained.
4. **`loopcoder doctor` diagnose.** Expand doctor into the read-only readiness
   report: git/gh, provider CLI presence, codex/claude/agy auth readiness,
   config validity, model/depth validation, reporter/relay wiring, version and
   upgrade status, installed skill freshness, and stale-state counts.
5. **`doctor --fix`.** Add explicit mutating mode for config-key migration,
   hook command migration, hook state migration, stale-log cleanup, and
   post-upgrade repair. Keep provider login, relay flush, GitHub mutations, and
   model selection out of scope.
6. **Release docs and releasing rule.** Rewrite the 0.6.0 `CHANGELOG.md`,
   GitHub Release Note, and `README.md` sections; create or update
   `docs/reference/releasing.md` with the standing all-three-docs rule.

Code slices must not combine unrelated release-doc rewrites with migration or
doctor logic unless the issue explicitly says it is the release-docs slice.

## Acceptance Criteria For Implementation

- 0.6.0 accepts old `[attestation]` tokens and new `[reporter]` tokens during
  the transition window.
- 0.6.0 emits only `[reporter]` for new reporter output.
- 0.6.0 accepts old env vars
  `LOOPCODER_CONDUCTOR_ATTEST_SCOPE` and
  `LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR`, preferring the new reporter env vars
  when both are set.
- `loopcoder hook conductor-attest` remains an alias for one release.
- `loopcoder skill install --repo <repo>` and `doctor --fix` write
  `loopcoder hook conductor-reporter`.
- `.delivery.yml` legacy `attestation` keys read successfully and are written
  back as `report` keys only when an explicit write/fix path runs.
- Existing `.attest` relay ledgers remain readable and keep their extension.
- Existing canonical JSON field names remain unchanged.
- New run/result/state writes use `report`, while readers accept legacy
  `attestation`.
- `loopcoder doctor --repo .` performs no file mutations and reports every
  problem with an actionable fix command when available.
- `loopcoder doctor --repo . --fix` performs only the mutations listed in this
  spec and is idempotent.
- Stale cleanup retains active runs, newest retained runs, pending relay
  obligations, and all `.attest` ledgers inside retention.
- Cleanup never follows symlinks out of `.loopcoder/`.
- Doctor reports model/depth validity using the Unit A registry and respects
  `models.strict`.
- Doctor checks provider readiness for configured Worker and Verifier
  providers, including Antigravity OAuth readiness via `agy models`.
- Doctor is not a role and does not emit or satisfy Worker/Verifier/Conductor
  reporter records.
- 0.6.0 release docs update `CHANGELOG.md`, GitHub Release Note, `README.md`,
  and `docs/reference/releasing.md` in the release-docs slice, not in this
  design PR.

## Relationship To Existing Specs

- [`0212-release-distribution-and-upgrade.md`](0212-release-distribution-and-upgrade.md)
  defines the release, install, self-upgrade, integrity, and baseline doctor
  model. Unit C adds the first breaking-release migration behavior.
- [`0291-skill-propagation-on-upgrade.md`](0291-skill-propagation-on-upgrade.md)
  defines stale-aware managed skill refresh. Unit C relies on that behavior
  when 0.6.0 installs updated playbooks and hooks.
- [`0554-model-depth-selection.md`](0554-model-depth-selection.md) defines the
  Unit A model registry and doctor model/depth readiness checks that Unit C
  includes in the overall doctor contract.
- [`0567-reporter.md`](0567-reporter.md) defines the Unit B reporter rename,
  frozen surfaces, dual-token window, and compatibility aliases. Unit C defines
  how upgrade and doctor handle those surfaces operationally.
- [`0306-local-only-attestation.md`](0306-local-only-attestation.md) remains the
  local-only report invariant. Unit C cleanup must not copy report blocks into
  tracked files or GitHub artifacts.
- [`0316-conductor-local-enforcement.md`](0316-conductor-local-enforcement.md)
  remains the conductor hook foundation. Unit C checks and fixes hook wiring.
- [`0447-relay-enforcement-hardgate.md`](0447-relay-enforcement-hardgate.md)
  remains the relay hard-gate contract. Unit C must not delete pending relay
  obligations or weaken the fail-loud/no-lockout design.
- [`0041-resilience.md`](0041-resilience.md) defines run state, recovery, and
  resume semantics. Unit C cleanup treats that state as advisory but preserves
  active and recent recovery evidence.
