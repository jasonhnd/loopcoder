# loopcoder Learnings

Status: APPEND ONLY. Entries may be superseded by later entries, but existing
entries are not rewritten except to fix formatting or remove sensitive data.

This file has advisory authority only. It informs conductor and worker prompts,
but it never overrides:

1. System and tool safety constraints.
2. The user's current request.
3. [PROCESS.md](PROCESS.md).
4. [`../SKILL.md`](../SKILL.md).
5. `.delivery.yml` in the target repository.
6. Issue acceptance criteria.

If a learning conflicts with a higher-authority source, prefer the
higher-authority source and report the conflict.

## Entry Template

### YYYY-MM-DD - run <run-id> - <short title>

- Scope: <issue, PR, or command>
- Role: conductor | worker | verifier | human
- Observed: <what happened>
- Evidence: <links to issue, PR, check, log, or command output>
- Learning: <reusable fact or pattern>
- Applies to: <SKILL.md | scripts | docs | scheduling | worker prompts | repo-specific>
- Candidate improvement: <none | suggested issue title>
- Confidence: low | medium | high
- Supersedes: <optional earlier entry id>

### 2026-07-20 - run depowershell-1058 - Release smoke must not keep a second schema truth in PowerShell

- Scope: #1058 / docs/specs/1058-depowershell-release-smoke.md
- Role: conductor
- Observed: Release publish was blocked because `scripts/*-smoke.ps1` asserted
  hard-coded schema 30 while `storage.CurrentSchemaVersion` was already 31.
  Product support is darwin/arm64 only; PowerShell was historical release glue.
- Evidence: release smoke failure on schema 30 vs plan target 31; spec 1058.
- Learning: Keep release/self-bootstrap acceptance in Go next to
  `CurrentSchemaVersion`. Thin bash drivers only; never hard-code the current
  schema generation in scripts. Legacy v0.7 source schema 9 may stay literal.
  Zero `scripts/*.ps1` on the current surface; CI must fail if they return.
