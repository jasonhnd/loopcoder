---
id: 41
title: loopcoder Resilience Design
status: accepted
date: 2026-06-26
issue: 41
pr: null
supersedes: []
superseded_by: []
---

# loopcoder Resilience Design

Status: DESIGN. This is a target design and is not yet built.

This document extends the v1 loop described in `docs/reference/architecture.md`,
`docs/specs/0028-scheduling.md`, `docs/reference/worker.md`, `SKILL.md`, and
`loopcoder dispatch`. It is intentionally written as a bridge between
the current single-session implementation and the stateless/background conductor
described in `DESIGN.md` sections 8 and 12.

## Problem

loopcoder v1 is deliberately small and session-bound. The Opus chat session is
the Conductor, the in-chat table is part of the live state machine, and
`loopcoder dispatch` launches one Codex worker per issue in a fresh git
worktree. This is enough for small batches, but it creates two resilience gaps.

First, conductor state is fragile. The durable systems today are GitHub issues,
labels, PRs, branches, and checks. The detailed dependency DAG, dispatch
attempts, worker handles, in-flight progress, verifier notes, and retry context
live mainly in the conductor session. If that session ends mid-run, the workers
may keep running, but a new session cannot reliably tell which local process,
scratch worktree, prompt file, log file, branch, or eventual PR belongs to which
issue. Those workers are effectively orphaned.

Second, loopcoder has no stuck-worker detection. A worker can hang, block on
stdin, wait on a tool, enter an idle state, run out of context, or stop producing
observable output without the conductor knowing. The current worker adapter
already avoids one specific failure mode by feeding `codex exec` from a prompt
file with closed stdin. That fix matters because we have already seen Codex hang
on stdin for roughly eight minutes. But avoiding one known hang is not a general
liveness model. The conductor needs a heartbeat and timeout contract for every
worker attempt.

The target resilience layer makes worker progress observable, records conductor
state outside the chat, and lets a fresh conductor session re-derive and resume
the run from GitHub plus state files committed to git.

## Design Goals

- Detect workers that make no observable progress within a configurable timeout.
- Classify stuck, failed, idle, context-limited, orphaned, and completed worker
  attempts consistently.
- Retry or re-dispatch work with captured context instead of blindly rerunning
  the original prompt.
- Preserve enough state in files and git that a new conductor session can resume
  after chat loss, terminal loss, host reboot, or context exhaustion.
- Reuse the scheduling model in `docs/specs/0028-scheduling.md`: real dependencies govern
  dispatch order, file overlap is handled at merge time, and conflict eviction
  narrows and re-dispatches work with concrete conflict context.
- Keep GitHub issues, labels, PRs, branches, and checks as the source of truth
  for externally visible delivery state, matching `DESIGN.md` section 8.
- Make the v1 chat conductor behave like a foreground implementation of the
  later v2 background conductor from `DESIGN.md` section 12: each tick can start
  from a fresh context and re-derive the next action.

## Non-Goals

- This document does not add code. It defines the contract that later code
  issues should implement.
- It does not change the human merge gate. loopcoder still never auto-merges in
  the current v1 line.
- It does not introduce a database requirement. SQLite or another store can be
  added later for querying and metrics, but correctness must not depend on it.
- It does not make worker internals trusted. A worker may update files, logs, or
  summaries, but the conductor relies on observable process, filesystem, git,
  and GitHub state.

## Reference Patterns

This design is informed by three external systems and by loopcoder's own
north-star design.

- `claude_code_agent_farm` uses heartbeat monitoring to detect agents with stale
  pulses after more than two minutes, and its auto-restart mode restarts agents
  that hit errors, go idle, or have stale heartbeats. It also tracks context
  pressure, adaptive idle timeouts, restart counts, and exponential backoff.
  loopcoder should borrow the heartbeat/restart shape, not the tmux-specific
  implementation.
