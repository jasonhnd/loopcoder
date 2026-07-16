# loopcoder Usage

loopcoder provides commands and a conductor playbook for turning one delivery
need into GitHub issues, provider-backed worktree changes, pull requests,
`loopreview` verdicts, durable status, and user-directed merges.

v0.8.0 is a controlled-canary/development release. A command, schema, or
adapter declaration is not by itself a production-support promise. Before
using a provider or orchestration mode, read the binding
[`v0.8.0 capability and support matrix`](v0.8.0-capability-matrix.md), which
separates implemented code from reachable product paths, deterministic tests,
real-provider evidence, and production support.

## Quickstart (new project)

Use this flow to onboard an existing repository from zero to a driven
loopcoder loop. One installed binary can serve many local repositories. Each
repository keeps its own `.delivery.yml`; in v0.8.0, registered projects write
new runtime payloads under `$LOOPCODER_HOME`
instead of under the repository.

Per-project prerequisites: `git`, authenticated `gh`, at least one
authenticated provider CLI (`codex` and/or `claude`), and a GitHub remote with
push access.

1. Install the v0.8.0 binary once per supported macOS Apple Silicon machine,
   shared across all local projects.

   ```text
   curl -fsSL https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.sh | sh -s -- --version 0.8.0
   ```

   Windows, Linux, WSL, containers, and Intel macOS are not supported by the
   v0.8.0 installer. Users who need those hosts should remain on the historical
   v0.7.0 release.

   The installer puts `loopcoder` under `~/.loopcoder/bin`. Keep that directory
   on `PATH`, or set `LOOPCODER_BIN` to the full binary path.

2. Verify the selected binary.

   ```text
   loopcoder version
   ```

3. Initialize the consumer repository from the repository root.

   ```text
   cd <repo>
   loopcoder init --repo .
   ```

   `loopcoder init --repo .` scaffolds `.delivery.yml`, `ROADMAP.md`, GitHub
   labels, and local `.git/info/exclude` protection for `.loopcoder/`. New
   scaffolds default to `adapters.gate: human-merge`; pass `--gate auto` only
   when the project should opt into automatic production promotion.

4. Install the conductor playbook once per agent home and wire project
   conductor hooks into this repository.

   ```text
   loopcoder skill install --repo .
   ```

   This writes the bundled `SKILL.md` plus the Codex `AGENTS.md` entrypoint to
   the Claude Code loopcoder skill directory, merges loopcoder conductor hooks
   into `.claude/settings.json`, writes `.loopcoder/conductor-workspace`, and
   ensures local `.git/info/exclude` protects `.loopcoder/`.

5. Diagnose the repository without mutating it.

   ```text
   loopcoder doctor --repo .
   ```

6. Confirm local report querying works. A fresh repository may have no reports
   yet; the command is still read-only and should render a valid empty report
   list.

   ```text
   loopcoder report --repo .
   ```

7. Drive the loop from a conductor session in the repository.

   ```text
   /loopcoder <your need>
   ```

   For v0.8.0, keep `human-merge`, select Worker and Verifier providers
   explicitly, and treat the flow as a human-controlled canary. Do not enable
   automatic promotion until the repository's own gates and every required
   capability in the support matrix are satisfied.

Command side effects in the first-run path:

| Command | Side effects |
| --- | --- |
| `loopcoder init --repo .` | Writes `.delivery.yml`, `ROADMAP.md`, GitHub labels, and local `.git/info/exclude` protection for `.loopcoder/`. |
| `loopcoder skill install --repo .` | Writes or refreshes the global skill files, project hook settings, `.loopcoder/conductor-workspace`, and local `.git/info/exclude` protection. |
| `loopcoder doctor --repo .` | Read-only diagnostics in the first-run path; use `--format json` for the machine-readable form. |
| `loopcoder report --repo .` | Read-only local report query. |
| `loopcoder status --repo .` | Read-only local run status. |
| `loopcoder attach --repo . --run <run-id>` | Read-only durable detached run progress follow. |
| `loopcoder cancel --repo . --run <run-id>` | Requests cancellation for a LoopCoder-owned detached run. |
| `loopcoder state push --repo .` | Explicitly writes run summaries to the dedicated state branch. |
| `loopcoder promote --repo .` | May change the configured production branch, subject to `adapters.gate` and the human command that invokes promotion. |

Register each checkout after the read-only first doctor pass, then preview any
legacy repo-local import before applying it:

```text
loopcoder projects register --repo .
loopcoder projects show --repo .
loopcoder migrate local-state --repo . --dry-run
```

`loopcoder projects register --repo .` writes or refreshes this checkout's row
in the machine-local project registry after sanitizing Git remote credentials
from project metadata. `loopcoder projects remove --repo .` detaches this
checkout from active registry listings while preserving the project row,
identity links, run history, reports, legacy import records, and import status.
`loopcoder migrate local-state --repo .` explicitly copies legacy repo-local
`.loopcoder/` records into machine-local storage; it does not delete or rewrite
those files.

For registered v0.8.0 projects, new attempts, events, reports, relay records,
recovery briefs, lifecycle records, logs, and temporary worker scratch space
are machine-local under `$LOOPCODER_HOME/projects/<project_id>/`,
`$LOOPCODER_HOME/logs/`, and `$LOOPCODER_HOME/tmp/`. Repo-local `.loopcoder/`
remains an explicit unregistered fallback and a read-only compatibility source
for legacy records. `init` and `skill install --repo` still protect
`.loopcoder/` with local `.git/info/exclude` because unregistered and legacy
state can exist, but registered runtime payloads are outside the repository by
default.

## Prerequisites

- A conductor host session that follows `SKILL.md`, `AGENTS.md`, or `GEMINI.md`.
- `git` on `PATH`.
- `gh` on `PATH`, authenticated for the target GitHub repository.
- At least one registered provider CLI on `PATH`. `codex` is the default
  worker; `agy` is used by provider key `antigravity`; and `grok` discovers its
  model catalog dynamically. Registration does not establish authentication,
  usable capacity, role safety, or exact-release real-provider evidence.
  Codex and Claude have historical Worker/Verifier mechanism smokes,
  Antigravity has no read-only Verifier mode, and direct `gemini` remains
  experimental outside the static model registry.
- A GitHub repository with a configured remote.
- For the no-Go installer on native macOS Apple Silicon: `curl`, `tar`,
  `cosign`, and `sha256sum` or `shasum`. The installer verifies the
  Sigstore-signed `SHA256SUMS` before trusting checksums. This establishes
  archive integrity, not Apple Gatekeeper trust: the v0.8.0 Mach-O is not
  Developer ID signed or notarized. Go is optional for developer installs and
  local source builds.

## Install

The supported v0.8.0 consumer distribution is GitHub Releases on native macOS
Apple Silicon only. Tagged v0.8.0 releases publish
`loopcoder_<version>_darwin_arm64.tar.gz`, plus `SHA256SUMS` and signature
material. The installer rejects unsupported hosts before release lookup,
download, temporary directory creation, install directory creation, binary
replacement, or PATH/profile mutation. On the supported host, it verifies the
`SHA256SUMS` signature with cosign before trusting checksums, installs under
`~/.loopcoder/bin`, and updates or prints PATH instructions. The installer does
not require Go.

The default install directory is the supported documented path. v0.8.0 can
place the binary in an absolute `LOOPCODER_INSTALL_DIR`, but its PATH/profile
guidance still targets `~/.loopcoder/bin`; custom-directory setup is therefore
not supported as a complete installer flow.

```text
curl -fsSL https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.sh | sh -s -- --version 0.8.0
```

To choose a different supported v0.8.x release, replace `0.8.0` with the
desired version. v0.7.0 remains the final legacy multi-platform release for
Windows, Linux, WSL, containers, and Intel macOS.

After installation, confirm the selected binary:

```text
loopcoder version
```

`go install` remains available for users who already have Go:

```text
go install github.com/jasonhnd/loopcoder/cmd/loopcoder@v0.8.0
```

From a source checkout, you can also build a development binary locally:

```text
go build ./cmd/loopcoder
```

Make sure the installed or built binary is on `PATH`, or set `LOOPCODER_BIN` to
its full path. The conductor uses this binary as its only mechanical backend.
Release and installation rationale lives in
[`0212-release-distribution-and-upgrade.md`](../specs/0212-release-distribution-and-upgrade.md).

## Backend Selection

