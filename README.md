<div align="center">

# loopcoder

**Turn a delivery need into reviewed pull requests -- without leaving the chat.**

[![Version](https://img.shields.io/badge/version-v0.2.0-brightgreen.svg)](CHANGELOG.md)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Claude Code Skill](https://img.shields.io/badge/Claude%20Code-Skill-green.svg)](SKILL.md)
[![Cross-platform](https://img.shields.io/badge/cross--platform-Go-00ADD8.svg)](docs/go-migration.md)

[What it is](#what-it-is) | [The loop](#the-loop) | [Install](#install) | [Usage](#usage) | [How it works](#how-it-works) | [Design](#design)

</div>

## What it is

loopcoder is an autonomous delivery loop. Describe what you want shipped in one chat; it plans the work into GitHub issues, dispatches Codex workers in isolated git worktrees, opens pull requests, has Opus review them, and leaves the merge to you.

It kills the copy-paste churn of AI coding: ask the model, paste issues into GitHub, run an agent, review the diff, repeat. With loopcoder that loop runs from the conversation. One chat. No window-switching. You stay the merge authority.

## The loop

```mermaid
flowchart LR
  need([your need]) --> plan[plan issues + DAG]
  plan --> dispatch[dispatch workers<br/>git worktree -> codex]
  dispatch --> pr[pull requests]
  pr --> review[Opus review<br/>+ required checks]
  review --> gate{{you merge}}
  gate -. next layer .-> plan
```

The conductor is Opus in Claude Code; the worker is Codex; the gate is you.

## What it looks like

Illustrative:

```text
you   > /loopcoder add a /healthz endpoint and a test, behind a feature flag
loop  > plan: #41 endpoint, #42 test (blocked-by #41). dispatch the ready set? [y]
loop  > #41 -> worktree -> codex -> PR #43   checks green   verdict: pass
loop  > #41 merged; #42 ready -> PR #44   verdict: pass
loop  > done. 2 PRs, you merged both, 0 blocked.
```

## Install

```bash
go install github.com/jasonhnd/loopcoder/cmd/loopcoder@latest
```

Prerequisites on `PATH`: `git`, `gh` (authenticated), and `codex`.

Cross-platform: macOS, Linux, and Windows -- a single Go binary, no PowerShell. loopcoder is also usable as a Claude Code skill; point the `loopcoder` skill at this repo.

## Usage

- In Claude Code: `/loopcoder <your need>` -- the Opus conductor plans, dispatches, reviews, and reports; you name what to merge.
- The mechanical layer is the `loopcoder` binary. The conductor calls it; you can too:

```bash
loopcoder ready-set     --repo .                  # classify ready vs blocked work
loopcoder dispatch-wave --repo .                  # dispatch the current ready wave
loopcoder resume        --repo .                  # reconcile a run after an interruption
loopcoder recover       --repo . --issue-number 41 --issue-title "Add /healthz endpoint" --run-id <id>   # bounded retry of a failed attempt
loopcoder verify-local  --repo . --pr-number 43   # run a repo's local check commands on a PR
```

## How it works

- Conductor: Opus, in Claude Code. It plans issues, reviews PRs, and reports status. It never writes the code itself.
- Worker: `loopcoder dispatch` runs Codex for one issue in a fresh, isolated git worktree, then opens a PR.
- Gate: you merge. loopcoder never auto-merges.
- Ports and adapters: GitHub work items, git-worktree workspace, Codex worker (provider-pluggable), GitHub PRs and checks, Opus verifier, human-merge gate. Configure them per repo in `.delivery.yml`.
- Doc-first: a design or spec document merges before any code implements it. See [`docs/PROCESS.md`](docs/PROCESS.md).
- Cross-platform: one Go binary; Codex runs through a real file-handle stdin instead of shell redirection, and worktree creation is serialized with a cross-platform file lock.

## Why loopcoder

- You always merge -- explicit human gate, never auto-merge.
- Isolated git worktrees -- parallel workers do not collide; conflicts are handled at merge time.
- Doc-first -- code implements a merged design, and review checks conformance to it.
- A real verification gate -- required CI checks must be green before a PR is merge-eligible; every review ends in `pass`, `fail`, or `needs-human`.
- Cross-platform native binary -- `go install`, no runtime dependency beyond `git`, `gh`, and `codex`.
- Self-hosting -- loopcoder planned, dispatched, reviewed, and merged most of its own development, including its v0.2.0 rewrite from PowerShell to Go.

## Design

- [`docs/PROCESS.md`](docs/PROCESS.md) -- mandatory doc-first workflow.
- [`docs/architecture.md`](docs/architecture.md) -- current architecture and limits.
- [`docs/scheduling.md`](docs/scheduling.md) -- dependency-aware scheduling.
- [`docs/verification.md`](docs/verification.md) -- required checks and verifier verdicts.
- [`docs/self-improvement.md`](docs/self-improvement.md) -- bounded, human-gated learning loop.
- [`docs/resilience.md`](docs/resilience.md) -- worker state, resume, recovery, and retry.
- [`docs/orchestration.md`](docs/orchestration.md) -- ready-set and dispatch-wave orchestration.
- [`docs/go-migration.md`](docs/go-migration.md) -- native Go backend migration.
- [`docs/usage.md`](docs/usage.md) -- setup and end-to-end usage.
- [`docs/learnings.md`](docs/learnings.md) -- append-only operational learnings.
- [`CHANGELOG.md`](CHANGELOG.md) -- release history.

## Status

v0.2.0 is the current cross-platform native Go CLI: Opus conductor, Codex worker, human-merge gate, doc-first workflow, and real self-hosting. Some pieces remain documented targets rather than current behavior, including a background or cloud conductor tick and broader provider adapters.

## License

[MIT](LICENSE)
