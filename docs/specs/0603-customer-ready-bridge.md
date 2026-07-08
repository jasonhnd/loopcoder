---
id: 603
title: 0.6.1 Customer-Ready Bridge Release
status: draft
date: 2026-07-08
issue: 603
pr: null
supersedes: []
superseded_by: []
---

# 0.6.1 Customer-Ready Bridge Release

This is a design-only spec for loopcoder 0.6.1. This PR adds only this
document: no Go code, no `.delivery.yml` change, no command behavior change, no
`CHANGELOG.md` rewrite, no `README.md` rewrite, and no reference-documentation
rewrite. Code and release-documentation work must be filed separately after
this spec merges, per [`docs/PROCESS.md`](../PROCESS.md).

0.6.1 is the customer-ready bridge release for the 0.6 line. It is not the
0.7.0 architecture rewrite. It packages the 0.6-line capabilities that already
exist on `main` into a version an external customer can safely install,
understand, diagnose, and upgrade.

The release framing is:

```text
loopcoder v0.6.1 is the customer-ready bridge release for the 0.6 line: it
makes reporter/doctor/upgrade/local-state behavior safe and diagnosable for
real consumer repositories, while preserving existing orchestration semantics
and keeping larger project-database and sub-agent orchestration work for
v0.7.0.
```

## Goals

- Make a new customer's first install, init, doctor, dispatch, loopreview, and
  report run hit no obvious traps.
- Prevent repo-local `.loopcoder/` machine state from being accidentally
  committed into a customer business repository.
- Make `doctor` and `report` dependable diagnostic interfaces for host tools
  and customer support.
- Align README, reference docs, release notes, stability policy, roadmap, and
  changelog with the actual 0.6.1 public release state.
- Preserve the 0.5.x to 0.6.x compatibility transition. 0.6.1 must not remove
  legacy reporter inputs, the old Conductor report command alias, old hook
  alias, or legacy local-state readers.
- Keep 0.6.1 focused as a bridge release, not a 0.7.0 platform rewrite.

## Non-Goals

- No code implementation in this design PR.
- No rewrite of `CHANGELOG.md`, `README.md`, `ROADMAP.md`, or
  `docs/reference/*` in this design PR.
- No SQLite database.
- No global project registry.
- No project alias, repo-name mapping, or cross-project dashboard.
- No native sub-agent scheduling or worker/provider rewrite.
- No `plan`, `continue`, or `decide` workflow.
- No removal of the 0.5.x compatibility aliases or legacy local-state readers.
- No change to the runtime behavior that treats an empty production gate as
  `auto`.
- No movement of repo-local `.loopcoder/` state to a machine-global database.
- No default mutation of tracked `.gitignore` in customer repositories.
- No automatic `git rm` of already-tracked `.loopcoder/` files.

## Release Positioning

The public release/tag line is behind the 0.6 implementation work. At the time
this spec was written, the latest public customer release is still `v0.5.4`;
there is no public `v0.6.0` or `v0.6.1` release. The release documentation
slice must therefore not send customers to a nonexistent public 0.6.0 release.

0.6.1 is the public bridge for the 0.6 line. Release notes must say that 0.6.1
contains the customer-ready packaging of the 0.6 capabilities: model/depth
selection, Antigravity worker support, reporter terminology, upgrade/migration
compatibility, skill install, doctor, local run status, report querying, relay
gates, loopreview, audit, state branch, lease, and process watchdog behavior.

The 0.6.1 release may mention that 0.6.0 design and code slices landed before
it, but customer installation and upgrade instructions must target `v0.6.1`.

## Current Customer-Blocking Gaps

The follow-on implementation issues must treat these as the starting facts:

- Public release status and docs disagree. Current tracked prose refers to
  older releases and to 0.6.0 as current, while the public customer tag is
  still `v0.5.4`.
- `docs/reference/stability-policy.md` still names `0.3.7` as the current
  stable release.
- README, usage docs, release docs, stability policy, roadmap, and changelog do
  not consistently describe one customer-ready version.
- The current command inventory in `internal/cli.Commands()` includes commands
  that are missing or under-documented in user-facing command lists,
  especially `report`, `state`, `lease`, `ps`, and `kill`.