- Steve Yegge's Gas Town uses Beads as a git-backed data plane for work,
  identity, hooks, and workflows. The useful lesson for loopcoder is not the
  naming or the full orchestration stack; it is the crash-survival property:
  work must be represented outside any one agent session, in a durable git-backed
  ledger that another session can pick up.
- `snarktank/ralph` runs each iteration with fresh context and persists
  continuity through git history, `progress.txt`, and `prd.json`. loopcoder
  should follow the same shape for conductor ticks: fresh context is safe only
  when the state needed for the next iteration is in files and git.
- `DESIGN.md` section 8 defines the north-star state model as GitHub-first and
  re-derived on every conductor tick. Section 12 says v2 swaps the local runtime
  for a cloud/background conductor while keeping the same `.delivery.yml` and
  labels. The resilience layer is the practical intermediate step that makes
  this swap possible.

References:

- <https://github.com/Dicklesworthstone/claude_code_agent_farm>
- <https://steve-yegge.medium.com/welcome-to-gas-town-4f25ee16dd04>
- <https://github.com/snarktank/ralph>
- `DESIGN.md` sections 8 and 12

## Architecture Overview

The resilience layer adds four concepts around the current loop:

1. Durable run state: a small set of files records the issue DAG, attempts,
   worker leases, PR mappings, verifier status, and recovery decisions.
2. Worker heartbeat: every worker attempt has a sidecar heartbeat file updated
   by the worker adapter or an adapter-owned supervisor process.
3. Reconciliation tick: the conductor periodically re-reads GitHub, local state,
   heartbeat files, worktrees, branches, and PRs, then computes the next action.
4. Recovery dispatch: stuck or failed attempts are terminated, captured, and
   retried or re-dispatched with explicit context and bounded retry policy.

The current chat conductor can execute these steps manually or by helper
scripts. The important design rule is that the chat is no longer the only state
machine. The chat becomes a user surface and a conductor execution environment;
the authoritative replay material lives in GitHub plus files committed to git.

## Durable State Model

loopcoder should store run state under a repo-local state directory, for
example:

```text
.loopcoder/
  runs/
    <run-id>/
      state.json
      events.jsonl
      workers/
        <job-id>.heartbeat.json
        <job-id>.attempt.json
      recovery/
        <job-id>-context.md
```

The exact path can become configurable in `.delivery.yml`, but the schema should
not depend on the conductor session. `state.json` is the compact current
snapshot. `events.jsonl` is append-only and records state transitions. The
snapshot can be regenerated from events plus GitHub state if needed.

The minimum `state.json` model is:

```json
{
  "version": 1,
  "run_id": "2026-06-26T12-00-00Z-issue-batch",
  "repo": "owner/name",
  "base_branch": "main",
  "created_at": "2026-06-26T12:00:00Z",
  "updated_at": "2026-06-26T12:15:00Z",
  "conductor": {
    "lease_id": "host-pid-random",
    "lease_expires_at": "2026-06-26T12:20:00Z",
    "last_tick_at": "2026-06-26T12:15:00Z"
  },
  "items": {
    "41": {
      "issue": 41,
      "title": "Design: resilience (docs/specs/0041-resilience.md)",
      "depends_on": [],
      "status": "implementing",
      "labels": ["delivery:unit", "status:implementing"],
      "attempts": ["job-41-1"],
      "active_attempt": "job-41-1",
      "pr": null,
      "verifier": null
    }
  },
  "attempts": {
    "job-41-1": {
      "issue": 41,
      "attempt": 1,
      "branch": "loop/issue-41",
      "worktree": "C:/Users/owner/AppData/Local/Temp/loopcoder-abcd1234/wt",
      "provider": "codex",
      "status": "running",
      "started_at": "2026-06-26T12:01:00Z",
      "last_progress_at": "2026-06-26T12:14:30Z",
      "last_progress_kind": "log_advanced",
      "heartbeat_path": ".loopcoder/runs/.../workers/job-41-1.heartbeat.json",
      "log_path": "C:/Users/owner/AppData/Local/Temp/loopcoder-abcd1234/codex.log",
      "summary_path": "C:/Users/owner/AppData/Local/Temp/loopcoder-abcd1234/summary.txt",
      "retry_of": null,
      "retry_count": 0
    }
  }
}
```

