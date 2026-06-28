---
id: 89
title: Go Native CLI Migration
status: accepted
date: 2026-06-27
issue: 89
pr: null
supersedes: []
superseded_by: []
---

# Go Native CLI Migration

Status: DESIGN. This is a target design and is not yet built.

This document defines the doc-first migration from loopcoder's current
PowerShell helper layer to a single cross-platform Go binary. It is written per
[`PROCESS.md`](../PROCESS.md): this design should merge before any Go code lands,
and the implementation should be split into follow-up PRs with one concern per
PR.

The design is intentionally a compatibility design, not a rewrite of
loopcoder's product model. The existing PowerShell scripts and the current
design documents remain the behavioral contract until a Go command proves
parity.

## Problem And Motivation

loopcoder's conductor is already cross-platform in the important sense: it is
the Opus session following [`../SKILL.md`](../../SKILL.md) inside Claude Code. The
conductor reads the playbook, plans issues, reviews PRs, reports status, and
honors the human merge gate without relying on Windows-only process semantics.

The part that is not cross-platform is smaller and more concrete: the
mechanical helper layer is PowerShell and Windows-bound. Today the conductor
calls:

- `scripts/dispatch-worker.ps1`
- `scripts/ready-set.ps1`
- `scripts/resume.ps1`
- `scripts/recover-and-retry.ps1`
- `scripts/verify-local.ps1`

Those scripts currently own the deterministic mechanics around GitHub, git
worktrees, Codex execution, local run state, recovery briefs, ready-set
classification, and local verification. The current dependency on PowerShell is
not acceptable for macOS usage because requiring `pwsh` on macOS turns a
portable Claude Code skill into a Windows-first tool with a heavyweight shell
prerequisite.

The lock-in is narrow enough to remove cleanly. The main Windows-specific
implementation details are:

- `cmd /c "codex ... < promptfile"` in
  `scripts/dispatch-worker.ps1`, which depends on
  `cmd.exe` for stdin redirection and log redirection.
- `[System.Threading.Mutex] "Global\..."` in
  `scripts/dispatch-worker.ps1`, which serializes
  `git worktree add` with a Windows named mutex.

The goal is a single native helper binary named `loopcoder` that the conductor
can call on macOS, Linux, and Windows with no PowerShell dependency. The binary
does not replace `git`, `gh`, or `codex`; those remain the external adapters
already described by [`architecture.md`](../reference/architecture.md). The binary replaces
the PowerShell glue around those tools.

## Goals

- Provide a single cross-platform Go binary, `loopcoder`, that replaces the
  PowerShell helper layer.
- Preserve identical observable behavior and identical state/config formats.
  The `.ps1` scripts are the reference implementation during the migration.
- Keep the conductor model intact: Opus reads
  [`../SKILL.md`](../../SKILL.md), chooses the next action, and calls the binary as
  its hands.
- Cleanly fix the Windows-specific process and locking details while porting.
- Keep model and reasoning-effort handling as pass-through only. If the caller
  does not explicitly provide a model or effort override, the binary must omit
  those Codex flags and inherit the user's Codex configuration.
- Make parity testable. Each ported subcommand should have fixture or golden
  coverage for config parsing, state parsing, classifications, output shape,
  and error routing before the conductor playbook switches to it.

## Non-Goals

This migration is Scope A only: it replaces the helper implementation language.
It does not change the loopcoder runtime model.

Out of scope:

- Replacing the Opus conductor. The conductor remains the
  [`../SKILL.md`](../../SKILL.md) playbook running in Claude Code. The binary is
  the mechanical layer it calls.
