<div align="center">

# loopcoder

An autonomous delivery loop - ROADMAP in, shipped code out

[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Claude Code](https://img.shields.io/badge/Claude%20Code-Skill-green.svg)](SKILL.md)
[![Version](https://img.shields.io/badge/version-v0.2.0-brightgreen.svg)](CHANGELOG.md)

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

In v0.2.0, the mechanical backend is a single native Go `loopcoder` binary. It
replaces the PowerShell helper layer so loopcoder can run on macOS, Linux, and
Windows without a PowerShell dependency. The Opus conductor in [`SKILL.md`](SKILL.md)
is unchanged: the binary is the worktree, Codex, PR, state, resume, and recovery
backend it calls.

## How the loop works

loopcoder is a Claude Code skill plus a native helper CLI, not a separate
daemon. The design is ports and adapters: the loop core speaks stable
interfaces, while v1 binds those interfaces to GitHub, git worktrees, Codex,
Opus, `gh`, the `loopcoder` binary, and chat.

The conductor is the Opus session following [`SKILL.md`](SKILL.md). It reads
`.delivery.yml`, drafts the issue dependency DAG, keeps a compact state table,
dispatches ready work, verifies PRs, and reports progress.

loopcoder uses a doc-first process: write and merge the design or spec under
`docs/` first, implement from that merged document in a separate issue, then
verify the result against the document.

The worker backend is `loopcoder dispatch`. It creates a fresh git worktree,
runs headless `codex exec`, commits and pushes the result, and opens a pull
request. The Worker port is provider-pluggable by design; v1 ships the `codex`
adapter, with other direct-CLI adapters deferred. Model and speed are knobs only
when you choose them. By default the worker passes no model or reasoning-effort
flags, inherits your Codex config, and loopcoder never chooses them for you.

The dependency DAG drives scheduling: independent ready work can run in
parallel, real code dependencies stay serial until upstream PRs merge, and file
overlap is observed at merge time from the actual PR diffs.

The v1 ports are:

| Port | v1 adapter |
| --- | --- |
| WorkItemSource | GitHub issues via `gh` |
| Workspace | `git worktree` |
| Worker | `codex` via `loopcoder dispatch` |
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
loopcoder dispatch / dispatch-wave
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

loopcoder is a Claude Code skill plus a native helper binary. Invoke the
conductor from Claude Code with:

```text
/loopcoder <need>
```

Install the native backend with:

```text
go install github.com/jasonhnd/loopcoder/cmd/loopcoder@latest
```

The backend expects `git`, `gh`, and `codex` on `PATH`. Put the installed
`loopcoder` binary on `PATH`, or set `LOOPCODER_BIN` to an explicit binary path.
The conductor selects `LOOPCODER_BIN` first, then `loopcoder` from `PATH`, then
the Windows `.ps1` fallback scripts for one release window. macOS and Linux use
the native binary; they do not require PowerShell.

The binary commands are:

- `loopcoder dispatch`
- `loopcoder ready-set`
- `loopcoder dispatch-wave`
- `loopcoder resume`
- `loopcoder recover`
- `loopcoder verify-local`
- `loopcoder state`
- `loopcoder lease`

For a global install, copy or symlink this repo to:

```text
~/.claude/skills/loopcoder/
```

Install automation is on the roadmap.

## Status

v0.2.0 is the current cross-platform native Go CLI release. The base loop
remains Opus as the single-session conductor, Codex as the v1 worker, GitHub as
the work-item and PR host, Opus review, chat reporting, doc-first execution, and
merge-on-instruction. The change is the mechanical backend: the native
`loopcoder` binary replaces the PowerShell helper layer as the normal path.

It implements:

- Native backend: `dispatch`, `ready-set`, `dispatch-wave`, `resume`, `recover`,
  `verify-local`, `state`, and `lease` commands in one cross-platform Go binary.
- Verification: a real GitHub Actions `verify` check, required-check gating
  through `.delivery.yml`, spec-conformance review against the merged design
  document, and explicit `pass`, `fail`, or `needs-human` verdicts. A PR is not
  merge-eligible until the conductor has seen the required checks green.
  Command parity is covered by unit tests plus the CI `go` gate; real-Codex
  end-to-end validation is still an operator responsibility.
- Self-improvement: an append-only [`docs/learnings.md`](docs/learnings.md)
  file with real entries, relevant-learning read guidance, final-run learning
  review, and a bounded, proposal-only improvement-review pass that remains
  human-gated.
- Resilience: local durable run state under `.loopcoder/`, attempt sidecars,
  heartbeat/progress records, an events log, recovery briefs, bounded retry via
  `loopcoder recover`, GitHub-first resume/reconcile via `loopcoder resume`, and
  state/lease operations through the native backend.

The mandatory doc-first process, worker model/speed inheritance,
dependency-aware scheduling, and human merge gate are active v1 rules. The
background or cloud conductor tick, browser verification automation, periodic
self-improvement review, and broader provider adapters remain documented
targets rather than current behavior.

loopcoder is self-hosting: it has written its own `SKILL.md`, `.delivery.yml`,
`README.md`, and `ROADMAP.md`.

## Design

- [`DESIGN.md`](DESIGN.md) - north-star autonomous delivery engine design.
- [`docs/PROCESS.md`](docs/PROCESS.md) - mandatory doc-first workflow.
- [`docs/architecture.md`](docs/architecture.md) - current v1 architecture and limits.
- [`docs/go-migration.md`](docs/go-migration.md) - native Go CLI migration design.
- [`docs/worker.md`](docs/worker.md) - Codex worker adapter and model/speed knobs.
- [`docs/usage.md`](docs/usage.md) - install, setup, and end-to-end usage.
- [`docs/scheduling.md`](docs/scheduling.md) - dependency-aware scheduling design.
- [`docs/verification.md`](docs/verification.md) - verification gate design.
- [`docs/self-improvement.md`](docs/self-improvement.md) - bounded learnings loop design.
- [`docs/resilience.md`](docs/resilience.md) - worker resilience design.
- [`docs/learnings.md`](docs/learnings.md) - captured learnings.
- [`docs/specs/2026-06-26-loopcoder-v1-design.md`](docs/specs/2026-06-26-loopcoder-v1-design.md) - v1 design spec.
- [`docs/specs/`](docs/specs/) - implementation specs.
- [`docs/BACKLOG.md`](docs/BACKLOG.md) - deferred items and follow-ups.