State should be committed to a dedicated state branch, not to feature branches
or `main`, unless a repo explicitly opts into visible state files. A branch such
as `loopcoder/state` avoids polluting delivery PRs while still giving crash
survival, history, and cross-session handoff. The conductor updates local state
atomically, commits state snapshots after meaningful transitions, and pushes the
state branch when network access is available.

The committed state is a durable mirror, not a replacement for GitHub. On every
tick, GitHub wins for issue closure, labels, PR existence, merged status, and
checks. Local heartbeat files win for local process liveness. The state branch
connects those facts into a replayable run graph: which issue was dispatched,
which branch/worktree/log belonged to it, what attempts already happened, and
why a retry was chosen.

Full raw logs should not be committed by default because they can contain
tokens, paths, or private reasoning traces. The state branch should store log
paths, byte counts, hashes, summaries, and short scrubbed tails. Full logs remain
local artifacts unless the user explicitly asks to preserve them.

## Conductor Lease

Durable state creates the possibility of two conductors acting on the same run.
The resilience layer therefore needs a lightweight lease.

At the start of each tick, the conductor:

1. Pulls the state branch.
2. Reads `state.json`.
3. Acquires or renews a lease by writing `lease_id`, `host`, `pid`,
   `started_at`, and `lease_expires_at`.
4. Commits and pushes the lease update.

If another valid lease exists, the conductor observes only. If the lease is
expired, a new conductor may take it over and append an event explaining the
takeover. This is enough for v1 and v2. It is not a distributed lock with
perfect guarantees, but git push conflicts plus short lease expiry provide a
practical compare-and-swap boundary for a developer tool.

## Worker Heartbeat Contract

Every worker attempt must have a heartbeat file owned by the worker adapter, not
by Codex. This matters because Codex can hang before producing useful output,
and because loopcoder should support other providers later. The adapter or an
adapter-owned supervisor observes the worker from the outside and writes:

```json
{
  "version": 1,
  "job_id": "job-41-1",
  "issue": 41,
  "attempt": 1,
  "provider": "codex",
  "pid": 12345,
  "phase": "codex_exec",
  "status": "running",
  "started_at": "2026-06-26T12:01:00Z",
  "heartbeat_at": "2026-06-26T12:14:45Z",
  "last_progress_at": "2026-06-26T12:14:30Z",
  "last_progress_kind": "log_advanced",
  "log_bytes": 38291,
  "diff_fingerprint": "sha256:...",
  "dirty": true,
  "exit_code": null,
  "error": null
}
```

`heartbeat_at` means the adapter supervisor is alive. `last_progress_at` means
the worker made observable forward progress. These are intentionally different.
A heartbeat can be fresh while progress is stale; that is a stuck worker. A
heartbeat can be stale because the supervisor died; that is an orphaned or lost
worker until the conductor proves otherwise from process and GitHub state.

## What Counts As Progress

For the Codex worker in `loopcoder dispatch`, progress is any
observable change that increases confidence the attempt is moving toward a PR.
The adapter should update `last_progress_at` when any of these changes:

- Adapter phase transition: worktree created, prompt written, Codex process
  started, Codex exited, dirty check passed, commit created, push completed, PR
  opened, cleanup completed.
- Codex log advancement: `codex.log` size or mtime changes.
- Summary advancement: the `-o <summaryFile>` output appears or changes.
- Worktree advancement: `git status --porcelain` changes, or the git diff
  fingerprint changes.
- Child command observation, if the provider exposes it later: command started,
  command output advanced, command exited.

Progress is not the same as success. A worker that repeatedly appends the same
error can still be making process-level progress while failing semantically.
That case is handled by error classification and retry limits. Stuck detection
only answers: has anything observable changed recently?