- `.loopcoder/` is used as repo-local run state. loopcoder's own repository
  ignores it, but customer repositories are not automatically protected.
- `loopcoder skill install --repo <repo>` writes a project
  `.loopcoder/conductor-workspace` marker but does not currently make the
  customer's `.git/info/exclude` protect `.loopcoder/`.
- `loopcoder init` currently targets the current directory only and scaffolds
  `adapters.gate: auto`.
- Runtime gate-less configs still normalize to `auto`. 0.6.1 must not silently
  migrate or reinterpret existing gate-less configs.
- `doctor` currently renders text only and lacks a stable JSON diagnostic
  surface for host tools.
- `doctor` does not currently diagnose the local-state mis-commit risk:
  missing `.git/info/exclude` protection or tracked `.loopcoder/` files.
- `loopcoder report` exists but needs to be documented alongside other
  commands. Its JSON currently preserves top-level `reports` only, which drops
  source/run/path context that support tools need.

## Version And Documentation Truth

The release-documentation slices must make one release truth visible everywhere:

- The customer install target is `v0.6.1`.
- 0.6.1 is the public customer-ready bridge for the 0.6 line.
- If there is no public `v0.6.0` tag/release, docs must not tell customers to
  install or upgrade to `0.6.0`.
- Non-historical docs must not call `v0.3.7`, `v0.4.2`, or `v0.5.4` the current
  stable release after 0.6.1 ships.
- Historical changelog entries and accepted historical specs keep their
  historical wording.

The command documentation must be checked against `internal/cli.Commands()` in
the implementation slice. The public command list must include every
operator-facing command and must either document or explicitly mark internal
commands. The inventory at spec time is:

| Command | Documentation rule |
|---|---|
| `attest` | Document as a compatibility alias for direct Conductor self-reports, not as the preferred new command. |
| `version` | Document as the install verification command; `--version` remains accepted at the root. |
| `models` | Document model/depth discovery. |
| `audit` | Document read-only security audit usage. |
| `doctor` | Document read-only diagnostics, JSON output, and legacy migration notes without making fix mode the first-run path. |
| `init` | Document `--repo` and `--gate`. |
| `discover` | Document or mark advanced. |
| `compile` | Document or mark conductor/advanced. |
| `tick` | Document delivery-loop pass. |
| `trigger` | Document automation trigger semantics or mark advanced. |
| `promote` | Document production side effects and gate. |
| `upgrade` | Document v0.5.4 to v0.6.1 upgrade and skill refresh. |
| `skill` | Document global skill install plus project hook install. |
| `dispatch` | Document one-issue worker dispatch. |
| `relay` | Document `list` and `flush` local relay state. |
| `report` | Document text and JSON report query surfaces. |
| `ready-set` | Document ready/blocked classification. |
| `status` | Document read-only local run status. |
| `resume` | Document reconciliation after interruption. |
| `state` | Document explicit state-branch publishing/pulling. |
| `lease` | Document conductor lease acquire/release. |
| `recover` | Document bounded recovery/retry. |
| `loopreview` | Document independent read-only verifier. |
| `verify-local` | Document local configured gates. |
| `dispatch-wave` | Document ready-wave dispatch. |
| `hook` | Mark as host-hook integration/internal, not a normal customer workflow command. |
| `ps` | Document loopcoder-managed process listing. |
| `kill` | Document guarded process termination. |

The docs-consistency slice must include a command-inventory check or test so
future command additions do not silently fall out of README and usage docs.

## Local State Protection

`.loopcoder/` remains repo-local state in 0.6.1. This is intentional: existing
run state, relay ledgers, status, recovery, report query, and state-branch
publishing all depend on that location. 0.6.1 does not move this state into
SQLite or a machine-global project registry.

The customer-ready bridge must protect this repo-local state from accidental
commits in consumer repositories:

- Add a small internal package, for example `internal/gitlocal` or
  `internal/localstate`, that can protect `.loopcoder/` for a git repository.
- The package must locate the repository's `.git/info/exclude` using git-aware
  path resolution rather than assuming `.git` is always a directory. Worktrees
  and `.git` files must be handled.