- Building the autonomous/background runtime described in
  [`../DESIGN.md` section 12](../../DESIGN.md#12-local-v1-vs-cloud-v2). That is
  Scope B. The Go binary may include foreground helpers that are shaped like
  future ticks, but this document does not authorize a daemon, cron loop, or
  unattended cloud conductor.
- Changing `.delivery.yml` schema, the `.loopcoder/runs/<RunId>/` layout,
  GitHub issue/PR/label/check flow, branch names, PR body conventions, or the
  conductor's intelligence.
- Changing the doc-first process in [`PROCESS.md`](../PROCESS.md).
- Changing the human merge gate. A passing verification state still means
  merge-eligible, not auto-merged.
- Adding provider-specific worker intelligence. The first Go binary keeps the
  current `codex` worker adapter behavior and keeps other providers deferred.
- Introducing model or effort defaults. The binary must not pick a model,
  effort, or per-issue routing policy.
- Bundling Go code in this documentation PR.

## Current Helper Responsibilities

The PowerShell layer is thin, but it is not trivial. The Go port must preserve
the responsibilities that currently live in the scripts:

| Script | Current responsibility |
| --- | --- |
| `dispatch-worker.ps1` | Resolve repo, create an isolated worktree, write the Codex prompt, run headless `codex exec`, detect changes, commit, push, open a PR, write attempt sidecars, append events, write recovery briefs, and clean up. |
| `ready-set.ps1` | Read GitHub issues, dependency labels, open PRs, PR checks, and local run state; classify ready and non-ready work; print text, JSON, or both. |
| `resume.ps1` | Reconcile a run GitHub-first, then local sidecars; classify done, adopt-PR, in-review, fixing, gated, running, stale, hung, orphaned, and ready recovery actions; print a read-mostly report. |
| `recover-and-retry.ps1` | Adopt an existing PR if present, enforce bounded retry, read the latest recovery brief, back off, and re-dispatch with attempt-specific branch names and recovery context. |
| `verify-local.ps1` | Create an isolated verification worktree, check out a PR or branch, read local command gates from `.delivery.yml`, run tests/typecheck/build commands, classify failures, and print a structured summary. |

The migration should not fragment those responsibilities across multiple
languages. The point is to make the helper layer easier to install and run on
every platform, not to add another compatibility layer around PowerShell.

## CLI Surface

The Go binary exposes subcommands that map 1:1 to the current helper scripts.
The command names are stable user-facing contracts after the migration.

| Go command | Current reference |
| --- | --- |
| `loopcoder dispatch` | `scripts/dispatch-worker.ps1` |
| `loopcoder ready-set` | `scripts/ready-set.ps1` |
| `loopcoder resume` | `scripts/resume.ps1` |
| `loopcoder recover` | `scripts/recover-and-retry.ps1` |
| `loopcoder verify-local` | `scripts/verify-local.ps1` |
| `loopcoder dispatch-wave` | The one-wave helper specified by [`orchestration.md`](0081-orchestration.md), implemented natively in Go. |

During migration, the binary should accept idiomatic long flags such as
`--repo` and compatibility aliases matching the PowerShell parameters, such as
`-Repo`, wherever doing so keeps `SKILL.md` and existing operator snippets
simple. Compatibility aliases are a migration convenience; the documented
post-parity form should be lowercase long flags.

### `loopcoder dispatch`

Reference:
`scripts/dispatch-worker.ps1`.

Target command shape:

```text
loopcoder dispatch \
  --repo . \
  --issue-number 89 \
  --issue-title "Design: Go native CLI migration" \
  --issue-body "<body>" \
  --base-branch main \
  --provider codex
```

Parameters must match the script's public behavior:

| Parameter | Required | Default | Meaning |
| --- | --- | --- | --- |
| `--repo` | Yes | none | Local repository path. |
| `--issue-number` | Yes | none | GitHub issue number. |
| `--issue-title` | Yes | none | Issue title used in prompt, commit, and PR title. |
| `--issue-body` | No | empty string | Issue body included in the worker prompt. |
| `--base-branch` | No | `main` | Base branch fetched from `origin`. |
| `--branch` | No | `loop/issue-<IssueNumber>` | Worker branch name. |
| `--run-id` | No | generated run id | Run namespace under `.loopcoder/runs/`. |
| `--attempt` | No | `1` | Attempt number. |
| `--recovery-context` | No | empty string | Recovery brief text appended to the prompt. |
| `--provider` | No | `codex` | Worker provider. v1 accepts `codex` only. |
| `--model` | No | unset | Passed to Codex as `-m <model>` only when set. |
| `--effort` | No | unset | Passed to Codex as `-c model_reasoning_effort=<effort>` only when set. |
| `--keep-worktree` | No | false | Preserve the scratch worktree and logs. |

On success, the command must print the same compact JSON shape currently
printed by `dispatch-worker.ps1`:

```json
{
  "ok": true,
  "issue": 89,
  "branch": "loop/issue-89",
  "run_id": "run-20260626T120000Z-issue-89",
  "pr": "https://github.com/owner/repo/pull/123",
  "summary": "Worker summary text",
  "attempt_path": "C:/repo/.loopcoder/runs/.../workers/job-89-1234.attempt.json",
  "status": "succeeded",
  "exit_code": 0,
  "log_bytes": 12345
}
```

The exact fields are part of the contract:
`ok`, `issue`, `branch`, `run_id`, `pr`, `summary`, `attempt_path`, `status`,
`exit_code`, and `log_bytes`.

### `loopcoder ready-set`

Reference: `scripts/ready-set.ps1`.

Target command shape:

```text
loopcoder ready-set --repo . --base-branch main --run-id <run-id> --format text
```

Parameters:

| Parameter | Required | Default | Meaning |
| --- | --- | --- | --- |
| `--repo` | Yes | none | Repository path. |
| `--base-branch` | No | `main` | Base branch used for dependency reasoning. |
| `--run-id` | No | latest local run when present | Run namespace to inspect. |
| `--format` | No | `text` | `text`, `json`, or `both`. |
| `--include-closed` | No | false | Diagnostic switch for closed issues. |

The JSON output must preserve the current shape:

- `version`
- `repo`
- `repo_path`
- `base_branch`
- `run_id`
- `generated_at`
- `ready[]`
- `blocked[]`
- `summary`

The ready/non-ready classifications must remain compatible with
[`orchestration.md`](0081-orchestration.md): `blocked-by-unmerged-dep`,
`has-open-PR`, `has-live-attempt`, `recovery-needed`, and diagnostic states
such as `closed` when `--include-closed` is supplied.

### `loopcoder resume`

Reference: `scripts/resume.ps1`.

Target command shape:

```text
loopcoder resume --repo . --run-id <run-id> --base-branch main
```

Parameters:

| Parameter | Required | Default | Meaning |
| --- | --- | --- | --- |
| `--repo` | Yes | none | Repository path. |
| `--run-id` | No | latest local run when present | Run namespace to inspect. |
| `--base-branch` | No | `main` | Base branch for branch and dependency reasoning. |

The text report must keep the same sections and semantics:

- `RESUME REPORT`
- repo, base branch, run id, generated time
- GitHub snapshot counts
- local state counts
- threshold summary
- issue classifications and evidence lines
- next ready actions
- blocked / awaiting human input
- safety note

Classifications such as `done`, `adopt-PR`, `in-review`, `fixing`, `gated`,
`running`, `stale`, `hung`, `orphaned`, `ready`, and `needs-inspection` must
keep their current meanings.

### `loopcoder recover`

Reference:
`scripts/recover-and-retry.ps1`.

Target command shape:

```text
loopcoder recover \
  --repo . \
  --issue-number 89 \
  --issue-title "..." \
  --issue-body "<body>" \
  --run-id <run-id>
```

Parameters:

| Parameter | Required | Default | Meaning |
| --- | --- | --- | --- |
| `--repo` | Yes | none | Repository path. |
| `--issue-number` | Yes | none | Issue number to recover. |
| `--issue-title` | Yes | none | Issue title used for retry dispatch. |
| `--issue-body` | No | empty string | Issue body used for retry dispatch. |
| `--run-id` | Yes | none | Run namespace containing attempt history. |
| `--base-branch` | No | `main` | Retry base branch. |
| `--max-attempts` | No | `3` | Bounded retry limit. |
| `--backoff-seconds` | No | `10,30,120` | Retry backoff schedule. |
| `--provider` | No | `codex` | Worker provider. |
| `--model` | No | unset | Optional pass-through model override. |
| `--effort` | No | unset | Optional pass-through reasoning effort override. |

The adoption and blocked outputs must preserve the current report shapes:

- `ADOPT EXISTING PR; NO RETRY`
- `BLOCKED: retry limit reached`
- `RETRY: dispatching issue #<N> attempt <M>`

The command must continue to adopt an existing issue PR before retrying.

### `loopcoder verify-local`

Reference: `scripts/verify-local.ps1`.

Target command shapes:

```text
loopcoder verify-local --repo . --pr-number 123 --base-branch main
loopcoder verify-local --repo . --branch loop/issue-89 --base-branch main
```

Parameters:

| Parameter | Required | Default | Meaning |
| --- | --- | --- | --- |
| `--repo` | Yes | none | Repository path. |
| `--pr-number` | Choice | none | Pull request to verify. Mutually exclusive with `--branch`. |
| `--branch` | Choice | none | Branch to verify. Mutually exclusive with `--pr-number`. |
| `--base-branch` | No | `main` | Base branch used for isolated checkout. |

The command must preserve the local command gate model from
[`verification.md`](0039-verification.md):

- read `ci.tests`, `ci.typecheck`, and `ci.build` from `.delivery.yml`;
- create an isolated worktree;
- check out the PR or branch;
- run configured commands from the worktree root;
- classify command failures as `pass`, `fail`, or `needs-human`;
- print `LOCAL VERIFICATION SUMMARY` followed by a `JSON SUMMARY`;
- exit `0` for pass, `1` for fail, and `2` for needs-human or infrastructure
  errors.

### `loopcoder dispatch-wave`

Reference: the one-wave helper specified by
[`orchestration.md`](0081-orchestration.md). The PowerShell design called this
helper `dispatch-ready-wave.ps1`; the Go CLI should expose it as the shorter
subcommand `dispatch-wave`.

Target command shapes:

```text
loopcoder dispatch-wave --repo . --base-branch main --issue-numbers 81,84
loopcoder ready-set --repo . --format json | loopcoder dispatch-wave --repo . --from-ready-set
loopcoder dispatch-wave --repo . --ready-set-path ready-set.json
```

Parameters:

| Parameter | Required | Default | Meaning |
| --- | --- | --- | --- |
| `--repo` | Yes | none | Repository path. |
| `--base-branch` | No | `main` | Base branch passed to dispatch. |
| `--run-id` | No | generated once per wave | Shared run id for every worker in the wave. |
| `--issue-numbers` | Choice | none | Explicit issue numbers to dispatch. |
| `--from-ready-set` | Choice | false | Read a ready-set JSON snapshot from stdin. |
| `--ready-set-path` | Choice | none | Read a ready-set JSON snapshot from a file. |
| `--provider` | No | `codex` | Worker provider pass-through. |
| `--model` | No | unset | Optional pass-through model override. |
| `--effort` | No | unset | Optional pass-through reasoning effort override. |
| `--throttle-limit` | No | implementation default | Local concurrency bound. |

This subcommand is native Go from the start. It should not shell out to a
PowerShell wave script. It dispatches exactly one ready layer, under one shared
run id, then stops. It must not merge, push directly, edit labels, or loop the
DAG. Push and PR creation remain inside the dispatch path, as they are today.

The state-branch and conductor-lease work described in
[`0041-resilience.md`](0041-resilience.md) is also implemented directly in Go when it
lands. It should not be introduced as another PowerShell shim.

## Unchanged Contracts

The Go binary must read and write the same formats that the PowerShell scripts
read and write. This section is the compatibility spec for the migration.

### `.delivery.yml`

The binary must parse the existing `version: 1` config and the existing keys:

- `adapters`
- `worker`
- `ci`
- `verification`
- `resilience`
- `report`

The current meanings remain unchanged:

- `adapters.work_items: github`
- `adapters.workspace: git-worktree`
- `adapters.worker: codex`
- `adapters.vcs: github`
- `adapters.verifier: opus`
- `adapters.gate: human-merge`
- `worker.base_branch`
- optional `worker.model`
- optional `worker.reasoning_effort`
- `ci.checks`
- optional `ci.tests`
- optional `ci.typecheck`
- optional `ci.build`
- optional `verification.spec_required`
- optional `verification.max_fix_passes`
- optional `verification.browser`
- optional `resilience.worker.heartbeat_interval_seconds`
- optional `resilience.worker.stale_after_seconds`
- optional `resilience.worker.hung_after_seconds`
- optional `resilience.worker.max_attempts`
- optional `resilience.worker.retry_backoff_seconds`

The Go parser may be stricter about malformed YAML, but it must remain tolerant
of absent optional sections and use the same defaults documented in
[`SKILL.md`](../../SKILL.md), [`0041-resilience.md`](0041-resilience.md), and
[`verification.md`](0039-verification.md).

The migration must not add required `.delivery.yml` keys. New optional keys
need their own doc-first design or a narrowly scoped amendment.

### Run State

The binary must preserve the local run layout:

```text
.loopcoder/
  runs/
    <RunId>/
      events.jsonl
      workers/
        <job-id>.attempt.json
      recovery/
        <job-id>-context.md
```

The attempt sidecar fields must remain compatible with records already written
by `dispatch-worker.ps1`:

```json
{
  "version": 1,
  "job_id": "job-89-1234",
  "issue": 89,
  "attempt": 1,
  "provider": "codex",
  "pid": 1234,
  "phase": "codex_started",
  "status": "running",
  "started_at": "2026-06-26T12:00:00Z",
  "heartbeat_at": "2026-06-26T12:01:00Z",
  "last_progress_at": "2026-06-26T12:01:00Z",
  "log_bytes": 1024,
  "exit_code": null,
  "error": null
}
```

Existing records may omit fields that later docs mention, such as `branch` or
`recovery_context_path`. The Go reader must preserve the PowerShell fallback
behavior: infer `loop/issue-<N>` for first attempts and
`loop/issue-<N>-retry-<M>` for retry attempts where needed.

`events.jsonl` remains append-only JSON lines with the current transition
fields:

- `ts`
- `run_id`
- `job_id`
- `issue`
- `phase`
- `status`
- `log_bytes`
- `exit_code`
- `error`

Recovery briefs remain Markdown files under `recovery/` with the same core
content:

- issue number and title;
- branch;
- worktree path;
- log path;
- summary path;
- attempt number;
- last phase;
- status;
- error;
- changed files;
- existing PR lookup;
- scrubbed log tail.

Secret scrubbing must be preserved. At minimum, the Go implementation must
scrub GitHub token patterns, OpenAI-style API keys, bearer tokens, and common
`token`, `password`, `secret`, and `api_key` assignments before writing log
tails into recovery briefs.

### Dispatch Output

`loopcoder dispatch` must print the same success JSON fields as
`dispatch-worker.ps1`:

- `ok`
- `issue`
- `branch`
- `run_id`
- `pr`
- `summary`
- `attempt_path`
- `status`
- `exit_code`
- `log_bytes`

Existing conductor code and future wave dispatch can depend on those field
names. A later version may add fields, but it must not remove or rename these
without a separate compatibility design.

### Ready-Set And Resume Reports

The `ready-set` JSON report shape from [`orchestration.md`](0081-orchestration.md)
is unchanged:

- `version`
- `repo`
- `repo_path`
- `base_branch`
- `run_id`
- `generated_at`
- `ready`
- `blocked`
- `summary`

The text report should remain close enough to the current `READY SET` output
that a conductor can read it without new instructions.

The `resume` report remains a human-readable reconciliation report. It is not
currently a machine JSON contract, but its classifications are a behavioral
contract. The Go command must not rename classifications casually.

### GitHub And Git Contracts

The Go binary still uses GitHub and git the way the current scripts do:

- GitHub issues, labels, PRs, branches, and checks are source of truth for
  delivery state.
- Local `.loopcoder` sidecars are advisory for liveness and recovery.
- A dependency represented by `blocked-by:#N` is satisfied only when the
  dependency has landed on the base branch, not merely when it has an open PR.
- Open PR attribution uses the same signals: closing issue references, branch
  names such as `loop/issue-<N>`, retry branch names, and safe issue references
  in titles.
- The human merge gate is unchanged. No helper may call `gh pr merge` as part
  of this migration.

The Go port should initially keep using the `gh` CLI rather than switching to
the GitHub API. Switching the VcsHost adapter from `gh` to direct API calls
would be a separate adapter change, not a mechanical Go migration.

## Cross-Platform Solutions

### Codex Execution

The current worker script uses `cmd /c` only because Windows shell redirection
was the simplest way to feed a prompt file to `codex exec` and close stdin:

```text
cmd /c "codex ... - < promptfile > codex.log 2>&1"
```

The Go implementation should not invoke a shell. It should use
`exec.Command` with explicit arguments and real file handles:

```go
prompt, err := os.Open(promptFile)
if err != nil {
    return err
}
defer prompt.Close()

logFile, err := os.Create(logPath)
if err != nil {
    return err
}
defer logFile.Close()

cmd := exec.CommandContext(ctx, "codex", args...)
cmd.Stdin = prompt
cmd.Stdout = logFile
cmd.Stderr = logFile

err = cmd.Run()
```

This is the portable version of the existing closed-stdin fix. A real file
handle is assigned to stdin; when Codex finishes reading the prompt, it sees
EOF rather than waiting for an interactive terminal. No `cmd.exe`, `bash`, or
`sh -c` is needed.

The argument list must preserve the current Codex invocation:

```text
codex exec
  --cd <worktree>
  --dangerously-bypass-approvals-and-sandbox
  --skip-git-repo-check
  [-m <model>]
  [-c model_reasoning_effort=<effort>]
  -o <summaryFile>
  -
```

Rules:

- `--dangerously-bypass-approvals-and-sandbox` stays because the worker is
  unattended and the worktree is the blast radius.
- `--skip-git-repo-check` stays because the fresh worktree is already created
  deliberately.
- `-m` is passed only when `--model` is explicitly set.
- `-c model_reasoning_effort=<effort>` is passed only when `--effort` is
  explicitly set.
- Arguments are passed as a slice, not shell-quoted strings.
- stdout and stderr are both written to the same log file, matching the current
  recovery and progress model.

### Worktree Add Lock

The current script serializes `git worktree add` with a Windows named mutex:

```text
Global\loopcoder-worktree-add-<repo-hash>
```

The Go implementation should replace that with a cross-platform file lock. The
lock should be keyed by the canonical repository path, normalized before
hashing so multiple loopcoder processes for the same repo agree on the same
lock. A future implementation may prefer the resolved git common directory
when available, but the public behavior is still "serialize worktree creation
for one repo."

The lock file path is not part of the public state contract. It can live under
the OS user cache directory, the OS temp directory, or a gitignored
`.loopcoder/locks/` directory. The important rules are:

- all loopcoder processes for the same repo acquire the same lock before
  running `git worktree add`;
- the lock is held only around the worktree creation step;
- Codex execution, commit, push, PR creation, verification, and report
  generation do not hold the worktree lock;
- lock acquisition has a bounded timeout and reports which repo key was being
  locked.

The recommended implementation is a small internal `lockfile` package using a
vetted cross-platform library such as `github.com/gofrs/flock`. The rest of the
code should depend on a local interface so the library can be replaced without
touching worker logic.

### Time, JSON, YAML, And Paths

Timestamps must be UTC RFC3339. The Go implementation should write times using
`time.Now().UTC()` and a stable RFC3339 format, and it must parse existing
PowerShell `ToString("o")` timestamps.

JSON should use Go's standard library. Struct tags should preserve exact field
names. Readers should tolerate absent optional fields where the current scripts
already infer defaults.

YAML should use a vetted library, initially `gopkg.in/yaml.v3`, behind an
internal `config` package. The binary only needs to read `.delivery.yml` in
this migration. If a future change needs comment-preserving writes, that should
be designed separately.

Path behavior must be explicit:

- command execution uses native absolute paths;
- public report fields that are already repo-relative should stay
  repo-relative;
- JSON fields that currently normalize separators to `/` should keep doing so;
- the binary must read existing Windows paths in sidecar files and must write
  paths that the local platform can use.

### Secret Scrubbing

Recovery briefs must remain safe to commit or paste into a worker prompt. The
Go port should implement a dedicated scrubber package and cover it with tests.

The scrubber should preserve the current behavior for:

- GitHub token prefixes such as `ghp_` and `github_pat_`;
- OpenAI-style `sk-...` values;
- bearer tokens;
- common `token`, `password`, `secret`, and `api_key` assignments.

The scrubber should be used before writing any log tail into
`.loopcoder/runs/<RunId>/recovery/<job-id>-context.md`.

## Concurrency

The Go migration should use goroutines for local concurrency, especially in
`dispatch-wave`. Concurrency is a resource-management concern, not a correctness
mechanism.

`dispatch-wave` should use an errgroup-style worker pool with a bounded
`--throttle-limit`. Each issue dispatch runs independently after the command
has selected a shared `run_id`. The only serialized part is `git worktree add`,
which is protected by the file lock described above.

Rules:

- independent ready issues may dispatch concurrently;
- all issues in one wave share one `run_id`;
- a failure in one issue does not roll back successful sibling PRs;
- every result is reported in the wave summary;
- cancellation should stop starting new workers but preserve already-written
  attempt state and recovery context where possible;
- dispatch concurrency must not bypass readiness checks, retry bounds, or the
  human merge gate.

This preserves [`scheduling.md`](0028-scheduling.md): real dependencies govern
dispatch readiness, and file overlap is handled later at merge time from real
PR diffs.

## Repo Layout And Build

The Go code should live at the repository root rather than in a nested tool
repo. The recommended layout is:

```text
go.mod
cmd/
  loopcoder/
    main.go
internal/
  cli/
  config/
  state/
  worker/
  vcs/
    github/
  gitutil/
  lockfile/
  report/
  recovery/
  verify/
  orchestration/
```

Package boundaries should follow the existing ports and helper responsibilities:

- `cmd/loopcoder`: process entrypoint and subcommand wiring.
- `internal/cli`: flag parsing, compatibility aliases, exit codes, and help
  text.
- `internal/config`: `.delivery.yml` parsing and defaults.
- `internal/state`: `.loopcoder/runs/<RunId>/` sidecars, events, timestamps,
  and run selection.
- `internal/worker`: dispatch flow, Codex invocation, prompt generation,
  attempt sidecars, cleanup, and recovery brief creation.
- `internal/vcs/github`: `gh` command wrappers for repo, issue, PR, checks, and
  PR creation.
- `internal/gitutil`: git command wrappers, worktree creation, branch checks,
  status, commit, push, and cleanup.
- `internal/lockfile`: cross-platform worktree-add lock.
- `internal/report`: ready-set, resume, and wave report models.
- `internal/recovery`: retry/adoption logic and secret scrubbing.
- `internal/verify`: local verification command gates.
- `internal/orchestration`: ready-set computation and one-wave dispatch.

The module path should be the GitHub repository path:

```text
module github.com/jasonhnd/loopcoder
```

The binary name is `loopcoder`. If a collision is discovered with an existing
tool in common package managers, the project can still keep the binary name for
repo-local use and publish packages with a qualified package name. The
conductor-facing command should remain `loopcoder`.

The CLI should start with the standard library where practical. A CLI framework
is not required for the first port. If compatibility aliases, nested help, or
shell completion become costly, the scaffold issue can choose a small CLI
library, but that choice should not leak into the behavioral contract.

### CI

The first Go scaffold PR should add a Go job to
`.github/workflows/ci.yml`. The job id should be `go`, and it should run:

```text
go test ./...
go vet ./...
go build ./cmd/loopcoder
```

The existing `verify` job should remain. It currently checks PowerShell parsing,
YAML validity, and the mapping between `.delivery.yml ci.checks` and workflow
job ids. The Go scaffold PR should update `.delivery.yml` and CI together so
the required checks stay truthful.

Target evolution:

1. Before Go code exists, `.delivery.yml` stays as it is with
   `ci.checks: [verify]`.
2. In the Go scaffold PR, add workflow job id `go`.
3. In the same PR, update `.delivery.yml` to require both jobs:

   ```yaml
   ci:
     checks: [verify, go]
   ```

4. The existing CI check mapping check then proves that every configured check
   maps to a real workflow job id.

This keeps docs truthful and avoids a window where `.delivery.yml` advertises a
required check that does not exist.

### Cross-Compile And Distribution

The binary should support at least:

- `darwin/arm64`
- `darwin/amd64`
- `linux/amd64`
- `linux/arm64`
- `windows/amd64`

The first distribution path should be `go install`:

```text
go install github.com/jasonhnd/loopcoder/cmd/loopcoder@latest
```

Prebuilt release binaries can follow after the binary is useful enough to
install outside a developer checkout. GoReleaser is a good fit for that later
step because it can build the matrix, attach checksums, and publish archives.
It should not block the first Go scaffold. The first scaffold only needs a
working module, local build, tests, and CI.

The exact minimum Go version should be selected in the scaffold PR after
checking the then-current supported Go releases. The constraint for this design
is that the project must use a supported Go release, record it in `go.mod`, and
keep the CI setup aligned with that version. Avoid pinning this design to a Go
version that may be stale by the time implementation starts.

## Strangler Migration Plan

The migration should be phased so every code PR has a narrow, testable purpose.
PowerShell remains the reference implementation until a Go subcommand proves
parity.

### Phase 1: Scaffold The Go Module

Add:

- `go.mod`;
- `cmd/loopcoder/`;
- internal package skeletons;
- root command and subcommand help;
- CI `go` job;
- tests for config defaults, timestamp parsing, and basic command wiring.

No behavior should switch in this phase. The PowerShell helpers remain the
operational path.

### Phase 2: Port Read-Only Helpers

Port `ready-set` and `resume` first because they are safer to validate:

- no dispatch;
- no push;
- no PR creation;
- no GitHub mutation;
- no worker process management.

Parity can be verified by running the PowerShell and Go commands against the
same repository state and diffing:

- ready-set JSON after normalizing timestamp values;
- ready-set text shape;
- resume classifications;
- resume evidence and next-action wording where it is contractually relevant.

These commands establish the Go config parser, GitHub reader, local state
reader, classifications, and report generation before the dispatch path is
ported.

### Phase 3: Port Dispatch

Port `dispatch` after the read-only commands are trustworthy. This is the core
cross-platform win because it removes both Windows-specific mechanics:

- replace `cmd /c` with `exec.Command` and file-handle stdin/stdout/stderr;
- replace the Windows named mutex with the cross-platform worktree lock.

Verification should include:

- unit tests for prompt generation and Codex arg construction;
- golden tests for attempt sidecar and event JSON;
- recovery-brief tests including secret scrubbing;
- an end-to-end dispatch of a real documentation issue in a test repository or
  the loopcoder repo, producing a real PR.

The end-to-end dispatch should prove that the Go binary can create the
worktree, feed Codex with closed stdin, commit, push, open a PR, write sidecar
state, and print the expected success JSON.

### Phase 4: Port Recovery, Local Verification, And Native Orchestration

Port:

- `recover`;
- `verify-local`;
- `dispatch-wave`;
- state-branch and conductor-lease mechanics from
  [`0041-resilience.md`](0041-resilience.md), when those are ready to leave target-design
  status.

`dispatch-wave` should be implemented directly in Go rather than copying the
PowerShell target helper shape. It is the point where goroutine concurrency and
the file lock pay off.

`verify-local` should preserve exit codes and failure classifications. It may
continue to run configured local commands through a shell where the command
itself is configured as a shell command, but that shell choice must be explicit
and cross-platform. For example, a future config may need to distinguish
portable argv commands from platform-specific shell snippets. The first port
should preserve current behavior and report `needs-human` when a configured
command cannot run on the current platform.

### Phase 5: Switch The Conductor Backend

After command parity is proven, update [`../SKILL.md`](../../SKILL.md) and usage
docs to call the binary:

```text
loopcoder ready-set --repo .
loopcoder dispatch-wave --repo .
loopcoder resume --repo .
loopcoder recover --repo .
loopcoder verify-local --repo . --pr-number <pr>
```

During the transition, the conductor selects the backend as follows:

1. If `LOOPCODER_BIN` is set, call that path.
2. Else if `loopcoder` is on `PATH`, call it.
3. Else if the session is on Windows and the PowerShell scripts are present,
   call the `.ps1` fallback.
4. Else report that the native binary is required.

After parity is proven across the command set, the playbook should reference
the binary as the normal path. The `.ps1` scripts can remain as a fallback for
one release window, then be deprecated in a separate doc-first PR. Removing
PowerShell scripts is not part of this design PR.

### Bootstrapping

loopcoder can self-host the migration. During the early phases, the existing
PowerShell `dispatch-worker.ps1` on Windows can dispatch Codex workers that
write the Go module and port each helper. That keeps the current delivery loop
usable while building its replacement.

Self-hosting does not weaken the doc-first rule. Each new behavior still needs
this merged design or a narrower follow-up design before code lands.

## SKILL.md Backend Selection

The conductor playbook should not need to understand Go internals. It should
select a backend, then call stable helper commands.

During migration:

- Prefer the Go binary when available.
- Fall back to PowerShell only for existing Windows self-hosting workflows.
- Do not require `pwsh` on macOS or Linux.
- Do not call both backends for one mutating operation.
- Keep model and effort flags omitted unless the user explicitly requested
  them.

Once parity lands, [`../SKILL.md`](../../SKILL.md) should describe `loopcoder` as
the helper interface. It can keep a short fallback note for Windows users on an
older checkout, but the examples should use the native binary.

The playbook should keep the same conceptual steps:

```text
ready-set
  -> dispatch one wave
  -> verify PRs
  -> human names merges
  -> resume/reconcile
  -> recover where needed
```

Only the mechanical command names change.

## Verification Strategy

The Go migration should be verified at three levels.

### Contract Tests

Use fixtures for:

- `.delivery.yml` parsing and defaults;
- attempt sidecar parsing, including missing optional fields;
- event JSON line writing;
- recovery brief generation and scrubbing;
- ready-set dependency classification;
- open PR attribution;
- resume liveness classifications;
- local verification verdict and exit-code mapping.

These tests should be deterministic and should not require GitHub credentials.

### Golden Parity Tests

For read-only helpers, capture PowerShell output from controlled fixtures or a
test repo and compare the Go output after normalizing volatile values:

- timestamps;
- absolute temp paths;
- process ids;
- check ordering where GitHub returns unordered data.

The goal is not byte-for-byte identity for volatile text. The goal is identical
observable meaning, field names, classification names, and report sections.

### Integration Drills

Before switching the playbook, run drills that mirror the existing docs:

- dispatch a real issue end-to-end;
- run `ready-set` with an unsatisfied `blocked-by:#N` dependency;
- run `ready-set` with an open PR for an issue;
- simulate failed dispatch and confirm recovery brief creation;
- run `recover` and confirm existing PR adoption wins before retry;
- run `verify-local` with no configured commands, a passing command, a failing
  command, and a missing tool;
- run concurrent `dispatch-wave` commands against one repo and confirm worktree
  creation is serialized.

## Risks

### Accidental Contract Drift

The largest migration risk is not Go itself. It is changing field names,
classification names, reports, branch conventions, or retry behavior while
porting. The mitigation is to keep the PowerShell scripts as reference
implementations until each Go command has parity coverage and a real run.

### Shell Behavior Differences

Removing `cmd /c` is intentional for Codex execution, but configured local
verification commands may currently assume PowerShell syntax. `verify-local`
must either preserve that behavior where PowerShell is available or clearly
classify unsupported platform-specific commands as `needs-human`. A later
config schema can distinguish portable argv commands from shell snippets.

### Lock Scope Bugs

If the file lock is keyed too narrowly, concurrent worktree creation can race.
If it is keyed too broadly, independent repos serialize unnecessarily. The
implementation should centralize lock key calculation and test normalization
for Windows and Unix-style paths.

### Installing A Binary Is Still Setup

The migration removes PowerShell as a dependency, but users still need `git`,
`gh`, `codex`, and the `loopcoder` binary. The install docs should make that
explicit and provide `go install` as the first path.

## Decisions

### Decision 1: One Native Binary Named `loopcoder`

Rationale: the conductor needs one stable helper command, and the project name
already describes the tool. Multiple helper binaries would recreate the script
sprawl in a different language.

Consequence: subcommands carry the behavior boundaries that scripts carry
today.

### Decision 2: Top-Level Go Module

Rationale: loopcoder is becoming a native tool, not embedding a one-off helper
inside `scripts/`. A top-level module makes `go install` and CI straightforward.

Consequence: the first scaffold PR adds root Go files and a Go CI job. That PR
must be separate from this design PR.

### Decision 3: Preserve `gh` As The GitHub Adapter

Rationale: the existing VcsHost contract is already expressed through `gh`.
Switching to direct GitHub API calls would change authentication, error
handling, pagination, and output behavior at the same time as the language
port.

Consequence: the Go binary shells out to `gh` for GitHub operations in the
first migration.

### Decision 4: Use Go File Handles For Codex Stdin And Logs

Rationale: this removes the Windows `cmd.exe` dependency while preserving the
closed-stdin behavior that prevents headless Codex from waiting forever for
interactive input.

Consequence: Codex args are built as argv values, not shell strings.

### Decision 5: Use A Cross-Platform File Lock For Worktree Creation

Rationale: `git worktree add` still needs serialization across loopcoder
processes for one repo. A file lock is the portable equivalent of the current
Windows named mutex.

Consequence: the lock is an implementation detail, but lock key calculation and
timeout behavior must be tested.

### Decision 6: Add A Dedicated Go CI Job And Require It

Rationale: Go build, vet, and tests are different evidence from the current
PowerShell parse and YAML checks. A separate job makes failures easier to
understand and lets `.delivery.yml ci.checks` name it directly.

Consequence: the scaffold PR adds workflow job id `go` and updates
`.delivery.yml ci.checks` to `[verify, go]` in the same change.

### Decision 7: `go install` First, Prebuilt Releases Later

Rationale: `go install` is enough for early contributors and avoids release
automation before the binary has proven parity. Prebuilt binaries matter later
for non-Go users.

Consequence: GoReleaser is a later distribution improvement, not part of the
first scaffold.

### Decision 8: Use A Supported Go Release, Chosen At Scaffold Time

Rationale: this document may merge before the code scaffold starts. Pinning an
exact Go version here risks choosing a stale release. The implementation still
needs a real minimum version, but it should be selected when `go.mod` and CI are
created.

Consequence: the scaffold PR sets the `go` directive to a then-supported Go
release and configures CI to use the same version. The minimum version is not
left implicit in code.

### Decision 9: Use `gopkg.in/yaml.v3` For Initial Config Reads

Rationale: the migration only needs to read `.delivery.yml`, and
`gopkg.in/yaml.v3` is a conventional Go YAML parser for structured config.
Comment-preserving writes are not required for this migration.

Consequence: YAML parsing is isolated behind `internal/config`, so a later
comment-preserving writer or stricter schema validator can be added without
rewriting command logic.

### Decision 10: Use `github.com/gofrs/flock` Behind An Internal Lock Package

Rationale: the migration needs one small cross-platform file lock for
`git worktree add`. Keeping the dependency behind `internal/lockfile` prevents
the rest of the codebase from coupling to one library.

Consequence: the scaffold PR should verify the library's current maintenance
and platform behavior, but the public design is a repo-keyed file lock with a
bounded timeout.

## Open Questions

- Exact minimum Go version: choose a supported Go release in the scaffold PR,
  record it in `go.mod`, and align CI with it.
- CLI flag compatibility depth: decide whether every PowerShell-style flag
  alias remains long term or only during the migration window.
- File lock validation: confirm `github.com/gofrs/flock` behavior on Windows,
  macOS, and Linux in CI or a manual scaffold drill.
- YAML write behavior: decide in a later config-editing design whether
  comment-preserving writes are needed. They are out of scope for this
  migration.
- Local verification shell semantics: decide whether configured commands are
  always shell snippets, portable argv lists, or a backward-compatible mix in a
  later schema. The first Go port should preserve current behavior as far as it
  can and report unsupported platform assumptions as `needs-human`.
- Release automation: decide when to add GoReleaser and prebuilt binaries
  after `go install` is working.
- Binary name collision: verify package-manager and PATH collision risk before
  publishing prebuilt binaries. The repo-local command should remain
  `loopcoder` unless a real collision is found.
- State branch timing: `0041-resilience.md` describes state branch plus conductor
  lease as target behavior. Decide whether that lands in the same phase as
  `dispatch-wave` or in a separate follow-up after local parity is complete.

## References

- [`architecture.md`](../reference/architecture.md): current v1 roles, ports, adapters, and
  single-session limits.
- [`scheduling.md`](0028-scheduling.md): dependency-aware dispatch, worktree
  isolation, file-overlap-at-merge, and worktree creation serialization.
- [`0041-resilience.md`](0041-resilience.md): durable run state, heartbeats, recovery
  briefs, resume, state branch, and conductor lease.
- [`verification.md`](0039-verification.md): hosted checks, local command gates,
  spec conformance, and explicit `pass` / `fail` / `needs-human` verdicts.
- [`orchestration.md`](0081-orchestration.md): ready-set computation and one-wave
  dispatch helper.
- [`PROCESS.md`](../PROCESS.md): doc-first workflow and one concern per PR.
- [`../DESIGN.md` section 8](../../DESIGN.md#8-state-model): GitHub-first state
  model and re-derive-on-tick direction.
- [`../DESIGN.md` section 12](../../DESIGN.md#12-local-v1-vs-cloud-v2): later
  local/cloud background conductor target, explicitly out of scope for this
  Scope A migration.
- [`../SKILL.md`](../../SKILL.md): conductor playbook that will select the Go
  backend after parity lands.
