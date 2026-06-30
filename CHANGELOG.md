# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.3.5] - 2026-06-30

### Fixed

- `loopcoder skill install` now updates a stale managed skill file instead of silently skipping it; `loopcoder upgrade` refreshes the bundled conductor skill from the newly selected binary; and `loopcoder doctor` warns on stale or partial installs per [`docs/specs/0291-skill-propagation-on-upgrade.md`](docs/specs/0291-skill-propagation-on-upgrade.md). Upgrading the binary now propagates the conductor playbook.
- Claude attestation now reports the pinned/configured model when that model is present in the provider's reported usage, instead of attributing the invocation to a token-dominant auxiliary model per [`docs/specs/0300-model-attribution.md`](docs/specs/0300-model-attribution.md).

### Changed

- The human-readable attestation block now shows the provider vendor (OpenAI/Anthropic/Google) plus the CLI `tool`, renders model source as `(detected)` or `(self-reported)`, uses host-local timestamps to the second, reports duration in seconds, and uses thousands-separated token counts with a derived total when only input/output are reported per [`docs/specs/0296-attestation-display-polish.md`](docs/specs/0296-attestation-display-polish.md).
- `.delivery.yml` pins the verifier model and effort with `model: "claude-opus-4-8[1m]"` and `reasoning_effort: max`.

### Notes

- Machine contracts are unchanged: canonical JSON, the `[attestation]` header, validation, and fail-closed behavior keep their existing behavior.

## [0.3.4] - 2026-06-30

### Changed

- `dispatch`, `loopreview`, and `dispatch-wave` now emit the human-readable pretty attestation block to stderr by default per [`docs/specs/0282-default-pretty-attestation.md`](docs/specs/0282-default-pretty-attestation.md). The default uses emoji on a TTY and plain ASCII on non-TTY output.
- `--pretty` and `LOOPCODER_PRETTY` force emoji pretty output even on non-TTY output; `--no-pretty` and `LOOPCODER_NO_PRETTY` suppress pretty output and win over force.
- `dispatch-wave` emits one pretty Worker attestation block per dispatched issue.
- The conductor playbook now relays Worker and Verifier pretty attestation blocks verbatim from command stderr instead of hand-formatting attestation report lines.

### Notes

- Machine contracts are unchanged: canonical JSON, `Header()` / `[attestation] ...`, PR bodies, verifier JSON, and fail-closed attestation validation keep their existing behavior.

## [0.3.3] - 2026-06-29

### Added

- `loopreview` now builds a bounded review packet per [`docs/specs/0194-reliable-loopreview-verifier.md`](docs/specs/0194-reliable-loopreview-verifier.md) and #202, including bounded changed-file, issue, merged-spec, and diff excerpts with visible truncation markers. If the packet is insufficient for a safe verdict, `loopreview` returns `needs-human` without invoking the provider.
- `codex` and `claude` are verified `loopreview` verifier providers in the mechanism sense per #205: each can return a valid structured verdict plus Verifier attestation within the timeout.
- A tag-triggered release workflow builds Windows, macOS, and Linux binaries for amd64 and arm64 and publishes `SHA256SUMS` per [`docs/specs/0212-release-distribution-and-upgrade.md`](docs/specs/0212-release-distribution-and-upgrade.md).
- No-Go install scripts, `scripts/install.sh` and `scripts/install.ps1`, install from GitHub Releases with checksum verification per spec 0212.
- `loopcoder version` plus root `--version` and `-v` print the selected binary version, commit, build date, Go version, and platform.
- `loopcoder doctor` runs a read-only preflight reporting `git`, `gh` authentication, configured provider CLIs, the origin remote and detected default branch, `.delivery.yml` validity, binary version and `min_loopcoder_version` compatibility, and conductor-runtime ownership per [`docs/specs/0212-release-distribution-and-upgrade.md`](docs/specs/0212-release-distribution-and-upgrade.md).
- `dispatch` now surfaces Worker attestation with the stable header, canonical JSON, and final result JSON `attestation` object per [`docs/specs/0218-surface-worker-attestation.md`](docs/specs/0218-surface-worker-attestation.md).
- Human-readable attestation pretty rendering, including `--pretty` on `dispatch`, `loopreview`, and `attest`, per [`docs/specs/0214-human-readable-attestation.md`](docs/specs/0214-human-readable-attestation.md). `dispatch` and `loopreview` keep machine stdout stable and write the pretty display to stderr; `attest --pretty` is an explicit opt-in and the default durable output is unchanged.
- Optional `.delivery.yml` `verifier.model` and `verifier.reasoning_effort` settings configure the verifier role per [`docs/specs/0215-per-role-model-override.md`](docs/specs/0215-per-role-model-override.md).
- Worker token usage captures input and output splits when the provider exposes them per spec 0218.
- [`docs/reference/stability-policy.md`](docs/reference/stability-policy.md) documents the 0.x compatibility policy for `.delivery.yml`, CLI flags, and labels.
- `loopcoder init` scaffolds `.delivery.yml` and `ROADMAP.md`, ensures the default labels, and can persist first-run worker and verifier model and effort defaults per spec 0212 and [`docs/specs/0215-per-role-model-override.md`](docs/specs/0215-per-role-model-override.md).
- The conductor playbook (`SKILL.md` and `AGENTS.md`) is embedded in the binary, and `loopcoder skill install` writes it to the Claude skill directory per spec 0212.
- `loopcoder upgrade [--version]` self-updates from GitHub Releases with checksum-before-install verification, an atomic swap, and a Windows deferred-swap fallback per spec 0212.
- A `~/.loopcoder` home with a versioned binary store, `LOOPCODER_HOME` and `LOOPCODER_BIN` resolution, and semver-aware version ordering per spec 0212.
- `dispatch` and `loopreview` accept provider-agnostic `--model` and `--effort` overrides for per-run model and reasoning-effort selection per spec 0215.
- `dispatch-wave` surfaces each worker's attestation facts (provider, model, effort, permission, duration, token usage, and verified) per spec 0218.
- A "Quickstart (new project)" guide in [`docs/reference/usage.md`](docs/reference/usage.md) documents install, `doctor`, `skill install`, per-repo `init`, and driving the loop per spec 0212.