- The package must idempotently append a loopcoder-managed comment and
  `.loopcoder/` entry when missing.
- The package must never modify tracked `.gitignore` by default.
- The package must not remove or untrack files.
- `loopcoder init` must call this protection step after successful scaffold
  writes.
- `loopcoder skill install --repo <repo>` must call this protection step when
  it writes the project conductor marker or hook settings.

The managed exclude block should be stable and minimal, for example:

```text
# loopcoder local state
.loopcoder/
```

Exact wording may vary, but repeated runs must not duplicate the managed block
or `.loopcoder/` entry.

## Local State Doctor Checks

`loopcoder doctor` must add read-only local-state protection checks:

- whether `.git/info/exclude` protects `.loopcoder/`;
- whether `git ls-files .loopcoder` reports already-tracked local state; and
- whether the repository shape prevents safe exclude detection.

Doctor must distinguish at least:

| State | Required diagnostic |
|---|---|
| Excluded and untracked | `ok`; local state is protected. |
| Not excluded and untracked | `warn`; show the fix command. |
| Already tracked | `fail` when it risks committing local state; show the untrack command. |
| Not a git repository or unreadable git metadata | `warn` or `fail` according to whether dispatch would be blocked. |

If `.loopcoder/` files are already tracked, doctor must not run `git rm`.
Instead it must print this exact fix command:

```text
git rm -r --cached .loopcoder && echo .loopcoder/ >> .git/info/exclude
```

The state-branch workflow is unaffected. `loopcoder state push` still
explicitly publishes run summaries to the dedicated state branch. Local exclude
protection only prevents the working-tree `.loopcoder/` directory from being
committed to the business repository's normal branches.

## Init First-Run Safety

`loopcoder init` must become safe and explicit for first customer repos:

- Add `--repo <path>`, defaulting to the current directory.
- Add `--gate human-merge|auto`.
- The generated `.delivery.yml` template must default to
  `adapters.gate: human-merge`.
- A customer must pass `--gate auto` to generate `adapters.gate: auto`.
- Existing model/depth flags remain supported.
- Existing `--force` behavior remains supported.
- On success, `init` must protect `.loopcoder/` in `.git/info/exclude`.

This slice must not change runtime gate normalization. Existing gate-less
configs still follow current runtime behavior where an empty gate normalizes to
`auto`. 0.6.1 changes only the newly generated scaffold default, so a first
customer repository starts with a human production merge gate unless the user
explicitly asks for `auto`.

## Doctor JSON And Actionable Diagnostics

`loopcoder doctor` must add:

```text
loopcoder doctor --repo . --format text
loopcoder doctor --repo . --format json
```

The default remains `text`, and current readable text style must stay
compatible. JSON output must be stable enough for host tools and support
automation:

```json
{
  "repo_path": "/absolute/repo",
  "version": "0.6.1",
  "commit": "abc123",
  "date": "2026-07-08T00:00:00Z",
  "exit_code": 0,
  "checks": [
    {
      "name": "local-state exclude",
      "status": "ok",
      "hard": false,
      "message": ".loopcoder/ is protected by .git/info/exclude",
      "fix_command": ""
    }
  ]
}
```

Required JSON fields:

- `repo_path`: resolved repository path used by doctor;
- `version`: selected binary version;
- `commit`: selected binary commit when known;
- `date`: selected binary build date when known;
- `exit_code`: the same code the command returns;
- `checks[]`: ordered rendered checks;
- `checks[].name`;
- `checks[].status`: `info`, `ok`, `warn`, or `fail`;
- `checks[].hard`: whether the check contributes to a non-zero exit code;
- `checks[].message`; and
- `checks[].fix_command`: empty when no safe exact command exists.

Doctor remains a diagnostic interface for this release work. The JSON slice
must not add new mutating doctor behavior. If Unit C's legacy `doctor --fix`
mode is present, 0.6.1 may keep it for the 0.5.x to 0.6.x migration path, but
the new JSON/local-state checks must be read-only and the customer first-run
flow must use diagnose-only doctor commands.