For headless Codex specifically, the strongest generic signals are log
advancement and worktree diff changes. The conductor should not require a diff
change early in the run because Codex may spend time reading files before
editing. It should not require continuous log output forever because tests or
tools can be quiet. The timeout therefore flags a stale worker first and only
terminates after a second threshold or policy decision.

## Stuck Detection Policy

The default policy should mirror the practical lesson from
`claude_code_agent_farm` while being conservative enough for Codex:

```yaml
resilience:
  worker:
    heartbeat_interval_seconds: 15
    stale_after_seconds: 120
    hung_after_seconds: 300
    max_attempts: 3
    retry_backoff_seconds: [10, 30, 120]
```

Definitions:

- Fresh: `now - heartbeat_at <= heartbeat_interval_seconds * 2`.
- Stale progress: `now - last_progress_at > stale_after_seconds`.
- Hung: progress is stale for longer than `hung_after_seconds`, or the
  heartbeat itself is stale and no matching live process can be found.
- Idle: the worker exits successfully but produces no file changes, no PR, or
  an adapter phase that means "waiting for input" or "ready" instead of done.
- Error: the worker exits non-zero, fails commit/push/PR creation, or emits a
  known fatal pattern such as authentication failure.
- Context-limited: the worker or log indicates context exhaustion, compaction
  failure, or repeated inability to continue due to context limits.

Handling:

1. On stale progress, mark the attempt `stale`, append an event, and report the
   issue as at risk in the conductor's progress table.
2. On hung, capture context, terminate the worker process if it is still alive,
   preserve the worktree/logs, and decide whether to retry.
3. On idle, inspect whether a PR or branch was created. If no deliverable exists,
   treat it as retryable failure with an idle reason.
4. On error, classify the error. Retry only when the error is likely transient
   or worker-local; block when it needs human credentials, missing dependencies,
   unclear requirements, or policy approval.
5. On context limit, restart with a compacted recovery prompt that includes the
   issue, prior summary, changed files, useful log tail, and explicit next step.

The default two-minute stale threshold is a detection threshold, not a kill
threshold. This distinction prevents false positives on a legitimate long
command while still surfacing the problem quickly.

## Recovery Context

A retry or re-dispatch must be context-rich. The conductor writes a recovery
brief for every terminated, failed, idle, context-limited, or conflict-evicted
attempt. The brief should include:

- Issue number, title, body, acceptance criteria, and dependency status.
- Original branch, worktree path, prompt path, summary path, and log path.
- Attempt number, status, start time, stop time, last progress time, and reason.
- Last known adapter phase and command, if available.
- Changed files and diff fingerprint.
- A scrubbed log tail and error summary.
- Whether a PR exists and, if so, its number, URL, head branch, checks, and
  verifier notes.
- For conflict eviction, the conflicting paths, rebase output, PRs that already
  landed, and the updated base branch.
- The narrowed next objective for the replacement worker.

This follows `docs/specs/0028-scheduling.md`: re-dispatch is not a blind retry. Conflict
eviction already requires capturing real conflict context and narrowing scope.
The resilience layer generalizes that rule to all recovery. A replacement worker
should know exactly what failed, what state survived, what it may reuse, and
what outcome is expected.

## Retry And Re-Dispatch Semantics

Recovery should prefer deterministic adoption before retry:

1. If a PR exists for the issue, adopt the PR as the current deliverable and
   continue with verification or a fix pass.
2. If a remote branch exists and clearly belongs to the active attempt, inspect
   it. If it contains useful work but no PR, either create the missing PR or use
   that branch for a fix pass.
3. If only a local worktree exists, preserve it, capture a patch or diff summary,
   and decide whether to continue in place or start a fresh worktree.
4. If there is no useful work, start a clean attempt from the current base.

Branching rules:

