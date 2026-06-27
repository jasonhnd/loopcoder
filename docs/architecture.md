# loopcoder Architecture

This document describes loopcoder as built in v1. It is an overview of the
current repo, not the larger north-star system in `DESIGN.md`.

## Overview

loopcoder is a Claude Code skill plus a native Go worker adapter. It is
not a daemon or cloud service in v1. The Opus chat session is the conductor and
runtime, while workers run in separate git worktrees through the
`loopcoder dispatch` adapter.

The built loop is:

```text
need
  -> issues + dependency DAG
  -> human approval
  -> background workers for ready issues
  -> pull requests
  -> Opus review + gh checks
  -> chat progress/final report
  -> user names PRs to merge
  -> gh pr merge from the chat
```

The loop is intentionally small-batch. It is meant for a handful of issues in a
single open Opus session, not large unattended roadmaps.

## Roles

### Conductor

The Conductor is the Opus session following [`../SKILL.md`](../SKILL.md). It
intakes the user's need, inspects repo context, drafts GitHub issues and a
dependency DAG, gets explicit approval before publishing work, dispatches ready
issues, reviews resulting PRs, reports progress in chat, and merges only the PRs
the user names.

The Conductor does not implement code or recreate worker mechanics. For
implementation it calls `loopcoder dispatch`.

### Worker

The Worker is `codex` in v1, invoked by `loopcoder dispatch`. The binary
creates a fresh git worktree from the configured base branch, feeds a
self-contained issue prompt to headless `codex exec`, commits the resulting
changes, pushes the branch, and opens a pull request.

The Worker port is provider-pluggable by design, but the v1 adapter accepts only
`codex`.

### Verifier

The Verifier is Opus review in the same conductor session. It is deliberately a
different role from the Worker. The Verifier inspects the PR diff, checks the
issue acceptance criteria, runs or reads `gh pr checks`, and reports pass/fail,
risks, or requested fixes in chat.

## Ports And Adapters

loopcoder is organized as ports and adapters. The core loop speaks stable
interfaces; `.delivery.yml` selects the v1 adapters that exist today.

| Port | Responsibility | v1 adapter |
| --- | --- | --- |
| WorkItemSource | Create, list, and update work items | GitHub issues via `gh` |
| Workspace | Create and clean isolated implementation workspaces | `git worktree` |
| Worker | Implement one approved issue in a workspace | `codex` via `loopcoder dispatch`; provider-pluggable, with `codex` shipped in v1 |
| VcsHost | Open PRs, read checks, and merge named PRs | GitHub PRs/checks/merge via `gh` |
| Verifier | Review changes against the issue and checks | Opus review |
| Gate | Decide whether a PR may merge | `human-merge`; the user names PRs and Opus runs `gh pr merge` |
| Reporter | Surface progress, verifier results, and final status | This Opus chat |

The current `.delivery.yml` binds those adapters to:

```yaml
adapters:
  work_items: github
  workspace: git-worktree
  worker: codex
  vcs: github
  verifier: opus
  gate: human-merge
report:
  channel: chat
```

## State Model

v1 keeps state in two places:

- GitHub issues, labels, PRs, branches, and checks.
- The Conductor's in-chat state table and in-memory dependency DAG.

Because the Conductor is one chat session, v1 has a real single-session ceiling.
It is built for small batches with short-to-medium tasks. If the session ends,
in-flight background workers may be orphaned; a later session can re-read
GitHub state, but v1 does not provide a fully stateless background conductor
that adopts existing workers automatically.

Full statelessness is deferred. The v1 spec documents this limit and points to a
future background/stateless conductor as the scaling path.

## Doc-First Process

loopcoder follows the mandatory doc-first workflow in
[`PROCESS.md`](PROCESS.md):

1. Write and merge the design or spec under `docs/`.
2. Open separate code issues only after the relevant document is merged.
3. Verify the implementation against the merged document.

Documentation and code are intentionally not bundled in the same issue or PR.

## Design References

- [`../DESIGN.md`](../DESIGN.md) is the north-star autonomous delivery engine
  design. It includes larger ideas that are not built in v1.
- [`specs/2026-06-26-loopcoder-v1-design.md`](specs/2026-06-26-loopcoder-v1-design.md)
  is the v1 design spec and the source for current scope and limits.
- [`specs/`](specs/) contains implementation specs.
