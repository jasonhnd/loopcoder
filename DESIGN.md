# Autonomous Delivery Engine — Design

Status: DRAFT (design-first; not yet built). Owner: jasonhnd. Name: `loopcoder` (codename `paseo-conductor`).

## 1. Purpose
Turn any repository into a self-driving delivery system: **ROADMAP in → shipped code out**, with the human acting only as approver-of-record at risk gates. Replaces the manual relay (human copy-pastes tasks between Claude and Codex, babysits each step) with an unattended conductor that dispatches, reviews, gates, and merges work across an issue dependency graph.

Reusable across projects: any repo onboards by dropping `.delivery.yml` + `ROADMAP.md` and registering one schedule. No bespoke per-repo orchestration code.

### Non-goals (v1)
- Not a cloud/SaaS service. v1 runs on the local paseo daemon (host must be awake). Cloud path is v2 (section 12).
- Not a replacement for human product judgment. The roadmap and the merge gate stay human-owned.
- No new orchestration runtime: we sit on paseo (already installed + running), not a hand-rolled scheduler.

## 2. Thesis and leverage
We already own ~80% of this:
- **paseo** (local daemon, running): the *runtime* — cron-scheduled agents, per-issue git worktrees, multi-provider workers (Codex + Claude), async finish/permission notifications, off-box (phone) approval, bounded autonomy (maxRuns/expiresIn).
- **ralphinho-rfc-pipeline + autonomous-loops** (prose specs): the *design* — DAG decomposition, tiered pipelines, "reviewer is not the author" adversarial review, merge-queue-with-eviction.
- **Harvestable ECC scripts**: `work-items.js` (+ `state-store/`, with `sync-github` pulling issue/PR state via gh), `orchestrate-codex-worker.sh` (codex-as-worktree-worker harness).

Engine = **paseo (muscle) + ralphinho design (brain) + a thin glue layer we build (section 5).**

## 3. Roles
- **Conductor** — a paseo `create_schedule` agent (cron). Stateless per tick; re-derives state from GitHub each wake. Scans issues, computes the ready set, dispatches workers, triggers reviews, enforces the gate, merges, advances the DAG.
- **Worker** — a paseo `create_agent` in its own worktree (branch-off main), provider = impl (Codex). Implements one issue, opens a PR, self-checks.
- **Reviewer** — a contrasting-provider agent (Claude/Opus) that never wrote the code; adversarial audit + `gh pr checks` verify gate.
- **Gatekeeper** — the conductor policy function: classify PR risk, then auto-merge (low) or page the human (risky/visual).
- **Human (you)** — owns ROADMAP; approves gated merges from phone; resolves escalations.

## 4. Issue lifecycle (the loop)
States carried as GitHub labels:
`status:ready` -> `status:implementing` -> `status:in-review` -> `status:fixing` -> `status:gated` -> (merge) -> closed.

Per conductor tick:
1. **Refresh DAG** from GitHub: list issues with `delivery:*` labels; an issue is *ready* iff every `blocked-by` issue is CLOSED and it is unassigned / `status:ready`.
2. **Dispatch** each ready issue to a Worker (paseo worktree agent, Codex). Label `status:implementing`; record worker id.
3. **On worker finish** (paseo notification): the Worker has opened a PR. Label `status:in-review`. Launch Reviewer (Claude) with `--verify-check "gh pr checks <n>"`.
4. **Review verdict**:
   - changes requested -> Worker fix pass (`status:fixing`), loop (bounded by max_fix_loops).
   - clean + CI green -> `status:gated`; run Gatekeeper.
5. **Gate**: risk = f(issue tier, changed paths, visual signal). Low -> auto-merge. Risky/visual -> raise paseo permission + push to phone; wait for approve/reject.
6. **Merge** (per `.delivery.yml` method) -> close issue -> `archive_worktree` -> dependents recompute as ready next tick.
7. Repeat until no open `delivery:*` issues remain.