Doctor must include these additional or tightened checks:

- local-state exclude protection;
- tracked `.loopcoder/` files;
- whether `reportquery` can read local report/run/relay records;
- whether the installed managed skill matches the embedded skill;
- whether project conductor hook settings exist;
- whether configured provider CLIs are on `PATH`; and
- provider auth only where the code has a stable cheap probe.

Doctor must not overstate Codex or Claude authentication when no stable cheap
probe exists. It should say that authentication could not be conclusively
checked rather than claiming success.

Hard failures must stay limited to truly blocking problems, such as missing
`git`, invalid `.delivery.yml`, strict model/depth rejection, configured
provider unavailability that prevents the selected role from running, or
tracked `.loopcoder/` state that would leak local machine records into normal
repo history.

## Report Command Contract

`loopcoder report` is the read-only local query surface for reporter records.
0.6.1 must make it useful for both humans and support tooling.

`internal/reportquery.MarshalJSON` must keep the existing top-level `reports`
array for compatibility and add a richer top-level `records` array:

```json
{
  "reports": [
    {
      "role": "worker",
      "provider": "codex",
      "model": "gpt-5.5"
    }
  ],
  "records": [
    {
      "report": {
        "role": "worker",
        "provider": "codex",
        "model": "gpt-5.5"
      },
      "source": "attempt",
      "run_id": "run-20260708T000000Z-issue-603",
      "path": ".loopcoder/runs/run-20260708T000000Z-issue-603/workers/worker.attempt.json"
    }
  ]
}
```

Rules:

- `reports` remains a plain array of report objects for old callers.
- `records[].report` contains the same report object.
- `records[].source` identifies where the report was found, such as
  `attempt`, `run-json`, `run-jsonl`, `relay-ledger`, or `relay-pending`.
- `records[].run_id` is populated when known.
- `records[].path` is populated with the local file path when known.
- The text renderer must add enough source/run/path context for an operator to
  locate local state.
- The query remains read-only and must not flush relay records, mutate run
  state, touch git, call GitHub, or invoke provider CLIs.
- The query must continue to accept legacy reporter-transition inputs:
  old headers and old JSON keys.

Reference docs must add `loopcoder report`, its flags, text usage, and JSON
examples. Docs must also correct the pretty-output description to match current
code: provider display is rendered as a combined line such as
`OpenAI Codex / codex`, not as separate vendor and tool lines.

## Reporter Transition Compatibility

0.6.1 must preserve the 0.5.x to 0.6.x transition window. It must not remove:

- `loopcoder attest` as the direct Conductor self-report compatibility alias;
- old report header token matchers;
- old Conductor hook command aliases;
- old reporter environment variable aliases;
- old `.delivery.yml` reporter config aliases;
- old local-state JSON input readers; or
- old relay ledger readability.

New output and new docs for new users must use reporter terminology. Legacy
names are compatible inputs, not recommended outputs.

Rename-discipline rule: new production code or docs that must mention a legacy
name must route that mention through the explicit migration constants in
`internal/migration` or through release-transition documentation. This keeps
the Unit B inventory sweep, including `TestReporterRenameInventorySweep`,
meaningful and green. Do not add ad hoc legacy-name strings in unrelated code,
fixtures, or manuals.

## Install, Upgrade, And Skill Refresh

0.6.1 installation docs must pin the customer release:

```text
loopcoder upgrade --version 0.6.1
```

Install docs must state that the no-Go install scripts verify signed release
checksums: `install.sh` and `install.ps1` verify `SHA256SUMS` signatures using
cosign before trusting the checksums. If cosign is required by the script path,
the docs must name it as an install prerequisite or describe the script's
bootstrap behavior accurately.

Upgrade docs must state the two-layer refresh model:

1. `loopcoder upgrade --version 0.6.1` selects the machine-level binary and
   refreshes the global bundled skill.
2. Each project must then run `loopcoder skill install --repo <repo>` to write
   or refresh project hook settings, the conductor workspace marker, and the
   local `.loopcoder/` exclude.
3. Each project should run `loopcoder doctor --repo <repo>` to confirm
   readiness.