The conductor resolves the `loopcoder` binary before running mechanical work:

1. `LOOPCODER_BIN` when set.
2. `loopcoder` found on `PATH`.
3. Otherwise, `loopcoder` is required on the supported Darwin arm64 host.

Use the resolved binary for dispatch, ready-set scheduling, status reporting,
resume, recovery, local verification, state, and lease operations.

## Conductor Hooks And Status

Install or refresh the Claude Code playbook and project conductor hooks with:

```text
loopcoder skill install --repo <project>
```

The command writes the bundled `SKILL.md` and `AGENTS.md` files to the Claude
Code loopcoder skill directory and merges two hook commands into
`<project>/.claude/settings.json`: `loopcoder hook conductor-reporter` and
`loopcoder hook conductor-relay-guard`. The hooks are embedded in the loopcoder
binary and invoked as subcommands, so they resolve regardless of the working
directory and need no Node dependency; the merge upgrades any stale
`node hooks/*.js` entries idempotently. The merge is project-scoped and
preserves unrelated settings and hooks, and writes a gitignored
`.loopcoder/conductor-workspace` marker that activates auto-enforcement in the
installed repo. It also ensures local `.git/info/exclude` protects
`.loopcoder/` so repo-local machine state is not accidentally committed.
`loopcoder doctor --repo <project>` warns when either conductor hook command is
missing or when `loopcoder` does not resolve on `PATH`.

`loopcoder hook conductor-reporter` enforces the local Conductor self-report
step before a delivery or merge turn can finish. The old
`loopcoder hook conductor-attest` command remains a one-version compatibility
alias during the reporter transition.
`loopcoder hook conductor-relay-guard` enforces local visible relay of Worker
and Verifier reports from `loopcoder dispatch`, `loopcoder dispatch-wave`,
and `loopcoder loopreview`. Do not redirect, hide, or suppress foreground
`dispatch` or `loopreview` stderr, and keep foreground `dispatch-wave` stdout
visible because each Worker pretty block streams there as that Worker completes.
The relay guard covers Bash, PowerShell, and pwsh tool events and treats
backgrounded command output as pending until the block is surfaced.

The Go binary also hard-gates mechanical progress while pending local relay
blocks are unacknowledged. A gated command exits with reserved code `4`, prints
the pending block(s) plus recovery instructions to stdout, and refuses to run.
Use `loopcoder relay flush --repo <project>` in the foreground to print pending
blocks verbatim and clear them, or `loopcoder relay list --repo <project>` to
inspect pending records without clearing.

Report delivery run state with the program-rendered local status command:

```text
loopcoder status --repo .
loopcoder status --repo . --run <run-id>
```

When `--run` is omitted, `status` selects the latest modified local run. The
text output includes a readable local run status. Since v0.7.0,
`status --format json` also exposes the additive `run_tree` object for machine
consumers. Each node includes `project_id`,
`run_id`, `parent_run_id`, `child_run_ids`, issue/PR metadata when observed,
role, provider, model, effort, permission, claim owner/lease fields, lifecycle
status/source, timestamps, last error, and report summary when those fields are
present in local records.
When provider-native children are registered, JSON output also includes an
additive `agent_tree` object from machine-local SQLite. That tree, not opaque
provider session state, is the restart/replay authority for child agent IDs,
parent/run/task/attempt references, budget reservation IDs, ownership locks,
claim generation, registration state, and federation fingerprints.
The output is read-only and local-only: for registered projects it reads the
global project payload root first, then legacy repo-local `.loopcoder/` only as
a compatibility fallback. It must not be copied into PR bodies, issues,
comments, commits, merge artifacts, docs, examples, fixtures, or tracked files.

Candidate JSON examples:

```text
loopcoder status --repo . --format json
loopcoder status --repo . --run <run-id> --format json
```

Example JSON shape:

```json
{
  "run_id": "run-20260709T000000Z-wave",
  "project": {
    "project_id": "proj_abc123"
  },
  "run_tree": {
    "root_run_id": "run-20260709T000000Z-wave",
    "selected_run_id": "run-20260709T000000Z-wave",
    "nodes": [
      {
        "project_id": "proj_abc123",
        "run_id": "run-20260709T000000Z-wave",
        "child_run_ids": ["run-20260709T000001Z-child-worker"],
        "depth": 0,
        "lifecycle_status": "running"
      },
      {
        "project_id": "proj_abc123",
        "run_id": "run-20260709T000001Z-child-worker",
        "parent_run_id": "run-20260709T000000Z-wave",
        "child_run_ids": [],
        "depth": 1,
        "issue": 651,
        "pr": "https://github.com/owner/repo/pull/651",
        "role": "worker",
        "provider": "codex",
        "model": "gpt-5.5",
        "effort": "high",
        "permission": "write",
        "claim_outcome": "claimed",
        "claim_owner": "nested-scheduler:run-20260709T000000Z-wave:1234:1",
        "claim_generation": 1,
        "lease_expires_at": "2026-07-09T00:30:01Z",
        "lifecycle_status": "succeeded",
        "started_at": "2026-07-09T00:00:01Z",
        "updated_at": "2026-07-09T00:02:00Z",
        "ended_at": "2026-07-09T00:02:00Z",
        "report_summary": "implement issue #651"
      }
    ],
    "summary": {
      "run_count": 2,
      "terminal_runs": 1,
      "interrupted_runs": 1,
      "failed_runs": 0,
      "needs_human_runs": 0
    }
  }
}
```

## Repository Initialization

Run `loopcoder init --repo <path>` to scaffold the local loopcoder starting
point:

```text
loopcoder init --repo .
```

`init` creates `.delivery.yml` and `ROADMAP.md` when they are absent and
ensures the default GitHub labels used by loopcoder are present. It also
protects `.loopcoder/` in local `.git/info/exclude`; it does not modify tracked
`.gitignore` and does not untrack already tracked files. Existing
`.delivery.yml` and `ROADMAP.md` files are left untouched by default; use
`--force` to overwrite those two files with the current scaffold:

```text
loopcoder init --repo . --force
```

New scaffolds default to `adapters.gate: human-merge` so humans choose
production promotion. To opt into automatic production promotion in a new
scaffold, pass:

```text
loopcoder init --repo . --gate auto
```

Label setup is best-effort through `gh label list` and `gh label create`. If
`gh` is unavailable or label creation fails, `init` reports a warning on stderr
instead of treating file scaffolding as failed.

The model and reasoning-effort flags are first-run persistence helpers. Use
them only when you want the generated `.delivery.yml` to pin project defaults:

```text
loopcoder init --repo . \
  --worker-model gpt-5.5 \
  --worker-effort high \
  --verifier-model claude-haiku-4-5-20251001 \
  --verifier-effort medium
```

Omitting these flags leaves model and depth absent in `.delivery.yml`. At run
time, each role resolves the selected provider's static registry default model
and then that model's default depth; those resolved defaults are not written
back to config unless you explicitly persist a preference.

## Per-Repo Setup

Use `loopcoder init` or manually add a `.delivery.yml` file at the repository
root. If it is absent at runtime, loopcoder uses the current defaults: GitHub issues, git
worktrees, the Codex worker adapter, GitHub PRs/checks/merges, independent
`loopreview` verification, pre-prod-only auto-integration for clean `tick` PRs,
default `auto` production promotion, and chat reporting. v0.6.1 changes only
the generated scaffold default: `loopcoder init --repo .` writes `gate:
human-merge`, while `loopcoder init --repo . --gate auto` writes `gate: auto`.
Existing gate-less configs still normalize as before and are not silently
migrated.

The current example is:

```yaml
version: 1
adapters:
  work_items: github      # Work items are GitHub issues.
  workspace: git-worktree # Work happens in git worktrees.
  conductor: codex-cli    # Transparency only: the human session that conducts.
  worker: codex           # Default worker provider.
  vcs: github             # GitHub hosts PRs and checks.
  verifier: claude        # Should differ from worker; provider registry key.
  gate: human-merge       # First-run safe default; pass init --gate auto to opt into automatic production promotion.
worker:
  # Optional. Absent = static registry default model for the resolved worker provider.
  # model:
  # Optional. Absent = default depth for the resolved worker model.
  # reasoning_effort:
  base_branch: main
  command_hint: "implement the issue, run relevant checks, commit"
environment:
  pre_prod_branch: pre-prod # Tick auto-merges clean PRs here only; promote is the separate production step.
# evidence:
#   # Optional. Tick copies configured evidence onto dispatched, pending, and pre-prod report items.
#   website:
#     preview_url: https://preview.example.com
#   cli:
#     example_output: |
#       $ loopcoder --version
#       version=dev commit=unknown date=unknown
#   library:
#     test_results: go test ./...
#   app:
#     preview_build: dist/app-preview.zip
# verifier:
#   # Optional. Absent = static registry default model for the resolved verifier provider.
#   # model:
#   # Optional. Absent = default depth for the resolved verifier model.
#   # reasoning_effort:
# models:
#   # Optional. false warns and continues; true rejects invalid model/depth selections before launch.
#   # strict: true
ci:
  checks: []
report:
  channel: chat
```

