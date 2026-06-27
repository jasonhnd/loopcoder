# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.3.0] - 2026-06-28

### Added

- Provider-neutral agent abstraction and registry for dispatch and verification, with actionable errors for unknown providers.
- `claude` verified worker adapter and experimental/unverified `gemini` worker adapter alongside the default `codex` adapter.
- Independent `loopcoder loopreview` verifier command that checks a PR branch in read-only mode, emits a structured `pass`, `fail`, or `needs-human` verdict when the verifier completes, and degrades a slow or hung verifier to `needs-human` at the timeout.
- `.delivery.yml adapters` role slots for `conductor`, `worker`, and `verifier`, plus a reviewer-not-worker advisory warning when the verifier is configured to match the worker.

### Changed

- Worker output and repo-facing artifacts are documented as English.
- Worker `--provider`, `--model`, and `--effort` behavior is provider-specific: `codex` remains the default, `claude` can honor effort, and experimental/unverified `gemini` ignores effort with an advisory.
- Documentation now describes loopcoder in runtime- and ecosystem-agnostic terms, removing paseo and internal-ecosystem framing from the user-facing surface.

### Notes

- The `gemini` adapter is present and registered, but it was not verified end-to-end because the Gemini CLI was not usable in the development environment due to missing authentication.
- `loopreview` ships as a command with a working timeout safety net. LLM verifier provider reliability is experimental in v0.3.0: a real `claude` verifier run did not complete within the 180s timeout and returned `needs-human`, and `gemini` verification is unverified. Reliable provider verification is a v0.3.1 follow-up.

## [0.2.0] - 2026-06-27

### Added

- Native cross-platform `loopcoder` Go binary with subcommands at parity with the v0.1.x PowerShell helpers: `dispatch`, `ready-set`, `resume`, `recover`, `verify-local`, plus native `dispatch-wave` (one-wave orchestration) and `state` / `lease` (cross-session state branch + conductor lease per docs/resilience.md).
- Cross-platform Codex execution: `exec.Command` with a real file-handle stdin (the portable closed-stdin fix), replacing the Windows `cmd /c` redirection.
- Cross-platform worktree-add lock via `github.com/gofrs/flock`, replacing the Windows named mutex.
- A CI `go` job (build / vet / test) and `.delivery.yml ci.checks: [verify, go]` so Go code is gated.
- `go install github.com/jasonhnd/loopcoder/cmd/loopcoder@latest` distribution.
- Secret scrubbing + recovery briefs, durable run state, and bounded retry ported to Go with deterministic unit tests.

### Changed

- SKILL.md backend selection: the conductor calls the `loopcoder` binary (resolution: `LOOPCODER_BIN` -> `loopcoder` on `PATH`, required on all platforms including Windows). Removed the PowerShell helper layer (`scripts/*.ps1`); the `loopcoder` binary is the sole mechanical backend. The CI `verify` job was de-PowerShelled (now runs in bash). The conductor model (human-merge only, doc-first, observe-at-merge, model/effort inheritance, verification gate) is unchanged; only the helper command names changed.

### Notes

- Before removing the PowerShell layer, the native binary was validated end-to-end: built locally, then ran `loopcoder dispatch`, producing a real PR via `codex` + `git` + `gh`.
- Command parity is covered by unit tests and the `go` CI gate; real-codex end-to-end is validated by the operator on their platform.

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