- First attempt uses the normal branch, for example `loop/issue-41`.
- If no PR exists and the branch is safe to reuse, retry may reuse it.
- If a PR exists, retry should add fix commits to the PR branch rather than
  opening duplicate PRs.
- If branch ownership is ambiguous, start a new attempt branch such as
  `loop/issue-41-retry-2`, mark the prior branch superseded in state, and report
  the ambiguity.
- If a conflict-evicted PR must be redone from updated `main`, start from
  updated `main` and include the conflict brief. Close or supersede the old PR
  only with explicit conductor policy or user approval.

Retry limits must be bounded. The default maximum is three attempts per issue,
with exponential backoff. After the limit, the issue becomes blocked and
dependents remain unready. The blocked report should include the recovery brief,
attempt history, and the concrete human decision needed.

## Auto-Restart Triggers

Auto-restart is allowed for worker-local failures that do not change product
scope or merge policy.

Retryable by default:

- Stale progress that becomes hung.
- Adapter process lost after worktree creation.
- Codex exits non-zero with a transient CLI/tool error.
- Worker exits idle with no file changes.
- Context-limit or compaction-limit signal.
- Push or PR creation failure that is clearly transient and does not require
  credentials or branch policy changes.

Block by default:

- Authentication failure or missing `gh`/provider credentials.
- Missing required repo setup or checks that cannot run.
- Ambiguous issue requirements.
- Repeated identical failure after the retry limit.
- Dirty or conflicting local state that cannot be attributed to the worker.
- Any step that would require auto-merge or bypassing the human merge gate.

The conductor may also auto-restart a worker after a clean idle state if the
issue is still unimplemented and no PR exists. This is the agent-farm-inspired
"idle is not done" rule: an agent being ready, idle, or quiet does not satisfy
the issue unless a branch/PR/check/verifier path proves a deliverable exists.

## Crash Survival And Resume

A fresh conductor session should be able to resume with this procedure:

1. Read `.delivery.yml`, `SKILL.md`, and the resilience state path.
2. Pull the state branch and load the newest run snapshot.
3. Re-read GitHub issues, labels, PRs, branches, and checks.
4. Reconcile GitHub state with the snapshot.
5. Scan known heartbeat files and worktree paths if on the same host.
6. For each active attempt, classify it as running, completed, stale, hung,
   orphaned, superseded, or unknown.
7. Adopt completed PRs, retry recoverable attempts, and block attempts needing
   human input.
8. Recompute the ready set using `docs/specs/0028-scheduling.md`.
9. Dispatch only items whose dependencies are merged and whose active attempts
   are not still running.
10. Write a new state event summarizing the resume.

This is the concrete loopcoder version of the Gas Town and Ralph lessons. Work
does not survive because one long-lived model session remembers it. Work
survives because the issue graph, attempt ledger, progress facts, and recovery
briefs are represented in files and git. A new session can start with fresh
context, read the ledger, and continue.

## Reconciliation Rules

Reconciliation must be deterministic enough that two fresh sessions make the
same decision from the same inputs.

- Issue closed and PR merged on GitHub: item is done, regardless of stale local
  worker state.
- PR open for issue: item is in review, fixing, gated, or blocked based on
  checks/verifier status; do not dispatch a duplicate implementation worker.
- Remote branch exists but no PR: inspect branch, compare to state, and either
  open a PR, run a fix pass, or mark the branch ambiguous.
- Heartbeat fresh and process alive: attempt remains running.
- Heartbeat fresh but progress stale: attempt is stale, then hung after the
  configured hung threshold.
- Heartbeat stale and process missing: attempt is orphaned/lost; capture and
  retry if under limits.
- Local process alive but no state entry: do not adopt automatically. Record an
  unknown worker and require manual attribution unless branch/log metadata
  proves the issue mapping.
- State says running but GitHub shows a PR from that branch: adopt the PR and
  stop monitoring the worker attempt as implementation.

The conductor should append reconciliation decisions to `events.jsonl` so later
sessions can see why a worker was killed, adopted, retried, or blocked.

