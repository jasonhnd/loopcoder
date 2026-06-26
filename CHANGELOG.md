# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.1] - 2026-06-26

### Added

- Mandatory doc-first process in `docs/PROCESS.md` and the `SKILL.md` "Process discipline" section, requiring document-first work, separate code implementation, and final verification.
- Documentation set for current v1 behavior: `docs/architecture.md`, `docs/worker.md`, `docs/usage.md`, and `docs/scheduling.md`.
- Optional worker model and speed overrides through `-Model` and `-Effort` in `scripts/dispatch-worker.ps1`; when absent, Codex inherits the user's global config, and loopcoder does not choose for the user.
- Scheduler playbook coverage in `SKILL.md` for layered ready-set dispatch, observe-at-merge ordering, and conflict eviction, per `docs/scheduling.md`.
- MIT `LICENSE`.

### Changed

- Serialized git worktree creation in `scripts/dispatch-worker.ps1` with a per-repo mutex so concurrent worker dispatches do not race on `git worktree add`.

## [0.1.0] - 2026-06-26

### Added

- Worker adapter in `scripts/dispatch-worker.ps1` for the issue -> git worktree -> Codex -> commit -> push -> PR flow.
- Conductor playbook in `SKILL.md` for planning issues, dispatching workers, reviewing PRs, reporting progress, and merging only on user instruction.
- `.delivery.yml` configuration for v1 adapters, worker defaults, checks, and chat reporting.
- Ports and adapters architecture covering work items, workspaces, workers, VCS hosting, verification, gate, and reporting.
- `ROADMAP.md` template for human-written work units and dependency planning.
- v1 design spec in `docs/specs/2026-06-26-loopcoder-v1-design.md`.
- Self-hosting materials: loopcoder built its own `SKILL.md`, `.delivery.yml`, `README.md`, and `ROADMAP.md`.
