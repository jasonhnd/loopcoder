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
