# Autonomous Delivery Engine — Design

Status: DRAFT (design-first; not yet built). Owner: jasonhnd. Name: `loopcoder`.

## 1. Purpose
Turn any repository into a self-driving delivery system: **ROADMAP in → shipped code out**, with the human acting as approver-of-record at the merge gate. Replaces the manual relay (human copy-pastes tasks between Claude and Codex, babysits each step) with an unattended conductor that dispatches, reviews, gates, and performs approved merges across an issue dependency graph.

Reusable across projects: any repo onboards by dropping `.delivery.yml` + `ROADMAP.md` and registering one conductor trigger. No bespoke per-repo orchestration code.

### Non-goals (v1)
- Not a cloud/SaaS service. v1 can run from a local human-launched or scheduled agent session; the host must be available while local work is running. Cloud path is v2 (section 12).
- Not a replacement for human product judgment. The roadmap and the merge gate stay human-owned.
- Not tied to one orchestration runtime. Any host that can launch an agent session, call the `loopcoder` binary, and report results can drive the loop.

## 2. Thesis and leverage
We already own ~80% of this:
- **Runtime/host layer**: a human-launched session, local scheduler, CI cron, or cloud worker can run conductor ticks, notifications, approval prompts, and bounded execution.
- **loopcoder worker adapter**: the native binary owns per-issue git worktrees, provider-pluggable workers, PR creation, liveness, recovery, and cleanup.
- **Reusable delivery patterns**: DAG decomposition, tiered pipelines, "reviewer is not the author" adversarial review, merge-queue-with-eviction.
- **GitHub integration primitives**: issue/PR state synced via `gh`, plus codex-as-worktree-worker harness conventions for dispatch, liveness, and cleanup.

Engine = **runtime-agnostic conductor + loopcoder worker adapter + reusable delivery patterns + a thin glue layer we build (section 5).**

## 3. Roles
- **Conductor** — a human-launched or scheduled agent session. Stateless per tick; re-derives state from GitHub each wake. Scans issues, computes the ready set, dispatches workers, triggers reviews, enforces the gate, merges only when allowed, advances the DAG.
- **Worker** — the `loopcoder` binary running an implementation provider in its own isolated git worktree (branch-off main). Implements one issue, opens a PR, self-checks.
- **Reviewer** — a contrasting-provider agent (Claude/Opus) that never wrote the code; adversarial audit + `gh pr checks` verify gate.
- **Gatekeeper** — the conductor policy function: classify PR risk, mark clean low-risk PRs as merge-eligible, and page the human for risky/visual changes. Human approval controls the actual merge.
- **Human (you)** — owns ROADMAP; approves gated merges; resolves escalations.

## 4. Issue lifecycle (the loop)
States carried as GitHub labels:
`status:ready` -> `status:implementing` -> `status:in-review` -> `status:fixing` -> `status:gated` -> (merge) -> closed.

Per conductor tick:
1. **Refresh DAG** from GitHub: list issues with `delivery:*` labels; an issue is *ready* iff every `blocked-by` issue is CLOSED and it is unassigned / `status:ready`.
2. **Dispatch** each ready issue to a Worker through `loopcoder` in a fresh worktree. Label `status:implementing`; record worker/job id.
3. **On worker finish** (adapter result or host notification): the Worker has opened a PR. Label `status:in-review`. Launch Reviewer (Claude) with `--verify-check "gh pr checks <n>"`.
4. **Review verdict**:
   - changes requested -> Worker fix pass (`status:fixing`), loop (bounded by max_fix_loops).
   - clean + CI green -> `status:gated`; run Gatekeeper.
5. **Gate**: risk = f(issue tier, changed paths, visual signal). Low -> mark merge-eligible and include in the human approval report. Risky/visual -> page the human through the configured approval channel; wait for approve/reject. All v1 merges require explicit human approval.
6. **Merge** (per `.delivery.yml` method) -> close issue -> clean up worker worktree -> dependents recompute as ready next tick.
7. Repeat until no open `delivery:*` issues remain.