- Applies to: scripts, release workflow, storage migrations, releasing docs
- Candidate improvement: none (implemented under #1058)
- Confidence: high
- Supersedes:

### 2026-06-26 - run 2026-06-26-v0.1.2 - Conductor must run its own newly-merged playbook steps in-session

- Scope: SKILL.md self-improvement close-out (issues #47 / #48)
- Role: conductor
- Observed: After merging B (the learnings close-out step), the conductor finished the v0.1.2 run without running that close-out step until the user pointed it out.
- Evidence: v0.1.2 session; no learnings were proposed at the first close-out.
- Learning: Shipping a capability is not the same as using it. After merging a playbook change, re-read and apply the new SKILL.md sections in the same run.
- Applies to: SKILL.md, process
- Candidate improvement: Add a close-out assertion that the conductor reloads SKILL.md after any SKILL.md merge.
- Confidence: high
- Supersedes: none

### 2026-06-26 - run 2026-06-26-v0.1.2 - Do not unilaterally descope the user goal to a first slice

- Scope: code issues #45 / #47 / #49 (A/B/C v0.1.2)
- Role: conductor
- Observed: The conductor scoped each direction to a minimal first slice and declared 0.1.2 done; the user intended fuller completion ("develop to the end").
- Evidence: user feedback in the v0.1.2 session; the premature v0.1.2 tag was deleted.
- Learning: "Develop to the end" means continue until the designs are substantially implemented. Any scope cut must be confirmed with the user, not silently chosen.
- Applies to: conductor, process
- Candidate improvement: When scoping a code issue smaller than its design doc, surface the deferral explicitly and get approval.
- Confidence: high
- Supersedes: none

### 2026-06-26 - run 2026-06-26-v0.1.2 - Self-modifying the worker adapter is safe to dogfood mid-run with guards

- Scope: scripts/dispatch-worker.ps1 heartbeat sidecar (issues #49 / #50)
- Role: verifier
- Observed: The resilience PR changed the live dispatch script; later dispatches (#51 / #52) used the new code with no failure.
- Evidence: #51 / #52 worker JSON included attempt_path / status / exit_code / log_bytes; PowerShell parse passed after merge.
- Learning: A self-modifying worker adapter can be dogfooded in the same session if you parse-check the merged script and constrain the issue to protect load-bearing parts (codex invocation, closed-stdin prompt feed, worktree mutex, model/effort pass-through).
- Applies to: scripts, worker
- Candidate improvement: Add a post-merge parse/smoke check before relying on a changed dispatch script.
- Confidence: high
- Supersedes: none

### 2026-06-26 - run 2026-06-26-v0.1.3-improve - Full v0.1.2 loop validated end-to-end on loopcoder itself

- Scope: validation cycle, issues #73 / #74 -> PRs #75 / #76
- Role: conductor
- Observed: An improvement-review (B M3) selected two real improvements; both ran through parallel dispatch under one -RunId, were gated by the `verify` CI check (green) plus explicit verdicts, reconciled by `scripts/resume.ps1` (classified `done`), and human-merged. No step failed.
- Evidence: PRs #75 and #76 verify green; `resume.ps1` on run-v0.1.3-improve classified #73 and #74 as `done` with no ready or blocked actions.
- Learning: The full v0.1.2 stack (verification gate + self-improvement reflection + durable state / resume + worker) works as one system on loopcoder's own development.
- Applies to: process
- Candidate improvement: none
- Confidence: high
- Supersedes: none

### 2026-06-26 - run 2026-06-26-v0.1.3-improve - The verify gate now protects itself

- Scope: .github/workflows/ci.yml (issues #74 / #76)
- Role: verifier
- Observed: Added a CI step asserting every `.delivery.yml` `ci.checks` name maps to a workflow job id; it ran on its own PR and passed, and was shown to fail on a bogus check name.
- Evidence: PR #76 verify green; the worker tested a missing-check name and got exit 1.
- Learning: Gate config drift (a renamed or removed required check) is now caught loudly by CI instead of silently stalling the conductor.
- Applies to: scripts, CI
- Candidate improvement: the python fallback in that CI step has leading indentation that would error if it ever ran; yq is always present on ubuntu-latest so it is dormant, but dedent or simplify it in a future cleanup.
- Confidence: high
- Supersedes: none

### 2026-06-29 - run 2026-06-29-v0.3.3 - Re-dispatch with a changed brief must sync the GitHub issue body

- Scope: issue/PR #245 (CHANGELOG batch)
- Role: conductor
- Observed: A worker was re-dispatched with a corrected brief, but the GitHub issue body was left stale; the verifier judged the PR against the old acceptance criteria and returned a false fail.
- Evidence: issue/PR #245 (CHANGELOG batch); fixed by `gh issue edit 242 --body-file <new>` then re-running loopreview -> pass.
- Learning: The verifier reads the GitHub issue, not the local `--issue-body` passed to dispatch. When you change a brief on re-dispatch, you MUST sync the issue with `gh issue edit` or the verifier reviews against stale criteria.
- Applies to: conductor, process
- Candidate improvement: none
- Confidence: high
- Supersedes: none

### 2026-06-29 - run 2026-06-29-v0.3.3 - Conductor must independently ground-truth factual claims

- Scope: PR #251
- Role: conductor
- Observed: A verifier claimed four GitHub Actions (`checkout`/`setup-go`/`upload-artifact`/`download-artifact`) could not all be on their stated major versions (a lockstep assumption from pre-cutoff knowledge); ground-truthing showed all were current.
- Evidence: PR #251; verified via `gh api repos/<action>/releases/latest`.
- Learning: Both worker and verifier have knowledge cutoffs and will assert stale "facts" about versions/APIs. The conductor must independently verify factual claims with `gh api` / `git ls-remote` before trusting OR overriding either side.
- Applies to: conductor, process
- Candidate improvement: none
- Confidence: high
- Supersedes: none

### 2026-06-29 - run 2026-06-29-v0.3.3 - Parallelize workers only on non-overlapping file regions

- Scope: W8, issues #264 / #265
- Role: conductor
- Observed: Waves that put two issues touching `internal/cli/cli.go` in parallel risked merge conflict; splitting by file region ran clean.
- Evidence: W8 ran #264 (`internal/cli/cli.go`) and #265 (`internal/orchestration/dispatch_wave.go`) in parallel and both merged with no conflict.
- Learning: `cli.go` is a hot file; cap each parallel wave at <=1 issue that touches it and pick file-disjoint issues for the rest.
- Applies to: conductor, scheduling
- Candidate improvement: none
- Confidence: high
- Supersedes: none

### 2026-06-29 - run 2026-06-29-v0.3.3 - Headless claude verifier model is non-deterministic; pin with --model

- Scope: PR #266 / PR #267 verifier attestations
- Role: verifier
- Observed: In one session the claude verifier ran `claude-opus-4-8[1m]` for one PR and `claude-haiku-4-5` for the next, with no flag change.
- Evidence: PR #266 vs PR #267 verifier attestations (same session).
- Learning: When the verdict needs a known strong model, pass `loopcoder loopreview --model <id>` (per-role override, spec 0215) rather than relying on inherited default model selection.
- Applies to: conductor, worker prompts
- Candidate improvement: document the canonical strong-verifier model id once a deterministic pin string is confirmed.
- Confidence: medium
- Supersedes: none

### 2026-06-29 - run 2026-06-29-v0.3.3 - Distribution correctness lives outside the test suite -- verify the published artifact

- Scope: PRs #278 / #279 (release `-ldflags` version stamping); v0.3.3 re-cut
- Role: conductor
- Observed: The published v0.3.3 binaries passed the full unit suite and both CI checks, yet a downloaded release binary reported `version=dev commit=unknown date=unknown`. Root cause: `.github/workflows/release.yml` built with `-trimpath` but no `-ldflags`, so the `cmd/loopcoder/main.go` version/commit/date vars kept their defaults. This broke `loopcoder version`, `doctor` version / min-version reporting, and `upgrade`'s already-latest detection for every consumer.
- Evidence: Downloaded `loopcoder_0.3.3_windows_amd64.zip` from the GitHub release, verified its SHA256 against `SHA256SUMS`, and ran the binary: `--version` showed `version=dev` before the fix and `version=0.3.3 commit=<sha> date=<ts>` after PRs #278 / #279; `upgrade` then reported "Already latest; no download needed."
- Learning: Green unit tests and green CI do not prove distribution correctness. Version stamping, archive packaging, checksums, and install/upgrade behavior only hold in the real released artifact. After every release cut, download at least one published platform binary, verify its checksum, and run `loopcoder version` and `loopcoder upgrade` to confirm the binary self-reports its real version and recognizes itself as already-latest -- act as a consumer, do not trust green CI alone.
- Applies to: conductor, scripts, process
- Candidate improvement: Partially mitigated -- `release.yml` now smokes the native linux/amd64 build's `--version` and fails on `version=dev`. A follow-up could add a post-publish download-and-verify step (checksum + `--version`) to the release workflow.
- Confidence: high
- Supersedes: none

### 2026-07-06 - run 2026-07-06-v0.5.4 - Flush leftover relay from the previous session before the first dispatch

- Scope: dispatch of issue #550 (0.5.4 C4); the initial dispatch failed with reserved exit code 4
- Role: conductor
- Observed: The first `loopcoder dispatch` of a fresh conductor session refused to run and exited 4, because a Verifier attestation block from the *previous* session's `loopreview` of PR #549 was still pending and unacknowledged in local relay state. Dispatch printed the pending block plus recovery instructions instead of proceeding.
- Evidence: dispatch stdout "loopcoder relay gate: pending local-only Worker/Verifier attestation block(s) must be relayed before this command can run"; `loopcoder relay flush --repo .` then surfaced the pending PR #549 verifier block (verifier claude opus-4.8-1m, exit 0, 6m36s) and cleared the gate; the re-dispatch of #550 then succeeded.
- Learning: The relay hard gate (spec 0447) persists across sessions via gitignored local state, so a leftover block from a prior session blocks the first mechanical command of the next one. A new conductor session resuming loopcoder self-work should run `loopcoder relay list --repo .` up front and `loopcoder relay flush --repo .` to surface any leftover Worker/Verifier block *before* the first dispatch, rather than discovering it as an exit-4.
- Applies to: SKILL.md (conductor procedure, resume/intake path)
- Candidate improvement: Add a "flush any leftover relay at session start" step to the resume/intake section of SKILL.md.
- Confidence: high
- Supersedes: none

### 2026-07-06 - run 2026-07-06-v0.5.4 - dispatch --run-id is overridden by a worker-generated run id

- Scope: `loopcoder dispatch --run-id 0550-c4 ...` for issue #550
- Role: conductor
- Observed: Dispatch was invoked with `--run-id 0550-c4`, but the attempt was recorded under `.loopcoder/runs/run-20260706T125951Z-issue-550/`, and `loopcoder status --repo . --run 0550-c4` reported `run "0550-c4" not found`. The worker branch (`loop/issue-550`) was correct.
- Evidence: dispatch result JSON `"run_id":"run-20260706T125951Z-issue-550"` and `attempt_path` under that directory despite the `--run-id 0550-c4` flag; `status --run 0550-c4` exited 1 not-found.
- Learning: `dispatch`'s `--run-id` does not control the on-disk run directory (an internal run id is generated), so `status --run <flag value>` will not find it. This is a sibling to the known ignored `--branch` flag. Until reconciled, read the effective run id back from the dispatch result JSON rather than assuming the flag value.
- Applies to: dispatch code, docs, conductor
- Candidate improvement: "dispatch: honor --run-id for the run directory, or document it as advisory and echo the effective run id".
- Confidence: medium
- Supersedes: none

### 2026-07-06 - run 2026-07-06-v0.5.4 - Read the referenced spec from the base branch when the conductor checkout lags main

- Scope: spec-conformance review of PR #551 (C4) against docs/specs/0533; conductor checkout was on a stale branch
- Role: conductor
- Observed: The conductor session working tree was on `roadmap/0.6.0`, 7 commits behind `main`. The merged C4 design spec `docs/specs/0533-audit-consumer-repo-usability.md` did not exist in the working tree, so Grep/Glob for it returned nothing -- even though the worker's worktree (branched from main) saw it fine.
- Evidence: Grep/Glob for `docs/specs/0533-*.md` returned "No files found" / "Path does not exist" on the roadmap/0.6.0 checkout; the file was present via `git show main:docs/specs/0533-...`.
- Learning: The conductor must not assume its working tree contains merged docs. The playbook already says to read the referenced design doc from the base branch (`git show origin/main:docs/<doc>.md`); this is essential, not optional, whenever the conductor checkout lags main. Never conclude "spec missing" from a stale working tree.
- Applies to: SKILL.md (verification gate)
- Candidate improvement: none (playbook already prescribes reading from the base branch; this reinforces it)
- Confidence: medium
- Supersedes: none

### 2026-07-12 - run p0rt01-20260712 - Storage schema newer than every available binary: back up and recreate

- Scope: loopcoder doctor / global storage (~/.loopcoder/data/loopcoder.db)
- Role: conductor
- Observed: doctor failed with "unsupported storage schema version 11; selected loopcoder supports schema version 9" while the release 0.7.0 binary, a fresh main build, and pre-prod supported at most schema 10; grepping CurrentSchemaVersion across all refs found no source for schema 11, so no available binary could open the DB.
- Evidence: doctor output in the mem P0-RT-01 conductor session (run p0rt01-20260712); `git grep "CurrentSchemaVersion = 11"` across all refs returned nothing.
- Learning: An experimental or since-deleted dev build can leave global storage at a schema version no shipping binary supports. GitHub remains the source of truth for delivery state and local run records are advisory, so the recovery is to rename the DB aside (for example loopcoder.db.schema11.bak) and let the selected binary recreate fresh storage; only local report history is lost.
- Applies to: docs, conductor
- Candidate improvement: doctor --fix could offer a guarded backup-and-recreate when the schema is newer than the selected binary and no known upgrade target supports it.
- Confidence: high
- Supersedes: none

### 2026-07-12 - run p0rt01-20260712 - Backgrounded dispatch/loopreview leaves relay records pending; flush before the next mechanical command

- Scope: dispatch-wave / loopreview run as background jobs; relay gate reserved exit code 4
- Role: conductor
- Observed: After running dispatch-wave and loopreview in background shells and relaying their pretty report blocks verbatim in chat, the next mechanical commands (ready-set, a loopreview retry) still refused with exit code 4 until `loopcoder relay flush --repo .` ran in the foreground.
- Evidence: ready-set and loopreview invocations in run p0rt01-20260712 exited 4 with pending-relay recovery instructions.
- Learning: Chat relay alone does not acknowledge relay records. Whenever dispatch/dispatch-wave/loopreview output is captured from a background job, plan a foreground `loopcoder relay flush --repo <repo>` immediately after relaying the blocks, before any further dispatch, ready-set, loopreview, verify-local, recover, or promote call.
- Applies to: SKILL.md, conductor
- Candidate improvement: none
- Confidence: high
- Supersedes: none

### 2026-07-16 - run v0.8.0-release-closeout - Provider cost budgets do not protect the local host

- Scope: v0.8.0 self-hosting and release closeout; issue #968
- Role: conductor
- Observed: The orchestration-cost contract bounded model calls, reported tokens, retries, and deterministic waits, but overlapping local tests, provider processes, and status watchers could still exhaust host CPU, memory, and process capacity.
- Evidence: issue #968; `docs/specs/0968-orchestration-cost-budget.md`; `docs/v0.8.0-retrospective.md`.
- Learning: Every self-hosting run needs an aggregate local-host budget for CPU, RSS, child-process count, test concurrency, and process-tree lifetime in addition to token and provider-call budgets. Until the binary enforces it, the conductor must keep heavy tests remote and allow only one local provider at a time.
- Applies to: PROCESS.md, scheduling, resilience, conductor
- Candidate improvement: "resource governor: enforce aggregate local CPU, memory, process-count, concurrency, and lifetime budgets"
- Confidence: high
- Supersedes: none

### 2026-07-16 - run v0.8.0-release-closeout - Resume the failed stage, not the completed task

- Scope: issue #997 / PR #998 delivery after a successful implementation commit
- Role: conductor
- Observed: A Worker could complete code and focused verification, then fail during push or PR delivery. Re-running the Worker would duplicate useful provider work and repeat tests even though the implementation commit already existed.
- Evidence: issue #997; PR #998; `docs/v0.8.0-retrospective.md`.
- Learning: Classify implementation, test, provider, delivery, infrastructure, waiting, and human-decision failures before retry. When a valid commit exists, reconcile and resume push, PR creation, or report delivery only; never call the provider again for a delivery-only failure.
- Applies to: conductor, dispatch, recovery, delivery
- Candidate improvement: "delivery resume: persist and retry commit/push/PR/report stages independently"
- Confidence: high
- Supersedes: none

### 2026-07-16 - run v0.8.0-release-closeout - Verification must be tiered by evidence boundary

- Scope: v0.8.0 PR, promotion, integrated-main, and release verification
- Role: conductor
- Observed: Substantially overlapping focused tests, full tests, race tests, pre-push checks, PR checks, promotion checks, integrated-main checks, and release checks multiplied time and failure surface without providing proportionally independent evidence.
- Evidence: PR #994; `docs/reference/releasing.md`; `docs/v0.8.0-retrospective.md`.
- Learning: Worker runs format/compile/focused tests; PR CI owns normal test/race/verify/security evidence; promotion uses protected remote checks; the release workflow owns the single full-race, build, signing, and exact-artifact smoke. Do not repeat a remote full gate locally while it is pending or green.
- Applies to: PROCESS.md, CI, hooks, worker prompts, release
- Candidate improvement: "verification planner: select and deduplicate checks by evidence boundary"
- Confidence: high
- Supersedes: none

### 2026-07-16 - run v0.8.0-release-closeout - Durable progress is not user-visible progress

- Scope: issues #967 and #959; host progress delivery during long-running self-hosting work
- Role: conductor
- Observed: A run could persist progress receipts and remain healthy while the active host showed no useful update. Receipt generation, transport write, host acceptance, user visibility, and acknowledgment are separate states.
- Evidence: issue #967; `docs/reference/progress-receipts.md`; `docs/reference/runtime-capabilities.md`.
- Learning: Every active task longer than five minutes must surface a bounded status packet with stage, elapsed time, last evidence, provider activity, local process count, remote gate, next timeout, and next action. A live PID, CPU activity, or unchanged pending check is not evidence of useful progress.
- Applies to: host adapters, reporter, conductor, progress delivery
- Candidate improvement: "progress visibility gate: require five-minute host-visible status or stop/detach"
- Confidence: high
- Supersedes: none

### 2026-07-16 - run v0.8.0-release-closeout - Release quality needs a stopping rule

- Scope: v0.8.0 release-candidate stabilization; issues #869 and #997
- Role: conductor
- Observed: Repeated audits can continue discovering real edge cases after feature completion. Without a frozen blocker definition and candidate limit, each finding can extend the same release indefinitely.
- Evidence: issue #869; issue #997; `docs/reference/v0.8.0-go-no-go.md`; `docs/v0.8.0-retrospective.md`.
- Learning: Enter RC only after planned implementation is complete; permit only P0, P1, and release-contract corrections; defer lower-severity findings; and stop for a human GO/NO-GO decision after two failed candidates. Quality is defined by explicit acceptance and residual-risk ownership, not by an unbounded search for another possible defect.
- Applies to: PROCESS.md, roadmap, release, conductor
- Candidate improvement: "release state: enforce RC freeze, blocker classes, and a two-candidate stop gate"
- Confidence: high
- Supersedes: none