### Changed

- Verifier provider invocation is read-only and headless-hardened per #204: `claude` uses `--print` with a `Read Grep Glob` allowlist and no plan mode, and `codex` uses `exec -s read-only`.
- `.delivery.yml adapters.verifier` now uses the real `claude` provider instead of the invalid `opus` value per spec 0215.
- Release and CI workflows use GitHub Action versions that no longer rely on the deprecated Node 20 runtime per spec 0212.

### Fixed

- Follow-up loopreview reliability polish from #208 and #209, including clearer documentation wording and visible omitted-file names when diff packet content is truncated.
- `loopreview` no longer forces `needs-human` for a brand-new doc-first spec PR whose referenced spec is naturally absent from the base branch; code PRs with a missing merged spec still return `needs-human` per [`docs/specs/0220-loopreview-new-spec-not-a-blocker.md`](docs/specs/0220-loopreview-new-spec-not-a-blocker.md).

### Notes

- The verified-provider proof is about the verifier mechanism, not deterministic model judgment. The LLM `pass` or `fail` verdict itself remains non-deterministic across otherwise valid runs.
- This release line also accepted design specs 0212, 0214, 0215, 0218, and 0220.

## [0.3.2] - 2026-06-28

### Added

- Delivery guardrails per [`docs/specs/0192-delivery-guardrails.md`](docs/specs/0192-delivery-guardrails.md), #198, and #203. `.delivery.yml guardrails.budget` can opt in to `max_runs`, `max_total_attempts`, `max_total_tokens`, and `max_total_cost_usd`; token accounting consumes attestation usage, cost caps are exact-only, and missing or corrupt evidence fails closed to `needs-human`.
- `.delivery.yml guardrails.circuit_breaker` can opt in to no-progress streak thresholds that freeze only the affected issue and require human input before more work is dispatched for that issue.

### Changed

- `dispatch-wave` and `recover` enforce guardrails as pre-dispatch gates and reuse a guardrail ledger for decisions. `ready-set` and `resume` surface budget-blocked or circuit-frozen issues as `needs-human` / `guardrail-frozen` instead of marking them ready.

## [0.3.1] - 2026-06-28

### Added

- Per-invocation attestation for Worker, Verifier, and Conductor roles per [`docs/specs/0146-attestation.md`](docs/specs/0146-attestation.md): worker PR bodies and verifier verdicts carry binary-stamped records with provider, parsed model, effort, permission, duration, and token usage; `loopcoder attest` emits Conductor self-attestation; the Conductor hook enforces the self-attestation step; and missing required identity or usage fails closed with no worker PR, a `needs-human` verifier verdict, or a non-zero `attest` exit.

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