## 5. Components to build (the glue)
### 5a. Roadmap -> issue-DAG compiler
- Input: `ROADMAP.md` / `roadmap.yml` — a list of work units. Output: GitHub issues + dependency labels.
- WorkUnit schema (from ralphinho): `{ id, title, scope, acceptance[], depends_on[], tier, risk, visual? }`.
- Emit `gh issue create` per unit (English body: Context/Scope/Acceptance/Constraints/Dependencies), labels `delivery:unit`, `tier:<n>`, `risk:<low|med|high>`, `blocked-by:#N`.
- Idempotent: re-running updates, never duplicates (match by a hidden `unit-id` marker in the body).
- We already did this by hand for GEO Phase-1 (#52-#57) — codify that exact logic.

### 5b. Conductor (scheduled paseo agent + prompt)
- `mcp__paseo__create_schedule`: cron (e.g. `*/15 * * * *`), provider `claude/opus` or `sonnet`, `cwd` = repo, `maxRuns`/`expiresIn` for safety.
- The agent *prompt* IS the conductor logic (scan -> ready set -> dispatch -> review -> gate -> merge -> advance), reading `.delivery.yml`.
- Stateless: re-derives everything from GitHub labels each tick (no external DB needed in v1).

### 5c. Worker dispatch + briefing
- `mcp__paseo__create_agent` with `workspace.source.kind="worktree"` branch-off main, provider = `.delivery.yml impl` (codex/gpt-5.4), notify-on-finish (NOT wait_for_agent).
- Briefing contract (reuse the paseo-handoff template): self-contained, zero-context: issue body + repo constraints + "implement, run <ci commands>, open a PR with Closes #<n>".

### 5d. Review stage (adversarial)
- Reviewer = contrasting provider (Claude/Opus), MUST NOT be the worker (author-bias elimination).
- `paseo loop run` worker/verifier shape with `--verify-check "gh pr checks <n> --fail-fast"` as the CI gate; reviewer posts findings; bounded fix loop (<=3) before escalation (loop-operator semantics: stall -> freeze + page).

### 5e. Merge-gate risk policy
- Risk inputs: issue tier/risk label; changed-path globs from `.delivery.yml`; diff size.
- Decision: `risk:low` AND not visual AND CI green AND review clean -> **auto-merge**. Else -> **page human** via paseo permission + push (`~/.paseo/push-tokens.json`), one-line summary + PR link; wait for `respond_to_permission`.
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
  auto_merge_when: { risk: low, visual: false }
gate:
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

## 7. Provider routing — `~/.paseo/orchestration-preferences.json`
(Currently absent -> defaults apply.) Author it to pin roles:
```json
{ "impl": "codex/gpt-5.4", "audit": "claude/opus", "research": "claude/sonnet", "tests": "claude/haiku" }
```

## 8. State model
- Source of truth = GitHub (issues, labels, PRs, checks). Conductor is stateless; re-derives each tick -> crash/restart safe.
- Labels: `delivery:unit`, `status:*`, `tier:*`, `risk:*`, `blocked-by:#N`, `gated`.
- Optional: harvest `work-items.js` + `state-store` (SQLite) as a durable mirror / metrics; not required for correctness.

## 9. Human surface
- You touch: ROADMAP (input) + gated-merge approvals (phone) + escalations.
- Page format: `[deliver] PR #<n> "<title>" — risk:<r> visual:<bool> — CI green, review clean. Approve merge? <url>`.
- Everything else is silent.

## 10. Guardrails and failure handling
- **Bounded**: paseo maxRuns/expiresIn; max_fix_loops; never an unbounded loop.
- **Never restart the paseo daemon** (it kills every running agent) — operational rule.
- **Stall/escalation** (loop-operator semantics): no progress across 2 ticks, identical failing check twice, or cost drift -> freeze that unit, page human, run `/harness-audit`.
- **Policy gates honored**: English-only, gate-at-merge, visual-signoff, design-doc rigor.
- **Liveness**: host must be awake (v1 local). If asleep, engine pauses; resumes on wake (stateless -> safe).

## 11. Onboarding another repo
1. Copy `.delivery.yml` (edit impl/review/ci/gate globs/constraints).
2. Write `ROADMAP.md`.
3. Run the compiler -> issues created.
4. Register the conductor schedule (one paseo `create_schedule`).
Same engine, new repo.

## 12. Local (v1) vs cloud (v2)
- **v1 (local, now):** paseo daemon on your machine; Codex via paseo provider; phone approval. Fast to build, host-bound.
- **v2 (cloud, later):** Conductor as a GitHub Action (cron/event); workers via Codex Cloud API + Claude API (not local codex.exe); approvals via GitHub review or a bot. Fully unattended, team-grade, host-independent. Same `.delivery.yml` + labels; swap the runtime.

## 13. Build milestones
- **M1** Compiler: `ROADMAP.md` -> issue DAG (idempotent). [reuses GEO Phase-1 logic]
- **M2** Happy path: conductor dispatches ONE ready issue -> Codex worker -> PR (no review/gate yet). Proves paseo worktree+provider wiring.
- **M3** Review + CI gate (reviewer != author, `gh pr checks`).
- **M4** Merge-gate risk policy + phone approval + visual detection.
- **M5** DAG advance (unblock dependents) + `.delivery.yml` portability + bounds/escalation.
- **M6** (optional) cloud v2.

## 14. Risks / open questions
- paseo permission model is a *runtime tool gate*, not a declarative merge-policy — we enforce policy in the conductor prompt + `--verify-check`. Validate it can pause cleanly for human merge approval.
- Codex-as-paseo-provider (codex/gpt-5.4) vs codex MCP: prefer the paseo provider (supervised, no cold-start); confirm it can open PRs (needs network + gh in its worktree).
- Idempotent compiler matching (the unit-id marker) — design the marker.
- Cost: many workers in parallel; bound via maxRuns + tier-based concurrency cap.
- Naming / repo home for the tool itself (this design currently lives at D:\AgenticCoder\loopcoder\).

## 15. Before "done" (rigor audit)
Per project rule: before finalizing, audit this doc against paseo's actual SKILL.md + the paseo MCP tool schemas (confirm create_schedule/create_agent worktree+provider fields, the permission/approval flow, notify-vs-wait exclusivity) and the harvested ECC scripts — not self-review only.