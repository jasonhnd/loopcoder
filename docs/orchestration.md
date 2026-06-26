# loopcoder Orchestration Design

Status: DESIGN. This is a target design and is not yet built.

This document defines the executable orchestration layer for loopcoder's
foreground conductor: ready-set computation plus dispatch of one ready wave. It
is written doc-first per [`PROCESS.md`](PROCESS.md). The scripts described here
must be implemented in later code issues after this document merges.

The design intentionally preserves the v1 boundaries from
[`architecture.md`](architecture.md), [`scheduling.md`](scheduling.md),
[`resilience.md`](resilience.md), and [`verification.md`](verification.md): the
human remains the merge authority, file overlap is observed at merge time, and
workers still run through [`../scripts/dispatch-worker.ps1`](../scripts/dispatch-worker.ps1).

## Problem

Today the conductor manually performs two steps that are already specified in
[`scheduling.md`](scheduling.md):

1. Compute the ready set: which open issues can start because their real
   dependencies have landed, no open PR already represents the issue, and no
   live local attempt is still running.
2. Dispatch each ready issue by calling
   [`../scripts/dispatch-worker.ps1`](../scripts/dispatch-worker.ps1), usually
   one command at a time, then reconcile the run with
   [`../scripts/resume.ps1`](../scripts/resume.ps1).

That is workable for small batches, but it leaves the most mechanical conductor
work in chat memory and manual copy/paste. The model exists, but it is a human
playbook rather than an executable foreground loop.

The orchestration layer makes those two steps executable:

- `scripts/ready-set.ps1` computes readiness from GitHub plus local run state.
- A one-wave dispatch helper dispatches the current ready set under one shared
  `-RunId`.

It does not remove the human merge gate. It does not decide merge order before
real PR diffs exist. It only automates the conductor's current ready-set and
dispatch mechanics.

## Goals

- Add a read-only `scripts/ready-set.ps1` target that computes the ready set
  from GitHub issues, GitHub PRs, and local `.loopcoder/runs/<RunId>` state.
- Add a one-wave dispatch helper target that dispatches the current ready set
  under a shared `-RunId`.
- Compose cleanly with [`../scripts/resume.ps1`](../scripts/resume.ps1) for
  reconciliation, [`../scripts/recover-and-retry.ps1`](../scripts/recover-and-retry.ps1)
  for recovery, and
  [`../scripts/dispatch-worker.ps1`](../scripts/dispatch-worker.ps1) for the
  worker adapter.
- Preserve human-merge: no helper created by this layer may call `gh pr merge`
  or mark a PR merged.
- Preserve observe-at-merge: file overlap and merge ordering are handled only
  when the human names PRs for merge and the conductor reads their real changed
  files.
- Keep model, effort, and provider choices as pass-throughs. The orchestration
  layer must not invent model or effort settings.

## Non-Goals

- No auto-merge. A dispatched and verified PR can become merge-eligible, but it
  is still merged only after the human names it.