`environment.pre_prod_branch` defaults to `pre-prod`. If that branch is absent,
empty, reserved as `main`/production, or cannot accept the merge, `tick` skips
auto-merge, records `needs-human`, and leaves production untouched. Promotion to
production remains a separate `promote` step: with an effective `gate: auto`
value, it auto-promotes only when required CI is green, `loopreview` passed,
configured evidence is present, and the red-line floor is clean. A failed
post-promote check triggers deterministic rollback to the recorded
`prior_stable_commit`. New scaffolds use `gate: human-merge`, which keeps the
explicit human promotion behavior unless the project opts into `auto`.

`evidence` is optional. When present, it is keyed by project type (`website`,
`cli`, `library`, or `app`) and supports simple proof fields such as
`preview_url`, `example_output`, `test_results`, and `preview_build`. `tick`
copies those configured artifacts into the JSON report and the human-readable
summary for dispatched, pending, and pre-prod items.

The verifier role has its own optional model and `reasoning_effort` depth
settings. Quote model IDs that contain YAML-special characters such as `[1m]`:

```yaml
verifier:
  model: "claude-opus-4-8[1m]"
  reasoning_effort: max
```

For compatibility signals such as `min_loopcoder_version`, see
[`stability-policy.md`](stability-policy.md).

## End-To-End Use

1. In a new repository, run `loopcoder init --repo .` once, or add
   `.delivery.yml` manually.

2. State a delivery need, for example:

   ```text
   /loopcoder add usage docs for the project
   ```

3. loopcoder inspects the repo context and drafts GitHub issues plus a
   dependency DAG. It shows the proposed issues, blocked-by relationships, and
   worker setting in chat.

4. You approve the plan before anything is published.

5. loopcoder creates the approved GitHub issues and dispatches ready issues to
   workers through `loopcoder dispatch` / `loopcoder dispatch-wave`. The binary
   creates a fresh git worktree, runs the selected provider, commits the
   resulting changes, pushes the branch, opens a PR, and cleans up.

6. loopcoder runs `loopcoder loopreview` for each PR, checks the diff and
   `gh pr checks`, relays Worker and Verifier report blocks verbatim, and
   reports progress, failures, risks, and final run state through
   `loopcoder status`. `codex` and `claude` have real verifier smoke proof;
   ambiguous, malformed, timed-out, or incomplete verifier output is still
   reported as `needs-human`.

7. After `loopreview = pass`, `tick` runs a deterministic risk gate. Clean PRs
   auto-merge into `environment.pre_prod_branch`; red-line PRs, red/missing CI,
   loopcoder-core changes, and verifier non-pass results are reported as
   `needs-human` and do not merge. After a pre-prod merge, `tick` reads the
   configured CI checks on the pre-prod branch head; if that head is the
   just-created merge commit and a required check is red, `tick` reverts that
   commit on pre-prod and records the PR as `needs-human`.

8. `loopcoder promote` advances the pre-prod batch to production/main. With an
   effective `gate: auto` value, promotion happens only after the deterministic
   gate has CI-green, verifier-pass, evidence-present, and red-line-clean
   signals, and it records rollback SHAs before merging. With `gate:
   human-merge`, you name the pre-prod batch or PRs to promote and loopcoder
   uses the explicit human merge path. `tick` never auto-merges to `main`.

## Version And Doctor

Use any of these forms to print one line of build information:

```text
loopcoder version
loopcoder --version
loopcoder -v
```

The output includes the loopcoder version, commit, build date, Go runtime, and
platform. The exact version, commit, and date values depend on the build
metadata available to the binary; a local source build without injected metadata
prints `version=dev commit=unknown date=unknown`.

```text
loopcoder version=<version> commit=<commit> date=<build-date> go=<go-version> platform=<os>/<arch>
```

`loopcoder doctor` is a read-only preflight for the current repository:

```text
loopcoder doctor --repo .
loopcoder doctor --repo . --format text
loopcoder doctor --repo . --format json
loopcoder providers refresh --repo .
loopcoder providers refresh --repo . --format json
```

It reports `[info]`, `[ok]`, `[warn]`, or `[fail]` checks for:

- `git` on `PATH`;
- `gh` on `PATH` and `gh auth status`;
- `.delivery.yml` presence and parse validity;
- configured worker and verifier provider CLI installation inventory;
- repository `origin` and detectable default branch;
- selected loopcoder binary path, version, commit, date, and release or
  development track;
- `.delivery.yml` schema version and `min_loopcoder_version` compatibility when
  declared;
- audit readiness: config parse result, effective threshold, configured SAST
  commands, required tool availability, parser recognition, rubric and
  baseline files, required `audit` CI check wiring, and Layer 2 verifier
  provider resolution;
- local-state protection: whether `.git/info/exclude` protects `.loopcoder/`,
  whether `.loopcoder/` files are already tracked, and the exact safe fix
  command when local state is tracked;
- reportquery readability for local report/run/relay records;
- storage permissions, storage health, and the current checkout's
  machine-local project registry identity, including ambiguity warnings;
- migration status, including concrete legacy surface identifiers,
  classifications, and per-surface remediation when migration warnings are
  present, plus nested run tree health for parent/child run records;
- project Claude Code conductor hook settings, warning when the
  `loopcoder hook conductor-reporter` or `loopcoder hook conductor-relay-guard`
  command is missing or when `loopcoder` does not resolve on `PATH`;
- resolved host profile (`codex-cli`, `claude-code`, `paseo-style`, or
  `generic-local`) and conductor runtime responsibility, which remains
  user-provided by the active host.

Provider installation is separate from provider authentication. Today `doctor`
checks `gh` authentication and bounded provider CLI installation inventory; it
does not infer provider-authentication status, account readiness, model
authorization, quota, invocation readiness, or usable capacity from an installed
CLI.

On the supported Darwin arm64 host, v0.8.0 creates and tightens
`$LOOPCODER_HOME` and `$LOOPCODER_HOME/data` with owner-only directory
permissions, and
`$LOOPCODER_HOME/data/loopcoder.db` plus SQLite `-wal` and `-shm` sidecars with
owner-only file permissions. The storage layer refuses symlink and non-regular
database paths before chmod or SQLite open. `doctor --repo .` reports insecure
existing modes without repairing them; `doctor --repo . --fix` tightens those
paths in place without deleting or recreating the database.

Windows, Linux/Ubuntu, WSL, containers used as a LoopCoder runtime, Intel
macOS, and Rosetta/amd64 macOS fail the v0.8.0 startup gate before storage is
opened or migrated. v0.7.0 remains the historical fallback for those hosts.

`doctor --format json` emits a stable support surface:

