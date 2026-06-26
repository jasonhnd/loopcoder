# loopcoder Scheduling Design

Status: DESIGN. This document specifies the intended scheduling behavior for a
batch of issues that mixes parallel and serial work. It is written doc-first per
[`PROCESS.md`](PROCESS.md); conductor and worker implementation changes come
later as separate code issues after this document merges.

Current state: the built conductor path dispatches issues sequentially today.
This document describes the target scheduling model, not the current
implementation.

## Scope

This is the minimal v1 scheduling design for small batches in a single open
conductor session:

- Work is isolated in git worktrees.
- Real code dependencies determine serial dispatch.
- Independent ready issues dispatch in parallel.
- File overlap is observed from PR diffs and handled at merge time.
- The conductor never auto-merges; the user names which PRs to merge.

Out of scope for v1:

- Predicting file overlap before work starts.
- An automated merge-queue daemon.
- SQLite or other resumable conductor state.
- Complexity tiers or a large-roadmap scheduler.

## Two Axes Of Ordering

loopcoder must keep these two ordering axes separate.

### 1. Real Code Dependency

A real code dependency means one issue's code must build on another issue's
merged code. If issue B depends on issue A, B is serial after A:

1. A is implemented and merged.
2. The conductor recomputes the ready set.
3. B becomes ready only after A is merged.
4. B starts from the updated `main`, not from A's feature branch.

This relation is represented as `depends_on` in the planned work graph and, in
the GitHub adapter, as `blocked-by:#N` labels plus the conductor's in-memory DAG.

The doc-to-code case follows the same rule. A code issue that implements a
design document depends on the design-doc issue. The code issue must wait until
the design document is merged, then branch from `main` and implement per the
merged document.

Stacking B directly on A's branch may be useful later for deep dependency
chains, but it is not the default v1 behavior. The default is wait-for-merge,
then branch from `main`.

### 2. File Overlap

File overlap means two issues touch one or more of the same files. File overlap
is not a dispatch dependency.

Two issues may touch the same files and still run in parallel if neither has a
real code dependency on the other. Parallel work is safe because each worker
runs in its own git worktree. The conflict risk appears only when the resulting
PRs are merged back to `main`.

Therefore file overlap is a merge-ordering concern, not a dispatch-ordering
concern.

## Worktree Isolation

Each worker runs in a separate git worktree created by the worker adapter
described in [`worker.md`](worker.md). Worktrees isolate the working directories,
branches, indexes, and uncommitted files used by each worker.

Because of that isolation, parallel work does not conflict while workers are
editing files. Two workers can both edit `README.md` in separate worktrees
without interfering with each other. The first real conflict can only occur
when one PR has merged and the next overlapping PR is rebased or merged into the
updated `main`.

loopcoder therefore does not try to predict file overlap before dispatch.
Predicted overlap is unreliable: issue text can be incomplete, workers can
choose different implementation paths, and generated diffs can differ from the
plan. Instead, loopcoder observes the real file set for each PR by running:

```text
gh pr diff <pr> --name-only
```

Those observed file sets drive merge ordering.

## Layered Ready-Set Scheduling

The scheduler treats the issue graph as a dependency DAG.

An issue is ready when all issues in its `depends_on` set are merged. "Done" for
dependency purposes means merged to `main`, not merely implemented, pushed, or
open as a PR.

The conductor loop is:

1. Compute the ready set: all unstarted issues whose dependencies are merged.
2. Dispatch the whole ready set in parallel.
3. As workers finish, review their PRs and report status in chat.
4. When a ready PR is merged, recompute the ready set.
5. Dispatch any newly ready issues.
6. Repeat until the DAG is drained or blocked.

This creates layers. All currently ready independent work starts together.
Dependent work waits for the merged outputs of earlier layers.

## Worktree Creation Race

There are two independent mechanical concerns:

1. Worktree creation.
2. Merge ordering for overlapping PRs.

Worktree creation needs its own small serialization rule. If N workers start at
the same time and all run `git worktree add` against the same repository at the
same time, git can race on repository lock files such as the index lock.

The conductor should therefore serialize only the worktree-creation step. After
each worktree exists, the Codex worker runs remain parallel.

This serialization does not change scheduling semantics. It is a git mechanics
guardrail, not a dependency rule.

## Merge Ordering

When PRs are ready for merge consideration, the conductor builds a simplified
merge queue from real PR diffs. A PR is ready for merge consideration when the
worker has opened it, verification is complete enough to present to the user,
and there is no known dependency blocker.

For each ready PR, the conductor reads the actual changed file list:

```text
gh pr diff <pr> --name-only
```

The conductor groups PRs by file-set overlap and proposes a merge order to the
user:

- Non-overlapping PRs may merge in any order.
- Overlapping PRs merge serially.
- For an overlapping group, merge the first PR, rebase the next PR onto updated
  `main`, then merge it if the rebase and verification are acceptable.

This is a simplified merge queue. It exists to order human-approved merges, not
to auto-merge work.

loopcoder never auto-merges. The user names which PRs to merge. The conductor
then handles the mechanics for those named PRs: checking the observed file
sets, proposing or applying the safe order, rebasing overlapping PRs as needed,
and running the configured `gh pr merge` command only for PRs the user named.

## Conflict Eviction

If an overlapping PR cannot be rebased cleanly onto updated `main`, the
conductor evicts the conflicting PR from the current merge group.

Eviction means:

1. Stop trying to merge that PR in the current sequence.
2. Capture the conflict context: PR number, issue number, changed files,
   conflicting paths, relevant rebase output, and the already-merged PRs that
   changed the base.
3. Narrow the remaining work scope based on the real conflict.
4. Re-dispatch a worker with that conflict context and narrowed scope.

The re-dispatch is not a blind retry. The worker prompt must explain what
changed on `main`, what conflicted, and what smaller outcome is expected.

## Serial Dependency Handling

For a real code dependency, dispatch remains strictly serial:

```text
A merged to main -> B branches from main -> B implements
```

The conductor must not dispatch B while A is only in progress or only available
as an unmerged PR. This keeps the default branch history simple and keeps B's
acceptance criteria tied to code that actually landed.

The design-doc-to-code dependency is the same pattern:

```text
design doc merged to main -> code issue branches from main -> code implements per doc
```

## Failure And Blocked States

The DAG drains when every issue is merged or otherwise closed according to the
approved plan. It is blocked when at least one issue cannot proceed and no
remaining unstarted issue is ready.

Blocked examples include:

- A worker fails and the issue needs human clarification.
- A PR fails verification and no fix pass is approved.
- A rebase conflict is evicted and needs narrowed-scope re-dispatch.
- A dependency PR is not merged, so downstream issues are not ready.

The conductor reports blocked state in chat rather than silently skipping work.

## Relationship To Existing Docs

[`architecture.md`](architecture.md) describes the v1 system as built: an Opus
conductor session, GitHub issues and PRs, git worktrees, the Codex worker
adapter, Opus verification, chat reporting, and human-directed merges.

[`PROCESS.md`](PROCESS.md) defines the mandatory doc-first workflow. This file
is the scheduling design document; conductor logic in `SKILL.md` and any worker
changes must come later in separate code issues after this document merges.

[`worker.md`](worker.md) describes the current worker adapter and its use of
fresh git worktrees.

[`../DESIGN.md`](../DESIGN.md) is the north-star lineage. It cites
`autonomous-loops` and the Ralphinho RFC-driven DAG plus
merge-queue-with-eviction patterns. This document narrows that lineage to the
minimal small-batch, single-session, no-auto-merge scheduling behavior for
loopcoder v1.