## 5. Components to build (the glue)
### 5a. Roadmap -> issue-DAG compiler
- Input: `ROADMAP.md` / `roadmap.yml` — a list of work units. Output: GitHub issues + dependency labels.
- WorkUnit schema: `{ id, title, scope, acceptance[], depends_on[], tier, risk, visual? }`.
- Emit `gh issue create` per unit (English body: Context/Scope/Acceptance/Constraints/Dependencies), labels `delivery:unit`, `tier:<n>`, `risk:<low|med|high>`, `blocked-by:#N`.
- Idempotent: re-running updates, never duplicates (match by a hidden `unit-id` marker in the body).
- Codify the compiler around roadmap parsing, GitHub issue creation, dependency labels, and idempotent updates.

### 5b. Conductor (host-driven agent session + prompt)
- Trigger: human command, cron, desktop scheduler, CI event, or cloud scheduler. The trigger starts a conductor-capable agent session with repo access.
- The agent *prompt* IS the conductor logic (scan -> ready set -> dispatch -> review -> gate -> merge -> advance), reading `.delivery.yml`.
- Stateless: re-derives everything from GitHub labels each tick (no external DB needed in v1). Optional state files can improve resilience, but GitHub remains the source of truth.
- Bounded: the host supplies run limits, expiry, and retry caps; the conductor enforces `max_fix_loops`.

### 5c. Worker dispatch + briefing
- Call `loopcoder dispatch` or `loopcoder dispatch-wave` with an isolated `git worktree` branch-off main, provider = `.delivery.yml` `impl` / worker adapter, and completion reporting handled by the host or adapter.
- Briefing contract: self-contained, zero-context issue body + repo constraints + "implement, run <ci commands>, open a PR with Closes #<n>".
- The conductor does not recreate worktree, provider, commit, push, PR, liveness, recovery, or cleanup mechanics; those live in the `loopcoder` binary.

### 5d. Review stage (adversarial)
- Reviewer = contrasting provider (Claude/Opus), MUST NOT be the worker (author-bias elimination).
- Review shape: inspect diff, acceptance criteria, and `gh pr checks <n> --fail-fast`; reviewer posts findings; bounded fix loop (<=3) before escalation (loop-operator semantics: stall -> freeze + page).

### 5e. Merge-gate risk policy
- Risk inputs: issue tier/risk label; changed-path globs from `.delivery.yml`; diff size.
- Decision: `risk:low` AND not visual AND CI green AND review clean -> **report as merge-eligible**. Else -> **page human** through the configured channel, with a one-line summary + PR link; wait for explicit approval. The conductor merges only after the human approves or names the PR.
- **Visual detection** (none of the existing skills have this; satisfies the visual-signoff rule): any changed path matching `visual_globs` is always gated, with a before/after preview URL attached.

## 6. `.delivery.yml` (per-repo portability layer)
```yaml
version: 1
language: en                     # all issues/PRs/comments
impl: codex/gpt-5.4              # worker provider
review: claude/opus             # reviewer provider (MUST differ from impl)
ci:
  commands: ["bun run lint", "bunx tsc --noEmit -p tsconfig.json", "bun run test"]
  checks: ["verify", "Vercel"]   # gh check names that must be green
merge:
  method: merge                  # merge|squash|rebase
  human_required: true           # v1 merge gate
gate:
  merge_eligible_when: { risk: low, visual: false }
  visual_globs: ["web/app/**", "**/*.css", "web/**/tokens*"]
  high_risk_globs: ["**/migrations/**", "**/auth/**", "**/contracts/**"]
  page: telegram                 # phone channel
constraints:                     # injected into every worker/reviewer briefing
  - "No runtime AI/LLM; deterministic only"
  - "Near-zero client JS on content pages"
  - "Vercel-first; no external paid service"
schedule: "*/15 * * * *"
bounds: { max_runs: 200, expires_in: "72h", max_fix_loops: 3 }
```