```json
{
  "repo_path": "/absolute/repo",
  "version": "0.8.0",
  "commit": "abc123",
  "date": "2026-07-08T00:00:00Z",
  "exit_code": 0,
  "host_profile": {
    "name": "codex-cli",
    "source": "env",
    "selector": "LOOPCODER_HOST",
    "invocation_style": "interactive Codex CLI conductor session calls loopcoder as a local subprocess",
    "supports_hooks": false,
    "supports_json_output": true
  },
  "runtime": {
    "home_dir": "/home/user/.loopcoder",
    "database": {
      "path": "/Users/example/.loopcoder/data/loopcoder.db",
      "exists": true,
      "schema_version": 30,
      "status": "ok",
      "message": "storage database is healthy"
    },
    "project_registry": {
      "status": "ok",
      "registered": true,
      "detached": false,
      "project_id": "proj_abc123",
      "identity_source": "github",
      "conflict_count": 0,
      "message": "project registry identity is registered"
    },
    "migration": {
      "status": "ok",
      "legacy_surfaces": 0,
      "message": "no legacy surfaces found"
    },
    "nested_runs": {
      "status": "ok",
      "run_count": 1,
      "parent_edges": 0,
      "child_edges": 0,
      "problem_count": 0,
      "message": "run tree readable; 1 run(s), no nested edges"
    }
  },
  "provider_inventory": {
    "schema_version": "loopcoder.provider_inventory_json.v1",
    "generated_at": "2026-07-12T00:00:00Z",
    "inventory_fingerprint": "sha256:...",
    "confidence": "exact",
    "installations": [],
    "probe_results": [],
    "account_profiles": [],
    "auth_readiness": [],
    "model_catalog_snapshots": [],
    "model_capabilities": [],
    "quota_telemetry_sources": [],
    "quota_snapshots": [],
    "gap_reasons": []
  },
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

`loopcoder providers refresh --repo .` explicitly persists the same
ProviderInstallation, ProbeResult, AccountProfile, AuthReadiness,
ModelCatalogSnapshot, ModelCapability, QuotaTelemetrySource, and QuotaSnapshot
inventory to the machine-local SQLite store. Refreshes append immutable probe
history, immutable catalog snapshots, and immutable quota snapshots, and mark
disappeared installations stale instead of deleting them. Probe and quota
diagnostic output is bounded and redacted, raw absolute paths and profile
references are redacted or hashed in JSON/human output, and command execution
uses fixed argv arrays, no shell interpolation, and an explicit non-credential
environment allowlist.
The allowlist carries location and platform variables required by provider
script shims, including Windows
`LOCALAPPDATA`, `APPDATA`, `ProgramData`, `ProgramFiles`, `SystemRoot`,
`ComSpec`, `PSModulePath`, `PATH`, `PATHEXT`, `TEMP`, `TMP`, `HOME`,
`USERPROFILE`, `OS`, `PROCESSOR_ARCHITECTURE`, and Unix `TMPDIR`, `LANG`, and
`LC_ALL`. A denylist still removes any variable whose name contains `key`,
`secret`, `token`, `password`, `credential`, or `auth`.

Auth readiness is independent of installation. Provider-supported readiness
commands may produce `ready`, `not-authenticated`, `expired`, or `unknown`;
unsupported providers return `unknown` with `auth-readiness-unsupported`.
The built-in declarations are credential-blind: Codex runs local status text
via `codex login status`; Claude runs local machine-readable status via
`claude auth status --json`; Gemini checks only declared auth artifact and
environment-name existence, so validity stays `unknown`; Antigravity declares
`agy models` as network-capable and the probe is skipped by default.
Readiness records can reference multiple credential-blind account profiles,
using deterministic `acct_` IDs from the adapter, source, and reference hash.
Profile displays are built only from allowlisted status structure such as a
redacted email or short handle after a marker like `profile` or `as`; otherwise
they fall back to `profile-<hash>`. If displays collide, policy must select the
opaque account profile ID rather than display text. The implementation does not
emit `expires_at` today because no current adapter exposes a credential-blind
machine-readable expiry field.

Network-declared auth probes are skipped by default. For example,
Antigravity's `agy models` auth probe is persisted as an auth-readiness
ProbeResult with `network_declared: true`, `network_permission: "denied"`, and
`gap_reasons: ["network-permission-denied"]`; the matching AuthReadiness record
is `unknown` and no network I/O is attempted.

Model catalog snapshots are captured from LoopCoder-owned static declarations
by default. Each ModelCapability records the exact snapshot ID, canonical model
ID, aliases, lifecycle state, availability state, runtime capability fields
(`read_only`, `json_output`, `nested_subagents`, `mcp_config`, `cancellation`,
and `token_usage_reporting`), freshness, confidence, and entry provenance.
Static catalog availability stays `unknown` because registry membership does
not prove account authorization or quota. Catalog listing commands that may
hit the network, such as Antigravity's `agy models`, are recorded as skipped
with an explicit `network-permission-denied` gap unless a future permission path
grants network access.

Quota telemetry is allowlist-only. Supported source declarations are limited to
official machine-readable provider APIs, official fixed-argv CLI JSON/status
commands, documented provider export files, LoopCoder's local ledger, operator
policy overlays, and fixtures. Private web UI scraping, copied browser cookies,
reverse-engineered endpoints, credential-file parsing, shell interpolation, and
environment value inspection are rejected instead of treated as best effort.
When no current provider exposes a safe machine-readable quota source,
`doctor` and JSON output still include a QuotaSnapshot with `confidence:
"unavailable"`, `freshness_state: "not-applicable"`, `reset_semantics:
"unknown"`, and `gap_reasons` such as `unsupported-source` and
`not-collected`; this is intentional and must not be rendered as exact quota.
Telemetry commands that declare possible network are skipped unless an explicit
future policy grants network permission, producing typed `network-denied`
evidence rather than performing network I/O.

Operators can add explicit executable locations without scanning a disk:

```yaml
provider_inventory:
  executables:
    custom-provider:
      - /opt/custom-provider/bin/custom-provider
  profile_selection:
    custom-provider: acct_abcdefghijklmnopqrstuvwxyz123456