The v0.5.4 to v0.6.1 upgrade acceptance path is:

```text
loopcoder upgrade --version 0.6.1
loopcoder version
loopcoder skill install --repo <repo>
loopcoder doctor --repo <repo>
```

After a v0.6.1 binary is selected, `loopcoder upgrade --version 0.6.1` must
recognize that it is already latest and report that clearly.

## Customer Quickstart

The customer quickstart must be a directly operable flow:

1. Install `v0.6.1`.
2. Run `loopcoder version`.
3. Run `loopcoder init --repo .`.
4. Run `loopcoder skill install --repo .`.
5. Run `loopcoder doctor --repo .`.
6. Run `loopcoder report --repo .`.
7. Then run dispatch/tick/loopreview through the conductor workflow.

README and `docs/reference/usage.md` must not contradict each other on this
order.

Docs must include a side-effect table:

| Command | Side effects |
|---|---|
| `loopcoder init --repo .` | Writes `.delivery.yml`, `ROADMAP.md`, GitHub labels, and local `.git/info/exclude` protection for `.loopcoder/`. |
| `loopcoder skill install --repo .` | Writes or refreshes the global skill files, project hook settings, `.loopcoder/conductor-workspace`, and local `.git/info/exclude` protection. |
| `loopcoder doctor --repo .` | Read-only diagnostics in the first-run path. |
| `loopcoder report --repo .` | Read-only local report query. |
| `loopcoder status --repo .` | Read-only local run status. |
| `loopcoder state push --repo .` | Explicitly writes run summaries to the dedicated state branch. |
| `loopcoder promote --repo .` | May change the configured production branch, subject to the gate and human command. |

Docs must also state:

- `.loopcoder/` is repo-local machine state and is not for normal business
  branches.
- A machine can serve many projects.
- Each project has its own `.delivery.yml` and repo-local `.loopcoder/`.
- Machine-level binary and skill installs live under the user's machine-level
  loopcoder/agent directories.
- 0.6.1 introduces no SQLite database or global project registry.

## Release Verification Gate

The 0.6.1 release cannot be called customer-ready until both source checks and
published-artifact checks pass.

Before tagging:

- `go test ./...`;
- `go vet ./...`;
- `go test -race ./internal/...` for core internal packages, with explicit
  documented exceptions if the full race run is too slow or platform-blocked;
- command-inventory documentation check;
- doctor JSON parse test;
- local-state exclude idempotence tests;
- report JSON compatibility tests; and
- release workflow dry-run or equivalent artifact-build proof.

The release workflow must build all configured platform artifacts.

After publishing:

- Download at least one real `v0.6.1` release artifact from GitHub Releases.
- Verify checksum/signature using the same mechanism the install scripts use.
- Run `loopcoder version` from the downloaded artifact and confirm it reports
  `v0.6.1` with non-placeholder commit/date metadata.
- In a temporary git repository, run:

  ```text
  loopcoder init --repo .
  loopcoder skill install --repo .
  loopcoder doctor --repo . --format json
  loopcoder report --repo .
  loopcoder upgrade --version 0.6.1
  ```

- Confirm `doctor --format json` parses with `jq` or an equivalent JSON parser.
- Confirm the already-latest upgrade path reports success without a download.

Self-hosting may use loopcoder to develop 0.6.1, but this repository remains
`gate: human-merge`. Tag pushes, GitHub release publication, production branch
updates, and other remote release writes require explicit human confirmation.
Release notes must not advertise 0.7.0 capabilities such as SQLite, global
project registry, or native sub-agent orchestration.

## Implementation Slices

After this spec merges, implementation should be split into dependency-ordered
issues. Each issue must reference this accepted spec.

1. **Local state protection code.** Add the git-local protection package,
   resolve `.git/info/exclude` safely for normal repos and worktrees, append
   the managed `.loopcoder/` exclude idempotently, and wire it into
   `loopcoder skill install --repo <repo>`. Do not touch tracked `.gitignore`
   and do not untrack existing files.
2. **Init first-run safety code.** Add `loopcoder init --repo <path>` and
   `--gate human-merge|auto`, default the generated scaffold to
   `human-merge`, wire local-state exclude protection on successful init, and
   preserve runtime empty-gate-to-`auto` semantics for existing configs.