## 7. Provider routing
Provider choices are configuration, not architecture. `.delivery.yml` and/or host config can pin roles such as implementation, audit, research, and tests:
```json
{ "impl": "codex/gpt-5.4", "audit": "claude/opus", "research": "claude/sonnet", "tests": "claude/haiku" }
```

The conductor reads those choices and passes only the relevant worker selection to `loopcoder`; the runtime that launched the conductor remains replaceable.

## 8. State model
- Source of truth = GitHub (issues, labels, PRs, checks). Conductor is stateless; re-derives each tick -> crash/restart safe.
- Labels: `delivery:unit`, `status:*`, `tier:*`, `risk:*`, `blocked-by:#N`, `gated`.
- Optional: maintain a durable mirror or metrics cache of GitHub issue/PR state synced via `gh`; not required for correctness.

## 9. Human surface
- You touch: ROADMAP (input) + gated-merge approvals + escalations.
- Page format: `[deliver] PR #<n> "<title>" — risk:<r> visual:<bool> — CI green, review clean. Approve merge? <url>`.
- Everything else is silent.

## 10. Guardrails and failure handling
- **Bounded**: host run limits/expiry; max_fix_loops; never an unbounded loop.
- **Runtime-safe operation**: do not interrupt active workers or the conductor host unless the run is explicitly paused or recovered; preserve worktrees/logs before cleanup.
- **Stall/escalation** (loop-operator semantics): no progress across 2 ticks, identical failing check twice, or cost drift -> freeze that unit, page human, run `/harness-audit`.
- **Policy gates honored**: English-only, gate-at-merge, visual-signoff, design-doc rigor.
- **Liveness**: local v1 pauses if the host is unavailable; stateless re-derivation lets the next tick resume from GitHub state.

## 11. Onboarding another repo
1. Copy `.delivery.yml` (edit impl/review/ci/gate globs/constraints).
2. Write `ROADMAP.md`.
3. Run the compiler -> issues created.
4. Register the conductor trigger (manual command, cron, scheduled host, or cloud event).
Same engine, new repo.

## 12. Local (v1) vs cloud (v2)
- **v1 (local, now):** human-launched or scheduled local conductor session; `loopcoder` binary invokes Codex in isolated worktrees; approval happens in chat or a configured human channel. Fast to build, host-bound.
- **v2 (cloud, later):** Conductor as a GitHub Action, cloud event, or service; workers via provider APIs or hosted worker adapters; approvals via GitHub review or a bot. Stateless, team-grade, host-independent. Same `.delivery.yml` + labels; swap the runtime.

## 13. Build milestones
- **M1** Compiler: `ROADMAP.md` -> issue DAG (idempotent), using the roadmap-to-issues rules in section 5a.
- **M2** Happy path: conductor dispatches ONE ready issue -> Codex worker -> PR (no review/gate yet). Proves worktree + provider adapter wiring.
- **M3** Review + CI gate (reviewer != author, `gh pr checks`).
- **M4** Merge-gate risk policy + human approval + visual detection.
- **M5** DAG advance (unblock dependents) + `.delivery.yml` portability + bounds/escalation.
- **M6** (optional) cloud v2.

## 14. Risks / open questions
- Approval UX is host-specific, but merge policy is not. The conductor must encode policy explicitly and pause cleanly for human merge approval whenever policy requires it.
- Worker provider adapters: Codex CLI vs future provider CLIs/APIs; confirm each adapter can edit in an isolated worktree and that the `loopcoder` binary can open PRs with available network + `gh` credentials.
- Idempotent compiler matching (the unit-id marker) — design the marker.
- Cost: many workers in parallel; bound via maxRuns + tier-based concurrency cap.
- Naming / repo home for the tool itself (this design currently lives at D:\AgenticCoder\loopcoder\).

## 15. Before "done" (rigor audit)
Per project rule: before finalizing, audit this doc against the current `SKILL.md`, `docs/architecture.md`, `docs/worker.md`, the `loopcoder` CLI help, `.delivery.yml`, GitHub issue/PR state-sync behavior, and worker-harness assumptions — not self-review only.