```

Those paths are checked before PATH entries and rendered with
`discovery_source: "explicit-config"` and redacted path output. Profile
selection pins use opaque `acct_` IDs from prior inventory output; they are not
display labels and do not copy provider config.

`loopcoder doctor --repo . --format json` includes a `quota_usage_budget` root
object. It summarizes the machine-local usage ledger and, when the persisted
ledger has no rows for the project, conservatively derives a bounded summary
from the same local report surfaces used by `loopcoder report`: imported
reports, run attempt files, run JSON/JSONL records, relay ledgers, and pending
relay records. That fallback is marked with gap reasons such as
`persisted-ledger-empty`, `derived-from-reports-fallback`, and
`loopcoder-local-ledger-not-provider-global`. It is local evidence only and
must not be read as provider-global remaining quota.

`loopcoder status --repo . --format json` includes `inventory_refs` and
`quota_usage_refs`, not full raw inventory, quota, or usage history. When the
selected run has local report usage, `quota_usage_refs.usage_record_ids` lists
deterministic usage ledger record IDs and the confidence remains conservative
because local reports do not cover work outside LoopCoder. Until a DeliveryRun
binds inventory or quota records, unrelated arrays are empty and confidence is
`unknown`.

Hierarchical budget accounting is also machine-local. Budget policies can be
recorded for machine, project, DeliveryRun, task, worker, sub-agent, and
provider/account/model scopes, with `parent_budget_policy_ids` preserved for
audit inheritance. A reservation checks every applicable policy in its supplied
scope chain inside one SQLite `BEGIN IMMEDIATE` transaction, then either
reserves the full requested value at every level or persists a typed refusal
such as `ErrBudgetExhausted` without changing any aggregate. Hard ceiling
breaches refuse the reservation. Soft ceiling breaches are explicit:
`warn-only` reservations proceed with soft-breach gap reasons, while
`requires-approval` breaches return `ErrBudgetApprovalRequired` unless the
request includes an approval id. Commits, releases, cancellations, and stale
lease expiry are generation-fenced; replay still checks the current generation
before returning `replay: true` so stale callers cannot hide behind an old
operation key.

`doctor --format json` renders budget state under
`quota_usage_budget.budget_summary[]`. Each summary includes the policy id,
scope, quantity, effective ceiling, reserved value, committed value, available
value, confidence, policy version, active reservation ids, denial code when
present, soft-overflow gap reasons, and approval/override provenance. Budget
reservation records use ids derived from
`idempotency_key`, `budget_policy_id`, and `requester_id`, and persist
`requester_id`, `authorization_fingerprint`, source estimate usage record ids,
commit usage record ids, and release usage record ids for audit.

Unknown or estimated requirements do not silently consume budget. Exact local
accounting can reserve directly; estimated requirements require an explicit
approval id and are marked with conservative gap reasons. Budget override or
approval only changes accounting policy and is not a permission, safety, or
provider-auth override.

For a local zero-network smoke check, use:

```text
loopcoder budget smoke --repo . --project-id <project-id> --ceiling 100 --reserve 40 --commit 25 --format json
```

The command creates machine and project hard budget policies in the
machine-local store by default, reserves capacity, commits observed usage,
releases the unused reservation balance, and prints the resulting JSON
accounting summary. Use `--policy-mode soft --overflow-behavior warn-only` to
exercise soft-budget warning paths; text output prints budget warnings, and JSON
output includes the same `gap_reasons`. Reusing the same `--idempotency-key`
replays the same operation keys instead of reserving, committing, or releasing
twice.

When legacy migration surfaces are present, `runtime.migration.surfaces[]` and
the `migration status` check's `legacy_surfaces[]` entries include `surface`,
`identifier`, `classification`, `remediation`, and, where applicable, `legacy`,
`current`, `location`, `detail`, and `conflict`.

## Security Audit

`loopcoder audit` runs a read-only security audit. The deterministic layer
(`--layer sast`) runs configured SAST tools plus native secret and sensitive
file-mode scans and is suitable for required CI. The LLM layer (`--layer llm`
or `--layer all`) uses the configured verifier provider in read-only mode with
the built-in threat model plus any configured rubric.

```text
loopcoder audit --repo . --layer sast
loopcoder audit --repo . --layer all --provider claude --pretty
```

The exit codes are `0` clean, `1` threshold findings, `2` needs-human, `3`
command/runtime failure, and `4` relay gate. Configure thresholds, SAST argv
arrays, rubrics, and baselines under `.delivery.yml audit`. Baseline waivers
must record a justification, date, review/expiry date, and either a
`fingerprint` or `normalized_evidence`; stale waivers are reported without
gating, while expired or malformed waivers require human judgment.

Layer 2 reports are local-only, like `loopreview`: pretty blocks, canonical
JSON, relay records, and logs must not be copied into repository-visible
artifacts. See [`audit.md`](audit.md) for the full command reference.

## Model And Depth

`loopcoder models` prints the static model registry. It does not read
`.delivery.yml`, call provider CLIs, call `agy models`, read provider config,
mutate files, or require provider authentication:

```text
loopcoder models
loopcoder models --provider codex
loopcoder models --provider claude
loopcoder models --provider antigravity
loopcoder models --provider grok
```

The registry provider key for Google Antigravity is `antigravity`; `agy` is the
CLI executable name. `loopcoder models --provider agy` exits non-zero and hints
to use `--provider antigravity`. Grok intentionally has no fabricated static
model row: `loopcoder models --provider grok` identifies dynamic inventory, and
`providers refresh` records the bounded catalog evidence available from the
installed `grok` CLI.

Worker and Verifier model selection is role-scoped. For each role, provider
comes from the command flag, then `.delivery.yml` (`adapters.worker` or
`adapters.verifier`), then the built-in role fallback (`codex` for Worker,
`claude` for Verifier). Model comes from the role command flag, then
`worker.model` or `verifier.model`, then the static registry default model for
the resolved provider. Depth comes from the role command `--effort` flag, then
`worker.reasoning_effort` or `verifier.reasoning_effort`, then the resolved
model's default depth.

The initial registry defaults are:

| Provider | CLI | Default model | Default depth |
| --- | --- | --- | --- |
| `codex` | `codex` | `gpt-5.5` | `high` |
| `claude` | `claude` | `claude-opus-4-8[1m]` | `max` |
| `antigravity` | `agy` | `Gemini 3.1 Pro` | `High` |
| `grok` | `grok` | dynamic provider inventory | provider default |

Configured model and depth values are exact and case-sensitive. By default,
invalid provider, model, or depth selections warn and keep the selected
pass-through value. Strict mode rejects invalid selections before launching a
provider. Enable strict mode durably with:

```yaml
models:
  strict: true
```

For one run, use `--strict` on the commands that resolve Worker or Verifier
model/depth selections: `dispatch`, `dispatch-wave`, `loopreview`, `audit`,
`tick`, `trigger`, and `recover`.

For a single run, say what you want in chat, such as:

```text
run faster
#B use max
```

The conductor translates that request into exact provider/model/depth flags for
that run only. Natural-language depth mapping is still a conductor concern; the
binary validates exact registry tokens such as `high`, `max`, or Antigravity's
capitalized `High`.

For a permanent project preference, say so explicitly, for example:

```text
from now on default to high
```

Only then should loopcoder write `worker.reasoning_effort`, `worker.model`,
`verifier.reasoning_effort`, or `verifier.model` into `.delivery.yml`. Absent
fields mean "resolve from the static registry at runtime," not "inherit a
provider global config."

Antigravity setup uses the Google Antigravity CLI:

```text
agy login
agy models
loopcoder providers refresh --repo .
loopcoder doctor --repo .
```

When the configured Worker or Verifier provider is `antigravity`, `doctor`
and `providers refresh` look for executable `agy` through bounded installation
probes and then record declared network auth/catalog probes as skipped by
default. Auth readiness and model availability are never inferred from
installation evidence. The Antigravity Worker invocation uses this argv shape after
registry/default resolution:

```text
agy -p <prompt> --add-dir <worktree> --model "<model> (<Depth>)"
```

The mandatory `--add-dir` is the workspace pin. Antigravity read-only mode is
not available or verified, so `loopreview` and audit-review invocations fail
closed when selected with provider `antigravity`.

## Doc-First Process

New work is documented first, coded second, and verified last. The mandatory
workflow is described in [`PROCESS.md`](../PROCESS.md):

1. Write and merge the design or spec under `docs/`.
2. Open separate code issues only after the relevant document is merged.
3. Verify the implementation against the merged document and working behavior.

Documentation and code are intentionally not bundled in the same issue or PR.

## Binary Commands

Use the native `loopcoder` commands as the helper interface. The stable
inventory below matches v0.8.0:

```text
loopcoder version
loopcoder --version
loopcoder -v

loopcoder doctor --repo .
loopcoder doctor --repo . --format json

loopcoder models
loopcoder models --provider antigravity
loopcoder models --provider grok

loopcoder providers refresh --repo .
loopcoder providers refresh --repo . --format json

loopcoder budget smoke --repo . --project-id <project-id> --format json

loopcoder audit --repo . --layer sast
loopcoder audit --repo . --layer all --provider claude --strict

loopcoder skill install --repo .

loopcoder init --repo .
loopcoder init --repo . --force
loopcoder init --repo . --gate auto
loopcoder init --repo . --worker-model <model> --worker-effort <effort>
loopcoder init --repo . --verifier-model <model> --verifier-effort <effort>

loopcoder discover --repo .
loopcoder compile --repo .

loopcoder ready-set --repo . --base-branch main --format text

loopcoder dispatch \
  --repo . \
  --issue-number <number> \
  --issue-title "<title>" \
  --issue-body "<body>" \
  --base-branch main \
  --provider codex \
  --foreground

loopcoder dispatch \
  --repo . \
  --issue-number <number> \
  --issue-title "<title>" \
  --provider codex \
  --strict \
  --pretty \
  --foreground

loopcoder dispatch \
  --repo . \
  --issue-number <number> \
  --issue-title "<title>" \
  --detach

loopcoder dispatch-wave --repo . --base-branch main --issue-numbers <n1>,<n2> --strict
loopcoder dispatch-wave --repo . --base-branch main --issue-numbers <n1>,<n2> --strict --foreground

loopcoder tick --repo . --strict
loopcoder tick --repo . --strict --foreground
loopcoder trigger goal-loop --repo . --max-iterations <n> --strict
loopcoder promote --repo .

loopcoder status --repo .
loopcoder status --repo . --run <run-id>
loopcoder status --repo . --format json
loopcoder attach --repo . --run <run-id>
loopcoder cancel --repo . --run <run-id>
loopcoder report --repo .
loopcoder report --repo . --verbose
loopcoder report --repo . --format json

loopcoder relay list --repo .
loopcoder relay flush --repo .

loopcoder resume --repo . --run-id <run-id>

loopcoder recover \
  --repo . \
  --issue-number <number> \
  --issue-title "<title>" \
  --issue-body "<body>" \
  --run-id <run-id> \
  --strict

loopcoder verify-local --repo . --pr-number <pr>

loopcoder loopreview --repo . --pr-number <pr> --provider claude --strict
loopcoder loopreview --repo . --pr-number <pr> --provider claude --pretty

loopcoder attest \
  --role conductor \
  --provider <host-provider> \
  --model <host-model> \
  --permission orchestrate \
  --action "<delivery action>" \
  --duration-ms <milliseconds> \
  --total-tokens <tokens>