3. **Doctor checks and JSON code.** Add `doctor --format text|json`, stable
   JSON fields, local-state exclude and tracked-state checks, reportquery
   readability check, installed-skill and hook checks, provider CLI checks, and
   precise `fix_command` fields. Keep the new diagnostics read-only.
4. **Report records code.** Extend `internal/reportquery.MarshalJSON` to keep
   top-level `reports` while adding top-level `records` with `report`,
   `source`, `run_id`, and `path`; add source/run/path context to text output;
   preserve legacy reporter-transition input readers.
5. **Docs consistency and release truth docs.** Update README badge/status,
   `docs/reference/usage.md`, `docs/reference/stability-policy.md`,
   `CHANGELOG.md`, `ROADMAP.md`, and release-note source for 0.6.1. Align
   current stable/public release language, document every command from
   `internal/cli.Commands()`, pin install/upgrade docs to `v0.6.1`, and avoid
   directing customers to a nonexistent public 0.6.0 release. This is a docs
   slice, not a code slice.
6. **Customer quickstart docs.** Rewrite the quickstart into the operable
   install/version/init/skill-install/doctor/report flow, add the side-effect
   table, document `.loopcoder/` locality and state-branch boundaries, and
   state clearly that 0.6.1 has no SQLite/global registry. This may be combined
   with slice 5 only if the follow-on issue explicitly scopes both docs slices
   together.
7. **Release gate and test slice.** Add or update unit tests and release
   verification scripts for local-state protection idempotence, init gate
   defaults, doctor JSON parsing, report JSON compatibility, command-doc
   inventory coverage, legacy reporter input compatibility, and release
   artifact smoke instructions.
8. **Release slice.** Cut and publish `v0.6.1` only after the preceding slices
   land. Build all platform artifacts, publish the full release note, perform
   the real download/checksum/signature/version/doctor/report/upgrade smoke,
   and record any exceptions in the release notes.

The implementation issues must not bundle unrelated release docs with behavior
code unless their issue explicitly says so. The release slice must not advertise
0.7.0 architecture work.

## Acceptance Criteria For Implementation

- Public install and upgrade documentation pins `v0.6.1`.
- Non-historical README, usage, stability-policy, roadmap, and release docs
  agree that 0.6.1 is the current customer-ready bridge release after it ships.
- No non-historical changelog/spec/reference file calls `v0.3.7`, `v0.4.2`, or
  `v0.5.4` the current stable release after the docs slice lands.
- Docs do not direct customers to install or upgrade to a nonexistent public
  `v0.6.0` release.
- README and usage command lists cover every command from
  `internal/cli.Commands()`, with internal commands labeled as such.
- `loopcoder init --repo /tmp/repo` works.
- `loopcoder init` defaults generated `.delivery.yml` to
  `adapters.gate: human-merge`.
- `loopcoder init --gate auto` generates `adapters.gate: auto`.
- Existing gate-less configs still follow current runtime empty-gate
  normalization; no silent migration changes them.
- `loopcoder init` and `loopcoder skill install --repo <repo>` idempotently
  protect `.loopcoder/` in `.git/info/exclude`.
- Repeated init/skill install runs do not duplicate the managed exclude block.
- Tracked `.gitignore` is never modified by default.
- No command automatically runs `git rm` for tracked `.loopcoder/` files.
- Doctor distinguishes excluded, not excluded, already tracked, and
  unreadable/non-git local-state cases.
- Doctor prints the exact tracked-state fix command:
  `git rm -r --cached .loopcoder && echo .loopcoder/ >> .git/info/exclude`.
- `loopcoder doctor --format text` preserves readable text output.
- `loopcoder doctor --format json` emits parseable JSON with `repo_path`,
  `version`, `commit`, `date`, `exit_code`, and `checks[]`.
- Every JSON doctor check has `name`, `status`, `hard`, `message`, and
  `fix_command`.
- Doctor hard-fails only for truly blocking problems.
- Doctor reports provider authentication only where a stable cheap probe exists
  and otherwise says the auth state was not conclusively checked.
