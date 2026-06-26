# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.2] - 2026-06-26

### Added

- `docs/verification.md`: design for the verification & quality-gate layer (required checks, spec-driven conformance, agent/browser verification, pass/fail/needs-human verdicts).
- `docs/self-improvement.md`: design for a bounded, human-gated self-improvement loop (append-only learnings, reflection-as-proposal, off-limits targets).
- `docs/resilience.md`: design for resilience (worker heartbeat, stuck/hung/idle detection, bounded retry with recovery context, GitHub-first crash recovery).
- `docs/learnings.md`: append-only operational learnings file with entry template and advisory-authority order.
- SKILL.md "Learnings (self-improvement)" subsection: conductor read path (relevant excerpts, advisory) and human-approved, separate-PR close-out write path.
- SKILL.md "Worker liveness & recovery" subsection: stale/hung/idle classification, idle-is-not-done, bounded retry, GitHub-first resume.
- `scripts/dispatch-worker.ps1`: per-attempt heartbeat/attempt JSON sidecar written at phase boundaries (job_id, phase, status, started_at/heartbeat_at/last_progress_at, log_bytes, exit_code, error); attempt fields added to the success JSON; failed-attempt artifacts preserved.
- `.delivery.yml`: optional commented configuration surfaces for `ci` (tests/typecheck/build), `verification`, and `resilience`.
- `.github/workflows/ci.yml`: required `verify` check covering PowerShell parse validation for scripts and YAML validity.
- `docs/learnings.md`: first three append-only operational learning entries from the v0.1.2 run.
- SKILL.md "Improvement review" subsection: bounded, human-gated self-improvement M3 reflection pass that drafts improvement candidates with evidence, target, risk, and verification.
- `scripts/dispatch-worker.ps1`: durable run state under `.loopcoder/runs/<RunId>/workers/*.attempt.json` plus append-only `.loopcoder/runs/<RunId>/events.jsonl`; added `-RunId` batch grouping and gitignored `.loopcoder/`.
- Resilience recovery: `scripts/dispatch-worker.ps1` writes secret-scrubbed recovery briefs under `.loopcoder/runs/<RunId>/recovery/`; new `scripts/recover-and-retry.ps1` adopts an existing PR first, otherwise retries with backoff up to the configured maximum and blocks after max attempts.
- `scripts/resume.ps1`: read-only GitHub-first resume/reconcile report that combines GitHub and local run state, classifies attempts as `done`, `in-review`, `running`, `stale`, `hung`, `orphaned`, or `ready`, and prints next ready actions without dispatching or merging.

### Changed

- SKILL.md verification: the verifier procedure now enforces required `ci.checks` and spec conformance against the referenced merged design doc, and ends every PR review with an explicit `pass`/`fail`/`needs-human` verdict and fix-pass routing, instead of advisory-only review. Human-merge remains the only merge gate.
- `.delivery.yml`: `ci.checks` now declares `[verify]`, so the verification gate enforces green-before-merge-eligible instead of remaining inert with empty checks.
- The `verify` CI job now also asserts that every `.delivery.yml` `ci.checks` name maps to a real workflow job id, so gate config drift (a renamed or removed required check) fails CI loudly instead of silently stalling the conductor.

### Fixed

- Recovery briefs written by `scripts/dispatch-worker.ps1` now use proper triple-backtick fenced code blocks (the brief here-string previously emitted collapsed fences for the changed-files, PR-status, and log-tail sections).

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