loopcoder attest --pretty \
  --role conductor \
  --provider <host-provider> \
  --model <host-model> \
  --permission orchestrate \
  --action "<delivery action>" \
  --duration-ms <milliseconds> \
  --total-tokens <tokens>

loopcoder state push --repo . --run-id <run-id>
loopcoder state pull --repo .
loopcoder lease acquire --repo . --run-id <run-id>
loopcoder lease release --repo . --run-id <run-id>

loopcoder upgrade --version 0.8.0

loopcoder hook conductor-reporter
loopcoder hook conductor-relay-guard
loopcoder ps --repo .
loopcoder kill --repo . --run <run-id>
loopcoder kill --repo . --all
```

Machine-local registry, migration, nested-run, and DeliveryRun commands are
part of the v0.8.0 command inventory. Inventory does not imply that every mode
is a supported product path:

```text
loopcoder projects register --repo .
loopcoder projects list --format json
loopcoder projects show --repo .
loopcoder projects remove --repo .

loopcoder migrate local-state --repo . --dry-run
loopcoder migrate local-state --repo .
loopcoder migrate local-state --repo . --format json

loopcoder migrate storage --format text
loopcoder migrate storage --format json
loopcoder migrate storage --apply --format json

loopcoder nested run --repo . --plan child-plan.json --provider codex --format json

loopcoder status --repo . --format json
loopcoder report --repo . --run <run-id> --format json

loopcoder delivery plan --project-id <project-id> --run-id <run-id> --format json
loopcoder delivery decide --project-id <project-id> --run-id <run-id> --action approve --expected-authorization-fingerprint <sha256:...>
loopcoder delivery continue --project-id <project-id> --run-id <run-id> --expected-authorization-fingerprint <sha256:...>
```

`hook` is for host hook integration rather than normal customer workflow.
`discover`, `compile`, `trigger`, `state`, `lease`, `ps`, and `kill` are
advanced/operator commands. `state push` is the explicit state-branch publish
path; `delivery` is the v0.8.0 approval-gating surface for DeliveryRun records;
`kill` only targets loopcoder-managed processes for
a run or repository and should not be used as a bare process-name terminator.

### DeliveryRun Approval Gates (v0.8.0)

`loopcoder delivery` is the host-safe v0.8 DeliveryRun approval surface for
terminal users and JSON callers. It works against the machine-local storage
database and takes explicit `--project-id` plus `--run-id` selectors.

`loopcoder delivery plan` is side-effect-free at the DeliveryRun layer: it reads
the persisted run, task, and dependency rows and returns a fingerprinted
proposal without launching providers, dispatching workers, mutating worktrees,
editing GitHub, changing credentials, or writing DeliveryRun rows. The response
contains `input_fingerprint`, `policy_fingerprint`, `plan_fingerprint`, and
`authorization_fingerprint`; callers should pass the authorization fingerprint
back to `decide` and `continue` with
`--expected-authorization-fingerprint`.

`loopcoder delivery decide` records a fingerprint-bound decision in one write
transaction. Supported actions are `approve`, `reject`, `edit`, `expire`, and
`supersede`. An approval is valid only for the exact current proposal
fingerprint; changed task requirements, scope, dependency graph, policy inputs,
or run intent produce `ErrStaleApproval` and require a fresh `plan` response
and decision.

`loopcoder delivery continue` is approval-gated. It verifies that the current
proposal still matches the expected fingerprint, that an active approval exists
and is not expired, and only then advances the DeliveryRun to a schedulable
state. It does not dispatch workers or launch providers.

### Nested Child Plans

`loopcoder nested run` is the v1 nested-plan command, but no real-provider
nested execution mode is supported in v0.8.0:

```text
loopcoder nested run --repo . --plan child-plan.json --provider codex
loopcoder nested run --repo . --plan child-plan.json --provider claude --format json
```

The plan file must use `schema_version: "loopcoder.child_plan.v1"`. The command
validates child keys, dependencies, depth, fan-out, declared scope, permission,
and aggregation policy before scheduling child work. It persists the accepted
plan and parent/child run graph in `$LOOPCODER_HOME/data/loopcoder.db`.
Re-running
the same `plan_id` first resolves child identity from the durable SQL
`(plan_id, child_key)` edge: terminal children are reported from durable state
without relaunching the provider, queued/running/interrupted children are
`resumed` only after acquiring an execution claim, and `needs-human` children
are blocked for human action. A changed plan body under an existing `plan_id`
fails closed; use a new `plan_id` for a real plan revision.

Provider launch is guarded by a durable child execution claim. Claim, child run
status, parent edge status, and the SQL transition event are updated in one
immediate write transaction with bounded retry on SQLite busy errors. Only the
claim owner for the current `claim_generation` may write terminal completion.
If another scheduler already owns an active lease, replay returns a non-error
observation with `claim_outcome`, `claim_owner`, `claim_generation`, and
`lease_expires_at`; it does not execute the provider. Expired leases are
recovered by a new generation, and stale owners are fenced from publishing
terminal status. This prevents duplicate provider launch while a claim is
active, but it does not promise universal exactly-once external side effects
after a crash; ambiguous side effects must be resolved with receipts,
idempotency keys, or `needs-human`.

In v0.8.0, production-provider `write` and `orchestrate` children are refused
before dispatch. The command accepts a `read-only` child but sends it through
the ordinary Worker adapter without an enforceable mutation-free permission,
so that accepted path is also unsafe for real providers. Do not use Codex,
Claude, Grok, Antigravity, Gemini, or any other real provider with `nested run`
in v0.8.0. The command's plan, scheduler, persistence, claim, recovery, and
permission records are implementation inventory and deterministic-test
infrastructure, not proof of a safe product bridge.

The reserved `test-subprocess` provider exists only for deterministic local and
release smoke tests. It executes each child item's `scope.commands` as real local
subprocesses and writes ordinary local attempt/report records without calling a
remote provider.

## Exit Codes

- `loopcoder loopreview` reserves `0`, `1`, and `2` for clean verifier verdicts:
  `pass`, `fail`, and `needs-human`.
- `loopcoder loopreview` exits `3` when the command itself fails before or after
  a clean verdict, such as invalid flags, repository/config/provider setup
  failure, or output/relay write failure.
- `loopcoder audit` reserves `0`, `1`, and `2` for clean audit verdicts:
  `clean`, `findings`, and `needs-human`; it exits `3` for command/runtime
  failures.
- Mechanical progress commands exit `4` when the relay hard gate finds pending
  local-only Worker/Verifier blocks. Run `loopcoder relay flush --repo <path>`
  to print and acknowledge them, or `loopcoder relay list --repo <path>` to
  inspect without clearing.
- `loopcoder delivery decide` and `loopcoder delivery continue` reserve `10`
  for pending or expired approval, `11` for stale plan or stale approval, `12`
  for rejection, and `13` for deterministic policy denial.

## Verifier Provider Status

`loopcoder loopreview` has 0.3.3 smoke proof for the `claude` and `codex`
verifier mechanism: both providers returned a valid structured
verdict plus report within the timeout. This proof does not make the
LLM verdict itself deterministic; `pass` and `fail` remain model judgments that
can vary across otherwise valid runs.

One representative point-in-time run used merged PR #202 (`0.3.3: loopreview
bounded review packet`) with the command:

```text
loopcoder loopreview --repo . --pr-number 202 --provider <provider> --timeout 180s
```

The review packet was not truncated: 2 changed files, 73 changed-file bytes
of 8,192, 29,730 diff bytes of 81,920, max per-file patch 18,925 bytes of
24,576, 541 issue-body bytes of 12,288, and 13,769 spec bytes of 40,960.

| Provider | Wall elapsed | Parsed model / effort | Token usage | Verdict in this run | Inputs truncated |
| --- | ---: | --- | --- | --- | --- |
| `codex` | 77.398s | `gpt-5.5` / `xhigh` | `18,266` total | `pass` | no |
| `claude` | 70.686s | `claude-haiku-4-5-20251001` / unset | `2,447` input / `4,947` output | `pass` | no |

The `codex` verifier run emits the expected reviewer-not-worker advisory when
the repository worker default is also `codex`; that warning does not block the
run, but `claude` remains the preferred verifier for default Codex-worker
repos. `gemini` remains unverified for `loopreview` until issue #188 resolves
headless authentication.
`antigravity` is registered as a provider but read-only mode is not available
or verified, so selecting it for `loopreview` fails closed rather than running
a mutating review.

## Reporter

Worker and verifier invocations produce validated local-only reports
with provider, model, effort, permission, action, exit code, timing, and
verification fields. For providers with parseable usage, including `codex` and
`claude`, records use `model_source: parsed`, real parsed model identity, and
token usage. For Claude runs with an explicit pinned model, the reported model
is the pinned/configured model when that exact model appears in
provider-reported usage; a token-dominant auxiliary model does not override it.
Missing required identity or usage fails closed for providers that expose those
signals: `dispatch` opens no PR, and `loopreview` returns `needs-human` with
the incomplete-report finding. Antigravity is a provider-scoped exception:
Worker records use the selected `agy --model` string, such as `Gemini 3.1 Pro
(High)`, as `model_source: self-reported` and accept absent token usage.
Reporter surfaces are local-only:
foreground `dispatch` / `loopreview` stderr pretty blocks,
`dispatch-wave --foreground` stdout Worker pretty blocks, `dispatch` /
`loopreview` result JSON, and
gitignored `.loopcoder/` run records. PR bodies, merge commits, and merge
comments are not reporter surfaces and must not contain `[reporter]` headers
or canonical JSON.

## Local State And Storage Migration

`loopcoder migrate local-state --repo .` imports v0.6.x repo-local
`.loopcoder/` attempts, events, reports, recovery briefs, and relay records into
the machine-local SQLite store under `$LOOPCODER_HOME/data/loopcoder.db`.
Use `--dry-run` first to scan the same sources and report the records that would
be imported without registering the project or writing the database.

The migration is explicit and idempotent. It registers or refreshes the current
project identity, records source-path and content-hash metadata, and skips
records already imported on prior runs. Malformed JSON or JSONL input is
reported with a source path and line when available, but malformed records do
not abort import of other valid records.

The command copies state only. It does not delete `.loopcoder/`, rewrite legacy
files, edit tracked repository files, mutate GitHub, or publish state to the
state branch. Existing file readers remain the fallback during the
compatibility window. After migration, `loopcoder report --repo . --format json`
includes imported records with `source` values such as `imported:attempt` plus
the original source path metadata.

Registered projects write new audit/relay/runtime file payloads under the
global project payload root. Legacy audit logs remain file-only repo-local
state, are not imported by `migrate local-state`, and an audit-only
`.loopcoder/audit/` directory does not make `loopcoder doctor --repo .` require
a local-state migration.

To back up the v0.8.0 runtime state, copy `$LOOPCODER_HOME/data/loopcoder.db`
and, when present, `$LOOPCODER_HOME/projects/`, `$LOOPCODER_HOME/logs/`, and
`$LOOPCODER_HOME/tmp/` while no loopcoder command is running. To remove the
machine-local runtime state completely, delete those same paths; the next
loopcoder command recreates storage as needed. Removing machine-local runtime
state does not delete repo-local `.loopcoder/` history, and deleting
repo-local `.loopcoder/` is still a manual user action outside migration.

Before any stateful v0.8.0 command opens v0.7 machine-local state, run
`loopcoder migrate storage --format json`. Planning is the default and does not
create or modify files. After reviewing the schema-9-to-schema-30 steps and
rollback contract, stop all LoopCoder processes and run `loopcoder migrate
storage --apply --format json`. The migration captures and verifies an
owner-only schema-9 backup before the write transaction, then applies versions
10 through 30 atomically. Repeated application is a `no-op`.
See [`storage-migration.md`](storage-migration.md) for the exact backup,
failure, and offline rollback contract. Backup images are local runtime state
only: do not copy them into repositories, PR bodies, issue comments, commits,
or tracked fixtures.

The schema-v10 migration preserves legacy run identity but does not infer an
approved v0.8 plan, approval, override, or plan fingerprint from logs or issue
titles. Legacy non-terminal execution claims that cannot prove whether provider
side effects launched are imported as `needs-human` with
`ErrAmbiguousLegacyState`; inspect the legacy run, claim, provider receipt, and
recovery context before continuing.

Opening a database older than schema 16 with a v0.8 planner-capable binary also
adds `task_requirements` and `task_requirement_overrides`. Operators can inspect
these rows through future routing/status surfaces; until then they are local
planner evidence. User corrections are stored as scoped overrides and are
revalidated every time a later classification applies them, so corrections that
would lower deterministic risk, permission, or side-effect floors fail closed.

`loopcoder report` is the read-only query surface for those local records:

```text
loopcoder report --repo .
loopcoder report --repo . --work-id <run-id>
loopcoder report --repo . --run <run-id> --format json
loopcoder report --repo . --issue 218
loopcoder report --repo . --role worker
loopcoder report --repo . --verbose
loopcoder report --repo . --format json
```

Default text output is a compact human receipt. It does not embed raw JSON:

```text
REPORTS
loopcoder report: worker succeeded