- No background daemon, cron job, scheduled tick, or unattended loop. That is
  the later target in [`../DESIGN.md` section 12](../DESIGN.md#12-local-v1-vs-cloud-v2).
- No change to the model or effort inheritance rule in
  [`../SKILL.md`](../SKILL.md). If the user did not explicitly request a model
  or effort override, the helper omits those flags.
- No change to the worktree mutex. Worktree creation remains serialized inside
  [`../scripts/dispatch-worker.ps1`](../scripts/dispatch-worker.ps1).
- No replacement for [`../scripts/resume.ps1`](../scripts/resume.ps1) or
  [`../scripts/recover-and-retry.ps1`](../scripts/recover-and-retry.ps1). The
  new scripts call into the same state model instead of creating a competing
  one.
- No new status-label workflow requirement. The layer can read labels such as
  `blocked-by:#N`, but it does not require new mutation labels to be correct.

## Design Principles

### GitHub First, Local State Second

Readiness is derived from GitHub first: issue state, dependency labels, open
PRs, PR head branches, closing issue references, and check status where useful.
Local `.loopcoder` state is advisory for local liveness and recovery. It can
block duplicate dispatch when a worker is still live, but it must not override a
GitHub PR or a completed GitHub issue.

This follows [`resilience.md`](resilience.md): GitHub is the source of truth for
delivery state, and local run state explains worker attempts.

### Execute Only The Next Layer

The conductor already schedules by layers. This orchestration layer should
execute exactly the next layer:

```text
current ready set -> one dispatch wave -> stop
```

It does not keep looping while workers finish. After the wave, the human and
conductor review PRs, apply the verification gate, merge only named PRs, run
reconciliation, and then invoke the ready-set computation again.

### No Merge-Time Guessing At Dispatch Time

File overlap is not a dispatch dependency in loopcoder. The orchestration layer
must not block ready issues because they might touch the same files. It may
report known risks, but it still treats file overlap as a merge-time concern.

Observe-at-merge remains the rule: when the human names PRs for merge, the
conductor reads actual file sets with `gh pr diff <pr> --name-only`, groups
overlapping PRs, rebases where needed, and evicts conflicts as described in
[`scheduling.md`](scheduling.md).

### Idempotent Enough For Re-Runs

Both target helpers should be safe to run repeatedly. A repeated ready-set run
should only re-read state. A repeated one-wave dispatch run should revalidate
each issue before calling the worker adapter so it does not duplicate work when
a PR or live attempt appeared after the ready-set snapshot was generated.

## Ready-Set Model (Executable)

`scripts/ready-set.ps1` is the executable version of "compute the ready set" in
[`scheduling.md`](scheduling.md), with the reconciliation vocabulary from
[`../scripts/resume.ps1`](../scripts/resume.ps1).

### Inputs

The ready-set computation reads:

- Open GitHub issues in the repository, including labels.
- `blocked-by:#N` labels on those issues.
- Dependency issue details for each referenced `#N`.
- Open GitHub PRs, including PR numbers, URLs, titles, head branches, draft
  state, closing issue references, and check summaries when available.
- Local attempt state under `.loopcoder/runs/<RunId>/workers/*.attempt.json`
  when a `-RunId` is supplied or a latest run is selected.
- The target base branch, defaulting to `main`.

The first implementation can use GitHub CLI calls like:

```text
gh issue list --state open --json number,title,labels,stateReason
gh issue view <n> --json number,title,state,stateReason,closedByPullRequestsReferences,labels
gh pr list --state open --json number,title,url,headRefName,isDraft,closingIssuesReferences
gh pr checks <pr> --json name,state,bucket,link,workflow
```

Those commands are implementation details, not a required public interface. The
contract is the state model and output shape below.

### Dependency Satisfaction

A `blocked-by:#N` label is a real code dependency. It is satisfied only when
GitHub shows the dependency as completed:

- the dependency issue is closed as completed, or
- a merged closing PR proves the dependency landed and GitHub associates that
  PR with the dependency issue.

An open dependency issue is not satisfied merely because a worker is running,
because a branch exists, or because an open PR exists. Downstream work starts
only after the dependency has landed on the base branch.

If a dependency issue cannot be read, the dependent issue is non-ready with
classification `blocked-by-unmerged-dep` and a reason that the dependency state
is unknown. Unknown dependency state must fail closed.

### Open PR Detection

An issue has an open PR when any open PR can be attributed to it by one of the
same signals used by `resume.ps1`:

- the PR's closing issue references include the issue number,
- the PR head branch matches the normal branch shape, such as
  `loop/issue-<N>` or a retry branch for that issue, or
- the PR title contains an issue reference that can be safely parsed.

When an open PR exists, the issue is not ready for a new implementation
dispatch. Its non-ready classification is `has-open-PR`.

The human-readable sub-state should mirror `resume.ps1`:

- `in-review` when the PR exists and checks are passing or not required,
- `fixing` when checks have failed or the verifier has requested a fix pass,
- `gated` when checks are pending, unreadable, or otherwise not yet a pass,
- `adopt-PR` when local state still says an attempt is running but GitHub
  already has the deliverable.

The exact sub-state is advisory. The important safety rule is that an open PR
blocks duplicate dispatch.

### Live Local Attempt Detection

An issue has a live local attempt when local `.loopcoder` sidecars show that a
worker attempt may still be active and no GitHub PR has superseded it.

The ready-set script should use the same signals as `resume.ps1`:

- latest `*.attempt.json` for the issue,
- `status`, `phase`, `heartbeat_at`, `last_progress_at`, and `pid`,
- configured stale and hung thresholds from `.delivery.yml` where present,
- same-host process liveness when a PID is available,
- candidate branches for the issue and attempts.

When the latest attempt is running, fresh, stale-but-not-yet-recoverable, or
hung with a live PID, the issue is not ready for ordinary dispatch. Its
classification is `has-live-attempt`, with a reason such as "running",
"progress stale", or "hung but pid still alive".

When the latest attempt failed, exited idle, became orphaned, or is hung with no
live process, the conductor should route through
[`../scripts/recover-and-retry.ps1`](../scripts/recover-and-retry.ps1) rather
than silently treating the issue as a clean first dispatch. `ready-set.ps1`
should surface that as a non-ready recovery reason unless the implementation
explicitly supports a separate `ready_for_recovery` bucket. Either way, the
one-wave dispatch helper should not start an ordinary duplicate worker over a
recoverable local attempt without conductor review.

### Readiness Rule

An issue is `READY` when all of the following are true:

1. It is an open issue in the candidate set.
2. Every `blocked-by:#N` dependency is completed on GitHub.
3. No open PR is attributable to the issue.
4. No live local attempt is attributable to the issue.
5. No recovery-required local attempt needs adoption or retry handling first.

Everything else is non-ready and must include a short reason.

The primary non-ready classifications are:

| Classification | Meaning | Next conductor action |
| --- | --- | --- |
| `blocked-by-unmerged-dep` | At least one `blocked-by:#N` dependency is not completed or cannot be read. | Wait for the dependency to merge or inspect the missing dependency. |
| `has-open-PR` | GitHub already has an open PR for the issue. | Verify, fix, gate, or wait for human merge. Do not dispatch a duplicate worker. |
| `has-live-attempt` | Local run state shows a worker attempt may still be active. | Wait, inspect, or run recovery when it becomes recoverable. |
| `recovery-needed` | A failed, idle, orphaned, or non-live attempt needs bounded recovery. | Use `recover-and-retry.ps1` with the same `-RunId`. |

The required machine output must make it easy for the dispatch helper to select
only `READY` issues and for the human to understand every exclusion.

## One-Wave Dispatch

The dispatch helper is the executable version of "dispatch the whole ready set"
in [`scheduling.md`](scheduling.md). Its target name is
`scripts/dispatch-ready-wave.ps1`.

### Inputs

The helper accepts either:

- an explicit list of issue numbers, or
- a ready-set snapshot produced by `scripts/ready-set.ps1`.

It also accepts:

- `-Repo`, defaulting to the current repository when safe,
- `-BaseBranch`, defaulting to `main`,
- optional `-RunId`,
- optional provider/model/effort pass-throughs,
- optional concurrency bounds such as `-ThrottleLimit`, if implementation needs
  them for local resource control.

If `-RunId` is absent, the helper generates one stable run id for the wave and
passes that same value to every worker dispatch. A suitable target shape is:

```text
run-<utc-compact>-wave
```

The run id must be generated once per helper invocation, not once per issue.
All worker attempts in the same wave then share:

```text
.loopcoder/runs/<RunId>/
```

### Behavior

For each selected issue, the helper:

1. Revalidates that the issue is still ready, or that the ready-set snapshot is
   fresh enough and no open PR or live attempt appeared since it was produced.
2. Reads the issue title and body from GitHub.
3. Calls [`../scripts/dispatch-worker.ps1`](../scripts/dispatch-worker.ps1)
   with the shared `-RunId`, issue fields, base branch, and only the explicit
   provider/model/effort flags the human or caller supplied.
4. Captures the worker result, including PR URL, branch, attempt path, status,
   exit code, and failure text when available.
5. Prints a wave summary.

The helper may dispatch concurrently because
[`../scripts/dispatch-worker.ps1`](../scripts/dispatch-worker.ps1) already owns
the git worktree creation mutex. The concurrency boundary is therefore local
resource management, not correctness. Serial dispatch remains an acceptable
implementation fallback, but it should not change scheduling semantics.

### Bounds

The helper performs exactly one wave per invocation. It never waits for the
whole DAG to drain, never recomputes newly ready dependents after merges, and
never loops on its own.

After the wave:

1. Workers open PRs or fail.
2. The conductor verifies PRs according to [`verification.md`](verification.md).
3. The human names PRs to merge.
4. The conductor applies observe-at-merge ordering from
   [`scheduling.md`](scheduling.md).
5. The conductor runs `resume.ps1` to reconcile.
6. The conductor runs `ready-set.ps1` again for the next layer.

This bounded shape is the main difference between this foreground orchestration
layer and the later background conductor tick.

## The Foreground Loop

The composed foreground cycle is:

```text
ready-set.ps1
  -> dispatch one wave
  -> human verifies + merges (gate + observe-at-merge)
  -> resume.ps1 reconcile
  -> repeat until the ready set is empty
```

Expanded:

1. `ready-set.ps1` reads GitHub and local run state, then reports ready and
   non-ready issues.
2. `dispatch-ready-wave.ps1` dispatches only the current ready set under one
   shared `-RunId`.
3. Workers create branches and PRs through `dispatch-worker.ps1`.
4. The conductor verifies each PR. A passing gate means merge-eligible, not
   merged.
5. The human names PRs to merge.
6. The conductor observes real PR file sets at merge time, orders overlapping
   PRs, rebases where needed, and evicts conflicts.
7. `resume.ps1` reconciles GitHub and local state.
8. The conductor repeats the cycle if more issues are ready.

This is the human-gated, foreground precursor to the stateless/background
conductor described in [`../DESIGN.md` section 12](../DESIGN.md#12-local-v1-vs-cloud-v2).
It borrows the "re-derive on each tick" state shape from
[`../DESIGN.md` section 8](../DESIGN.md#8-state-model), but the tick is a human
invocation rather than a daemon.

## Interfaces

### `scripts/ready-set.ps1`

Target command shape:

```powershell
pwsh scripts/ready-set.ps1 `
  -Repo . `
  -BaseBranch main `
  -RunId <run-id> `
  -Format text
```

Target parameters:

| Parameter | Required | Meaning |
| --- | --- | --- |
| `-Repo` | Yes | Repository path. Resolved before running GitHub and git commands. |
| `-BaseBranch` | No | Base branch used for dependency and branch reasoning. Defaults to `main`. |
| `-RunId` | No | Local run to inspect under `.loopcoder/runs/<RunId>`. If omitted, select the latest local run when present, matching `resume.ps1`. |
| `-Format` | No | `text`, `json`, or `both`. Text is human-readable. JSON is machine-readable for the dispatch helper. |
| `-IncludeClosed` | No | Optional diagnostic switch for implementation debugging. Normal ready-set output should focus on open issues. |

`ready-set.ps1` is read-only:

- no GitHub mutations,
- no dispatch,
- no push,
- no merge,
- no local state mutation beyond unavoidable command caches.

Target JSON shape:

```json
{
  "version": 1,
  "repo": "owner/name",
  "repo_path": "C:/path/to/repo",
  "base_branch": "main",
  "run_id": "run-20260626T120000Z-wave",
  "generated_at": "2026-06-26T12:00:00Z",
  "ready": [
    {
      "issue": 81,
      "title": "Design: orchestration layer",
      "reason": "dependencies completed; no open PR; no live local attempt"
    }
  ],
  "blocked": [
    {
      "issue": 82,
      "title": "Implement ready-set",
      "classification": "blocked-by-unmerged-dep",
      "reason": "blocked by #81, which is still open",
      "dependencies": [81],
      "open_prs": [],
      "attempts": []
    },
    {
      "issue": 83,
      "title": "Implement dispatch wave helper",
      "classification": "has-open-PR",
      "reason": "open PR #90 exists for loop/issue-83",
      "dependencies": [],
      "open_prs": [
        {
          "number": 90,
          "url": "https://github.com/owner/repo/pull/90",
          "head": "loop/issue-83",
          "sub_state": "gated"
        }
      ],
      "attempts": []
    }
  ],
  "summary": {
    "ready_count": 1,
    "blocked_count": 2,
    "blocked_by_unmerged_dep_count": 1,
    "has_open_pr_count": 1,
    "has_live_attempt_count": 0,
    "recovery_needed_count": 0
  }
}
```

Target text shape:

```text
READY SET
Repo: owner/name
Base branch: main
RunId: run-20260626T120000Z-wave
Generated at: 2026-06-26T12:00:00Z

Ready
- #81 Design: orchestration layer
  reason: dependencies completed; no open PR; no live local attempt

Non-ready
- #82 Implement ready-set
  classification: blocked-by-unmerged-dep
  reason: blocked by #81, which is still open
- #83 Implement dispatch wave helper
  classification: has-open-PR
  reason: open PR #90 exists for loop/issue-83

Safety
- ready-set is read-only: no dispatch, no merge, no push, and no GitHub mutation was attempted.
```

The text form should be close enough to `resume.ps1` that a conductor can read
both reports without translating vocabulary.

### `scripts/dispatch-ready-wave.ps1`

Target command shapes:

```powershell
pwsh scripts/dispatch-ready-wave.ps1 `
  -Repo . `
  -BaseBranch main `
  -IssueNumbers 81,84 `
  -RunId run-20260626T120000Z-wave
```

```powershell
pwsh scripts/ready-set.ps1 -Repo . -RunId run-20260626T120000Z-wave -Format json |
  pwsh scripts/dispatch-ready-wave.ps1 -Repo . -FromReadySet -RunId run-20260626T120000Z-wave
```

Target parameters:

| Parameter | Required | Meaning |
| --- | --- | --- |
| `-Repo` | Yes | Repository path. |
| `-BaseBranch` | No | Base branch passed to worker dispatch. Defaults to `main`. |
| `-RunId` | No | Shared run id for every worker in the wave. Generated once if absent. |
| `-IssueNumbers` | Choice | Explicit issue numbers to dispatch. Mutually exclusive with `-FromReadySet`. |
| `-FromReadySet` | Choice | Read the machine output from `ready-set.ps1` via pipeline or file. Dispatch only entries in `ready`. |
| `-ReadySetPath` | No | Optional path to a JSON ready-set snapshot. |
| `-Provider` | No | Pass-through to `dispatch-worker.ps1` only when explicitly supplied or configured. |
| `-Model` | No | Pass-through to `dispatch-worker.ps1` only when explicitly supplied by the human. |
| `-Effort` | No | Pass-through to `dispatch-worker.ps1` only when explicitly supplied by the human. |
| `-ThrottleLimit` | No | Optional local concurrency bound. It does not change readiness semantics. |

The helper's only delivery side effect is calling
[`../scripts/dispatch-worker.ps1`](../scripts/dispatch-worker.ps1). It may read
GitHub, read local state, and write its own console summary, but it must not
merge, push directly, edit issues, or mutate labels. Pushes and PR creation
happen inside `dispatch-worker.ps1`, which already owns that adapter boundary.

Target wave summary shape:

```text
DISPATCH WAVE
Repo: owner/name
Base branch: main
RunId: run-20260626T120000Z-wave
Issues requested: #81, #84
Issues dispatched: 2
Issues skipped: 0
Started at: 2026-06-26T12:00:00Z
Finished at: 2026-06-26T12:18:00Z

Results
- #81 succeeded
  branch: loop/issue-81
  pr: https://github.com/owner/repo/pull/91
  attempt: .loopcoder/runs/run-20260626T120000Z-wave/workers/job-81-1234.attempt.json
- #84 failed
  branch: loop/issue-84
  error: codex exec failed (exit 1)
  recovery: .loopcoder/runs/run-20260626T120000Z-wave/recovery/job-84-5678-context.md

Next
- Verify successful PRs before calling them merge-eligible.
- Use recover-and-retry.ps1 for failed attempts.
- Run resume.ps1 after human review/merge or interruption.
```

Machine output should contain the same fields:

- `run_id`,
- `issue`,
- `status` (`succeeded`, `failed`, `skipped`),
- `branch`,
- `pr`,
- `attempt_path`,
- `recovery_context_path`,
- `error`,
- timestamps.

## Relationship To Existing Docs

### `scheduling.md`

[`scheduling.md`](scheduling.md) defines the two ordering axes:

- real code dependencies govern dispatch readiness,
- file overlap governs merge ordering only after PR diffs exist.

This orchestration layer executes the first half of that model. It computes the
layered ready set and dispatches one layer. It does not change conflict
eviction, merge grouping, or observe-at-merge ordering.

### `resilience.md`

[`resilience.md`](resilience.md) defines durable local run state, worker
heartbeats, recovery briefs, reconciliation, and bounded retry. The ready-set
script reuses that vocabulary instead of creating a second state language.

The shared `-RunId` is the main bridge: every dispatch in a wave writes attempt
state under one run directory, and `resume.ps1` plus `recover-and-retry.ps1`
can reason about the wave after interruption or failure.

### `verification.md`

[`verification.md`](verification.md) is unchanged. The orchestration layer can
create PRs faster, but it cannot call them merge-eligible. The verifier still
checks required GitHub checks, local evidence when configured, and conformance
to the merged design document. The gate still reports `pass`, `fail`, or
`needs-human`.

### `architecture.md`

[`architecture.md`](architecture.md) says v1 is an Opus conductor session plus
a thin PowerShell worker adapter, not a daemon or cloud service. This design
keeps that architecture. The new scripts are conductor helpers in the foreground
session, and `dispatch-worker.ps1` remains the Worker adapter.

### `PROCESS.md`

[`PROCESS.md`](PROCESS.md) requires doc-first, one concern per PR, and separate
documentation and code changes. This file is the design document for the
orchestration layer. The implementation scripts must land in later code PRs.

### `SKILL.md`

[`../SKILL.md`](../SKILL.md) is the conductor playbook. After this design and
its implementation merge, the playbook should call:

1. `ready-set.ps1` before starting or resuming a dispatch layer.
2. `dispatch-ready-wave.ps1` to run the foreground wave.
3. `resume.ps1` after interruption, human review, or merges.
4. `recover-and-retry.ps1` for failed, hung, idle, or orphaned attempts.

The playbook should continue to state the human-merge rule explicitly.

## Failure Handling

### Partial Wave Failures

A wave can partially succeed:

- some issues open PRs,
- some issues fail before committing,
- some fail while pushing or opening PRs,
- some are skipped because a preflight recheck found an open PR or live attempt.

The helper must not roll back successful dispatches because a sibling issue
failed. Successful PRs proceed to verification. Failed issues are reported with
their attempt state and recovery brief, then routed through
`recover-and-retry.ps1` when retry is appropriate.

The wave summary is the handoff surface. It should make the next action obvious
for each issue:

- verify PR,
- recover and retry,
- inspect manually,
- wait for dependency,
- wait for live attempt.

### Snapshot Races

Ready-set snapshots can become stale. A human may merge a dependency, a worker
may open a PR, or another conductor session may start a worker between
`ready-set.ps1` and `dispatch-ready-wave.ps1`.

The dispatch helper therefore must perform a final preflight for each issue. If
the issue no longer satisfies the readiness rule, it is skipped and reported
rather than dispatched. This makes repeated or delayed invocations safe.

### Recovery

Recovery remains a separate step because it needs context and bounds, not a
blind duplicate dispatch.

When a wave failure produces a recovery brief under:

```text
.loopcoder/runs/<RunId>/recovery/<job_id>-context.md
```

the conductor should call:

```powershell
pwsh scripts/recover-and-retry.ps1 `
  -Repo . `
  -IssueNumber <n> `
  -IssueTitle "<title>" `
  -IssueBody "<body>" `
  -RunId <same-run-id>
```

`recover-and-retry.ps1` first adopts an existing PR when one exists. Otherwise
it retries with bounded attempts, backoff, and the latest recovery brief. That
behavior is deliberately outside ordinary ready dispatch.

### Verification Failures

If a dispatched PR fails verification, the issue has an open PR and is not
ready for a new implementation worker. The next action is a fix pass using the
verification evidence, not a new ready-wave dispatch.

The ready-set output should classify this as `has-open-PR` with sub-state
`fixing` or `gated`, depending on the evidence available from checks and the
conductor's verifier notes.

### Conflict Eviction

Conflict eviction remains a merge-time recovery path. The one-wave dispatch
helper must not predict or preempt file overlap. If an overlapping PR cannot be
rebased after another named PR lands, the conductor captures the conflict
context and re-dispatches a narrowed worker as described in
[`scheduling.md`](scheduling.md) and [`resilience.md`](resilience.md).

## Decisions

### Decision 1: Ready-Set Is A Read-Only Script

Rationale: readiness must be inspectable and safe. A conductor should be able
to run the command before deciding whether to dispatch anything.

Consequence: all mutations stay in explicit follow-up commands. The ready-set
script reports facts and recommendations only.

### Decision 2: One Wave Per Invocation

Rationale: v1 is foreground and human-gated. New readiness often depends on
human verification and merge decisions, not merely worker completion.

Consequence: the helper stops after dispatching the current ready set. The
human and conductor decide when to run the next wave.

### Decision 3: Shared `-RunId` Per Wave

Rationale: `resume.ps1`, `recover-and-retry.ps1`, and local sidecars need one
run namespace that groups sibling attempts.

Consequence: the dispatch helper generates or accepts a run id once and passes
it to every `dispatch-worker.ps1` call in the wave.

### Decision 4: Dispatch Uses The Existing Worker Adapter

Rationale: `dispatch-worker.ps1` already owns worktree creation, prompt file
construction, Codex execution, commit, push, PR creation, sidecars, recovery
briefs, and the worktree mutex.

Consequence: the orchestration helper does not recreate worker mechanics. It
coordinates calls to the adapter and summarizes their outcomes.

### Decision 5: Model And Effort Are Pass-Through Only

Rationale: [`../SKILL.md`](../SKILL.md) says defaults inherit from the user's
Codex configuration unless the user explicitly requests overrides.

Consequence: the helper never chooses model or effort. If the caller does not
provide `-Model` or `-Effort`, those flags are omitted.

### Decision 6: Observe-At-Merge Is Preserved

Rationale: predicted file overlap is unreliable. The real changed file set
exists only after workers open PRs.

Consequence: dispatch remains dependency-driven. Merge ordering remains
PR-diff-driven and human-named.

## Open Questions

- What final script name should the project prefer for the one-wave helper:
  `dispatch-ready-wave.ps1`, `dispatch-wave.ps1`, or another shorter name?
  This document uses `dispatch-ready-wave.ps1` because it names the scheduling
  concept directly.
- Should `ready-set.ps1` support issue filters, such as `-Label delivery:unit`
  or `-IssueNumbers`, or should v1 inspect every open issue in the repository?
  The core readiness rule works either way.
- Should ready-set snapshots be accepted only from stdin/file JSON, or should
  the dispatch helper always recompute readiness internally and treat snapshots
  as display-only? Recomputing is safer; snapshots are more auditable.
- What concurrency default is appropriate for local machines? The correctness
  model allows parallel dispatch because of worktree isolation and the mutex,
  but local CPU, memory, and provider limits may justify a conservative
  `-ThrottleLimit`.
- Should recovery-needed issues appear in the primary `blocked` list only, or
  should the JSON output include a separate `recovery` array to make conductor
  routing easier?

These questions do not change the core design. The layer is still a foreground,
one-wave executor that reads GitHub plus local state, dispatches only currently
ready issues, and stops before verification and human merge.

## Compatibility With The Background Tick

[`../DESIGN.md` section 8](../DESIGN.md#8-state-model) defines the north-star
state model as GitHub-first and re-derived on each conductor tick. Section 12
moves the conductor to a later local/cloud background runtime.

This foreground orchestration layer is intentionally shaped like that future
tick without becoming that tick:

```text
read GitHub + local run state
compute ready set
dispatch one ready wave
write worker attempt state through the worker adapter
stop for human verification and merge
```

A later background conductor can reuse the ready-set computation and dispatch
interfaces, then add scheduling, leases, notifications, and policy around them.
Because v1 keeps ready-set read-only and dispatch bounded, the future tick can
compose these pieces without inheriting an accidental auto-merge loop.

## References

- [`scheduling.md`](scheduling.md): layered ready-set scheduling, the two
  ordering axes, observe-at-merge, and conflict eviction.
- [`resilience.md`](resilience.md): run state, heartbeat, recovery,
  reconciliation, and bounded retry.
- [`verification.md`](verification.md): evidence-backed verification and the
  human merge gate.
- [`architecture.md`](architecture.md): v1 roles, ports, adapters, and
  single-session limits.
- [`PROCESS.md`](PROCESS.md): doc-first workflow and one concern per PR.
- [`../SKILL.md`](../SKILL.md): conductor playbook that should call these
  helpers after implementation.
- [`../DESIGN.md` section 8](../DESIGN.md#8-state-model): GitHub-first state
  model.
- [`../DESIGN.md` section 12](../DESIGN.md#12-local-v1-vs-cloud-v2): later
  local/cloud background conductor target.
