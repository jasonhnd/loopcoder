<div align="center">

# loopcoder

An autonomous delivery loop - ROADMAP in, shipped code out

![License](https://img.shields.io/badge/license-not%20specified-lightgrey.svg)
[![Claude Code](https://img.shields.io/badge/Claude%20Code-Skill-green.svg)](SKILL.md)
[![Version](https://img.shields.io/badge/version-v0.1.0-brightgreen.svg)](CHANGELOG.md)

[What it is](#what-it-is) . [How the loop works](#how-the-loop-works) . [Install](#install) . [Design](#design)

</div>

## What it is

loopcoder removes the human relay between Opus, GitHub, Codex, and PR review.
Today, a person often has to turn a need into issues, post them to GitHub,
start Codex workers, copy PR results back to Opus, review the output, and merge
the result.

With loopcoder, one chat drives the loop:

```text
need -> issues + DAG -> approve -> codex workers in worktrees -> PRs -> Opus review -> merge in chat
```

The v1 conductor is the Opus chat session itself. It plans a small batch,
publishes approved GitHub issues, dispatches background workers, reviews each
PR, reports progress in the same chat, and merges only the PRs the user names.

## How the loop works

loopcoder is a Claude Code skill plus a thin worker adapter, not a separate
daemon. The design is ports and adapters: the loop core speaks stable
interfaces, while v1 binds those interfaces to GitHub, git worktrees, Codex,
Opus, `gh`, and chat.

The conductor is the Opus session following [`SKILL.md`](SKILL.md). It reads
`.delivery.yml`, drafts the issue dependency DAG, keeps a compact state table,
dispatches ready work, verifies PRs, and reports progress.

The worker is [`scripts/dispatch-worker.ps1`](scripts/dispatch-worker.ps1). It
creates a fresh git worktree, runs headless `codex exec`, commits and pushes the
result, and opens a pull request. The Worker port is provider-pluggable by
design; v1 ships the `codex` adapter, with other direct-CLI adapters deferred.

The v1 ports are:

| Port | v1 adapter |
| --- | --- |
| WorkItemSource | GitHub issues via `gh` |
| Workspace | `git worktree` |
| Worker | `codex` via `scripts/dispatch-worker.ps1` |
| VcsHost | GitHub PRs, checks, and merges via `gh` |
| Verifier | Opus review in chat |
| Gate | human-merge; never auto-merge |
| Reporter | the same Opus chat |

```text
User need
    |
    v
Opus conductor (SKILL.md)
    |
    v
Issues + dependency DAG
    |
    v
Human approval
    |
    v
GitHub issues
    |
    v
Ready issue(s)
    |
    v
scripts/dispatch-worker.ps1
    |
    v
git worktree + codex worker
    |
    v
Pull request
    |
    v
Opus verifier + gh checks
    |
    v
Chat report -> user names PRs -> gh pr merge
```

## Install

loopcoder is a Claude Code skill. Invoke it from Claude Code with:

```text
/loopcoder <need>
```

For a global install, copy or symlink this repo to:

```text
~/.claude/skills/loopcoder/
```

Install automation is on the roadmap.

## Status

v0.1.0 is the M1 build: a thin full loop for small batches, with Opus as the
single-session conductor, Codex as the v1 worker, GitHub as the work-item and
PR host, Opus review, chat reporting, and merge-on-instruction.

loopcoder is self-hosting: it has written its own `SKILL.md`, `.delivery.yml`,
`README.md`, and `ROADMAP.md`.

## Design

- [`DESIGN.md`](DESIGN.md) - north-star autonomous delivery engine design.
- [`docs/specs/2026-06-26-loopcoder-v1-design.md`](docs/specs/2026-06-26-loopcoder-v1-design.md) - v1 design spec.
- [`docs/specs/`](docs/specs/) - implementation specs.
- [`docs/BACKLOG.md`](docs/BACKLOG.md) - deferred items and follow-ups.