Target
- work ID: run-218
- issue: #218
- branch: loop/issue-218

Verdict
- status: succeeded
- blocking defects: 0
- reason: completed without a blocking report signal

Review summary
- acceptance criteria: not reviewed
- regressions found: none reported
- findings: none

Run
- worker: OpenAI Codex / codex / gpt-5.5 (xhigh) (parsed) / xhigh
- permission: write
- action: "implement issue #218"
- exit: 0
- duration: 42.0s
- tokens: input=120  output=34  total=154

Next
- run verifier review before calling the PR merge-eligible
- details: loopcoder report --work-id run-218 --verbose
- raw JSON: loopcoder report --work-id run-218 --format json
Source
- source: attempt
- run ID: run-218
- path: .loopcoder/runs/run-218/workers/job-218-1.attempt.json
```

Use `--verbose` to append the stable `[reporter]` header and canonical JSON
record to text output. Use `--format json` when a machine needs parseable JSON
with no receipt text mixed in.

JSON output keeps the compatibility `reports` array and adds `records` with
source/run/path context. When `--run <run-id>` is provided with JSON output,
the payload also includes the same additive `run_tree` object exposed by
`loopcoder status --format json`:

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
      "run_id": "run-218",
      "path": ".loopcoder/runs/run-218/workers/job-218-1.attempt.json"
    }
  ],
  "run_tree": {
    "root_run_id": "run-218",
    "selected_run_id": "run-218",
    "nodes": [
      {
        "project_id": "proj_abc123",
        "run_id": "run-218",
        "child_run_ids": [],
        "depth": 0,
        "issue": 218,
        "role": "worker",
        "provider": "codex",
        "model": "gpt-5.5",
        "effort": "xhigh",
        "permission": "write",
        "lifecycle_status": "succeeded"
      }
    ],
    "summary": {
      "run_count": 1,
      "terminal_runs": 1,
      "interrupted_runs": 0,
      "failed_runs": 0,
      "needs_human_runs": 0
    }
  }
}
```

Reporter-producing commands use explicit output modes:

| Mode | Intended reader | Output |
|---|---|---|
| default text | people and merged-stream host integrations | concise receipt only, with no raw canonical JSON |
| `--format json` | machines | one JSON value with no prefix, suffix, header, or receipt text |
| `--verbose` | local debugging and compatibility inspection | canonical headers/records and diagnostic details in addition to the human receipt where supported |

The same contract applies to Worker (`dispatch` and `dispatch-wave`), Verifier
(`loopreview`), audit Layer 2, and Conductor self-report (`attest`) receipts.
During the 0.6.x transition window, readers accept legacy `[attestation]`
headers and nested `attestation` objects from prior output, but newly emitted
JSON uses `[reporter]` and `report` per
[`../specs/0567-reporter.md`](../specs/0567-reporter.md).

Use `--format json` when a parent process needs the stable dispatch result
schema:

```json
{"ok":true,"issue":218,"branch":"loop/issue-218","run_id":"run-218","pr":"https://github.com/owner/repo/pull/999","summary":"Worker summary","attempt_path":".loopcoder/runs/run-218/workers/job-218-1.attempt.json","status":"succeeded","exit_code":0,"log_bytes":12345,"report":{"role":"worker","provider":"codex","model":"gpt-5.5","model_source":"parsed","effort":"xhigh","permission":"write","action":"implement issue #218","exit_code":0,"started_at":"2026-06-29T00:00:00Z","ended_at":"2026-06-29T00:00:42Z","duration_ms":42000,"usage":{"input_tokens":2447,"output_tokens":4461,"total_tokens":6908},"verified":true}}
```