## Relationship To Scheduling

The resilience layer does not change the two ordering axes in
`docs/specs/0028-scheduling.md`.

Real code dependencies still force serial dispatch: B waits until A is merged
to `main`, then branches from updated `main`. Resilience state can remember that
B is blocked, but it must not dispatch B while A is merely running or open as a
PR.

File overlap remains a merge-time concern. Parallel workers may still touch the
same files in separate worktrees. If an overlapping PR cannot rebase after
another PR lands, the conflict-eviction model applies: capture the real conflict
context, evict the PR from the merge group, and re-dispatch with a narrower
brief. The only addition from resilience is that the eviction context is stored
durably and counted as an attempt with its own recovery reason.

## Relationship To The Worker Adapter

`loopcoder dispatch` owns the mechanical worker path:
worktree, prompt file, `codex exec`, dirty check, commit, push, PR creation, and
cleanup. It is the worker adapter and includes supervisor
responsibilities:

- Create a stable `job_id` and attempt metadata before launching Codex.
- Write heartbeat and attempt files during each adapter phase.
- Launch Codex in a way the supervisor can observe without blocking heartbeat
  updates.
- Track log size, summary mtime, git dirty state, and diff fingerprint while
  Codex runs.
- Preserve worktree/logs on failure, hung termination, or retry capture.
- Return structured JSON for success, failure, and retryable classification.

Codex still only edits files. The adapter still controls commits, pushes, and PR
creation. The resilience layer makes those adapter-owned phases visible and
replayable.

## Toward A Stateless Background Conductor

`DESIGN.md` section 8 says the conductor should re-derive state from GitHub each
tick, with optional durable mirrors for metrics. Section 12 says v2 swaps the
local runtime for a cloud/background conductor while preserving `.delivery.yml`
and labels. This resilience design is the missing middle step.

With durable state and heartbeat contracts in place, a conductor tick becomes:

```text
read GitHub + state files
acquire lease
reconcile attempts
classify liveness
recover or dispatch ready work
verify PRs
write state
report material changes
release lease
```

That tick can run inside the current Opus chat, a local scheduled process, a
scheduled agent session, a GitHub Action, or a later cloud service. The model session no
longer needs to carry the full run in memory. Each tick can be fresh context,
like Ralph, and still make correct progress because it reads the same GitHub and
git-backed state.

## Implementation Phases

1. State files only: write `state.json` and `events.jsonl` for dispatch, PR
   creation, verification, merge, and blocked transitions. Do not change retry
   behavior yet.
2. Heartbeat instrumentation: have the worker adapter write heartbeat files and
   classify stale/hung/idle/error/context-limit attempts.
3. Recovery briefs: preserve failed worktrees/log summaries and generate
   context-rich retry prompts.
4. Bounded retry: add auto-restart for retryable worker-local failures with
   backoff and max attempts.
5. Resume command: teach a fresh conductor session to load state, reconcile with
   GitHub, and produce the next ready actions.
6. Background tick: move the same reconciliation loop behind a scheduled local
   or cloud runtime without changing the state schema.

Each phase should be verified with deliberate failure drills: kill Codex mid-run,
freeze log output, remove the conductor session, create a PR while state says
running, force a rebase conflict, and simulate context-limit output.

## Open Design Questions

- Whether `.loopcoder/` state should be committed to a single global
  `loopcoder/state` branch per repo or to one branch per run.
- How much log tail can be safely stored after scrubbing.
- Whether retries should reuse the first branch more aggressively or always use
  attempt-specific branches until a PR exists.
- How to represent cross-host local process state when a cloud conductor resumes
  a run started on a developer workstation.
- Whether future providers can expose richer progress events than log/diff
  polling, and how to normalize them through the Worker port.

These questions should be resolved during implementation. They do not change
the central design: progress must be observable, recovery context must be
durable, and a fresh conductor must be able to continue from GitHub plus
git-backed state rather than from chat memory.