- Doctor's new JSON/local-state/reportquery checks are read-only.
- `loopcoder report --format json` still emits top-level `reports`.
- `loopcoder report --format json` also emits top-level `records`.
- Each `records[]` entry includes `report`, `source`, `run_id`, and `path` when
  that context is known.
- Report text output includes source/run/path context sufficient to locate the
  local state record.
- Report query remains read-only and does not flush relay records or mutate
  state.
- Report query still accepts legacy reporter-transition inputs.
- 0.6.1 does not remove the direct Conductor self-report compatibility alias,
  legacy report token matching, old hook alias, old environment aliases, old
  config aliases, or old local-state readers.
- New docs describe legacy names as compatible input only, not recommended new
  output.
- New production legacy-name references use migration constants or are confined
  to explicit release-transition documentation so the reporter inventory sweep
  stays green.
- Install docs accurately describe cosign/checksum verification.
- Upgrade docs state that `loopcoder upgrade --version 0.6.1` refreshes the
  global skill and each project must still run
  `loopcoder skill install --repo <repo>` plus
  `loopcoder doctor --repo <repo>`.
- README and usage quickstarts use the same install/version/init/skill-install/
  doctor/report order.
- Quickstart side-effect table documents init, skill install, doctor, report,
  status, state push, and promote.
- Docs clearly state `.loopcoder/` is local repo state, while `state push` is
  the explicit state-branch publishing boundary.
- Docs clearly state that each project owns its own `.delivery.yml` and
  `.loopcoder/`, and 0.6.1 adds no SQLite/global project registry.
- Pre-release checks run `go test ./...`, `go vet ./...`, and a race-test pass
  for core internal packages or document bounded exceptions.
- The release workflow builds all configured platform artifacts.
- A downloaded real `v0.6.1` artifact verifies checksum/signature, reports
  `v0.6.1` with non-placeholder metadata, runs the temp-repo init/skill
  install/doctor/report smoke, and recognizes
  `loopcoder upgrade --version 0.6.1` as already latest.

## Relationship To Existing Specs

- [`0212-release-distribution-and-upgrade.md`](0212-release-distribution-and-upgrade.md)
  defines release distribution, install, upgrade, integrity checks, and the
  original doctor model. 0.6.1 tightens the customer-facing release and
  post-release smoke obligations.
- [`0291-skill-propagation-on-upgrade.md`](0291-skill-propagation-on-upgrade.md)
  defines managed skill refresh. 0.6.1 documents that binary upgrade refreshes
  the global skill, while each project still needs `skill install --repo`.
- [`0403-e2-auto-promote-production.md`](0403-e2-auto-promote-production.md)
  defines default production auto-promotion behavior. 0.6.1 does not change
  runtime empty-gate normalization, but changes the new scaffold default to
  `human-merge`.
- [`0447-relay-enforcement-hardgate.md`](0447-relay-enforcement-hardgate.md)
  remains the local relay hard-gate contract. 0.6.1 preserves relay/report
  compatibility while improving local-state protection.
- [`0518-loopcoder-audit.md`](0518-loopcoder-audit.md) defines the audit command
  and audit readiness expectations that doctor must continue to report.
- [`0533-audit-consumer-repo-usability.md`](0533-audit-consumer-repo-usability.md)
  already treats `.loopcoder/**` as excluded for audit scanning. 0.6.1 extends
  customer safety by protecting `.loopcoder/` from ordinary git commits.
- [`0554-model-depth-selection.md`](0554-model-depth-selection.md) defines the
  0.6 model/depth registry and Antigravity provider setup that 0.6.1 packages
  for customers.
- [`0567-reporter.md`](0567-reporter.md) defines the reporter rename,
  transition aliases, and `loopcoder report`. 0.6.1 preserves the transition
  and strengthens the report query contract for support tooling.
- [`0583-upgrade-migration-doctor.md`](0583-upgrade-migration-doctor.md)
  defines 0.6 upgrade, migration, doctor, and stale-state cleanup. 0.6.1 keeps
  that compatibility window, adds customer local-state protection, and adds a
  machine-readable doctor surface.