Use `--verbose` when you intentionally need the historical debugging records:
the stable `[reporter]` header, canonical report JSON, and command result JSON.

Before this output contract, a merged verifier transcript could show
`needs-human` while its receipt reason was a positive evidence sentence:

```text
- status: needs-human
- reason: All five acceptance criteria satisfied and no regressions were found.
```

The receipt now separates decision reason from next action and chooses the
escalating finding for `needs-human`:

```text
- status: needs-human
- reason: docs/specs/merged-design.md: merged design/spec unavailable: origin/main does not contain the referenced file

Next
- human should decide whether the reported uncertainty is acceptable for this PR
```

The receipt is conclusion-first and always uses `Target`, `Verdict`, `Review
summary`, `Run`, and `Next`. It displays provider vendor and provider key on
one combined line, such as `OpenAI Codex / codex`, `Anthropic / claude`, or
`Google Antigravity / antigravity`. It renders parsed model sources as
`(parsed)` and self-reported sources as `(self-reported)`, displays `started`
and `ended` in the host local timezone to whole seconds, reports compact
duration, and groups token counts with thousands separators. When input and
output tokens are present without a total, the receipt derives
`total=<input+output>` for display only; canonical JSON and the stable
`[reporter]` header are unchanged.

`--pretty` or `LOOPCODER_PRETTY=1` forces the emoji form even on non-TTY
output. `--no-pretty` or `LOOPCODER_NO_PRETTY=1` suppresses the pretty block
and wins over any force or default setting. When pretty output is shown,
`NO_COLOR`, `LOOPCODER_PLAIN=1`, or `LOOPCODER_NO_EMOJI=1` forces the plain
ASCII form.

Pretty output is diagnostic local output only. Together with JSON mode,
verbose mode, and gitignored `.loopcoder/` run records, it is a
local reporter surface only. It is not copied into PR bodies, comments,
commits, merge commit bodies, merge comments, or other repository-visible
artifacts. The `relay` command group is the explicit recovery surface:
`relay flush` prints pending blocks verbatim to stdout and clears them, while
`relay list` inspects pending records without clearing. `conductor-relay-guard`
locally backstops hidden or suppressed `dispatch`, `dispatch-wave`, and
`loopreview` blocks where hooks are active. Machine consumers should use
`--format json`; local compatibility tools that inspect reporter headers should
use `--verbose`. For one release, relay and conductor hook matchers accept both
`[reporter]` and legacy `[attestation]` tokens.

`loopcoder attest` is the one-version compatibility alias for Conductor
self-reports. Default text mode emits the Conductor receipt. `--format json`
emits the canonical report JSON only, while `--verbose` emits canonical JSON
followed by the one-line `[reporter] ...` header. It forces `model_source` to
`self-reported`, and forces `verified` to `false` even if flags try to set other
values. It exits non-zero when required fields are missing or invalid,
including provider, model, action, timing, and usage. Provide either
`--total-tokens` or both `--input-tokens` and `--output-tokens`.

Pretty output uses emoji when the target is an interactive terminal, or when
emoji is forced, and emoji is not disabled:

```text
✅ loopcoder report: worker succeeded

Target
- work ID: run-218
- issue: #218
- branch: loop/issue-218

Verdict
- status: succeeded
- blocking defects: 0
- reason: completed without a blocking report signal

Review summary
- acceptance criteria: not reviewed
- regressions found: none reported
- findings: none

Run
- worker: OpenAI Codex / codex / gpt-5.5 (xhigh) (parsed) / xhigh
- permission: write
- action: "implement issue #218"
- exit: 0
- duration: 7m53.9s
- tokens: total=165,268

Next
- run verifier review before calling the PR merge-eligible
```

Non-interactive default output, `NO_COLOR`, `LOOPCODER_NO_EMOJI=1`, or
`LOOPCODER_PLAIN=1` uses the plain ASCII form with the same fields:

```text
loopcoder report: verifier pass

Target
- work ID: loopreview-295
- issue: #295
- PR: #295

Verdict
- status: pass
- blocking defects: 0
- reason: review passed

Review summary
- acceptance criteria: satisfied
- regressions found: none reported
- findings: none

Run
- verifier: Anthropic / claude / claude-opus-4-8[1m] (max) (parsed) / max
- permission: read-only
- action: "review PR #295"
- exit: 0
- duration: 2m7.0s
- tokens: input=2,447  output=9,844  total=12,291

Next
- continue with the configured merge or promotion gate
```

Design rationale: [`../specs/0567-reporter.md`](../specs/0567-reporter.md),
[`../specs/0146-attestation.md`](../specs/0146-attestation.md),
[`../specs/0214-human-readable-attestation.md`](../specs/0214-human-readable-attestation.md),
[`../specs/0218-surface-worker-attestation.md`](../specs/0218-surface-worker-attestation.md),
[`../specs/0282-default-pretty-attestation.md`](../specs/0282-default-pretty-attestation.md),
[`../specs/0296-attestation-display-polish.md`](../specs/0296-attestation-display-polish.md),
[`../specs/0300-model-attribution.md`](../specs/0300-model-attribution.md),
[`../specs/0306-local-only-attestation.md`](../specs/0306-local-only-attestation.md),
and [`../specs/0316-conductor-local-enforcement.md`](../specs/0316-conductor-local-enforcement.md).

## Troubleshooting v0.8.0

| Symptom | Meaning | Safe next action |
| --- | --- | --- |
| Exit `78` with `ErrUnsupportedPlatform` | The host is not native `darwin/arm64`; no startup side effect was allowed. | Use v0.7.0 on that host or move the v0.8.0 work to Apple Silicon macOS. Do not bypass the platform gate. |
| `migrate storage` reports pending steps | A v0.7 schema-9 database has not been upgraded. | Stop all LoopCoder processes, save the JSON plan, then run `loopcoder migrate storage --apply --format json`. |
| Migration apply fails | The write transaction did not reach a verified current schema. | Preserve the database, backup, and diagnostic; fix disk/path/permission conditions and rerun. Never edit `PRAGMA user_version` manually. |
| A detached run appears quiet | The provider may be running, queued, waiting, or unable to deliver a host notification. | Run `loopcoder status --repo . --run <id>`, `loopcoder attach --repo . --run <id>`, and `loopcoder ps --repo .`. Active work should have no durable progress gap over five minutes. |
| Recovery sees live or ambiguous provider authority | Redispatch could duplicate useful work or target a reused PID. | Keep the existing run, inspect `status`/`ps`, and follow the `needs-human` action. Do not launch another provider manually. |
| Quota is unavailable or stale | The installed provider did not expose a supported fresh source, or network permission was not granted. | Run `loopcoder providers refresh --repo . --format json`; inspect gap reasons and reset evidence. LoopCoder will not guess capacity. |
| One provider is exhausted | The routing decision must use another hard-eligible provider with fresh account/model/quota evidence. | Refresh inventory, then rerun planning/routing. If no eligible route exists, wait for the recorded reset or approve an explicit policy change. |
| Grok is installed but has no selectable model | Dynamic `grok models` evidence was absent, unsupported, stale, or attributed to a non-xAI endpoint. | Run `grok version`, authenticate with the provider-owned flow, then `loopcoder providers refresh --repo .`; do not add a fabricated static model. |
| Rollback to v0.7.0 is required | v0.7.0 cannot open schema 30. | Stop all LoopCoder processes and follow [`storage-migration.md`](storage-migration.md): copy the verified schema-9 backup into an offline home, select v0.7.0, and accept loss of v0.8-only state. |

When reporting a failure, retain the stable error code, run ID, candidate
version/commit, platform tuple, redacted doctor JSON, and local evidence paths.
Do not paste credentials, raw provider sessions, reporter canonical JSON, or
machine-local database contents into a public issue.

## Limits

LoopCoder remains deliberately bounded: one conductor coordinates a small
approved batch, and no autonomous cloud scheduler replaces the human release
and production gates. Non-interactive worker execution can outlive the host
turn through durable detached supervision, and a later conductor can inspect,
attach, cancel, or reconcile it from machine-local state. Waiting for CI,
approval, quota reset, outbox delivery, or worker terminalization is local and
provider-free. Ambiguous external side effects still stop at `needs-human`;
LoopCoder does not promise universal exactly-once behavior across arbitrary
provider or GitHub failures. See [`architecture.md`](architecture.md) for the
current architecture and limits.
